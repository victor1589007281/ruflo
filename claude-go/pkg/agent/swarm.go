// Swarm — Kimi K2.5 启发的动态蜂群编排器。
//
// 核心思想 (参考 Kimi K2.5 Agent Swarm + PARL):
//   - 不预定义角色: LLM 动态分析任务, 决定拆分多少子任务
//   - 拓扑排序: 按依赖关系分层, 同层并行执行
//   - 异步检查点: 每个子任务执行前后存档
//   - 结果汇聚: LLM 综合所有子任务结果, 生成最终答案
//
// 执行流程:
//
//  1. Decompose: LLM 分析目标, 生成 SubTask DAG (含依赖)
//
//  2. Schedule:  拓扑排序, 按层级并行调度到 AgentPool
//
//  3. Execute:   每层并行执行, 完成后写入 Blackboard
//
//  4. Merge:     LLM 综合所有结果, 生成最终输出
//
//     ┌─────────────────────────────────────────────────┐
//     │ SwarmOrchestrator                               │
//     │  decompose() → SubTask DAG                      │
//     │  topologicalLevels() → [[level0], [level1], ...]│
//     │  executeLevel() → 并行执行同层子任务            │
//     │  merge() → LLM 综合最终结果                     │
//     └─────────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SubTask 蜂群子任务 (LLM 动态生成)。
type SubTask struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Role        string   `json:"role"`
	DependsOn   []string `json:"dependsOn,omitempty"`
	Priority    int      `json:"priority,omitempty"` // 0=normal, 1=high, 2=critical
}

// DecompositionPlan LLM 生成的任务拆解计划。
type DecompositionPlan struct {
	SubTasks  []SubTask `json:"subTasks"`
	Strategy  string    `json:"strategy"` // parallel, pipeline, hybrid
	Rationale string    `json:"rationale"`
}

// SwarmOrchestrator 蜂群编排器。
type SwarmOrchestrator struct {
	llm         LLMClient
	pool        *AgentPool
	taskTracker TaskTracker
	notify      NotifyFunc
	chatID      string
	maxAgents   int
	evolution   *EvolutionEngine
	roles       *RoleRegistry
}

// NewSwarmOrchestrator 创建蜂群编排器。
func NewSwarmOrchestrator(llm LLMClient, pool *AgentPool, taskTracker TaskTracker, notify NotifyFunc, chatID string, maxAgents int) *SwarmOrchestrator {
	if maxAgents <= 0 {
		maxAgents = 8
	}
	return &SwarmOrchestrator{
		llm:         llm,
		pool:        pool,
		taskTracker: taskTracker,
		notify:      notify,
		chatID:      chatID,
		maxAgents:   maxAgents,
	}
}

// Execute 蜂群执行: 动态分解 → 拓扑排序 → 逐层并行 → 结果汇聚。
// 蜂群沿用自身的 topologicalLevels 波次调度 (不走 V2 DAG)。
func (s *SwarmOrchestrator) Execute(ctx context.Context, team *ProductionTeam, objective string) ([]StageResult, error) {
	s.notify(s.chatID, "🐝 **蜂群模式启动** — 正在分析任务并拆解子任务...")

	plan, err := s.decompose(ctx, objective)
	if err != nil {
		return nil, fmt.Errorf("任务分解失败: %w", err)
	}
	if len(plan.SubTasks) == 0 {
		return nil, fmt.Errorf("LLM 未能拆解出子任务")
	}

	if team.Blackboard != nil {
		planJSON, _ := json.Marshal(plan)
		team.Blackboard.Write("swarm-plan", string(planJSON), "orchestrator", "context")
		team.Blackboard.Write("swarm-strategy", plan.Strategy, "orchestrator", "context")
	}

	s.notify(s.chatID, fmt.Sprintf("📋 拆解为 **%d** 个子任务 (策略: %s)\n%s",
		len(plan.SubTasks), plan.Strategy, s.formatPlan(plan)))

	levels, err := s.topologicalLevels(plan.SubTasks)
	if err != nil {
		return nil, fmt.Errorf("拓扑排序失败: %w", err)
	}

	var allResults []StageResult
	resultMap := make(map[string]string)

	for levelIdx, level := range levels {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		s.notify(s.chatID, fmt.Sprintf("🔄 执行第 %d/%d 层 (%d 个子任务并行)...",
			levelIdx+1, len(levels), len(level)))

		levelResults := s.executeLevel(ctx, level, objective, resultMap, team)
		for _, sr := range levelResults {
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				resultMap[sr.Name] = sr.Output
			} else {
				s.notify(s.chatID, fmt.Sprintf("⚠️ 子任务 **%s** 失败: %s", sr.Name, sr.Error))
			}
		}
	}

	// LLM 汇聚
	successCount := 0
	for _, r := range allResults {
		if r.Status == TaskCompleted {
			successCount++
		}
	}
	if successCount > 0 && s.llm != nil {
		s.notify(s.chatID, "📊 正在汇总所有子任务结果...")
		merged, err := s.merge(ctx, allResults, objective)
		if err == nil && merged != "" {
			allResults = append(allResults, StageResult{
				Name: "swarm-synthesis", Role: "synthesizer",
				Status: TaskCompleted, Output: merged, StartedAt: time.Now(),
			})
			if team.Blackboard != nil {
				team.Blackboard.Write("swarm-final-result", merged, "orchestrator", "result")
			}
		}
	}

	return allResults, nil
}

