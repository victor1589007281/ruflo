// workflow_orchestrated.go — orchestrated 模式入口: 图引擎 + 裸 completion 内核。
//
// ---------------------------------------------------------------------------
// 这个文件从"第二套调度系统的桥接层"变成了"图引擎的一个装配"
// ---------------------------------------------------------------------------
//
// 改造前 (design/01 §1.2 记的"双调度系统"):
//
//	系统 A: pkg/agent/coordinator.go + runPipelineWithRecovery / pkg/graph 图引擎
//	系统 B: pkg/orchestrator/engine.go  ← 本文件曾是它的桥接层
//
// 系统 B 自带一整套与系统 A 平行的设施: 另一份 DAG (orchestrator.Graph)、另一份黑板
// (orchestrator.Blackboard, 唯一有 Watch 的那份)、另一套生命周期 Hook
// (LifecycleHook: ExpanderHook/MetricsHook/心跳/ObservabilityBridge)、另一份检查点
// (CheckpointStore)、另一套背压 (RPM 令牌桶 + AIMD)。于是三件事全卡在它身上:
// 双黑板归一 (§4.11)、hook 三套合一 (§4.5)、进度四源归一 (§4.3)。
//
// 改造后 (design/01 M4): orchestrated 与其他 mode 共用 pkg/graph 的 ready-set 调度器,
// 差异**只在节点执行内核** —— orchNodeRunner 跑裸 LLM completion (无工具、无角色模板
// 合并、无黑板交接), 见 orchestrated_runner.go。于是:
//
//	orchestrator.Graph        → graph.GraphSpec        (本文件 orchestratedGraphSpec)
//	orchestrator.LLMRunner    → orchNodeRunner         (裸 completion, 提示词逐字照抄)
//	AdversarialRunner         → orchNodeRunner 的对抗分支
//	RunnerPool                → Policies.MaxParallel + 对抗节点内部信号量
//	LifecycleHook 四件套      → graph.HookBus (teamGraphHooks: 刷盘/指标/心跳/日志)
//	CheckpointStore           → graph FileJournal (事件溯源, 重放即恢复)
//	orchestrator.Blackboard   → 删除 (上游产出经 NodeInput.PrevOutputs 传递)
//	RPM 令牌桶 + AIMD          → 删除 (见下方"刻意丢掉的东西")
//
// ---------------------------------------------------------------------------
// 刻意丢掉的东西 (逐条给理由, 因为"少了什么"比"多了什么"更容易埋雷)
// ---------------------------------------------------------------------------
//
//  1. **RPM 令牌桶 + AIMD 自适应并发**: 旧 cfg 把 RPM 设成 120 (2 QPS, 突发 10) 并在
//     代码里注明"由 api.Client 自身处理限流, 引擎不做额外限速" —— 对 8~12 个阶段的
//     DAG 这个桶从来不会成为瓶颈。AIMD 那半 (失败后并发折半) 确有价值但没有等价物,
//     图层的 RateLimiter 拦截器在 design/01 §4.10 里仍是未实现项。记账在此。
//  2. **Suspended 态 + 停滞恢复**: 瞬态额度耗尽时旧引擎置 Suspended (不级联下游),
//     等 stallRecovery 唤醒并给一份全新重试额度 (最多 3 次)。3 次用尽后主循环
//     **永久空转**: isComplete 永假 (任务既不 completed 也不 failed)、无 Ready 任务可派、
//     stallTicker 每 48s 空转一次, 直到 ctx 取消 —— team.EngineRunning 一直为 true。
//     新路径瞬态耗尽即判 failed, 少了"再等等"的耐心, 换掉了一条挂死路径。
//  3. **ExpanderHook / LLMExpander (运行期 LLM 裂变)**: 触发条件是
//     task.Config["expandable"]==true, 而全仓**没有任何地方设置过它** —— 注册了但恒休眠。
//     图层的对等能力是 NodeSpec.Expand + parseStageExpansion (graph_adapter.go), 需要时
//     经覆盖表显式授权即可, 不必搬一份死代码过来。
//  4. **orchestrator 黑板的 objective / <task>/output / adv/... 三类键**: 全是只写不读
//     (LLMRunner 只经 <dep>/output 读上游, 这条已由 PrevOutputs 覆盖), 且那份黑板是
//     纯内存、跑完即弃。删掉不丢任何可观测数据。
//  5. **每 15 秒的 watchdog 心跳 ticker (callbackHook)**: 它其实早就坏了 ——
//     stopHeartbeat 用 sync.Once 关同一个 stopCh, 于是**第一个任务完成后**所有后续
//     startHeartbeat 造出的 goroutine 都立刻退出。真正生效的只有第一个任务那一段。
//     新路径由 teamGraphHooks 在每个节点开始/结束时 touch (与 pipeline 侧
//     executeStage 的口径一致), 覆盖面反而更广。watchdog 本身只发通知不终止团队
//     (coordinator.go teamWatchdog), 所以最坏后果是一条多余的告警。
//
// ---------------------------------------------------------------------------
// 顺带修掉的三处旧缺陷 (等价性测试里逐条钉住)
// ---------------------------------------------------------------------------
//
//  1. **返回的阶段序列此前是随机序**: 旧 convertResults 遍历 `map[taskID]stageInfo`,
//     Go 的 map 迭代序是随机化的 ⇒ 同一次运行两次调用可以给出不同顺序, 而
//     REPORT.md / 门禁统计 / dashboard / 下游平台都按这个序读。现在按节点声明序,
//     与 pipeline 一致。
//  2. **未执行的阶段被报成 completed**: 旧 convertResults 对 stageMapping 里每个阶段
//     先置 Status=TaskCompleted, 只有 Errors 里有条目才改 failed。于是 ctx 取消导致
//     根本没跑的阶段会以"completed + 空产出"出现 —— 静默假成功。现在只回译引擎真正
//     给出终态的节点。
//  3. **orchestrated 全程没有 stage 级增量刷盘/指标/journal**: 旧路径只有一个
//     stageFlushHook 往 team.Stages 追加 (不落盘、不出指标)。现在复用 teamGraphHooks:
//     running 占位 + 增量刷盘 + 阶段指标 + 心跳 + Journal 事件, 与图路径同形。
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
)

