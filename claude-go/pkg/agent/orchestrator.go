// Orchestrator — DAG 驱动的任务编排器。
//
// 参考论文/方案:
//   - DynTaskMAS (ICAPS 2025): 动态任务图 + 异步并行执行引擎, 21-33% 执行时间缩减
//   - AgentOrchestra (2025): 层级化编排 + MCP Manager Agent
//   - Gradientsys (2025): ReAct 编排 + 重试重规划机制
//   - MetaGPT SOP: 标准化操作流程 + 中间结果验证
//
// 核心职责 (区别于 Planner):
//
//	Planner:      输出 WBS (任务分解 + 依赖图 + 验收标准)
//	Orchestrator:  消费 WBS → 创建 DAG 任务 → 调度并发 → 失败重试 → 监督收尾
//
// 设计模式: Supervisor + DAG Scheduler
//
//	┌─────────────────────────────────────────────────────────────┐
//	│ Orchestrator                                                │
//	│  1. ParsePlan()  → 解析 Planner 输出为 TaskNode DAG        │
//	│  2. Schedule()   → 拓扑排序 + ReadyNodes → 并发调度        │
//	│  3. MicroTest()  → 每个 task 完成后轻量测试                │
//	│  4. Retry()      → 失败 task 重试 (最多 N 次)              │
//	│  5. Supervise()  → 监控进度 + 驱动下游 + 触发 E2E          │
//	└─────────────────────────────────────────────────────────────┘
package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
)

// TaskNode 编排器内部的任务节点 (DAG 中的一个顶点)
type TaskNode struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Role           string   `json:"role"`
	DependsOn      []string `json:"dependsOn"`
	DesignRef      string   `json:"designRef"`
	ConstraintRefs []string `json:"constraintRefs"`
	AcceptCriteria string   `json:"acceptCriteria"`
	Priority       int      `json:"priority"`
	MaxRetries     int      `json:"maxRetries"`

	Status    string    `json:"status"` // pending, blocked, running, completed, failed, retrying
	Output    string    `json:"output"`
	Error     string    `json:"error"`
	Retries   int       `json:"retries"`
	StartedAt time.Time `json:"startedAt"`
	Duration  string    `json:"duration"`

	TestResult  string `json:"testResult,omitempty"`
	TestPassed  bool   `json:"testPassed"`
	DriftReport string `json:"driftReport,omitempty"`
}

// OrchestratorConfig 编排器配置
type OrchestratorConfig struct {
	MaxParallel    int
	MaxRetries     int
	MicroTestAfter bool
}

// Orchestrator DAG 任务编排器
type Orchestrator struct {
	config OrchestratorConfig
	nodes  map[string]*TaskNode
	mu     sync.Mutex

	factory   CreateAgentFunc
	notify    NotifyFunc
	pool      *AgentPool
	chatID    string
	designDoc string
	planDoc   string

	completedCount int
	failedCount    int
}

// NewOrchestrator 创建编排器
func NewOrchestrator(cfg OrchestratorConfig, factory CreateAgentFunc, notify NotifyFunc, pool *AgentPool, chatID string) *Orchestrator {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 3
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	return &Orchestrator{
		config:  cfg,
		nodes:   make(map[string]*TaskNode),
		factory: factory,
		notify:  notify,
		pool:    pool,
		chatID:  chatID,
	}
}

// SetDesignContext 注入设计文档和计划文档 (供偏差检测使用)
func (o *Orchestrator) SetDesignContext(designDoc, planDoc string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.designDoc = designDoc
	o.planDoc = planDoc
}

