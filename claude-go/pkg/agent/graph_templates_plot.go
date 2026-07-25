package agent

// graph_templates_plot —— plot_simulate / plot_predict 两个"群体智能焦点"mode 的图装配
// (design/01 §五, 承接 ensemble_extract / review_panel 那条"专属内核"路线)。
//
// ---------------------------------------------------------------------------
// 上一轮给它们记的缺口, 与这次真正补上的东西
// ---------------------------------------------------------------------------
//
// 旧记账原文: "0 个 Stages, 单次 swarm_intel.Engine.Simulate/Predict 后包一条 StageResult。
// 整个 mode 没有可调度的阶段结构, 图化只能是 1 节点包壳; **真要收编需要一个 swarm_intel
// 内核的 NodeRunner**, 收益是 journal/hook 统一而非调度。"
//
// 这一轮补的正是那句话里点名的东西: plotSwarmNodeRunner。缺口描述里"图化只能是 1 节点
// 包壳"这句仍然**完全成立** —— 图确实只有 1 个节点, 本文件没有假装它变成了 DAG。收编的
// 理由不是调度, 而是这四样此前对这两个 mode 完全缺席的能力:
//
//	journal      引擎跑完落 <团队>/graph-journal/journal.jsonl; 进程被杀后重跑吃缓存
//	             (Resume=true), 而 Predict 是一次十几分钟、几十次 LLM 调用的重活。
//	hook         teamGraphHooks 把节点起止刷进 team.Stages ⇒ dashboard 上"正在跑哪一步"
//	             第一次可见 (旧路径整条团队只有一条 completed 记录, 跑的时候是黑的)。
//	拦截器        graphInterceptors 的 BudgetManager 对这两个 mode 生效 (此前恒不生效)。
//	运行参数      graphRunParams 的 run.id / team.* 进 ctx, 与其余 mode 同一口径。
//
// **journal 那条同时是一处已知的行为差异, 必须记账**: 图路径开 Resume=true, 于是
// **同一 RunID 重入**时已完成的节点直接吃缓存 (零 LLM、零通知、不重算), 而旧路径每次
// 都会重跑一遍引擎。日常调用不受影响 (每次团队运行取新 RunID, 缓存不会命中), 命中的
// 场景只有"同一次运行被中断后按同一 RunID 续跑" —— 那正是要的语义。
// 代价是收尾通知在吃缓存那次不会发 (没有统计可报, 见 executePlotSwarmGraph 的 ran 判断);
// 发一条数字全是 0 的 "✅ 完成" 比不发更误导。
//
// ---------------------------------------------------------------------------
// 为什么图只有 1 个节点 (而不是把 Predict 的 7 个 Phase 拆开)
// ---------------------------------------------------------------------------
//
// Engine.Predict 内部是 decompose → scout → predict → (DTI/Boids 多样性) → debate →
// fuse → byzantine trim → conformal 八段, 看起来天然是一张图。但拆开**不是等价迁移**:
//
//  1. 阶段序列会从 1 条变成 8 条。plot-predict 的调用方 (织叙 StoryLoom 的走向评估面板)
//     按 StageResult.Name == "plot-predict" 取产出, 多出来的 7 条会让它读到一堆
//     不认识的阶段, 而真正的产出藏在第 8 条里。
//  2. Phase 间传的是 *PredictionDomain / []AgentPrediction / *FusedPrediction 这些**结构体**,
//     不是文本。图的节点产出是 string —— 拆开就得在每个边界上序列化再解析一遍,
//     那不是"把调度交给图", 那是重写一遍引擎。
//  3. debate 轮数由 Boids 分歧度与预算共同决定 (engine.go 的 for + ShouldDebate 门控),
//     图上要表达它就得再配一个终止器, 而终止器的输入只有 NodeResult.Score 这个标量,
//     Boids 的分歧度矩阵表达不了 (terminator.go 文件头 (a) 记的同一条能力边界)。
//
// 所以本文件的立场是明确的: **1 个节点就是 1 个节点**。它换来的是上面四样统一能力,
// 不换调度 —— 把"1 节点包壳"说成"已图化"才是把账记糊。
//
// ---------------------------------------------------------------------------
// 刻意保留 / 刻意偏离 (逐条)
// ---------------------------------------------------------------------------
//
//  1. **零重试**: 旧路径对 Simulate/Predict 只调一次, 失败即整个 mode 报错。故图级
//     DefaultRetry 与节点 Retry 都显式填 MaxRetries=0。不填会落到直译器的图层默认 6 次
//     —— 一次 Predict 是几十次 LLM 调用, 重试 6 轮的 token 账是七倍。
//  2. **TimeoutSec 必须是 0 (不限)**。这条不是省事, 是硬约束: Engine.Predict 用
//     `ctx.Deadline()` 反推自己的分级预算 (engine.go: `totalBudget = time.Until(dl)`,
//     无 deadline 时取 15min)。图节点一旦设了 TimeoutSec, 引擎就会拿这个 deadline 去
//     切 decompose/scout/predict/debate 各段的子预算 —— 也就是说**设一个"更宽松"的
//     超时也会改掉引擎内部的行为**。旧路径不设 deadline, 图路径就必须也不设。
//  3. **notify 逐字照抄**: 起始/进度/收尾三类文案与旧路径完全相同, 且进度回调仍写
//     黑板的 swarm-progress 键 (织叙侧读它做实时进度条)。图引擎自己的 hook 不发通知
//     (只写日志与 team.Stages 快照), 所以飞书侧观感一字不变。
//  4. **LLMClient 前置检查留在灰度判据之前**: 两条路径都需要它, 且错误文案必须相同
//     ("plot-simulate 需要 LLMClient 以创建 swarm_intel.Engine")。放在后面会让同一个
//     配置错误在开关两侧报出不同的错。
//  5. **team == nil 时不切图**: journal 落在 team.dataDir 下, 图路径对 team 是硬依赖,
//     而旧路径只用它发通知 (与 ensemble 同款判断)。
//  6. **失败时返回 (nil, 原错误)**: 旧路径 `return nil, fmt.Errorf("剧情模拟失败: %w", err)`
//     —— 不返回半截 StageResult。图路径把引擎错误原样接住再返回, 而不是让调用方看到
//     图引擎自己的 "图执行失败" 文案 (那会让织叙侧的错误提示从"剧情模拟失败: 上游 429"
//     变成一句无信息量的 "graph_adapter: 图执行失败")。
//
// 等价性由 graph_templates_plot_test.go 四维比对钉住 (阶段序列 / LLM 调用次数 /
// 峰值并发 / 提示词), 外加**飞书通知序列逐字**、产出文本逐字/逐字段比对与五条变异反证。
// 通知序列那一维是第 2 条决策唯一可观测的地方 —— 超时改了提示词里看不出来, 但
// "⏱️ 预测总预算 15m0s (decompose 1m30s)" 这句会立刻变。
//
// **plot_predict 的提示词只能比对到"确定性的那几类"**: 引擎的辩论提示词里嵌了
// map 打印 (键序随机) 与"参与辩论的角色名单"(forceDiversity 顶掉的是**最后完成**的那位,
// 取决于 goroutine 调度) —— 这两处在旧路径上本来就每次不同, 逐条实测记在测试文件头。

