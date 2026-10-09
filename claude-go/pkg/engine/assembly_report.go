// 装配自报的**采集端** (方案 20.6.4 D0.7)。
//
// 本文件只做一件事: 从一个**已经构造好的**引擎上把装配事实读出来, 变成
// pkg/assembly.Report。它不装配任何东西, 也不改任何开关 —— 采集本身必须是零副作用的,
// 否则"为了报告而多开一个组件"会让报告与不报时行为不同, 报告就不再是观测。
//
// 为什么读引擎而不是读配置: 方案 20.6.2 记的那个真实缺陷 (CLI 路径六个组件为 nil)
// 的表现正是"配置文件里一切都在, 引擎里那一半是空的"。读配置得到的报告会一路全绿,
// 而那恰是本报告要消灭的假象。所有 "实际" 字段一律取自活对象。
//
// ---------------------------------------------------------------------------
// 为什么住在 pkg/engine 而不是某个入口的包里 (21.8.12 P2)
// ---------------------------------------------------------------------------
//
// 采集端最早只有一份, 写在 cmd/claude-go (CLI 入口)。P2 要求"按 Asset 声明装配出的
// agent 运行时也要喂给 Reconcile 对账", 而子代理的派生入口有**两个** (pkg/feishu 的
// 飞书/团队路径与 cmd/claude-go 的 CLI 路径), 两者都要对账。此时留在 cmd 里的那份
// 采集端就变成了"只有 CLI 用得上"的私有件, 飞书那条路只能:
//   (a) 抄一份 —— 于是"什么算装配好了"出现两个真相源, 两边迟早分家; 或
//   (b) 不对账 —— 于是飞书路径上"资产声明了却没生效"永远不可见。
// 两条都不可接受, 所以把它移到引擎自己这里: 报告读的全是引擎的公开读数
// (InternalHookPhases / VisibleTools / HiddenTools / Config / HookRunner / PromptMgr),
// 引擎是最自然的拥有者, 而两个入口都能 import 它。**一份采集端, 两个入口**。
package engine

import (
	"sort"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/assembly"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
)

// AssemblyReportInput 采集端所需的、**引擎自己不知道**的那几项上下文。
//
// 单独打包成结构体而不是串成位置参数: 这几项全是"这次运行是从哪儿被叫起来的"
// 这类归属信息 (描述符版本 / 配置来源 / 状态目录), 与装配事实无关; 位置参数到第四个
// 之后就没人记得住顺序, 而它们三个都是字符串 —— 写反了编译器一声不吭。
type AssemblyReportInput struct {
	// DeclarationVersion 本次运行**实际读到的**那份装配描述符的版本 (空 = 没读到)。
	//
	// 为什么自报要带上"声明"的版本号: 平台侧的对账是拿**当前**描述符与**这一次**的
	// 自报比。若档案在这之后改过, 平台签名发的已是新版本, 而这份自报来自旧版本 ——
	// 两个不同源的输入比出来的"差异"是幻觉, 人照着修只会越修越乱。带上版本号,
	// 消费侧才判得出"我手上的声明是不是这次跑的那一份"。
	//
	// 这仍是**观测**: 记录的是从配置里读到了什么, 不是"我同意它"。
	DeclarationVersion string
	// Config 实际生效的配置文件来源; **nil 表示这台引擎不是从查找链装出来的**。
	//
	// 为什么要三态而不是"路径为空即没有": 三种情况必须分得开 ——
	//   ① 从查找链读了某个文件 (Path 非空);
	//   ② 六环都没命中, 走的是纯默认值 (Path 空但 Found=false);
	//   ③ 这台引擎是**程序化装配**出来的, 压根没有"配置文件"这一环 (nil)。
	// 嵌套子代理是第 ③ 种: 它的模型/预算/档位都继承自父会话, 说它"六环都没命中"
	// 是一句错话 —— 报告里会凭空多出一条"这个子代理没读到配置"的假线索。
	Config *assembly.ConfigSource
	// StateDir statestore 根; 空 = 本路径不适用 (同上, 不是"根目录是空字符串")。
	StateDir string
}