// orchDefaultMaxParallel orchestrated 的默认并发上限 (照抄旧 cfg.MaxParallel 的初值)。
// 刻意不用 we.effectiveParallel() (=6, 且会随 API 流控收敛): 旧路径就是拿这个 4,
// 换成 6 会让四路专家评审之外多挤进一个阶段, 属并发度变更。
const orchDefaultMaxParallel = 4

// executeOrchestrated 用图引擎驱动 orchestrated 工作流。
func (we *WorkflowExecutor) executeOrchestrated(
	ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam,
) ([]StageResult, error) {
	ctx, endSpan := logging.WithSpan(ctx, "orchestrated."+wf.Name)
	defer endSpan()

	// LLMClient 缺失时降级 pipeline: 与改造前逐字一致 (裸 completion 没有 LLM 无从下手,
	// 但 pipeline 走 factory 造 agent, 那条路不依赖 we.llm)。
	if we.llm == nil {
		logging.Event(ctx, "orchestrated.fallback", "reason", "LLMClient 未设置, 降级到 pipeline 模式")
		we.notify(we.chatID, "⚠️ LLMClient 未注入, 降级到 pipeline 模式")
		return we.executePipeline(ctx, wf, objective, team)
	}

	maxParallel := we.orchestratedMaxParallel(team)
	spec, err := orchestratedGraphSpec(wf, maxParallel)
	if err != nil {
		return nil, err
	}

	ready := orchestratedReadyCount(spec)
	logging.Event(ctx, "orchestrated.graph",
		"total_tasks", len(spec.Nodes), "ready_tasks", ready, "max_parallel", maxParallel)
	we.notify(we.chatID, fmt.Sprintf("⚡ **Orchestrated 模式** — 图引擎驱动 DAG 调度 (%d 个任务, %d 就绪, 并发≤%d)",
		len(spec.Nodes), ready, maxParallel))

	// EngineRunning 供 Coordinator.checkTeamHealth 区分"引擎在独立 goroutine 里跑"
	// 与"所有 agent 都 idle 却还 running"。orchestrated 的节点不占 team.Agents,
	// 不打这个标记会让心跳日志每轮误报一次。
	team.mu.Lock()
	team.EngineRunning = true
	team.mu.Unlock()
	defer func() { team.mu.Lock(); team.EngineRunning = false; team.mu.Unlock() }()

	var runner *orchNodeRunner
	results, runErr := we.runGraphSpec(ctx, wf, spec, objective, team, func(s graph.GraphSpec) graph.NodeRunner {
		runner = newOrchNodeRunner(we, team, objective, s)
		return runner
	})
	we.notifyOrchestratedSummary(results, runner)
	return results, runErr
}

