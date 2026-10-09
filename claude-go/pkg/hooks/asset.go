package hooks

import (
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/types"
)

// asset.go —— hook 资产的**声明解析** (21.8.12 P3「hook 进池」的解析半边)。
//
// ---------------------------------------------------------------------------
// 为什么这个函数住在 pkg/hooks, 而不是留在组装器里
// ---------------------------------------------------------------------------
//
// 它原本只住在 pkg/agentbind (组装器), 因为那时唯一的消费者是"把一份 agent 资产的
// hooks 声明装进子代理的 Runner"。P3 让 hook 也能作为**独立资产**进池 (§21.8.12 的
// "池里今天只装 tool + skill"), 于是出现了第二个消费者 —— 而它住在
// pkg/tool/builtin, 那个包**不能**引 pkg/agentbind (agentbind 已经引 builtin,
// 反向即成环)。
//
// 两条路: ① 在池那边抄一份解析; ② 把解析挪到两个消费者都能引的地方。
// 取 ②。抄一份的代价不是多几十行, 是**同一份声明格式有了两个真相源** ——
// 哪天给 hook 声明加一个字段 (或修一处切分 bug), 改一边不改另一边, 症状是
// "组装器认得、池不认得"或者反过来, 而两者都不会报错: 读不出来与没声明同形。
// 本仓对 dual-source 有明确纪律 (见 pkg/engine 的 toolExposed 注释),
// 这里按同一条办: 格式只有一份实现, 两个调用方。
//
// pkg/hooks 是它自然的归宿: 它讲的就是 hook 的语义, 且已经依赖 pkg/types。
// 新增的 pkg/asset 依赖是叶子包 (只引标准库), 不成环。

// HookAssetName 一个 hook 资产在池/装载面里的名字。
//
// 与技能、工具同名同形 —— 池的成员表只有一份名字空间, 前缀或后缀会让
// `pool_search` 的结果与 `pool_load` 的入参对不上 (模型看到的是加了前缀的名字,
// 而它照抄那个名字来加载)。
func HookAssetName(a *asset.Asset) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.Name)
}

// ParseAssetHooks 把资产里的 `hooks` 声明解析成可直接喂给 hooks.Runner 的配置。
//
// ---------------------------------------------------------------------------
// 为什么是**扁平三元组**, 而不是 Claude Code 的嵌套写法
// ---------------------------------------------------------------------------
//
// Claude Code 的 hooks 一定是嵌套的:
//
//	hooks:
//	  PreToolUse:
//	    - matcher: "Bash"
//	      hooks: [{type: command, command: ./x.sh}]
//
// 而本仓的 frontmatter 解析器 (pkg/asset.SplitFrontmatter) 是**扁平**的: 它只认
// 单值键与一层 `- item` 列表 (这是为了与 pkg/skills 逐字节一致, 那套解析被整个
// SKILL.md 生态依赖, 不能改)。嵌套写法喂进来会塌成 `pre-tooluse: [matcher: Bash, ...]`
// —— 键名被归一、层级被抹平, 读出来是一堆认不出的东西。
//
// 与其悄悄读不到, 不如把**本仓能读的形态**定死并校验:
//
//	hooks:
//	  - <event>:<type>:<target>[:<if 匹配器>]
//
// 例:
//
//	hooks:
//	  - PreToolUse:command:./scripts/guard.sh:Bash*
//	  - SessionStart:http:https://hooks.internal/session
//
// `type` 留空即 command (与 types.HookType 的既有约定一致)。`if` 省略即不限制。
// 行内写法 (`hooks: A, B`) 与多行写法都读得到 —— MetaList 两种都认。
//
// 返回 (解析出的 hook, 逐条问题说明)。问题与 hook **同时**返回: 一份资产里三条
// hook 两条合法一条拼错时, 合法的两条该照常生效, 拼错的那条该被点名 ——
// 整份作废会让作者以为"格式没写对", 而实际是"有一条没生效"。
//
// ★ 这条注释原本在 pkg/agentbind, 随实现一起搬过来。搬的时候刻意**一字未改** ——
// 它记录的是格式的取值域与理由, 与调用方是谁无关。
func ParseAssetHooks(a *asset.Asset) ([]types.HookConfig, []string) {
	if a == nil {
		return nil, nil
	}
	return ParseHookDecls(a.MetaList("hooks"))
}

