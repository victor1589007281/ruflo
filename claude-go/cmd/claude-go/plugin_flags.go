package main

// plugin_flags.go —— `run --plugin <name>` 与 agent 资产 frontmatter `plugins:` 的
// 运行时展开 (KindPlugin 资产的消费方, 此前全仓只有解析没有消费)。
//
// ---------------------------------------------------------------------------
// 装配纪律 (三条, 各对应一处具体的静默失败)
// ---------------------------------------------------------------------------
//
//  1. **展开先于 agent 解析。** `--plugin foo --agent <来自 foo 的 agent>` 要能跑:
//     buildEngine 在池装配 (RegisterPoolTools) 之前完成展开, runAsAgent 在
//     resolveAgentRunSpec 之前重合并目录。顺序反了, 插件提供的 agent 就查不到,
//     表现是"资产总是没命中"。
//  2. **合并进副本, 不改污共享目录。** 插件 agents 进的是原目录的**拷贝**
//     (新建 Set: 原目录全部 + 插件 agents), 原 Set 一个字节不动 —— 直接 Add 会让
//     "这个 agent 来自插件"在目录里再也分不出来, 而 dashboard 列目录时报不出归属。
//  3. **找不到就报错退出。** 插件名拼错时静默跳过的话, "我启用了插件怎么没生效"
//     与"生效了"从外面分不出来 (与 resolveAgentRunSpec 不回落成普通 run 同一条)。
//
// 两类成员的接缝各一条, 都在引擎开跑前生效:
//   - buildEngine (旗标插件): hooks 装成 Runner 的可装卸组, 同时并入装配用
//     hookConfigs 变量 (deps.hookConfigs 会把它们带给子代理的 nested runner);
//     skills 在 Skill 工具注册判定之前进注册表 (source="plugin", rank 150);
//   - runAsAgent (agent 声明的插件): 引擎已建好, hooks 走 Runner 组
//     (InstallHookGroup "plugin:<name>"), skills 走引擎里真正服役的那份注册表
//     (engineSkillRegistry)。

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/pluginrun"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/types"
)

// cliPlugins buildEngine 展开的旗标插件 (nil = 没启用任何插件)。
//
// 与 cliPoolPolicy 同一套路: buildEngine 没有返回值通道可带, 而 runAsAgent /
// 斜杠分支的 loadAssetCatalog 会重建进程级目录 (把合并过的那份换掉), 重合并
// 要用到它。
var cliPlugins *cliPluginWiring

// cliPluginWiring 旗标插件的装配产物, 供后续接缝取用。
type cliPluginWiring struct {
	bundles []*pluginrun.Bundle
	// hookCfgs 全部插件 hook 的解析结果 (组装卸之外另留一份): 并入 deps.hookConfigs,
	// 让子代理的 nested runner 也继承 (runNestedAgent 的合并读的是那个切片)。
	hookCfgs []types.HookConfig
}

// wireFlagPlugins buildEngine 的旗标插件装配: 目录装载 (此前没装过时) → 展开 →
// agents 合并进目录副本 → hooks 装上 runner 并留一份给 deps.hookConfigs。
// jsonCfg 只用于取 StateDir (与 runAsAgent 同一口径)。没启用插件返回 (nil, nil)。
func wireFlagPlugins(cwd string, jsonCfg *feishu.JSONConfig, runner *hooks.Runner) (*cliPluginWiring, error) {
	names := normalizePluginNames(flagPlugins)
	if len(names) == 0 {
		return nil, nil
	}
	// 资产目录可能还没装过 (run 的默认路径此前从不装载): 插件展开查的就是它,
	// 没装就是"全部插件都找不到" —— 那种报错会把"没装载"误报成"没这个插件"。
	if agent.AgentCatalog() == nil {
		stateDirInput := ""
		if jsonCfg != nil {
			stateDirInput = jsonCfg.StateDir
		}
		stateDir := basedir.ResolveDefault(stateDirInput, cwd)
		log.Printf("[plugin] 资产装载: %s", loadAssetCatalog(cwd, stateDir))
	}
	bundles, err := expandRunPlugins(agent.AgentCatalog(), names)
	if err != nil {
		return nil, err
	}
	mergePluginAgents(agent.AgentCatalog(), bundles)
	w := &cliPluginWiring{bundles: bundles}
	w.hookCfgs = installPluginHookGroups(runner, bundles)
	return w, nil
}