// orchestratedMaxParallel 解析并发上限: 模型配置声明优先, 否则 orchDefaultMaxParallel。
//
// 旧路径同时读 resolved.RPM 去配引擎的令牌桶; 令牌桶已随 pkg/orchestrator 删除,
// RPM 由 api.Client 自己管 (旧代码注释也是这么写的), 故此处不再读它。
func (we *WorkflowExecutor) orchestratedMaxParallel(team *ProductionTeam) int {
	if we.planCfgResolver == nil || team == nil {
		return orchDefaultMaxParallel
	}
	if resolved := we.planCfgResolver.Resolve(team.Workflow, ""); resolved.MaxParallel > 0 {
		return resolved.MaxParallel
	}
	return orchDefaultMaxParallel
}

// orchestratedGraphSpec 把 WorkflowDef 直接编成 orchestrated 的 GraphSpec。
//
// **为什么不复用 TranslateWorkflow** (graph_adapter.go 的通用直译器) —— 三条硬理由:
//
//  1. 它按关键词把阶段推断成 gate 节点 (stageIsGate: 名字/角色含 gate|门禁|...)。
//     testing 工作流有个叫 `quality-gate` 的普通 LLM 阶段, 一过直译器就变成打分节点,
//     产出从"质量门禁报告"变成 `{"gate":"...","score":80}` —— 这是最典型的静默行为变更。
//  2. 它会应用 per-workflow 图覆盖表 (WorkflowGraphOverride), 覆盖表能声明
//     map/reduce/loop-group/Loop。orchNodeRunner 只认 agent 节点 (裸 completion),
//     被注入一个 map 节点会拿到 N 个分片却按同一个提示词跑 N 次。授权面不该在这里敞开。
//  3. 它给的重试与超时是 pipeline 口径 (图级 DefaultRetry={6,5s} + 角色级预算),
//     而 orchestrated 的重试按错误类型分档且在 runner 内 (见 orchestrated_runner.go)。
//
// 于是这里手工构图, 三个字段之外一律零值, 图的形状与旧 buildGraph 一一对应:
// 节点 ID = 阶段名, 边 = DependsOn (声明序, {prev_result} 的拼接序依赖它)。
func orchestratedGraphSpec(wf *WorkflowDef, maxParallel int) (graph.GraphSpec, error) {
	if wf == nil || len(wf.Stages) == 0 {
		return graph.GraphSpec{}, fmt.Errorf("orchestrated: 工作流没有阶段可执行")
	}
	// 图层重试恒 0: 重试全部在 orchNodeRunner 里按错误类型分档做。
	// 两处都填是刻意的 —— NodeSpec.Retry 为 nil 时引擎会回落 Policies.DefaultRetry,
	// 只填一处将来有人改另一处就会悄悄变成"图层重试 × runner 重试"的乘法放大。
	noRetry := &graph.RetryPolicy{MaxRetries: 0}
	spec := graph.GraphSpec{
		Name:    wf.Name,
		Version: "orchestrated-v1",
		Meta:    WorkflowGateMeta(wf),
		Policies: graph.GraphPolicies{
			MaxParallel:  maxParallel,
			DefaultRetry: noRetry,
		},
	}
	attempts := orchDefaultRetryPolicy().orchMaxAttempts()
	for _, st := range wf.Stages {
		// 节点**总预算**必须包住全部重试 (引擎在 retry 环之外施加它), 而单次尝试的
		// 3/4/5 分钟由 runner 自己加。给紧了会让合法的重试序列撞 deadline, 所以按
		// 最坏情况 (每次尝试都跑满超时) 再乘 2 倍宽裕, 最后由 2h 硬顶夹住。
		// 它是卡死兜底闸 —— 旧路径连这道闸都没有 (只有单次尝试超时)。
		budget := capNodeBudget(int(taskTimeout(st).Seconds()) * attempts * orchBudgetSlack)
		spec.Nodes = append(spec.Nodes, graph.NodeSpec{
			ID:         st.Name,
			Kind:       graph.NodeKindAgent, // 一律 agent: 见上文第 1 条
			Agent:      graph.AgentSpec{Role: st.Role, Prompt: st.Prompt},
			Retry:      noRetry,
			TimeoutSec: budget,
		})
	}
	for _, st := range wf.Stages {
		for _, dep := range st.DependsOn {
			// 无条件边: 上游失败时的"级联取消"由 orchNodeRunner 的级联闸复刻
			// (条件边 + OR-join 表达不出 AND-join, 见 orchestrated_runner.go)。
			spec.Edges = append(spec.Edges, graph.EdgeSpec{From: dep, To: st.Name})
		}
	}
	if err := spec.Validate(); err != nil {
		return graph.GraphSpec{}, fmt.Errorf("orchestrated: 图非法: %w", err)
	}
	return spec, nil
}

