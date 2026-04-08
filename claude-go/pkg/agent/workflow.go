// Workflow — 多 Agent 协作工作流模式。
//
// 三种核心模式 (参考 CrewAI + LangGraph + AutoGen):
//
//	1. Pipeline (开发): Architect → Coder → Reviewer → Tester (串行依赖)
//	2. Fan-Out (调研): Researcher₁ ∥ Researcher₂ → Synthesizer (并行汇聚)
//	3. Adversarial (辩论): Proposer ↔ Opponent × N轮 → Judge (对抗决策)
//
// 工作流执行器按 stage 依赖拓扑排序, 自动传递上下文。
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// WorkflowDef 工作流定义
type WorkflowDef struct {
	Name        string
	Description string
	Mode        string     // pipeline, fanout, adversarial
	Stages      []StageDef // pipeline/fanout 模式
	Rounds      int        // adversarial 模式的对抗轮数
}

// StageDef 阶段定义
type StageDef struct {
	Name      string   // 阶段名称
	Role      string   // agent 角色
	Prompt    string   // 系统提示词模板 (支持 {objective}, {prev_result} 占位符)
	DependsOn []string // 依赖的前置阶段
	Parallel  bool     // 是否可与同级并行
}

// GetWorkflow 获取预定义工作流
func GetWorkflow(name string) *WorkflowDef {
	switch name {
	case "development", "dev":
		return developmentWorkflow()
	case "research":
		return researchWorkflow()
	case "debate":
		return debateWorkflow()
	case "swarm":
		return swarmWorkflow()
	default:
		return nil
	}
}

// ListWorkflows 列出所有可用工作流
func ListWorkflows() []WorkflowDef {
	return []WorkflowDef{
		*developmentWorkflow(),
		*researchWorkflow(),
		*debateWorkflow(),
		*swarmWorkflow(),
	}
}

func swarmWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "swarm",
		Description: "蜂群模式: LLM 动态拆解 → 并行执行 → 结果汇聚 (Kimi K2.5 启发)",
		Mode:        "swarm",
	}
}

func developmentWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "development",
		Description: "软件开发流水线: 设计 → 实现 → 审查 → 测试",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "design", Role: "architect",
				Prompt: `You are a senior software architect. Analyze the requirement and produce a detailed technical design.

Requirement: {objective}

Output a design document with:
1. Architecture overview and key design decisions
2. Component breakdown with interfaces
3. Data flow and state management
4. File structure and naming conventions
5. Edge cases and error handling strategy

Be specific about implementation details. Output in markdown.`,
			},
			{
				Name: "implement", Role: "coder", DependsOn: []string{"design"},
				Prompt: `You are an expert software developer. Implement the solution based on the architecture design.

Objective: {objective}

Architecture Design:
{prev_result}

Write clean, production-quality code. Include proper error handling, logging, and documentation.
Create all necessary files. Use the tools available to write files and run commands.`,
			},
			{
				Name: "review", Role: "reviewer", DependsOn: []string{"implement"},
				Prompt: `You are a senior code reviewer. Review the implementation for quality, security, and best practices.

Objective: {objective}

Implementation summary:
{prev_result}

Review checklist:
1. Code correctness and logic errors
2. Security vulnerabilities (injection, auth bypass, data leak)
3. Performance issues (N+1 queries, memory leaks, blocking calls)
4. Error handling completeness
5. API design and naming conventions
6. Documentation quality

Provide specific, actionable feedback with file paths and line references.`,
			},
			{
				Name: "test", Role: "tester", DependsOn: []string{"implement"},
				Prompt: `You are a quality engineer. Write comprehensive tests for the implementation.

Objective: {objective}

Implementation summary:
{prev_result}

Write tests covering:
1. Unit tests for all public functions
2. Edge cases and error paths
3. Integration tests if applicable
4. Test data setup and cleanup

Use the project's testing framework. Ensure tests are deterministic and independent.`,
				Parallel: true,
			},
		},
	}
}

func researchWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "research",
		Description: "调研汇总: 多路并行调研 → 综合分析",
		Mode:        "fanout",
		Stages: []StageDef{
			{
				Name: "research-tech", Role: "researcher",
				Prompt: `You are a technical researcher. Investigate the TECHNICAL aspects of the topic.

Topic: {objective}

Focus on:
- Current state of the art and recent developments
- Key technologies, frameworks, and tools
- Technical trade-offs and limitations
- Code examples and implementation patterns

Provide detailed findings with references where possible. Output in structured markdown.`,
				Parallel: true,
			},
			{
				Name: "research-market", Role: "researcher",
				Prompt: `You are a market researcher. Investigate the MARKET and ADOPTION aspects of the topic.

Topic: {objective}

Focus on:
- Industry adoption and market trends
- Key players and their approaches
- Cost analysis and ROI considerations
- Case studies and success stories

Provide detailed findings with data points where possible. Output in structured markdown.`,
				Parallel: true,
			},
			{
				Name: "research-risk", Role: "researcher",
				Prompt: `You are a risk analyst. Investigate the RISKS and CHALLENGES of the topic.

Topic: {objective}

Focus on:
- Technical risks and failure modes
- Security and compliance concerns
- Scalability and maintenance challenges
- Migration and integration difficulties

Provide detailed risk assessment with mitigation strategies. Output in structured markdown.`,
				Parallel: true,
			},
			{
				Name: "synthesize", Role: "synthesizer",
				DependsOn: []string{"research-tech", "research-market", "research-risk"},
				Prompt: `You are a senior analyst. Synthesize all research findings into a comprehensive report.

Topic: {objective}

Research Findings:
{prev_result}

Produce a final report with:
1. Executive Summary (key findings in 3-5 bullets)
2. Technical Analysis (synthesized from all researchers)
3. Market Analysis
4. Risk Assessment
5. Recommendations (prioritized, actionable)
6. Conclusion

Be concise but thorough. Highlight agreements and contradictions between researchers.`,
			},
		},
	}
}

func debateWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "debate",
		Description: "对抗辩论: 正方 ↔ 反方 × 3轮 → 裁判",
		Mode:        "adversarial",
		Rounds:      3,
		Stages: []StageDef{
			{
				Name: "proposer", Role: "proposer",
				Prompt: `You are the PROPOSER in a structured debate. Argue IN FAVOR of the proposition.

Proposition: {objective}

{debate_context}

Make your strongest arguments:
1. Present clear, evidence-based reasoning
2. Address any counterarguments from the opponent
3. Provide specific examples and data
4. Strengthen any weakened arguments

Be persuasive but intellectually honest. Acknowledge valid opposing points while explaining why your position is stronger.`,
			},
			{
				Name: "opponent", Role: "opponent",
				Prompt: `You are the OPPONENT in a structured debate. Argue AGAINST the proposition.

Proposition: {objective}

{debate_context}

Make your strongest counterarguments:
1. Identify weaknesses in the proposer's arguments
2. Present alternative perspectives and evidence
3. Highlight risks, costs, and unintended consequences
4. Propose better alternatives if applicable

Be rigorous and critical but fair. Don't use straw man arguments.`,
			},
			{
				Name: "judge", Role: "judge",
				Prompt: `You are the JUDGE in a structured debate. Evaluate both sides and render a verdict.

Proposition: {objective}

Full Debate Transcript:
{prev_result}

Evaluate:
1. Strength of arguments from each side
2. Quality of evidence presented
3. How well each side addressed counterarguments
4. Logical consistency and coherence

Render your verdict:
- Which side presented the stronger case and why
- Key deciding factors
- Nuances and areas of agreement
- Final recommendation with caveats`,
			},
		},
	}
}

// WorkflowExecutor 工作流执行器。
// 集成 Blackboard (bMAS) + TaskTracker (V2 Task) + Structured Handoff。
type WorkflowExecutor struct {
	factory     CreateAgentFunc
	notify      NotifyFunc
	chatID      string
	taskTracker TaskTracker // 复用 V2 Task 系统 (可为 nil)
}