import (
	"context"
	"fmt"
	"sync"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

// plotGraphKind 一个群体智能焦点 mode 的装配参数。
//
// 做成数据而不是两份代码: 两个 mode 的差异只有 4 处字面量 (节点名/角色/起始通知/
// 引擎调用体), 其余 (LLMClient 检查、灰度判据、引擎装配、通知闭包、黑板回写、结果包壳)
// 逐字相同 —— 抄两遍必然漂移, 而漂移的表现是"某一个 mode 的灰度行为悄悄不一样了"。
type plotGraphKind struct {
	// Mode 模式名 (日志/错误文案与 span 名)。
	Mode string
	// NodeID 图里唯一那个节点的 ID = 旧路径 StageResult.Name, 同时也是错误文案里的自称。
	NodeID string
	// Role 节点角色 = 旧路径 StageResult.Role。
	Role string
	// StartNotify 起始通知文案 (与旧路径逐字一致)。
	StartNotify string
	// Run 引擎调用 + 产出装配, 返回 (产出文本, 收尾通知文案, 错误)。
	// **错误必须已按旧路径文案包好** (剧情模拟失败: %w / 走向评估失败: %w),
	// 入口原样返回它 —— 见文件头第 6 条。
	Run func(ctx context.Context, engine *swarm_intel.Engine, chatID, objective string) (output, done string, err error)
}

// plotSimulateKind plot-simulate 的装配参数。
func plotSimulateKind() plotGraphKind {
	return plotGraphKind{
		Mode:        "plot_simulate",
		NodeID:      "plot-simulate",
		Role:        "plot-simulator",
		StartNotify: "⚡ **剧情模拟**: swarm_intel.Engine.Simulate 多情景群体推演中…",
		Run:         runPlotSimulateCore,
	}
}

// plotPredictKind plot-predict 的装配参数。
func plotPredictKind() plotGraphKind {
	return plotGraphKind{
		Mode:        "plot_predict",
		NodeID:      "plot-predict",
		Role:        "plot-analyst",
		StartNotify: "⚡ **走向评估**: swarm_intel.Engine.Predict 多分析师辩论融合中…",
		Run:         runPlotPredictCore,
	}
}

// ---------------------------------------------------------------------------
// 两条路径共用的前置: LLMClient 检查 / 灰度判据 / 引擎装配 / 结果包壳
// ---------------------------------------------------------------------------

// executePlotSwarm 群体智能焦点 mode 的统一入口 (旧路径 + 灰度切图判据)。
//
// 顺序是语义 (见文件头第 4、5 条): 先 LLMClient 检查, 再灰度判据, 最后才是旧路径。
func (we *WorkflowExecutor) executePlotSwarm(
	ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam, k plotGraphKind,
) ([]StageResult, error) {
	if we.llm == nil {
		return nil, fmt.Errorf("%s 需要 LLMClient 以创建 swarm_intel.Engine", k.NodeID)
	}
	if graphEngineEnabled() && team != nil {
		return we.executePlotSwarmGraph(ctx, wf, objective, team, k)
	}
	notify := plotNotifier(we, team)
	notify(k.StartNotify)
	output, done, err := k.Run(ctx, newPlotSwarmEngine(we.llm, team, notify), teamChatID(team), objective)
	if err != nil {
		return nil, err
	}
	notify(done)
	return []StageResult{{Name: k.NodeID, Role: k.Role, Status: TaskCompleted, Output: output}}, nil
}

// plotNotifier 通知闭包 (team 可能为 nil)。
func plotNotifier(we *WorkflowExecutor, team *ProductionTeam) func(string) {
	return func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}
}

