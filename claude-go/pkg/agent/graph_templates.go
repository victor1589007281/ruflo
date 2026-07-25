package agent

// graph_templates —— 15 种 mode → 图模板映射 (design/01 §五)。
//
// ---------------------------------------------------------------------------
// 模板是什么, 不是什么
// ---------------------------------------------------------------------------
//
// 一个 mode 的图模板 = 把该 mode **执行器真实跑出来的阶段序列**编成 GraphSpec,
// 交给 pkg/graph 的 ready-set 调度器执行, 取代 mode 各自手写的一套调度。
// 模板产出的图随后仍走 TranslateWorkflow 直译器 (graph_adapter.go), 于是节点预算 /
// 图层重试 / 门禁元数据 / per-workflow 覆盖表这些能力对模板与普通 pipeline 完全一致
// —— 模板只负责"阶段序列长什么样", 不重新发明一套节点属性推导。
//
// **模板照抄源码的真实行为, 不照抄设计文档的理想图。** design/01 §五 那张表写于改造
// 之前, 与 HEAD 有多处偏差 (逐条见下方 modeGraphNotTemplated 与各模板注释)。凡设计
// 文档与源码冲突, 一律以源码为准; 硬按设计文档做会得到一个"看起来漂亮但与线上行为
// 不同"的模板, 那比不做更危险 —— 灰度打开即静默改变 8+ 个下游平台看到的阶段序列。
//
// ---------------------------------------------------------------------------
// 灰度与默认行为
// ---------------------------------------------------------------------------
//
// 模板只在 CLAUDE_GO_GRAPH_ENGINE=1 时才被使用 (WorkflowExecutor.Execute 的分发口)。
// 开关未设 ⇒ 每个 mode 仍走自己的既有执行器, 默认行为一字不变。
//
// ---------------------------------------------------------------------------
// 为什么只有 4 个 mode 有模板 (而不是 15 个)
// ---------------------------------------------------------------------------
//
// 等价的门槛是"同样的阶段、同样的顺序、同样的并发度、同样的门禁"。逐个读完 15 个
// 执行器后, 剩下 11 个 mode 里 1 个 (orchestrated) 已在 M4 用**专属节点内核**迁到图
// 引擎 (见 modeGraphNativeKernel), 另外 10 个各自缺的东西是**能力级**的,
// 不是工作量级的, 归为三类:
//
//  1. **自适应终止器 (AdaptiveTerminator) 驱动的循环**: 退出条件不是"分数过线",
//     而是 加权分 ≥6.0 / 收敛 ε=0.5 / 退化 0.3×2 次 / 策略转换 ≤2 次 / best-of-N
//     回滚 五路信号的组合 (pkg/agent/adversarial.go)。图的 LoopPolicy.Until 只有
//     ok|fail|score|output contains 四类原子条件 (pkg/graph/condition.go), 表达不了,
//     硬用 `score >= 6` 顶替会把"收敛/退化/回滚"三种终止悄悄改成"永远跑满轮次"。
//     命中: creative_media / app_composite / game_composite / novel_writing / swarm_novel。
//  2. **非 agent 执行内核**: 阶段不是一次 ExecuteSingleStage, 而是 裸 LLM completion、
//     swarm_intel 引擎、pkg/media 渲染或本地 shell 门禁。
//     stageNodeRunner 只会跑 ExecuteSingleStage —— 用它跑这些阶段等于**悄悄换了内核**
//     (最直接的后果: 裸 completion 阶段突然拿到了工具权限)。
//     命中: ensemble_extract / review_panel / plot_simulate /
//     plot_predict / swarm_novel / game_composite / creative_media。
//     orchestrated 原本也在这一类, M4 的解法不是硬塞进 stageNodeRunner 而是**为它写一个
//     裸 completion 的 NodeRunner** (orchestrated_runner.go) —— 这条路对上面几个 mode
//     同样成立, 是后续里程碑的模板。
//  3. **运行期才知道的图形状**: 节点名/节点数来自 LLM 产出 (WBS 任务标题、章节数、
//     时间线数)。图侧对应能力是 ExpandSpec 动态展开, 但它要求 runner 能把上游产出
//     解析成 []NodeSpec —— 那是每个 mode 一个解析器的活, 且解析失败的降级语义
//     (fail-open 还是 fail-closed) 各不相同。
//     命中: adversarial_dev / novel_writing / swarm_novel。
//
// 每条都在 modeGraphNotTemplated 里有一句话的记账, 且有守护测试保证
// "模板表 ∪ 专属内核表 ∪ 未做表 = 全部 15 个 mode" —— 将来新增 mode 忘了表态就会红。