// Execute 执行工作流, 返回所有阶段结果
func (we *WorkflowExecutor) Execute(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	switch wf.Mode {
	case "pipeline":
		return we.executePipeline(ctx, wf, objective, team)
	case "fanout":
		return we.executeFanOut(ctx, wf, objective, team)
	case "adversarial":
		return we.executeAdversarial(ctx, wf, objective, team)
	default:
		return we.executePipeline(ctx, wf, objective, team)
	}
}

// executePipeline 串行流水线执行
func (we *WorkflowExecutor) executePipeline(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	results := make(map[string]string)
	var allResults []StageResult

	// 按依赖拓扑执行 (检测可并行的阶段)
	completed := make(map[string]bool)

	for len(completed) < len(wf.Stages) {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// 找到所有依赖已满足的阶段
		var ready []StageDef
		for _, stage := range wf.Stages {
			if completed[stage.Name] {
				continue
			}
			allDepsReady := true
			for _, dep := range stage.DependsOn {
				if !completed[dep] {
					allDepsReady = false
					break
				}
			}
			if allDepsReady {
				ready = append(ready, stage)
			}
		}

		if len(ready) == 0 {
			return allResults, fmt.Errorf("工作流死锁: 无法找到可执行的阶段")
		}

		// 检查是否有多个可并行的阶段
		parallelGroup := filterParallel(ready)
		if len(parallelGroup) > 1 {
			stageResults := we.executeParallel(ctx, parallelGroup, objective, results, team)
			for _, sr := range stageResults {
				allResults = append(allResults, sr)
				if sr.Status == TaskCompleted {
					completed[sr.Name] = true
					results[sr.Name] = sr.Output
				} else {
					return allResults, fmt.Errorf("阶段 %s 失败: %s", sr.Name, sr.Error)
				}
			}
		} else {
			stage := ready[0]
			sr := we.executeStage(ctx, stage, objective, results, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				completed[stage.Name] = true
				results[stage.Name] = sr.Output
			} else {
				return allResults, fmt.Errorf("阶段 %s 失败: %s", stage.Name, sr.Error)
			}
		}
	}
	return allResults, nil
}

// executeFanOut 并行扇出 → 汇聚
func (we *WorkflowExecutor) executeFanOut(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	return we.executePipeline(ctx, wf, objective, team)
}