// newPlotSwarmEngine 按旧路径的口径装配 swarm_intel 引擎。
//
// cfg 一律取 DefaultConfig(): 阈值 (DTIThreshold 0.7 / TrimRatio 0.2 / DebateEntropy 0.7)
// 与持久化目录 (~/.claude-go/swarm_intel) 全在引擎内部, 图层不该也无法改写它们。
// 唯一被改的是 Notify —— 它把引擎进度同时送到飞书与黑板 (织叙侧读 swarm-progress 做进度条)。
func newPlotSwarmEngine(llm swarm_intel.LLMClient, team *ProductionTeam, notify func(string)) *swarm_intel.Engine {
	cfg := swarm_intel.DefaultConfig()
	cfg.Notify = func(_, msg string) {
		notify("  [群体智能] " + msg)
		if team != nil && team.Blackboard != nil {
			team.Blackboard.Write("swarm-progress", msg, "engine", "progress")
		}
	}
	return swarm_intel.NewEngine(llm, cfg)
}

// ---------------------------------------------------------------------------
// 图装配
// ---------------------------------------------------------------------------

// plotSwarmGraphSpec 造图: 1 个 agent 节点, 零重试, 不设超时。
//
// 为什么不复用 TranslateWorkflow: 这两个 mode 的 WorkflowDef **声明 0 个 Stages**,
// 直译器无从下手; 而且直译器会按角色名推预算 (graphStageBudgetSec) 给节点塞一个
// TimeoutSec, 那会经 ctx.Deadline() 改掉 Engine.Predict 的内部分级预算 (文件头第 2 条)。
func plotSwarmGraphSpec(wf *WorkflowDef, k plotGraphKind) (graph.GraphSpec, error) {
	if wf == nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates_plot: %s 的工作流为空", k.Mode)
	}
	// 零重试: 两处 (图级默认 + 节点级) 都填, 否则将来有人改另一处就会悄悄变成
	// "图层重试 × 一次几十调用的引擎" 的乘法放大 (与 ensemble/orchestrated 同款纪律)。
	noRetry := &graph.RetryPolicy{MaxRetries: 0}
	spec := graph.GraphSpec{
		Name:    wf.Name,
		Version: "plot-" + k.Mode,
		Meta:    WorkflowGateMeta(wf),
		Policies: graph.GraphPolicies{
			// 1 个节点, 并发上限写 1 是如实描述而不是限制: 引擎内部的分析师并行由
			// swarm_intel 自己的 goroutine 池管, 与图的并发闸无关 (它看到的只有 1 个节点)。
			// 不写会落 we.effectiveParallel(), 那个值会随 API 流控在 1..6 之间浮动,
			// 让 journal 里"这次跑的并发上限是几"变成一个没有意义的数。
			MaxParallel:  1,
			DefaultRetry: noRetry,
		},
		Nodes: []graph.NodeSpec{{
			ID:    k.NodeID,
			Kind:  graph.NodeKindAgent,
			Agent: graph.AgentSpec{Role: k.Role},
			Retry: noRetry,
			// TimeoutSec 刻意留 0 = 不限, 见文件头第 2 条。**改成任何正数都是行为变更。**
		}},
	}
	if err := spec.Validate(); err != nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates_plot: %s 的图非法: %w", k.Mode, err)
	}
	return spec, nil
}