import (
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/graph"
)

// ModeGraphTemplate 一个 mode 的图模板。
//
// Stages 返回该 mode **真实**的阶段序列 (按执行顺序; 阶段间的先后靠 DependsOn 表达,
// 无依赖的阶段由 ready-set 调度器自然并行)。返回 nil 阶段即视为模板不适用。
type ModeGraphTemplate struct {
	// Mode 模式名 (与 WorkflowDef.Mode 一致)。
	Mode string
	// Equivalence 等价性范围与已知差异 (灰度前必须能一眼读到边界, 否则"等价"就是空话)。
	Equivalence string
	// Stages 把工作流定义展开为该 mode 真实的阶段序列。
	Stages func(wf *WorkflowDef) ([]StageDef, error)
}

// modeGraphTemplates 已落地的 mode 图模板 (design/01 §五)。
//
// 只收**行为等价且有等价性测试**的 mode。不等价的宁可不收 —— 收进来就意味着灰度
// 打开后该 mode 的阶段序列变了, 而 mode 的调用方 (REPORT.md / 门禁统计 / dashboard /
// 下游平台的阶段解析) 全都读这个序列。
var modeGraphTemplates = map[string]ModeGraphTemplate{
	"pipeline": {
		Mode: "pipeline",
		Equivalence: "阶段序列 = wf.Stages 的 DependsOn 拓扑; ready-set 调度天然并行, " +
			"与 executePipeline 的 filterParallel 同并发度 (均取 effectiveParallel)。",
		Stages: identityTemplateStages,
	},
	"fanout": {
		Mode: "fanout",
		// 与 design/01 §五 的偏差 (以源码为准): 设计写 fanout → map→reduce "首次真正
		// 实现"。但 HEAD 的 executeFanOut 是一行转调 executePipeline 的空壳
		// (workflow.go), 真实行为就是 pipeline —— 改成 map→reduce 是**行为变更**,
		// 不是等价迁移: 阶段名会从 research-tech/market/risk 变成 <map节点>#0/#1/#2,
		// 阶段数随上游产出行数浮动, 下游按阶段名取产出的平台会直接读空。
		// 真 map→reduce 已在 pkg/graph/fanout.go 实现, 经覆盖表 kind:"map" 显式启用
		// (graph_adapter.go StageGraphOverride.Map), 属于 opt-in 的新能力而非本模板。
		Equivalence: "现状 executeFanOut 是转调 executePipeline 的空壳, 故模板与 pipeline 同构; " +
			"真 map→reduce 需经覆盖表显式声明 (行为变更, 不在等价迁移范围)。",
		Stages: identityTemplateStages,
	},
	"adversarial": {
		Mode: "adversarial",
		// 同上: executeAdversarial 也是转调 executePipeline 的空壳, 且 wf.Rounds 在这条
		// 路径上**从未被读取**。设计写的 loop-group[generate→critique] 会引入原本不存在
		// 的循环轮次 (阶段名带 #it<轮次>), 不等价。
		Equivalence: "现状 executeAdversarial 是转调 executePipeline 的空壳 (wf.Rounds 未被读取), " +
			"故模板与 pipeline 同构; loop-group 化会新增轮次, 属行为变更。",
		Stages: identityTemplateStages,
	},
	"trading_debate": {
		Mode: "trading_debate",
		Equivalence: "18 个阶段全静态 (轮数是包级常量 2+2, 不来自 wf.Rounds), 逐个展开为节点: " +
			"4 路分析并行 → Bull/Bear 串行辩论链 → research-manager → trader → " +
			"三方风险串行链 → portfolio-manager → signal-extractor。阶段名/角色/顺序/并发度逐项等价; " +
			"提示词由依赖累积 (占位符 {阶段名}) 复现辩论历史, 截断口径与 truncateForDebate(2000) 不同, " +
			"且生产上 11 个交易角色全部注册 ⇒ buildStagePromptWithRoles 走 MergedPrompt 分支, " +
			"stage.Prompt 本就被丢弃 (workflow.go), 该差异在生产路径上不可观测。",
		Stages: tradingDebateTemplateStages,
	},
}

