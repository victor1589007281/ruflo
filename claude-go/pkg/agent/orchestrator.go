// Orchestrator — 复用 V2 TaskStore DAG 的任务编排器。
//
// 参考论文/方案:
//   - DynTaskMAS (ICAPS 2025): 动态任务图 + 异步并行执行引擎
//   - AgentOrchestra (2025): 层级化编排 + 监督协议
//   - Gradientsys (2025): 失败重试 + 上下文累积 Phoenix protocol
//
// 关键设计决策:
//
//	删除自建的 DAG (ParsePlanToDAG/ReadyNodes/UnblockDependents),
//	直接复用 V2 TaskStore 已有的 DAG 能力 (AddTaskWithDeps/ReadyTasks/SetTaskStatusAndUnblock)。
//	Swarm 的 topologicalLevels 也应迁移到 V2 TaskStore (统一调度器)。
//
// 职责分工:
//
//	Planner:      输出 WBS (任务分解 + 依赖图)
//	Orchestrator:  消费 WBS → 写入 V2 DAG → 调度 → micro-test → 重试 → E2E
//	V2 TaskStore: 提供 DAG 存储 + 就绪队列 + 依赖解除 (单一数据源)
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
)

// Bottleneck micro-test 识别的瓶颈 (参考 GLM 5.1 benchmark-driven 优化)。
type Bottleneck struct {
	Type     string `json:"type"`     // "compilation", "logic", "design_drift", "constraint"
	Severity string `json:"severity"` // "blocking", "degrading"
	Detail   string `json:"detail"`
}

// ClassifyBottlenecks 从 micro-test 结果中分类瓶颈。
// 按 "field: PASS/FAIL" 格式逐段匹配, 避免跨段误判。
func ClassifyBottlenecks(testResult string) []Bottleneck {
	if testResult == "" {
		return nil
	}
	upper := strings.ToUpper(testResult)
	segments := strings.Split(upper, "|")
	var bns []Bottleneck
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if !strings.Contains(seg, "FAIL") {
			continue
		}
		switch {
		case strings.Contains(seg, "编译") || strings.Contains(seg, "COMPIL"):
			bns = append(bns, Bottleneck{Type: "compilation", Severity: "blocking", Detail: "编译/语法错误"})
		case strings.Contains(seg, "对齐") || strings.Contains(seg, "ALIGN"):
			bns = append(bns, Bottleneck{Type: "design_drift", Severity: "degrading", Detail: "接口/设计偏差"})
		case strings.Contains(seg, "约束") || strings.Contains(seg, "CONSTRAINT"):
			bns = append(bns, Bottleneck{Type: "constraint", Severity: "degrading", Detail: "约束违反"})
		default:
			bns = append(bns, Bottleneck{Type: "logic", Severity: "degrading", Detail: "逻辑/测试失败"})
		}
	}
	return bns
}

// SubGoal 子目标 (参考 DeepSeek Prover-V2 子目标分解验证)。
// 复杂 task 可分解为可独立验证的子步骤, 精确定位失败点。
type SubGoal struct {
	Description string `json:"description"`
	Verifier    string `json:"verifier,omitempty"` // e.g. "go build", "go test -run XXX"
	Passed      bool   `json:"passed"`
}

// TaskNode 编排器的任务元数据 (与 V2 TaskStore 中的 task ID 关联)
type TaskNode struct {
	V2TaskID       string    `json:"v2TaskId"`
	Title          string    `json:"title"`
	Role           string    `json:"role"`
	DesignRef      string    `json:"designRef"`
	ConstraintRefs []string  `json:"constraintRefs"`
	AcceptCriteria string    `json:"acceptCriteria"`
	MaxRetries     int       `json:"maxRetries"`
	Complexity     string    `json:"complexity,omitempty"` // "simple"(1轮), "medium"(2轮), "complex"(3轮)
	SubGoals       []SubGoal `json:"subGoals,omitempty"`
	TargetPackages []string  `json:"targetPackages,omitempty"` // 该任务涉及的包路径 (如 "./internal/storage/...")
	TargetFiles    []string  `json:"targetFiles,omitempty"`    // 该任务涉及的具体文件
	TaskType       string    `json:"taskType,omitempty"`       // "macro", "leaf", "verification"
	ParentID       string    `json:"parentId,omitempty"`
	EstimatedMin   int       `json:"estimatedMinutes,omitempty"`
	RiskLevel      string    `json:"riskLevel,omitempty"` // "low", "medium", "high"
	VerifyCommand  string    `json:"verifyCommand,omitempty"`
	ParallelGroup  string    `json:"parallelGroup,omitempty"`
	BlockingPolicy string    `json:"blockingPolicy,omitempty"` // "fail_blocks_dependents", "fail_open"
	SplitReason    string    `json:"splitReason,omitempty"`

	Output      string `json:"output"`
	Error       string `json:"error"`
	Retries     int    `json:"retries"`
	TestResult  string `json:"testResult,omitempty"`
	TestPassed  bool   `json:"testPassed"`
	DriftReport string `json:"driftReport,omitempty"`
}

// OrchestratorConfig 编排器配置
type OrchestratorConfig struct {
	MaxParallel      int
	MaxRetries       int
	MicroTestAfter   bool
	AdversarialRound int // 每个 task 内 mini 对抗轮数上限 (0=默认2)
}

// StageFlusher 回调: Orchestrator 每批任务完成后调用, 让调用方增量刷新 team.json。
type StageFlusher func(results []StageResult)

// Orchestrator 复用 V2 TaskStore 的 DAG 编排器
type Orchestrator struct {
	config OrchestratorConfig
	dag    DAGTaskTracker
	nodes  map[string]*TaskNode
	mu     sync.Mutex

	factory          CreateAgentFunc
	notify           NotifyFunc
	pool             *AgentPool
	chatID           string
	designDoc        string
	planDoc          string
	checkpoints      CheckpointStore                                                      // 检查点 (从 WorkflowExecutor 传入, 可为 nil)
	flusher          StageFlusher                                                         // 增量刷新回调 (可为 nil)
	activityCallback func()                                                               // Coordinator 活动追踪回调
	progressCallback func(phase string, iteration int, bytesWritten int64, taskID string) // 进展上报回调

	completedCount int
	failedCount    int
	totalCount     int
	dagMaxWidth    int
	teamName       string // 用于 resume 时识别孤儿任务

	wbsSplitCount        int
	wbsTimeoutSplitCount int
}

// rawTask ParsePlanToDAG 内部用的中间表示
type rawTask struct {
	num            string
	title, role    string
	depNums        []string
	designRef      string
	constraintRefs []string
	accept         string
	priority       int
	complexity     string // "simple", "medium", "complex"
	subGoals       []SubGoal
	targetPackages []string // 该任务涉及的包路径 (如 "./internal/storage/...")
	targetFiles    []string // 该任务涉及的具体文件 (如 "internal/storage/engine.go")
	taskType       string
	parentID       string
	estimatedMin   int
	riskLevel      string
	verifyCommand  string
	parallelGroup  string
	blockingPolicy string
	splitReason    string
}

const (
	wbsTaskTypeMacro        = "macro"
	wbsTaskTypeLeaf         = "leaf"
	wbsTaskTypeVerification = "verification"

	wbsRiskLow    = "low"
	wbsRiskMedium = "medium"
	wbsRiskHigh   = "high"

	wbsBlockingFailBlocks = "fail_blocks_dependents"
	wbsBlockingFailOpen   = "fail_open"
)

// NewOrchestrator 创建编排器 (需要 DAGTaskTracker, 不再自建 DAG)
func NewOrchestrator(cfg OrchestratorConfig, dag DAGTaskTracker, factory CreateAgentFunc, notify NotifyFunc, pool *AgentPool, chatID string) *Orchestrator {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 3
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.AdversarialRound <= 0 {
		cfg.AdversarialRound = 2
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

// SetStageFlusher 注入增量刷新回调, Execute 每批任务完成后调用。
func (o *Orchestrator) SetStageFlusher(fn StageFlusher) {
	o.mu.Lock()
	o.flusher = fn
	o.mu.Unlock()
}

// SetActivityCallback 注入 Coordinator 活动追踪回调, 使 Orchestrator 执行期间能刷新 watchdog 计时器。
func (o *Orchestrator) SetActivityCallback(fn func()) {
	o.activityCallback = fn
}

// SetProgressCallback 注入进展上报回调, 用于 watchdog 区分 "进程活着" 和 "任务在前进"。
func (o *Orchestrator) SetProgressCallback(fn func(phase string, iteration int, bytesWritten int64, taskID string)) {
	o.progressCallback = fn
}

func (o *Orchestrator) reportProgress(phase string, iteration int, bytesWritten int64, taskID string) {
	if o.progressCallback != nil {
		o.progressCallback(phase, iteration, bytesWritten, taskID)
	}
}

func (o *Orchestrator) touchActivity() {
	if o.activityCallback != nil {
		o.activityCallback()
	}
}

// SetCheckpointStore 注入检查点存取 (供每个 task 完成后持久化)。
func (o *Orchestrator) SetCheckpointStore(cs CheckpointStore) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.checkpoints = cs
}

// SetDesignContext 注入设计文档 (供 micro-test 偏差检测)
func (o *Orchestrator) SetDesignContext(designDoc, planDoc string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.designDoc = designDoc
	o.planDoc = planDoc
}

// ParsePlanToDAG 解析 Planner WBS → 写入 V2 TaskStore (DAG 单一数据源)。
// 多策略解析: JSON (优先) → 宽松 markdown 表格 → 编号列表。
// 返回任务节点列表 (元数据保存在内存, DAG 关系在 TaskStore)。
func (o *Orchestrator) ParsePlanToDAG(planOutput, teamName string) ([]*TaskNode, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.dag == nil {
		return nil, fmt.Errorf("DAGTaskTracker 未配置")
	}

	rawTasks := o.multiStrategyParse(planOutput)
	if len(rawTasks) == 0 {
		return nil, nil
	}
	rawTasks = o.normalizeAndSplitRawTasks(rawTasks)
	return o.rawTasksToDAG(rawTasks, teamName)
}

// ParsePlanToDAGWithRepair 带 repair loop 的解析: 多策略 → repair prompt → fallback。
func (o *Orchestrator) ParsePlanToDAGWithRepair(ctx context.Context, planOutput, objective, teamName string, llmFactory CreateAgentFunc) ([]*TaskNode, error) {
	o.mu.Lock()

	if o.dag == nil {
		o.mu.Unlock()
		return nil, fmt.Errorf("DAGTaskTracker 未配置")
	}

	if synthesized := synthesizeObjectiveWBS(objective); len(synthesized) > 0 {
		o.notify(o.chatID, "🧩 WBS policy: 新 Go 目标目录使用稳定内置 Leaf DAG")
		rawTasks := o.normalizeAndSplitRawTasks(synthesized)
		nodes, err := o.rawTasksToDAG(rawTasks, teamName)
		o.mu.Unlock()
		return nodes, err
	}

	// 层 1+2: 多策略解析
	rawTasks := o.multiStrategyParse(planOutput)
	if len(rawTasks) > 0 && validateParsedWBSForObjective(rawTasks, objective) == nil {
		rawTasks = o.normalizeAndSplitRawTasks(rawTasks)
		nodes, err := o.rawTasksToDAG(rawTasks, teamName)
		o.mu.Unlock()
		return nodes, err
	} else if len(rawTasks) > 0 {
		o.notify(o.chatID, "⚠️ Planner WBS 未满足目标目录/可执行 Leaf 约束, 尝试 repair")
	}

	// 层 3: Repair Prompt (1 轮 LLM 修复)
	o.mu.Unlock()
	if llmFactory != nil {
		repaired := o.repairPlanFormat(ctx, planOutput, objective, llmFactory)
		if repaired != "" {
			o.mu.Lock()
			rawTasks = o.multiStrategyParse(repaired)
			if len(rawTasks) > 0 && validateParsedWBSForObjective(rawTasks, objective) == nil {
				rawTasks = o.normalizeAndSplitRawTasks(rawTasks)
				nodes, err := o.rawTasksToDAG(rawTasks, teamName)
				o.mu.Unlock()
				return nodes, err
			} else if len(rawTasks) > 0 {
				o.notify(o.chatID, "⚠️ Repair WBS 仍未满足目标目录/可执行 Leaf 约束, 使用 objective fallback")
			}
			o.mu.Unlock()
		}
	}

	// 层 4: Fallback — 从自由文本提取最小 DAG
	o.mu.Lock()
	rawTasks = o.fallbackExtractTasks(planOutput)
	if len(rawTasks) > 0 && validateParsedWBSForObjective(rawTasks, objective) == nil {
		o.notify(o.chatID, "⚠️ WBS 格式解析失败, 使用 fallback 最小 DAG")
		rawTasks = o.normalizeAndSplitRawTasks(rawTasks)
		nodes, err := o.rawTasksToDAG(rawTasks, teamName)
		o.mu.Unlock()
		return nodes, err
	}
	if synthesized := synthesizeObjectiveWBS(objective); len(synthesized) > 0 {
		o.notify(o.chatID, "🧩 WBS fallback: 基于目标目录合成可执行 Go 项目 Leaf DAG")
		rawTasks = o.normalizeAndSplitRawTasks(synthesized)
		nodes, err := o.rawTasksToDAG(rawTasks, teamName)
		o.mu.Unlock()
		return nodes, err
	}
	o.mu.Unlock()
	return nil, nil
}

// multiStrategyParse 依次尝试 JSON → 表格 → 编号列表三种策略解析 rawTasks。
func (o *Orchestrator) multiStrategyParse(planOutput string) []rawTask {
	// 策略 1: JSON
	if tasks := parseWBSFromJSON(planOutput); len(tasks) > 0 {
		return tasks
	}
	// 策略 2: markdown 表格 (宽松)
	if tasks := parseWBSFromTable(planOutput); len(tasks) > 0 {
		return tasks
	}
	// 策略 3: 编号列表
	return parseWBSFromNumberedList(planOutput)
}

// rawTasksToDAG 将 rawTasks 写入 V2 DAG, 返回 TaskNode 列表 (调用者需持有 o.mu)。
func (o *Orchestrator) rawTasksToDAG(rawTasks []rawTask, teamName string) ([]*TaskNode, error) {
	o.teamName = teamName
	var nodes []*TaskNode
	numToV2ID := make(map[string]string)
	groupLastV2ID := make(map[string]string)
	groupLastNum := make(map[string]string)
	fileLastV2ID := make(map[string]string)
	fileLastNum := make(map[string]string)
	widthTasks := make([]rawTask, 0, len(rawTasks))
	for _, rt := range rawTasks {
		var depV2IDs []string
		for _, dn := range rt.depNums {
			if v2id, ok := numToV2ID[dn]; ok {
				depV2IDs = append(depV2IDs, v2id)
			}
		}
		if rt.parallelGroup != "" {
			if prev, ok := groupLastV2ID[rt.parallelGroup]; ok && !containsString(depV2IDs, prev) {
				depV2IDs = append(depV2IDs, prev)
			}
			if prevNum, ok := groupLastNum[rt.parallelGroup]; ok && !containsString(rt.depNums, prevNum) {
				rt.depNums = append(rt.depNums, prevNum)
			}
		}
		for _, file := range rt.targetFiles {
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			if prev, ok := fileLastV2ID[file]; ok && !containsString(depV2IDs, prev) {
				depV2IDs = append(depV2IDs, prev)
			}
			if prevNum, ok := fileLastNum[file]; ok && !containsString(rt.depNums, prevNum) {
				rt.depNums = append(rt.depNums, prevNum)
			}
		}

		subject := orchestratorTaskSubject(teamName, rt.title)
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
			Complexity:     rt.complexity,
			SubGoals:       rt.subGoals,
			TargetPackages: rt.targetPackages,
			TargetFiles:    rt.targetFiles,
			TaskType:       rt.taskType,
			ParentID:       rt.parentID,
			EstimatedMin:   rt.estimatedMin,
			RiskLevel:      rt.riskLevel,
			VerifyCommand:  rt.verifyCommand,
			ParallelGroup:  rt.parallelGroup,
			BlockingPolicy: rt.blockingPolicy,
			SplitReason:    rt.splitReason,
		}
		nodes = append(nodes, node)
		o.nodes[v2ID] = node
		if rt.parallelGroup != "" {
			groupLastV2ID[rt.parallelGroup] = v2ID
			groupLastNum[rt.parallelGroup] = rt.num
		}
		for _, file := range rt.targetFiles {
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			fileLastV2ID[file] = v2ID
			fileLastNum[file] = rt.num
		}
		widthTasks = append(widthTasks, rt)
	}

	o.totalCount = len(nodes)
	o.dagMaxWidth = o.computeDAGWidth(widthTasks)
	if o.dagMaxWidth > 0 && (o.config.MaxParallel <= 0 || o.config.MaxParallel > o.dagMaxWidth) {
		o.config.MaxParallel = o.dagMaxWidth
	}
	return nodes, nil
}

// --- 策略 1: JSON 解析 ---

type wbsJSON struct {
	Tasks []wbsJSONTask `json:"tasks"`
}
type wbsJSONTask struct {
	ID             flexibleWBSID   `json:"id"`
	Title          string          `json:"title"`
	Role           string          `json:"role"`
	DependsOn      []flexibleWBSID `json:"dependsOn"`
	DesignRef      string          `json:"designRef"`
	Constraints    []string        `json:"constraints"`
	Acceptance     string          `json:"acceptance"`
	Priority       int             `json:"priority"`
	Complexity     string          `json:"complexity,omitempty"` // "simple", "medium", "complex"
	SubGoals       []SubGoal       `json:"subGoals,omitempty"`
	TargetPackages []string        `json:"targetPackages,omitempty"` // 该任务涉及的包路径
	TargetFiles    []string        `json:"targetFiles,omitempty"`    // 该任务涉及的具体文件
	TaskType       string          `json:"taskType,omitempty"`
	ParentID       flexibleWBSID   `json:"parentId,omitempty"`
	EstimatedMin   int             `json:"estimatedMinutes,omitempty"`
	RiskLevel      string          `json:"riskLevel,omitempty"`
	VerifyCommand  string          `json:"verifyCommand,omitempty"`
	ParallelGroup  string          `json:"parallelGroup,omitempty"`
	BlockingPolicy string          `json:"blockingPolicy,omitempty"`
	SplitReason    string          `json:"splitReason,omitempty"`
}

type flexibleWBSID string

func (id *flexibleWBSID) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*id = ""
		return nil
	}
	var quoted string
	if err := json.Unmarshal(data, &quoted); err == nil {
		*id = flexibleWBSID(strings.TrimSpace(quoted))
		return nil
	}
	*id = flexibleWBSID(s)
	return nil
}