// ParsePlanToDAG 从 Planner 的 WBS 输出解析任务 DAG。
// 表格格式: | # | 任务 | 角色 | 依赖 | 设计章节 | 约束编号 | 验收标准 | 优先级 |
func (o *Orchestrator) ParsePlanToDAG(planOutput string) []*TaskNode {
	o.mu.Lock()
	defer o.mu.Unlock()

	var nodes []*TaskNode
	lines := strings.Split(planOutput, "\n")

	tableRe := regexp.MustCompile(`^\|\s*(\d+)\s*\|`)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !tableRe.MatchString(line) {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 9 {
			continue
		}

		num := strings.TrimSpace(cells[1])
		title := strings.TrimSpace(cells[2])
		role := strings.TrimSpace(cells[3])
		deps := strings.TrimSpace(cells[4])
		designRef := strings.TrimSpace(cells[5])
		constraints := strings.TrimSpace(cells[6])
		accept := strings.TrimSpace(cells[7])
		priStr := strings.TrimSpace(cells[8])

		if title == "" || title == "任务" {
			continue
		}

		nodeID := fmt.Sprintf("task-%s", num)
		var depIDs []string
		if deps != "" && deps != "-" && deps != "无" {
			for _, d := range strings.Split(deps, ",") {
				d = strings.TrimSpace(d)
				d = strings.TrimPrefix(d, "#")
				if d != "" {
					depIDs = append(depIDs, "task-"+d)
				}
			}
		}

		var constraintRefs []string
		if constraints != "" && constraints != "-" {
			for _, c := range strings.Split(constraints, ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					constraintRefs = append(constraintRefs, c)
				}
			}
		}

		priority := 0
		if p, err := strconv.Atoi(priStr); err == nil {
			priority = p
		}

		status := "pending"
		if len(depIDs) > 0 {
			status = "blocked"
		}

		node := &TaskNode{
			ID:             nodeID,
			Title:          title,
			Role:           orchNormalizeRole(role),
			DependsOn:      depIDs,
			DesignRef:      designRef,
			ConstraintRefs: constraintRefs,
			AcceptCriteria: accept,
			Priority:       priority,
			MaxRetries:     o.config.MaxRetries,
			Status:         status,
		}
		nodes = append(nodes, node)
		o.nodes[nodeID] = node
	}

	return nodes
}

func orchNormalizeRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch {
	case strings.Contains(role, "coder") || strings.Contains(role, "开发"):
		return "coder"
	case strings.Contains(role, "tester") || strings.Contains(role, "测试"):
		return "tester"
	case strings.Contains(role, "review") || strings.Contains(role, "审查"):
		return "reviewer"
	default:
		return "coder"
	}
}

// ReadyNodes 返回所有可执行的任务节点 (依赖已满足), 按优先级降序。
func (o *Orchestrator) ReadyNodes() []*TaskNode {
	o.mu.Lock()
	defer o.mu.Unlock()

	var ready []*TaskNode
	for _, node := range o.nodes {
		if node.Status != "pending" {
			continue
		}
		allDepsOK := true
		for _, dep := range node.DependsOn {
			if d, ok := o.nodes[dep]; !ok || d.Status != "completed" {
				allDepsOK = false
				break
			}
		}
		if allDepsOK {
			ready = append(ready, node)
		}
	}

	for i := 1; i < len(ready); i++ {
		for j := i; j > 0 && ready[j].Priority > ready[j-1].Priority; j-- {
			ready[j], ready[j-1] = ready[j-1], ready[j]
		}
	}
	return ready
}

// UnblockDependents 完成一个任务后,解除下游任务的阻塞。
// completedID 指定刚完成的节点 (即使其 Status 字段尚未更新,也视为 completed)。
func (o *Orchestrator) UnblockDependents(completedID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	unblocked := 0
	for _, node := range o.nodes {
		if node.Status != "blocked" {
			continue
		}
		allDone := true
		for _, dep := range node.DependsOn {
			if dep == completedID {
				continue // 参数指定已完成
			}
			d, ok := o.nodes[dep]
			if !ok || d.Status != "completed" {
				allDone = false
				break
			}
		}
		if allDone {
			node.Status = "pending"
			unblocked++
		}
	}
	return unblocked
}