// modeGraphNotTemplated 已核实"当前不可等价图化"的 mode → 原因 (design/01 §五 逐条交代)。
//
// 这张表是**承诺表的反面**: 有它才能保证"没做"是核实过的结论而不是遗漏。
// 每条原因写的是**能力缺口**, 不是"工作量不够"。
var modeGraphNotTemplated = map[string]string{
	"adversarial_dev": "Phase 2 的真实执行体是 pkg/agent.Orchestrator 的 WBS DAG: 节点名/节点数是 " +
		"ParsePlanToDAGWithRepair 从 planner 产出里解析出来的 (LLM 撰写的任务标题), 运行期才知道; " +
		"节点内还嵌 L2 编译硬门禁 (4 次内部重试 + CompileRepairGuard(3))。Phase 3 的 E2E 轮次由 " +
		"产出子串启发式 (含 bug/fail/错误) + AdaptiveTerminator(1,3) + 本地 shell 门禁 runLocalE2EGate 驱动。" +
		"缺 ExpandSpec 的 WBS 解析器 + 能跑 Orchestrator/shell 门禁的 NodeRunner。" +
		"另注: runAdversarialLoop / initAdaptiveTerminator / autoScalePool / runBuildGate / runTestGate " +
		"在 HEAD 已无调用方 (死代码), design/01 §五 写的 loop-group[coder→reviewer→fixer] 对应的是这段死代码, 不是线上行为。",
	"ensemble_extract": "工作流声明 0 个 Stages; 真实执行是 3 路差异化 lens 的裸 we.llm 调用 " +
		"(swarm_intel.FanOutCollect, MaxConcurrency=4/单路 6min/总 450s), 再做投票融合 " +
		"(confidence = 命中路数/总路数, 证据截 3 条)。map 形态能表达扇出, 但缺 (a) 裸 completion 的 " +
		"NodeRunner (b) 投票融合这一确定性 reduce 策略 —— 现有策略只有 runner/concat/longest。",
	"review_panel": "同 ensemble_extract: 0 个 Stages + 3 路裸 LLM 人格评审 + 截尾均值融合 " +
		"(ByzantineFuser(0.34), N=3 时取中位数, 末尾还做跨维度 normalize 使分数变成占比)。" +
		"缺裸 completion NodeRunner + 截尾均值 reduce 策略。",
	"plot_simulate": "0 个 Stages, 单次 swarm_intel.Engine.Simulate(Agents=3, Rounds=2) 后包一条 " +
		"StageResult。整个 mode 没有可调度的阶段结构, 图化只能是 1 节点包壳; 真要收编需要一个 " +
		"swarm_intel 内核的 NodeRunner, 收益是 journal/hook 统一而非调度。",
	"plot_predict": "同 plot_simulate: 单次 swarm_intel.Engine.Predict, 阈值全在引擎内部 " +
		"(DTIThreshold 0.7 / TrimRatio 0.2 / DebateEntropy 0.7)。",
	"novel_writing": "Phase 1/2 的 4 个阶段确实是静态串行, 但 Phase 3 的章节数来自 LLM 大纲产出 " +
		"(parseChapterCount, 夹到 1..30, 默认 5), 每章还套 AdaptiveTerminator(1,3); " +
		"章节阶段走 we.runAgent (刻意绕过 RoleRegistry.MergedPrompt), 与 ExecuteSingleStage 不同内核; " +
		"Phase 4 的 book-assembly 失败也 fail-open 不影响整体。缺 ExpandSpec 章节展开器 + 自适应终止条件。",
	"swarm_novel": "Phase B 是 swarm_intel.FanOutFirstN 跑 TimelineCount+1 条时间线取前 N 条成功的, " +
		"时间线数/轮数来自 evo-blueprint 的 LLM JSON (2..5 / 3..10); Phase C 的 Predict 失败会降级为单 agent " +
		"story-judge 并**把状态改写回 completed**; Phase F 的统稿失败按条件降级。三处都是内容驱动的动态形状, " +
		"叠加 novel_writing 的全部缺口。",
	"creative_media": "对抗轮由 AdaptiveTerminator(1,5) 驱动; Phase 4 的 media-render 是 pkg/media 引擎 " +
		"(chromedp/ffmpeg) 而非 agent; 还有一条视觉质检环 (gemma 视觉后端, 单次修复) 和一条 manim 分支 " +
		"(IsAlgorithmExplainerVideo 命中时整体换成 3 阶段的另一套形状)。缺确定性渲染节点内核 + 自适应终止条件。",
	"app_composite": "无跨团队编排 (核实: 不调 RunTeam/TeamManager), 但 Phase B 的原型对抗环由 " +
		"AdaptiveTerminator(1,4) 驱动 (parseCreativeScore 从不设 PassSet, 于是 reviewer 的 pass:false 无法否决), " +
		"且 media.Engine.RenderAll 是确定性渲染而非 agent。缺自适应终止条件 + 渲染节点内核。" +
		"另注: design/01 §五 写的 subgraph 组合并不需要 —— 它从头到尾在一个 executor 内跑, 没有子团队。",
	"game_composite": "同 app_composite (AdaptiveTerminator(1,3) + media 渲染), 另加 Phase B 的 " +
		"swarm_intel 三路情景模拟 (硬编码 {5,3}/{4,2}/{3,2}, 自带 sem=3 并发闸)。同样不需要 subgraph。",
}