func stripCodeFences(s string) string {
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\n?(.*?)```")
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return s
}

func parseWBSFromJSON(planOutput string) []rawTask {
	body := stripCodeFences(planOutput)

	var wbs wbsJSON
	if err := json.Unmarshal([]byte(body), &wbs); err != nil {
		// 尝试从文本中找到 JSON 对象
		start := strings.Index(body, "{")
		end := strings.LastIndex(body, "}")
		if start >= 0 && end > start {
			if err2 := json.Unmarshal([]byte(body[start:end+1]), &wbs); err2 != nil {
				return nil
			}
		} else {
			return nil
		}
	}
	if len(wbs.Tasks) == 0 {
		return nil
	}

	var tasks []rawTask
	for _, t := range wbs.Tasks {
		id := strings.TrimSpace(string(t.ID))
		if id == "" {
			continue
		}
		var deps []string
		for _, d := range t.DependsOn {
			dep := strings.TrimSpace(string(d))
			if dep != "" {
				deps = append(deps, dep)
			}
		}
		tasks = append(tasks, rawTask{
			num: id, title: t.Title,
			role: orchNormalizeRole(t.Role), depNums: deps,
			designRef: t.DesignRef, constraintRefs: t.Constraints,
			accept: t.Acceptance, priority: t.Priority,
			complexity: t.Complexity, subGoals: t.SubGoals,
			targetPackages: t.TargetPackages, targetFiles: t.TargetFiles,
			taskType:       t.TaskType,
			parentID:       strings.TrimSpace(string(t.ParentID)),
			estimatedMin:   t.EstimatedMin,
			riskLevel:      t.RiskLevel,
			verifyCommand:  t.VerifyCommand,
			parallelGroup:  t.ParallelGroup,
			blockingPolicy: t.BlockingPolicy,
			splitReason:    t.SplitReason,
		})
	}
	return tasks
}

// --- 策略 2: 宽松 markdown 表格 ---

func parseWBSFromTable(planOutput string) []rawTask {
	lines := strings.Split(planOutput, "\n")
	tableRe := regexp.MustCompile(`^\|\s*(\d+)\s*\|`)
	var tasks []rawTask

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !tableRe.MatchString(line) {
			continue
		}
		cells := strings.Split(line, "|")
		// 宽松: 只需 >= 6 列 (id|title|role|deps|...|)
		if len(cells) < 6 {
			continue
		}
		title := strings.TrimSpace(cells[2])
		if title == "" || title == "任务" || title == "Task" {
			continue
		}

		num := strings.TrimSpace(cells[1])
		role := ""
		if len(cells) > 3 {
			role = strings.TrimSpace(cells[3])
		}
		deps := ""
		if len(cells) > 4 {
			deps = strings.TrimSpace(cells[4])
		}
		designRef := ""
		if len(cells) > 5 {
			designRef = strings.TrimSpace(cells[5])
		}
		constraints := ""
		if len(cells) > 6 {
			constraints = strings.TrimSpace(cells[6])
		}
		accept := ""
		if len(cells) > 7 {
			accept = strings.TrimSpace(cells[7])
		}
		priStr := ""
		if len(cells) > 8 {
			priStr = strings.TrimSpace(cells[8])
		}

		depNums := parseDepsString(deps)
		cRefs := splitTrimNonEmpty(constraints, ",")
		priority := 0
		if p, err := strconv.Atoi(priStr); err == nil {
			priority = p
		}

		tasks = append(tasks, rawTask{
			num: num, title: title, role: orchNormalizeRole(role),
			depNums: depNums, designRef: designRef,
			constraintRefs: cRefs, accept: accept, priority: priority,
		})
	}
	return tasks
}

// --- 策略 3: 编号列表启发式 ---

func parseWBSFromNumberedList(planOutput string) []rawTask {
	lines := strings.Split(planOutput, "\n")
	listRe := regexp.MustCompile(`^\s*(\d+)[.)]\s+(.+)`)
	roleRe := regexp.MustCompile(`(?i)(?:角色|role)[:\s]*(\S+)`)
	depRe := regexp.MustCompile(`(?i)(?:依赖|depends?(?:\s*on)?|dep)[:\s]*([#\d,\s]+)`)
	var tasks []rawTask

	for _, line := range lines {
		m := listRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		num := m[1]
		rest := m[2]
		title := rest

		role := "coder"
		if rm := roleRe.FindStringSubmatch(rest); rm != nil {
			role = orchNormalizeRole(rm[1])
			title = strings.Replace(title, rm[0], "", 1)
		}

		var deps []string
		if dm := depRe.FindStringSubmatch(rest); dm != nil {
			deps = parseDepsString(dm[1])
			title = strings.Replace(title, dm[0], "", 1)
		}

		title = strings.TrimSpace(strings.TrimRight(title, " -—|"))
		if title == "" {
			continue
		}

		tasks = append(tasks, rawTask{
			num: num, title: title, role: role, depNums: deps,
		})
	}
	return tasks
}

// --- Repair Prompt (1 轮 LLM 修复) ---

func (o *Orchestrator) repairPlanFormat(ctx context.Context, badOutput, objective string, factory CreateAgentFunc) string {
	targetRoot := inferObjectiveTargetRoot(objective)
	targetRule := ""
	if targetRoot != "" {
		targetRule = fmt.Sprintf("\n硬约束: 用户指定输出目录为 %s。所有 targetFiles 必须以 %s/ 开头; 新 Go 项目的第一个 Leaf 必须包含 %s/go.mod; verification 使用 cd %s && go test ./...。\n",
			targetRoot, targetRoot, targetRoot, targetRoot)
	}
	prompt := fmt.Sprintf(`以下开发计划的格式无法被系统解析或不可执行。请将其转换为严格 JSON, 不要添加任何解释:

项目目标:
%s
%s

%sjson
{
  "tasks": [
    {"id": 1, "title": "...", "role": "coder", "taskType": "leaf", "dependsOn": [], "designRef": "", "constraints": [], "acceptance": "...", "priority": 1, "estimatedMinutes": 3, "riskLevel": "medium", "blockingPolicy": "fail_blocks_dependents"}
  ]
}
%s

原始计划:
%s

请直接输出 JSON (用 %sjson ... %s 包裹):`, objective, targetRule, "```", "```", truncateResult(badOutput, 8000), "```", "```")

	agent, err := factory(ctx, "planner", "")
	if err != nil {
		return ""
	}
	result, err := agent.Execute(ctx, prompt)
	if err != nil {
		return ""
	}
	return result
}

// --- Fallback: 从自由文本提取最小 DAG ---

func (o *Orchestrator) fallbackExtractTasks(planOutput string) []rawTask {
	lines := strings.Split(planOutput, "\n")
	taskRe := regexp.MustCompile(`(?i)(?:task|任务|步骤|step)\s*#?\d*[.:：]?\s*(.{5,80})`)
	var tasks []rawTask
	id := 1

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if m := taskRe.FindStringSubmatch(line); m != nil {
			title := strings.TrimSpace(m[1])
			title = strings.TrimRight(title, " -—|:：")
			if title == "" {
				continue
			}
			tasks = append(tasks, rawTask{
				num: strconv.Itoa(id), title: title, role: "coder",
			})
			id++
			if id > 20 {
				break
			}
		}
	}

	// 如果上面提取不到,尝试提取 markdown header 作为任务
	if len(tasks) == 0 {
		headerRe := regexp.MustCompile(`^#{2,4}\s+(.{5,80})`)
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if m := headerRe.FindStringSubmatch(line); m != nil {
				title := strings.TrimSpace(m[1])
				if strings.Contains(strings.ToLower(title), "职责") || strings.Contains(strings.ToLower(title), "偏差") {
					continue
				}
				tasks = append(tasks, rawTask{
					num: strconv.Itoa(id), title: title, role: "coder",
				})
				id++
				if id > 15 {
					break
				}
			}
		}
	}
	return tasks
}

func validateParsedWBSForObjective(rawTasks []rawTask, objective string) error {
	if len(rawTasks) == 0 {
		return fmt.Errorf("empty WBS")
	}
	genericMeta := 0
	for _, task := range rawTasks {
		if isGenericMetaWBSTitle(task.title) {
			genericMeta++
		}
	}
	if genericMeta == len(rawTasks) {
		return fmt.Errorf("WBS contains only meta/review tasks")
	}

	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" {
		return nil
	}
	if len(rawTasks) > 12 {
		return fmt.Errorf("WBS has %d tasks for target root %s; prefer bounded objective fallback", len(rawTasks), targetRoot)
	}
	targetPrefix := targetRoot + "/"
	hasTargetFile := false
	hasGoMod := false
	for _, task := range rawTasks {
		for _, file := range task.targetFiles {
			clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
			if strings.HasPrefix(clean, targetPrefix) {
				hasTargetFile = true
			}
			if clean == targetPrefix+"go.mod" {
				hasGoMod = true
			}
		}
	}
	if !hasTargetFile {
		return fmt.Errorf("WBS has no targetFiles under %s", targetPrefix)
	}
	if objectiveLooksLikeGoProject(objective) && !hasGoMod {
		return fmt.Errorf("WBS for new Go project does not create %sgo.mod", targetPrefix)
	}
	return nil
}