// ParseHookDecls 是上面那个格式的**唯一**解析实现, 输入是声明原文。
//
// 为什么把入参从 *asset.Asset 放宽到 []string: StageDef.Hooks (21.8.12 P4/X1) 声明的
// 是同一套 `<event>:<type>:<target>[:<if>]` 文本, 但它是工作流定义里的字符串切片,
// 背后没有 asset.Asset (工作流不是资产文件)。让调用方为了喂进来而伪造一个 Asset
// 是荒唐的; 而在这里再写一份解析, 就是同一套取值域有两个实现 —— 那正是本批次反复
// 要消灭的形态 (两份实现漂移的表现是"某种写法在 A 处读得出、在 B 处读不出", 不报错)。
//
// 校验、问题文案、`type` 空即 command 的约定、冒号切分的三个坑全部保持原样,
// 一行未改 —— 见 ParseAssetHooks 上方的注释, 它记的是格式本身。
func ParseHookDecls(items []string) ([]types.HookConfig, []string) {
	if len(items) == 0 {
		return nil, nil
	}
	var out []types.HookConfig
	var problems []string
	for _, raw := range items {
		// SplitN(…, 3): target 里可能有冒号 (https://…) 或 Windows 盘符, 只能切前两刀。
		parts := strings.SplitN(strings.TrimSpace(raw), ":", 3)
		if len(parts) < 3 {
			problems = append(problems, "hook 声明 "+strconv.Quote(raw)+
				" 读不出来: 需要 <event>:<type>:<target> 三段 (type 可留空即 command), "+
				"注意本仓 frontmatter 是扁平的, Claude Code 那种嵌套 hooks 写法读不到")
			continue
		}
		ev := types.HookEvent(strings.TrimSpace(parts[0]))
		ty := types.HookType(strings.TrimSpace(parts[1]))
		target := strings.TrimSpace(parts[2])
		// 第 4 段 (可选的 if 匹配器) 挂在 target 后面, 用同一个冒号切 —— 只有当
		// target 本身不含冒号时才可能切歪, 而那种 target (command 路径 / URL) 里
		// 出现冒号时本来就在第三段的余部, 所以这里再切一次是安全的。
		ifMatch := ""
		if idx := strings.LastIndex(target, ":"); idx > 0 && !strings.Contains(target[:idx], "://") {
			// 仅当冒号后面那截不含路径分隔符时, 才认为是 if 匹配器 —— 否则
			// `C:\path\to.sh` 会被切成 `C` + `\path\to.sh`。
			tail := target[idx+1:]
			if tail != "" && !strings.ContainsAny(tail, `/\`) {
				ifMatch = tail
				target = target[:idx]
			}
		}

		var prob string
		switch {
		case !types.ValidHookEvent(ev):
			prob = "hook 事件 " + strconv.Quote(string(ev)) + " 不是合法取值: 这条 hook 永远不会触发 " +
				"(装载与注册都会成功, 只是没人按这个名字问它)"
		case !types.ValidHookType(ty):
			prob = "hook 类型 " + strconv.Quote(string(ty)) + " 不是合法取值"
		case target == "":
			prob = "hook 声明 " + strconv.Quote(raw) + " 没有 target (命令 / URL / 策略路径)"
		}
		if prob != "" {
			problems = append(problems, prob)
			continue
		}
		out = append(out, types.HookConfig{
			Event:    ev,
			HookType: ty,
			If:       ifMatch,
			Command:  target,
		})
	}
	return out, problems
}

// HookDeclNames 把一组 hook 配置渲染成读数用的短名 (与组装器的 Binding.Declared 同形)。
//
// 单独提出来是因为它此前是组装器里内联的两行: P3 之后池那一侧也要报同一份清单,
// 内联就意味着两处各拼一次 —— 而"两处各拼一次"的漂移表现是读数对不上, 不是报错。
func HookDeclNames(cfgs []types.HookConfig) []string {
	if len(cfgs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfgs))
	for _, h := range cfgs {
		t := h.HookType
		if t == "" {
			t = types.HookTypeCommand // 与 effectiveHookType 同源: 留空即 command
		}
		out = append(out, string(h.Event)+":"+string(t))
	}
	return out
}