// CollectAssemblyReport 从一个已构造的引擎采集装配自报。
func CollectAssemblyReport(eng *QueryEngine, in AssemblyReportInput) *assembly.Report {
	if eng == nil {
		return nil
	}
	r := &assembly.Report{
		SchemaVersion:      assembly.SchemaVersion,
		DeclarationVersion: in.DeclarationVersion,
		ConfigSource:       in.Config,
	}

	if cfg := eng.Config; cfg != nil {
		mi := &assembly.ModelInfo{Primary: cfg.Model, Execution: cfg.ExecutionModel, Fallback: cfg.FallbackModel}
		if cfg.PlanClient != nil {
			mi.Plan = cfg.PlanClient.Model
		}
		// 四个字段全空说明引擎没配模型 —— 不是"配了空模型"。
		if mi.Primary != "" || mi.Execution != "" || mi.Plan != "" || mi.Fallback != "" {
			r.Model = mi
		}

		r.Budget = &assembly.BudgetInfo{
			MaxTurns:      cfg.MaxTurns,
			MaxTokens:     cfg.MaxTokens,
			ContextWindow: cfg.ContextWindow,
		}
		r.Policy = &assembly.PolicyInfo{PermissionMode: string(cfg.PermissionMode)}
		r.Workspace = &assembly.WorkspaceInfo{Cwd: cfg.Cwd, StateDir: in.StateDir}
	}

	r.Tools = collectTools(eng)
	r.InternalHooks = eng.InternalHookPhases()
	r.ExternalHooks = collectExternalHooks(eng.HookRunner)
	r.Components = collectComponents(eng)

	var promptWarnings []string
	r.Prompt, promptWarnings = collectPromptInfo(eng.PromptMgr, eng.Tools)

	r.Warnings = append(AssemblyWarnings(r), promptWarnings...)
	return r
}

// ReconcileAssembly 采集 + 对账一步到位, 返回可直接落盘/上报的结论。
//
// decl 为 nil 时仍然产出一份**报告**: "没有声明可对"不等于"没有装配可看",
// 两件事压成一个 nil 会让"未对账"与"没采集"分不开。
func ReconcileAssembly(eng *QueryEngine, in AssemblyReportInput, decl *assembly.Declaration) (*assembly.Report, *assembly.ReconcileResult) {
	rep := CollectAssemblyReport(eng, in)
	if rep == nil {
		return nil, nil
	}
	return rep, assembly.NewReconcileResult(decl, rep)
}

// collectTools 采集工具面。
//
// 三个数必须分开取 (注册数 / 可见数 / 被挡数), 理由见 assembly.ToolsInfo 的注释:
// "注册了但看不见"与"根本没注册"是两件事, 压成一个数就分不出来了。
func collectTools(eng *QueryEngine) *assembly.ToolsInfo {
	if eng == nil || eng.Tools == nil {
		return nil
	}
	t := &assembly.ToolsInfo{
		Registered: len(eng.Tools.Names()),
		Visible:    eng.VisibleTools(),
		Hidden:     eng.HiddenTools(),
	}
	if cfg := eng.Config; cfg != nil {
		t.WhitelistSet = cfg.AllowedTools != nil
		t.Whitelist = sortedKeys(cfg.AllowedTools)
		t.Blacklist = sortedKeys(cfg.DisabledTools)
	}
	return t
}

// collectExternalHooks 采集外部 hook。
//
// 只取 Describe() 给的形状 (事件 / 类型 / 序号 / 有无条件 / 超时), **不取命令原文与
// URL** —— 那两样常带凭据与内网地址, 而本报告会被落盘、进 envelope、上页面。详见
// pkg/hooks.HookSummary 的注释。
func collectExternalHooks(runner *hooks.Runner) []assembly.ExternalHook {
	if runner == nil {
		return nil
	}
	sums := runner.Describe()
	out := make([]assembly.ExternalHook, 0, len(sums))
	for _, s := range sums {
		out = append(out, assembly.ExternalHook{
			Event:        s.Event,
			Type:         s.Type,
			Index:        s.Index,
			HasCondition: s.HasCondition,
			Timeout:      s.Timeout,
		})
	}
	return out
}