// decompose 使用 LLM 动态分解任务。
func (s *SwarmOrchestrator) decompose(ctx context.Context, objective string) (*DecompositionPlan, error) {
	if s.llm == nil {
		return s.fallbackDecompose(objective), nil
	}

	sysPrompt := fmt.Sprintf(`你是任务分解引擎。将复杂任务拆解为可并行执行的子任务。

## 拆解规则
1. 每个子任务应是独立可执行的工作单元
2. 标注依赖关系 (dependsOn): 哪些子任务必须在其之前完成
3. 无依赖的子任务会自动并行执行
4. 最多拆解 %d 个子任务
5. role 可选: researcher, coder, reviewer, tester, architect, analyst,
   go-coder, go-reviewer, go-tester,
   typescript-coder, typescript-reviewer, typescript-tester,
   python-coder, python-reviewer, python-tester,
   django-coder, django-reviewer, django-tester,
   dotnet-coder, dotnet-reviewer, dotnet-tester,
   cpp-coder, cpp-reviewer, cpp-tester,
   build-resolver

## 优化规则 (重要!)
6. **最小依赖原则**: 只依赖真正需要的前置任务，不要过度串行化。
   例: 如果 t6(优化方案) 只需要 t3(架构) 和 t4(最佳实践)，不要额外依赖 t5(差距分析)
7. **负载均衡**: 平衡各角色的任务数量和复杂度，避免某个角色过载
   - reviewer 任务不应超过 2 个
   - 将重型任务拆分为多个子任务分配给不同角色
8. **量化要求**: 在需要评估/审计类任务的 description 中，明确要求输出量化指标
   (如: 代码行数、测试覆盖率、圈复杂度、依赖数量、安全评分等)
9. **最终汇总**: 最后一个任务必须是综合汇总，它的 description 中应要求:
   - 生成结构化的最终报告
   - 包含量化指标汇总表
   - 给出明确的结论和建议

输出严格JSON (不要解释):
{
  "subTasks": [
    {"id": "t1", "description": "具体任务描述", "role": "researcher", "dependsOn": []},
    {"id": "t2", "description": "另一个任务", "role": "coder", "dependsOn": ["t1"]}
  ],
  "strategy": "parallel|pipeline|hybrid",
  "rationale": "简述拆解理由 (含依赖优化和负载均衡说明)"
}`, s.maxAgents)

	resp, err := s.llm.SimpleComplete(ctx, sysPrompt, objective)
	if err != nil {
		return s.fallbackDecompose(objective), nil
	}

	resp = strings.TrimSpace(resp)
	if idx := strings.Index(resp, "```"); idx >= 0 {
		resp = resp[idx+3:]
		resp = strings.TrimPrefix(resp, "json")
		if end := strings.Index(resp, "```"); end >= 0 {
			resp = resp[:end]
		}
	}
	resp = strings.TrimSpace(resp)

	var plan DecompositionPlan
	if err := json.Unmarshal([]byte(resp), &plan); err != nil {
		return s.fallbackDecompose(objective), nil
	}

	if len(plan.SubTasks) > s.maxAgents {
		plan.SubTasks = plan.SubTasks[:s.maxAgents]
	}

	return &plan, nil
}

