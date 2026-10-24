// Package pluginrun —— 插件包 (KindPlugin 资产) 的**运行时展开器**。
//
// ---------------------------------------------------------------------------
// 它补的是什么
// ---------------------------------------------------------------------------
//
// 21.8.12 P1 让六种资产都能被解析进 asset.Set, 但 KindPlugin 长期只有"解析"没有
// "消费": pkg/agentbind 的 bindPlugin 只能如实报 BindingNoConsumer ("本仓没有
// plugin 运行时")。本包是那个缺口的另一半 —— 把一个插件资产展开成它磁盘上
// bundle 目录里的全部成员资产:
//
//	<root>/plugins/<name>/
//	  PLUGIN.md            清单 (kind: plugin; name/description/version/body)
//	  agents/<n>.md 或 agents/<n>/AGENT.md
//	  skills/<n>/SKILL.md
//	  hooks/<n>.md 或 hooks/<n>/HOOK.md
//	  tools/...  workflows/...
//
// 展开只做"找出来并解析", **不注册进任何运行时**: 装上是调用方的事
// (cmd/claude-go 的 run --plugin: agents 合并进目录副本 / skills 进注册表 /
// hooks 进 Runner 组)。与 pkg/agentbind 的分工同 BindAgentAsset 的先例:
// 这里是纯装配的上游, 消费方的名字写进 agentbind.BindPluginAsset 的读数里。
//
// 两种不实现的形态, 都如实留痕而不是静默:
//   - 扁平形式 (plugins/foo.md 单文件) 没有 bundle 目录, 成员恒空;
//   - PLUGIN.md 声明的 commands:/mcp: 键在本版本没有运行时消费方
//     (CommandTypePrompt 无运行时; dynmcp 在 CLI 从未初始化)。
package pluginrun

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/types"
)

// Bundle 一个插件包的展开结果: 清单资产 + 各标准子目录的成员资产。
//
// 成员切片的顺序是**按名字排序**的 (asset.Set.Names 的既有保证) —— 这些名字会进
// 日志与装配读数, map 遍历序会让同一份插件两次运行报出不同的排列。
type Bundle struct {
	Plugin *asset.Asset // 清单资产 (KindPlugin)
	Name   string
	// Dir bundle 目录; 扁平文件形式 (plugins/foo.md) 为 "" —— 那种形式没有
	// 子目录可展开, 成员恒空且 Problems 里有一句说明。
	Dir       string
	Agents    []*asset.Asset
	Skills    []*asset.Asset
	Hooks     []*asset.Asset
	Tools     []*asset.Asset
	Workflows []*asset.Asset
	// Problems 展开问题 (逐条人话, 含子目录前缀)。收集品而不是 error:
	// 一个子目录里坏了一个文件, 其余成员仍该可用, 但坏的那个必须能被说出来
	// (与 asset.Problem 同一条纪律)。
	Problems []string
}

// bundleMembers bundle 目录下的标准子目录 → 资产 kind。
//
// 是一张表而不是五处 LoadDir 调用, 与 asset.standardLayout 同一理由: 加一类成员
// 只需加一行, 散着写的话"某类成员没被展开"不会报错 —— 它的表现是"放进去了没反应"。
var bundleMembers = []struct {
	sub  string
	kind asset.Kind
}{
	{"agents", asset.KindAgent},
	{"skills", asset.KindSkill},
	{"hooks", asset.KindHook},
	{"tools", asset.KindTool},
	{"workflows", asset.KindWorkflow},
}

// Expand 按名查 KindPlugin 资产并展开其子目录资产; 找不到返回 error。
//
// 找不到必须报错而不是返回空 Bundle: 插件名拼错时, "启用了但没生效"与"生效了"
// 从外面完全不可区分 —— 静默跳过会把这两件事压成同一件。
func Expand(set *asset.Set, name string) (*Bundle, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("pluginrun: 插件名为空")
	}
	if set == nil {
		return nil, fmt.Errorf("pluginrun: 没有资产目录 (Set 为 nil), 查不了插件 %q", name)
	}
	a := set.Get(asset.KindPlugin, name)
	if a == nil {
		known := set.Names(asset.KindPlugin)
		return nil, fmt.Errorf("pluginrun: 找不到插件 %q: 资产目录里没有这个 KindPlugin 资产 (现有 %d 个: %s)",
			name, len(known), strings.Join(known, ", "))
	}
	b := &Bundle{Plugin: a, Name: a.Name, Dir: bundleDirOf(a)}
	if b.Dir == "" {
		b.Problems = append(b.Problems,
			"扁平形式的插件 (单文件 "+filepath.Base(a.SourcePath)+"): 没有 bundle 目录, "+
				"agents/skills/hooks/tools/workflows 子资产无从展开 —— 要带成员资产请改成 "+
				"plugins/"+name+"/PLUGIN.md 的目录形式")
	} else {
		for _, m := range bundleMembers {
			sub := filepath.Join(b.Dir, m.sub)
			tmp := asset.NewSet()
			// kind 是 LoadDir 的入参, 子文件无需自声明 kind:; 来源标成
			// "plugin:<name>" —— 成员资产的出处 (SourcePath 的上级目录) 与归属
			// (哪个插件) 是两件事, 后者只能靠 Source 留住。
			asset.LoadDir(sub, "plugin:"+b.Name, m.kind, tmp)
			for _, p := range tmp.Problems() {
				b.Problems = append(b.Problems, m.sub+"/: "+p.Error())
			}
			members := sortedAssets(tmp, m.kind)
			switch m.kind {
			case asset.KindAgent:
				b.Agents = members
			case asset.KindSkill:
				b.Skills = members
			case asset.KindHook:
				b.Hooks = members
			case asset.KindTool:
				b.Tools = members
			case asset.KindWorkflow:
				b.Workflows = members
			}
		}
	}
	// commands / mcp 键: 本版本没有消费方 (见包注释)。声明了必须说出来 ——
	// 不说的话, 作者会以为写进 PLUGIN.md 就生效了, 而运行期一个字都不会提。
	if _, ok := a.MetaListSet("commands"); ok {
		b.Problems = append(b.Problems,
			"PLUGIN.md 声明了 commands 键: 本版本无运行时消费方 (prompt 类命令在本仓没有运行时), 这部分声明不生效")
	}
	if _, ok := a.MetaListSet("mcp"); ok {
		b.Problems = append(b.Problems,
			"PLUGIN.md 声明了 mcp 键: 本版本无运行时消费方 (dynmcp 在 CLI 从未初始化), 这部分声明不生效")
	}
	return b, nil
}

