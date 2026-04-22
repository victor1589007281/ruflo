// workflow_orchestrated.go — 桥接层: 将 WorkflowDef 转换为 pkg/orchestrator.Engine 驱动的执行。
//
// 解决的核心问题:
//   原来 pipeline 模式下, 对抗只是 Prompt 角色扮演 (单次调用)。
//   orchestrated 模式下, 对抗阶段使用 AdversarialRunner 实现真正的多轮博弈,
//   DAG 调度、背压控制、检查点等全部由 pkg/orchestrator.Engine 驱动。
//
// 桥接流程:
//   WorkflowDef.Stages → orchestrator.Graph.Tasks
//   StageDef.Role → LLMRunner (通过 RunnerRegistry)
//   StageDef.DependsOn → Edge (依赖关系)
//   对抗阶段 → AdversarialRunner (多轮循环 + 质量终止)
//   可扩展阶段 → LLMExpander (运行时裂变)
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/orchestrator"
)

// executeOrchestrated 使用 pkg/orchestrator.Engine 驱动工作流执行。
//
// 与 executePipeline 的关键区别:
//  1. 对抗阶段使用 AdversarialRunner (真正的多轮对抗, 带反馈回路)
//  2. DAG 调度由引擎管理 (Filter→Score→Dispatch, 非简单拓扑排序)
//  3. 背压控制 (RPM 令牌桶 + 自适应并发)
//  4. 检查点和断点续跑
//  5. 停滞检测和自动恢复
//  6. 可选: LLM 裂变 (运行时动态展开子任务)
func (we *WorkflowExecutor) executeOrchestrated(
	ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam,
) ([]StageResult, error) {
	ctx, endSpan := logging.WithSpan(ctx, "orchestrated."+wf.Name)
	defer endSpan()

	if we.llm == nil {
		logging.Event(ctx, "orchestrated.fallback", "reason", "LLMClient 未设置, 降级到 pipeline 模式")
		we.notify(we.chatID, "⚠️ LLMClient 未注入, 降级到 pipeline 模式")
		return we.executePipeline(ctx, wf, objective, team)
	}

	// 桥接: pkg/agent.LLMClient → pkg/orchestrator.LLMClient (duck typing 兼容)
	llmAdapter := &orchLLMAdapter{llm: we.llm}

	// 1. 构建 orchestrator 引擎
	cfg := orchestrator.DefaultEngineConfig()
	cfg.MaxParallel = 2
	cfg.DefaultTimeout = 5 * time.Minute
	cfg.StallTimeout = 8 * time.Minute
	cfg.RPM = 120 // 由 api.Client 自身处理限流, 引擎不做额外限速
	cfg.RPMBurst = 10
	cfg.CheckpointEvery = 1

	eng := orchestrator.NewEngine(cfg)

	// 2. 构建 RunnerPool (per-runner 并发控制)
	pool := orchestrator.NewRunnerPool()
	pool.SetLimit("llm-stage", 4)          // LLM 阶段最多 4 并发
	pool.SetLimit("llm-adversarial", 1)    // 对抗循环串行

	// 3. 注册 Runner
	baseLLM := orchestrator.NewLLMRunner("llm-stage", llmAdapter)
	pooledLLM := orchestrator.NewPooledRunner(baseLLM, pool)
	eng.Runners().Register(pooledLLM)

	// 为对抗阶段注册 AdversarialRunner (如果有的话)
	advRunner := we.buildAdversarialRunner(wf, pool, llmAdapter)
	if advRunner != nil {
		eng.Runners().Register(advRunner)
	}

	// 4. 将 WorkflowDef 转换为 Graph
	g, stageMapping := we.buildGraph(wf, objective, team, advRunner != nil)

	// 5. 可选: 注册 LLMExpander (DAG 裂变)
	expander := orchestrator.NewLLMExpander(llmAdapter, "llm-stage", 8)
	expanderHook := orchestrator.NewExpanderHook(expander, g)
	metricsHook := orchestrator.NewMetricsHook(nil)
	eng.SetHook(orchestrator.NewMultiHook(expanderHook, metricsHook))

	// 6. 设置 Blackboard
	bb := orchestrator.NewBlackboard()
	bb.Write("objective", objective, orchestrator.WriteMeta{Author: "system", Category: "context"})
	eng.SetBlackboard(bb)

	// 7. 诊断: 确认图构建正确
	if buildErr := g.Build(); buildErr != nil {
		return nil, fmt.Errorf("图构建失败: %w", buildErr)
	}
	readyTasks := g.ReadyTasks()
	logging.Event(ctx, "orchestrated.graph",
		"total_tasks", g.TaskCount(),
		"ready_tasks", len(readyTasks),
		"max_parallel", cfg.MaxParallel,
	)
	for _, rt := range readyTasks {
		logging.Event(ctx, "orchestrated.ready_task", "id", rt.ID, "runner", rt.Runner)
	}
	we.notify(we.chatID, fmt.Sprintf("⚡ **Orchestrated 模式** — 引擎驱动 DAG 调度 (%d 个任务, %d 就绪, 并发≤%d)",
		g.TaskCount(), len(readyTasks), cfg.MaxParallel))

	// 8. 执行 (Build() 已在上面调用, Engine.Run 会再次调用但幂等)
	result, err := eng.Run(ctx, g)
	if err != nil && result == nil {
		return nil, fmt.Errorf("引擎执行失败: %w", err)
	}

	// 9. 将引擎结果转换回 StageResult
	return we.convertResults(result, stageMapping, team), err
}