// fallbackDecompose 当 LLM 不可用时的降级拆解。
func (s *SwarmOrchestrator) fallbackDecompose(objective string) *DecompositionPlan {
	return &DecompositionPlan{
		Strategy:  "pipeline",
		Rationale: "LLM 不可用, 使用默认 research→synthesize 流程",
		SubTasks: []SubTask{
			{ID: "t1", Description: "调研和分析: " + objective, Role: "researcher"},
			{ID: "t2", Description: "综合整理研究结果", Role: "analyst", DependsOn: []string{"t1"}},
		},
	}
}

// topologicalLevels 将子任务按依赖拓扑排序, 返回分层结果。
// 同一层内的子任务可以并行执行。
func (s *SwarmOrchestrator) topologicalLevels(tasks []SubTask) ([][]SubTask, error) {
	taskMap := make(map[string]SubTask)
	inDegree := make(map[string]int)
	depGraph := make(map[string][]string)

	for _, t := range tasks {
		taskMap[t.ID] = t
		inDegree[t.ID] = 0
	}

	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if _, exists := taskMap[dep]; exists {
				depGraph[dep] = append(depGraph[dep], t.ID)
				inDegree[t.ID]++
			}
		}
	}

	var levels [][]SubTask
	processed := make(map[string]bool)

	for len(processed) < len(tasks) {
		var level []SubTask
		for _, t := range tasks {
			if processed[t.ID] {
				continue
			}
			if inDegree[t.ID] == 0 {
				level = append(level, t)
			}
		}

		if len(level) == 0 {
			return levels, fmt.Errorf("检测到循环依赖")
		}

		levels = append(levels, level)

		for _, t := range level {
			processed[t.ID] = true
			for _, next := range depGraph[t.ID] {
				inDegree[next]--
			}
		}
	}

	return levels, nil
}

// executeLevel 并行执行同一层的所有子任务。
func (s *SwarmOrchestrator) executeLevel(
	ctx context.Context,
	level []SubTask,
	objective string,
	prevResults map[string]string,
	team *ProductionTeam,
) []StageResult {
	// 自动扩缩池: 根据本层并发任务数调整
	if s.pool != nil {
		s.pool.AutoScale(len(level))
	}

	results := make([]StageResult, len(level))
	var wg sync.WaitGroup

	for i, task := range level {
		wg.Add(1)
		go func(idx int, t SubTask) {
			defer wg.Done()
			results[idx] = s.executeSubTask(ctx, t, objective, prevResults, team)
		}(i, task)
	}

	wg.Wait()
	return results
}