// Execute 执行编排: 循环调度就绪任务直到全部完成或达到终止条件。
// 核心循环 (DynTaskMAS 异步并行引擎):
//  1. 查询 ReadyNodes
//  2. 并发调度 (受 MaxParallel 限制)
//  3. 等待完成 → 解除下游依赖 → micro-test → 更新状态
//  4. 失败重试 (Gradientsys Phoenix protocol)
//  5. 重复直到所有任务完成
func (o *Orchestrator) Execute(ctx context.Context, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	var resultsMu sync.Mutex
	totalTasks := len(o.nodes)
	if totalTasks == 0 {
		return nil, nil
	}

	o.notify(o.chatID, fmt.Sprintf("🎯 编排器启动: %d 个任务, 最大并发 %d", totalTasks, o.config.MaxParallel))
	logging.Event(ctx, "orchestrator.start", "tasks", totalTasks, "maxParallel", o.config.MaxParallel)

	for {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		ready := o.ReadyNodes()
		if len(ready) == 0 {
			if o.completedCount+o.failedCount >= totalTasks {
				break
			}
			o.mu.Lock()
			hasRunning := false
			for _, n := range o.nodes {
				if n.Status == "running" || n.Status == "retrying" {
					hasRunning = true
					break
				}
			}
			o.mu.Unlock()
			if !hasRunning {
				break
			}
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return allResults, ctx.Err()
			}
		}

		batch := ready
		if len(batch) > o.config.MaxParallel {
			batch = batch[:o.config.MaxParallel]
		}

		var wg sync.WaitGroup
		for _, node := range batch {
			o.mu.Lock()
			node.Status = "running"
			node.StartedAt = time.Now()
			o.mu.Unlock()

			wg.Add(1)
			go func(n *TaskNode) {
				defer wg.Done()
				sr := o.executeTaskNode(ctx, n, objective, team)
				resultsMu.Lock()
				allResults = append(allResults, sr)
				resultsMu.Unlock()
			}(node)
		}

		wg.Wait()
	}

	o.notify(o.chatID, fmt.Sprintf("🏁 编排完成: %d/%d 成功, %d 失败",
		o.completedCount, totalTasks, o.failedCount))
	logging.Event(ctx, "orchestrator.done",
		"completed", o.completedCount, "failed", o.failedCount, "total", totalTasks)

	return allResults, nil
}

// executeTaskNode 执行单个任务节点 (含 micro-test + 偏差检测 + 重试)
func (o *Orchestrator) executeTaskNode(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam) StageResult {
	start := time.Now()
	prompt := o.buildTaskPrompt(node, objective)

	o.notify(o.chatID, fmt.Sprintf("▶️ [%s] %s (%s) 执行中...", node.ID, node.Title, node.Role))
	logging.Event(ctx, "orchestrator.task.start", "taskID", node.ID, "role", node.Role, "title", node.Title)

	runner, err := o.factory(ctx, node.Role, "")
	if err != nil {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.ID, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: time.Since(start).String()})
	}

	result, err := runner.Execute(ctx, prompt)
	duration := time.Since(start)

	if err != nil {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.ID, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: duration.String()})
	}

	if reason := validateAgentOutput(result, node.Role); reason != "" {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.ID, Role: node.Role, Status: TaskFailed,
				Error: fmt.Sprintf("产出验证失败: %s", reason), Output: result,
				StartedAt: start, Duration: duration.String()})
	}

	o.mu.Lock()
	node.Output = result
	o.mu.Unlock()

	if o.config.MicroTestAfter {
		o.runMicroTest(ctx, node, team)
	}

	o.mu.Lock()
	node.Status = "completed"
	node.Duration = duration.Round(time.Second).String()
	o.completedCount++
	o.mu.Unlock()
	o.UnblockDependents(node.ID)

	testInfo := ""
	if node.TestResult != "" {
		if node.TestPassed {
			testInfo = " ✅micro-test"
		} else {
			testInfo = " ⚠️micro-test异常"
		}
	}
	o.notify(o.chatID, fmt.Sprintf("✅ [%s] %s 完成 (%s)%s", node.ID, node.Title, node.Duration, testInfo))

	if team.Blackboard != nil {
		team.Blackboard.Write(node.ID+"-result", result, node.Role, "result")
	}

	return StageResult{
		Name: node.ID, Role: node.Role, Status: TaskCompleted,
		Output: result, StartedAt: start, Duration: duration.Round(time.Second).String(),
	}
}