// modeGraphNativeKernel 已在图引擎上跑、但**不经 stageNodeRunner 模板路径**的 mode
// (design/01 M4)。第三类的存在理由:
//
//	modeGraphTemplates    = "阶段序列可等价展开, 灰度开关命中即切图, 内核仍是 stageNodeRunner"
//	modeGraphNotTemplated = "现在还不能等价图化, 缺哪项能力逐条记账"
//	modeGraphNativeKernel = "已无条件在图引擎上跑, 但配的是该 mode 专属的 NodeRunner"
//
// 把 orchestrated 塞进前两张表任何一张都会说谎: 它既不是"灰度可切"(已经无条件切了,
// 入口在 executeOrchestrated —— 那里有 LLMClient 缺失时的降级与飞书通知, 绕过它会
// 丢掉降级), 也不是"还没图化"。ModeHasGraphTemplate 因此**不含**它, 于是
// workflow.go 的灰度分发口不会把它劫走。
var modeGraphNativeKernel = map[string]string{
	"orchestrated": "已迁至 pkg/graph 图引擎 (design/01 M4, 退役 pkg/orchestrator): 图结构 = wf.Stages 的 " +
		"1:1 DAG, 执行内核换成专属的 orchNodeRunner —— 裸 LLM completion (无工具/无角色模板合并/无黑板交接), " +
		"对抗阶段走内层 3 轮 + QualityTermination(7.0, 0.5), 重试按 fatal/transient/permanent 分档 (0/2/7 次), " +
		"AND-join 与级联取消由 runner 的级联闸复刻。等价性由 orchestrated_equiv_test.go 四维比对钉住 " +
		"(阶段序列/LLM 调用次数/峰值并发/提示词逐字)。刻意保留的旧行为与刻意丢掉的能力见 " +
		"workflow_orchestrated.go 文件头。",
}

