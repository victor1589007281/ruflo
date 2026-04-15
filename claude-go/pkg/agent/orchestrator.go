// Orchestrator — 复用 V2 TaskStore DAG 的任务编排器。
//
// 参考论文/方案:
//   - DynTaskMAS (ICAPS 2025): 动态任务图 + 异步并行执行引擎
//   - AgentOrchestra (2025): 层级化编排 + 监督协议
//   - Gradientsys (2025): 失败重试 + 上下文累积 Phoenix protocol
//
// 关键设计决策:
//   删除自建的 DAG (ParsePlanToDAG/ReadyNodes/UnblockDependents),
//   直接复用 V2 TaskStore 已有的 DAG 能力 (AddTaskWithDeps/ReadyTasks/SetTaskStatusAndUnblock)。
//   Swarm 的 topologicalLevels 也应迁移到 V2 TaskStore (统一调度器)。
//
// 职责分工:
//
//	Planner:      输出 WBS (任务分解 + 依赖图)
//	Orchestrator:  消费 WBS → 写入 V2 DAG → 调度 → micro-test → 重试 → E2E
//	V2 TaskStore: 提供 DAG 存储 + 就绪队列 + 依赖解除 (单一数据源)
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

// TaskNode 编排器的任务元数据 (与 V2 TaskStore 中的 task ID 关联)
type TaskNode struct {
	V2TaskID       string   `json:"v2TaskId"`
	Title          string   `json:"title"`
	Role           string   `json:"role"`
	DesignRef      string   `json:"designRef"`
	ConstraintRefs []string `json:"constraintRefs"`
	AcceptCriteria string   `json:"acceptCriteria"`
	MaxRetries     int      `json:"maxRetries"`

	Output      string `json:"output"`
	Error       string `json:"error"`
	Retries     int    `json:"retries"`
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

// Orchestrator 复用 V2 TaskStore 的 DAG 编排器
type Orchestrator struct {
	config OrchestratorConfig
	dag    DAGTaskTracker // 复用 V2 TaskStore 而非自建 DAG
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
	totalCount     int
}

// NewOrchestrator 创建编排器 (需要 DAGTaskTracker, 不再自建 DAG)
func NewOrchestrator(cfg OrchestratorConfig, dag DAGTaskTracker, factory CreateAgentFunc, notify NotifyFunc, pool *AgentPool, chatID string) *Orchestrator {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 3
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	return &Orchestrator{
		config: cfg,
		dag:    dag,
		nodes:  make(map[string]*TaskNode),

		factory: factory,
		notify:  notify,
		pool:    pool,
		chatID:  chatID,
	}
}

// SetDesignContext 注入设计文档 (供 micro-test 偏差检测)
func (o *Orchestrator) SetDesignContext(designDoc, planDoc string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.designDoc = designDoc
	o.planDoc = planDoc
}

// ParsePlanToDAG 解析 Planner WBS → 写入 V2 TaskStore (DAG 单一数据源)。
// 返回任务节点列表 (元数据保存在内存, DAG 关系在 TaskStore)。
func (o *Orchestrator) ParsePlanToDAG(planOutput, teamName string) ([]*TaskNode, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.dag == nil {
		return nil, fmt.Errorf("DAGTaskTracker 未配置")
	}

	var nodes []*TaskNode
	lines := strings.Split(planOutput, "\n")
	tableRe := regexp.MustCompile(`^\|\s*(\d+)\s*\|`)

	// 第一遍: 解析表格, 收集任务信息 (需要两遍因为依赖用序号而非 V2 ID)
	type rawTask struct {
		num            string
		title, role    string
		depNums        []string
		designRef      string
		constraintRefs []string
		accept         string
		priority       int
	}
	var rawTasks []rawTask

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !tableRe.MatchString(line) {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 9 {
			continue
		}
		title := strings.TrimSpace(cells[2])
		if title == "" || title == "任务" {
			continue
		}

		num := strings.TrimSpace(cells[1])
		role := strings.TrimSpace(cells[3])
		deps := strings.TrimSpace(cells[4])
		designRef := strings.TrimSpace(cells[5])
		constraints := strings.TrimSpace(cells[6])
		accept := strings.TrimSpace(cells[7])
		priStr := strings.TrimSpace(cells[8])

		var depNums []string
		if deps != "" && deps != "-" && deps != "无" {
			for _, d := range strings.Split(deps, ",") {
				d = strings.TrimSpace(d)
				d = strings.TrimPrefix(d, "#")
				if d != "" {
					depNums = append(depNums, d)
				}
			}
		}

		var cRefs []string
		if constraints != "" && constraints != "-" {
			for _, c := range strings.Split(constraints, ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					cRefs = append(cRefs, c)
				}
			}
		}

		priority := 0
		if p, err := strconv.Atoi(priStr); err == nil {
			priority = p
		}

		rawTasks = append(rawTasks, rawTask{
			num: num, title: title, role: orchNormalizeRole(role),
			depNums: depNums, designRef: designRef,
			constraintRefs: cRefs, accept: accept, priority: priority,
		})
	}

	// 第二遍: 按序号创建 V2 任务, 建立 num→v2ID 映射
	numToV2ID := make(map[string]string)
	for _, rt := range rawTasks {
		var depV2IDs []string
		for _, dn := range rt.depNums {
			if v2id, ok := numToV2ID[dn]; ok {
				depV2IDs = append(depV2IDs, v2id)
			}
		}

		subject := fmt.Sprintf("[%s] %s", teamName, rt.title)
		v2ID, err := o.dag.AddTaskWithDeps(subject, rt.accept, rt.role, depV2IDs, rt.priority)
		if err != nil {
			return nodes, fmt.Errorf("创建V2 DAG任务失败: %w", err)
		}
		numToV2ID[rt.num] = v2ID

		node := &TaskNode{
			V2TaskID:       v2ID,
			Title:          rt.title,
			Role:           rt.role,
			DesignRef:      rt.designRef,
			ConstraintRefs: rt.constraintRefs,
			AcceptCriteria: rt.accept,
			MaxRetries:     o.config.MaxRetries,
		}
		nodes = append(nodes, node)
		o.nodes[v2ID] = node
	}

	o.totalCount = len(nodes)
	return nodes, nil
}