func isGenericMetaWBSTitle(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	if lower == "" {
		return true
	}
	metaPhrases := []string{
		"review complete design document",
		"check existing project structure",
		"create development plan",
		"complete development plan",
		"understand requirements",
		"review design",
		"analyse design",
		"analyze design",
		"设计评审",
		"检查现有项目结构",
		"制定开发计划",
	}
	for _, phrase := range metaPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func objectiveLooksLikeGoProject(objective string) bool {
	lower := strings.ToLower(objective)
	return strings.Contains(lower, "golang") || strings.Contains(lower, "go ") ||
		strings.Contains(lower, "go项目") || strings.Contains(lower, "go 项目") ||
		strings.Contains(lower, "go.mod")
}

func synthesizeObjectiveWBS(objective string) []rawTask {
	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" || !objectiveLooksLikeGoProject(objective) {
		return nil
	}
	pkg := "./" + targetRoot + "/..."
	return []rawTask{
		{
			num:            "1",
			title:          "初始化 AgentDB Go 模块与公共 API 骨架",
			role:           "coder",
			accept:         "只创建 go.mod、README、agentdb.go 最小可编译骨架; agentdb.go 仅定义 Config、DB、New、Close、Stats 等不会与后续模块冲突的最小 API; 不要定义 Namespace/Collection/Vector/Graph/Index/Store 类型; 仅使用 Go 标准库; cd " + targetRoot + " && go test ./... 通过",
			priority:       3,
			complexity:     "simple",
			targetFiles:    []string{targetRoot + "/go.mod", targetRoot + "/README.md", targetRoot + "/agentdb.go"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeLeaf,
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-core",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback",
		},
		{
			num:            "2",
			title:          "实现文件对象与普通 KV 存储能力",
			role:           "coder",
			depNums:        []string{"1"},
			accept:         "实现面向 agent 的文件对象、普通 KV 读写、命名空间隔离和基础错误处理; 单元测试使用 testing 标准库覆盖核心路径",
			priority:       3,
			complexity:     "medium",
			targetFiles:    []string{targetRoot + "/store.go", targetRoot + "/store_test.go"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeLeaf,
			estimatedMin:   3,
			riskLevel:      wbsRiskMedium,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-core",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback",
		},
		{
			num:            "3",
			title:          "实现向量存储与相似度检索",
			role:           "coder",
			depNums:        []string{"2"},
			accept:         "实现内存 VectorStore、Add/Get/Delete/SearchTopK; 仅使用标准库和朴素余弦相似度; 单元测试覆盖基础查询",
			priority:       3,
			complexity:     "medium",
			targetFiles:    []string{targetRoot + "/vector.go", targetRoot + "/vector_test.go"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeLeaf,
			estimatedMin:   3,
			riskLevel:      wbsRiskMedium,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-vector",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback",
		},
		{
			num:            "4",
			title:          "实现图谱节点边存储与邻接查询",
			role:           "coder",
			depNums:        []string{"3"},
			accept:         "实现内存 GraphStore、节点/边 CRUD、邻接查询; 仅使用标准库; 单元测试覆盖基础图查询",
			priority:       3,
			complexity:     "medium",
			targetFiles:    []string{targetRoot + "/graph.go", targetRoot + "/graph_test.go"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeLeaf,
			estimatedMin:   3,
			riskLevel:      wbsRiskMedium,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-graph",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback",
		},
		{
			num:            "5",
			title:          "实现倒排索引与关键词检索",
			role:           "coder",
			depNums:        []string{"4"},
			accept:         "实现内存 InvertedIndex、文档索引、AND 查询、删除; 仅使用标准库; 单元测试覆盖基础检索",
			priority:       3,
			complexity:     "medium",
			targetFiles:    []string{targetRoot + "/index.go", targetRoot + "/index_test.go"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeLeaf,
			estimatedMin:   3,
			riskLevel:      wbsRiskMedium,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-index",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback",
		},
		{
			num:            "6",
			title:          "集成 AgentDB 服务门面与端到端测试",
			role:           "coder",
			depNums:        []string{"5"},
			accept:         "修改 agentdb.go 提供统一 AgentDB 门面、使用示例和端到端测试; 测试使用 testing 标准库; cd " + targetRoot + " && go test ./... 通过",
			priority:       3,
			complexity:     "medium",
			targetFiles:    []string{targetRoot + "/agentdb.go", targetRoot + "/agentdb_test.go", targetRoot + "/example_test.go"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeLeaf,
			estimatedMin:   3,
			riskLevel:      wbsRiskMedium,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-integration",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback",
		},
		{
			num:            "7",
			title:          "AgentDB V1 本地验证",
			role:           "tester",
			depNums:        []string{"6"},
			accept:         "本地执行 cd " + targetRoot + " && go test ./...; 不调用 tester LLM",
			priority:       2,
			complexity:     "simple",
			targetFiles:    []string{targetRoot + "/go.mod"},
			targetPackages: []string{pkg},
			taskType:       wbsTaskTypeVerification,
			estimatedMin:   1,
			riskLevel:      wbsRiskLow,
			verifyCommand:  "cd " + targetRoot + " && go test ./...",
			parallelGroup:  targetRoot + "-verification",
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "objective-fallback;auto-verification",
		},
	}
}

// normalizeAndSplitRawTasks 在 DAG 入库前执行 TaskSizingGate:
// 1. 补齐旧 WBS 默认值; 2. Macro 不直接执行; 3. 高风险/超预算 Leaf 展开为可验证 Leaf DAG。
func (o *Orchestrator) normalizeAndSplitRawTasks(rawTasks []rawTask) []rawTask {
	if len(rawTasks) == 0 {
		return nil
	}
	o.wbsSplitCount = 0
	var normalized []rawTask
	rewriteDep := make(map[string]string)

	for _, rt := range rawTasks {
		rt = normalizeRawTaskDefaults(rt)
		shouldSplit, reason := shouldSplitRawTask(rt)
		if shouldSplit {
			children := expandRawTask(rt, reason)
			if len(children) > 0 {
				normalized = append(normalized, children...)
				rewriteDep[rt.num] = children[len(children)-1].num
				o.wbsSplitCount += len(children)
				continue
			}
		}
		if rt.taskType == wbsTaskTypeMacro {
			// 宏任务理论上必须被展开。兜底情况下也转成 Leaf, 避免写入一个不可执行节点后卡住 DAG。
			rt.taskType = wbsTaskTypeLeaf
			rt.splitReason = appendSplitReason(rt.splitReason, "macro-fallback-to-leaf")
		}
		normalized = append(normalized, rt)
	}

	for i := range normalized {
		normalized[i].depNums = rewriteRawTaskDeps(normalized[i].depNums, rewriteDep, normalized[i].num)
	}
	return normalized
}

func normalizeRawTaskDefaults(rt rawTask) rawTask {
	rt.num = strings.TrimSpace(rt.num)
	rt.title = strings.TrimSpace(rt.title)
	rt.role = orchNormalizeRole(rt.role)
	rt.taskType = normalizeWBSTaskType(rt.taskType)
	rt.riskLevel = normalizeWBSRisk(rt.riskLevel)
	rt.blockingPolicy = normalizeWBSBlockingPolicy(rt.blockingPolicy)
	rt.complexity = strings.ToLower(strings.TrimSpace(rt.complexity))
	rt.parentID = strings.TrimSpace(rt.parentID)
	rt.verifyCommand = strings.TrimSpace(rt.verifyCommand)
	rt.parallelGroup = strings.TrimSpace(rt.parallelGroup)
	rt.splitReason = strings.TrimSpace(rt.splitReason)
	if rt.priority == 0 {
		rt.priority = 1
	}
	if rt.estimatedMin <= 0 {
		switch rt.complexity {
		case "simple", "low":
			rt.estimatedMin = 2
		case "complex", "high":
			rt.estimatedMin = 4
		default:
			rt.estimatedMin = 3
		}
	}
	if rt.accept == "" {
		rt.accept = "编译通过 + 目标文件满足任务验收标准"
	}
	return rt
}

func normalizeWBSTaskType(taskType string) string {
	switch strings.ToLower(strings.TrimSpace(taskType)) {
	case wbsTaskTypeMacro, "milestone", "parent":
		return wbsTaskTypeMacro
	case wbsTaskTypeVerification, "verify", "test":
		return wbsTaskTypeVerification
	default:
		return wbsTaskTypeLeaf
	}
}

func normalizeWBSRisk(risk string) string {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case wbsRiskHigh:
		return wbsRiskHigh
	case wbsRiskLow:
		return wbsRiskLow
	default:
		return wbsRiskMedium
	}
}

func normalizeWBSBlockingPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case wbsBlockingFailOpen, "continue", "non_blocking":
		return wbsBlockingFailOpen
	default:
		return wbsBlockingFailBlocks
	}
}

func shouldSplitRawTask(rt rawTask) (bool, string) {
	if rt.taskType == wbsTaskTypeVerification {
		return false, ""
	}
	if rt.parentID != "" || strings.Contains(rt.splitReason, "sizing-gate") {
		return false, ""
	}
	if strings.Contains(rt.splitReason, "objective-fallback") {
		return false, ""
	}
	if rt.taskType == wbsTaskTypeMacro {
		return true, "macro-task-not-executable"
	}
	if rt.estimatedMin > 4 {
		return true, fmt.Sprintf("estimated-%dmin-over-leaf-budget", rt.estimatedMin)
	}
	if rt.riskLevel == wbsRiskHigh {
		return true, "high-risk-leaf-requires-micro-milestones"
	}
	if len(rt.targetFiles) > 3 {
		return true, fmt.Sprintf("target-files-%d-over-leaf-budget", len(rt.targetFiles))
	}
	if isHighRiskTaskText(rt.title + " " + rt.designRef + " " + strings.Join(rt.constraintRefs, " ")) {
		return true, "domain-risk-keyword"
	}
	return false, ""
}

func expandRawTask(rt rawTask, reason string) []rawTask {
	if isMVCCTask(rt) {
		return expandMVCCRawTask(rt, reason)
	}
	return expandGenericRawTask(rt, reason)
}

func expandMVCCRawTask(rt rawTask, reason string) []rawTask {
	steps := []struct {
		title    string
		accept   string
		taskType string
		role     string
		minutes  int
	}{
		{"MVCC 数据结构与事务状态枚举", "只定义 TxnID、Version、ReadView、WriteSet、事务状态枚举; scoped build 通过", wbsTaskTypeLeaf, "coder", 2},
		{"Begin/Commit/Rollback 生命周期骨架", "实现 Begin/Commit/Rollback 状态转换骨架; 覆盖非法状态转换; scoped build 通过", wbsTaskTypeLeaf, "coder", 3},
		{"ReadView 与可见性判断", "实现 snapshot visibility / ReadView 判断; 增加表驱动测试; scoped build/test 通过", wbsTaskTypeLeaf, "coder", 3},
		{"写写冲突与提交校验", "实现 write-write conflict detection 与提交前校验; 增加冲突测试; scoped build/test 通过", wbsTaskTypeLeaf, "coder", 3},
		{"版本链读写与 GC 接口", "实现版本链读写边界与 retention/cleanup 接口; scoped build/test 通过", wbsTaskTypeLeaf, "coder", 3},
		{"MVCC 集成验证", "本地执行并发读写、回滚、可见性测试; 不调用 tester LLM", wbsTaskTypeVerification, "tester", 2},
	}
	return buildExpandedTasks(rt, reason, "mvcc-core", steps)
}

func expandGenericRawTask(rt rawTask, reason string) []rawTask {
	steps := []struct {
		title    string
		accept   string
		taskType string
		role     string
		minutes  int
	}{
		{"接口与数据结构边界", "定义最小接口/数据结构/文件骨架; scoped build 通过", wbsTaskTypeLeaf, "coder", 2},
		{"核心行为实现", "实现单一核心行为路径; scoped build 通过; 不扩展无关文件", wbsTaskTypeLeaf, "coder", 3},
		{"本地验证与回归检查", "本地执行 build/test/TODO scan; 不调用 tester LLM", wbsTaskTypeVerification, "tester", 2},
	}
	return buildExpandedTasks(rt, reason, "sizing-gate", steps)
}

func buildExpandedTasks(rt rawTask, reason, groupSuffix string, steps []struct {
	title    string
	accept   string
	taskType string
	role     string
	minutes  int
}) []rawTask {
	children := make([]rawTask, 0, len(steps))
	parentID := rt.num
	group := rt.parallelGroup
	if group == "" {
		group = parentID + ":" + groupSuffix
	}
	for i, step := range steps {
		child := rt
		child.num = fmt.Sprintf("%s.%d", parentID, i+1)
		child.title = step.title
		if rt.title != "" {
			child.title = rt.title + " - " + step.title
		}
		child.role = orchNormalizeRole(step.role)
		child.taskType = step.taskType
		child.parentID = parentID
		child.estimatedMin = step.minutes
		child.riskLevel = wbsRiskMedium
		child.parallelGroup = group
		child.blockingPolicy = wbsBlockingFailBlocks
		child.splitReason = appendSplitReason(rt.splitReason, "sizing-gate:"+reason)
		child.accept = step.accept
		child.complexity = "medium"
		if step.taskType == wbsTaskTypeVerification {
			child.complexity = "simple"
			child.riskLevel = wbsRiskLow
			child.verifyCommand = defaultVerifyCommand(rt)
		}
		if i == 0 {
			child.depNums = append([]string(nil), rt.depNums...)
		} else {
			child.depNums = []string{children[i-1].num}
		}
		children = append(children, child)
	}
	return children
}

func appendSplitReason(existing, reason string) string {
	existing = strings.TrimSpace(existing)
	reason = strings.TrimSpace(reason)
	if existing == "" {
		return reason
	}
	if reason == "" || strings.Contains(existing, reason) {
		return existing
	}
	return existing + ";" + reason
}

func defaultVerifyCommand(rt rawTask) string {
	if len(rt.targetPackages) > 0 {
		return "go test " + strings.Join(rt.targetPackages, " ")
	}
	return "go test ./..."
}

func rewriteRawTaskDeps(depNums []string, rewrite map[string]string, self string) []string {
	if len(depNums) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(depNums))
	for _, dep := range depNums {
		dep = strings.TrimSpace(dep)
		if repl, ok := rewrite[dep]; ok {
			dep = repl
		}
		if dep == "" || dep == self || seen[dep] {
			continue
		}
		seen[dep] = true
		out = append(out, dep)
	}
	return out
}

func isMVCCTask(rt rawTask) bool {
	text := strings.ToLower(rt.title + " " + rt.designRef + " " + strings.Join(rt.constraintRefs, " "))
	return strings.Contains(text, "mvcc") || strings.Contains(text, "readview") ||
		strings.Contains(text, "事务") || strings.Contains(text, "transaction")
}

func isHighRiskTaskText(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		"mvcc", "transaction", "事务", "lock", "锁", "concurrency", "并发",
		"scheduler", "调度", "index", "索引", "parser", "解析",
		"consensus", "共识", "raft", "cache", "缓存", "gc", "snapshot",
		"readview", "版本链", "visibility", "冲突",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// --- 辅助函数 ---

func parseDepsString(deps string) []string {
	if deps == "" || deps == "-" || deps == "无" || deps == "none" {
		return nil
	}
	var result []string
	for _, d := range strings.Split(deps, ",") {
		d = strings.TrimSpace(d)
		d = strings.TrimPrefix(d, "#")
		d = strings.TrimSpace(d)
		if d != "" && regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(d) {
			result = append(result, d)
		}
	}
	return result
}

func splitTrimNonEmpty(s, sep string) []string {
	if s == "" || s == "-" {
		return nil
	}
	var result []string
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func uniqueStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func replaceString(items []string, oldValue, newValue string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		if item == oldValue {
			item = newValue
		}
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func orchestratorTaskSubject(teamName, title string) string {
	return fmt.Sprintf("%s%s", orchestratorTaskPrefix(teamName), title)
}

func orchestratorTaskPrefix(teamName string) string {
	return fmt.Sprintf("[%s] [orch] ", teamName)
}

// computeDAGWidth Kahn 算法计算拓扑分层的最大宽度 (用于 pool 精确扩缩)。
func (o *Orchestrator) computeDAGWidth(tasks []rawTask) int {
	inDeg := make(map[string]int)
	graph := make(map[string][]string) // num → downstream nums
	for _, t := range tasks {
		inDeg[t.num] = 0
	}
	for _, t := range tasks {
		for _, d := range t.depNums {
			graph[d] = append(graph[d], t.num)
			inDeg[t.num]++
		}
	}

	maxWidth := 0
	processed := 0
	total := len(tasks)
	for processed < total {
		var level []string
		for _, t := range tasks {
			if inDeg[t.num] == 0 {
				level = append(level, t.num)
			}
		}
		if len(level) == 0 {
			break // 循环依赖保护
		}
		if len(level) > maxWidth {
			maxWidth = len(level)
		}
		for _, num := range level {
			inDeg[num] = -1 // 标记已处理
			for _, next := range graph[num] {
				inDeg[next]--
			}
			processed++
		}
	}
	return maxWidth
}

// DAGMaxWidth 返回 DAG 拓扑最大宽度 (供 pool 扩缩参考)
func (o *Orchestrator) DAGMaxWidth() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dagMaxWidth
}

// orchestratorStallTimeout 编排器停滞超时: 连续无进展超过此时间触发恢复。
const orchestratorStallTimeout = 10 * time.Minute

// orchestratorMaxStallRecoveries 最大停滞恢复次数, 超过后退出
const orchestratorMaxStallRecoveries = 5

// taskExecutionTimeout 单个任务执行超时: 防止 goroutine 永久卡在 LLM 重试循环中。
const taskExecutionTimeout = 12 * time.Minute

const (
	coderCallTimeout        = 6 * time.Minute
	reviewerCallTimeout     = 2 * time.Minute
	testerCallTimeout       = 2 * time.Minute
	splitPlannerCallTimeout = 2 * time.Minute
	longRunningLeafBudget   = coderCallTimeout + 30*time.Second
)

// AgentExecutionTimeoutError 标记单次 agent 调用超时。它不是普通瞬态重试信号:
// 对高风险 Leaf 应触发 split-on-timeout, 避免把同一个过粗 prompt 原样重试 5 次。
type AgentExecutionTimeoutError struct {
	Timeout time.Duration
	Cause   error
}

func (e *AgentExecutionTimeoutError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("agent execution timeout after %s: %v", e.Timeout, e.Cause)
}

func (e *AgentExecutionTimeoutError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func isAgentExecutionTimeout(err error) bool {
	var timeoutErr *AgentExecutionTimeoutError
	return errors.As(err, &timeoutErr)
}

func (o *Orchestrator) recordWBSPlanMetrics(team *ProductionTeam) {
	mc := team.metrics()
	if mc == nil {
		return
	}
	baseLabels := map[string]string{"workflow": team.Workflow}
	mc.RecordRun("team", metrics.MWBSSplitCount, float64(o.wbsSplitCount), team.Name, baseLabels)
	mc.RecordRun("team", metrics.MWBSTimeoutSplitCount, float64(o.wbsTimeoutSplitCount), team.Name, baseLabels)

	o.mu.Lock()
	nodes := make([]*TaskNode, 0, len(o.nodes))
	for _, node := range o.nodes {
		nodes = append(nodes, node)
	}
	o.mu.Unlock()

	groupSizes := make(map[string]int)
	for _, node := range nodes {
		labels := node.wbsMetricLabels(team)
		if node.EstimatedMin > 0 {
			mc.RecordRun("team", metrics.MWBSTaskEstimatedMinutes, float64(node.EstimatedMin), team.Name, labels)
		}
		if node.TaskType == wbsTaskTypeLeaf || node.TaskType == wbsTaskTypeVerification {
			mc.RecordRun("team", metrics.MWBSLeafFiles, float64(len(node.TargetFiles)), team.Name, labels)
		}
		if node.ParallelGroup != "" {
			groupSizes[node.ParallelGroup]++
		}
	}
	for group, size := range groupSizes {
		labels := map[string]string{"workflow": team.Workflow, "parallel_group": group}
		mc.RecordRun("team", metrics.MWBSParallelGroupSize, float64(size), team.Name, labels)
	}
}

func (o *Orchestrator) recordWBSTaskDuration(team *ProductionTeam, node *TaskNode, duration time.Duration, status TaskStatus) {
	mc := team.metrics()
	if mc == nil || node == nil {
		return
	}
	labels := node.wbsMetricLabels(team)
	labels["status"] = string(status)
	mc.RecordRun("team", metrics.MWBSTaskActualDurationSec, duration.Seconds(), team.Name, labels)
}

func (o *Orchestrator) recordWBSFailedBlockedDependents(team *ProductionTeam, node *TaskNode, cascaded int) {
	mc := team.metrics()
	if mc == nil || node == nil || cascaded <= 0 {
		return
	}
	labels := node.wbsMetricLabels(team)
	mc.RecordRun("team", metrics.MWBSFailedBlockedDependents, float64(cascaded), team.Name, labels)
}

func (o *Orchestrator) recordWBSMaterialization(team *ProductionTeam, node *TaskNode, written []string, buildCwd string) {
	if team == nil || node == nil {
		return
	}
	mc := team.metrics()
	if mc == nil {
		return
	}
	labels := node.wbsMetricLabels(team)
	mc.RecordRun("team", metrics.MWBSMaterializedFiles, float64(len(written)), team.Name, labels)
	if buildCwd == "" {
		return
	}
	rootLabels := node.wbsMetricLabels(team)
	rootLabels["build_root"] = buildRootMetricLabel(team.Cwd, buildCwd)
	mc.RecordRun("team", metrics.MWBSBuildRootDetected, 1, team.Name, rootLabels)
}

func buildRootMetricLabel(cwd, buildCwd string) string {
	if buildCwd == "" {
		return ""
	}
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, buildCwd); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Base(buildCwd))
}

func (n *TaskNode) wbsMetricLabels(team *ProductionTeam) map[string]string {
	workflow := ""
	if team != nil {
		workflow = team.Workflow
	}
	return map[string]string{
		"workflow":        workflow,
		"role":            n.Role,
		"task_type":       n.TaskType,
		"risk_level":      n.RiskLevel,
		"blocking_policy": n.BlockingPolicy,
	}
}

// Execute 从 V2 TaskStore 的就绪队列循环调度, 直到所有任务完成。
func (o *Orchestrator) Execute(ctx context.Context, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	var resultsMu sync.Mutex
	var allErr error

	if o.totalCount == 0 {
		return nil, nil
	}

	if o.pool != nil && o.dagMaxWidth > 0 {
		o.pool.AutoScale(o.dagMaxWidth)
	}

	// 补齐孤儿节点 (必须在 restoreCompletedTasksFromDAG 之前执行):
	// 将 DAG 中已存在但未被 ParsePlanToDAG 包含的任务加入 o.nodes。
	// 修复: 如果先执行 restore, 孤儿节点不在 o.nodes 中 → 跳过状态恢复 → 永久停滞。
	orphaned := o.populateOrphanNodes(team.Name)
	if orphaned > 0 {
		o.notify(o.chatID, fmt.Sprintf("🔗 补齐 %d 个孤儿任务节点 (DAG 已有但 WBS 未输出)", orphaned))
	}

	// 恢复已完成的检查点: 从 V2 TaskStore 中已有的 completed/failed 任务恢复, 避免重复执行。
	// 注意: 这里从 V2 DAG (o.dag) 读取, 而非仅 o.checkpoints, 因为 DAG 是单一数据源。
	// 关键: populateOrphanNodes 必须先执行, 确保 o.nodes 包含所有 DAG 任务, 这样 in_progress 僵尸任务才能被正确重置。
	restored := o.restoreCompletedTasksFromDAG(team)
	if restored > 0 {
		o.notify(o.chatID, fmt.Sprintf("♻️ 编排器从检查点恢复 %d 个已完成任务 (跳过)", restored))
	}

	o.notify(o.chatID, fmt.Sprintf("🎯 编排器启动: %d 个任务, 最大并发 %d (DAG 宽度: %d, 已恢复: %d, 补齐: %d)",
		o.totalCount, o.config.MaxParallel, o.dagMaxWidth, restored, orphaned))
	logging.Event(ctx, "orchestrator.start", "tasks", o.totalCount, "maxParallel", o.config.MaxParallel, "dagWidth", o.dagMaxWidth, "restored", restored, "orphaned", orphaned)
	o.recordWBSPlanMetrics(team)

	lastProgressAt := time.Now()
	lastCompletedCount := 0
	stallRecoveries := 0

	for {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// 完成检查前置: 所有任务已处理完 → 立即退出
		o.mu.Lock()
		done := o.completedCount+o.failedCount >= o.totalCount
		currentCompleted := o.completedCount + o.failedCount
		o.mu.Unlock()
		if done {
			break
		}

		// 停滞检测 + 恢复 (参考 Temporal heartbeat timeout + K8s liveness probe)
		if currentCompleted > lastCompletedCount {
			lastCompletedCount = currentCompleted
			lastProgressAt = time.Now()
			stallRecoveries = 0
		} else if time.Since(lastProgressAt) > orchestratorStallTimeout {
			stallRecoveries++
			if stallRecoveries > orchestratorMaxStallRecoveries {
				o.notify(o.chatID, fmt.Sprintf(
					"🔴 编排器停滞超时 (%s 无进展, %d 次恢复均失败, %d/%d 完成, %d 失败), 退出",
					orchestratorStallTimeout, stallRecoveries-1, o.completedCount, o.totalCount, o.failedCount))
				allErr = fmt.Errorf("编排器停滞超时: %s 无进展, %d 次恢复均失败, %d/%d 完成, %d 失败",
					orchestratorStallTimeout, stallRecoveries-1, o.completedCount, o.totalCount, o.failedCount)
				break
			}

			recovered := o.attemptStallRecovery(ctx, objective, team)
			if recovered > 0 {
				o.notify(o.chatID, fmt.Sprintf(
					"♻️ 编排器停滞恢复: 重新调度 %d 个卡住任务 (第 %d 次恢复)",
					recovered, stallRecoveries))
				lastProgressAt = time.Now()
			} else {
				o.notify(o.chatID, fmt.Sprintf(
					"⚠️ 编排器停滞 (%s 无进展, 恢复尝试 %d/%d, %d/%d 完成, %d 失败)",
					orchestratorStallTimeout, stallRecoveries, orchestratorMaxStallRecoveries,
					o.completedCount, o.totalCount, o.failedCount))
			}
		}

		readyV2 := o.dag.ReadyTasks()

		// 过滤孤儿任务: 只调度本编排器创建的任务 (防止 V2 TaskStore 中残留旧任务导致死循环)
		var myReady []DAGTaskSummary
		o.mu.Lock()
		for _, t := range readyV2 {
			if _, ok := o.nodes[t.ID]; ok {
				myReady = append(myReady, t)
			}
		}
		o.mu.Unlock()

		if len(myReady) == 0 {
			select {
			case <-time.After(500 * time.Millisecond):
				continue
			case <-ctx.Done():
				return allResults, ctx.Err()
			}
		}

		// 流式调度: 用 semaphore 限制并发, 任务完成立即触发下一轮就绪检查。
		// 修复: 之前 wg.Wait() 整批等待 → 同批最慢任务拖住所有后续任务。
		// 现在: 每个任务完成后发信号, 主循环立即检查新就绪任务。
		batch := myReady
		if len(batch) > o.config.MaxParallel {
			batch = batch[:o.config.MaxParallel]
		}

		doneCh := make(chan struct{}, len(batch))
		for _, task := range batch {
			o.mu.Lock()
			node := o.nodes[task.ID]
			o.mu.Unlock()

			_ = o.dag.SetTaskStatus(task.ID, "in_progress")

			go func(n *TaskNode, taskID string) {
				// 关键修复: panic 恢复 + 保证 doneCh 始终发送信号。
				// 之前: goroutine panic → doneCh 不发送 → 主循环永久阻塞 → 团队停滞。
				// 现在: defer recover 捕获 panic, 确保 doneCh 一定被发送, 主循环不会卡死。
				defer func() {
					if r := recover(); r != nil {
						o.notify(o.chatID, fmt.Sprintf("🔴 %s goroutine panic: %v, 标记为 failed", n.Title, r))
						o.dag.SetTaskStatusAndUnblock(taskID, "failed")
						o.mu.Lock()
						o.failedCount++
						o.mu.Unlock()
					}
					doneCh <- struct{}{}
				}()

				// 修复: 为每个任务添加独立超时, 防止 goroutine 永久卡在 LLM 重试循环中。
				// 之前: 使用父 ctx (无超时) → LLM 429 重试无限循环 → 任务永久 in_progress。
				// 现在: 每个任务独立超时 30min → 超时后自动标记 failed, 不影响其他任务。
				taskCtx, cancel := context.WithTimeout(ctx, taskExecutionTimeout)
				defer cancel()
				sr := o.executeTaskNode(taskCtx, n, objective, team)
				resultsMu.Lock()
				allResults = append(allResults, sr)
				resultsMu.Unlock()

				// 增量刷新: 每个任务完成后立即刷新
				if o.flusher != nil {
					resultsMu.Lock()
					snapshot := make([]StageResult, len(allResults))
					copy(snapshot, allResults)
					resultsMu.Unlock()
					o.flusher(snapshot)
				}
			}(node, task.ID)
		}

		// 等待本批所有任务完成 (不再阻塞主循环, 因为 unblock 在 goroutine 内发生)
		for i := 0; i < len(batch); i++ {
			select {
			case <-doneCh:
			case <-ctx.Done():
				return allResults, ctx.Err()
			}
		}
	}

	// DAG 残留任务清理: 报告被遗弃的任务
	o.cleanupResidualTasks()

	o.notify(o.chatID, fmt.Sprintf("🏁 编排完成: %d/%d 成功, %d 失败",
		o.completedCount, o.totalCount, o.failedCount))
	return allResults, allErr
}

// populateOrphanNodes 将 DAG 中存在但未加入 o.nodes 的任务（孤儿）补齐。
// 修复 resume 场景: 当 Planner 的 WBS 遗漏已有任务时，AddTaskWithDeps 不会匹配到它，
// 导致该任务不在 o.nodes 中 → restoreCompletedTasksFromDAG 跳过它 → 主循环孤儿过滤器拦截它 → 永久停滞。
func (o *Orchestrator) populateOrphanNodes(teamName string) int {
	if o.dag == nil {
		return 0
	}
	prefix := orchestratorTaskPrefix(teamName)
	allTasks := o.dag.GetAllTasks()
	o.mu.Lock()
	defer o.mu.Unlock()

	orphaned := 0
	for _, t := range allTasks {
		if _, ok := o.nodes[t.ID]; ok {
			continue // 已有对应节点，跳过
		}
		// 只处理属于当前 team 的任务
		if !strings.HasPrefix(t.Subject, prefix) {
			continue
		}
		title := strings.TrimPrefix(t.Subject, prefix)
		o.nodes[t.ID] = &TaskNode{
			V2TaskID:   t.ID,
			Title:      title,
			Role:       t.Owner,
			MaxRetries: o.config.MaxRetries,
		}
		o.totalCount++
		orphaned++
	}
	return orphaned
}

// restoreCompletedTasksFromDAG 从 V2 DAG 恢复已完成/失败任务计数, 并重置卡住的 in_progress 任务。
// 关键: 如果不更新这些计数, 完成检查 (completedCount+failedCount >= totalCount) 永远不满足,
// 导致 orchestrator 认为所有任务都未处理, 全部重新调度。
// 修复: 对于 in_progress 但没有对应活跃执行的任务, 重置为 pending 让主循环重新调度。
func (o *Orchestrator) restoreCompletedTasksFromDAG(team *ProductionTeam) int {
	if o.dag == nil {
		return 0
	}

	allTasks := o.dag.GetAllTasks()
	o.mu.Lock()
	nodeIDs := make(map[string]bool)
	for _, n := range o.nodes {
		nodeIDs[n.V2TaskID] = true
	}

	restored := 0
	var inProgressIDs []string
	for _, t := range allTasks {
		if !nodeIDs[t.ID] {
			continue
		}
		switch t.Status {
		case "completed":
			o.completedCount++
			restored++
		case "failed":
			o.failedCount++
			restored++
		case "in_progress":
			// in_progress 任务可能是: (a) 真正在执行中 (b) 进程崩溃/重启后遗留的僵尸状态
			// 由于 orchestrator 刚启动, 没有任何活跃执行 → 这些是僵尸任务, 重置为 pending
			inProgressIDs = append(inProgressIDs, t.ID)
		}
	}
	o.mu.Unlock()

	// 重置僵尸 in_progress 任务为 pending
	if len(inProgressIDs) > 0 {
		logging.Event(context.Background(), "orchestrator.restore.inprogress", "count", len(inProgressIDs), "team", team.Name)
		for _, id := range inProgressIDs {
			if err := o.dag.SetTaskStatus(id, "pending"); err != nil {
				logging.Event(context.Background(), "orchestrator.restore.error", "task", id, "error", err.Error())
			}
		}
	}

	return restored
}

func (o *Orchestrator) attemptStallRecovery(ctx context.Context, objective string, team *ProductionTeam) int {
	o.mu.Lock()
	doneIDs := make(map[string]bool)
	for id, node := range o.nodes {
		if o.checkpoints != nil {
			// 修复: checkpoint key 是 node.Title (而非 V2TaskID)
			if cp := o.checkpoints.GetCheckpoint(node.Title); cp != nil && cp.Status == "completed" {
				doneIDs[id] = true
			}
		}
	}

	// 收集: in_progress 卡住的 + failed(瞬态) 可恢复的
	var stuckIDs []string
	var transientFailedIDs []string
	for id, node := range o.nodes {
		if doneIDs[id] {
			continue
		}
		// 通过 node.Error 判断是否瞬态失败
		if node.Error != "" && isTransientError(node.Error) {
			transientFailedIDs = append(transientFailedIDs, id)
		} else if !doneIDs[id] {
			stuckIDs = append(stuckIDs, id)
		}
	}
	o.mu.Unlock()

	recovered := 0

	// 恢复瞬态失败的任务: 重置为 pending, 清零重试计数器
	for _, id := range transientFailedIDs {
		err := o.dag.SetTaskStatus(id, "pending")
		if err == nil {
			recovered++
			o.mu.Lock()
			if n, ok := o.nodes[id]; ok {
				n.Retries = 0 // 瞬态恢复: 重置重试计数, 给予完整重试配额
				n.Error = "stall recovery: 瞬态错误恢复, 重新调度"
			}
			o.failedCount-- // 从 failedCount 中减回
			o.mu.Unlock()
		}
	}

	// 恢复卡住的任务 (in_progress 超时等)
	for _, id := range stuckIDs {
		err := o.dag.SetTaskStatus(id, "pending")
		if err == nil {
			recovered++
			o.mu.Lock()
			if n, ok := o.nodes[id]; ok {
				n.Retries++
				n.Error = "stall recovery: 因停滞被重置"
			}
			o.mu.Unlock()
		}
	}
	return recovered
}

// cleanupResidualTasks 编排结束后清理 DAG 中未完成的残留任务。
// 修复: 不仅清理 pending 任务, 还通过 GetAllTasks 清理 in_progress 的僵尸任务。
func (o *Orchestrator) cleanupResidualTasks() {
	o.mu.Lock()
	completed := o.completedCount
	failed := o.failedCount
	total := o.totalCount
	o.mu.Unlock()

	residualCount := total - completed - failed
	if residualCount <= 0 {
		return
	}

	// 1. 通过 ReadyTasks 找到仍可调度的 pending 残留任务, 标记为 failed
	ready := o.dag.ReadyTasks()
	var residualIDs []string
	o.mu.Lock()
	for _, t := range ready {
		if _, ok := o.nodes[t.ID]; ok {
			residualIDs = append(residualIDs, t.ID)
		}
	}
	o.mu.Unlock()

	for _, id := range residualIDs {
		o.dag.SetTaskStatusAndUnblock(id, "failed")
	}

	// 2. 通过 GetAllTasks 找到 in_progress 的残留任务 (进程退出后遗留的僵尸状态)
	if o.dag != nil {
		allTasks := o.dag.GetAllTasks()
		o.mu.Lock()
		var zombieIDs []string
		for _, t := range allTasks {
			if t.Status == "in_progress" {
				if _, ok := o.nodes[t.ID]; ok {
					zombieIDs = append(zombieIDs, t.ID)
				}
			}
		}
		o.mu.Unlock()

		for _, id := range zombieIDs {
			o.dag.SetTaskStatusAndUnblock(id, "failed")
		}

		if len(zombieIDs) > 0 {
			o.notify(o.chatID, fmt.Sprintf("⚠️ 清理 %d 个 in_progress 僵尸任务 (已标记为 failed)", len(zombieIDs)))
		}
	}

	if residualCount > 0 {
		o.notify(o.chatID, fmt.Sprintf("⚠️ 清理 %d 个残留任务 (已标记 %d 个为 failed)", residualCount, len(residualIDs)))
	}
}

func (o *Orchestrator) roundBudgetFor(node *TaskNode) int {
	cfg := o.config.AdversarialRound
	if cfg <= 0 {
		cfg = 2
	}
	if cfg > 3 {
		cfg = 3
	}

	switch strings.ToLower(strings.TrimSpace(node.Complexity)) {
	case "simple", "low":
		return 1
	case "medium", "moderate":
		return min(cfg, 2)
	case "complex", "high":
		return min(cfg, 3)
	}

	// Planner 偶尔漏填 complexity。按文件范围和标题保守推断, 避免 CLI/配置类小任务默认跑满多轮。
	title := strings.ToLower(node.Title)
	switch {
	case len(node.TargetFiles) > 0 && len(node.TargetFiles) <= 2:
		return 1
	case strings.Contains(title, "cli") || strings.Contains(title, "命令") ||
		strings.Contains(title, "flag") || strings.Contains(title, "配置") ||
		strings.Contains(title, "入口") || strings.Contains(title, "格式化"):
		return 1
	case len(node.TargetFiles) > 0 && len(node.TargetFiles) <= 4:
		return min(cfg, 2)
	default:
		return min(cfg, 2)
	}
}

func (o *Orchestrator) shouldFastPassSimpleTask(node *TaskNode, maxRounds int, buildPassed bool, team *ProductionTeam) bool {
	if !buildPassed || maxRounds > 1 || team == nil || team.Cwd == "" {
		return false
	}
	complexity := strings.ToLower(strings.TrimSpace(node.Complexity))
	if complexity == "simple" || complexity == "low" {
		return true
	}
	return len(node.TargetFiles) > 0 && len(node.TargetFiles) <= 2
}

func (o *Orchestrator) shouldRunLocalVerification(node *TaskNode) bool {
	if node.TaskType == wbsTaskTypeVerification {
		return true
	}
	role := strings.ToLower(node.Role)
	title := strings.ToLower(node.Title)
	acceptance := strings.ToLower(node.AcceptCriteria)
	if strings.Contains(role, "tester") {
		return true
	}
	return (strings.Contains(title, "测试") || strings.Contains(title, "验证") ||
		strings.Contains(title, "test") || strings.Contains(title, "build")) &&
		(strings.Contains(acceptance, "go test") || strings.Contains(acceptance, "go build") ||
			strings.Contains(acceptance, "编译") || strings.Contains(acceptance, "测试"))
}

func (o *Orchestrator) executeLocalVerificationTask(node *TaskNode, team *ProductionTeam, start time.Time) StageResult {
	lang := "go"
	if team != nil && team.Language != "" {
		lang = team.Language
	}
	var checks []string
	var failures []string
	if team == nil || team.Cwd == "" {
		failures = append(failures, "工作目录为空, 无法执行本地验证")
	} else {
		buildCwd := inferBuildCwdFromTaskScope(team.Cwd, node.TargetFiles, node.TargetPackages)
		targetPackages := adjustTargetPackagesForBuildRoot(team.Cwd, buildCwd, node.TargetPackages)
		if errText := runBuildCheckScoped(buildCwd, lang, targetPackages); errText != "" {
			failures = append(failures, errText)
		} else {
			checks = append(checks, "scoped build passed")
		}
		if errText := runTestCheckLang(buildCwd, lang); errText != "" {
			failures = append(failures, errText)
		} else {
			checks = append(checks, "test command passed or not configured")
		}
	}
	duration := time.Since(start)
	output := "local verification: " + strings.Join(checks, "; ")
	if len(failures) > 0 {
		output = "local verification failed:\n" + strings.Join(failures, "\n\n")
		node.Error = output
		cascaded, _ := o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount += 1 + cascaded
		o.mu.Unlock()
		o.recordWBSTaskDuration(team, node, duration, TaskFailed)
		o.recordWBSFailedBlockedDependents(team, node, cascaded)
		o.notify(o.chatID, fmt.Sprintf("🔴 %s 本地验证失败 (%s)", node.Title, duration.Round(time.Second)))
		return StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: output, Output: output, StartedAt: start, Duration: duration.Round(time.Second).String()}
	}

	node.Output = output
	node.TestResult = output
	node.TestPassed = true
	_, _ = o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "completed")
	o.mu.Lock()
	o.completedCount++
	o.mu.Unlock()
	if o.checkpoints != nil {
		o.checkpoints.SaveCheckpoint(node.Title, "completed", 0, output)
	}
	o.recordWBSTaskDuration(team, node, duration, TaskCompleted)
	if team != nil && team.Blackboard != nil {
		team.Blackboard.Write(node.Title+"-result", output, node.Role, "result")
	}
	o.touchActivity()
	o.reportProgress("local-verification", 1, int64(len(output)), node.V2TaskID)
	o.notify(o.chatID, fmt.Sprintf("✅ %s 本地验证完成 (%s) ✅build/test通过", node.Title, duration.Round(time.Second)))
	return StageResult{Name: node.Title, Role: node.Role, Status: TaskCompleted, Output: output, StartedAt: start, Duration: duration.Round(time.Second).String()}
}

func (o *Orchestrator) handleTaskTimeout(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, start time.Time, err error) StageResult {
	duration := time.Since(start)
	errText := "split-on-timeout: " + err.Error()
	node.Error = errText
	node.SplitReason = appendSplitReason(node.SplitReason, "timeout")

	children, splitSource, splitErr := o.addTimeoutSplitChildren(ctx, node, objective, team, err)
	if team != nil {
		if mc := team.metrics(); mc != nil {
			labels := node.wbsMetricLabels(team)
			labels["split_source"] = splitSource
			mc.RecordRun("team", metrics.MWBSTimeoutSplitCount, 1, team.Name, labels)
		}
	}
	if splitErr == nil && children > 0 {
		node.TaskType = wbsTaskTypeMacro
		node.Output = fmt.Sprintf("split-on-timeout: generated %d child leaf tasks via %s after %s", children, splitSource, duration.Round(time.Second))
		_, _ = o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "completed")
		o.mu.Lock()
		o.completedCount++
		o.wbsTimeoutSplitCount++
		o.mu.Unlock()
		if o.checkpoints != nil {
			o.checkpoints.SaveCheckpoint(node.Title, "completed", node.Retries, node.Output)
		}
		o.recordWBSTaskDuration(team, node, duration, TaskCompleted)
		o.notify(o.chatID, fmt.Sprintf("⏱️ %s 超过 agent %s 预算, 已通过 %s 在线拆分为 %d 个 Leaf, 不再原样重试", node.Title, coderCallTimeout, splitSource, children))
		return StageResult{Name: node.Title, Role: node.Role, Status: TaskCompleted, Output: node.Output, StartedAt: start, Duration: duration.Round(time.Second).String()}
	}

	errText += "\n动态拆分失败: " + fmt.Sprint(splitErr)
	cascaded, _ := o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
	o.mu.Lock()
	o.failedCount += 1 + cascaded
	o.wbsTimeoutSplitCount++
	o.mu.Unlock()
	if o.checkpoints != nil {
		o.checkpoints.SaveCheckpoint(node.Title, "failed", node.Retries, errText)
	}
	o.recordWBSTaskDuration(team, node, duration, TaskFailed)
	o.recordWBSFailedBlockedDependents(team, node, cascaded)
	o.notify(o.chatID, fmt.Sprintf("⏱️ %s 超过 agent %s 预算, split-on-timeout 动态拆分失败, 标记 failed", node.Title, coderCallTimeout))
	return StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: errText, StartedAt: start, Duration: duration.Round(time.Second).String()}
}