// ExpandAll 展开 set 里全部 KindPlugin 资产 (按名字排序 —— Names 的既有保证)。
func ExpandAll(set *asset.Set) []*Bundle {
	if set == nil {
		return nil
	}
	var out []*Bundle
	for _, name := range set.Names(asset.KindPlugin) {
		b, err := Expand(set, name)
		if err != nil {
			// 名字来自 set.Names 本身, 正常不可能走到这; 走到说明 Set 在两次调用
			// 之间被改过, 跳过比 panic 诚实。
			continue
		}
		out = append(out, b)
	}
	return out
}

// HookConfigs 用 hooks.ParseAssetHooks 解析全部 hook 成员资产。
//
// 解析问题与配置**同时**返回 (与 ParseAssetHooks 的契约一致): 三条 hook 两条合法
// 一条拼错时, 合法的两条照常生效, 拼错的那条被点名 —— 整份作废会让作者以为
// "格式整个没写对"。problems 逐条带成员名前缀 ("hooks/<名字>: ...")。
func (b *Bundle) HookConfigs() (configs []types.HookConfig, problems []string) {
	if b == nil {
		return nil, nil
	}
	for _, h := range b.Hooks {
		cfgs, probs := hooks.ParseAssetHooks(h)
		configs = append(configs, cfgs...)
		for _, p := range probs {
			problems = append(problems, "hooks/"+h.Name+": "+p)
		}
	}
	return configs, problems
}

// Counts 返回 {"agents":n,"skills":n,"hooks":n,"tools":n,"workflows":n}。
//
// 固定五键齐全 (零成员也是 0, 不缺键): 调用方把这行打进日志/面板, 缺键会让
// 解析方分不清"没有这类成员"与"这个字段没印"。
func (b *Bundle) Counts() map[string]int {
	if b == nil {
		return map[string]int{"agents": 0, "skills": 0, "hooks": 0, "tools": 0, "workflows": 0}
	}
	return map[string]int{
		"agents":    len(b.Agents),
		"skills":    len(b.Skills),
		"hooks":     len(b.Hooks),
		"tools":     len(b.Tools),
		"workflows": len(b.Workflows),
	}
}

// SkillsDir 返回 <Dir>/skills (无 Dir 返回 "")。
func (b *Bundle) SkillsDir() string { return b.subDir("skills") }

// AgentsDir 返回 <Dir>/agents (无 Dir 返回 "")。
func (b *Bundle) AgentsDir() string { return b.subDir("agents") }

// HooksDir 返回 <Dir>/hooks (无 Dir 返回 "")。
func (b *Bundle) HooksDir() string { return b.subDir("hooks") }

// subDir 返回 <Dir>/<sub>; Bundle 为 nil 或未设 Dir 时返回空串 (由各 *Dir 方法共用)。
func (b *Bundle) subDir(sub string) string {
	if b == nil || b.Dir == "" {
		return ""
	}
	return filepath.Join(b.Dir, sub)
}

// bundleDirOf 判定插件资产的 bundle 目录 (扁平形式返回 "")。
//
// 判据两条同时成立才算 bundle:
//  1. 文件名是清单文档名 (PLUGIN.md / README.md, 与 asset.docNames[KindPlugin]
//     同一份名单 —— LoadDir 只按这两个名字认目录形式的插件);
//  2. 父目录不是 plugins/ 本身 —— plugins/README.md 是**扁平**插件
//     (LoadDir 对 plugins/ 下的直接 .md 也收), 它的父目录是整个插件根,
//     把它当 bundle 会去扫 plugins/agents 这种不存在的语义。
func bundleDirOf(a *asset.Asset) string {
	if a == nil || a.SourcePath == "" {
		return ""
	}
	base := strings.ToUpper(strings.TrimSpace(filepath.Base(a.SourcePath)))
	if base != "PLUGIN.MD" && base != "README.MD" {
		return ""
	}
	dir := filepath.Dir(a.SourcePath)
	if filepath.Base(dir) == "plugins" {
		return ""
	}
	return dir
}

// sortedAssets 按名字序取出某 kind 的全部资产 (Names 已排序, 这里只是跟着取)。
func sortedAssets(set *asset.Set, kind asset.Kind) []*asset.Asset {
	names := set.Names(kind)
	out := make([]*asset.Asset, 0, len(names))
	for _, n := range names {
		if a := set.Get(kind, n); a != nil {
			out = append(out, a)
		}
	}
	return out
}