// Execute 从 V2 TaskStore 的就绪队列循环调度, 直到所有任务完成。
func (o *Orchestrator) Execute(ctx context.Context, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	var resultsMu sync.Mutex

	if o.totalCount == 0 {
		return nil, nil
	}

	o.notify(o.chatID, fmt.Sprintf("🎯 编排器启动: %d 个任务, 最大并发 %d (V2 DAG 驱动)",
		o.totalCount, o.config.MaxParallel))
	logging.Event(ctx, "orchestrator.start", "tasks", o.totalCount, "maxParallel", o.config.MaxParallel)

	for {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// 从 V2 TaskStore 获取就绪任务 (单一数据源)
		readyV2 := o.dag.ReadyTasks()
		if len(readyV2) == 0 {
			if o.completedCount+o.failedCount >= o.totalCount {
				break
			}
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return allResults, ctx.Err()
			}
		}

		batch := readyV2
		if len(batch) > o.config.MaxParallel {
			batch = batch[:o.config.MaxParallel]
		}

		var wg sync.WaitGroup
		for _, task := range batch {
			o.mu.Lock()
			node, ok := o.nodes[task.ID]
			o.mu.Unlock()
			if !ok {
				continue
			}

			// 标记为 running
			_ = o.dag.SetTaskStatus(task.ID, "in_progress")

			wg.Add(1)
			go func(n *TaskNode, taskID string) {
				defer wg.Done()
				sr := o.executeTaskNode(ctx, n, objective, team)
				resultsMu.Lock()
				allResults = append(allResults, sr)
				resultsMu.Unlock()
			}(node, task.ID)
		}
		wg.Wait()
	}

	o.notify(o.chatID, fmt.Sprintf("🏁 编排完成: %d/%d 成功, %d 失败",
		o.completedCount, o.totalCount, o.failedCount))
	return allResults, nil
}

// executeTaskNode 执行单个任务 + micro-test + 重试
func (o *Orchestrator) executeTaskNode(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam) StageResult {
	start := time.Now()
	prompt := o.buildTaskPrompt(node, objective)

	o.notify(o.chatID, fmt.Sprintf("▶️ %s (%s) 执行中...", node.Title, node.Role))

	runner, err := o.factory(ctx, node.Role, "")
	if err != nil {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.V2TaskID, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: time.Since(start).String()})
	}

	result, err := runner.Execute(ctx, prompt)
	duration := time.Since(start)

	if err != nil {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.V2TaskID, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: duration.String()})
	}

	if reason := validateAgentOutput(result, node.Role); reason != "" {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.V2TaskID, Role: node.Role, Status: TaskFailed,
				Error: "产出验证失败: " + reason, Output: result,
				StartedAt: start, Duration: duration.String()})
	}

	node.Output = result

	if o.config.MicroTestAfter {
		o.runMicroTest(ctx, node)
	}

	// 通过 V2 TaskStore 标记完成 + 自动解除下游依赖
	o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "completed")
	o.mu.Lock()
	o.completedCount++
	o.mu.Unlock()

	testInfo := ""
	if node.TestResult != "" {
		if node.TestPassed {
			testInfo = " ✅micro-test"
		} else {
			testInfo = " ⚠️micro-test偏差"
		}
	}
	o.notify(o.chatID, fmt.Sprintf("✅ %s 完成 (%s)%s",
		node.Title, duration.Round(time.Second), testInfo))

	if team.Blackboard != nil {
		team.Blackboard.Write(node.V2TaskID+"-result", result, node.Role, "result")
	}

	return StageResult{
		Name: node.V2TaskID, Role: node.Role, Status: TaskCompleted,
		Output: result, StartedAt: start, Duration: duration.Round(time.Second).String(),
	}
}