func (o *Orchestrator) shouldSplitLongRunningLeaf(node *TaskNode, start time.Time) bool {
	if node == nil || time.Since(start) < longRunningLeafBudget {
		return false
	}
	taskType := strings.ToLower(strings.TrimSpace(node.TaskType))
	if taskType == wbsTaskTypeMacro || taskType == wbsTaskTypeVerification {
		return false
	}
	if strings.Contains(node.SplitReason, "long-running") {
		return false
	}
	return true
}

func (o *Orchestrator) handleLongRunningLeaf(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, start time.Time) StageResult {
	node.SplitReason = appendSplitReason(node.SplitReason, "long-running")
	return o.handleTaskTimeout(ctx, node, objective, team, start, &AgentExecutionTimeoutError{
		Timeout: longRunningLeafBudget,
		Cause:   fmt.Errorf("long-running leaf exceeded %s without hard gate pass", longRunningLeafBudget),
	})
}

func (o *Orchestrator) addTimeoutSplitChildren(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, timeoutErr error) (int, string, error) {
	if node == nil || o.dag == nil {
		return 0, "", fmt.Errorf("missing node or DAG")
	}
	teamName := o.teamName
	if team != nil && team.Name != "" {
		teamName = team.Name
	}
	if teamName == "" {
		teamName = "team"
	}

	all := o.dag.GetAllTasks()
	var downstream []DAGTaskSummary
	for _, task := range all {
		if task.ID == node.V2TaskID {
			continue
		}
		if containsString(task.DependsOn, node.V2TaskID) {
			downstream = append(downstream, task)
		}
	}

	children, splitSource, planErr := o.planTimeoutSplitWithLLM(ctx, node, objective, team, timeoutErr)
	if planErr != nil || len(children) == 0 {
		rt := rawTask{
			num:            node.V2TaskID,
			title:          node.Title,
			role:           node.Role,
			designRef:      node.DesignRef,
			constraintRefs: node.ConstraintRefs,
			accept:         node.AcceptCriteria,
			priority:       1,
			complexity:     "complex",
			subGoals:       node.SubGoals,
			targetPackages: node.TargetPackages,
			targetFiles:    node.TargetFiles,
			taskType:       wbsTaskTypeMacro,
			estimatedMin:   max(node.EstimatedMin, 6),
			riskLevel:      wbsRiskHigh,
			parallelGroup:  node.ParallelGroup,
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    appendSplitReason(node.SplitReason, "timeout"),
		}
		children = expandRawTask(rt, "timeout")
		splitSource = "deterministic-fallback"
		if len(children) == 0 {
			return 0, splitSource, fmt.Errorf("timeout split produced no children; llm_error=%v", planErr)
		}
	}

	children = o.prepareTimeoutSplitChildren(children, node)
	childNumToV2ID := make(map[string]string)
	groupLastV2ID := make(map[string]string)
	fileLastV2ID := make(map[string]string)
	var lastChildV2ID string
	childNodes := make([]*TaskNode, 0, len(children))
	for _, child := range children {
		depV2IDs := make([]string, 0, len(child.depNums)+1)
		if len(child.depNums) == 0 {
			depV2IDs = append(depV2IDs, node.V2TaskID)
		}
		for _, dep := range child.depNums {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			switch {
			case dep == node.V2TaskID || dep == node.ParentID:
				depV2IDs = append(depV2IDs, node.V2TaskID)
			case childNumToV2ID[dep] != "":
				depV2IDs = append(depV2IDs, childNumToV2ID[dep])
			default:
				return 0, splitSource, fmt.Errorf("timeout split child %s depends on unknown child %s", child.num, dep)
			}
		}
		if len(depV2IDs) == 0 {
			depV2IDs = append(depV2IDs, node.V2TaskID)
		}
		if child.parallelGroup != "" {
			if prev, ok := groupLastV2ID[child.parallelGroup]; ok && !containsString(depV2IDs, prev) {
				depV2IDs = append(depV2IDs, prev)
			}
		}
		for _, file := range child.targetFiles {
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			if prev, ok := fileLastV2ID[file]; ok && !containsString(depV2IDs, prev) {
				depV2IDs = append(depV2IDs, prev)
			}
		}
		depV2IDs = uniqueStrings(depV2IDs)

		subject := orchestratorTaskSubject(teamName, child.title)
		v2ID, err := o.dag.AddTaskWithDeps(subject, child.accept, child.role, depV2IDs, child.priority)
		if err != nil {
			return 0, splitSource, fmt.Errorf("create timeout split child: %w", err)
		}
		childNode := &TaskNode{
			V2TaskID:       v2ID,
			Title:          child.title,
			Role:           child.role,
			DesignRef:      child.designRef,
			ConstraintRefs: child.constraintRefs,
			AcceptCriteria: child.accept,
			MaxRetries:     o.config.MaxRetries,
			Complexity:     child.complexity,
			SubGoals:       child.subGoals,
			TargetPackages: child.targetPackages,
			TargetFiles:    child.targetFiles,
			TaskType:       child.taskType,
			ParentID:       node.V2TaskID,
			EstimatedMin:   child.estimatedMin,
			RiskLevel:      child.riskLevel,
			VerifyCommand:  child.verifyCommand,
			ParallelGroup:  child.parallelGroup,
			BlockingPolicy: child.blockingPolicy,
			SplitReason:    appendSplitReason(child.splitReason, "source:"+splitSource),
		}
		childNodes = append(childNodes, childNode)
		childNumToV2ID[child.num] = v2ID
		if child.parallelGroup != "" {
			groupLastV2ID[child.parallelGroup] = v2ID
		}
		for _, file := range child.targetFiles {
			file = strings.TrimSpace(file)
			if file != "" {
				fileLastV2ID[file] = v2ID
			}
		}
		lastChildV2ID = v2ID
	}

	if lastChildV2ID == "" {
		return 0, splitSource, fmt.Errorf("timeout split created no terminal child")
	}

	for _, task := range downstream {
		newDeps := replaceString(task.DependsOn, node.V2TaskID, lastChildV2ID)
		if _, err := o.dag.AddTaskWithDeps(task.Subject, task.Description, task.Owner, newDeps, task.Priority); err != nil {
			return 0, splitSource, fmt.Errorf("rewire downstream %s: %w", task.ID, err)
		}
	}

	o.mu.Lock()
	for _, childNode := range childNodes {
		o.nodes[childNode.V2TaskID] = childNode
	}
	o.totalCount += len(childNodes)
	o.wbsSplitCount += len(childNodes)
	o.dagMaxWidth = max(1, o.dagMaxWidth)
	o.mu.Unlock()
	if team != nil {
		if mc := team.metrics(); mc != nil {
			for _, childNode := range childNodes {
				labels := childNode.wbsMetricLabels(team)
				labels["split_source"] = splitSource
				mc.RecordRun("team", metrics.MWBSTaskEstimatedMinutes, float64(childNode.EstimatedMin), team.Name, labels)
				mc.RecordRun("team", metrics.MWBSLeafFiles, float64(len(childNode.TargetFiles)), team.Name, labels)
			}
			labels := node.wbsMetricLabels(team)
			labels["split_source"] = splitSource
			mc.RecordRun("team", metrics.MWBSSplitCount, float64(len(childNodes)), team.Name, labels)
		}
	}
	return len(childNodes), splitSource, nil
}