// orchBudgetSlack 节点总预算的宽裕系数 (与 graph_interceptors.go 的 budgetSlack 同义:
// 这道闸的定位是兜住卡死, 不是精确控成本)。
const orchBudgetSlack = 2

// orchestratedReadyCount 入度 0 的节点数 (通知文案里的"就绪"数)。
func orchestratedReadyCount(spec graph.GraphSpec) int {
	hasIn := make(map[string]bool, len(spec.Edges))
	for _, ed := range spec.Edges {
		hasIn[ed.To] = true
	}
	n := 0
	for _, nd := range spec.Nodes {
		if !hasIn[nd.ID] {
			n++
		}
	}
	return n
}

// notifyOrchestratedSummary 收尾通知 (文案与改造前一致, 便于飞书侧观感不变)。
func (we *WorkflowExecutor) notifyOrchestratedSummary(results []StageResult, runner *orchNodeRunner) {
	completed, failed := 0, 0
	for _, r := range results {
		if r.Status == TaskCompleted {
			completed++
		} else {
			failed++
		}
	}
	if failed == 0 {
		retries := int64(0)
		if runner != nil {
			retries = runner.retries.Load()
		}
		we.notify(we.chatID, fmt.Sprintf("✅ Orchestrated 执行完成 — %d 个任务成功, %d 次重试", completed, retries))
		return
	}
	we.notify(we.chatID, fmt.Sprintf("⚠️ Orchestrated 执行部分失败 — 成功 %d, 失败 %d", completed, failed))
}

// isAdversarialStage 判断阶段是否为对抗阶段 (判据与改造前逐字相同)。
// 命中它的阶段走 orchNodeRunner 的对抗分支 (内层多轮 + 质量终止), 而不是单次 completion。
func isAdversarialStage(stage StageDef) bool {
	lower := strings.ToLower(stage.Name + " " + stage.Role)
	return strings.Contains(lower, "adversarial") ||
		strings.Contains(lower, "skeptic") ||
		strings.Contains(lower, "red-team") ||
		strings.Contains(lower, "challenge")
}

// taskTimeout 根据阶段类型分配差异化的**单次尝试**超时 (与改造前逐字相同)。
// 汇总/报告类阶段需要合并多个上游输出, 耗时更长。
func taskTimeout(stage StageDef) time.Duration {
	lower := strings.ToLower(stage.Name + " " + stage.Role)
	// 汇总报告类: 需要整合多个上游阶段输出
	if strings.Contains(lower, "report") ||
		strings.Contains(lower, "summary") ||
		strings.Contains(lower, "final") {
		return 5 * time.Minute
	}
	// 计划类阶段: 需要综合多位专家建议
	if strings.Contains(lower, "plan") ||
		strings.Contains(lower, "strategy") ||
		strings.Contains(lower, "synthesize") {
		return 4 * time.Minute
	}
	// 默认: 独立专家阶段
	return 3 * time.Minute
}