// expandRunPlugins 按名展开插件; 任一名字找不到即 error (纪律 3, 不静默跳过)。
// 每个成功展开的插件: Problems 逐条上 stderr ([plugin] 前缀, 不致命), 另打一行
// 启用事实 —— 供 webapp 解析运行时事实, 与既有 [agent] 横幅行同风格 (log.Printf)。
func expandRunPlugins(catalog *asset.Set, names []string) ([]*pluginrun.Bundle, error) {
	var bundles []*pluginrun.Bundle
	for _, name := range normalizePluginNames(names) {
		b, err := pluginrun.Expand(catalog, name)
		if err != nil {
			return bundles, err
		}
		for _, p := range b.Problems {
			fmt.Fprintf(os.Stderr, "[plugin] %s: %s\n", b.Name, p)
		}
		c := b.Counts()
		log.Printf("[plugin] 已启用插件 %q (agents:%d skills:%d hooks:%d tools:%d workflows:%d)",
			b.Name, c["agents"], c["skills"], c["hooks"], c["tools"], c["workflows"])
		bundles = append(bundles, b)
	}
	return bundles, nil
}

// mergePluginAgents 把各插件的 agents 成员合并进**目录副本**并装入进程级目录
// (纪律 2)。原目录一个字节不动; 插件 agent 与原目录同名时后者被覆盖且 Set 会
// 留一条 problem —— 覆盖方向是"显式启用的插件赢", 与资产层既有覆盖语义一致。
func mergePluginAgents(catalog *asset.Set, bundles []*pluginrun.Bundle) {
	if catalog == nil {
		return
	}
	hasAgents := false
	for _, b := range bundles {
		if len(b.Agents) > 0 {
			hasAgents = true
			break
		}
	}
	if !hasAgents {
		return
	}
	merged := asset.NewSet()
	for _, k := range asset.Kinds() {
		for _, n := range catalog.Names(k) {
			merged.Add(catalog.Get(k, n))
		}
	}
	// 原目录的装载问题一并带进副本: Problems() 是"这批资产有多少没读出来"的
	// 唯一读数, 换目录时丢掉它们会让摘要行突然变绿。
	for _, p := range catalog.Problems() {
		merged.AddProblem(p.Path, fmt.Errorf("%s", p.Err))
	}
	for _, b := range bundles {
		for _, a := range b.Agents {
			merged.Add(a)
		}
	}
	agent.SetAgentCatalog(merged)
}

// installPluginHookGroups 把各插件的 hook 成员装进 Runner 的可装卸组
// ("plugin:<name>"), 返回装入的全部配置。Runner 组的语义恰好贴合插件:
// 同幂等 (同名组整体替换, 重复启用不会把 hook 装两遍), 且与配置基线分层。
//
// 解析问题 (HookConfigs 的 problems) 逐条上 stderr, 合法的照常装 ——
// 与 hooks.ParseAssetHooks "问题与 hook 同时返回"的契约一致。
func installPluginHookGroups(runner *hooks.Runner, bundles []*pluginrun.Bundle) []types.HookConfig {
	var all []types.HookConfig
	for _, b := range bundles {
		cfgs, probs := b.HookConfigs()
		for _, p := range probs {
			fmt.Fprintf(os.Stderr, "[plugin] %s: %s\n", b.Name, p)
		}
		if len(cfgs) == 0 {
			continue
		}
		if runner == nil {
			fmt.Fprintf(os.Stderr, "[plugin] %s: %d 条 hook 无处安装 (引擎没有 Hook Runner), 这部分成员不生效\n",
				b.Name, len(cfgs))
			continue
		}
		runner.InstallHookGroup("plugin:"+b.Name, cfgs)
		all = append(all, cfgs...)
	}
	return all
}

// loadPluginSkillDirs 把插件的 skills 目录装进技能注册表 (source="plugin",
// rank 150 —— 见 skills.SourceRank)。reg 为 nil (引擎没有 Skill 工具) 时如实
// 点名: "装了但没生效"与"生效了"从外面分不出来, 必须留一句话。
func loadPluginSkillDirs(reg *skills.Registry, bundles []*pluginrun.Bundle) {
	for _, b := range bundles {
		dir := b.SkillsDir()
		if dir == "" {
			continue // 扁平形式没有 bundle 目录 —— Problems 里已有说明, 不重复报
		}
		if reg == nil {
			fmt.Fprintf(os.Stderr, "[plugin] %s: 有 skills 目录但引擎没有技能注册表 (Skill 工具未注册), 这部分成员不生效\n",
				b.Name)
			continue
		}
		reg.LoadFromDirs([]string{dir}, "plugin")
	}
}

// normalizePluginNames 去空白、去空、去重 (保序)。--plugin 是可重复旗标
// (StringArray), 同一个名字给两次只展开一次 —— 重复展开的表现是 hook 组被
// 重装、技能被重复注册, 都无害但噪声。
func normalizePluginNames(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// pluginNamesDifference 返回 declared 里没出现在 already 中的名字 (各自先归一)。
// 用于 agent frontmatter `plugins:` 与 --plugin 旗标的去重合并: 旗标插件在
// buildEngine 已展开过, agent 再声明同一个不重复展开。
func pluginNamesDifference(declared, already []string) []string {
	seen := map[string]bool{}
	for _, n := range normalizePluginNames(already) {
		seen[n] = true
	}
	var out []string
	for _, n := range normalizePluginNames(declared) {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
}