func (o *Orchestrator) planTimeoutSplitWithLLM(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, timeoutErr error) ([]rawTask, string, error) {
	if o.factory == nil {
		return nil, "", fmt.Errorf("split planner factory is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runner, err := o.factory(ctx, "planner", "")
	if err != nil {
		return nil, "", fmt.Errorf("create split planner: %w", err)
	}
	prompt := o.buildTimeoutSplitPlannerPrompt(node, objective, team, timeoutErr)
	result, err := executeRunnerBounded(ctx, runner, prompt, splitPlannerCallTimeout)
	if err != nil {
		return nil, "", fmt.Errorf("execute split planner: %w", err)
	}
	tasks := parseWBSFromJSON(result)
	if len(tasks) == 0 {
		return nil, "", fmt.Errorf("split planner returned no parseable tasks")
	}
	tasks, err = sanitizeLLMTimeoutSplitTasks(tasks, node)
	if err != nil {
		return nil, "", err
	}
	return tasks, "llm-split-planner", nil
}

func (o *Orchestrator) buildTimeoutSplitPlannerPrompt(node *TaskNode, objective string, team *ProductionTeam, timeoutErr error) string {
	var b strings.Builder
	b.WriteString("你是 WBS Split Planner。某个 coder Leaf 超过 6 分钟预算, 禁止原样重试。请基于 timeout summary 重新拆成更小的 Leaf DAG。\n\n")
	b.WriteString("## 项目目标\n" + objective + "\n\n")
	b.WriteString("## 超时任务\n")
	b.WriteString(fmt.Sprintf("- title: %s\n- role: %s\n- taskType: %s\n- estimatedMinutes: %d\n- riskLevel: %s\n- error: %s\n",
		node.Title, node.Role, node.TaskType, node.EstimatedMin, node.RiskLevel, timeoutErr))
	if node.DesignRef != "" {
		b.WriteString("- designRef: " + node.DesignRef + "\n")
	}
	if node.AcceptCriteria != "" {
		b.WriteString("- acceptance: " + node.AcceptCriteria + "\n")
	}
	if len(node.ConstraintRefs) > 0 {
		b.WriteString("- constraints: " + strings.Join(node.ConstraintRefs, ", ") + "\n")
	}
	if len(node.TargetFiles) > 0 {
		b.WriteString("- originalTargetFiles: " + strings.Join(node.TargetFiles, ", ") + "\n")
	}
	if len(node.TargetPackages) > 0 {
		b.WriteString("- originalTargetPackages: " + strings.Join(node.TargetPackages, ", ") + "\n")
	}
	if node.TestResult != "" {
		b.WriteString("\n## 最近验证结果\n" + truncateResult(node.TestResult, 2000) + "\n")
	}
	if node.Output != "" {
		b.WriteString("\n## 已有部分输出摘要\n" + truncateResult(node.Output, 3000) + "\n")
	}
	if api := currentGoAPISummaryForTask(team, node, objective, 7000); api != "" {
		b.WriteString("\n## 当前真实文件 API 摘要 (来自磁盘)\n")
		b.WriteString(api)
		b.WriteString("\n要求: split child 必须继承并对齐这些真实签名, 不要把上游错误代码里的重复定义/漂移接口继续拆下去。\n")
	}

	b.WriteString(`
## 输出要求
只输出严格 JSON, 不要解释:
` + "```" + `json
{
  "tasks": [
    {
      "id": "s1",
      "title": "更小的单一职责任务",
      "role": "coder",
      "taskType": "leaf",
      "parentId": "` + node.V2TaskID + `",
      "dependsOn": [],
      "designRef": "必要接口/约束摘要",
      "constraints": [],
      "acceptance": "scoped build/test 通过的具体标准",
      "priority": 2,
      "complexity": "medium",
      "estimatedMinutes": 2,
      "riskLevel": "medium",
      "verifyCommand": "go test ./...",
      "parallelGroup": "",
      "blockingPolicy": "fail_blocks_dependents",
      "splitReason": "llm-timeout-split",
      "targetFiles": [],
      "targetPackages": []
    }
  ]
}
` + "```" + `

规则:
1. 输出 2-8 个 task, 最多 12 个; 禁止输出 macro。
2. 每个 leaf 预算 2-4 分钟, estimatedMinutes 不得超过 4。
3. 每个 leaf 默认 1-3 个 targetFiles; 如果无法精确到文件, 继承原任务文件, 但标题必须进一步收窄。
4. 最后必须有 taskType="verification" 的本地验证任务, role="tester", 只跑 build/test/TODO scan。
5. MVCC/事务/锁/并发/索引/调度/缓存一致性等共享核心状态默认串行, 使用 dependsOn 或相同 parallelGroup 表达。
6. 只有无共享文件、无共享核心状态、无依赖边的 leaf 才允许并发。
7. dependsOn 只能引用本次输出中更早的 id。
8. blockingPolicy 默认 fail_blocks_dependents。
9. designRef/acceptance 必须说明要复用的真实 API 名称, 避免重复定义已有符号。
`)
	return b.String()
}

func sanitizeLLMTimeoutSplitTasks(tasks []rawTask, parent *TaskNode) ([]rawTask, error) {
	if parent == nil {
		return nil, fmt.Errorf("missing timeout parent")
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("empty split planner tasks")
	}
	if len(tasks) > 12 {
		tasks = tasks[:12]
	}

	parentRaw := rawTask{
		num:            parent.V2TaskID,
		title:          parent.Title,
		role:           parent.Role,
		designRef:      parent.DesignRef,
		constraintRefs: parent.ConstraintRefs,
		accept:         parent.AcceptCriteria,
		priority:       1,
		complexity:     "complex",
		subGoals:       parent.SubGoals,
		targetPackages: parent.TargetPackages,
		targetFiles:    parent.TargetFiles,
		taskType:       wbsTaskTypeMacro,
		estimatedMin:   max(parent.EstimatedMin, 6),
		riskLevel:      wbsRiskHigh,
		parallelGroup:  parent.ParallelGroup,
		blockingPolicy: wbsBlockingFailBlocks,
		splitReason:    appendSplitReason(parent.SplitReason, "timeout"),
	}

	seenIDs := make(map[string]bool)
	previousIDs := make(map[string]bool)
	cleaned := make([]rawTask, 0, len(tasks)+1)
	for i, task := range tasks {
		task = normalizeRawTaskDefaults(task)
		if task.num == "" {
			task.num = fmt.Sprintf("llm-%d", i+1)
		}
		if seenIDs[task.num] {
			task.num = fmt.Sprintf("%s-%d", task.num, i+1)
		}
		for _, dep := range task.depNums {
			dep = strings.TrimSpace(dep)
			if dep == "" || dep == parent.V2TaskID || dep == parent.ParentID {
				continue
			}
			if !previousIDs[dep] {
				return nil, fmt.Errorf("split planner task %s depends on non-earlier task %s", task.num, dep)
			}
		}
		if task.title == "" {
			return nil, fmt.Errorf("split planner task %s has empty title", task.num)
		}
		if task.taskType == wbsTaskTypeMacro {
			return nil, fmt.Errorf("split planner returned macro task %s", task.num)
		}
		if task.taskType == wbsTaskTypeVerification {
			task.role = "tester"
			task.complexity = "simple"
			task.riskLevel = wbsRiskLow
		}
		if task.taskType == wbsTaskTypeLeaf {
			task.role = orchNormalizeRole(task.role)
			if task.role == "tester" {
				task.role = "coder"
			}
		}
		task.parentID = parent.V2TaskID
		if task.estimatedMin <= 0 {
			task.estimatedMin = 3
		}
		if task.estimatedMin > 4 {
			task.estimatedMin = 4
			task.splitReason = appendSplitReason(task.splitReason, "llm-estimate-clamped")
		}
		if len(task.targetFiles) == 0 {
			task.targetFiles = append([]string(nil), parent.TargetFiles...)
		}
		if len(task.targetPackages) == 0 {
			task.targetPackages = append([]string(nil), parent.TargetPackages...)
		}
		if task.verifyCommand == "" {
			task.verifyCommand = defaultVerifyCommand(parentRaw)
		}
		if task.parallelGroup == "" {
			task.parallelGroup = parent.ParallelGroup
		}
		task.blockingPolicy = wbsBlockingFailBlocks
		task.splitReason = appendSplitReason(task.splitReason, "llm-timeout-split")
		cleaned = append(cleaned, task)
		seenIDs[task.num] = true
		previousIDs[task.num] = true
	}

	childIDs := make(map[string]bool)
	for _, task := range cleaned {
		childIDs[task.num] = true
	}
	for _, task := range cleaned {
		for _, dep := range task.depNums {
			dep = strings.TrimSpace(dep)
			if dep == "" || dep == parent.V2TaskID || dep == parent.ParentID {
				continue
			}
			if !childIDs[dep] {
				return nil, fmt.Errorf("split planner task %s depends on unknown task %s", task.num, dep)
			}
		}
	}

	if !hasVerificationTask(cleaned) {
		verificationDeps := terminalRawTaskIDs(cleaned)
		cleaned = append(cleaned, rawTask{
			num:            "llm-verification",
			title:          parent.Title + " - 本地验证与回归检查",
			role:           "tester",
			depNums:        verificationDeps,
			designRef:      parent.DesignRef,
			constraintRefs: parent.ConstraintRefs,
			accept:         "本地执行 build/test/TODO scan, 不调用 tester LLM",
			priority:       1,
			complexity:     "simple",
			targetPackages: append([]string(nil), parent.TargetPackages...),
			targetFiles:    append([]string(nil), parent.TargetFiles...),
			taskType:       wbsTaskTypeVerification,
			parentID:       parent.V2TaskID,
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			verifyCommand:  defaultVerifyCommand(parentRaw),
			parallelGroup:  parent.ParallelGroup,
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "llm-timeout-split;auto-verification",
		})
	} else {
		fillVerificationDeps(cleaned)
	}
	return cleaned, nil
}

func (o *Orchestrator) prepareTimeoutSplitChildren(children []rawTask, parent *TaskNode) []rawTask {
	if parent == nil {
		return children
	}
	parentRaw := rawTask{
		num:            parent.V2TaskID,
		title:          parent.Title,
		role:           parent.Role,
		designRef:      parent.DesignRef,
		constraintRefs: parent.ConstraintRefs,
		accept:         parent.AcceptCriteria,
		targetPackages: parent.TargetPackages,
		targetFiles:    parent.TargetFiles,
		parallelGroup:  parent.ParallelGroup,
	}
	for i := range children {
		child := normalizeRawTaskDefaults(children[i])
		if child.num == "" {
			child.num = fmt.Sprintf("split-%d", i+1)
		}
		if child.parentID == "" {
			child.parentID = parent.V2TaskID
		}
		if child.taskType == wbsTaskTypeMacro {
			child.taskType = wbsTaskTypeLeaf
			child.splitReason = appendSplitReason(child.splitReason, "timeout-macro-coerced")
		}
		if child.taskType == wbsTaskTypeVerification {
			child.role = "tester"
			child.complexity = "simple"
			child.riskLevel = wbsRiskLow
		} else if child.role == "tester" {
			child.role = "coder"
		}
		if child.estimatedMin <= 0 {
			child.estimatedMin = 3
		}
		if child.estimatedMin > 4 {
			child.estimatedMin = 4
			child.splitReason = appendSplitReason(child.splitReason, "timeout-estimate-clamped")
		}
		if len(child.targetFiles) == 0 {
			child.targetFiles = append([]string(nil), parent.TargetFiles...)
		}
		if len(child.targetPackages) == 0 {
			child.targetPackages = append([]string(nil), parent.TargetPackages...)
		}
		if child.verifyCommand == "" {
			child.verifyCommand = defaultVerifyCommand(parentRaw)
		}
		if child.parallelGroup == "" {
			child.parallelGroup = parent.ParallelGroup
		}
		child.blockingPolicy = wbsBlockingFailBlocks
		children[i] = child
	}
	if !hasVerificationTask(children) {
		children = append(children, rawTask{
			num:            "timeout-verification",
			title:          parent.Title + " - 本地验证与回归检查",
			role:           "tester",
			depNums:        terminalRawTaskIDs(children),
			designRef:      parent.DesignRef,
			constraintRefs: parent.ConstraintRefs,
			accept:         "本地执行 build/test/TODO scan, 不调用 tester LLM",
			priority:       1,
			complexity:     "simple",
			targetPackages: append([]string(nil), parent.TargetPackages...),
			targetFiles:    append([]string(nil), parent.TargetFiles...),
			taskType:       wbsTaskTypeVerification,
			parentID:       parent.V2TaskID,
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			verifyCommand:  defaultVerifyCommand(parentRaw),
			parallelGroup:  parent.ParallelGroup,
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    "timeout-split;auto-verification",
		})
	} else {
		fillVerificationDeps(children)
	}
	return children
}

func hasVerificationTask(tasks []rawTask) bool {
	for _, task := range tasks {
		if task.taskType == wbsTaskTypeVerification {
			return true
		}
	}
	return false
}

func terminalRawTaskIDs(tasks []rawTask) []string {
	ids := make(map[string]bool)
	hasDownstream := make(map[string]bool)
	for _, task := range tasks {
		if task.num != "" {
			ids[task.num] = true
		}
	}
	for _, task := range tasks {
		for _, dep := range task.depNums {
			dep = strings.TrimSpace(dep)
			if ids[dep] {
				hasDownstream[dep] = true
			}
		}
	}
	var terminals []string
	for _, task := range tasks {
		if task.num == "" || task.taskType == wbsTaskTypeVerification {
			continue
		}
		if !hasDownstream[task.num] {
			terminals = append(terminals, task.num)
		}
	}
	if len(terminals) > 0 {
		return terminals
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].num != "" {
			return []string{tasks[i].num}
		}
	}
	return nil
}

func fillVerificationDeps(tasks []rawTask) {
	terminals := terminalRawTaskIDs(tasks)
	for i := range tasks {
		if tasks[i].taskType != wbsTaskTypeVerification || len(tasks[i].depNums) > 0 {
			continue
		}
		if len(terminals) > 0 {
			tasks[i].depNums = append([]string(nil), terminals...)
		} else if i > 0 {
			tasks[i].depNums = []string{tasks[i-1].num}
		}
	}
}

func (o *Orchestrator) tryDeterministicContractPatch(team *ProductionTeam, objective string, node *TaskNode, lang string, written []string, buildCwd string, targetPackages []string) (string, []string, string, []string, bool, bool) {
	if team == nil || team.Cwd == "" || node == nil || lang != "go" {
		return "", written, buildCwd, targetPackages, false, false
	}
	targetRoot := inferObjectiveTargetRoot(objective)
	if !strings.EqualFold(targetRoot, "agentDBV1") {
		return "", written, buildCwd, targetPackages, false, false
	}
	patchWritten, err := writeAgentDBV1ContractPatch(team.Cwd, targetRoot, node.TargetFiles)
	if err != nil || len(patchWritten) == 0 {
		return fmt.Sprintf("deterministic contract patch failed: %v", err), written, buildCwd, targetPackages, false, false
	}
	written = uniqueStrings(append(written, patchWritten...))
	if inferred := inferTaskBuildCwd(team.Cwd, patchWritten); inferred != "" {
		buildCwd = inferred
		targetPackages = adjustTargetPackagesForBuildRoot(team.Cwd, buildCwd, node.TargetPackages)
	}
	if buildCwd == "" {
		buildCwd = filepath.Join(team.Cwd, targetRoot)
	}
	o.recordWBSMaterialization(team, node, patchWritten, buildCwd)
	o.notify(o.chatID, fmt.Sprintf("🧩 %s deterministic contract patch: 写入 %d 个 AgentDBV1 合约文件", node.Title, len(patchWritten)))

	buildErrors := missingTargetFilesError(team.Cwd, node.TargetFiles)
	if buildErrors == "" {
		buildErrors = runBuildCheckScoped(buildCwd, lang, targetPackages)
	}
	localTestsPassed := false
	if buildErrors == "" && shouldRunLocalTestsForWritten(patchWritten) {
		buildErrors = runTestCheckLang(buildCwd, lang)
		localTestsPassed = buildErrors == ""
	}
	return buildErrors, written, buildCwd, targetPackages, true, localTestsPassed
}

func writeAgentDBV1ContractPatch(cwd, targetRoot string, targetFiles []string) ([]string, error) {
	if cwd == "" || targetRoot == "" {
		return nil, fmt.Errorf("missing cwd or target root")
	}
	root := filepath.Join(cwd, targetRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	files := agentDBV1ContractFiles()
	var written []string
	for rel, data := range files {
		if !strings.HasPrefix(rel, targetRoot+"/") {
			continue
		}
		localRel := strings.TrimPrefix(rel, targetRoot+"/")
		path := filepath.Join(root, filepath.FromSlash(localRel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			return written, err
		}
		written = append(written, rel)
	}
	sort.Strings(written)
	return written, nil
}

func agentDBV1ContractFiles() map[string]string {
	prefix := "agentDBV1/"
	return map[string]string{
		prefix + "go.mod":          agentDBV1ContractGoMod,
		prefix + "README.md":       agentDBV1ContractReadme,
		prefix + "agentdb.go":      agentDBV1ContractCore,
		prefix + "store.go":        agentDBV1ContractStore,
		prefix + "store_test.go":   agentDBV1ContractStoreTest,
		prefix + "vector.go":       agentDBV1ContractVector,
		prefix + "vector_test.go":  agentDBV1ContractVectorTest,
		prefix + "graph.go":        agentDBV1ContractGraph,
		prefix + "graph_test.go":   agentDBV1ContractGraphTest,
		prefix + "index.go":        agentDBV1ContractIndex,
		prefix + "index_test.go":   agentDBV1ContractIndexTest,
		prefix + "agentdb_test.go": agentDBV1ContractIntegrationTest,
		prefix + "example_test.go": agentDBV1ContractExampleTest,
	}
}

const agentDBV1ContractGoMod = `module agentdbv1

go 1.21
`

const agentDBV1ContractReadme = `# AgentDB V1

AgentDB V1 is a stdlib-only Go storage facade for agent workloads. It offers key/value memory, file payloads, exact vector search, graph edges, and an inverted text index behind a small in-process API.
`

const agentDBV1ContractCore = `package agentdb

import "sync"

type DB struct {
	mu      sync.RWMutex
	kv      map[string][]byte
	files   map[string]FileObject
	vectors map[string]Vector
	nodes   map[string]GraphNode
	edges   []GraphEdge
	index   map[string]map[string]struct{}
}

type Stats struct {
	Keys    int
	Files   int
	Vectors int
	Nodes   int
	Edges   int
	Terms   int
}

func New() *DB {
	return &DB{
		kv:      make(map[string][]byte),
		files:   make(map[string]FileObject),
		vectors: make(map[string]Vector),
		nodes:   make(map[string]GraphNode),
		index:   make(map[string]map[string]struct{}),
	}
}

func (db *DB) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return Stats{
		Keys:    len(db.kv),
		Files:   len(db.files),
		Vectors: len(db.vectors),
		Nodes:   len(db.nodes),
		Edges:   len(db.edges),
		Terms:   len(db.index),
	}
}
`

const agentDBV1ContractStore = `package agentdb

type FileObject struct {
	Name     string
	MIMEType string
	Data     []byte
}

func (db *DB) Put(key string, value []byte) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.kv[key] = cloneBytes(value)
}

func (db *DB) Get(key string) ([]byte, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	value, ok := db.kv[key]
	return cloneBytes(value), ok
}

func (db *DB) PutFile(id string, file FileObject) {
	db.mu.Lock()
	defer db.mu.Unlock()
	file.Data = cloneBytes(file.Data)
	db.files[id] = file
}

func (db *DB) GetFile(id string) (FileObject, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	file, ok := db.files[id]
	file.Data = cloneBytes(file.Data)
	return file, ok
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
`

const agentDBV1ContractStoreTest = `package agentdb

import "testing"

func TestKVAndFileCopies(t *testing.T) {
	db := New()
	value := []byte("agent memory")
	db.Put("memory/session-1", value)
	value[0] = 'x'
	got, ok := db.Get("memory/session-1")
	if !ok || string(got) != "agent memory" {
		t.Fatalf("Get() = %q, %v", string(got), ok)
	}
	got[0] = 'x'
	again, _ := db.Get("memory/session-1")
	if string(again) != "agent memory" {
		t.Fatalf("Get returned mutable backing slice")
	}
	db.PutFile("file:plan", FileObject{Name: "plan.md", MIMEType: "text/markdown", Data: []byte("# Plan")})
	file, ok := db.GetFile("file:plan")
	if !ok || file.Name != "plan.md" || string(file.Data) != "# Plan" {
		t.Fatalf("GetFile() = %+v, %v", file, ok)
	}
}
`

const agentDBV1ContractVector = `package agentdb

import (
	"errors"
	"math"
	"sort"
)

type Vector struct {
	ID       string
	Values   []float64
	Metadata map[string]string
}

type SearchResult struct {
	ID    string
	Score float64
}

func (db *DB) AddVector(v Vector) error {
	if v.ID == "" || len(v.Values) == 0 {
		return errors.New("agentdb: vector requires id and values")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	v.Values = cloneFloat64s(v.Values)
	v.Metadata = cloneStringMap(v.Metadata)
	db.vectors[v.ID] = v
	return nil
}

func (db *DB) SearchVector(query []float64, topK int) []SearchResult {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if topK <= 0 || len(query) == 0 {
		return nil
	}
	results := make([]SearchResult, 0, len(db.vectors))
	for _, v := range db.vectors {
		if len(v.Values) == len(query) {
			results = append(results, SearchResult{ID: v.ID, Score: cosine(query, v.Values)})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].ID < results[j].ID
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > topK {
		results = results[:topK]
	}
	return results
}

func cosine(a, b []float64) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func cloneFloat64s(in []float64) []float64 {
	if in == nil {
		return nil
	}
	out := make([]float64, len(in))
	copy(out, in)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
`

const agentDBV1ContractVectorTest = `package agentdb

import "testing"

func TestVectorSearch(t *testing.T) {
	db := New()
	if err := db.AddVector(Vector{ID: "doc:agents", Values: []float64{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddVector(Vector{ID: "doc:storage", Values: []float64{0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	results := db.SearchVector([]float64{0.9, 0.1, 0}, 1)
	if len(results) != 1 || results[0].ID != "doc:agents" {
		t.Fatalf("SearchVector() = %+v", results)
	}
}
`

const agentDBV1ContractGraph = `package agentdb

import "errors"

type GraphNode struct {
	ID       string
	Kind     string
	Metadata map[string]string
}

type GraphEdge struct {
	From string
	To   string
	Kind string
}

func (db *DB) AddNode(node GraphNode) error {
	if node.ID == "" {
		return errors.New("agentdb: graph node requires id")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	node.Metadata = cloneStringMap(node.Metadata)
	db.nodes[node.ID] = node
	return nil
}

func (db *DB) AddEdge(edge GraphEdge) error {
	if edge.From == "" || edge.To == "" {
		return errors.New("agentdb: graph edge requires from and to")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.edges = append(db.edges, edge)
	return nil
}

func (db *DB) Neighbors(id string) []GraphNode {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []GraphNode
	for _, edge := range db.edges {
		if edge.From != id {
			continue
		}
		if node, ok := db.nodes[edge.To]; ok {
			node.Metadata = cloneStringMap(node.Metadata)
			out = append(out, node)
		}
	}
	return out
}
`

const agentDBV1ContractGraphTest = `package agentdb

import "testing"

func TestGraphNeighbors(t *testing.T) {
	db := New()
	if err := db.AddNode(GraphNode{ID: "agent", Kind: "actor"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddNode(GraphNode{ID: "memory", Kind: "resource"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddEdge(GraphEdge{From: "agent", To: "memory", Kind: "uses"}); err != nil {
		t.Fatal(err)
	}
	neighbors := db.Neighbors("agent")
	if len(neighbors) != 1 || neighbors[0].ID != "memory" {
		t.Fatalf("Neighbors() = %+v", neighbors)
	}
}
`

const agentDBV1ContractIndex = `package agentdb

import (
	"sort"
	"strings"
)

func (db *DB) IndexDoc(id, text string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, term := range tokenize(text) {
		if db.index[term] == nil {
			db.index[term] = make(map[string]struct{})
		}
		db.index[term][id] = struct{}{}
	}
}

func (db *DB) SearchTerms(query string) []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil
	}
	var ids map[string]struct{}
	for i, term := range terms {
		postings := db.index[term]
		if len(postings) == 0 {
			return nil
		}
		if i == 0 {
			ids = cloneSet(postings)
			continue
		}
		for id := range ids {
			if _, ok := postings[id]; !ok {
				delete(ids, id)
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	out := fields[:0]
	for _, field := range fields {
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}
`

const agentDBV1ContractIndexTest = `package agentdb

import "testing"

func TestInvertedIndexANDQuery(t *testing.T) {
	db := New()
	db.IndexDoc("doc1", "agent vector graph storage")
	db.IndexDoc("doc2", "agent file storage")
	db.IndexDoc("doc3", "vector only")
	results := db.SearchTerms("agent storage")
	if len(results) != 2 || results[0] != "doc1" || results[1] != "doc2" {
		t.Fatalf("SearchTerms() = %+v", results)
	}
}
`

const agentDBV1ContractIntegrationTest = `package agentdb

import "testing"

func TestAgentDBIntegration(t *testing.T) {
	db := New()
	db.Put("session", []byte("memory"))
	db.PutFile("file", FileObject{Name: "note.txt", Data: []byte("agent note")})
	if err := db.AddVector(Vector{ID: "memory", Values: []float64{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddNode(GraphNode{ID: "agent"}); err != nil {
		t.Fatal(err)
	}
	db.IndexDoc("doc", "agent memory")
	stats := db.Stats()
	if stats.Keys != 1 || stats.Files != 1 || stats.Vectors != 1 || stats.Nodes != 1 || stats.Terms != 2 {
		t.Fatalf("Stats() = %+v", stats)
	}
}
`

const agentDBV1ContractExampleTest = `package agentdb_test

import (
	"fmt"

	"agentdbv1"
)

func ExampleDB() {
	db := agentdb.New()
	db.Put("memory", []byte("agent context"))
	value, _ := db.Get("memory")
	fmt.Println(string(value))
	// Output: agent context
}
`

// executeTaskNode 执行单个任务, 内置 mini 对抗循环:
//
//	每轮: coder 执行 → reviewer 审查 (SkepticalReviewerPersona + ParseEvalScoreJSON)
//	      → tester micro-test → AdaptiveTerminator 决定继续/停止
//
// 完全复用 adversarial.go 已有基础设施, 不重复实现。
func (o *Orchestrator) executeTaskNode(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam) StageResult {
	start := time.Now()
	o.touchActivity()

	o.notify(o.chatID, fmt.Sprintf("▶️ %s (%s) 执行中...", node.Title, node.Role))

	if o.shouldRunLocalVerification(node) {
		return o.executeLocalVerificationTask(node, team, start)
	}

	if !o.config.MicroTestAfter {
		return o.executeTaskOnce(ctx, node, objective, team, start)
	}

	// L1: 自适应任务粒度 — 以 token/time 预算为先, 避免简单任务陷入多轮评审黑洞。
	maxRounds := o.roundBudgetFor(node)
	minRounds := 2
	if maxRounds < minRounds {
		minRounds = maxRounds
	}
	terminator := NewAdaptiveTerminator(minRounds, maxRounds)

	var lastOutput string
	var lastFeedback string
	var lastScore EvalScore
	var lastBuildPassed bool
	var iterMemory []IterationMemory
	bottleneckCounts := make(map[string]int)

	for round := 1; round <= terminator.MaxRounds; round++ {
		localTestsPassed := false
		// L6 重采样: 连续低质量时清空上轮输出，重新开始
		if round > 2 && terminator.ShouldResample() {
			o.notify(o.chatID, fmt.Sprintf("♻️ %s 触发重采样 (连续低完整度), 清空上轮输出重新生成", node.Title))
			lastOutput = ""
			lastFeedback = "⚠️ 重采样模式: 上一轮实现严重不完整, 请完全重新开始, 优先保证核心功能完整输出"
		}

		// === Step 1: Coder 生成/修复 ===
		prompt := o.buildTaskPrompt(node, objective, team)
		if round > 1 && lastFeedback != "" {
			memorySection := FormatMemoryChain(iterMemory)
			maxCtx := 12000
			if round == 3 {
				maxCtx = 6000
			} else if round >= 4 {
				maxCtx = 3000
			}
			// L7: 差分修复 prompt — 明确要求只修改被指出的问题, 不动其余代码
			prompt = fmt.Sprintf("%s\n\n%s\n### ⚠️ 第 %d 轮差分修复 (仅修改被指出的问题, 保留其余代码不变):\n"+
				"**规则**: 1) 只修改 reviewer 指出的 MUST-FIX 函数 2) 其余代码原封不动输出 3) 不要重构未提及的模块\n\n%s\n\n### 上轮产出 (基线代码, 仅在标记处修改):\n%s",
				prompt, memorySection, round, truncateResult(lastFeedback, 2000), truncateResult(lastOutput, maxCtx))
		}

		runner, err := o.factory(ctx, node.Role, "")
		if err != nil {
			return o.handleTaskFailure(ctx, node, objective, team,
				StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: err.Error(),
					StartedAt: start, Duration: time.Since(start).String()})
		}
		result, err := executeRunnerBounded(ctx, runner, prompt, coderCallTimeout)
		if err != nil {
			if isAgentExecutionTimeout(err) {
				return o.handleTaskTimeout(ctx, node, objective, team, start, err)
			}
			return o.handleTaskFailure(ctx, node, objective, team,
				StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: err.Error(),
					StartedAt: start, Duration: time.Since(start).String()})
		}
		if reason := validateAgentOutput(result, node.Role); reason != "" {
			return o.handleTaskFailure(ctx, node, objective, team,
				StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed,
					Error: "产出验证失败: " + reason, Output: result,
					StartedAt: start, Duration: time.Since(start).String()})
		}

		lastOutput = result
		node.Output = result
		o.reportProgress("LLM生成", round, int64(len(result)), node.V2TaskID)

		// L4: 文件物化 — 提取代码块写入磁盘
		lang := team.Language
		if lang == "" {
			lang = "go"
		}
		buildCwd := team.Cwd
		targetPackages := append([]string(nil), node.TargetPackages...)
		var written []string
		if team.Cwd != "" {
			written = MaterializeCode(team.Cwd, result, lang)
			written = enforceTargetFileScope(team.Cwd, written, node.TargetFiles)
			if len(written) > 0 {
				o.notify(o.chatID, fmt.Sprintf("📁 %s 文件物化: %d 个文件", node.Title, len(written)))
				if inferred := inferTaskBuildCwd(team.Cwd, written); inferred != "" {
					buildCwd = inferred
					targetPackages = adjustTargetPackagesForBuildRoot(team.Cwd, buildCwd, node.TargetPackages)
				}
			}
		}
		o.recordWBSMaterialization(team, node, written, buildCwd)

		// === Step 1.5: L2 编译硬门禁 (按任务目标包隔离编译, 避免跨任务污染) ===
		buildPassed := true
		o.reportProgress("编译", round, 0, node.V2TaskID)
		if buildCwd != "" {
			buildErrors := materializationGateErrorForTask(result, written, lang, node)
			if buildErrors == "" {
				buildErrors = missingTargetFilesError(team.Cwd, node.TargetFiles)
			}
			if buildErrors == "" {
				buildErrors = runBuildCheckScoped(buildCwd, lang, targetPackages)
			}
			if buildErrors == "" && shouldRunLocalTestsForWritten(written) {
				buildErrors = runTestCheckLang(buildCwd, lang)
				localTestsPassed = buildErrors == ""
			}
			if buildErrors != "" {
				var patched bool
				var patchTestsPassed bool
				var patchErrors string
				patchErrors, written, buildCwd, targetPackages, patched, patchTestsPassed = o.tryDeterministicContractPatch(team, objective, node, lang, written, buildCwd, targetPackages)
				if patched {
					buildErrors = patchErrors
					localTestsPassed = localTestsPassed || patchTestsPassed
					if buildErrors == "" {
						buildPassed = true
						o.notify(o.chatID, fmt.Sprintf("🟢 %s deterministic contract patch 后编译通过", node.Title))
					}
				}
			}
			if buildErrors != "" {
				buildPassed = false
				o.notify(o.chatID, fmt.Sprintf("🔴 %s 第 %d 轮编译失败, 启动内部修复...", node.Title, round))
				for retry := 1; retry <= 2; retry++ {
					fixPrompt := fmt.Sprintf("%s\n\n### 编译错误 (第 %d 次修复, 仅修复编译问题):\n%s\n\n上轮代码:\n%s",
						o.buildTaskPrompt(node, objective, team), retry, truncateResult(buildErrors, 3000), truncateResult(lastOutput, 10000))
					fixRunner, fixErr := o.factory(ctx, node.Role, "")
					if fixErr != nil {
						break
					}
					fixResult, fixErr := executeRunnerBounded(ctx, fixRunner, fixPrompt, coderCallTimeout)
					if fixErr != nil {
						break
					}
					lastOutput = fixResult
					node.Output = fixResult
					written = MaterializeCode(team.Cwd, fixResult, lang)
					written = enforceTargetFileScope(team.Cwd, written, node.TargetFiles)
					if len(written) > 0 {
						o.notify(o.chatID, fmt.Sprintf("📁 %s 修复物化: %d 个文件", node.Title, len(written)))
						if inferred := inferTaskBuildCwd(team.Cwd, written); inferred != "" {
							buildCwd = inferred
							targetPackages = adjustTargetPackagesForBuildRoot(team.Cwd, buildCwd, node.TargetPackages)
						}
					}
					o.recordWBSMaterialization(team, node, written, buildCwd)
					buildErrors = materializationGateErrorForTask(fixResult, written, lang, node)
					if buildErrors == "" {
						buildErrors = missingTargetFilesError(team.Cwd, node.TargetFiles)
					}
					if buildErrors == "" {
						buildErrors = runBuildCheckScoped(buildCwd, lang, targetPackages)
					}
					if buildErrors == "" && shouldRunLocalTestsForWritten(written) {
						buildErrors = runTestCheckLang(buildCwd, lang)
						localTestsPassed = buildErrors == ""
					}
					if buildErrors != "" {
						var patched bool
						var patchTestsPassed bool
						var patchErrors string
						patchErrors, written, buildCwd, targetPackages, patched, patchTestsPassed = o.tryDeterministicContractPatch(team, objective, node, lang, written, buildCwd, targetPackages)
						if patched {
							buildErrors = patchErrors
							localTestsPassed = localTestsPassed || patchTestsPassed
						}
					}
					if buildErrors == "" {
						buildPassed = true
						o.notify(o.chatID, fmt.Sprintf("  🟢 %s 编译修复成功 (重试 %d)", node.Title, retry))
						break
					}
				}
				if !buildPassed {
					o.notify(o.chatID, fmt.Sprintf("  🔴 %s 编译修复失败, 跳过 reviewer, 直接记低分", node.Title))
					score := EvalScore{Correctness: 3, Completeness: 2, Security: 5, CodeQuality: 3, DesignAlignment: 2,
						Feedback: "编译未通过，无法评审代码质量"}
					lastScore = score
					terminator.RecordBuildResult(false)
					terminator.RecordRoundOutput(round, score, lastOutput)
					iterMemory = append(iterMemory, IterationMemory{
						Round: round, Approach: "编译失败", Score: score,
						KeyIssues: []string{"编译未通过"}, TestPass: false, Kept: false,
					})
					decision := terminator.ShouldTerminate(round, score)
					if decision.ShouldStop {
						break
					}
					if o.shouldSplitLongRunningLeaf(node, start) {
						return o.handleLongRunningLeaf(ctx, node, objective, team, start)
					}
					lastFeedback = "编译失败，必须优先修复编译错误后再考虑功能"
					continue
				}
			}
			if buildPassed {
				lastBuildPassed = true
				terminator.RecordBuildResult(true)
				o.notify(o.chatID, fmt.Sprintf("🟢 %s 第 %d 轮编译通过", node.Title, round))
			}
		}

		if o.shouldFastPassSimpleTask(node, maxRounds, buildPassed, team) {
			score := EvalScore{
				Correctness:     8,
				Completeness:    7,
				Security:        7,
				CodeQuality:     7,
				DesignAlignment: 7,
				Feedback:        "simple task fast-pass: scoped build passed; skipped reviewer/tester LLM to preserve team token budget",
				Pass:            true,
			}
			lastScore = score
			node.TestPassed = true
			node.TestResult = "fast-pass: scoped build passed"
			terminator.RecordTestResult(true)
			terminator.RecordRoundOutput(round, score, lastOutput)
			o.reportProgress("fast-pass", round, int64(len(lastOutput)), node.V2TaskID)
			o.touchActivity()
			o.notify(o.chatID, fmt.Sprintf("⚡ %s simple fast-pass: 编译通过, 跳过 reviewer/tester LLM", node.Title))
			if team.Blackboard != nil {
				fullScore := "正确=8 完整=7 安全=7 质量=7 通过:true test:true fast-pass"
				team.Blackboard.Write(fmt.Sprintf("%s-eval-round%d", node.V2TaskID, round),
					fullScore, "evaluator", "score")
				team.Blackboard.Write(fmt.Sprintf("eval-task-%s-round%d-score", node.Title, round),
					fullScore, "evaluator", "score")
			}
			break
		}

		// === Step 2: Reviewer 审查 (复用 SkepticalReviewerPersona + ParseEvalScoreJSON) ===
		// 根因修复: 将编译状态注入 reviewer prompt, 避免编译通过仍给 0 分的问题。
		score := o.runSkepticalReview(ctx, node, objective, lastScore, buildPassed)
		lastScore = score
		o.reportProgress("评审", round, 0, node.V2TaskID)

		// === Step 3: Tester micro-test + 瓶颈分类 (参考 GLM 5.1) ===
		o.runMicroTest(ctx, node)
		if localTestsPassed && !node.TestPassed {
			node.TestPassed = true
			node.TestResult = "local go test passed; LLM micro-test advisory was overridden:\n" + node.TestResult
		}
		bottlenecks := ClassifyBottlenecks(node.TestResult)
		if len(bottlenecks) > 0 {
			for _, bn := range bottlenecks {
				bnKey := bn.Type
				// L9: 编译已通过时覆盖"编译"类瓶颈 (消除 micro-test LLM 误判)
				if bnKey == "compilation" && buildPassed {
					continue
				}
				prevCount := bottleneckCounts[bnKey]
				bottleneckCounts[bnKey] = prevCount + 1
				if bottleneckCounts[bnKey] >= 2 {
					o.notify(o.chatID, fmt.Sprintf("🔴 %s 重复瓶颈: %s (连续 %d 轮)",
						node.Title, bn.Detail, bottleneckCounts[bnKey]))
				}
			}
		}

		// 评分日志
		scoreMsg := fmt.Sprintf("正确=%.0f 完整=%.0f 安全=%.0f 质量=%.0f",
			score.Correctness, score.Completeness, score.Security, score.CodeQuality)
		if score.DesignAlignment > 0 {
			scoreMsg += fmt.Sprintf(" 对齐=%.0f", score.DesignAlignment)
		}
		testLabel := map[bool]string{true: "✅", false: "⚠️"}[node.TestPassed]
		o.touchActivity()
		o.notify(o.chatID, fmt.Sprintf("📊 %s 第 %d 轮: %s | micro-test: %s",
			node.Title, round, scoreMsg, testLabel))

		if team.Blackboard != nil {
			fullScore := scoreMsg + fmt.Sprintf(" 通过:%v test:%v", score.MeetsHardPassThreshold(), node.TestPassed)
			// 旧键 (向后兼容)
			team.Blackboard.Write(fmt.Sprintf("%s-eval-round%d", node.V2TaskID, round),
				fullScore, "evaluator", "score")
			// 新键 (dashboard 可识别的 eval-*-score 格式)
			team.Blackboard.Write(fmt.Sprintf("eval-task-%s-round%d-score", node.Title, round),
				fullScore, "evaluator", "score")
		}

		// === Step 3.5: 即时 Keep/Revert 决策 (参考 MiniMax M2.7) ===
		terminator.RecordRoundOutput(round, score, lastOutput)
		revertOccurred := false
		if revert, bestOut, bestR := terminator.ShouldRevert(score); revert && round > 1 {
			o.notify(o.chatID, fmt.Sprintf("⏪ %s 第 %d 轮退化, revert 到第 %d 轮最佳版本 (继续迭代)",
				node.Title, round, bestR))
			lastOutput = bestOut
			node.Output = lastOutput
			revertOccurred = true
		}

		// === Step 4: AdaptiveTerminator 决定继续/停止 ===
		decision := terminator.ShouldTerminate(round, score)

		// 阶梯式策略转换 (参考 GLM 5.1): converged 时按瓶颈类型注入针对性策略
		if decision.StrategyShift {
			o.notify(o.chatID, fmt.Sprintf("🔀 %s 改进饱和, 触发策略转换 (第 %d 次, 最多 %d 次)",
				node.Title, terminator.StrategyShiftCount, terminator.MaxStrategyShifts))
			// 根据瓶颈类型生成针对性策略转换 prompt (精准复刻 GLM 5.1 benchmark-driven)
			shiftAdvice := "换一种完全不同的实现思路"
			for bnType, count := range bottleneckCounts {
				if count >= 2 {
					switch bnType {
					case "compilation":
						shiftAdvice = "编译持续失败: 简化实现, 减少依赖, 分步构建确保每步可编译"
					case "design_drift":
						shiftAdvice = "设计偏差持续: 重新阅读设计文档, 先对齐接口签名再实现逻辑"
					case "constraint":
						shiftAdvice = "约束违反持续: 列出所有约束, 逐条检查当前实现是否满足"
					case "logic":
						shiftAdvice = "逻辑错误持续: 增加单元测试驱动开发, 先写测试再写实现"
					}
					break
				}
			}
			lastFeedback = fmt.Sprintf("⚠️ **策略转换要求** (第 %d 次):\n"+
				"当前修补方式已饱和, **必须**:\n%s\n\n之前的反馈:\n%s",
				terminator.StrategyShiftCount, shiftAdvice, lastFeedback)
			// 不 break, 继续下一轮
		} else if decision.ShouldStop {
			if decision.BestOutput != "" {
				lastOutput = decision.BestOutput
				node.Output = lastOutput
				o.notify(o.chatID, fmt.Sprintf("⏪ %s best-of-N 回滚到第 %d 轮 (最高分)", node.Title, decision.BestRound))
			}
			reasonCN := map[string]string{
				"quality_pass": "质量达标", "max_rounds": "达到轮数上限",
				"degradation": "连续退化", "converged": "改进已饱和",
			}[decision.Reason]
			if reasonCN == "" {
				reasonCN = decision.Reason
			}
			o.notify(o.chatID, fmt.Sprintf("🏁 %s 对抗终止: %s (第 %d 轮)", node.Title, reasonCN, round))
			o.touchActivity()
			break
		}

		// L10: Reviewer MUST-FIX 上限递减 (减少反馈漂移)
		// R2: max 3 issues, R3: max 2, R4+: max 1
		maxIssues := 5
		switch {
		case round >= 4:
			maxIssues = 1
		case round == 3:
			maxIssues = 2
		case round == 2:
			maxIssues = 3
		}
		feedbackText := score.Feedback
		if feedbackText != "" {
			issues := ExtractKeyIssues(feedbackText)
			if len(issues) > maxIssues {
				issues = issues[:maxIssues]
				feedbackText = fmt.Sprintf("⚠️ 仅列出最关键的 %d 个问题 (聚焦修复, 勿过度重构):\n", maxIssues)
				for _, iss := range issues {
					feedbackText += "- " + iss + "\n"
				}
			}
		}

		// L7: R1 保底+差分修复 — feedback 强调"增量修改, 保留已有好的部分"
		var parts []string
		if feedbackText != "" {
			parts = append(parts, "### Reviewer 审查 (EvalScore):\n"+feedbackText)
		}
		if !node.TestPassed && node.TestResult != "" {
			parts = append(parts, "### Tester Micro-Test:\n"+node.TestResult)
		}
		lastFeedback = strings.Join(parts, "\n\n")
		if lastFeedback == "" {
			lastFeedback = "上一轮未通过硬门槛，请全面改进。"
		}

		// 记录结构化短期记忆 (参考 MiniMax M2.7, 精准复刻: Approach 赋值)
		kept := !(revertOccurred)
		approach := truncateResult(lastOutput, 200)
		if idx := strings.Index(approach, "\n"); idx > 0 && idx < 150 {
			approach = approach[:idx]
		}
		iterMemory = append(iterMemory, IterationMemory{
			Round: round, Approach: approach, Score: score,
			KeyIssues: ExtractKeyIssues(score.Feedback),
			TestPass:  node.TestPassed, Kept: kept,
		})

		if o.shouldSplitLongRunningLeaf(node, start) && !score.MeetsHardPassThreshold() {
			return o.handleLongRunningLeaf(ctx, node, objective, team, start)
		}

		if round+1 <= terminator.MaxRounds {
			o.notify(o.chatID, fmt.Sprintf("🔄 %s 继续对抗 (第 %d/%d 轮)...",
				node.Title, round+1, terminator.MaxRounds))
		}
	}

	duration := time.Since(start)

	passLabel := "⚠️未达标"
	taskPassed := lastScore.MeetsHardPassThreshold() && node.TestPassed
	incrementalAccepted := false
	if !taskPassed && shouldAcceptIncrementalLeaf(node, lastScore, node.TestPassed, lastBuildPassed) {
		taskPassed = true
		incrementalAccepted = true
		passLabel = "✅增量Leaf本地通过"
	}
	if incrementalAccepted {
		passLabel = "✅增量Leaf本地通过"
	} else if taskPassed {
		passLabel = "✅全通过"
	} else if lastScore.MeetsHardPassThreshold() {
		passLabel = "✅review通过 ⚠️test偏差"
	} else if node.TestPassed {
		passLabel = "⚠️review未达标 ✅test通过"
	}

	if !taskPassed && node.BlockingPolicy != wbsBlockingFailOpen {
		if o.shouldSplitLongRunningLeaf(node, start) {
			return o.handleLongRunningLeaf(ctx, node, objective, team, start)
		}
		errText := fmt.Sprintf("hard gate failed: %s", passLabel)
		if lastScore.Feedback != "" {
			errText += "\n" + truncateResult(lastScore.Feedback, 1200)
		}
		node.Error = errText
		cascaded, _ := o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount += 1 + cascaded
		o.mu.Unlock()
		if o.checkpoints != nil {
			o.checkpoints.SaveCheckpoint(node.Title, "failed", node.Retries, errText)
		}
		o.recordWBSTaskDuration(team, node, duration, TaskFailed)
		o.recordWBSFailedBlockedDependents(team, node, cascaded)
		if cascaded > 0 {
			o.notify(o.chatID, fmt.Sprintf("🔴 %s hard gate 未通过 (%s) %s, 级联阻塞 %d 个下游任务", node.Title, duration.Round(time.Second), passLabel, cascaded))
		} else {
			o.notify(o.chatID, fmt.Sprintf("🔴 %s hard gate 未通过 (%s) %s", node.Title, duration.Round(time.Second), passLabel))
		}
		return StageResult{
			Name: node.Title, Role: node.Role, Status: TaskFailed,
			Output: lastOutput, Error: errText, StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "completed")
	o.mu.Lock()
	o.completedCount++
	o.mu.Unlock()

	// 检查点: task 完成后持久化 (断点续作)
	// 修复: 使用 node.Title (stage name) 作为 key, 与 WorkflowExecutor.restoreCheckpoints 保持一致
	if o.checkpoints != nil {
		o.checkpoints.SaveCheckpoint(node.Title, "completed", 0, lastOutput)
	}
	o.recordWBSTaskDuration(team, node, duration, TaskCompleted)
	o.notify(o.chatID, fmt.Sprintf("✅ %s 完成 (%s) %s", node.Title, duration.Round(time.Second), passLabel))

	if team != nil && team.Blackboard != nil {
		team.Blackboard.Write(node.Title+"-result", lastOutput, node.Role, "result")
	}

	return StageResult{
		Name: node.Title, Role: node.Role, Status: TaskCompleted,
		Output: lastOutput, StartedAt: start, Duration: duration.Round(time.Second).String(),
	}
}

func materializationGateError(output string, written []string, lang string) string {
	if len(written) > 0 || !outputContainsRelevantCodeBlock(output, lang) {
		return ""
	}
	return "未能从 coder 输出中物化任何文件。请使用 `File: path/to/file.go` 或 `### path/to/file.go` 标注每个代码块，并确保输出包含 go.mod。"
}

func materializationGateErrorForTask(output string, written []string, lang string, node *TaskNode) string {
	if len(written) > 0 {
		return ""
	}
	if node != nil && orchNormalizeRole(node.Role) == "coder" && node.TaskType != wbsTaskTypeVerification && len(node.TargetFiles) > 0 {
		return "当前 coder Leaf 声明了 targetFiles 但未能物化任何文件。请按 `File: path/to/file.go` 或 `### path/to/file.go` 输出完整文件内容。"
	}
	return materializationGateError(output, written, lang)
}

func shouldRunLocalTestsForWritten(written []string) bool {
	for _, file := range written {
		if strings.HasSuffix(strings.ToLower(filepath.ToSlash(file)), "_test.go") {
			return true
		}
	}
	return false
}

func shouldAcceptIncrementalLeaf(node *TaskNode, score EvalScore, testPassed, buildPassed bool) bool {
	if node == nil || !testPassed || !buildPassed {
		return false
	}
	if node.TaskType != wbsTaskTypeLeaf {
		return false
	}
	if !strings.Contains(node.SplitReason, "objective-fallback") && !strings.Contains(node.SplitReason, "sizing-gate") {
		return false
	}
	return score.Correctness >= 6 && score.CodeQuality >= 7 && score.Security >= 6
}

func enforceTargetFileScope(cwd string, written, targetFiles []string) []string {
	if cwd == "" || len(written) == 0 || len(targetFiles) == 0 {
		return written
	}
	allowed := make(map[string]bool, len(targetFiles))
	for _, target := range targetFiles {
		clean := cleanMaterializeRelPath(target)
		if clean != "" {
			allowed[filepath.ToSlash(clean)] = true
		}
	}
	if len(allowed) == 0 {
		return written
	}
	filtered := make([]string, 0, len(written))
	for _, file := range written {
		clean := cleanMaterializeRelPath(file)
		if clean == "" {
			continue
		}
		slash := filepath.ToSlash(clean)
		if allowed[slash] {
			filtered = append(filtered, clean)
			continue
		}
		_ = os.Remove(filepath.Join(cwd, clean))
	}
	return filtered
}

func missingTargetFilesError(cwd string, targetFiles []string) string {
	if cwd == "" || len(targetFiles) == 0 {
		return ""
	}
	var missing []string
	for _, target := range targetFiles {
		clean := cleanMaterializeRelPath(target)
		if clean == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(cwd, clean)); err != nil {
			missing = append(missing, filepath.ToSlash(clean))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "目标文件缺失, 必须输出完整文件: " + strings.Join(missing, ", ")
}

func inferTaskBuildCwd(cwd string, written []string) string {
	if cwd == "" || len(written) == 0 {
		return ""
	}
	var first string
	for _, rel := range written {
		clean := filepath.ToSlash(filepath.Clean(rel))
		parts := strings.Split(clean, "/")
		if len(parts) < 2 {
			return ""
		}
		if first == "" {
			first = parts[0]
		} else if first != parts[0] {
			return ""
		}
	}
	if !isLikelyGeneratedProjectRoot(first) {
		return ""
	}
	return filepath.Join(cwd, filepath.FromSlash(first))
}

func inferBuildCwdFromTaskScope(cwd string, targetFiles, targetPackages []string) string {
	if cwd == "" {
		return ""
	}
	if inferred := inferTaskBuildCwd(cwd, targetFiles); inferred != "" {
		return inferred
	}
	for _, pkg := range targetPackages {
		pkg = strings.TrimSpace(filepath.ToSlash(pkg))
		if strings.HasPrefix(pkg, "./") {
			pkg = strings.TrimPrefix(pkg, "./")
		}
		pkg = strings.TrimSuffix(pkg, "/...")
		parts := strings.Split(pkg, "/")
		if len(parts) > 0 && isLikelyGeneratedProjectRoot(parts[0]) {
			return filepath.Join(cwd, filepath.FromSlash(parts[0]))
		}
	}
	return cwd
}

func isLikelyGeneratedProjectRoot(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	switch name {
	case "cmd", "internal", "pkg", "test", "tests", "docs", "config", "configs", "scripts", "api":
		return false
	default:
		return true
	}
}

func adjustTargetPackagesForBuildRoot(cwd, buildCwd string, targetPackages []string) []string {
	if cwd == "" || buildCwd == "" || len(targetPackages) == 0 || cwd == buildCwd {
		return targetPackages
	}
	rel, err := filepath.Rel(cwd, buildCwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return targetPackages
	}
	rel = filepath.ToSlash(rel)
	prefixes := []string{"./" + rel + "/", rel + "/"}
	adjusted := make([]string, 0, len(targetPackages))
	for _, pkg := range targetPackages {
		next := pkg
		for _, prefix := range prefixes {
			if strings.HasPrefix(next, prefix) {
				next = "./" + strings.TrimPrefix(next, prefix)
				break
			}
		}
		adjusted = append(adjusted, next)
	}
	return adjusted
}

var (
	objectiveTargetRootPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:输出到|放到|写到|生成到|创建到)(?:工作目录(?:下|的)?|当前目录(?:下|的)?|cwd)?\s*[\\/]*([A-Za-z0-9][A-Za-z0-9._-]*)(?:\s*(?:目录|文件夹|/|下|中|内|$|[，,。).）]))`),
		regexp.MustCompile(`(?i)([A-Za-z0-9][A-Za-z0-9._-]*)(?:\s*(?:目录|文件夹))(?:下|中|内)?`),
	}
	objectiveTargetRootNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func inferObjectiveTargetRoot(objective string) string {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return ""
	}
	for _, pattern := range objectiveTargetRootPatterns {
		if match := pattern.FindStringSubmatch(objective); len(match) > 1 {
			if root := sanitizeObjectiveTargetRoot(match[1]); root != "" {
				return root
			}
		}
	}
	return ""
}

func sanitizeObjectiveTargetRoot(raw string) string {
	root := strings.TrimSpace(raw)
	root = strings.Trim(root, "`'\"“”‘’ ，,。.)）(")
	root = filepath.ToSlash(filepath.Clean(root))
	if root == "." || root == "" || strings.Contains(root, "/") || strings.HasPrefix(root, ".") || strings.HasPrefix(root, "..") {
		return ""
	}
	if !objectiveTargetRootNamePattern.MatchString(root) {
		return ""
	}
	return root
}