// handleTaskFailure 处理任务失败 (含重试逻辑, 参考 Gradientsys Phoenix protocol)
func (o *Orchestrator) handleTaskFailure(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, sr StageResult) StageResult {
	o.mu.Lock()
	node.Retries++
	canRetry := node.Retries <= node.MaxRetries
	o.mu.Unlock()

	if canRetry {
		o.notify(o.chatID, fmt.Sprintf("🔄 [%s] %s 失败 (重试 %d/%d): %s",
			node.ID, node.Title, node.Retries, node.MaxRetries, sr.Error))
		logging.Event(ctx, "orchestrator.task.retry",
			"taskID", node.ID, "retry", node.Retries, "error", sr.Error)

		o.mu.Lock()
		node.Status = "retrying"
		node.Error = sr.Error
		o.mu.Unlock()

		return o.executeTaskNode(ctx, node, objective, team)
	}

	o.mu.Lock()
	node.Status = "failed"
	node.Error = sr.Error
	o.failedCount++
	o.mu.Unlock()

	o.notify(o.chatID, fmt.Sprintf("❌ [%s] %s 最终失败 (已重试 %d 次): %s",
		node.ID, node.Title, node.MaxRetries, sr.Error))

	return sr
}

// runMicroTest 轻量级测试: 针对单个任务的快速验证。
// 参考 TDAD (2026): 基于影响图分析选择性运行测试, 而非全量测试。
func (o *Orchestrator) runMicroTest(ctx context.Context, node *TaskNode, _ *ProductionTeam) {
	if o.factory == nil {
		return
	}

	designCtx := o.orchDesignRefContext(node.DesignRef)
	prompt := fmt.Sprintf(`你是轻量级验证工程师 (Micro-Tester)。
仅对以下单个任务的产出做快速验证, 不需要写完整测试代码。

## 被验证的任务
- 任务: %s
- 角色: %s
- 设计章节: %s
- 关联约束: %s
- 验收标准: %s

## 任务产出
%s

## 快速验证 (3项检查, 每项 PASS/FAIL):

### 1. 编译完整性
产出的代码片段是否语法正确? 是否有明显的 import 缺失或类型错误?

### 2. 接口对齐
%s

### 3. 约束遵守
检查约束 %s 在本任务产出中是否被遵守。

## 输出格式 (简洁):
编译: PASS/FAIL (原因)
对齐: PASS/FAIL (偏差说明)
约束: PASS/FAIL (违反项)
综合: PASS/FAIL`,
		node.Title, node.Role, node.DesignRef,
		strings.Join(node.ConstraintRefs, ","), node.AcceptCriteria,
		truncateResult(node.Output, 8000),
		designCtx,
		strings.Join(node.ConstraintRefs, ","))

	testCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	runner, err := o.factory(testCtx, "tester", "")
	if err != nil {
		node.TestResult = "micro-test 创建失败: " + err.Error()
		node.TestPassed = false
		return
	}

	result, err := runner.Execute(testCtx, prompt)
	if err != nil {
		node.TestResult = "micro-test 执行失败: " + err.Error()
		node.TestPassed = false
		return
	}

	node.TestResult = result
	node.TestPassed = !strings.Contains(strings.ToUpper(result), "FAIL")

	if !node.TestPassed {
		node.DriftReport = orchExtractDriftInfo(result)
		logging.Event(ctx, "orchestrator.microtest.fail",
			"taskID", node.ID, "drift", node.DriftReport != "")
	}
}