// buildGraph 将 WorkflowDef 的 Stages 转换为 orchestrator.Graph。
func (we *WorkflowExecutor) buildGraph(
	wf *WorkflowDef, objective string, team *ProductionTeam, hasAdversarial bool,
) (*orchestrator.Graph, map[string]string) {
	g := orchestrator.NewGraph(team.Name+"-graph", wf.Name)
	stageMapping := make(map[string]string) // taskID → stageName

	for _, stage := range wf.Stages {
		taskID := fmt.Sprintf("%s/%s", team.Name, stage.Name)
		stageMapping[taskID] = stage.Name

		// 判断是否是对抗阶段 (角色名含 adversarial/skeptical)
		isAdversarial := hasAdversarial && isAdversarialStage(stage)
		runnerName := "llm-stage"
		if isAdversarial {
			runnerName = "llm-adversarial"
		}

		// 构建 prompt
		systemPrompt := stage.Prompt

		t := &orchestrator.Task{
			ID:           taskID,
			Name:         stage.Name,
			Priority:     5,
			Timeout:      3 * time.Minute,
			MaxRetries:   1,
			MaxTransient: 2,
			Runner:       runnerName,
			Config: map[string]any{
				"system_prompt": systemPrompt,
				"user_prompt":   "{prev_result}\n\n任务目标: {objective}",
				"objective":     objective,
				"stage_name":    stage.Name,
				"role":          stage.Role,
				"expandable":    isExpandableStage(stage),
			},
			Labels: map[string]string{
				"workflow": wf.Name,
				"stage":    stage.Name,
				"role":     stage.Role,
			},
		}

		// 设置依赖
		for _, dep := range stage.DependsOn {
			depID := fmt.Sprintf("%s/%s", team.Name, dep)
			t.DependsOn = append(t.DependsOn, depID)
		}

		_ = g.AddTask(t)
	}

	// 添加边
	for _, stage := range wf.Stages {
		taskID := fmt.Sprintf("%s/%s", team.Name, stage.Name)
		for _, dep := range stage.DependsOn {
			depID := fmt.Sprintf("%s/%s", team.Name, dep)
			_ = g.AddEdge(orchestrator.Edge{From: depID, To: taskID, Kind: orchestrator.EdgeDependency})
		}
	}

	return g, stageMapping
}

// buildAdversarialRunner 检测工作流中是否有对抗阶段, 如果有则构建 AdversarialRunner。
func (we *WorkflowExecutor) buildAdversarialRunner(wf *WorkflowDef, pool *orchestrator.RunnerPool, llm orchestrator.LLMClient) *AdversarialRunnerAdapter {
	var advStages []StageDef
	var genStages []StageDef

	for _, stage := range wf.Stages {
		if isAdversarialStage(stage) {
			advStages = append(advStages, stage)
		}
	}

	if len(advStages) == 0 {
		return nil
	}

	// 找到对抗阶段依赖的上游 (这些是 generator)
	for _, adv := range advStages {
		for _, dep := range adv.DependsOn {
			for _, stage := range wf.Stages {
				if stage.Name == dep {
					genStages = append(genStages, stage)
				}
			}
		}
	}

	generator := orchestrator.NewLLMRunner("adv-generator", llm)
	var reviewers []orchestrator.TaskRunner
	for i, adv := range advStages {
		r := orchestrator.NewLLMRunner(fmt.Sprintf("adv-reviewer-%d", i), llm)
		_ = adv
		reviewers = append(reviewers, r)
	}

	qualityPolicy := orchestrator.NewQualityTermination(7.0, 0.5, nil)
	inner := orchestrator.NewAdversarialRunner("adversarial-inner", generator, reviewers, qualityPolicy, 3)

	return &AdversarialRunnerAdapter{
		inner:  inner,
		stages: advStages,
		pool:   pool,
	}
}