// executeTaskOnce 非对抗模式: 单轮执行
func (o *Orchestrator) executeTaskOnce(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, start time.Time) StageResult {
	prompt := o.buildTaskPrompt(node, objective, team)
	runner, err := o.factory(ctx, node.Role, "")
	if err != nil {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: time.Since(start).String()})
	}
	result, err := executeRunnerBounded(ctx, runner, prompt, coderCallTimeout)
	if err != nil {
		if isAgentExecutionTimeout(err) {
			return o.handleTaskTimeout(ctx, node, objective, team, start, err)
		}
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: time.Since(start).String()})
	}
	if reason := validateAgentOutput(result, node.Role); reason != "" {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed,
				Error: "产出验证失败: " + reason, Output: result,
				StartedAt: start, Duration: time.Since(start).String()})
	}

	node.Output = result
	o.reportProgress("LLM生成", 1, int64(len(result)), node.V2TaskID)
	duration := time.Since(start)

	o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "completed")
	o.mu.Lock()
	o.completedCount++
	o.mu.Unlock()

	// 修复: 使用 node.Title (stage name) 作为 key, 与 WorkflowExecutor.restoreCheckpoints 保持一致
	if o.checkpoints != nil {
		o.checkpoints.SaveCheckpoint(node.Title, "completed", 0, result)
	}
	o.recordWBSTaskDuration(team, node, duration, TaskCompleted)

	o.notify(o.chatID, fmt.Sprintf("✅ %s 完成 (%s)", node.Title, duration.Round(time.Second)))

	if team != nil && team.Blackboard != nil {
		team.Blackboard.Write(node.Title+"-result", result, node.Role, "result")
	}
	return StageResult{
		Name: node.Title, Role: node.Role, Status: TaskCompleted,
		Output: result, StartedAt: start, Duration: duration.Round(time.Second).String(),
	}
}

