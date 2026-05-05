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
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
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
}

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
	return o.rawTasksToDAG(rawTasks, teamName)
}

// ParsePlanToDAGWithRepair 带 repair loop 的解析: 多策略 → repair prompt → fallback。
func (o *Orchestrator) ParsePlanToDAGWithRepair(ctx context.Context, planOutput, teamName string, llmFactory CreateAgentFunc) ([]*TaskNode, error) {
	o.mu.Lock()

	if o.dag == nil {
		o.mu.Unlock()
		return nil, fmt.Errorf("DAGTaskTracker 未配置")
	}

	// 层 1+2: 多策略解析
	rawTasks := o.multiStrategyParse(planOutput)
	if len(rawTasks) > 0 {
		nodes, err := o.rawTasksToDAG(rawTasks, teamName)
		o.mu.Unlock()
		return nodes, err
	}

	// 层 3: Repair Prompt (1 轮 LLM 修复)
	o.mu.Unlock()
	if llmFactory != nil {
		repaired := o.repairPlanFormat(ctx, planOutput, llmFactory)
		if repaired != "" {
			o.mu.Lock()
			rawTasks = o.multiStrategyParse(repaired)
			if len(rawTasks) > 0 {
				nodes, err := o.rawTasksToDAG(rawTasks, teamName)
				o.mu.Unlock()
				return nodes, err
			}
			o.mu.Unlock()
		}
	}

	// 层 4: Fallback — 从自由文本提取最小 DAG
	o.mu.Lock()
	rawTasks = o.fallbackExtractTasks(planOutput)
	if len(rawTasks) > 0 {
		o.notify(o.chatID, "⚠️ WBS 格式解析失败, 使用 fallback 最小 DAG")
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
	for _, rt := range rawTasks {
		var depV2IDs []string
		for _, dn := range rt.depNums {
			if v2id, ok := numToV2ID[dn]; ok {
				depV2IDs = append(depV2IDs, v2id)
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
		}
		nodes = append(nodes, node)
		o.nodes[v2ID] = node
	}

	o.totalCount = len(nodes)
	o.dagMaxWidth = o.computeDAGWidth(rawTasks)
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

func (o *Orchestrator) repairPlanFormat(ctx context.Context, badOutput string, factory CreateAgentFunc) string {
	prompt := fmt.Sprintf(`以下开发计划的格式无法被系统解析。请将其转换为严格 JSON, 不要添加任何解释:

%sjson
{
  "tasks": [
    {"id": 1, "title": "...", "role": "coder", "dependsOn": [], "designRef": "", "constraints": [], "acceptance": "...", "priority": 1}
  ]
}
%s

原始计划:
%s

请直接输出 JSON (用 %sjson ... %s 包裹):`, "```", "```", truncateResult(badOutput, 8000), "```", "```")

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
	coderCallTimeout    = 6 * time.Minute
	reviewerCallTimeout = 2 * time.Minute
	testerCallTimeout   = 2 * time.Minute
)

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
		if errText := runBuildCheckScoped(team.Cwd, lang, node.TargetPackages); errText != "" {
			failures = append(failures, errText)
		} else {
			checks = append(checks, "scoped build passed")
		}
		if errText := runTestCheckLang(team.Cwd, lang); errText != "" {
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
		_, _ = o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount++
		o.mu.Unlock()
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
	if team != nil && team.Blackboard != nil {
		team.Blackboard.Write(node.Title+"-result", output, node.Role, "result")
	}
	o.touchActivity()
	o.reportProgress("local-verification", 1, int64(len(output)), node.V2TaskID)
	o.notify(o.chatID, fmt.Sprintf("✅ %s 本地验证完成 (%s) ✅build/test通过", node.Title, duration.Round(time.Second)))
	return StageResult{Name: node.Title, Role: node.Role, Status: TaskCompleted, Output: output, StartedAt: start, Duration: duration.Round(time.Second).String()}
}

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
	var iterMemory []IterationMemory
	bottleneckCounts := make(map[string]int)

	for round := 1; round <= terminator.MaxRounds; round++ {
		// L6 重采样: 连续低质量时清空上轮输出，重新开始
		if round > 2 && terminator.ShouldResample() {
			o.notify(o.chatID, fmt.Sprintf("♻️ %s 触发重采样 (连续低完整度), 清空上轮输出重新生成", node.Title))
			lastOutput = ""
			lastFeedback = "⚠️ 重采样模式: 上一轮实现严重不完整, 请完全重新开始, 优先保证核心功能完整输出"
		}

		// === Step 1: Coder 生成/修复 ===
		prompt := o.buildTaskPrompt(node, objective)
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
		if team.Cwd != "" {
			if written := MaterializeCode(team.Cwd, result, lang); len(written) > 0 {
				o.notify(o.chatID, fmt.Sprintf("📁 %s 文件物化: %d 个文件", node.Title, len(written)))
			}
		}

		// === Step 1.5: L2 编译硬门禁 (按任务目标包隔离编译, 避免跨任务污染) ===
		buildPassed := true
		o.reportProgress("编译", round, 0, node.V2TaskID)
		if team.Cwd != "" {
			buildErrors := runBuildCheckScoped(team.Cwd, lang, node.TargetPackages)
			if buildErrors != "" {
				buildPassed = false
				o.notify(o.chatID, fmt.Sprintf("🔴 %s 第 %d 轮编译失败, 启动内部修复...", node.Title, round))
				for retry := 1; retry <= 2; retry++ {
					fixPrompt := fmt.Sprintf("%s\n\n### 编译错误 (第 %d 次修复, 仅修复编译问题):\n%s\n\n上轮代码:\n%s",
						o.buildTaskPrompt(node, objective), retry, truncateResult(buildErrors, 3000), truncateResult(lastOutput, 10000))
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
					MaterializeCode(team.Cwd, fixResult, lang)
					buildErrors = runBuildCheckScoped(team.Cwd, lang, node.TargetPackages)
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
					lastFeedback = "编译失败，必须优先修复编译错误后再考虑功能"
					continue
				}
			}
			if buildPassed {
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

		if round+1 <= terminator.MaxRounds {
			o.notify(o.chatID, fmt.Sprintf("🔄 %s 继续对抗 (第 %d/%d 轮)...",
				node.Title, round+1, terminator.MaxRounds))
		}
	}

	duration := time.Since(start)

	o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "completed")
	o.mu.Lock()
	o.completedCount++
	o.mu.Unlock()

	// 检查点: task 完成后持久化 (断点续作)
	// 修复: 使用 node.Title (stage name) 作为 key, 与 WorkflowExecutor.restoreCheckpoints 保持一致
	if o.checkpoints != nil {
		o.checkpoints.SaveCheckpoint(node.Title, "completed", 0, lastOutput)
	}

	passLabel := "⚠️未达标"
	if lastScore.MeetsHardPassThreshold() && node.TestPassed {
		passLabel = "✅全通过"
	} else if lastScore.MeetsHardPassThreshold() {
		passLabel = "✅review通过 ⚠️test偏差"
	} else if node.TestPassed {
		passLabel = "⚠️review未达标 ✅test通过"
	}
	o.notify(o.chatID, fmt.Sprintf("✅ %s 完成 (%s) %s", node.Title, duration.Round(time.Second), passLabel))

	if team.Blackboard != nil {
		team.Blackboard.Write(node.Title+"-result", lastOutput, node.Role, "result")
	}

	return StageResult{
		Name: node.Title, Role: node.Role, Status: TaskCompleted,
		Output: lastOutput, StartedAt: start, Duration: duration.Round(time.Second).String(),
	}
}

// executeTaskOnce 非对抗模式: 单轮执行
func (o *Orchestrator) executeTaskOnce(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, start time.Time) StageResult {
	prompt := o.buildTaskPrompt(node, objective)
	runner, err := o.factory(ctx, node.Role, "")
	if err != nil {
		return o.handleTaskFailure(ctx, node, objective, team,
			StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: err.Error(),
				StartedAt: start, Duration: time.Since(start).String()})
	}
	result, err := executeRunnerBounded(ctx, runner, prompt, coderCallTimeout)
	if err != nil {
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

	o.notify(o.chatID, fmt.Sprintf("✅ %s 完成 (%s)", node.Title, duration.Round(time.Second)))

	if team.Blackboard != nil {
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
		return "", fmt.Errorf("agent execution timeout after %s: %w", timeout, callCtx.Err())
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
	} else {
		// 永久失败: 级联下游, 避免浪费 LLM 调用
		cascaded, _ := o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount += 1 + cascaded
		o.mu.Unlock()
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

	// 文件隔离: 明确限定 coder 只能修改目标文件/包
	if len(node.TargetFiles) > 0 {
		b.WriteString("### 目标文件 (仅限以下文件, 不要修改其他文件)\n")
		for _, f := range node.TargetFiles {
			b.WriteString("- " + f + "\n")
		}
		b.WriteString("\n")
	}
	if len(node.TargetPackages) > 0 {
		b.WriteString("### 目标包 (编译验证范围)\n")
		for _, p := range node.TargetPackages {
			b.WriteString("- " + p + "\n")
		}
		b.WriteString("\n")
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