// AdversarialRunnerAdapter 将 orchestrator.AdversarialRunner 适配为可注册到引擎的 TaskRunner。
type AdversarialRunnerAdapter struct {
	inner  *orchestrator.AdversarialRunner
	stages []StageDef
	pool   *orchestrator.RunnerPool
}

func (a *AdversarialRunnerAdapter) Name() string { return "llm-adversarial" }

func (a *AdversarialRunnerAdapter) Execute(ctx context.Context, task *orchestrator.Task, bb orchestrator.ReadOnlyBlackboard) (any, error) {
	if err := a.pool.Acquire(ctx, "llm-adversarial"); err != nil {
		return nil, err
	}
	defer a.pool.Release("llm-adversarial")
	return a.inner.Execute(ctx, task, bb)
}

// convertResults 将 orchestrator.ExecutionResult 转换为 []StageResult。
func (we *WorkflowExecutor) convertResults(
	result *orchestrator.ExecutionResult, stageMapping map[string]string, team *ProductionTeam,
) []StageResult {
	if result == nil {
		return nil
	}
	var results []StageResult
	for taskID, stageName := range stageMapping {
		sr := StageResult{
			Name:   stageName,
			Status: TaskCompleted,
		}

		if output, ok := result.Outputs[taskID]; ok {
			sr.Output = fmt.Sprintf("%v", output)
		}
		if errStr, ok := result.Errors[taskID]; ok && errStr != "" {
			sr.Status = TaskFailed
			sr.Error = errStr
		}
		results = append(results, sr)

		// 写入团队 Blackboard
		if team.Blackboard != nil {
			if sr.Status == TaskCompleted {
				team.Blackboard.Write(stageName+"-result", sr.Output, "orchestrator", "result")
				team.Blackboard.Write(stageName+"-status", "completed", "system", "progress")
			} else {
				team.Blackboard.Write(stageName+"-status", "failed: "+sr.Error, "system", "progress")
			}
		}
	}

	// 通知结果
	if result.Success {
		we.notify(we.chatID, fmt.Sprintf("✅ Orchestrated 执行完成 — %d 个任务成功, %d 次重试",
			result.Metrics.CompletedTasks, result.Metrics.TotalRetries))
	} else {
		we.notify(we.chatID, fmt.Sprintf("⚠️ Orchestrated 执行部分失败 — 成功 %d, 失败 %d",
			result.Metrics.CompletedTasks, result.Metrics.FailedTasks))
	}

	return results
}

// isAdversarialStage 判断阶段是否为对抗阶段。
func isAdversarialStage(stage StageDef) bool {
	lower := strings.ToLower(stage.Name + " " + stage.Role)
	return strings.Contains(lower, "adversarial") ||
		strings.Contains(lower, "skeptic") ||
		strings.Contains(lower, "red-team") ||
		strings.Contains(lower, "challenge")
}

// isExpandableStage 判断阶段是否可以 LLM 裂变。
func isExpandableStage(stage StageDef) bool {
	lower := strings.ToLower(stage.Name)
	return strings.Contains(lower, "plan") ||
		strings.Contains(lower, "decompose") ||
		strings.Contains(lower, "risk-model")
}

// orchLLMAdapter 桥接 pkg/agent.LLMClient → pkg/orchestrator.LLMClient。
// 两者接口签名完全一致, 但属于不同包, 需要适配器。
type orchLLMAdapter struct {
	llm LLMClient
}

func (a *orchLLMAdapter) SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return a.llm.SimpleComplete(ctx, systemPrompt, userPrompt)
}