func executeRunnerBounded(ctx context.Context, runner AgentRunner, prompt string, timeout time.Duration) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("agent runner is nil")
	}
	if timeout <= 0 {
		return runner.Execute(ctx, prompt)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := runner.Execute(callCtx, prompt)
		done <- result{output: output, err: err}
	}()

	select {
	case res := <-done:
		return res.output, res.err
	case <-callCtx.Done():
		return "", &AgentExecutionTimeoutError{Timeout: timeout, Cause: callCtx.Err()}
	}
}

// runSkepticalReview 复用 SkepticalReviewerPersona + BuildSkepticalEvaluatorUserPrompt + ParseEvalScoreJSON。
// lastScore: 上一轮分数, 解析失败时 hold-last-value 而不是返回全零 (避免噪声注入 terminator)。
// buildPassed: 硬门禁结果, 注入 prompt 避免编译通过仍给 0 分 (根因修复)。
func (o *Orchestrator) runSkepticalReview(ctx context.Context, node *TaskNode, objective string, lastScore EvalScore, buildPassed bool) EvalScore {
	if o.factory == nil {
		return EvalScore{Pass: true, Correctness: 8, Completeness: 8, Security: 8, CodeQuality: 8}
	}

	taskObjective := fmt.Sprintf("任务: %s | 角色: %s | 验收标准: %s\n目标: %s",
		node.Title, node.Role, node.AcceptCriteria, objective)
	if node.DesignRef != "" && node.DesignRef != "-" {
		designCtx := o.orchDesignRefContext(node.DesignRef)
		taskObjective += "\n\n设计参考:\n" + designCtx
	}

	userPrompt := BuildSkepticalEvaluatorUserPrompt(taskObjective, node.Output)

	reviewCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	runner, err := o.factory(reviewCtx, "reviewer", SkepticalReviewerPersona)
	if err != nil {
		return HoldLastOrDefault(lastScore)
	}
	result, err := executeRunnerBounded(reviewCtx, runner, userPrompt, reviewerCallTimeout)
	if err != nil {
		return HoldLastOrDefault(lastScore)
	}

	score, parseErr := ParseEvalScoreJSON([]byte(result))
	if parseErr != nil {
		held := HoldLastOrDefault(lastScore)
		held.Feedback = result
		return held
	}
	return score
}