// ModeHasGraphTemplate 该 mode 是否有已验证等价的图模板 (灰度分发的唯一判据)。
//
// 注意它**不**回答"该 mode 是否跑在图引擎上" —— orchestrated 跑在图引擎上但不在这张表里
// (见 modeGraphNativeKernel 的说明)。灰度分发口只该认这张表。
func ModeHasGraphTemplate(mode string) bool {
	_, ok := modeGraphTemplates[mode]
	return ok
}

// ModeGraphTemplateEquivalence 返回某 mode 模板的等价性说明 (无模板时返回未做的原因)。
// 供 CLI/dashboard 在灰度前把边界直接呈现给运维, 而不是让人去翻源码注释。
// 第二个返回值 = 该 mode 的阶段序列是否已被等价验证 (专属内核 mode 同样为 true)。
func ModeGraphTemplateEquivalence(mode string) (string, bool) {
	if tpl, ok := modeGraphTemplates[mode]; ok {
		return tpl.Equivalence, true
	}
	if doc, ok := modeGraphNativeKernel[mode]; ok {
		return doc, true
	}
	if reason, ok := modeGraphNotTemplated[mode]; ok {
		return reason, false
	}
	return "", false
}

// identityTemplateStages 恒等模板: 阶段序列就是工作流声明的 Stages。
// 用于那些"执行器本身就是 DependsOn 拓扑执行"的 mode (pipeline) 以及
// "执行器是转调 pipeline 的空壳"的 mode (fanout / adversarial)。
func identityTemplateStages(wf *WorkflowDef) ([]StageDef, error) {
	if wf == nil || len(wf.Stages) == 0 {
		return nil, fmt.Errorf("graph_templates: 工作流 %q 没有阶段可展开", workflowName(wf))
	}
	return wf.Stages, nil
}

// BuildModeGraph 按 mode 模板产出 GraphSpec。
//
// 两步: ① 模板把 mode 展开成真实阶段序列; ② 交给 TranslateWorkflow 直译。
// 第二步是刻意的 —— 节点预算/图层重试/门禁元数据/覆盖表都只在直译器里有一份实现,
// 模板另抄一份必然漂移 (这正是 design/01 §1.2 双 mode switch 那个缺陷的形态)。
func BuildModeGraph(wf *WorkflowDef) (graph.GraphSpec, error) {
	if wf == nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates: 空工作流")
	}
	tpl, ok := modeGraphTemplates[wf.Mode]
	if !ok {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates: mode %q 没有图模板", wf.Mode)
	}
	stages, err := tpl.Stages(wf)
	if err != nil {
		return graph.GraphSpec{}, err
	}
	if len(stages) == 0 {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates: mode %q 的模板展开出 0 个阶段", wf.Mode)
	}
	// 合成体只换 Stages: Name 保持不变, 于是 per-workflow 覆盖表 / 门禁元数据 /
	// QualityGate 声明对模板图与直译图是同一套 (LookupGraphOverride 按 wf.Name 查表)。
	synth := *wf
	synth.Stages = stages
	spec, err := TranslateWorkflow(&synth)
	if err != nil {
		return graph.GraphSpec{}, err
	}
	spec.Version = "template-" + wf.Mode
	return spec, nil
}

// graphSpecForWorkflow 图引擎的图来源单一入口: 有 mode 模板用模板, 否则直译 wf.Stages。
//
// 由 executeGraph 调用。放在这里而不是 graph_adapter.go 是为了让"模板库"这件事只有
// 一个落点: 加一个 mode 模板 = 往 modeGraphTemplates 加一行, 不必碰直译器与执行入口。
func graphSpecForWorkflow(wf *WorkflowDef) (graph.GraphSpec, error) {
	if wf != nil && ModeHasGraphTemplate(wf.Mode) {
		return BuildModeGraph(wf)
	}
	return TranslateWorkflow(wf)
}