// collectComponents 采集前沿优化组件的装配情况。
//
// 键名取方案与配置里的通用叫法, 不用 Go 字段名 —— 面板与对账的读者是运维与平台侧,
// 他们看到的词必须与文档里 (20.6.2 的缺口表) 的词对得上。
func collectComponents(eng *QueryEngine) map[string]bool {
	if eng == nil {
		return nil
	}
	return map[string]bool{
		"metrics":          eng.Metrics != nil,
		"prompt_cache":     eng.PromptCache != nil,
		"budget_degrade":   eng.Budget != nil,
		"loop_detector":    eng.LoopDet != nil,
		"error_classifier": eng.ErrClassifier != nil,
		"json_repair":      eng.JSONRepair != nil,
		"trajectory":       eng.TrajStore != nil,
		"stop_signal":      eng.StopDet != nil,
	}
}

// collectPromptInfo 采集系统提示词的分段字符数, 并顺带返回分段布局上的提醒。
//
// 字符数而不是 token 数: 这是装配自报不是计费报告, 字符数足够回答"那一段到底有没有
// 进去", 且不必跑 tokenizer。
//
// VolatileTail 用**内容比对**判定而不是靠约定: 拿几个已知逐次变化的输入 (经验卡、
// 技能清单) 去各段里找, 落在哪段就标哪段。这样标出来的结论可以被复核 —— 而且它顺带
// 照出一个真问题: 逐次变化的内容若**不在末段**, 它之后的所有前缀每跑一次就作废一次,
// 那属于缓存布局违纪 (方案 20.3 改造 7)。
func collectPromptInfo(mgr *prompt.Manager, reg *tool.Registry) (*assembly.PromptInfo, []string) {
	if mgr == nil {
		return nil, nil
	}
	segs := mgr.BuildEffectiveSystemPrompt(reg)
	if len(segs) == 0 {
		return nil, nil
	}
	names := segmentNames(mgr, segs)
	info := &assembly.PromptInfo{Segments: map[string]int{}}
	for i, s := range segs {
		info.Total += len(s)
		info.Segments[names[i]] = len(s)
	}

	var warnings []string
	for _, probe := range []struct {
		name string
		text string
	}{
		{"experience_hints", mgr.ExperienceHints},
		{"skill_listing", mgr.SkillListing},
	} {
		if strings.TrimSpace(probe.text) == "" {
			continue
		}
		frag := firstNonEmptyLine(probe.text)
		if frag == "" {
			continue
		}
		for i, s := range segs {
			if strings.Contains(s, frag) {
				info.VolatileTail = append(info.VolatileTail, names[i])
				if i != len(segs)-1 {
					// 逐次变化的内容落在非末段 —— 其后每一段都要陪着重算。
					warnings = append(warnings, "系统提示词的逐次变化段 "+names[i]+
						" 不在末段 (共 "+strconv.Itoa(len(segs))+" 段): 它之后的前缀每跑一次即作废, "+
						"缓存布局违反「静态在前、逐工单变化在后」")
				}
				break
			}
		}
	}
	// 去重后排序, 让两次采样的结果可比。
	sort.Strings(info.VolatileTail)
	if len(info.VolatileTail) > 1 {
		uniq := info.VolatileTail[:1]
		for _, v := range info.VolatileTail[1:] {
			if v != uniq[len(uniq)-1] {
				uniq = append(uniq, v)
			}
		}
		info.VolatileTail = uniq
	}
	return info, warnings
}