// handleTaskFailure 失败处理 + Phoenix 重试
func (o *Orchestrator) handleTaskFailure(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, sr StageResult) StageResult {
	node.Retries++
	if node.Retries <= node.MaxRetries {
		o.notify(o.chatID, fmt.Sprintf("🔄 %s 重试 %d/%d: %s",
			node.Title, node.Retries, node.MaxRetries, sr.Error))
		node.Error = sr.Error
		_ = o.dag.SetTaskStatus(node.V2TaskID, "pending") // 重置为 pending 允许重调度
		return o.executeTaskNode(ctx, node, objective, team)
	}

	_ = o.dag.SetTaskStatus(node.V2TaskID, "failed")
	o.mu.Lock()
	o.failedCount++
	o.mu.Unlock()
	o.notify(o.chatID, fmt.Sprintf("❌ %s 最终失败 (重试 %d 次)", node.Title, node.MaxRetries))
	return sr
}

// runMicroTest 轻量级验证 (参考 TDAD 2026)
func (o *Orchestrator) runMicroTest(ctx context.Context, node *TaskNode) {
	if o.factory == nil {
		return
	}
	designCtx := o.orchDesignRefContext(node.DesignRef)
	prompt := fmt.Sprintf(`你是轻量级验证工程师 (Micro-Tester)。仅对单个任务做快速验证。

## 被验证的任务
- 任务: %s | 角色: %s | 设计章节: %s
- 约束: %s | 验收标准: %s

## 任务产出 (截取)
%s

## 快速验证 (3项, 每项 PASS/FAIL):
1. **编译完整性**: 语法正确? import 完整?
2. **接口对齐**: %s
3. **约束遵守**: %s 是否被遵守?

输出格式: 编译: PASS/FAIL | 对齐: PASS/FAIL | 约束: PASS/FAIL | 综合: PASS/FAIL`,
		node.Title, node.Role, node.DesignRef,
		strings.Join(node.ConstraintRefs, ","), node.AcceptCriteria,
		truncateResult(node.Output, 6000),
		designCtx,
		strings.Join(node.ConstraintRefs, ","))

	testCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	runner, err := o.factory(testCtx, "tester", "")
	if err != nil {
		return
	}
	result, err := runner.Execute(testCtx, prompt)
	if err != nil {
		return
	}

	node.TestResult = result
	node.TestPassed = !strings.Contains(strings.ToUpper(result), "FAIL")
	if !node.TestPassed {
		node.DriftReport = orchExtractDriftInfo(result)
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
			if len(section) > 30 || (len(section) > 2 && strings.HasPrefix(line, "## ")) {
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
		if strings.Contains(lower, "fail") || strings.Contains(lower, "偏差") {
			drifts = append(drifts, strings.TrimSpace(line))
		}
	}
	return strings.Join(drifts, "; ")
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

func (o *Orchestrator) buildTaskPrompt(node *TaskNode, objective string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 任务: %s\n项目目标: %s\n\n", node.Title, objective))

	if node.DesignRef != "" && node.DesignRef != "-" {
		b.WriteString("### 设计参考\n" + o.orchDesignRefContext(node.DesignRef) + "\n\n")
	}
	if len(node.ConstraintRefs) > 0 {
		b.WriteString("### 必须遵守的约束: " + strings.Join(node.ConstraintRefs, ", ") + "\n\n")
	}
	if node.AcceptCriteria != "" && node.AcceptCriteria != "-" {
		b.WriteString("### 验收标准\n" + node.AcceptCriteria + "\n\n")
	}
	if node.Retries > 0 {
		b.WriteString(fmt.Sprintf("### ⚠️ 重试 (第 %d 次)\n上次失败: %s\n", node.Retries, node.Error))
		if node.TestResult != "" {
			b.WriteString("Micro-Test 结果:\n" + node.TestResult + "\n")
		}
	}
	return b.String()
}

// Progress 返回编排进度
func (o *Orchestrator) Progress() (completed, total, failed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.completedCount, o.totalCount, o.failedCount
}

// MicroTestSummary 返回 micro-test 汇总
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
				drifts = append(drifts, fmt.Sprintf("%s: %s", node.Title, node.DriftReport))
			}
		}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("### Micro-Test 汇总: %d 通过, %d 失败\n", passed, failed))
	for _, d := range drifts {
		b.WriteString("- " + d + "\n")
	}
	return b.String()
}

// NodeCount 返回任务节点总数
func (o *Orchestrator) NodeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.totalCount
}