func workflowName(wf *WorkflowDef) string {
	if wf == nil {
		return ""
	}
	return wf.Name
}

// ---------------------------------------------------------------------------
// 依赖顺序: 让图路径的 {prev_result} 与 pipeline 一致
// ---------------------------------------------------------------------------

// graphNodeDeps 从 GraphSpec 抽出"节点 → 上游节点 ID (按边声明序)"。
//
// 为什么必须有这张表: stageNodeRunner 造 StageDef 时若不填 DependsOn,
// buildStagePromptWithRoles 遍历的就是空依赖列表 ⇒ **`{prev_result}` 恒被替换成空串**,
// 上游产出根本进不了提示词 (workflow.go buildStagePromptWithRoles / substituteStagePlaceholders)。
// 这是图路径与 pipeline 路径之间一处真实的行为差异, 任何模板的等价性都以它为前提。
//
// 顺序取**边声明序**而不是 map 遍历序或字典序: pipeline 侧拼接依赖产出用的是
// StageDef.DependsOn 的声明序, 排序或随机化都会让同一个阶段在两条路径上拿到
// 不同的提示词 (进而不同的模型产出与不同的 prompt 缓存命中)。
//
// 组内子图 (loop-group) 的成员边一并收进来: 成员节点的运行期 ID 形如
// <组>#it<轮次>/<成员>, 经 graphBaseNodeID 归一后仍能查到自己的组内上游。
func graphNodeDeps(spec graph.GraphSpec) map[string][]string {
	deps := map[string][]string{}
	var collect func(nodes []graph.NodeSpec, edges []graph.EdgeSpec)
	collect = func(nodes []graph.NodeSpec, edges []graph.EdgeSpec) {
		for _, ed := range edges {
			deps[ed.To] = append(deps[ed.To], ed.From)
		}
		for _, n := range nodes {
			if n.Group != nil {
				collect(n.Group.Nodes, n.Group.Edges)
			}
		}
	}
	collect(spec.Nodes, spec.Edges)
	return deps
}

// graphBaseNodeID 把引擎合成的运行期节点 ID 归一为**声明期**节点 ID。
//
// 引擎会按分隔符合成三类派生 ID (pkg/graph/spec.go):
//
//	map 分片      <map节点>#<序号>
//	组内成员      <组节点>#it<轮次>/<成员>
//	动态展开产物  <父节点>/<子节点>
//
// 归一规则: 先取最后一个 "/" 之后 (拿到成员/子节点名), 再截到第一个 "#" 之前
// (去掉分片序号)。顺序不能反 —— 组内 ID 的 "#it<轮次>" 在 "/" 左边, 先截 "#"
// 会把整个成员名丢掉。
func graphBaseNodeID(id string) string {
	if i := strings.LastIndex(id, graph.GroupMemberIDSep); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.Index(id, graph.ShardIDSep); i >= 0 {
		id = id[:i]
	}
	return id
}