func (o *Orchestrator) orchDesignRefContext(ref string) string {
	o.mu.Lock()
	doc := o.designDoc
	o.mu.Unlock()

	if doc == "" || ref == "" || ref == "-" {
		return "无设计文档参考"
	}
	lower := strings.ToLower(ref)
	lines := strings.Split(doc, "\n")
	var section []string
	capturing := false
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), lower) {
			capturing = true
		}
		if capturing {
			section = append(section, line)
			if len(section) > 30 {
				break
			}
			if len(section) > 2 && strings.HasPrefix(line, "## ") {
				break
			}
		}
	}
	if len(section) > 0 {
		return "设计文档相关章节:\n" + strings.Join(section, "\n")
	}
	return "设计文档参考: " + ref
}

func orchExtractDriftInfo(testResult string) string {
	var drifts []string
	for _, line := range strings.Split(testResult, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "fail") || strings.Contains(lower, "偏差") || strings.Contains(lower, "不一致") {
			drifts = append(drifts, strings.TrimSpace(line))
		}
	}
	if len(drifts) > 0 {
		return strings.Join(drifts, "; ")
	}
	return ""
}

// buildTaskPrompt 构建任务执行 prompt (包含依赖输出 + 设计约束 + 重试上下文)
func (o *Orchestrator) buildTaskPrompt(node *TaskNode, objective string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("## 任务: %s\n\n", node.Title))
	b.WriteString(fmt.Sprintf("项目目标: %s\n\n", objective))

	if len(node.DependsOn) > 0 {
		b.WriteString("### 前置任务产出\n")
		o.mu.Lock()
		for _, depID := range node.DependsOn {
			if dep, ok := o.nodes[depID]; ok && dep.Output != "" {
				output := dep.Output
				if len(output) > 6000 {
					output = output[:6000] + "\n...(已截断)"
				}
				b.WriteString(fmt.Sprintf("#### %s: %s\n%s\n\n", depID, dep.Title, output))
			}
		}
		o.mu.Unlock()
	}

	if node.DesignRef != "" && node.DesignRef != "-" {
		b.WriteString(fmt.Sprintf("### 设计参考 (章节: %s)\n", node.DesignRef))
		b.WriteString(o.orchDesignRefContext(node.DesignRef))
		b.WriteString("\n\n")
	}

	if len(node.ConstraintRefs) > 0 {
		b.WriteString(fmt.Sprintf("### 必须遵守的约束: %s\n", strings.Join(node.ConstraintRefs, ", ")))
		b.WriteString("完成后请附上: **约束检查:** " + strings.Join(node.ConstraintRefs, " ✅ | ") + " ✅\n\n")
	}

	if node.AcceptCriteria != "" && node.AcceptCriteria != "-" {
		b.WriteString(fmt.Sprintf("### 验收标准\n%s\n\n", node.AcceptCriteria))
	}

	if node.Retries > 0 {
		b.WriteString(fmt.Sprintf("### ⚠️ 重试 (第 %d 次, 上次失败原因: %s)\n", node.Retries, node.Error))
		if node.TestResult != "" {
			b.WriteString(fmt.Sprintf("上次 Micro-Test 结果:\n%s\n\n", node.TestResult))
		}
		b.WriteString("请修复上述问题后重新执行。\n\n")
	}

	return b.String()
}

// Progress 返回编排进度
func (o *Orchestrator) Progress() (completed, total, failed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.completedCount, len(o.nodes), o.failedCount
}

// MicroTestSummary 返回所有 micro-test 的汇总 (供 evaluator 参考)
func (o *Orchestrator) MicroTestSummary() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	var passed, failed int
	var drifts []string
	for _, node := range o.nodes {
		if node.TestResult == "" {
			continue
		}
		if node.TestPassed {
			passed++
		} else {
			failed++
			if node.DriftReport != "" {
				drifts = append(drifts, fmt.Sprintf("[%s] %s: %s", node.ID, node.Title, node.DriftReport))
			}
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("### Micro-Test 汇总: %d 通过, %d 失败\n", passed, failed))
	if len(drifts) > 0 {
		b.WriteString("### 偏差报告:\n")
		for _, d := range drifts {
			b.WriteString("- " + d + "\n")
		}
	}
	return b.String()
}

// NodeCount 返回任务节点总数
func (o *Orchestrator) NodeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.nodes)
}