// executeSubTask 执行单个子任务。
func (s *SwarmOrchestrator) executeSubTask(
	ctx context.Context,
	task SubTask,
	objective string,
	prevResults map[string]string,
	team *ProductionTeam,
) StageResult {
	start := time.Now()

	// 构建子任务 prompt (优先从 RoleRegistry 获取角色系统提示词)
	var sb strings.Builder
	rolePromptInjected := false
	if s.roles != nil {
		if rp := s.roles.MergedPrompt(task.Role, objective, ""); rp != "" {
			sb.WriteString(rp)
			sb.WriteString("\n\n")
			rolePromptInjected = true
		}
	}
	if !rolePromptInjected {
		sb.WriteString(fmt.Sprintf("你的角色: %s\n\n", task.Role))
	}
	sb.WriteString(fmt.Sprintf("整体目标: %s\n\n", objective))
	sb.WriteString(fmt.Sprintf("你的具体任务: %s\n\n", task.Description))

	// 注入依赖结果
	if len(task.DependsOn) > 0 {
		sb.WriteString("前置任务的结果:\n")
		for _, dep := range task.DependsOn {
			if r, ok := prevResults[dep]; ok {
				sb.WriteString(fmt.Sprintf("\n--- %s ---\n%s\n", dep, truncateResult(r, 3000)))
			}
		}
		sb.WriteString("\n")
	}

	// 注入黑板上下文 (使用角色感知的智能截断)
	if team.Blackboard != nil {
		snapshot := team.Blackboard.SnapshotForRole(task.Role, 4000)
		if len(snapshot) > 0 {
			sb.WriteString("\n共享知识库:\n")
			sb.WriteString(snapshot)
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n请深入执行你的任务, 给出详细、高质量的结果。使用可用的工具来完成工作。")
	// 蜂群通用质量约束: 防止空壳/编译失败/架构脱节
	sb.WriteString(`

## 质量红线 (所有蜂群子任务必须遵守)
1. 禁止 Mock/Stub: 核心模块必须有真实实现
2. 编译检查: 如果你写了代码, 完成后必须运行 go build/go vet 确认编译通过
3. 架构对齐: 如果有前置任务的设计文档, 必须严格遵循其接口定义和目录结构
4. 测试函数名不允许包含中文符号(如-), 使用下划线分隔
5. 最终产出必须是可运行/可编译的代码, 不是文档或说明
6. 如果是审查/分析任务, 必须输出结构化报告(表格+评分)
`)

	prompt := sb.String()

	// 注入进化经验
	var injectedExpIDs []string
	if s.evolution != nil {
		exps := s.evolution.RetrieveFor(task.Role, task.Description, 3)
		if len(exps) > 0 {
			prompt = FormatExperiencesForPrompt(exps) + "\n" + prompt
			for _, e := range exps {
				injectedExpIDs = append(injectedExpIDs, e.ID)
			}
		}
	}

	// 通过 AgentPool 获取 agent
	if s.pool == nil {
		return StageResult{Name: task.ID, Role: task.Role, Status: TaskFailed, Error: "agent pool 未初始化"}
	}

	agent, err := s.pool.Acquire(ctx, task.Role, "")
	if err != nil {
		return StageResult{Name: task.ID, Role: task.Role, Status: TaskFailed, Error: err.Error(), StartedAt: start}
	}
	defer s.pool.Release(agent)

	// 子任务级别超时保护 (防止单个子任务卡死拖垮整体)
	taskTimeout := 5 * time.Minute
	if task.Priority >= 2 {
		taskTimeout = 10 * time.Minute
	}
	taskCtx, taskCancel := context.WithTimeout(ctx, taskTimeout)
	defer taskCancel()

	// V2 Task 追踪
	var v2ID string
	if s.taskTracker != nil {
		if id, err := s.taskTracker.AddTask(fmt.Sprintf("[swarm] %s", task.ID), task.Description, task.Role); err == nil {
			v2ID = id
			_ = s.taskTracker.SetTaskStatus(id, "in_progress")
		}
	}

	// 更新 team agent 状态
	if team != nil {
		team.mu.Lock()
		team.Agents[task.ID] = &BGAgent{Name: task.ID, Role: task.Role, Status: AgentStatusRunning}
		team.mu.Unlock()
	}

	result, execErr := agent.Runner.Execute(taskCtx, prompt)
	duration := time.Since(start)

	// 更新状态
	if team != nil {
		team.mu.Lock()
		if ag, ok := team.Agents[task.ID]; ok {
			if execErr != nil {
				ag.Status = AgentStatusFailed
				ag.Error = execErr.Error()
			} else {
				ag.Status = AgentStatusCompleted
				ag.Result = truncateResult(result, 1000)
			}
		}
		team.mu.Unlock()
	}

	// 写入黑板
	if team.Blackboard != nil {
		if execErr == nil {
			team.Blackboard.Write(task.ID+"-result", result, task.Role, "result")
		} else {
			team.Blackboard.Write(task.ID+"-error", execErr.Error(), task.Role, "result")
		}
	}

	// 更新 V2 Task
	if s.taskTracker != nil && v2ID != "" {
		if execErr != nil {
			_ = s.taskTracker.SetTaskStatus(v2ID, "failed")
		} else {
			_ = s.taskTracker.SetTaskStatus(v2ID, "completed")
		}
	}

	// 记录轨迹 + 经验反馈 + 增量学习
	if s.evolution != nil {
		traj := Trajectory{
			TeamName:  team.Name,
			StageName: task.ID,
			Role:      task.Role,
			Objective: task.Description,
			Input:     prompt,
			Output:    result,
			Error: func() string {
				if execErr != nil {
					return execErr.Error()
				}
				return ""
			}(),
			Success:   execErr == nil,
			Duration:  duration.Round(time.Second).String(),
			Timestamp: time.Now(),
		}
		s.evolution.RecordTrajectory(traj)
		s.evolution.LearnFromStage(traj)
		if len(injectedExpIDs) > 0 {
			s.evolution.RecordBatchFeedback(injectedExpIDs, execErr == nil)
		}
	}

	if execErr != nil {
		return StageResult{
			Name: task.ID, Role: task.Role, Status: TaskFailed,
			Error: execErr.Error(), StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	// 蜂群输出验证: 与工作流路径一致, 防止 Agent 空转
	if reason := validateAgentOutput(result, task.Role); reason != "" {
		return StageResult{
			Name: task.ID, Role: task.Role, Status: TaskFailed,
			Error:     fmt.Sprintf("产出验证失败: %s", reason),
			Output:    result,
			StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	return StageResult{
		Name: task.ID, Role: task.Role, Status: TaskCompleted,
		Output: result, V2TaskID: v2ID, StartedAt: start,
		Duration: duration.Round(time.Second).String(),
	}
}

// merge 使用 LLM 综合所有子任务结果。
func (s *SwarmOrchestrator) merge(ctx context.Context, results []StageResult, objective string) (string, error) {
	if s.llm == nil {
		return s.fallbackMerge(results), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("目标: %s\n\n各子任务结果:\n", objective))

	for _, r := range results {
		if r.Status == TaskCompleted && r.Output != "" {
			sb.WriteString(fmt.Sprintf("\n### [%s] %s\n%s\n", r.Role, r.Name, truncateResult(r.Output, 2000)))
		}
	}

	sysPrompt := `你是结果综合专家。将多个子任务的结果汇总为一份连贯、完整的最终报告。

## 报告结构 (必须包含)
1. **执行摘要** — 3-5 条关键发现
2. **各任务产出综合** — 提炼核心发现，去除冗余，保留关键细节和代码
3. **量化指标汇总表** — 如果子任务中有量化数据，必须汇总为表格
   | 指标 | 值 | 来源 | 备注 |
   |------|----|------|------|
4. **矛盾点分析** — 指出各子任务间的数据分歧和结论矛盾
5. **结论与建议** — 分优先级的可操作建议
6. **附录** — 各子任务耗时统计

## 要求
- 结构化 Markdown 输出
- 不要遗漏任何子任务的关键产出
- 如果子任务产出了代码或文件，明确列出文件路径`

	return s.llm.SimpleComplete(ctx, sysPrompt, sb.String())
}

func (s *SwarmOrchestrator) fallbackMerge(results []StageResult) string {
	var sb strings.Builder
	sb.WriteString("# 蜂群执行结果汇总\n\n")
	for _, r := range results {
		if r.Status == TaskCompleted && r.Output != "" {
			sb.WriteString(fmt.Sprintf("## %s (%s)\n%s\n\n", r.Name, r.Role, r.Output))
		}
	}
	return sb.String()
}

func (s *SwarmOrchestrator) formatPlan(plan *DecompositionPlan) string {
	var sb strings.Builder
	for i, t := range plan.SubTasks {
		deps := ""
		if len(t.DependsOn) > 0 {
			deps = fmt.Sprintf(" (依赖: %s)", strings.Join(t.DependsOn, ", "))
		}
		sb.WriteString(fmt.Sprintf("  %d. [%s] %s%s\n", i+1, t.Role, t.Description, deps))
	}
	return sb.String()
}