// orderedPrevDeps 按声明序给出本节点"有产出的上游"列表, 供 StageDef.DependsOn 使用。
//
// 只保留 prev 里真有产出的上游 (与 pipeline 侧 `if r, ok := prevResults[dep]` 的口径
// 一致); 查不到声明期依赖时 (例如动态展开出来的节点) 退化为按键名升序 —— 退化也必须
// 是确定性的, 否则同一节点两次运行的提示词会随 map 遍历序抖动。
func orderedPrevDeps(deps map[string][]string, nodeID string, prev map[string]string) []string {
	if len(prev) == 0 {
		return nil
	}
	declared := deps[nodeID]
	if len(declared) == 0 {
		declared = deps[graphBaseNodeID(nodeID)]
	}
	if len(declared) == 0 {
		keys := make([]string, 0, len(prev))
		for k := range prev {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	out := make([]string, 0, len(declared))
	seen := make(map[string]bool, len(declared))
	for _, d := range declared {
		if seen[d] {
			continue
		}
		if _, ok := prev[d]; !ok {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// ---------------------------------------------------------------------------
// trading_debate 模板
// ---------------------------------------------------------------------------

// tradingDebateTemplateStages 把 executeTradingDebate 的五段式流程展开为 18 个阶段。
//
// 真实行为 (workflow_trading_v2.go, 以源码为准):
//
//	Phase 1  executeParallel(wf.Stages[0..3])          → 4 路分析, 并发 effectiveParallel
//	Phase 2  for round 1..defaultDebateRounds:          → bull-round<r> → bear-round<r> 严格串行
//	         再 research-manager 无条件裁决
//	Phase 3  trader
//	Phase 4  for round 1..defaultRiskRounds:            → 激进 → 保守 → 中性 严格串行
//	         再 portfolio-manager 最终裁决
//	Phase 5  signal-extractor (角色仍是 portfolio-manager)
//
// 三个关键取舍:
//
//  1. **轮数写死为包级常量 defaultDebateRounds/defaultRiskRounds**, 不读 wf.Rounds ——
//     执行器就是这么写的 (trading-v2 的 Rounds 字段从未被赋值, 也从未被读取)。
//     改成读 wf.Rounds 会让"给 trading-v2 设 Rounds=3"这件事在两条路径上行为不同。
//  2. **辩论的串行性靠依赖链表达, 不靠"顺序执行"**: 每个辩论阶段依赖它前面**全部**
//     辩论阶段 + 4 路分析。这同时拿到两个语义: ① 拓扑上不可能并行 (与执行器的 for
//     循环等价); ② prevResults 里累积着全部历史, 对应执行器手工拼的 debateHistory。
//     只依赖"上一个"会让历史在第 2 轮丢掉第 1 轮 —— 那是行为变更。
//  3. **提示词用占位符引用具体阶段** ({tech-analysis} 等, 由 substituteStagePlaceholders
//     解析), 而不是在模板里 fmt.Sprintf 真实产出 —— 图是纯数据, 翻译期没有也不该有
//     运行期产出 (design/01 设计原则 1)。
func tradingDebateTemplateStages(wf *WorkflowDef) ([]StageDef, error) {
	const wantAnalysis = 4
	if wf == nil || len(wf.Stages) < wantAnalysis {
		return nil, fmt.Errorf("graph_templates: trading_debate 模板要求工作流至少声明 %d 个分析阶段 (现有 %d 个); "+
			"执行器也是按下标取 wf.Stages[0..3] 的", wantAnalysis, len(wf.Stages))
	}

	// —— Phase 1: 四路并行分析 ——
	// 直接取工作流定义的前 4 个阶段 (执行器按下标取, 连 DependsOn/Parallel 都不看),
	// 但要清掉 DependsOn: trading-v2 定义里这 4 个阶段本就无依赖, 清掉是防御
	// 未来有人给它们加依赖后模板与执行器悄悄分叉。
	stages := make([]StageDef, 0, 18)
	analysisNames := make([]string, 0, wantAnalysis)
	for i := 0; i < wantAnalysis; i++ {
		st := wf.Stages[i]
		st.DependsOn = nil
		st.Parallel = true
		stages = append(stages, st)
		analysisNames = append(analysisNames, st.Name)
	}
	analysisRef := placeholderRefs(analysisNames)

	// —— Phase 2: Bull/Bear 串行辩论 + research-manager 裁决 ——
	debateSides := []struct{ name, role, prompt string }{
		{"bull", "bull-researcher", bullResearcherPrompt},
		{"bear", "bear-researcher", bearResearcherPrompt},
	}
	var debateNames []string
	for round := 1; round <= defaultDebateRounds; round++ {
		for _, side := range debateSides {
			name := fmt.Sprintf("%s-round%d", side.name, round)
			stages = append(stages, StageDef{
				Name: name, Role: side.role,
				// 三个 %s = 标的 / 分析师报告 / 辩论历史
				Prompt:    fmt.Sprintf(side.prompt, "{objective}", analysisRef, placeholderRefs(debateNames)),
				DependsOn: concatNames(analysisNames, debateNames),
			})
			debateNames = append(debateNames, name)
		}
	}
	stages = append(stages, StageDef{
		Name: "research-manager", Role: "research-manager",
		Prompt:    fmt.Sprintf(researchManagerPrompt, "{objective}", analysisRef, placeholderRefs(debateNames)),
		DependsOn: concatNames(analysisNames, debateNames),
	})

	// —— Phase 3: Trader ——
	stages = append(stages, StageDef{
		Name: "trader", Role: "trader",
		// 两个 %s = 标的 / research-manager 结论
		Prompt:    fmt.Sprintf(traderPrompt, "{objective}", "{research-manager}"),
		DependsOn: []string{"research-manager"},
	})

	// —— Phase 4: 三方风险串行辩论 + portfolio-manager 终裁 ——
	riskPerspectives := []struct{ name, role, prompt string }{
		{"aggressive-risk", "aggressive-risk", aggressiveRiskPrompt},
		{"conservative-risk", "conservative-risk", conservativeRiskPrompt},
		{"neutral-risk", "neutral-risk", neutralRiskPrompt},
	}
	var riskNames []string
	for round := 1; round <= defaultRiskRounds; round++ {
		for _, p := range riskPerspectives {
			name := fmt.Sprintf("%s-round%d", p.name, round)
			stages = append(stages, StageDef{
				Name: name, Role: p.role,
				// 三个 %s = 标的 / 交易方案 / 风险辩论历史
				Prompt:    fmt.Sprintf(p.prompt, "{objective}", "{trader}", placeholderRefs(riskNames)),
				DependsOn: concatNames([]string{"trader"}, riskNames),
			})
			riskNames = append(riskNames, name)
		}
	}
	stages = append(stages, StageDef{
		Name: "portfolio-manager", Role: "portfolio-manager",
		Prompt:    fmt.Sprintf(portfolioManagerPrompt, "{objective}", "{trader}", placeholderRefs(riskNames)),
		DependsOn: concatNames([]string{"trader"}, riskNames),
	})

	// —— Phase 5: 信号提取 (角色沿用 portfolio-manager, 与执行器一致) ——
	// 注意: 量化风控 applyRiskGuardrails 只影响通知文案, 不写回任何 StageResult
	// (workflow_trading_v2.go), 所以它不属于阶段序列, 模板里也不该出现。
	stages = append(stages, StageDef{
		Name: "signal-extractor", Role: "portfolio-manager",
		Prompt:    fmt.Sprintf(signalExtractorPrompt, "{portfolio-manager}"),
		DependsOn: []string{"portfolio-manager"},
	})
	return stages, nil
}

// placeholderRefs 把阶段名列表拼成 "{a}\n\n{b}" 形式的占位符引用块 (空列表 → 空串)。
// substituteStagePlaceholders 会把每个 {阶段名} 换成该阶段产出的摘要。
func placeholderRefs(names []string) string {
	if len(names) == 0 {
		return ""
	}
	refs := make([]string, 0, len(names))
	for _, n := range names {
		refs = append(refs, "{"+n+"}")
	}
	return strings.Join(refs, "\n\n")
}

// concatNames 拼接依赖名列表 (返回新切片: 调用方持有的 riskNames 等还要继续追加)。
func concatNames(head, tail []string) []string {
	out := make([]string, 0, len(head)+len(tail))
	out = append(out, head...)
	out = append(out, tail...)
	return out
}