// HoldLastOrDefault 评分解析失败时沿用上轮分数; 上轮也为零时返回保守默认值 (6/10)。
func HoldLastOrDefault(last EvalScore) EvalScore {
	if last.Correctness > 0 || last.Completeness > 0 {
		return last
	}
	return EvalScore{Pass: true, Correctness: 6, Completeness: 6, Security: 6, CodeQuality: 6}
}

// handleTaskFailure 失败处理 + Phoenix 重试。
//
// 关键设计: 区分瞬态错误 (限流/网络) 和永久错误 (验证/代码质量):
//   - 瞬态: 不级联, 任务回 pending, 等 cooldown 后由主循环重新调度
//   - 永久: 级联下游, 避免浪费 LLM 调用
func (o *Orchestrator) handleTaskFailure(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, sr StageResult) StageResult {
	transient := isTransientError(sr.Error)

	// 瞬态错误: 额外允许更多重试 (限流可能持续数分钟)
	maxRetries := node.MaxRetries
	if transient {
		maxRetries = node.MaxRetries + 3 // 瞬态错误额外 3 次 (共 5 次)
	}

	node.Retries++
	if node.Retries <= maxRetries {
		label := "🔄"
		if transient {
			label = "⏳"
		}
		o.notify(o.chatID, fmt.Sprintf("%s %s 重试 %d/%d: %s",
			label, node.Title, node.Retries, maxRetries, sr.Error))
		node.Error = sr.Error
		_ = o.dag.SetTaskStatus(node.V2TaskID, "pending")
		return o.executeTaskNode(ctx, node, objective, team)
	}

	// 重试耗尽
	if transient {
		// 瞬态失败: 不级联, 仅标记自身 failed, 下游保持 blocked。
		// stall recovery 或 watchdog 可以在限流解除后重置此任务。
		_ = o.dag.SetTaskStatus(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount++
		o.mu.Unlock()
		o.notify(o.chatID, fmt.Sprintf("⏳ %s 因瞬态错误暂停 (重试 %d 次, 限流/网络), 下游保留等待恢复",
			node.Title, maxRetries))
		o.recordWBSTaskDuration(team, node, durationFromStageResult(sr), TaskFailed)
	} else {
		// 永久失败: 级联下游, 避免浪费 LLM 调用
		cascaded, _ := o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount += 1 + cascaded
		o.mu.Unlock()
		o.recordWBSTaskDuration(team, node, durationFromStageResult(sr), TaskFailed)
		o.recordWBSFailedBlockedDependents(team, node, cascaded)
		if cascaded > 0 {
			o.notify(o.chatID, fmt.Sprintf("❌ %s 最终失败 (重试 %d 次), 级联跳过 %d 个下游任务",
				node.Title, maxRetries, cascaded))
		} else {
			o.notify(o.chatID, fmt.Sprintf("❌ %s 最终失败 (重试 %d 次)", node.Title, maxRetries))
		}
	}
	return sr
}

// isTransientError 判断错误是否为瞬态 (限流/网络/超时), 这类错误值得等待后重试。
func isTransientError(errMsg string) bool {
	if errMsg == "" {
		return false
	}
	lower := strings.ToLower(errMsg)
	transientPatterns := []string{
		"429", "rate", "throttl", "限流", "频率",
		"timeout", "deadline exceeded", "超时",
		"connection refused", "connection reset", "网络错误",
		"503", "529", "overloaded", "过载",
		"temporary", "unavailable",
	}
	for _, p := range transientPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func durationFromStageResult(sr StageResult) time.Duration {
	if !sr.StartedAt.IsZero() {
		return time.Since(sr.StartedAt)
	}
	if sr.Duration != "" {
		if d, err := time.ParseDuration(sr.Duration); err == nil {
			return d
		}
	}
	return 0
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

## 快速验证 (4项, 每项 PASS/FAIL):
1. **编译完整性**: 语法正确? import 完整?
2. **接口对齐**: %s
3. **约束遵守**: %s 是否被遵守?
4. **TODO/STUB 检测**: 代码中是否含有 TODO, FIXME, HACK, STUB, placeholder, 占位, 待实现, panic("not implemented") 或空壳函数? 如有则 FAIL。

输出格式: 编译: PASS/FAIL | 对齐: PASS/FAIL | 约束: PASS/FAIL | TODO: PASS/FAIL | 综合: PASS/FAIL`,
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
	result, err := executeRunnerBounded(testCtx, runner, prompt, testerCallTimeout)
	if err != nil {
		return
	}

	node.TestResult = result
	node.TestPassed = !strings.Contains(strings.ToUpper(result), "FAIL")
	if !node.TestPassed {
		node.DriftReport = orchExtractDriftInfo(result)
	}

	// 子目标逐个验证 (参考 DeepSeek Prover-V2)
	if len(node.SubGoals) > 0 {
		upper := strings.ToUpper(result)
		for i := range node.SubGoals {
			sg := &node.SubGoals[i]
			sgKey := strings.ToUpper(sg.Description)
			if strings.Contains(upper, sgKey) && strings.Contains(upper, "PASS") {
				sg.Passed = true
			} else if sg.Verifier != "" && strings.Contains(upper, strings.ToUpper(sg.Verifier)) && !strings.Contains(upper, "FAIL") {
				sg.Passed = true
			}
		}
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

func languageHintForFile(path string) string {
	switch strings.ToLower(filepath.Base(path)) {
	case "go.mod", "go.sum":
		return "mod"
	case "makefile":
		return "makefile"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".md":
		return "markdown"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".cpp", ".cc", ".cxx", ".hpp", ".h":
		return "cpp"
	case ".txt":
		return "txt"
	default:
		return "txt"
	}
}

func currentGoAPISummaryForTask(team *ProductionTeam, node *TaskNode, objective string, maxChars int) string {
	if team == nil || team.Cwd == "" || node == nil {
		return ""
	}
	root := inferBuildCwdFromTaskScope(team.Cwd, node.TargetFiles, node.TargetPackages)
	if root == "" || root == team.Cwd {
		if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
			root = filepath.Join(team.Cwd, targetRoot)
		}
	}
	return summarizeGoPackageAPI(root, maxChars)
}

func summarizeGoPackageAPI(root string, maxChars int) string {
	if root == "" {
		return ""
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ""
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root {
				switch d.Name() {
				case ".git", ".claude-go", "vendor", "node_modules":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	var lines []string
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, path)
		lines = append(lines, fmt.Sprintf("File %s package %s:", filepath.ToSlash(rel), file.Name.Name))
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						lines = append(lines, "  "+summarizeTypeSpec(fset, s))
					case *ast.ValueSpec:
						kind := strings.ToLower(d.Tok.String())
						for _, name := range s.Names {
							lines = append(lines, fmt.Sprintf("  %s %s", kind, name.Name))
						}
					}
				}
			case *ast.FuncDecl:
				lines = append(lines, "  "+summarizeFuncDecl(fset, d))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	out := strings.Join(lines, "\n")
	if maxChars > 0 && len(out) > maxChars {
		out = out[:maxChars] + "\n...(API summary truncated)"
	}
	return out + "\n"
}

func summarizeTypeSpec(fset *token.FileSet, spec *ast.TypeSpec) string {
	if spec == nil {
		return ""
	}
	kind := "type"
	switch spec.Type.(type) {
	case *ast.StructType:
		kind = "struct"
	case *ast.InterfaceType:
		kind = "interface"
	case *ast.FuncType:
		kind = "func type"
	case *ast.MapType:
		kind = "map type"
	case *ast.ArrayType:
		kind = "array type"
	}
	rendered := compactWhitespace(renderNode(fset, spec.Type))
	if len(rendered) > 240 {
		rendered = rendered[:240] + "..."
	}
	return fmt.Sprintf("type %s %s = %s", spec.Name.Name, kind, rendered)
}

func summarizeFuncDecl(fset *token.FileSet, fn *ast.FuncDecl) string {
	if fn == nil {
		return ""
	}
	sig := strings.TrimPrefix(renderNode(fset, fn.Type), "func")
	recv := ""
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv = "(" + compactWhitespace(renderNode(fset, fn.Recv.List[0].Type)) + ") "
	}
	return "func " + recv + fn.Name.Name + compactWhitespace(sig)
}

func renderNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func compactWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (o *Orchestrator) buildTaskPrompt(node *TaskNode, objective string, team *ProductionTeam) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 任务: %s\n项目目标: %s\n\n", node.Title, objective))
	if node.TaskType != "" || node.EstimatedMin > 0 || node.RiskLevel != "" {
		b.WriteString("### WBS 执行预算\n")
		if node.TaskType != "" {
			b.WriteString("- taskType: " + node.TaskType + "\n")
		}
		if node.ParentID != "" {
			b.WriteString("- parentId: " + node.ParentID + "\n")
		}
		if node.EstimatedMin > 0 {
			b.WriteString(fmt.Sprintf("- estimatedMinutes: %d\n", node.EstimatedMin))
		}
		if node.RiskLevel != "" {
			b.WriteString("- riskLevel: " + node.RiskLevel + "\n")
		}
		if node.SplitReason != "" {
			b.WriteString("- splitReason: " + node.SplitReason + "\n")
		}
		b.WriteString("- 规则: 只完成当前 Leaf 的单一目标; 不要顺手实现后续 Leaf; 超出目标时优先保持可编译。\n\n")
	}

	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		b.WriteString("### 用户指定输出目录 (硬约束)\n")
		b.WriteString("- targetRoot: " + targetRoot + "\n")
		b.WriteString("- 所有新增或修改的 File path 必须以 `" + targetRoot + "/` 开头, 不要写到仓库根目录或其它目录。\n")
		b.WriteString("- 如果这是新的 Go 项目, 必须在最早的代码 Leaf 中输出 `" + targetRoot + "/go.mod`、README 和最小可编译骨架; 后续 Leaf 只能在该目录内增量补充。\n")
		b.WriteString("- Go V1 默认只使用标准库; 不要引入 testify、gonum、uuid 等第三方依赖。若绝对必须引入依赖, 目标文件必须同时包含 go.mod/go.sum, 否则 go test 会失败。\n")
		b.WriteString("- 验证命令优先使用 `cd " + targetRoot + " && go test ./...`。\n\n")
	}

	if node.DesignRef != "" && node.DesignRef != "-" {
		b.WriteString("### 设计参考\n" + o.orchDesignRefContext(node.DesignRef) + "\n\n")
	}
	if len(node.ConstraintRefs) > 0 {
		b.WriteString("### 必须遵守的约束: " + strings.Join(node.ConstraintRefs, ", ") + "\n\n")
	}
	if node.AcceptCriteria != "" && node.AcceptCriteria != "-" {
		b.WriteString("### 验收标准\n" + node.AcceptCriteria + "\n\n")
	}

	// 文件隔离: 明确限定 coder 只能修改目标文件/包
	if len(node.TargetFiles) > 0 {
		b.WriteString("### 目标文件 (仅限以下文件, 不要修改其他文件)\n")
		for _, f := range node.TargetFiles {
			b.WriteString("- " + f + "\n")
		}
		b.WriteString("\n### 文件输出格式 (硬约束, 必须照做)\n")
		b.WriteString("你必须输出每个目标文件的完整内容, 不要只描述方案。每个文件使用以下格式之一, 路径必须和目标文件完全一致:\n\n")
		for _, f := range node.TargetFiles {
			langHint := languageHintForFile(f)
			b.WriteString("### " + f + "\n")
			b.WriteString("```" + langHint + "\n")
			b.WriteString("// 或对应文件格式的完整内容\n")
			b.WriteString("```\n\n")
		}
		b.WriteString("未按上述格式输出目标文件会被视为未执行任务并触发编译门禁失败。\n\n")
	}
	if len(node.TargetPackages) > 0 {
		b.WriteString("### 目标包 (编译验证范围)\n")
		for _, p := range node.TargetPackages {
			b.WriteString("- " + p + "\n")
		}
		b.WriteString("\n")
	}

	if api := currentGoAPISummaryForTask(team, node, objective, 6000); api != "" {
		b.WriteString("### 当前真实文件 API 摘要 (来自磁盘, 必须优先遵守)\n")
		b.WriteString(api)
		b.WriteString("\n")
		b.WriteString("- 不要重复定义上述 type/var/const/func/method; 如需扩展, 只新增缺失方法或在目标文件中保持兼容。\n")
		b.WriteString("- 如果编译错误来自重复定义或签名漂移, 优先对齐这里列出的真实 API, 不要根据上轮错误输出重新发明接口。\n\n")
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