// segmentNames 给 BuildEffectiveSystemPrompt 的分支结果起名字。
//
// 命名必须与 buildEffectiveSystemPrompt 的优先级链逐一对应 (override > coordinator >
// agent > custom > default, append 殿后), 否则报告里的段名会指错段 —— 而"哪一段进去
// 了"正是这张表唯一要回答的问题。
func segmentNames(m *prompt.Manager, segs []string) []string {
	var head string
	switch {
	case m.OverridePrompt != "":
		head = "override"
	case m.CoordinatorPrompt != "":
		head = "coordinator"
	case m.AgentPrompt != "":
		head = "agent"
	case m.CustomPrompt != "":
		head = "custom"
	default:
		head = "default"
	}
	names := make([]string, len(segs))
	for i := range segs {
		switch {
		case i == 0:
			names[i] = head
		// custom 分支下, 中间那一段可能是 MCP 指引 (仅在非空时才追加), 末段是 append。
		case head == "custom" && i == 1 && len(segs) == 3:
			names[i] = "mcp_guidance"
		default:
			names[i] = "append"
		}
	}
	return names
}

// firstNonEmptyLine 取首个非空行作为探测片段 (整段做子串匹配会被前缀里的空白差异骗过)。
//
// 取第一行可能为空, 这里跳过空行 —— 探测片段若取到空串会退化成「任何段都包含它」,
// 于是每段都被标成逐次变化。
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// AssemblyWarnings 产出给人看的装配提醒。不参与对账 —— 对账比的是"声明 vs 实际",
// 而这里说的是"实际本身就有问题"。
func AssemblyWarnings(r *assembly.Report) []string {
	var out []string
	if r == nil {
		return nil
	}
	if r.Tools != nil {
		if r.Tools.WhitelistSet && len(r.Tools.Whitelist) == 0 {
			out = append(out, "白名单已设但为空集: 一切工具对模型不可见 (fail-closed)。"+
				"若非本意, 说明岗位模板下发的工具面是空的")
		}
		if r.Tools.WhitelistSet && len(r.Tools.Whitelist) > 0 && len(r.Tools.Visible) == 0 {
			out = append(out, "白名单非空但可见工具为 0: 名单里的名字与注册表里的名字对不上")
		}
		// 名单里**逐项**指向注册表——白名单只对"名字刚好相等"的工具放行, 而平台
		// 下发岗位模板时大小写是按它自己的口径写的。一条谁都放不进的白名单不会
		// 报错, 只会静默少给工具; 这里把它点出来。注意这属于"实际本身有问题"
		// (观测), 不是"声明与生效不一致"(对账) —— 描述符里就是这么写的, harness
		// 照做了, 谁的错在描述符的作者那边。
		if r.Tools.WhitelistSet && len(r.Tools.Whitelist) > 0 {
			// Visible ∪ Hidden = 注册表全集, 故不在 Visible 里即不在注册表里。
			visible := make(map[string]bool, len(r.Tools.Visible))
			for _, n := range r.Tools.Visible {
				visible[n] = true
			}
			for _, n := range r.Tools.Whitelist {
				if !visible[n] {
					out = append(out, "白名单项 "+n+
						" 放不进任何工具: 注册表里没有同名工具, 该条在名单里等于没有")
				}
			}
		}
	}
	if r.Components != nil {
		var missing []string
		for _, name := range []string{"error_classifier", "json_repair", "loop_detector", "prompt_cache"} {
			if !r.Components[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			// 这条正是 20.6.2 那个缺陷的现场特征: 护栏类组件缺席, 而运行一切正常。
			out = append(out, "护栏类组件未装配: "+strings.Join(missing, ", ")+
				" —— 对应的内置 hook 未注册, 该路径上这些护栏不生效")
		}
	}
	if r.InternalHooks != nil {
		empty := 0
		for _, names := range r.InternalHooks {
			if len(names) == 0 {
				empty++
			}
		}
		if empty > 0 {
			out = append(out, "内置 hook 链有 "+strconv.Itoa(empty)+
				" 个相位为空 (PostCompact/OnStreamDelta/OnRecovery 为空是设计如此, 专供外部 hook)")
		}
	}
	return out
}

// sortedKeys 把 map[string]bool 的键排序取出。nil 与空 map 都返回 nil —— 报告里
// "没设"与"设了空"由 WhitelistSet 表达, 不该让这里再制造一个同形的歧义。
func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