// ---------------------------------------------------------------------------
// 节点执行内核: swarm_intel 引擎
// ---------------------------------------------------------------------------

// plotSwarmNodeRunner 群体智能焦点 mode 的 graph.NodeRunner 实现。
//
// 与 orchNodeRunner / ensembleNodeRunner 同款做法: 共用 runGraphSpec 的调度/journal/
// hook/拦截器/预算, 只换节点执行内核 —— 这里的内核是 swarm_intel.Engine, 既不经
// agent 工厂也不经 ExecuteSingleStage (那会给这个"裸引擎"阶段发一套工具权限)。
type plotSwarmNodeRunner struct {
	we        *WorkflowExecutor
	team      *ProductionTeam
	objective string
	k         plotGraphKind

	mu sync.Mutex
	// done 收尾通知文案 (只有跑成功才有)。
	done string
	// err 引擎报的原始错误 (已按旧路径文案包好)。入口用它取代图引擎的通用错误文案。
	err error
	// ran 节点是否真的跑到了 (resume 吃缓存时不会跑 ⇒ 没有 done 也没有 err)。
	ran bool
}

var _ graph.NodeRunner = (*plotSwarmNodeRunner)(nil)

func newPlotSwarmNodeRunner(we *WorkflowExecutor, team *ProductionTeam, objective string, k plotGraphKind) *plotSwarmNodeRunner {
	return &plotSwarmNodeRunner{we: we, team: team, objective: objective, k: k}
}

// RunNode 见 graph.NodeRunner。
func (r *plotSwarmNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, _ graph.NodeInput) graph.NodeResult {
	if node.ID != r.k.NodeID {
		// 不可达 (图是本文件造的)。**不静默成功**: 静默成功会让一个什么都没做的节点
		// 冒充产出, 表现为"剧情模拟跑完了但产出是空的"。
		return graph.NodeResult{Status: graph.NodeStatusFailed,
			Err: fmt.Sprintf("%s: 未知节点 %q (图与 runner 不同源?)", r.k.Mode, node.ID)}
	}
	if r.we == nil || r.we.llm == nil { // 入口已查, 这里是防御性兜底
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: r.k.Mode + ": LLMClient 未注入"}
	}
	notify := plotNotifier(r.we, r.team)
	engine := newPlotSwarmEngine(r.we.llm, r.team, notify)
	output, done, err := r.k.Run(ctx, engine, teamChatID(r.team), r.objective)

	r.mu.Lock()
	r.ran = true
	r.done, r.err = done, err
	r.mu.Unlock()

	if err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: err.Error()}
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: output}
}

// outcome 取本次运行的收尾文案与引擎错误 (第三个返回值 = 节点是否真跑过)。
func (r *plotSwarmNodeRunner) outcome() (done string, err error, ran bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done, r.err, r.ran
}

// ---------------------------------------------------------------------------
// 入口: 灰度开关命中时取代旧路径
// ---------------------------------------------------------------------------

// executePlotSwarmGraph 用图引擎跑一个群体智能焦点 mode。
func (we *WorkflowExecutor) executePlotSwarmGraph(
	ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam, k plotGraphKind,
) ([]StageResult, error) {
	ctx, endSpan := logging.WithSpan(ctx, k.Mode+".graph")
	defer endSpan()

	spec, err := plotSwarmGraphSpec(wf, k)
	if err != nil {
		return nil, err
	}
	notify := plotNotifier(we, team)
	notify(k.StartNotify)

	// runner 在**调用前**装配好 (与 ensemble 同款理由): runGraphSpec 有一条"还没调工厂
	// 就返回"的路径 (journal 打不开), 那时闭包变量仍是 nil, 后面读 outcome() 会空指针。
	runner := newPlotSwarmNodeRunner(we, team, objective, k)
	results, runErr := we.runGraphSpec(ctx, wf, spec, objective, team,
		func(graph.GraphSpec) graph.NodeRunner { return runner })

	done, engineErr, ran := runner.outcome()
	if engineErr != nil {
		// 旧路径返回 (nil, "剧情模拟失败: <原因>")。原样复刻: 图引擎的通用失败文案
		// 会把真正的原因 (上游 429 / decompose 超时) 盖成一句 "图执行失败"。
		return nil, engineErr
	}
	if runErr != nil {
		return results, runErr
	}
	if ran {
		// resume 吃缓存时节点没跑过 ⇒ 没有收尾统计可报, 此时不发一条数字全是 0 的
		// "✅ 完成" —— 那比不发更误导 (会让人以为这次真的重算了)。
		notify(done)
	}
	return results, nil
}