// executeAdversarial 对抗辩论执行
func (we *WorkflowExecutor) executeAdversarial(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	rounds := wf.Rounds
	if rounds <= 0 {
		rounds = 3
	}

	var proposerStage, opponentStage, judgeStage *StageDef
	for i := range wf.Stages {
		switch wf.Stages[i].Role {
		case "proposer":
			proposerStage = &wf.Stages[i]
		case "opponent":
			opponentStage = &wf.Stages[i]
		case "judge":
			judgeStage = &wf.Stages[i]
		}
	}
	if proposerStage == nil || opponentStage == nil || judgeStage == nil {
		return nil, fmt.Errorf("辩论工作流需要 proposer, opponent, judge 角色")
	}

	var debateTranscript strings.Builder
	debateTranscript.WriteString("# Debate Transcript\n\n")

	// 在黑板上写入辩论主题
	if team.Blackboard != nil {
		team.Blackboard.Write("debate-topic", objective, "system", "context")
	}

	for round := 1; round <= rounds; round++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		debateCtx := debateTranscript.String()
		if round == 1 {
			debateCtx = "(This is the opening round. Present your initial arguments.)"
		}

		// 正方发言
		proposerPrompt := strings.ReplaceAll(proposerStage.Prompt, "{objective}", objective)
		proposerPrompt = strings.ReplaceAll(proposerPrompt, "{debate_context}", debateCtx)

		we.notify(we.chatID, fmt.Sprintf("🗣️ 辩论第 %d/%d 轮 — 正方发言中...", round, rounds))
		sr := we.runAgent(ctx, proposerStage.Role, proposerPrompt, team)
		sr.Name = fmt.Sprintf("round%d-proposer", round)
		allResults = append(allResults, sr)

		if sr.Status != TaskCompleted {
			return allResults, fmt.Errorf("正方第 %d 轮失败: %s", round, sr.Error)
		}
		debateTranscript.WriteString(fmt.Sprintf("## Round %d — Proposer\n%s\n\n", round, sr.Output))
		if team.Blackboard != nil {
			team.Blackboard.Write(fmt.Sprintf("round%d-proposer", round), sr.Output, "proposer", "result")
		}

		// 反方发言
		opponentPrompt := strings.ReplaceAll(opponentStage.Prompt, "{objective}", objective)
		opponentPrompt = strings.ReplaceAll(opponentPrompt, "{debate_context}", debateTranscript.String())

		we.notify(we.chatID, fmt.Sprintf("🗣️ 辩论第 %d/%d 轮 — 反方发言中...", round, rounds))
		sr = we.runAgent(ctx, opponentStage.Role, opponentPrompt, team)
		sr.Name = fmt.Sprintf("round%d-opponent", round)
		allResults = append(allResults, sr)

		if sr.Status != TaskCompleted {
			return allResults, fmt.Errorf("反方第 %d 轮失败: %s", round, sr.Error)
		}
		debateTranscript.WriteString(fmt.Sprintf("## Round %d — Opponent\n%s\n\n", round, sr.Output))
		if team.Blackboard != nil {
			team.Blackboard.Write(fmt.Sprintf("round%d-opponent", round), sr.Output, "opponent", "result")
		}
	}

	// 裁判裁决
	we.notify(we.chatID, "⚖️ 裁判裁决中...")
	judgePrompt := strings.ReplaceAll(judgeStage.Prompt, "{objective}", objective)
	judgePrompt = strings.ReplaceAll(judgePrompt, "{prev_result}", debateTranscript.String())

	sr := we.runAgent(ctx, judgeStage.Role, judgePrompt, team)
	sr.Name = "verdict"
	allResults = append(allResults, sr)

	return allResults, nil
}

// ExecuteSingleStage 公开的单阶段执行 (供 Coordinator 调用)。
func (we *WorkflowExecutor) ExecuteSingleStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	return we.executeStage(ctx, stage, objective, prevResults, team)
}

// executeStage 执行单个阶段。
// 集成 Blackboard 读/写 + V2 Task 创建/更新 + Structured Handoff。
func (we *WorkflowExecutor) executeStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	// 1. 构建 prompt: 原有模板 + Blackboard 上下文 + Handoff 信息
	bbContext := ""
	if team.Blackboard != nil {
		var completedStages []string
		for name := range prevResults {
			completedStages = append(completedStages, name)
		}
		bbContext = team.Blackboard.HandoffContext(completedStages, stage.Role)
	}
	prompt := buildStagePrompt(stage, objective, prevResults)
	if bbContext != "" {
		prompt = bbContext + "\n\n---\n\n" + prompt
	}

	// 2. 创建 V2 Task (LLM 可通过 TaskList 看到团队进度)
	var v2TaskID string
	if we.taskTracker != nil {
		taskSubject := fmt.Sprintf("[%s] %s", team.Name, stage.Name)
		id, err := we.taskTracker.AddTask(taskSubject, objective, stage.Role)
		if err == nil {
			v2TaskID = id
			_ = we.taskTracker.SetTaskStatus(id, "in_progress")
		}
	}

	we.notify(we.chatID, fmt.Sprintf("🔄 阶段 **%s** (%s) 开始执行...", stage.Name, stage.Role))

	// 3. 执行 Agent
	sr := we.runAgent(ctx, stage.Role, prompt, team)
	sr.Name = stage.Name
	sr.Role = stage.Role
	sr.V2TaskID = v2TaskID

	// 4. 将结果写入 Blackboard (bMAS 核心: Agent 执行后写回黑板)
	if team.Blackboard != nil {
		if sr.Status == TaskCompleted {
			team.Blackboard.Write(stage.Name+"-result", sr.Output, stage.Role, "result")
			team.Blackboard.Write(stage.Name+"-status", "completed", "system", "progress")
		} else {
			team.Blackboard.Write(stage.Name+"-status", "failed: "+sr.Error, "system", "progress")
		}
	}

	// 5. 更新 V2 Task 状态
	if we.taskTracker != nil && v2TaskID != "" {
		if sr.Status == TaskCompleted {
			_ = we.taskTracker.SetTaskStatus(v2TaskID, "completed")
		} else {
			_ = we.taskTracker.SetTaskStatus(v2TaskID, "failed")
		}
	}

	return sr
}

// executeParallel 并行执行多个阶段
func (we *WorkflowExecutor) executeParallel(ctx context.Context, stages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	results := make([]StageResult, len(stages))
	var wg sync.WaitGroup

	for i, stage := range stages {
		wg.Add(1)
		go func(idx int, s StageDef) {
			defer wg.Done()
			results[idx] = we.executeStage(ctx, s, objective, prevResults, team)
		}(i, stage)
	}

	wg.Wait()
	return results
}

// runAgent 创建并运行一个 agent
func (we *WorkflowExecutor) runAgent(ctx context.Context, role, prompt string, team *ProductionTeam) StageResult {
	start := time.Now()

	if we.factory == nil {
		return StageResult{Role: role, Status: TaskFailed, Error: "Agent 工厂未配置"}
	}

	// 更新 agent 状态
	team.mu.Lock()
	if ag, ok := team.Agents[role]; ok {
		ag.Status = AgentStatusRunning
	}
	team.mu.Unlock()
	team.persist()

	runner, err := we.factory(ctx, role, "")
	if err != nil {
		return StageResult{Role: role, Status: TaskFailed, Error: err.Error(), StartedAt: start, Duration: time.Since(start).String()}
	}

	result, err := runner.Execute(ctx, prompt)
	duration := time.Since(start)

	// 更新 agent 状态
	team.mu.Lock()
	if ag, ok := team.Agents[role]; ok {
		if err != nil {
			ag.Status = AgentStatusFailed
			ag.Error = err.Error()
		} else {
			ag.Status = AgentStatusCompleted
			ag.Result = truncateResult(result, 1000)
		}
	}
	team.mu.Unlock()
	team.persist()

	if err != nil {
		return StageResult{Role: role, Status: TaskFailed, Error: err.Error(), StartedAt: start, Duration: duration.Round(time.Second).String()}
	}

	return StageResult{
		Role: role, Status: TaskCompleted,
		Output: result, StartedAt: start,
		Duration: duration.Round(time.Second).String(),
	}
}

func buildStagePrompt(stage StageDef, objective string, prevResults map[string]string) string {
	prompt := stage.Prompt
	prompt = strings.ReplaceAll(prompt, "{objective}", objective)

	// 合并所有依赖阶段的输出
	var prevOutput strings.Builder
	for _, dep := range stage.DependsOn {
		if r, ok := prevResults[dep]; ok {
			prevOutput.WriteString(fmt.Sprintf("### Output from %s:\n%s\n\n", dep, r))
		}
	}
	prompt = strings.ReplaceAll(prompt, "{prev_result}", prevOutput.String())
	return prompt
}

func filterParallel(stages []StageDef) []StageDef {
	if len(stages) <= 1 {
		return stages
	}
	// 如果所有 ready stages 具有相同的依赖且标记了 parallel, 可并行
	var parallel []StageDef
	for _, s := range stages {
		if s.Parallel || len(stages) > 1 {
			parallel = append(parallel, s)
		}
	}
	// 只有多个无依赖或同依赖的才并行
	if len(parallel) > 1 {
		deps0 := strings.Join(parallel[0].DependsOn, ",")
		allSameDeps := true
		for _, p := range parallel[1:] {
			if strings.Join(p.DependsOn, ",") != deps0 {
				allSameDeps = false
				break
			}
		}
		if allSameDeps {
			return parallel
		}
	}
	return stages[:1]
}
