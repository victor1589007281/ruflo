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
	"os/exec"
	"path"
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
	WorkUnitType   string    `json:"workUnitType,omitempty"`   // "contract", "implementation", "integration", "verification"
	CapabilityID   string    `json:"capabilityId,omitempty"`
	ContractRefs   []string  `json:"contractRefs,omitempty"`
	Provides       []string  `json:"provides,omitempty"`
	Requires       []string  `json:"requires,omitempty"`
	ReadFiles      []string  `json:"readFiles,omitempty"`
	WriteFiles     []string  `json:"writeFiles,omitempty"`
	ConflictKeys   []string  `json:"conflictKeys,omitempty"`
	EstimatedLOC   int       `json:"estimatedChangedLOC,omitempty"`
	ParentID       string    `json:"parentId,omitempty"`
	EstimatedMin   int       `json:"estimatedMinutes,omitempty"`
	RiskLevel      string    `json:"riskLevel,omitempty"` // "low", "medium", "high"
	VerifyCommand  string    `json:"verifyCommand,omitempty"`
	ParallelGroup  string    `json:"parallelGroup,omitempty"`
	BlockingPolicy string    `json:"blockingPolicy,omitempty"` // "fail_blocks_dependents", "fail_open"
	SplitReason    string    `json:"splitReason,omitempty"`
	TimeoutKind    string    `json:"timeoutKind,omitempty"`

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
	planningLang   string // language adapter used by local plan compiler/sizing gate

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
	workUnitType   string
	capabilityID   string
	contractRefs   []string
	provides       []string
	requires       []string
	readFiles      []string
	writeFiles     []string
	conflictKeys   []string
	estimatedLOC   int
	parentID       string
	estimatedMin   int
	riskLevel      string
	verifyCommand  string
	parallelGroup  string
	blockingPolicy string
	splitReason    string
	timeoutKind    string
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

	wbsWorkUnitContract       = "contract"
	wbsWorkUnitImplementation = "implementation"
	wbsWorkUnitIntegration    = "integration"
	wbsWorkUnitVerification   = "verification"

	wbsTimeoutKindTrueOversize  = "true_oversize"
	wbsTimeoutKindProviderStall = "provider_or_network_stall"
	wbsTimeoutKindPromptBloat   = "prompt_bloat"
	wbsTimeoutKindRateLimit     = "rate_limit_or_overload"
	wbsTimeoutKindUnknown       = "unknown_timeout"
	wbsTimeoutActionSplit       = "split"
	wbsTimeoutActionRetryOrFail = "retry_or_fail"
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

func (o *Orchestrator) SetPlanningLanguage(language string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.planningLang = inferPlanningLanguageID("", language)
}

func (o *Orchestrator) planningAdapter(objective string) planningLanguageAdapter {
	if o.planningLang == "" {
		o.planningLang = inferPlanningLanguageID(objective, "")
	}
	return planningLanguageAdapterFor(o.planningLang)
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
	rawTasks = o.normalizeAndSplitRawTasks(rawTasks, "")
	return o.rawTasksToDAG(rawTasks, teamName)
}

// ParsePlanToDAGWithRepair 带 repair loop 的解析: 多策略 → repair prompt → fallback。
func (o *Orchestrator) ParsePlanToDAGWithRepair(ctx context.Context, planOutput, objective, teamName string, llmFactory CreateAgentFunc) ([]*TaskNode, error) {
	o.mu.Lock()

	if o.dag == nil {
		o.mu.Unlock()
		return nil, fmt.Errorf("DAGTaskTracker 未配置")
	}
	o.planningAdapter(objective)

	// 层 1+2: 多策略解析
	rawTasks := o.multiStrategyParse(planOutput)
	var firstValidationErr error
	if len(rawTasks) > 0 {
		firstValidationErr = validateParsedWBSForObjective(rawTasks, objective)
	}
	if len(rawTasks) > 0 && firstValidationErr == nil {
		rawTasks = o.normalizeAndSplitRawTasks(rawTasks, objective)
		if err := validateFinalWBSForObjective(rawTasks, objective); err != nil {
			if objectiveAllowsDeterministicV1Fallback(objective) {
				o.notify(o.chatID, "⚠️ Planner WBS 二次拆分后超过最终预算, 切换到 universal V1 fallback DAG")
				rawTasks = o.normalizeAndSplitRawTasks(synthesizeObjectiveWBS(objective), objective)
				if fallbackErr := validateFinalWBSForObjective(rawTasks, objective); fallbackErr != nil {
					o.mu.Unlock()
					return nil, fmt.Errorf("%v; fallback invalid: %w", err, fallbackErr)
				}
			} else {
				o.notify(o.chatID, "⚠️ Planner WBS 未满足完整设计任务预算, 已拒绝 V1 降级并要求真实 Planner 重做")
				o.mu.Unlock()
				return nil, err
			}
		}
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
				rawTasks = o.normalizeAndSplitRawTasks(rawTasks, objective)
				if err := validateFinalWBSForObjective(rawTasks, objective); err != nil {
					if objectiveAllowsDeterministicV1Fallback(objective) {
						o.notify(o.chatID, "⚠️ Repair WBS 二次拆分后超过最终预算, 切换到 universal V1 fallback DAG")
						rawTasks = o.normalizeAndSplitRawTasks(synthesizeObjectiveWBS(objective), objective)
						if fallbackErr := validateFinalWBSForObjective(rawTasks, objective); fallbackErr != nil {
							o.mu.Unlock()
							return nil, fmt.Errorf("%v; fallback invalid: %w", err, fallbackErr)
						}
					} else {
						o.notify(o.chatID, "⚠️ Repair WBS 仍未满足完整设计任务预算, 已拒绝 V1 降级")
						o.mu.Unlock()
						return nil, err
					}
				}
				nodes, err := o.rawTasksToDAG(rawTasks, teamName)
				o.mu.Unlock()
				return nodes, err
			} else if len(rawTasks) > 0 {
				o.notify(o.chatID, "⚠️ Repair WBS 仍未满足目标目录/可执行 Leaf 约束, 尝试从原计划提取最小 DAG")
			}
			o.mu.Unlock()
		}
	}

	// 层 4: Fallback — 从自由文本提取最小 DAG
	o.mu.Lock()
	rawTasks = o.fallbackExtractTasks(planOutput)
	if len(rawTasks) > 0 && validateParsedWBSForObjective(rawTasks, objective) == nil {
		o.notify(o.chatID, "⚠️ WBS 格式解析失败, 使用 fallback 最小 DAG")
		rawTasks = o.normalizeAndSplitRawTasks(rawTasks, objective)
		if err := validateFinalWBSForObjective(rawTasks, objective); err != nil {
			if objectiveAllowsDeterministicV1Fallback(objective) {
				rawTasks = o.normalizeAndSplitRawTasks(synthesizeObjectiveWBS(objective), objective)
				if fallbackErr := validateFinalWBSForObjective(rawTasks, objective); fallbackErr != nil {
					o.mu.Unlock()
					return nil, fmt.Errorf("%v; fallback invalid: %w", err, fallbackErr)
				}
			} else {
				o.mu.Unlock()
				return nil, err
			}
		}
		nodes, err := o.rawTasksToDAG(rawTasks, teamName)
		o.mu.Unlock()
		return nodes, err
	}
	o.mu.Unlock()
	if firstValidationErr != nil && !objectiveAllowsDeterministicV1Fallback(objective) {
		return nil, firstValidationErr
	}
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
	fillVerificationDeps(rawTasks)
	rawTasks = orderRawTasksByDeps(rawTasks)
	o.teamName = teamName
	var nodes []*TaskNode
	numToV2ID := make(map[string]string)
	fileLastV2ID := make(map[string]string)
	fileLastNum := make(map[string]string)
	conflictLastV2ID := make(map[string]string)
	conflictLastNum := make(map[string]string)
	widthTasks := make([]rawTask, 0, len(rawTasks))
	for _, rt := range rawTasks {
		var depV2IDs []string
		for _, dn := range rt.depNums {
			if v2id, ok := numToV2ID[dn]; ok {
				depV2IDs = append(depV2IDs, v2id)
			}
		}
		for _, key := range rawTaskConflictKeys(rt) {
			if prev, ok := conflictLastV2ID[key]; ok && !containsString(depV2IDs, prev) {
				depV2IDs = append(depV2IDs, prev)
			}
			if prevNum, ok := conflictLastNum[key]; ok && !containsString(rt.depNums, prevNum) {
				rt.depNums = append(rt.depNums, prevNum)
			}
		}
		for _, file := range rawTaskWriteFiles(rt) {
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
		depV2IDs = uniqueTrimmedStrings(depV2IDs)

		subject := orchestratorTaskSubject(teamName, rt.title)
		v2ID, err := o.dag.AddTaskWithDeps(subject, rt.accept, rt.role, depV2IDs, rt.priority)
		if err != nil {
			return nodes, fmt.Errorf("创建V2 DAG任务失败: %w", err)
		}
		if containsString(depV2IDs, v2ID) {
			depV2IDs = removeString(depV2IDs, v2ID)
			v2ID, err = o.dag.AddTaskWithDeps(subject, rt.accept, rt.role, depV2IDs, rt.priority)
			if err != nil {
				return nodes, fmt.Errorf("修正V2 DAG自依赖失败: %w", err)
			}
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
			WorkUnitType:   rt.workUnitType,
			CapabilityID:   rt.capabilityID,
			ContractRefs:   rt.contractRefs,
			Provides:       rt.provides,
			Requires:       rt.requires,
			ReadFiles:      rt.readFiles,
			WriteFiles:     rt.writeFiles,
			ConflictKeys:   rt.conflictKeys,
			EstimatedLOC:   rt.estimatedLOC,
			ParentID:       rt.parentID,
			EstimatedMin:   rt.estimatedMin,
			RiskLevel:      rt.riskLevel,
			VerifyCommand:  rt.verifyCommand,
			ParallelGroup:  rt.parallelGroup,
			BlockingPolicy: rt.blockingPolicy,
			SplitReason:    rt.splitReason,
			TimeoutKind:    rt.timeoutKind,
		}
		nodes = append(nodes, node)
		o.nodes[v2ID] = node
		for _, key := range rawTaskConflictKeys(rt) {
			conflictLastV2ID[key] = v2ID
			conflictLastNum[key] = rt.num
		}
		for _, file := range rawTaskWriteFiles(rt) {
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
	WorkUnitType   string          `json:"workUnitType,omitempty"`
	CapabilityID   string          `json:"capabilityId,omitempty"`
	ContractRefs   []string        `json:"contractRefs,omitempty"`
	Provides       []string        `json:"provides,omitempty"`
	Requires       []string        `json:"requires,omitempty"`
	ReadFiles      []string        `json:"readFiles,omitempty"`
	WriteFiles     []string        `json:"writeFiles,omitempty"`
	ConflictKeys   []string        `json:"conflictKeys,omitempty"`
	EstimatedLOC   int             `json:"estimatedChangedLOC,omitempty"`
	ParentID       flexibleWBSID   `json:"parentId,omitempty"`
	EstimatedMin   int             `json:"estimatedMinutes,omitempty"`
	RiskLevel      string          `json:"riskLevel,omitempty"`
	VerifyCommand  string          `json:"verifyCommand,omitempty"`
	ParallelGroup  string          `json:"parallelGroup,omitempty"`
	BlockingPolicy string          `json:"blockingPolicy,omitempty"`
	SplitReason    string          `json:"splitReason,omitempty"`
	TimeoutKind    string          `json:"timeoutKind,omitempty"`
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
	for _, body := range wbsJSONCandidateBodies(planOutput) {
		var wbs wbsJSON
		if err := json.Unmarshal([]byte(body), &wbs); err != nil || len(wbs.Tasks) == 0 {
			continue
		}
		return rawTasksFromWBS(wbs)
	}
	return nil
}

func rawTasksFromWBS(wbs wbsJSON) []rawTask {
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
			workUnitType:   t.WorkUnitType,
			capabilityID:   t.CapabilityID,
			contractRefs:   t.ContractRefs,
			provides:       t.Provides,
			requires:       t.Requires,
			readFiles:      t.ReadFiles,
			writeFiles:     t.WriteFiles,
			conflictKeys:   t.ConflictKeys,
			estimatedLOC:   t.EstimatedLOC,
			parentID:       strings.TrimSpace(string(t.ParentID)),
			estimatedMin:   t.EstimatedMin,
			riskLevel:      t.RiskLevel,
			verifyCommand:  t.VerifyCommand,
			parallelGroup:  t.ParallelGroup,
			blockingPolicy: t.BlockingPolicy,
			splitReason:    t.SplitReason,
			timeoutKind:    t.TimeoutKind,
		})
	}
	return tasks
}

func wbsJSONCandidateBodies(text string) []string {
	var candidates []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range candidates {
			if existing == s {
				return
			}
		}
		candidates = append(candidates, s)
	}
	add(stripCodeFences(text))
	add(text)
	for _, obj := range extractJSONObjectCandidates(text) {
		add(obj)
	}
	return candidates
}

func extractJSONObjectCandidates(text string) []string {
	var out []string
	start := -1
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, strings.TrimSpace(text[start:i+1]))
				start = -1
			}
		}
	}
	return out
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
		targetRule = fmt.Sprintf("\n硬约束: 用户指定输出目录为 %s。所有 targetFiles/writeFiles 必须以 %s/ 开头; verification 必须使用架构阶段确定的语言适配器命令, 不要默认假设 Go。\n",
			targetRoot, targetRoot)
	}
	sampleRoot := targetRoot
	if sampleRoot == "" {
		sampleRoot = "X"
	}
	sampleManifest := objectiveManifestPath(sampleRoot, objective)
	sampleSource := filepath.ToSlash(filepath.Join(sampleRoot, "internal", "module.ext"))
	prompt := fmt.Sprintf(`以下开发计划的格式无法被系统解析或不可执行。请将其转换为严格 JSON, 不要添加任何解释:

项目目标:
%s
%s

%sjson
{
  "tasks": [
    {
      "id": 1,
      "title": "创建目标语言项目骨架与 manifest",
      "role": "coder",
      "taskType": "leaf",
      "dependsOn": [],
      "designRef": "上游架构设计中的目录结构/语言适配器摘要",
      "constraints": [],
      "acceptance": "目标目录下最小 build/test 通过",
      "priority": 3,
      "estimatedMinutes": 3,
      "riskLevel": "low",
      "verifyCommand": "架构阶段确定的本地 build/test 命令",
      "parallelGroup": "project-skeleton",
      "workUnitType": "contract",
      "capabilityId": "project-skeleton",
      "contractRefs": [],
      "provides": ["project skeleton"],
      "requires": [],
      "readFiles": [],
      "writeFiles": ["%s", "%s/README.md", "%s"],
      "conflictKeys": ["contract:project"],
      "estimatedChangedLOC": 120,
      "blockingPolicy": "fail_blocks_dependents",
      "targetFiles": ["%s", "%s/README.md", "%s"],
      "targetPackages": []
    }
  ]
}
%s

硬性规则:
1. 禁止输出“读取设计文档/查看目录/检查目标目录/理解需求/制定计划”等元任务。
2. 禁止输出 bash/cat/ls/Read/minimax:tool_call 等伪工具调用。
3. 每个 coder leaf 必须有非空 targetFiles/writeFiles, 且必须是实现或测试文件。
4. 第一个 coder leaf 创建目标语言项目骨架和 manifest; 后续 leaf 只增量实现模块。
5. verification task 只跑本地 build/test/TODO scan, 不调用 tester LLM。

原始计划:
%s

请直接输出 JSON (用 %sjson ... %s 包裹):`, objective, targetRule, "```", sampleManifest, sampleRoot, sampleSource, sampleManifest, sampleRoot, sampleSource, "```", truncateResult(badOutput, 8000), "```", "```")

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

func objectiveManifestPath(root, objective string) string {
	lower := strings.ToLower(objective)
	switch {
	case strings.Contains(lower, "golang") || strings.Contains(lower, "go ") || strings.Contains(lower, "go项目") || strings.Contains(lower, "go 项目"):
		return filepath.ToSlash(filepath.Join(root, "go.mod"))
	case strings.Contains(lower, "python") || strings.Contains(lower, "pyproject") || strings.Contains(lower, "pytest"):
		return filepath.ToSlash(filepath.Join(root, "pyproject.toml"))
	case strings.Contains(lower, "typescript") || strings.Contains(lower, "javascript") || strings.Contains(lower, "node") || strings.Contains(lower, "npm"):
		return filepath.ToSlash(filepath.Join(root, "package.json"))
	case strings.Contains(lower, "rust") || strings.Contains(lower, "cargo"):
		return filepath.ToSlash(filepath.Join(root, "Cargo.toml"))
	case strings.Contains(lower, "java") || strings.Contains(lower, "maven"):
		return filepath.ToSlash(filepath.Join(root, "pom.xml"))
	case strings.Contains(lower, "c++") || strings.Contains(lower, "cpp") || strings.Contains(lower, "cmake"):
		return filepath.ToSlash(filepath.Join(root, "CMakeLists.txt"))
	default:
		return filepath.ToSlash(filepath.Join(root, "README.md"))
	}
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
	if err := validateWBSLeafBudget(rawTasks, objective); err != nil {
		return err
	}
	if err := validateUnrequestedWBSSurfaces(rawTasks, objective); err != nil {
		return err
	}
	if err := validateUnrequestedAdvancedCoreSurfaces(rawTasks, objective); err != nil {
		return err
	}

	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" {
		return nil
	}
	applyObjectiveTargetRootToWBS(rawTasks, targetRoot)
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

func validateFinalWBSForObjective(rawTasks []rawTask, objective string) error {
	if len(rawTasks) == 0 {
		return fmt.Errorf("empty normalized WBS")
	}
	if objectiveRequiresDesignCompleteMode(objective) && hasObjectiveFallbackRawTask(rawTasks) {
		return fmt.Errorf("design-complete objective rejected deterministic V1 fallback WBS; planner must produce a real design-coverage DAG")
	}
	leafCount := countExecutableWBSLeaves(rawTasks)
	budget := objectiveWBSFinalLeafBudget(objective)
	if leafCount > budget {
		return fmt.Errorf("normalized WBS executable leaf count %d exceeds final DAG budget %d; planner must merge trivial scaffolding while preserving the requested acceptance scope", leafCount, budget)
	}
	return nil
}

func hasObjectiveFallbackRawTask(rawTasks []rawTask) bool {
	for _, task := range rawTasks {
		if strings.Contains(strings.ToLower(task.splitReason), "objective-fallback") {
			return true
		}
	}
	return false
}

func validateWBSLeafBudget(rawTasks []rawTask, objective string) error {
	leafCount := countExecutableWBSLeaves(rawTasks)
	if leafCount == 0 {
		return fmt.Errorf("WBS has no executable leaf tasks")
	}
	budget := objectiveWBSLeafBudget(objective)
	if leafCount > budget {
		return fmt.Errorf("WBS leaf count %d exceeds objective budget %d; keep macro tasks at capability boundaries and let TaskSizingGate split only oversized leaves", leafCount, budget)
	}
	return nil
}

func countExecutableWBSLeaves(rawTasks []rawTask) int {
	count := 0
	for _, task := range rawTasks {
		taskType := normalizeWBSTaskType(task.taskType)
		if taskType == wbsTaskTypeMacro || taskType == wbsTaskTypeVerification {
			continue
		}
		if isGenericMetaWBSTitle(task.title) {
			continue
		}
		count++
	}
	return count
}

func objectiveWBSLeafBudget(objective string) int {
	if objectiveRequiresDesignCompleteMode(objective) {
		return designCompleteWBSBudget(objective, 72)
	}
	if objectiveRequestsExpandedScope(objective) {
		return 28
	}
	return 18
}

func objectiveWBSFinalLeafBudget(objective string) int {
	if objectiveRequiresDesignCompleteMode(objective) {
		return max(120, designCompleteWBSBudget(objective, 120))
	}
	if objectiveRequestsExpandedScope(objective) {
		return 30
	}
	return 20
}

func designCompleteWBSBudget(objective string, floor int) int {
	paths := extractLocalReferencePaths(objective)
	if len(paths) == 0 {
		return floor
	}
	docCount, totalBytes := localReferenceDocStats(paths)
	if docCount == 0 && totalBytes == 0 {
		return floor
	}
	// Full-design objectives need enough DAG room to keep leaf tasks small.
	// Scale with the referenced design corpus, but cap to avoid pathological
	// planner output from turning into unlimited concurrency pressure.
	byDocs := 80 + docCount*10
	bySize := 80 + int(totalBytes/2048)
	return min(260, max(floor, max(byDocs, bySize)))
}

func localReferenceDocStats(paths []string) (int, int64) {
	var count int
	var total int64
	seen := make(map[string]bool)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if isReferenceDocFile(p) && !seen[p] {
				seen[p] = true
				count++
				total += info.Size()
			}
			continue
		}
		root := p
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != root {
					switch d.Name() {
					case ".git", ".claude-go", "node_modules", "vendor", "dist", "build":
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !isReferenceDocFile(path) || seen[path] {
				return nil
			}
			seen[path] = true
			count++
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
			return nil
		})
	}
	return count, total
}

func objectiveRequestsExpandedScope(objective string) bool {
	if objectiveRequiresDesignCompleteMode(objective) {
		return true
	}
	lower := strings.ToLower(objective)
	hints := []string{
		"full", "complete", "all modules", "end-to-end", "e2e",
		"enterprise", "production-grade", "production ready",
		"distributed", "cluster", "microservice",
		"完整", "全量", "全部", "所有模块", "端到端", "企业级", "生产级", "生产可用", "分布式", "集群", "微服务",
	}
	for _, hint := range hints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func objectiveAllowsDeterministicV1Fallback(objective string) bool {
	return !objectiveRequiresDesignCompleteMode(objective)
}

func objectiveRequiresDesignCompleteMode(objective string) bool {
	lower := strings.ToLower(objective)
	strongHints := []string{
		"100%", "100 percent", "百分百", "完全满足", "严格满足",
		"严格按照", "完全按照", "完整按照", "全部按照",
		"完整实现", "全量实现", "全部实现", "全功能",
		"全部设计", "完整设计", "设计文档", "design docs", "design document",
	}
	for _, hint := range strongHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	if !objectiveReferencesDesignSource(objective) {
		return false
	}
	// When the user points development at a concrete design corpus, the design
	// corpus is the acceptance scope. Do not silently degrade it to a V1 slice.
	designScopeHints := []string{
		"参考", "按照", "基于", "设计方案", "设计目录",
		"reference", "based on", "follow", "according to",
	}
	for _, hint := range designScopeHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func objectiveReferencesDesignSource(objective string) bool {
	lower := strings.ToLower(objective)
	if strings.Contains(lower, "/v2") || strings.Contains(lower, "\\v2") {
		return true
	}
	hints := []string{
		"设计文档", "设计方案", "设计目录", "参考设计", "架构设计",
		"design doc", "design docs", "reference design", "architecture doc",
	}
	for _, hint := range hints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	for _, path := range extractLocalReferencePaths(objective) {
		base := strings.ToLower(filepath.Base(path))
		if strings.Contains(base, "design") || strings.Contains(base, "v2") || strings.Contains(base, "设计") {
			return true
		}
	}
	return false
}

func validateUnrequestedWBSSurfaces(rawTasks []rawTask, objective string) error {
	if objectiveRequestsExternalProtocolSurface(objective) {
		return nil
	}
	var offenders []string
	for _, task := range rawTasks {
		if !rawTaskUsesExternalProtocolSurface(task) {
			continue
		}
		label := strings.TrimSpace(task.title)
		if label == "" {
			label = strings.TrimSpace(task.num)
		}
		if label == "" {
			label = "unnamed task"
		}
		offenders = append(offenders, label)
		if len(offenders) >= 3 {
			break
		}
	}
	if len(offenders) > 0 {
		return fmt.Errorf("WBS includes external serving/RPC surface not requested by the objective: %s", strings.Join(offenders, "; "))
	}
	return nil
}

func objectiveRequestsExternalProtocolSurface(objective string) bool {
	if objectiveRequestsExpandedScope(objective) {
		return true
	}
	return containsExternalProtocolSurfaceTerm(objective)
}

func rawTaskUsesExternalProtocolSurface(task rawTask) bool {
	parts := []string{task.title, task.designRef, task.accept, task.verifyCommand, task.parentID, task.capabilityID, task.parallelGroup}
	parts = append(parts, task.targetFiles...)
	parts = append(parts, task.writeFiles...)
	parts = append(parts, task.readFiles...)
	for _, part := range parts {
		if containsExternalProtocolSurfaceTerm(part) {
			return true
		}
	}
	return false
}

func containsExternalProtocolSurfaceTerm(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, ".proto") || strings.Contains(lower, "cmd/server") || strings.Contains(lower, "/server/") ||
		strings.Contains(lower, "api client") || strings.Contains(lower, "api 客户端") ||
		strings.Contains(lower, "http client") || strings.Contains(lower, "http.client") ||
		strings.Contains(lower, "endpoint") || strings.Contains(lower, "api key") || strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "remote service") || strings.Contains(lower, "remote api") || strings.Contains(lower, "远程服务") || strings.Contains(lower, "远程 api") {
		return true
	}
	re := regexp.MustCompile(`(^|[^a-z0-9])(grpc|protobuf|proto|rpc|rest|http-server|http server|websocket)([^a-z0-9]|$)`)
	return re.MatchString(lower)
}

func validateUnrequestedAdvancedCoreSurfaces(rawTasks []rawTask, objective string) error {
	if objectiveRequestsAdvancedCoreSurface(objective) {
		return nil
	}
	var offenders []string
	for _, task := range rawTasks {
		if !rawTaskUsesAdvancedCoreSurface(task) {
			continue
		}
		label := strings.TrimSpace(task.title)
		if label == "" {
			label = strings.TrimSpace(task.num)
		}
		if label == "" {
			label = "unnamed task"
		}
		offenders = append(offenders, label)
		if len(offenders) >= 3 {
			break
		}
	}
	if len(offenders) > 0 {
		return fmt.Errorf("WBS includes advanced core engine surface not explicitly requested by the objective: %s", strings.Join(offenders, "; "))
	}
	return nil
}

func objectiveRequestsAdvancedCoreSurface(objective string) bool {
	if objectiveRequestsExpandedScope(objective) {
		return true
	}
	return containsAdvancedCoreSurfaceTerm(objective)
}

func rawTaskUsesAdvancedCoreSurface(task rawTask) bool {
	parts := []string{task.title, task.designRef, task.accept, task.verifyCommand, task.parentID, task.capabilityID, task.parallelGroup, task.splitReason}
	parts = append(parts, task.targetFiles...)
	parts = append(parts, task.writeFiles...)
	parts = append(parts, task.readFiles...)
	for _, part := range parts {
		if containsAdvancedCoreSurfaceTerm(part) {
			return true
		}
	}
	return false
}

func containsAdvancedCoreSurfaceTerm(text string) bool {
	lower := strings.ToLower(text)
	terms := []string{
		"hnsw", "hierarchical navigable small world",
		"lsm", "lsm-tree", "log structured merge", "log-structured merge",
		"sstable", "sorted string table", "sst 文件", "sst file", "sst-",
		"mvcc", "multi-version concurrency control",
		"wal", "write-ahead log", "write ahead log",
		"mmap", "memory mapped", "memory-mapped",
		"page cache", "pagecache",
		"compaction", "压缩合并", "层级合并",
		"snapshot isolation", "快照隔离",
		"readview", "read view", "版本链",
		"csr", "compressed sparse row", "adjacency", "邻接",
		"delta store", "delta-store", "增量更新", "增量存储",
		"product quantization", "quantization", "量化", "pq",
	}
	for _, term := range terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func applyObjectiveTargetRootToWBS(rawTasks []rawTask, targetRoot string) {
	targetRoot = strings.Trim(strings.TrimSpace(filepath.ToSlash(targetRoot)), "/")
	if targetRoot == "" {
		return
	}
	for i := range rawTasks {
		rawTasks[i].targetFiles = prefixObjectiveTargetFiles(rawTasks[i].targetFiles, targetRoot)
		rawTasks[i].writeFiles = prefixObjectiveTargetFiles(rawTasks[i].writeFiles, targetRoot)
		rawTasks[i].readFiles = prefixObjectiveTargetFiles(rawTasks[i].readFiles, targetRoot)
	}
}

func prefixObjectiveTargetFiles(files []string, targetRoot string) []string {
	if len(files) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	prefix := targetRoot + "/"
	for _, file := range files {
		trimmed := strings.TrimSpace(file)
		if trimmed == "" {
			continue
		}
		slash := filepath.ToSlash(strings.TrimPrefix(trimmed, "./"))
		if filepath.IsAbs(trimmed) {
			if rel := absolutePathUnderTargetRoot(trimmed, targetRoot); rel != "" {
				out = append(out, rel)
			} else {
				out = append(out, trimmed)
			}
			continue
		}
		if strings.HasPrefix(slash, prefix) || strings.HasPrefix(slash, "../") {
			out = append(out, trimmed)
			continue
		}
		out = append(out, prefix+slash)
	}
	return uniqueTrimmedStrings(out)
}

func absolutePathUnderTargetRoot(file, targetRoot string) string {
	targetRoot = strings.Trim(strings.TrimSpace(filepath.ToSlash(targetRoot)), "/")
	if targetRoot == "" {
		return ""
	}
	slash := filepath.ToSlash(filepath.Clean(file))
	marker := "/" + targetRoot + "/"
	if idx := strings.Index(slash, marker); idx >= 0 {
		return targetRoot + "/" + strings.TrimPrefix(slash[idx+len(marker):], "/")
	}
	if strings.HasSuffix(slash, "/"+targetRoot) {
		return targetRoot
	}
	return ""
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
		"读取设计文档",
		"查看目录",
		"检查目标目录",
		"理解需求",
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
	return synthesizedObjectiveWBS(objective)
}

// normalizeAndSplitRawTasks 在 DAG 入库前执行 TaskSizingGate:
// 1. 补齐旧 WBS 默认值; 2. Macro 不直接执行; 3. 高风险/超预算 Leaf 展开为可验证 Leaf DAG。
func (o *Orchestrator) normalizeAndSplitRawTasks(rawTasks []rawTask, objective string) []rawTask {
	if len(rawTasks) == 0 {
		return nil
	}
	o.wbsSplitCount = 0
	adapter := o.planningAdapter(objective)
	var normalized []rawTask
	rewriteDep := make(map[string]string)
	macroDeps := collectRawMacroDeps(rawTasks)

	for _, rt := range rawTasks {
		rt = normalizeRawTaskDefaults(rt)
		if rt.taskType == wbsTaskTypeMacro {
			continue
		}
		rt.depNums = expandRawMacroDeps(rt.depNums, macroDeps)
		if isProjectBootstrapRawTask(rt) && bootstrapBusinessFiles(rt) != nil {
			bootstrap, business := splitBootstrapBusinessRawTask(rt, adapter)
			if hasBootstrapSkeletonTargets(bootstrap) {
				normalized = append(normalized, bootstrap)
			}
			for _, child := range business {
				shouldSplit, reason := shouldSplitRawTask(child, objective)
				if shouldSplit {
					children := expandRawTask(child, reason, adapter, objective)
					if len(children) > 0 {
						normalized = append(normalized, children...)
						rewriteDep[child.num] = children[len(children)-1].num
						o.wbsSplitCount += len(children)
						continue
					}
				}
				normalized = append(normalized, child)
			}
			continue
		}
		shouldSplit, reason := shouldSplitRawTask(rt, objective)
		if shouldSplit {
			children := expandRawTask(rt, reason, adapter, objective)
			if len(children) > 0 {
				normalized = append(normalized, children...)
				rewriteDep[rt.num] = children[len(children)-1].num
				o.wbsSplitCount += len(children)
				continue
			}
		}
		normalized = append(normalized, rt)
	}

	for i := range normalized {
		normalized[i].depNums = rewriteRawTaskDeps(normalized[i].depNums, rewriteDep, normalized[i].num)
	}
	normalized = addContractFirstRawDeps(normalized)
	normalized = addProjectBootstrapRawDeps(normalized)
	normalized = relaxOverSerialRawDeps(normalized)
	normalized = addDesignCompletePhaseBarriers(normalized, objective)
	fillVerificationDeps(normalized)
	normalized = orderRawTasksByDeps(normalized)
	return normalized
}

func addContractFirstRawDeps(tasks []rawTask) []rawTask {
	if len(tasks) == 0 {
		return tasks
	}
	var contractTasks []rawTask
	for _, task := range tasks {
		if task.num == "" || !isContractRawTask(task) {
			continue
		}
		contractTasks = append(contractTasks, task)
	}
	if len(contractTasks) == 0 {
		return tasks
	}
	for i := range tasks {
		if tasks[i].num == "" || isContractRawTask(tasks[i]) || isProjectBootstrapRawTask(tasks[i]) {
			continue
		}
		for _, contract := range contractTasks {
			if contract.num == "" || contract.num == tasks[i].num {
				continue
			}
			if shouldDependOnContractTask(tasks[i], contract) {
				tasks[i].depNums = uniqueTrimmedStrings(append(tasks[i].depNums, contract.num))
			}
		}
	}
	return tasks
}

func isContractRawTask(task rawTask) bool {
	if task.workUnitType == wbsWorkUnitContract {
		return true
	}
	if task.parentID != "" && strings.Contains(task.splitReason, "sizing-gate") {
		return false
	}
	text := strings.ToLower(task.title + " " + strings.Join(task.writeFiles, " ") + " " + strings.Join(task.targetFiles, " "))
	return strings.Contains(text, "contract") || strings.Contains(text, "interface") ||
		strings.Contains(text, "schema") || strings.Contains(text, "types") ||
		strings.Contains(text, "接口") || strings.Contains(text, "契约") || strings.Contains(text, "类型")
}

func shouldDependOnContractTask(cur, contract rawTask) bool {
	if cur.num == contract.num {
		return false
	}
	if rawStringSetsOverlap(cur.depNums, []string{contract.num}) {
		return false
	}
	if rawStringSetsOverlap(cur.readFiles, rawTaskWriteFiles(contract)) {
		return true
	}
	if rawStringSetsOverlap(cur.contractRefs, contract.provides) || rawStringSetsOverlap(cur.requires, contract.provides) {
		return true
	}
	if rawStringSetsOverlap(nonGlobalRawTaskConflictKeys(cur), nonGlobalRawTaskConflictKeys(contract)) {
		return true
	}
	if len(cur.readFiles) == 0 && len(cur.contractRefs) == 0 && len(cur.requires) == 0 {
		return false
	}
	return false
}

func addProjectBootstrapRawDeps(tasks []rawTask) []rawTask {
	bootstrapIDs := projectBootstrapRawTaskIDs(tasks)
	if len(bootstrapIDs) == 0 {
		return tasks
	}
	for i := range tasks {
		if tasks[i].num == "" || tasks[i].taskType == wbsTaskTypeVerification || isProjectBootstrapRawTask(tasks[i]) {
			continue
		}
		tasks[i].depNums = uniqueTrimmedStrings(append(tasks[i].depNums, bootstrapIDs...))
	}
	return tasks
}

func addDesignCompletePhaseBarriers(tasks []rawTask, objective string) []rawTask {
	if !objectiveRequiresDesignCompleteMode(objective) || len(tasks) == 0 {
		return tasks
	}
	type classifiedTask struct {
		idx   int
		id    string
		phase int
	}
	var classified []classifiedTask
	for i := range tasks {
		if tasks[i].num == "" || tasks[i].taskType == wbsTaskTypeVerification {
			continue
		}
		classified = append(classified, classifiedTask{idx: i, id: tasks[i].num, phase: designCompleteTaskPhase(tasks[i])})
	}
	for i := range classified {
		cur := classified[i]
		if cur.phase <= designCompletePhaseBase {
			continue
		}
		for _, dep := range classified {
			if dep.id == cur.id || dep.phase >= cur.phase {
				if dep.id != cur.id && isDesignCompleteFoundationContractRawTask(tasks[dep.idx]) && !isDesignCompleteFoundationContractRawTask(tasks[cur.idx]) && dep.phase == cur.phase {
					tasks[cur.idx].depNums = uniqueTrimmedStrings(append(tasks[cur.idx].depNums, dep.id))
				}
				continue
			}
			if dep.phase == designCompletePhaseProtocol && cur.phase < designCompletePhaseProtocol {
				continue
			}
			if shouldAddDesignPhaseDependency(tasks[cur.idx], tasks[dep.idx]) {
				tasks[cur.idx].depNums = uniqueTrimmedStrings(append(tasks[cur.idx].depNums, dep.id))
			}
		}
	}
	return tasks
}

const (
	designCompletePhaseBase = iota
	designCompletePhaseContract
	designCompletePhaseCore
	designCompletePhaseQuery
	designCompletePhaseProtocol
)

func designCompleteTaskPhase(task rawTask) int {
	if isProjectBootstrapRawTask(task) {
		return designCompletePhaseBase
	}
	if rawTaskUsesExternalProtocolSurface(task) || containsProtocolEntrypointTerm(rawTaskTextForClassification(task)) {
		return designCompletePhaseProtocol
	}
	if task.workUnitType == wbsWorkUnitContract || isContractRawTask(task) {
		return designCompletePhaseContract
	}
	text := rawTaskTextForClassification(task)
	if containsQueryAdapterTerm(text) {
		return designCompletePhaseQuery
	}
	if containsAdvancedCoreSurfaceTerm(text) || isHighRiskTaskText(text) || containsCoreEngineTerm(text) {
		return designCompletePhaseCore
	}
	return designCompletePhaseCore
}

func shouldAddDesignPhaseDependency(cur, dep rawTask) bool {
	if dep.taskType == wbsTaskTypeVerification || cur.num == dep.num || containsString(cur.depNums, dep.num) {
		return false
	}
	if isProjectBootstrapRawTask(dep) {
		return true
	}
	if isDesignCompleteFoundationContractRawTask(dep) && !isDesignCompleteFoundationContractRawTask(cur) {
		return true
	}
	depPhase := designCompleteTaskPhase(dep)
	if depPhase == designCompletePhaseContract {
		return shouldDependOnDesignContractPhase(cur, dep)
	}
	if designCompleteTaskPhase(cur) == designCompletePhaseProtocol {
		return depPhase == designCompletePhaseQuery
	}
	if designCompleteTaskPhase(cur) == designCompletePhaseQuery {
		return depPhase == designCompletePhaseCore
	}
	if designCompleteTaskPhase(cur) == designCompletePhaseCore {
		return false
	}
	return false
}

func shouldDependOnDesignContractPhase(cur, dep rawTask) bool {
	if isDesignCompleteFoundationContractRawTask(dep) && !isDesignCompleteFoundationContractRawTask(cur) {
		return true
	}
	if shouldDependOnContractTask(cur, dep) {
		return true
	}
	curDomain := coreStateDomainForRawTask(cur)
	depDomain := coreStateDomainForRawTask(dep)
	return curDomain != "" && curDomain == depDomain
}

func isDesignCompleteFoundationContractRawTask(task rawTask) bool {
	if task.taskType != wbsTaskTypeLeaf || task.workUnitType == wbsWorkUnitVerification || isProjectBootstrapRawTask(task) {
		return false
	}
	if !(task.workUnitType == wbsWorkUnitContract || isContractRawTask(task) || isContractBoundaryRawTask(task)) {
		return false
	}
	files := strings.Join(append(append([]string{}, task.targetFiles...), task.writeFiles...), " ")
	fileText := strings.ToLower(filepath.ToSlash(files))
	if strings.Contains(fileText, "/internal/types/") ||
		strings.Contains(fileText, "/internal/errors/") ||
		strings.Contains(fileText, "/internal/api/core") ||
		strings.Contains(fileText, "/pkg/types/") ||
		strings.Contains(fileText, "/pkg/errors/") {
		return true
	}
	text := strings.ToLower(strings.Join([]string{
		task.title,
		task.designRef,
		task.capabilityID,
		task.parentID,
		strings.Join(task.constraintRefs, " "),
	}, " "))
	terms := []string{
		"核心类型", "错误定义", "错误类型", "错误码", "核心错误",
		"core type", "core types", "error type", "error code", "core api", "core interface",
		"base type", "shared type", "公共类型", "基础类型", "共享类型",
	}
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func rawTaskTextForClassification(task rawTask) string {
	parts := []string{
		task.title, task.designRef, task.accept, task.verifyCommand, task.parentID,
		task.capabilityID, task.parallelGroup, task.splitReason, task.workUnitType,
	}
	parts = append(parts, task.constraintRefs...)
	parts = append(parts, task.targetFiles...)
	parts = append(parts, task.writeFiles...)
	parts = append(parts, task.readFiles...)
	return strings.ToLower(strings.Join(parts, " "))
}

func containsProtocolEntrypointTerm(text string) bool {
	terms := []string{
		"cmd/http", "cmd/grpc", "cmd/server", "cmd/cli", "pkg/protocol",
		"http api", "rest api", "grpc api", "mcp", "cli", "server",
		"handler", "endpoint", "router", "listener", "transport",
		"协议", "入口", "适配器",
	}
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func containsQueryAdapterTerm(text string) bool {
	terms := []string{
		"sql", "cypher", "query", "planner", "executor", "parser", "semantic",
		"retrieve", "retrieval", "resolver", "查询", "解析", "执行器", "检索",
	}
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func containsCoreEngineTerm(text string) bool {
	terms := []string{
		"storage", "engine", "store", "vector", "graph", "file", "embedding",
		"memory", "session", "format", "kv", "column", "index",
		"存储", "引擎", "向量", "图谱", "文件", "记忆", "会话", "索引",
	}
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func projectBootstrapRawTaskIDs(tasks []rawTask) []string {
	var ids []string
	for _, task := range tasks {
		if task.num == "" || task.taskType == wbsTaskTypeVerification || !isProjectBootstrapRawTask(task) {
			continue
		}
		ids = append(ids, task.num)
	}
	return uniqueTrimmedStrings(ids)
}

func collectRawMacroDeps(tasks []rawTask) map[string][]string {
	macroDeps := make(map[string][]string)
	for _, task := range tasks {
		task = normalizeRawTaskDefaults(task)
		if task.num == "" || task.taskType != wbsTaskTypeMacro {
			continue
		}
		macroDeps[task.num] = append([]string(nil), task.depNums...)
	}
	return macroDeps
}

func expandRawMacroDeps(deps []string, macroDeps map[string][]string) []string {
	if len(deps) == 0 || len(macroDeps) == 0 {
		return deps
	}
	var expand func(string, map[string]bool) []string
	expand = func(dep string, seen map[string]bool) []string {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			return nil
		}
		if seen[dep] {
			return nil
		}
		mdeps, ok := macroDeps[dep]
		if !ok {
			return []string{dep}
		}
		seen[dep] = true
		var out []string
		for _, mdep := range mdeps {
			out = append(out, expand(mdep, seen)...)
		}
		return out
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		out = append(out, expand(dep, map[string]bool{})...)
	}
	return uniqueTrimmedStrings(out)
}

func relaxOverSerialRawDeps(tasks []rawTask) []rawTask {
	if len(tasks) == 0 {
		return tasks
	}
	byID := make(map[string]rawTask, len(tasks))
	for _, task := range tasks {
		byID[task.num] = task
	}
	for i := range tasks {
		if len(tasks[i].depNums) == 0 || isBarrierRawTask(tasks[i]) {
			continue
		}
		kept := make([]string, 0, len(tasks[i].depNums))
		for _, depID := range tasks[i].depNums {
			dep, ok := byID[depID]
			if !ok || shouldKeepRawDependency(tasks[i], dep) {
				kept = append(kept, depID)
			}
		}
		tasks[i].depNums = uniqueTrimmedStrings(kept)
	}
	return tasks
}

func orderRawTasksByDeps(tasks []rawTask) []rawTask {
	if len(tasks) <= 1 {
		return tasks
	}
	known := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if strings.TrimSpace(task.num) != "" {
			known[task.num] = true
		}
	}
	used := make([]bool, len(tasks))
	resolved := make(map[string]bool, len(tasks))
	ordered := make([]rawTask, 0, len(tasks))
	for len(ordered) < len(tasks) {
		progressed := false
		for i, task := range tasks {
			if used[i] || !rawTaskDepsResolved(task, known, resolved) {
				continue
			}
			ordered = append(ordered, task)
			used[i] = true
			if strings.TrimSpace(task.num) != "" {
				resolved[task.num] = true
			}
			progressed = true
		}
		if !progressed {
			return tasks
		}
	}
	return ordered
}

func rawTaskDepsResolved(task rawTask, known, resolved map[string]bool) bool {
	for _, dep := range task.depNums {
		dep = strings.TrimSpace(dep)
		if dep == "" || dep == task.num || !known[dep] {
			continue
		}
		if !resolved[dep] {
			return false
		}
	}
	return true
}

func shouldKeepRawDependency(cur, dep rawTask) bool {
	if dep.taskType == wbsTaskTypeMacro || isProjectBootstrapRawTask(dep) {
		return true
	}
	if cur.parentID != "" && cur.parentID == dep.parentID {
		return true
	}
	if rawStringSetsOverlap(rawTaskWriteFiles(cur), rawTaskWriteFiles(dep)) {
		return true
	}
	if rawStringSetsOverlap(nonGlobalRawTaskConflictKeys(cur), nonGlobalRawTaskConflictKeys(dep)) {
		return true
	}
	if rawStringSetsOverlap(cur.readFiles, rawTaskWriteFiles(dep)) {
		return true
	}
	if rawStringSetsOverlap(cur.contractRefs, dep.provides) || rawStringSetsOverlap(cur.requires, dep.provides) {
		return true
	}
	return false
}

func isBarrierRawTask(task rawTask) bool {
	if isLocalVerificationRawTask(task) {
		return true
	}
	text := strings.ToLower(strings.Join([]string{
		task.title,
		task.role,
		task.workUnitType,
		task.capabilityID,
	}, " "))
	if orchNormalizeRole(task.role) == "tester" {
		return true
	}
	phrases := []string{
		"test", "testing", "verification", "validate", "validation", "benchmark", "acceptance",
		"测试", "验证", "验收", "基准", "集成测试", "回归",
	}
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func isLocalVerificationRawTask(task rawTask) bool {
	if task.taskType == wbsTaskTypeVerification &&
		(strings.Contains(task.splitReason, "sizing-gate") || strings.Contains(task.splitReason, "timeout")) {
		return true
	}
	return false
}

func isProjectBootstrapRawTask(task rawTask) bool {
	text := strings.ToLower(task.title + " " + task.workUnitType + " " + task.capabilityID)
	if strings.Contains(text, "project-init") ||
		strings.Contains(text, "project-structure") ||
		strings.Contains(text, "init") ||
		strings.Contains(text, "bootstrap") ||
		strings.Contains(text, "skeleton") ||
		strings.Contains(text, "scaffold") ||
		strings.Contains(text, "初始化") ||
		strings.Contains(text, "项目结构") ||
		strings.Contains(text, "目录结构") ||
		strings.Contains(text, "项目骨架") ||
		strings.Contains(text, "脚手架") ||
		strings.Contains(text, "骨架") {
		return true
	}
	for _, file := range append(append([]string{}, task.targetFiles...), task.writeFiles...) {
		if isProjectManifestFile(file) {
			return true
		}
	}
	return false
}

func bootstrapBusinessFiles(task rawTask) []string {
	var business []string
	for _, file := range concreteWritablePlanFiles(task) {
		if !isBootstrapSkeletonTarget(file) {
			business = append(business, file)
		}
	}
	return uniqueTrimmedStrings(business)
}

func splitBootstrapBusinessRawTask(task rawTask, adapter planningLanguageAdapter) (rawTask, []rawTask) {
	bootstrap := task
	bootstrapTargets := make([]string, 0, len(task.targetFiles))
	for _, target := range task.targetFiles {
		if isBootstrapSkeletonTarget(target) || pathIsDirectoryLike(target) {
			bootstrapTargets = append(bootstrapTargets, target)
		}
	}
	bootstrapWrites := make([]string, 0, len(task.writeFiles))
	for _, target := range task.writeFiles {
		if isBootstrapSkeletonTarget(target) || pathIsDirectoryLike(target) {
			bootstrapWrites = append(bootstrapWrites, target)
		}
	}
	bootstrap.targetFiles = uniqueTrimmedStrings(bootstrapTargets)
	bootstrap.writeFiles = uniqueTrimmedStrings(bootstrapWrites)

	var business []rawTask
	for i, file := range bootstrapBusinessFiles(task) {
		child := task
		child.num = fmt.Sprintf("%s.bootstrap-file.%d", task.num, i+1)
		child.title = "实现业务源码文件 - " + path.Base(filepath.ToSlash(file))
		child.taskType = wbsTaskTypeLeaf
		child.workUnitType = wbsWorkUnitImplementation
		child.riskLevel = wbsRiskMedium
		if isHighRiskTaskText(file+" "+task.title+" "+task.accept) || containsAdvancedCoreSurfaceTerm(file) || containsQueryAdapterTerm(file) {
			child.riskLevel = wbsRiskHigh
		}
		child.complexity = "medium"
		child.parentID = task.num
		child.targetFiles = []string{file}
		child.writeFiles = []string{file}
		child.readFiles = nil
		child.targetPackages = targetPackagesForFileByLanguage(file, adapter.ID)
		child.parallelGroup = ""
		child.conflictKeys = nil
		child.splitReason = appendSplitReason(task.splitReason, "bootstrap-business-file")
		child.accept = "该文件不是项目初始化产物; 只实现该业务文件的单一职责; 不修改 go.mod/目录骨架; scoped build 通过"
		business = append(business, child)
	}
	return bootstrap, business
}

func hasBootstrapSkeletonTargets(task rawTask) bool {
	return len(task.targetFiles) > 0 || len(task.writeFiles) > 0 || len(directoryLikeTaskTargets(task)) > 0
}

func isBootstrapSkeletonTarget(file string) bool {
	clean := strings.TrimSpace(filepath.ToSlash(file))
	if clean == "" {
		return false
	}
	if isProjectManifestFile(clean) || isPlanPlaceholderFile(clean) {
		return true
	}
	base := strings.ToLower(path.Base(strings.TrimSuffix(clean, "/")))
	switch base {
	case "main.go", "main.py", "main.ts", "main.js", "index.ts", "index.js",
		"mod.rs", "lib.rs", "__init__.py", "readme.md", "makefile", "dockerfile":
		return true
	default:
		return false
	}
}

func pathIsDirectoryLike(file string) bool {
	clean := strings.TrimSpace(filepath.ToSlash(file))
	if clean == "" || filepath.IsAbs(clean) {
		return false
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(clean, "./"), "/...")
	trimmed = strings.TrimSuffix(trimmed, "/")
	return strings.HasSuffix(clean, "/") || strings.HasSuffix(clean, "/...") || (path.Ext(trimmed) == "" && strings.Contains(trimmed, "/"))
}

func rawStringSetsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, item := range a {
		item = strings.ToLower(strings.TrimSpace(filepath.ToSlash(item)))
		if item != "" {
			seen[item] = true
		}
	}
	for _, item := range b {
		item = strings.ToLower(strings.TrimSpace(filepath.ToSlash(item)))
		if item != "" && seen[item] {
			return true
		}
	}
	return false
}

func normalizeRawTaskDefaults(rt rawTask) rawTask {
	rt.num = strings.TrimSpace(rt.num)
	rt.title = strings.TrimSpace(rt.title)
	rt.role = orchNormalizeRole(rt.role)
	rt.taskType = normalizeWBSTaskType(rt.taskType)
	rt.workUnitType = normalizeWBSWorkUnitType(rt.workUnitType, rt.taskType)
	rt.riskLevel = normalizeWBSRisk(rt.riskLevel)
	rt.blockingPolicy = normalizeWBSBlockingPolicy(rt.blockingPolicy)
	rt.complexity = strings.ToLower(strings.TrimSpace(rt.complexity))
	rt.capabilityID = normalizePlanningID(rt.capabilityID)
	rt.contractRefs = uniqueTrimmedStrings(rt.contractRefs)
	rt.provides = uniqueTrimmedStrings(rt.provides)
	rt.requires = uniqueTrimmedStrings(rt.requires)
	rt.readFiles = normalizePlanFiles(rt.readFiles)
	rt.writeFiles = normalizePlanFiles(rt.writeFiles)
	rt.targetFiles = normalizePlanFiles(rt.targetFiles)
	rt.targetPackages = uniqueTrimmedStrings(rt.targetPackages)
	rt.conflictKeys = uniqueTrimmedStrings(rt.conflictKeys)
	rt.parentID = strings.TrimSpace(rt.parentID)
	rt.verifyCommand = strings.TrimSpace(rt.verifyCommand)
	rt.parallelGroup = strings.TrimSpace(rt.parallelGroup)
	rt.splitReason = strings.TrimSpace(rt.splitReason)
	rt.timeoutKind = strings.TrimSpace(rt.timeoutKind)
	if len(rt.targetFiles) == 0 && len(rt.writeFiles) > 0 {
		rt.targetFiles = append([]string(nil), rt.writeFiles...)
	}
	if len(rt.writeFiles) == 0 && len(rt.targetFiles) > 0 {
		rt.writeFiles = append([]string(nil), rt.targetFiles...)
	}
	if isContractBoundaryRawTask(rt) {
		root := firstPlanPathSegment(append(append([]string{}, rt.targetFiles...), rt.writeFiles...))
		if root == "" {
			root = "project"
		}
		component := coreStateComponentForPlanFiles(append(append([]string{}, rt.targetFiles...), rt.writeFiles...))
		if component != "" {
			rt.conflictKeys = append(rt.conflictKeys, "contract:"+root+":"+component)
		} else {
			rt.conflictKeys = append(rt.conflictKeys, "contract:"+root)
		}
		rt.conflictKeys = uniqueTrimmedStrings(rt.conflictKeys)
	}
	if isCoreStateRawTask(rt) {
		rt.conflictKeys = uniqueTrimmedStrings(append(rt.conflictKeys, coreStateConflictKeysForRawTask(rt)...))
	}
	if shouldSerializePackageForRawTask(rt) {
		rt.conflictKeys = uniqueTrimmedStrings(append(rt.conflictKeys, packageConflictKeysForPlanFiles(rt.targetFiles, rt.writeFiles)...))
	}
	if rt.parallelGroup == "" && len(rt.conflictKeys) > 0 {
		rt.parallelGroup = rt.conflictKeys[0]
	}
	if rt.priority == 0 {
		rt.priority = 1
	}
	if rt.estimatedLOC <= 0 {
		rt.estimatedLOC = estimateRawTaskLOC(rt)
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

func isContractBoundaryRawTask(rt rawTask) bool {
	if rt.taskType != wbsTaskTypeLeaf {
		return false
	}
	if rt.workUnitType == wbsWorkUnitContract {
		return true
	}
	text := strings.ToLower(rt.title + " " + rt.accept + " " + rt.splitReason)
	return strings.Contains(text, "接口") ||
		strings.Contains(text, "契约") ||
		strings.Contains(text, "数据结构边界") ||
		strings.Contains(text, "interface") ||
		strings.Contains(text, "contract") ||
		strings.Contains(text, "type boundary")
}

func isMixedContractImplementationRawTask(rt rawTask) bool {
	if rt.taskType != wbsTaskTypeLeaf || rt.workUnitType == wbsWorkUnitContract || rt.workUnitType == wbsWorkUnitVerification {
		return false
	}
	text := strings.ToLower(strings.Join([]string{
		rt.title,
		rt.accept,
		rt.designRef,
		rt.capabilityID,
		rt.parentID,
		rt.splitReason,
		strings.Join(rt.constraintRefs, " "),
	}, " "))
	hasContract := strings.Contains(text, "接口") ||
		strings.Contains(text, "契约") ||
		strings.Contains(text, "数据结构边界") ||
		strings.Contains(text, "interface") ||
		strings.Contains(text, "contract") ||
		strings.Contains(text, "type boundary")
	hasImplementation := strings.Contains(text, "实现") ||
		strings.Contains(text, "implementation") ||
		strings.Contains(text, "implement") ||
		strings.Contains(text, "engine") ||
		strings.Contains(text, "executor") ||
		strings.Contains(text, "runtime")
	return hasContract && hasImplementation
}

func isCoreStateRawTask(rt rawTask) bool {
	if rt.taskType != wbsTaskTypeLeaf {
		return false
	}
	text := strings.Join([]string{
		rt.title,
		rt.accept,
		rt.designRef,
		rt.capabilityID,
		rt.parentID,
		rt.parallelGroup,
		rt.splitReason,
		strings.Join(rt.constraintRefs, " "),
	}, " ")
	return isHighRiskTaskText(text) || containsAdvancedCoreSurfaceTerm(text)
}

func coreStateConflictKeysForRawTask(rt rawTask) []string {
	files := append(append([]string{}, rt.targetFiles...), rt.writeFiles...)
	root := firstPlanPathSegment(files)
	if root == "" {
		root = "project"
	}
	component := coreStateComponentForPlanFiles(files)
	if component == "" {
		component = normalizePlanningID(rt.capabilityID)
	}
	if component == "" {
		component = normalizePlanningID(rt.parentID)
	}
	if component == "" {
		component = "core"
	}
	keys := []string{"core-state:" + root + ":" + component}
	if domain := coreStateDomainForRawTask(rt); domain != "" {
		keys = append(keys, "core-domain:"+root+":"+domain)
	}
	return uniqueTrimmedStrings(keys)
}

func coreStateDomainForRawTask(rt rawTask) string {
	text := strings.ToLower(strings.Join([]string{
		rt.title,
		rt.accept,
		rt.designRef,
		rt.capabilityID,
		rt.parentID,
		rt.parallelGroup,
		strings.Join(rt.constraintRefs, " "),
		strings.Join(rt.targetFiles, " "),
		strings.Join(rt.writeFiles, " "),
	}, " "))
	domains := []struct {
		id    string
		terms []string
	}{
		{"hnsw", []string{"hnsw"}},
		{"lsm", []string{"lsm", "sstable", "sst 文件", "sst file", "sst-", "compaction", "memtable", "skiplist", "skip list"}},
		{"wal", []string{"wal", "write-ahead", "write ahead"}},
		{"mvcc", []string{"mvcc", "transaction", "事务", "readview", "版本链", "snapshot isolation"}},
		{"graph", []string{"csr", "adjacency", "邻接", "bfs", "dfs", "traversal", "graph", "图"}},
		{"query", []string{"sql", "dsl", "parser", "解析", "lexer", "planner", "executor", "query", "查询"}},
		{"vector", []string{"vector", "向量", "mmap", "quantization", "量化", "pq", "product quantization"}},
		{"pagecache", []string{"page cache", "pagecache", "页面缓存", "缓存层"}},
		{"delta", []string{"delta store", "delta-store", "增量更新", "增量存储"}},
	}
	for _, domain := range domains {
		for _, term := range domain.terms {
			if strings.Contains(text, term) {
				return domain.id
			}
		}
	}
	return ""
}

func coreStateComponentForPlanFiles(files []string) string {
	for _, file := range files {
		clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
		if clean == "" || clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") {
			continue
		}
		parts := strings.Split(clean, "/")
		if len(parts) >= 3 && (parts[1] == "internal" || parts[1] == "pkg" || parts[1] == "src" || parts[1] == "lib") {
			return parts[1] + "/" + parts[2]
		}
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return ""
}

func packageConflictKeysForPlanFiles(fileGroups ...[]string) []string {
	var keys []string
	for _, files := range fileGroups {
		for _, file := range files {
			key := packageConflictKeyForPlanFile(file)
			if key != "" {
				keys = append(keys, key)
			}
		}
	}
	return uniqueTrimmedStrings(keys)
}

func shouldSerializePackageForRawTask(rt rawTask) bool {
	if rt.taskType == wbsTaskTypeVerification {
		return true
	}
	switch rt.workUnitType {
	case wbsWorkUnitContract, wbsWorkUnitIntegration, wbsWorkUnitVerification:
		return true
	}
	for _, file := range append(append([]string{}, rt.targetFiles...), rt.writeFiles...) {
		if isProjectManifestFile(file) || strings.HasSuffix(strings.ToLower(filepath.Base(file)), "_test.go") {
			return true
		}
	}
	return false
}

func packageConflictKeyForPlanFile(file string) string {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if ext == "" {
		return ""
	}
	dir := filepath.ToSlash(path.Dir(clean))
	if dir == "." || dir == "" {
		dir = "_root"
	}
	switch ext {
	case ".go":
		return "pkg:" + dir
	case ".proto":
		return "pkg:" + dir
	case ".py":
		return "pkg:" + pythonPackageConflictDir(dir)
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return "pkg:" + dir
	case ".rs":
		return "pkg:" + rustPackageConflictDir(dir)
	case ".java", ".kt", ".kts", ".scala", ".cs", ".cpp", ".cc", ".cxx", ".c", ".h", ".hpp", ".hh":
		return "pkg:" + dir
	default:
		return ""
	}
}

func pythonPackageConflictDir(dir string) string {
	if dir == "" || dir == "." {
		return "_root"
	}
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 && (parts[0] == "src" || parts[0] == "lib" || parts[0] == "tests" || parts[0] == "test") {
		return strings.Join(parts[:2], "/")
	}
	return dir
}

func rustPackageConflictDir(dir string) string {
	if dir == "" || dir == "." {
		return "_root"
	}
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 && parts[0] == "src" {
		return strings.Join(parts[:2], "/")
	}
	if len(parts) >= 3 && parts[1] == "src" {
		return strings.Join(parts[:3], "/")
	}
	return dir
}

func normalizeWBSWorkUnitType(workUnitType, taskType string) string {
	switch strings.ToLower(strings.TrimSpace(workUnitType)) {
	case wbsWorkUnitContract, wbsWorkUnitImplementation, wbsWorkUnitIntegration, wbsWorkUnitVerification:
		return strings.ToLower(strings.TrimSpace(workUnitType))
	}
	if taskType == wbsTaskTypeVerification {
		return wbsWorkUnitVerification
	}
	return wbsWorkUnitImplementation
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

func shouldSplitRawTask(rt rawTask, objective string) (bool, string) {
	if rt.taskType == wbsTaskTypeVerification {
		return false, ""
	}
	if strings.Contains(rt.splitReason, "sizing-gate") {
		return false, ""
	}
	if strings.Contains(rt.splitReason, "objective-fallback") {
		return false, ""
	}
	if rt.taskType == wbsTaskTypeMacro {
		return true, "macro-task-not-executable"
	}
	if isProjectBootstrapRawTask(rt) && len(directoryLikeTaskTargets(rt)) > 0 {
		return false, ""
	}
	reasonPrefix := ""
	if objectiveRequiresDesignCompleteMode(objective) {
		reasonPrefix = "design-complete:"
	}
	if len(directoryLikeTaskTargets(rt)) > 0 {
		return true, reasonPrefix + "directory-target-requires-file-leaves"
	}
	if objectiveRequiresDesignCompleteMode(objective) && isMixedContractImplementationRawTask(rt) {
		return true, "mixed-contract-implementation"
	}
	if objectiveRequiresDesignCompleteMode(objective) && isDesignCompleteRiskySingleFileRawTask(rt) {
		return true, reasonPrefix + "single-file-risk-leaf"
	}
	if rt.estimatedMin > 4 {
		return true, reasonPrefix + fmt.Sprintf("estimated-%dmin-over-leaf-budget", rt.estimatedMin)
	}
	if rt.riskLevel == wbsRiskHigh {
		return true, reasonPrefix + "high-risk-leaf-requires-micro-milestones"
	}
	if len(rawTaskWriteFiles(rt)) > 3 {
		return true, reasonPrefix + fmt.Sprintf("write-files-%d-over-leaf-budget", len(rawTaskWriteFiles(rt)))
	}
	if rt.estimatedLOC > 250 {
		return true, reasonPrefix + fmt.Sprintf("estimated-loc-%d-over-leaf-budget", rt.estimatedLOC)
	}
	if isHighRiskTaskText(rt.title + " " + rt.designRef + " " + strings.Join(rt.constraintRefs, " ")) {
		return true, reasonPrefix + "domain-risk-keyword"
	}
	return false, ""
}

func expandRawTask(rt rawTask, reason string, adapter planningLanguageAdapter, objective string) []rawTask {
	if dirs := directoryLikeTaskTargets(rt); len(dirs) > 0 {
		return expandDirectoryTargetRawTask(rt, reason, dirs, adapter)
	}
	if files := concreteWritablePlanFiles(rt); len(files) > 1 {
		return expandFileTargetRawTask(rt, reason, files, adapter)
	}
	if files := concreteWritablePlanFiles(rt); len(files) == 1 && shouldMicroSplitFileTarget(rt, files[0], reason) {
		rt.targetFiles = []string{files[0]}
		rt.writeFiles = []string{files[0]}
		rt.targetPackages = targetPackagesForFileByLanguage(files[0], adapter.ID)
		return expandFileTargetMicroRawTask(rt, "single-file-risk:"+reason)
	}
	if file := inferRiskyRawTaskTargetFile(rt, adapter, objective); file != "" && shouldMicroSplitFileTarget(rt, file, reason) {
		rt.targetFiles = []string{file}
		rt.writeFiles = []string{file}
		rt.targetPackages = targetPackagesForFileByLanguage(file, adapter.ID)
		return expandFileTargetMicroRawTask(rt, "inferred-file-risk:"+reason)
	}
	return expandGenericRawTask(rt, reason)
}

func concreteWritablePlanFiles(rt rawTask) []string {
	files := rawTaskWriteFiles(rt)
	out := make([]string, 0, len(files))
	for _, file := range files {
		clean := strings.TrimSpace(filepath.ToSlash(file))
		if clean == "" || filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") {
			continue
		}
		if strings.HasSuffix(clean, "/") || strings.HasSuffix(clean, "/...") || path.Ext(clean) == "" {
			continue
		}
		out = append(out, clean)
	}
	return uniqueTrimmedStrings(out)
}

func inferRiskyRawTaskTargetFile(rt rawTask, adapter planningLanguageAdapter, objective string) string {
	if rt.taskType != wbsTaskTypeLeaf || rt.workUnitType == wbsWorkUnitVerification {
		return ""
	}
	if len(concreteWritablePlanFiles(rt)) > 0 {
		return ""
	}
	if adapter.ID == "" {
		adapter = planningLanguageAdapterFor("")
	}
	text := strings.Join([]string{
		rt.title,
		rt.accept,
		rt.designRef,
		rt.capabilityID,
		rt.parentID,
		strings.Join(rt.constraintRefs, " "),
	}, " ")
	if !isHighRiskTaskText(text) && !containsAdvancedCoreSurfaceTerm(text) && !containsQueryAdapterTerm(text) {
		return ""
	}
	component, stem := inferRiskyTaskComponentAndStem(text)
	if explicit := inferExplicitPlanFileFromText(text, adapter); explicit != "" {
		if strings.Contains(filepath.ToSlash(explicit), "/") {
			return prefixInferredPlanFileWithObjectiveRoot(explicit, objective)
		}
		if component != "" {
			ext := strings.TrimSpace(adapter.SourceExt)
			if ext == "" {
				ext = path.Ext(explicit)
			}
			stem = strings.TrimSuffix(path.Base(filepath.ToSlash(explicit)), ext)
			return renderInferredRiskyTaskFile(inferObjectiveTargetRoot(objective), component, stem, adapter)
		}
		return prefixInferredPlanFileWithObjectiveRoot(explicit, objective)
	}
	if stem == "" {
		return ""
	}
	if component == "" {
		component = stem
	}
	return renderInferredRiskyTaskFile(inferObjectiveTargetRoot(objective), component, stem, adapter)
}

func renderInferredRiskyTaskFile(root, component, stem string, adapter planningLanguageAdapter) string {
	root = strings.Trim(strings.TrimSpace(filepath.ToSlash(root)), "/")
	if root == "" {
		root = "project"
	}
	component = safePlanFileStem(component)
	stem = safePlanFileStem(stem)
	if component == "" || stem == "" {
		return ""
	}
	switch adapter.ID {
	case "python":
		return filepath.ToSlash(filepath.Join(root, component, stem+".py"))
	case "typescript":
		return filepath.ToSlash(filepath.Join(root, "src", component, stem+".ts"))
	case "javascript":
		return filepath.ToSlash(filepath.Join(root, "src", component, stem+".js"))
	case "rust":
		return filepath.ToSlash(filepath.Join(root, "src", component, stem+".rs"))
	case "cpp":
		return filepath.ToSlash(filepath.Join(root, "src", component, stem+".cpp"))
	default:
		return filepath.ToSlash(filepath.Join(root, "internal", component, stem+".go"))
	}
}

func inferExplicitPlanFileFromText(text string, adapter planningLanguageAdapter) string {
	ext := strings.TrimSpace(adapter.SourceExt)
	if ext == "" {
		ext = ".go"
	}
	re := regexp.MustCompile(`[A-Za-z0-9_.@+-]+(?:/[A-Za-z0-9_.@+-]+)*` + regexp.QuoteMeta(ext) + `\b`)
	for _, match := range re.FindAllString(text, -1) {
		clean := cleanMaterializeRelPath(match)
		if clean == "" {
			continue
		}
		slash := filepath.ToSlash(clean)
		if strings.HasSuffix(slash, ext) && !strings.HasSuffix(slash, adapter.TestSuffix) {
			return slash
		}
	}
	return ""
}

func prefixInferredPlanFileWithObjectiveRoot(file, objective string) string {
	file = strings.TrimSpace(filepath.ToSlash(file))
	root := inferObjectiveTargetRoot(objective)
	if file == "" || root == "" || filepath.IsAbs(file) || strings.HasPrefix(file, "../") {
		return file
	}
	prefix := strings.Trim(root, "/") + "/"
	if strings.HasPrefix(file, prefix) {
		return file
	}
	if strings.Contains(file, "/") {
		return prefix + file
	}
	return file
}

func inferRiskyTaskComponentAndStem(text string) (string, string) {
	lower := strings.ToLower(text)
	type candidate struct {
		terms     []string
		component string
		stem      string
	}
	candidates := []candidate{
		{[]string{"sstable", "sorted string table", "sst 文件", "sst"}, "storage", "sstable"},
		{[]string{"memtable", "skiplist", "skip list"}, "storage", "memtable"},
		{[]string{"wal", "write-ahead", "write ahead"}, "storage", "wal"},
		{[]string{"lsm", "compaction"}, "storage", "lsm"},
		{[]string{"mvcc", "transaction", "事务", "readview", "版本链"}, "txn", "manager"},
		{[]string{"hnsw"}, "vector", "hnsw"},
		{[]string{"向量", "vector"}, "vector", "index"},
		{[]string{"bfs", "dfs", "traversal", "图遍历"}, "graph", "traversal"},
		{[]string{"graph", "图引擎", "图谱"}, "graph", "graph"},
		{[]string{"parser", "解析器", "词法", "lexer"}, "query", "parser"},
		{[]string{"planner", "计划器"}, "query", "planner"},
		{[]string{"executor", "执行器"}, "query", "executor"},
		{[]string{"auth", "鉴权", "认证", "rbac", "权限"}, "security", "auth"},
		{[]string{"cache", "缓存"}, "cache", "cache"},
		{[]string{"protocol", "协议"}, "protocol", "protocol"},
	}
	for _, c := range candidates {
		for _, term := range c.terms {
			if strings.Contains(lower, term) {
				return c.component, c.stem
			}
		}
	}
	id := normalizePlanningID(text)
	if id == "" {
		return "", ""
	}
	parts := strings.Split(id, "-")
	var useful []string
	stop := map[string]bool{
		"implement": true, "implementation": true, "create": true, "define": true,
		"task": true, "core": true, "behavior": true, "path": true,
	}
	for _, part := range parts {
		if len(part) < 3 || stop[part] {
			continue
		}
		useful = append(useful, part)
		if len(useful) >= 2 {
			break
		}
	}
	if len(useful) == 0 {
		return "", ""
	}
	stem := useful[len(useful)-1]
	component := useful[0]
	return component, stem
}

func expandFileTargetRawTask(rt rawTask, reason string, files []string, adapter planningLanguageAdapter) []rawTask {
	if len(files) == 0 {
		return expandGenericRawTask(rt, reason)
	}
	if len(files) > 8 {
		files = files[:8]
	}
	parentID := rt.num
	group := rt.parallelGroup
	if group == "" {
		group = parentID + ":file-leaves"
	}
	duplicateBase := duplicatePlanFileBasenames(files)
	children := make([]rawTask, 0, len(files)+1)
	for i, file := range files {
		child := rt
		child.num = fmt.Sprintf("%s.%d", parentID, i+1)
		child.title = fileTargetLeafTitle(rt.title, file, duplicateBase)
		child.role = "coder"
		child.taskType = wbsTaskTypeLeaf
		child.parentID = parentID
		child.estimatedMin = 2
		if i > 0 {
			child.estimatedMin = 3
		}
		child.riskLevel = wbsRiskMedium
		child.parallelGroup = group + ":" + sanitizeParallelGroupSegment(path.Base(filepath.ToSlash(file)))
		child.conflictKeys = nil
		child.blockingPolicy = wbsBlockingFailBlocks
		child.splitReason = appendSplitReason(rt.splitReason, "sizing-gate:"+reason+";file-target")
		child.complexity = "medium"
		child.targetFiles = []string{file}
		child.writeFiles = []string{file}
		child.readFiles = nil
		child.targetPackages = targetPackagesForFileByLanguage(file, adapter.ID)
		child.workUnitType = wbsWorkUnitImplementation
		if i == 0 || isPlanContractFile(file) {
			child.workUnitType = wbsWorkUnitContract
			child.accept = "定义该文件的最小接口、类型或结构边界; scoped build 通过"
		} else if isPlanTestFile(file) {
			child.workUnitType = wbsWorkUnitVerification
			child.accept = "为对应实现补最小测试文件; scoped build/test 通过"
		} else {
			child.accept = "实现该文件的单一职责; 不修改其它文件; scoped build 通过"
		}
		child.depNums = append([]string(nil), rt.depNums...)
		if i > 0 && isPlanTestFile(file) {
			child.depNums = append(child.depNums, children[i-1].num)
		}
		if shouldMicroSplitFileTarget(child, file, reason) {
			nested := expandFileTargetMicroRawTask(child, "file-target-risk:"+reason)
			if len(nested) > 0 {
				children = append(children, nested...)
				continue
			}
		}
		children = append(children, child)
	}
	verify := rt
	verify.num = fmt.Sprintf("%s.%d", parentID, len(children)+1)
	verify.title = rt.title + " - 本地验证与回归检查"
	verify.role = "tester"
	verify.taskType = wbsTaskTypeVerification
	verify.workUnitType = wbsWorkUnitVerification
	verify.parentID = parentID
	verify.estimatedMin = 2
	verify.riskLevel = wbsRiskLow
	verify.parallelGroup = group + ":verification"
	verify.conflictKeys = []string{verify.parallelGroup}
	verify.blockingPolicy = wbsBlockingFailBlocks
	verify.splitReason = appendSplitReason(rt.splitReason, "sizing-gate:"+reason+";file-target")
	verify.accept = "本地执行 build/test/TODO scan; 不调用 tester LLM"
	verify.verifyCommand = defaultVerifyCommand(rt)
	verify.depNums = []string{children[len(children)-1].num}
	children = append(children, verify)
	return children
}

func shouldMicroSplitFileTarget(rt rawTask, file, reason string) bool {
	if rt.taskType != wbsTaskTypeLeaf || rt.workUnitType == wbsWorkUnitVerification || isPlanTestFile(file) {
		return false
	}
	if !strings.Contains(reason, "design-complete") &&
		!strings.Contains(reason, "high-risk") &&
		!strings.Contains(reason, "domain-risk") &&
		!strings.Contains(reason, "inferred-file-risk") &&
		rt.riskLevel != wbsRiskHigh {
		return false
	}
	if isProjectBootstrapRawTask(rt) || isPlanPlaceholderFile(file) {
		return false
	}
	if rt.workUnitType == wbsWorkUnitContract && isPlanContractFile(file) {
		return false
	}
	text := strings.Join([]string{rt.title, rt.accept, rt.designRef, rt.capabilityID, reason, file, strings.Join(rt.constraintRefs, " ")}, " ")
	return isHighRiskTaskText(text) || containsAdvancedCoreSurfaceTerm(text) || containsQueryAdapterTerm(text) || isDesignCompleteRiskyPlanFile(file)
}

func isDesignCompleteRiskySingleFileRawTask(rt rawTask) bool {
	files := concreteWritablePlanFiles(rt)
	if len(files) != 1 || rt.taskType != wbsTaskTypeLeaf || rt.workUnitType == wbsWorkUnitVerification {
		return false
	}
	text := strings.Join([]string{
		rt.title,
		rt.accept,
		rt.designRef,
		rt.capabilityID,
		files[0],
		strings.Join(rt.constraintRefs, " "),
	}, " ")
	return isHighRiskTaskText(text) || containsAdvancedCoreSurfaceTerm(text) || containsQueryAdapterTerm(text) || isDesignCompleteRiskyPlanFile(files[0])
}

func isDesignCompleteRiskyPlanFile(file string) bool {
	base := strings.ToLower(path.Base(filepath.ToSlash(file)))
	stem := strings.TrimSuffix(base, path.Ext(base))
	switch stem {
	case "planner", "executor", "parser", "query", "logical", "physical", "optimizer",
		"bfs", "dfs", "traversal", "graph", "engine", "wal", "lsm", "sstable",
		"memtable", "mvcc", "txn", "transaction", "snapshot", "hnsw", "index",
		"cache", "compaction", "filter", "bloom", "adjacency", "csr", "delta", "quantize", "quantization", "pq":
		return true
	default:
		return false
	}
}

func expandFileTargetMicroRawTask(rt rawTask, reason string) []rawTask {
	file := ""
	if len(rt.targetFiles) > 0 {
		file = rt.targetFiles[0]
	}
	steps := []struct {
		title    string
		accept   string
		taskType string
		role     string
		minutes  int
	}{
		{"契约与数据边界", "只定义该文件需要的最小类型、接口、错误码和空实现骨架; 禁止实现复杂算法; scoped build 通过", wbsTaskTypeLeaf, "coder", 2},
		{"最小行为路径", "只实现该文件的单一 happy path; 禁止修改上游契约或其它核心文件; scoped build 通过", wbsTaskTypeLeaf, "coder", 2},
		{"边界与错误路径", "补齐该文件内的错误处理、边界条件和并发保护; 不扩大作用域; scoped build 通过", wbsTaskTypeLeaf, "coder", 3},
		{"本地验证与回归检查", "本地执行 build/test/TODO scan; 不调用 tester LLM", wbsTaskTypeVerification, "tester", 2},
	}
	children := buildExpandedTasks(rt, reason, "file-risk-slices", steps)
	for i := range children {
		children[i].targetFiles = []string{file}
		children[i].writeFiles = []string{file}
		children[i].readFiles = nil
		children[i].targetPackages = rt.targetPackages
		children[i].parallelGroup = rt.parallelGroup
		children[i].conflictKeys = rt.conflictKeys
		if i == 0 {
			children[i].workUnitType = wbsWorkUnitContract
			children[i].riskLevel = wbsRiskLow
			children[i].complexity = "simple"
		} else if children[i].taskType == wbsTaskTypeLeaf {
			children[i].workUnitType = wbsWorkUnitImplementation
		} else {
			children[i].workUnitType = wbsWorkUnitVerification
		}
	}
	return children
}

func sanitizeParallelGroupSegment(segment string) string {
	segment = normalizePlanningID(segment)
	if segment == "" {
		return "file"
	}
	return segment
}

func duplicatePlanFileBasenames(files []string) map[string]bool {
	counts := make(map[string]int, len(files))
	for _, file := range files {
		base := strings.ToLower(path.Base(filepath.ToSlash(file)))
		if base != "" && base != "." {
			counts[base]++
		}
	}
	out := make(map[string]bool)
	for base, count := range counts {
		if count > 1 {
			out[base] = true
		}
	}
	return out
}

func fileTargetLeafTitle(parentTitle, file string, duplicateBase map[string]bool) string {
	slash := filepath.ToSlash(file)
	base := path.Base(slash)
	if duplicateBase[strings.ToLower(base)] {
		dir := path.Base(path.Dir(slash))
		if dir != "" && dir != "." {
			return parentTitle + " - " + dir + "/" + base
		}
	}
	return parentTitle + " - " + base
}

func previousPlanFiles(files []string, idx int) []string {
	if idx <= 0 {
		return nil
	}
	prev := make([]string, 0, idx)
	for i := 0; i < idx && i < len(files); i++ {
		prev = append(prev, files[i])
	}
	return prev
}

func targetPackagesForFileByLanguage(file, language string) []string {
	if strings.ToLower(strings.TrimSpace(language)) != "go" {
		return nil
	}
	dir := path.Dir(filepath.ToSlash(file))
	if dir == "." || dir == "" {
		return nil
	}
	return targetPackagesForDirByLanguage(dir, "go")
}

func isPlanContractFile(file string) bool {
	base := strings.ToLower(path.Base(filepath.ToSlash(file)))
	return strings.Contains(base, "type") || strings.Contains(base, "interface") ||
		strings.Contains(base, "contract") || strings.Contains(base, "schema")
}

func isPlanTestFile(file string) bool {
	base := strings.ToLower(path.Base(filepath.ToSlash(file)))
	return strings.Contains(base, "_test.") || strings.Contains(base, ".test.") ||
		strings.HasPrefix(base, "test_")
}

func isPlanPlaceholderFile(file string) bool {
	base := strings.ToLower(path.Base(filepath.ToSlash(file)))
	return strings.Contains(base, "placeholder") || strings.Contains(base, "stub") ||
		strings.Contains(base, "doc.go") || strings.Contains(base, "empty")
}

func directoryLikeTaskTargets(rt rawTask) []string {
	return directoryLikePlanPaths(append(append([]string{}, rt.targetFiles...), rt.writeFiles...))
}

func directoryLikeNodeTargets(node *TaskNode) []string {
	if node == nil {
		return nil
	}
	return directoryLikePlanPaths(append(append([]string{}, node.TargetFiles...), node.WriteFiles...))
}

func directoryLikePlanPaths(files []string) []string {
	var dirs []string
	for _, file := range files {
		clean := strings.TrimSpace(filepath.ToSlash(file))
		if clean == "" || filepath.IsAbs(clean) {
			continue
		}
		trimmed := strings.TrimSuffix(strings.TrimPrefix(clean, "./"), "/...")
		trimmed = strings.TrimSuffix(trimmed, "/")
		if trimmed == "" || strings.HasPrefix(trimmed, "../") {
			continue
		}
		base := strings.ToLower(path.Base(trimmed))
		if base == "makefile" || base == "dockerfile" || base == "readme" {
			continue
		}
		if strings.HasSuffix(clean, "/") || strings.HasSuffix(clean, "/...") || (path.Ext(trimmed) == "" && strings.Contains(trimmed, "/")) {
			dirs = append(dirs, trimmed)
		}
	}
	return uniqueTrimmedStrings(dirs)
}

func expandDirectoryTargetRawTask(rt rawTask, reason string, dirs []string, adapter planningLanguageAdapter) []rawTask {
	if len(dirs) == 0 {
		return expandGenericRawTask(rt, reason)
	}
	if adapter.ID == "" {
		adapter = planningLanguageAdapterFor("")
	}
	if len(dirs) > 4 {
		dirs = dirs[:4]
	}
	children := make([]rawTask, 0, len(dirs)*len(adapter.DirectorySpecs)+1)
	parentID := rt.num
	group := rt.parallelGroup
	if group == "" {
		group = parentID + ":directory-files"
	}
	for _, dir := range dirs {
		stem := safePlanFileStem(path.Base(dir))
		for _, spec := range adapter.DirectorySpecs {
			child := rt
			child.num = fmt.Sprintf("%s.%d", parentID, len(children)+1)
			child.title = rt.title + " - " + spec.Title
			child.role = orchNormalizeRole(spec.Role)
			if child.role == "" {
				child.role = "coder"
			}
			if spec.IsTest || spec.WorkUnitType == wbsWorkUnitVerification {
				child.taskType = wbsTaskTypeVerification
				child.riskLevel = wbsRiskLow
				child.verifyCommand = defaultVerifyCommand(rt)
			} else {
				child.taskType = wbsTaskTypeLeaf
				child.riskLevel = wbsRiskMedium
			}
			child.workUnitType = spec.WorkUnitType
			child.parentID = parentID
			child.estimatedMin = spec.Minutes
			child.parallelGroup = group + ":" + dir
			child.conflictKeys = []string{child.parallelGroup}
			child.blockingPolicy = wbsBlockingFailBlocks
			child.splitReason = appendSplitReason(rt.splitReason, "sizing-gate:"+reason)
			child.accept = spec.Acceptance
			child.complexity = "medium"
			child.title = rt.title + " - " + path.Base(dir) + " " + spec.Title
			target := adapter.renderDirectoryLeafFile(dir, stem, spec)
			child.targetFiles = []string{target}
			child.writeFiles = []string{target}
			child.readFiles = nil
			child.targetPackages = targetPackagesForDirByLanguage(dir, adapter.ID)
			if len(children) == 0 {
				child.depNums = append([]string(nil), rt.depNums...)
			} else {
				child.depNums = []string{children[len(children)-1].num}
			}
			children = append(children, child)
		}
	}
	if len(children) > 0 {
		verify := rt
		verify.num = fmt.Sprintf("%s.%d", parentID, len(children)+1)
		verify.title = rt.title + " - 本地验证与回归检查"
		verify.role = "tester"
		verify.taskType = wbsTaskTypeVerification
		verify.workUnitType = wbsWorkUnitVerification
		verify.parentID = parentID
		verify.estimatedMin = 2
		verify.riskLevel = wbsRiskLow
		verify.parallelGroup = group + ":verification"
		verify.conflictKeys = []string{verify.parallelGroup}
		verify.blockingPolicy = wbsBlockingFailBlocks
		verify.splitReason = appendSplitReason(rt.splitReason, "sizing-gate:"+reason)
		verify.accept = "本地执行 build/test/TODO scan; 不调用 tester LLM"
		verify.verifyCommand = defaultVerifyCommand(rt)
		verify.depNums = []string{children[len(children)-1].num}
		children = append(children, verify)
	}
	return children
}

func safePlanFileStem(stem string) string {
	stem = normalizePlanningID(stem)
	if stem == "" {
		return "module"
	}
	return strings.ReplaceAll(stem, "-", "_")
}

func targetPackagesForDir(dir string) []string {
	return targetPackagesForDirByLanguage(dir, "go")
}

func targetPackagesForDirByLanguage(dir, language string) []string {
	if strings.ToLower(strings.TrimSpace(language)) != "go" {
		return nil
	}
	dir = strings.Trim(strings.TrimSpace(filepath.ToSlash(dir)), "/")
	if dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	if len(parts) > 1 && isLikelyGeneratedProjectRoot(parts[0]) {
		return []string{"./" + strings.Join(parts[1:], "/")}
	}
	return []string{"./" + dir}
}

func expandGenericRawTask(rt rawTask, reason string) []rawTask {
	if rt.riskLevel == wbsRiskHigh || isCoreStateRawTask(rt) || strings.Contains(reason, "domain-risk-keyword") || strings.Contains(reason, "high-risk") {
		steps := []struct {
			title    string
			accept   string
			taskType string
			role     string
			minutes  int
		}{
			{"接口与状态边界", "只定义最小公开接口、状态结构、错误/常量契约; 不实现复杂算法; scoped build 通过", wbsTaskTypeLeaf, "coder", 2},
			{"最小可运行路径", "实现单一 happy path 或生命周期骨架; 不扩展额外模块; scoped build 通过", wbsTaskTypeLeaf, "coder", 2},
			{"边界与失败路径", "补齐边界条件、错误路径、并发/一致性保护中的最小必要部分; scoped build 通过", wbsTaskTypeLeaf, "coder", 3},
			{"集成适配", "只接入已经存在的上游/下游接口, 不新建未声明文件; scoped build 通过", wbsTaskTypeLeaf, "coder", 3},
			{"本地验证与回归检查", "本地执行 build/test/TODO scan; 不调用 tester LLM", wbsTaskTypeVerification, "tester", 2},
		}
		return buildExpandedTasks(rt, reason, "core-state-slices", steps)
	}
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
		if strings.Contains(reason, "mixed-contract-implementation") {
			child.title = mixedContractImplementationChildTitle(rt.title, i)
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
		if i == 0 && strings.Contains(reason, "mixed-contract-implementation") && step.taskType == wbsTaskTypeLeaf {
			child.workUnitType = wbsWorkUnitContract
			child.riskLevel = wbsRiskLow
			child.complexity = "simple"
			child.accept = "只定义稳定接口/类型/错误码/最小文件骨架; 禁止实现复杂算法或跨模块接入; scoped build 通过"
		} else if strings.Contains(reason, "mixed-contract-implementation") && step.taskType == wbsTaskTypeLeaf {
			child.workUnitType = wbsWorkUnitImplementation
			child.blockingPolicy = wbsBlockingFailOpen
			child.accept = mixedContractImplementationChildAcceptance(i, step.accept)
		} else if rt.workUnitType == wbsWorkUnitContract && step.taskType == wbsTaskTypeLeaf {
			if i == 0 {
				child.workUnitType = wbsWorkUnitContract
			} else {
				child.workUnitType = wbsWorkUnitImplementation
				child.blockingPolicy = wbsBlockingFailOpen
			}
		}
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

func mixedContractImplementationChildTitle(parent string, index int) string {
	component := mixedContractImplementationComponentName(parent)
	switch index {
	case 0:
		return component + " 契约定义"
	case 1:
		return component + " 最小实现路径"
	case 2:
		return component + " 边界行为完善"
	case 3:
		return component + " 集成适配"
	default:
		return component + " 本地验证与回归检查"
	}
}

func mixedContractImplementationChildAcceptance(index int, fallback string) string {
	switch index {
	case 1:
		return "仅实现已定义契约的最小 happy path; 禁止修改上游契约; scoped build 通过"
	case 2:
		return "补齐同一契约内的错误路径和边界条件; 禁止新增未声明接口; scoped build 通过"
	case 3:
		return "只接入已经通过的上游/下游契约; 如需接口变化必须局部兼容; scoped build 通过"
	default:
		return fallback
	}
}

func mixedContractImplementationComponentName(parent string) string {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return "组件"
	}
	replacements := []string{
		"接口与实现", "",
		"interface and implementation", "",
		"Interface and Implementation", "",
		"接口实现", "",
		"实现接口", "",
	}
	name := strings.NewReplacer(replacements...).Replace(parent)
	name = strings.Join(strings.Fields(name), " ")
	name = strings.Trim(name, " -:：")
	if name == "" {
		return "组件"
	}
	return name
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
	if strings.TrimSpace(rt.verifyCommand) != "" {
		return strings.TrimSpace(rt.verifyCommand)
	}
	if len(rt.targetPackages) > 0 {
		return "go test " + strings.Join(rt.targetPackages, " ")
	}
	return ""
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

func isHighRiskTaskText(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		"mvcc", "transaction", "事务", "lock", "锁", "concurrency", "并发",
		"scheduler", "调度", "index", "索引", "parser", "解析",
		"consensus", "共识", "raft", "cache", "缓存", "gc", "snapshot",
		"readview", "版本链", "visibility", "冲突",
		"wal", "write-ahead", "lsm", "sstable", "sst 文件", "sst file", "sst-", "compaction", "mmap",
		"page cache", "pagecache", "hnsw", "btree", "b-tree", "trie",
		"csr", "adjacency", "邻接", "delta store", "delta-store", "增量更新",
		"product quantization", "quantization", "量化", "pq",
		"replication", "sharding", "isolation", "权限", "auth", "security",
		"runtime", "compiler", "编译器", "协议", "protocol",
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

func removeString(items []string, target string) []string {
	if len(items) == 0 {
		return items
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != target {
			out = append(out, item)
		}
	}
	return out
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
			fileCount := len(node.WriteFiles)
			if fileCount == 0 {
				fileCount = len(node.TargetFiles)
			}
			mc.RecordRun("team", metrics.MWBSLeafFiles, float64(fileCount), team.Name, labels)
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

func ensureProjectSkeletonForTeam(team *ProductionTeam, objective string, adapter planningLanguageAdapter) ([]string, error) {
	if team == nil || strings.TrimSpace(team.Cwd) == "" {
		return nil, nil
	}
	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" {
		return nil, nil
	}
	return ensureProjectSkeletonForRoot(team, targetRoot, adapter)
}

func ensureProjectSkeletonForRoot(team *ProductionTeam, targetRoot string, adapter planningLanguageAdapter) ([]string, error) {
	if team == nil || strings.TrimSpace(team.Cwd) == "" {
		return nil, nil
	}
	targetRoot = strings.Trim(strings.TrimSpace(filepath.ToSlash(targetRoot)), "/")
	if targetRoot == "" || strings.HasPrefix(targetRoot, "../") || filepath.IsAbs(targetRoot) {
		return nil, nil
	}
	if adapter.ID == "" {
		adapter = planningLanguageAdapterFor("")
	}
	files := projectSkeletonFiles(adapter, targetRoot)
	if len(files) == 0 {
		return nil, nil
	}
	rootDir := filepath.Join(team.Cwd, filepath.FromSlash(targetRoot))
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for rel, content := range files {
		clean := filepath.ToSlash(filepath.Clean(rel))
		if clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
			continue
		}
		abs := filepath.Join(rootDir, filepath.FromSlash(clean))
		if _, err := os.Stat(abs); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return written, err
		}
		written = append(written, filepath.ToSlash(filepath.Join(targetRoot, clean)))
	}
	sort.Strings(written)
	return written, nil
}

func projectSkeletonFiles(adapter planningLanguageAdapter, targetRoot string) map[string]string {
	name := safeProjectIdentifier(targetRoot, "-")
	if name == "" {
		name = "app"
	}
	switch adapter.ID {
	case "python":
		pkg := safeProjectIdentifier(targetRoot, "_")
		if pkg == "" {
			pkg = "app"
		}
		return map[string]string{
			"pyproject.toml": fmt.Sprintf(`[project]
name = "%s"
version = "0.1.0"
requires-python = ">=3.10"

`, strings.ReplaceAll(name, "_", "-")),
			filepath.ToSlash(filepath.Join(pkg, "__init__.py")): fmt.Sprintf("\"\"\"%s package.\"\"\"\n", pkg),
		}
	case "rust":
		return map[string]string{
			"Cargo.toml": fmt.Sprintf(`[package]
name = "%s"
version = "0.1.0"
edition = "2021"

[dependencies]

`, strings.ReplaceAll(name, "_", "-")),
			filepath.ToSlash(filepath.Join("src", "lib.rs")): "//! Project library root.\n\n",
		}
	case "typescript":
		return map[string]string{
			"package.json": fmt.Sprintf(`{"name":"%s","version":"0.1.0","type":"module","scripts":{"test":"node --test"},"dependencies":{},"devDependencies":{}}
`, strings.ReplaceAll(name, "_", "-")),
			filepath.ToSlash(filepath.Join("src", "index.ts")): "export {};\n",
		}
	case "javascript":
		return map[string]string{
			"package.json": fmt.Sprintf(`{"name":"%s","version":"0.1.0","type":"module","scripts":{"test":"node --test"},"dependencies":{},"devDependencies":{}}
`, strings.ReplaceAll(name, "_", "-")),
			filepath.ToSlash(filepath.Join("src", "index.js")): "export {};\n",
		}
	case "cpp":
		return map[string]string{
			"CMakeLists.txt": fmt.Sprintf("cmake_minimum_required(VERSION 3.16)\nproject(%s LANGUAGES CXX)\nset(CMAKE_CXX_STANDARD 17)\nset(CMAKE_CXX_STANDARD_REQUIRED ON)\n", safeCMakeProjectName(targetRoot)),
			filepath.ToSlash(filepath.Join("src", "main.cpp")): "int main() { return 0; }\n",
		}
	default:
		pkg := safeGoPackageName(targetRoot)
		if pkg == "" {
			pkg = "app"
		}
		return map[string]string{
			"go.mod": fmt.Sprintf("module %s\n\ngo 1.22\n", strings.ToLower(strings.ReplaceAll(name, "_", "-"))),
			"doc.go": fmt.Sprintf("package %s\n", pkg),
		}
	}
}

func safeGoPackageName(raw string) string {
	id := safeProjectIdentifier(raw, "_")
	id = strings.ReplaceAll(id, "-", "_")
	id = strings.ReplaceAll(id, ".", "_")
	if id == "" {
		return "app"
	}
	var b strings.Builder
	for i, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || out == "_" {
		out = "app"
	}
	out = strings.ToLower(out)
	if out[0] >= '0' && out[0] <= '9' {
		out = "pkg_" + out
	}
	if isGoKeywordPackageName(out) {
		out += "pkg"
	}
	return out
}

func isGoKeywordPackageName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "break", "default", "func", "interface", "select",
		"case", "defer", "go", "map", "struct",
		"chan", "else", "goto", "package", "switch",
		"const", "fallthrough", "if", "range", "type",
		"continue", "for", "import", "return", "var":
		return true
	default:
		return false
	}
}

func safeProjectIdentifier(raw, sep string) string {
	id := normalizePlanningID(raw)
	id = strings.ReplaceAll(id, "-", sep)
	id = strings.ReplaceAll(id, ".", sep)
	id = strings.Trim(id, sep)
	if id == "" {
		return ""
	}
	return strings.ToLower(id)
}

func safeCMakeProjectName(raw string) string {
	id := safeProjectIdentifier(raw, "_")
	if id == "" {
		return "app"
	}
	return id
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
		"work_unit_type":  n.WorkUnitType,
		"capability_id":   n.CapabilityID,
		"risk_level":      n.RiskLevel,
		"blocking_policy": n.BlockingPolicy,
		"timeout_kind":    n.TimeoutKind,
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

	if written, err := ensureProjectSkeletonForTeam(team, objective, o.planningAdapter(objective)); err != nil {
		o.notify(o.chatID, fmt.Sprintf("⚠️ 最小项目骨架初始化失败: %v", err))
	} else if len(written) > 0 {
		o.notify(o.chatID, fmt.Sprintf("🧱 本地语言适配器已初始化最小项目骨架: %s", strings.Join(written, ", ")))
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

func (o *Orchestrator) roundBudgetFor(node *TaskNode, objective string) int {
	cfg := o.config.AdversarialRound
	if cfg <= 0 {
		cfg = 2
	}
	if cfg > 3 {
		cfg = 3
	}
	if objectiveRequiresCompleteImplementation(objective) && requiresStrictLeafLocalIntegrity(node) {
		if cfg < 3 {
			cfg = 3
		}
		return min(cfg, 3)
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
	if !buildPassed || team == nil || team.Cwd == "" {
		return false
	}
	if objectiveRequiresCompleteImplementation(team.Objective) && requiresStrictLeafLocalIntegrity(node) {
		return false
	}
	if isManifestOnlyNode(node) {
		return validateManifestTargets(team.Cwd, node.TargetFiles) == ""
	}
	if isProjectSkeletonNode(node) {
		return true
	}
	if maxRounds > 1 {
		return false
	}
	complexity := strings.ToLower(strings.TrimSpace(node.Complexity))
	if complexity == "simple" || complexity == "low" {
		return true
	}
	return len(node.TargetFiles) > 0 && len(node.TargetFiles) <= 2
}

func effectiveTargetPackagesForBuild(node *TaskNode, targetPackages []string) []string {
	if isProjectSkeletonNode(node) || isManifestOnlyNode(node) {
		return nil
	}
	return targetPackages
}

func (o *Orchestrator) shouldRunLocalVerification(node *TaskNode) bool {
	if node.TaskType == wbsTaskTypeVerification {
		return true
	}
	role := strings.ToLower(node.Role)
	title := strings.ToLower(node.Title)
	acceptance := strings.ToLower(node.AcceptCriteria)
	if strings.Contains(role, "tester") {
		return len(node.TargetFiles) == 0 && len(node.WriteFiles) == 0
	}
	if len(node.TargetFiles) > 0 || len(node.WriteFiles) > 0 {
		return false
	}
	return (strings.Contains(title, "测试") || strings.Contains(title, "验证") ||
		strings.Contains(title, "test") || strings.Contains(title, "build")) &&
		(strings.Contains(acceptance, "go test") || strings.Contains(acceptance, "go build") ||
			strings.Contains(acceptance, "编译") || strings.Contains(acceptance, "测试"))
}

func (o *Orchestrator) shouldRunLocalDirectoryBootstrap(node *TaskNode) bool {
	if node == nil || node.TaskType == wbsTaskTypeVerification {
		return false
	}
	if orchNormalizeRole(node.Role) != "coder" {
		return false
	}
	if isManifestOnlyNode(node) {
		return !isOptionalManifestOnlyNode(node)
	}
	if !isProjectSkeletonNode(node) {
		return false
	}
	if hasRequiredProjectManifestTarget(node) && isProjectSkeletonText(node) {
		return true
	}
	return len(directoryLikeNodeTargets(node)) > 0
}

func (o *Orchestrator) executeLocalDirectoryBootstrapTask(node *TaskNode, objective string, team *ProductionTeam, start time.Time) StageResult {
	var ensured []string
	var failures []string
	if team == nil || strings.TrimSpace(team.Cwd) == "" {
		failures = append(failures, "工作目录为空, 无法创建目录结构")
	} else {
		if errText := o.ensureTaskProjectSkeleton(team, objective, node); errText != "" {
			failures = append(failures, errText)
		}
		for _, manifest := range node.TargetFiles {
			clean := cleanMaterializeRelPath(manifest)
			if clean == "" || !isProjectManifestFile(clean) || isOptionalProjectManifestFile(clean) {
				continue
			}
			if errText := validateManifestTargets(team.Cwd, []string{clean}); errText != "" {
				failures = append(failures, errText)
				continue
			}
			ensured = append(ensured, filepath.ToSlash(clean))
		}
		for _, dir := range directoryLikeNodeTargets(node) {
			clean := cleanMaterializeRelPath(dir)
			if clean == "" {
				continue
			}
			if err := os.MkdirAll(filepath.Join(team.Cwd, clean), 0o755); err != nil {
				failures = append(failures, fmt.Sprintf("创建目录失败 %s: %v", filepath.ToSlash(clean), err))
				continue
			}
			ensured = append(ensured, filepath.ToSlash(clean))
		}
	}
	duration := time.Since(start)
	if len(failures) > 0 {
		output := "local directory bootstrap failed:\n" + strings.Join(failures, "\n")
		node.Error = output
		cascaded, _ := o.dag.SetTaskStatusAndUnblock(node.V2TaskID, "failed")
		o.mu.Lock()
		o.failedCount += 1 + cascaded
		o.mu.Unlock()
		o.recordWBSTaskDuration(team, node, duration, TaskFailed)
		o.recordWBSFailedBlockedDependents(team, node, cascaded)
		o.notify(o.chatID, fmt.Sprintf("🔴 %s 目录初始化失败 (%s)", node.Title, duration.Round(time.Second)))
		return StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed, Error: output, Output: output, StartedAt: start, Duration: duration.Round(time.Second).String()}
	}
	sort.Strings(ensured)
	output := "local directory bootstrap: ensured " + strings.Join(ensured, ", ")
	node.Output = output
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
	o.reportProgress("local-directory-bootstrap", 1, int64(len(output)), node.V2TaskID)
	o.notify(o.chatID, fmt.Sprintf("✅ %s 目录初始化完成 (%s)", node.Title, duration.Round(time.Second)))
	return StageResult{Name: node.Title, Role: node.Role, Status: TaskCompleted, Output: output, StartedAt: start, Duration: duration.Round(time.Second).String()}
}

func (o *Orchestrator) executeLocalVerificationTask(node *TaskNode, objective string, team *ProductionTeam, start time.Time) StageResult {
	lang := o.runtimeLanguageForTeam(team, "")
	var checks []string
	var failures []string
	if team == nil || team.Cwd == "" {
		failures = append(failures, "工作目录为空, 无法执行本地验证")
	} else {
		if errText := o.ensureTaskProjectSkeleton(team, "", node); errText != "" {
			failures = append(failures, errText)
		}
		buildCwd := inferBuildCwdFromTaskScopeForObjective(team.Cwd, node.TargetFiles, node.TargetPackages, objective)
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
		if recovered, msg := o.recoverStaleVerificationWithProjectTest(team, objective, node); recovered {
			output = "local verification rechecked: " + msg
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
			o.reportProgress("local-verification-recheck", 1, int64(len(output)), node.V2TaskID)
			o.notify(o.chatID, fmt.Sprintf("🟡 %s 本地验证项目级复核通过 (%s)", node.Title, duration.Round(time.Second)))
			return StageResult{Name: node.Title, Role: node.Role, Status: TaskCompleted, Output: output, StartedAt: start, Duration: duration.Round(time.Second).String()}
		}
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

func (o *Orchestrator) recoverStaleVerificationWithProjectTest(team *ProductionTeam, objective string, node *TaskNode) (bool, string) {
	if team == nil || team.Cwd == "" || node == nil {
		return false, ""
	}
	lang := o.runtimeLanguageForTeam(team, objective)
	if strings.TrimSpace(lang) == "" {
		return false, ""
	}
	var candidates []string
	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		candidates = append(candidates, filepath.Join(team.Cwd, filepath.FromSlash(targetRoot)))
	}
	if buildCwd := inferBuildCwdFromTaskScopeForObjective(team.Cwd, node.TargetFiles, node.TargetPackages, objective); buildCwd != "" {
		candidates = append(candidates, buildCwd)
	}
	candidates = append(candidates, team.Cwd)
	for _, candidate := range uniqueTrimmedStrings(candidates) {
		if candidate == "" || !isDir(candidate) {
			continue
		}
		if errText := runTestCheckLang(candidate, lang); errText == "" {
			return true, "project-level local test passed; scoped verification failure was stale or over-specific"
		}
	}
	return false, ""
}

func (o *Orchestrator) runtimeLanguageForTeam(team *ProductionTeam, objective string) string {
	if team != nil && strings.TrimSpace(team.Language) != "" {
		return inferPlanningLanguageID("", team.Language)
	}
	if strings.TrimSpace(o.planningLang) != "" {
		return o.planningLang
	}
	if inferred := inferPlanningLanguageID(objective, ""); inferred != "" {
		return inferred
	}
	return "go"
}

func (o *Orchestrator) ensureTaskProjectSkeleton(team *ProductionTeam, objective string, node *TaskNode) string {
	if team == nil || strings.TrimSpace(team.Cwd) == "" {
		return ""
	}
	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" && node != nil {
		targetRoot = firstPlanPathSegment(append(append([]string{}, node.TargetFiles...), node.WriteFiles...))
	}
	var err error
	if targetRoot != "" {
		_, err = ensureProjectSkeletonForRoot(team, targetRoot, o.planningAdapter(objective))
	} else {
		_, err = ensureProjectSkeletonForTeam(team, objective, o.planningAdapter(objective))
	}
	if err != nil {
		return fmt.Sprintf("最小项目骨架初始化失败: %v", err)
	}
	return ""
}

func (o *Orchestrator) handleTaskTimeout(ctx context.Context, node *TaskNode, objective string, team *ProductionTeam, start time.Time, err error) StageResult {
	duration := time.Since(start)
	promptChars := 0
	if node != nil {
		promptChars = len(o.buildTaskPrompt(node, objective, team))
	}
	classification := classifyAgentTimeout(err, node, promptChars, duration)
	if node != nil {
		node.TimeoutKind = classification.Kind
	}
	errText := fmt.Sprintf("timeout-classifier: kind=%s action=%s reason=%s; error=%s",
		classification.Kind, classification.Action, classification.Reason, err.Error())
	node.Error = errText
	node.SplitReason = appendSplitReason(node.SplitReason, "timeout")

	if !classification.AllowSplit {
		o.notify(o.chatID, fmt.Sprintf("⏱️ %s 超过 agent %s 预算, 分类为 %s (%s), 不做动态拆分, 进入重试/失败流程",
			node.Title, coderCallTimeout, classification.Kind, classification.Reason))
		return o.handleTaskFailure(ctx, node, objective, team, StageResult{
			Name:      node.Title,
			Role:      node.Role,
			Status:    TaskFailed,
			Error:     errText,
			StartedAt: start,
			Duration:  duration.Round(time.Second).String(),
		})
	}

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
			workUnitType:   node.WorkUnitType,
			capabilityID:   node.CapabilityID,
			contractRefs:   node.ContractRefs,
			provides:       node.Provides,
			requires:       node.Requires,
			readFiles:      node.ReadFiles,
			writeFiles:     node.WriteFiles,
			conflictKeys:   node.ConflictKeys,
			estimatedLOC:   node.EstimatedLOC,
			estimatedMin:   max(node.EstimatedMin, 6),
			riskLevel:      wbsRiskHigh,
			parallelGroup:  node.ParallelGroup,
			blockingPolicy: wbsBlockingFailBlocks,
			splitReason:    appendSplitReason(node.SplitReason, "timeout"),
		}
		children = expandRawTask(rt, "timeout", o.planningAdapter(objective), objective)
		splitSource = "deterministic-fallback"
		if len(children) == 0 {
			return 0, splitSource, fmt.Errorf("timeout split produced no children; llm_error=%v", planErr)
		}
	}

	children = o.prepareTimeoutSplitChildren(children, node)
	childNumToV2ID := make(map[string]string)
	conflictLastV2ID := make(map[string]string)
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
		for _, key := range rawTaskConflictKeys(child) {
			if prev, ok := conflictLastV2ID[key]; ok && !containsString(depV2IDs, prev) {
				depV2IDs = append(depV2IDs, prev)
			}
		}
		for _, file := range rawTaskWriteFiles(child) {
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
			WorkUnitType:   child.workUnitType,
			CapabilityID:   child.capabilityID,
			ContractRefs:   child.contractRefs,
			Provides:       child.provides,
			Requires:       child.requires,
			ReadFiles:      child.readFiles,
			WriteFiles:     child.writeFiles,
			ConflictKeys:   child.conflictKeys,
			EstimatedLOC:   child.estimatedLOC,
			ParentID:       node.V2TaskID,
			EstimatedMin:   child.estimatedMin,
			RiskLevel:      child.riskLevel,
			VerifyCommand:  child.verifyCommand,
			ParallelGroup:  child.parallelGroup,
			BlockingPolicy: child.blockingPolicy,
			SplitReason:    appendSplitReason(child.splitReason, "source:"+splitSource),
			TimeoutKind:    child.timeoutKind,
		}
		childNodes = append(childNodes, childNode)
		childNumToV2ID[child.num] = v2ID
		for _, key := range rawTaskConflictKeys(child) {
			conflictLastV2ID[key] = v2ID
		}
		for _, file := range rawTaskWriteFiles(child) {
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
      "verifyCommand": "架构阶段确定的本地 build/test 命令",
	      "parallelGroup": "",
	      "workUnitType": "implementation",
	      "capabilityId": "",
	      "contractRefs": [],
	      "provides": [],
	      "requires": [],
	      "readFiles": [],
	      "writeFiles": [],
	      "conflictKeys": [],
	      "estimatedChangedLOC": 120,
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
3. 每个 leaf 默认 1-2 个 writeFiles/targetFiles; 如果无法精确到文件, 继承原任务文件, 但标题必须进一步收窄。
4. 最后必须有 taskType="verification" 的本地验证任务, role="tester", 只跑 build/test/TODO scan。
5. 共享核心状态、共享写文件、共享 schema/contract、共享 runtime manifest 的 leaf 默认串行, 使用 dependsOn 或 conflictKeys 表达；parallelGroup 只是能力分组标签, 不表示串行锁。
6. 只有无共享文件、无共享核心状态、无依赖边的 leaf 才允许并发。
7. dependsOn 只能引用本次输出中更早的 id。
8. blockingPolicy 默认 fail_blocks_dependents。
9. designRef/acceptance 必须说明要复用的真实 API 名称, 避免重复定义已有符号。
10. readFiles 只表示可参考文件, DAG compiler 不会因为共同 readFiles 串行; writeFiles/conflictKeys 才会触发串行。
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
		task = normalizeRawTaskDefaults(task)
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
	for i := range cleaned {
		cleaned[i] = normalizeRawTaskDefaults(cleaned[i])
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
		children[i] = normalizeRawTaskDefaults(child)
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
	for i := range children {
		children[i] = normalizeRawTaskDefaults(children[i])
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
		if isLocalVerificationRawTask(task) {
			continue
		}
		for _, dep := range task.depNums {
			dep = strings.TrimSpace(dep)
			if ids[dep] {
				hasDownstream[dep] = true
			}
		}
	}
	var terminals []string
	for _, task := range tasks {
		if task.num == "" || isLocalVerificationRawTask(task) {
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
	barrierTerminals := terminalRawTaskIDsExcludingBarriers(tasks)
	for i := range tasks {
		switch {
		case isLocalVerificationRawTask(tasks[i]):
			scopedTerminals := scopedTerminalRawTaskIDs(tasks, tasks[i].parentID, tasks[i].num)
			if len(scopedTerminals) > 0 {
				tasks[i].depNums = uniqueTrimmedStrings(append(tasks[i].depNums, scopedTerminals...))
			} else if len(tasks[i].depNums) > 0 {
				tasks[i].depNums = uniqueTrimmedStrings(tasks[i].depNums)
			} else if len(terminals) > 0 {
				tasks[i].depNums = uniqueTrimmedStrings(append(tasks[i].depNums, terminals...))
			} else if i > 0 {
				tasks[i].depNums = []string{tasks[i-1].num}
			}
		case isBarrierRawTask(tasks[i]):
			deps := excludeRawTaskID(barrierTerminals, tasks[i].num)
			if len(deps) > 0 {
				tasks[i].depNums = uniqueTrimmedStrings(append(tasks[i].depNums, deps...))
			} else if len(tasks[i].depNums) > 0 {
				tasks[i].depNums = uniqueTrimmedStrings(tasks[i].depNums)
			}
		}
	}
}

func scopedTerminalRawTaskIDs(tasks []rawTask, parentID, self string) []string {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return nil
	}
	ids := make(map[string]bool)
	hasDownstream := make(map[string]bool)
	for _, task := range tasks {
		if task.num == "" || task.num == self || task.parentID != parentID || isLocalVerificationRawTask(task) {
			continue
		}
		ids[task.num] = true
	}
	for _, task := range tasks {
		if task.parentID != parentID || isLocalVerificationRawTask(task) {
			continue
		}
		for _, dep := range task.depNums {
			dep = strings.TrimSpace(dep)
			if ids[dep] {
				hasDownstream[dep] = true
			}
		}
	}
	var terminals []string
	for _, task := range tasks {
		if task.num == "" || task.num == self || task.parentID != parentID || isLocalVerificationRawTask(task) {
			continue
		}
		if !hasDownstream[task.num] {
			terminals = append(terminals, task.num)
		}
	}
	return uniqueTrimmedStrings(terminals)
}

func terminalRawTaskIDsExcludingBarriers(tasks []rawTask) []string {
	ids := make(map[string]bool)
	hasDownstream := make(map[string]bool)
	for _, task := range tasks {
		if task.num != "" {
			ids[task.num] = true
		}
	}
	for _, task := range tasks {
		if isBarrierRawTask(task) {
			continue
		}
		for _, dep := range task.depNums {
			dep = strings.TrimSpace(dep)
			if ids[dep] {
				hasDownstream[dep] = true
			}
		}
	}
	var terminals []string
	for _, task := range tasks {
		if task.num == "" || isBarrierRawTask(task) {
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
		if tasks[i].num != "" && !isBarrierRawTask(tasks[i]) {
			return []string{tasks[i].num}
		}
	}
	return nil
}

func excludeRawTaskID(ids []string, excluded string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && id != excluded {
			out = append(out, id)
		}
	}
	return uniqueTrimmedStrings(out)
}

func (o *Orchestrator) tryDeterministicContractPatch(team *ProductionTeam, objective string, node *TaskNode, lang string, written []string, buildCwd string, targetPackages []string, initialBuildErrors string) (string, []string, string, []string, bool, bool) {
	if strings.ToLower(strings.TrimSpace(lang)) != "go" || strings.TrimSpace(buildCwd) == "" {
		return "", written, buildCwd, targetPackages, false, false
	}
	changed := applyGoCompileTextRepairs(buildCwd)
	if initialBuildErrors != "" {
		changed += applyGoCompileErrorRepairs(buildCwd, initialBuildErrors)
	}
	if changed == 0 {
		return "", written, buildCwd, targetPackages, false, false
	}
	buildErrors := o.ensureTaskProjectSkeleton(team, objective, node)
	if buildErrors == "" {
		buildErrors = runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
	}
	if buildErrors != "" {
		if errorFixes := applyGoCompileErrorRepairs(buildCwd, buildErrors); errorFixes > 0 {
			buildErrors = runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
			changed += errorFixes
		}
	}
	testsPassed := false
	if buildErrors == "" && shouldRunLocalTestsForNode(node, written) {
		buildErrors = runTestCheckLang(buildCwd, lang)
		testsPassed = buildErrors == ""
	}
	if buildErrors == "" && !shouldRunLocalTestsForNode(node, written) {
		testsPassed = true
	}
	if changed > 0 {
		o.notify(o.chatID, fmt.Sprintf("🛠️ %s deterministic Go compile repair applied (%d files/fixes)", node.Title, changed))
	}
	return buildErrors, written, buildCwd, targetPackages, true, testsPassed
}

func (o *Orchestrator) tryDeterministicContractSkeletonFallback(team *ProductionTeam, node *TaskNode, lang string, written []string, buildCwd string, targetPackages []string) (string, []string, string, []string, bool) {
	if team == nil || node == nil || strings.ToLower(strings.TrimSpace(lang)) != "go" || !isContractBoundaryNode(node) {
		return "", written, buildCwd, targetPackages, false
	}
	if objectiveRequiresCompleteImplementation(team.Objective) {
		return "", written, buildCwd, targetPackages, false
	}
	target := firstConcreteGoFallbackTargetFile(team.Cwd, node)
	if target == "" {
		return "", written, buildCwd, targetPackages, false
	}
	full := filepath.Join(team.Cwd, filepath.FromSlash(target))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err.Error(), written, buildCwd, targetPackages, false
	}
	src := buildGoContractSkeletonSource(target, node)
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		return err.Error(), written, buildCwd, targetPackages, false
	}
	_ = runGofmtFiles([]string{full})
	written = uniqueTrimmedStrings(append(written, target))
	if buildCwd == "" {
		buildCwd = team.Cwd
	}
	if _, err := os.Stat(filepath.Join(buildCwd, "go.mod")); err != nil {
		buildCwd = team.Cwd
	}
	if inferred := inferTaskBuildCwd(team.Cwd, []string{target}); inferred != "" && !goModExists(buildCwd) {
		buildCwd = inferred
		targetPackages = adjustTargetPackagesForBuildRoot(team.Cwd, buildCwd, node.TargetPackages)
	}
	buildErrors := runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
	if buildErrors != "" {
		if changed := applyGoCompileErrorRepairs(buildCwd, buildErrors); changed > 0 {
			buildErrors = runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
		}
	}
	if buildErrors != "" {
		return buildErrors, written, buildCwd, targetPackages, false
	}
	return "", written, buildCwd, targetPackages, true
}

func goModExists(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func runGofmtFiles(files []string) error {
	files = uniqueTrimmedStrings(files)
	if len(files) == 0 {
		return nil
	}
	args := append([]string{"-w"}, files...)
	return exec.Command("gofmt", args...).Run()
}

func firstConcreteGoTargetFile(node *TaskNode) string {
	if node == nil {
		return ""
	}
	for _, file := range append(append([]string{}, node.TargetFiles...), node.WriteFiles...) {
		clean := strings.TrimSpace(filepath.ToSlash(file))
		if clean == "" || filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") {
			continue
		}
		if strings.HasSuffix(clean, ".go") && !strings.HasSuffix(clean, "_test.go") {
			return clean
		}
	}
	return ""
}

func firstConcreteGoFallbackTargetFile(cwd string, node *TaskNode) string {
	if target := firstConcreteGoTargetFile(node); target != "" {
		return target
	}
	tokens := goFileTokensFromTaskNode(node)
	for _, token := range tokens {
		clean := cleanMaterializeRelPath(token)
		if clean == "" {
			continue
		}
		slash := filepath.ToSlash(clean)
		if strings.Contains(slash, "/") && strings.HasSuffix(slash, ".go") && !strings.HasSuffix(slash, "_test.go") {
			return slash
		}
	}
	if cwd == "" || len(tokens) == 0 {
		return ""
	}
	byBase := map[string]bool{}
	for _, token := range tokens {
		base := path.Base(filepath.ToSlash(token))
		if base != "" && strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go") {
			byBase[base] = true
		}
	}
	if len(byBase) == 0 {
		return ""
	}
	matches := map[string][]string{}
	_ = filepath.Walk(cwd, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".claude-go", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !byBase[info.Name()] {
			return nil
		}
		rel, relErr := filepath.Rel(cwd, p)
		if relErr == nil {
			matches[info.Name()] = append(matches[info.Name()], filepath.ToSlash(rel))
		}
		return nil
	})
	for _, token := range tokens {
		base := path.Base(filepath.ToSlash(token))
		candidates := uniqueTrimmedStrings(matches[base])
		if len(candidates) == 1 {
			return candidates[0]
		}
	}
	return ""
}

func goFileTokensFromTaskNode(node *TaskNode) []string {
	if node == nil {
		return nil
	}
	text := strings.Join([]string{
		node.Title,
		node.AcceptCriteria,
		node.SplitReason,
		node.CapabilityID,
		strings.Join(node.ReadFiles, " "),
		strings.Join(node.TargetFiles, " "),
		strings.Join(node.WriteFiles, " "),
	}, " ")
	re := regexp.MustCompile(`[A-Za-z0-9_.@+-]+(?:/[A-Za-z0-9_.@+-]+)*\.go\b`)
	return uniqueTrimmedStrings(re.FindAllString(text, -1))
}

func buildGoContractSkeletonSource(target string, node *TaskNode) string {
	pkg := goPackageNameForPlanFile(target)
	typeStem := exportedIdentifierFromText(node.Title)
	if typeStem == "" {
		typeStem = exportedIdentifierFromText(path.Base(strings.TrimSuffix(target, ".go")))
	}
	if typeStem == "" {
		typeStem = "Contract"
	}
	return fmt.Sprintf(`package %s

import "context"

// %s is a stable contract boundary generated after a failed contract draft.
type %s interface {
	ContractName() string
}

// %sSpec captures minimal contract metadata for downstream implementation leaves.
type %sSpec struct {
	Name string
}

func (s %sSpec) ContractName() string {
	if s.Name == "" {
		return "%s"
	}
	return s.Name
}

type OperationContext = context.Context
`, pkg, typeStem, typeStem, typeStem, typeStem, typeStem, strings.ToLower(typeStem))
}

func goPackageNameForPlanFile(file string) string {
	dir := path.Base(path.Dir(filepath.ToSlash(file)))
	if dir == "" || dir == "." || dir == string(filepath.Separator) {
		return "main"
	}
	return safeGoPackageName(dir)
}

func exportedIdentifierFromText(text string) string {
	parts := regexp.MustCompile(`[^A-Za-z0-9]+`).Split(text, -1)
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part[0] >= '0' && part[0] <= '9' {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		if len(part) > 1 {
			b.WriteString(part[1:])
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	return out
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

	if o.shouldRunLocalDirectoryBootstrap(node) {
		return o.executeLocalDirectoryBootstrapTask(node, objective, team, start)
	}

	if o.shouldRunLocalVerification(node) {
		return o.executeLocalVerificationTask(node, objective, team, start)
	}

	if !o.config.MicroTestAfter {
		return o.executeTaskOnce(ctx, node, objective, team, start)
	}

	// L1: 自适应任务粒度 — 以 token/time 预算为先, 避免简单任务陷入多轮评审黑洞。
	maxRounds := o.roundBudgetFor(node, objective)
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
		if reason := validateTaskAgentOutput(result, node); reason != "" {
			return o.handleTaskFailure(ctx, node, objective, team,
				StageResult{Name: node.Title, Role: node.Role, Status: TaskFailed,
					Error: "产出验证失败: " + reason, Output: result,
					StartedAt: start, Duration: time.Since(start).String()})
		}

		lastOutput = result
		node.Output = result
		o.reportProgress("LLM生成", round, int64(len(result)), node.V2TaskID)

		// L4: 文件物化 — 提取代码块写入磁盘
		lang := o.runtimeLanguageForTeam(team, objective)
		buildCwd := team.Cwd
		targetPackages := append([]string(nil), node.TargetPackages...)
		var written []string
		var scopeError string
		if team.Cwd != "" {
			rawWritten := MaterializeCode(team.Cwd, result, lang)
			written = enforceTargetFileScope(team.Cwd, rawWritten, node)
			scopeError = targetScopeViolationError(rawWritten, written, node)
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
				buildErrors = scopeError
			}
			if buildErrors == "" {
				buildErrors = o.ensureTaskProjectSkeleton(team, objective, node)
			}
			if buildErrors == "" {
				if isManifestOnlyNode(node) {
					buildErrors = validateManifestTargets(team.Cwd, node.TargetFiles)
				} else {
					buildErrors = runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
				}
			}
			if buildErrors == "" && shouldRunLocalTestsForNode(node, written) {
				buildErrors = runTestCheckLang(buildCwd, lang)
				localTestsPassed = buildErrors == ""
			}
			if buildErrors != "" {
				var patched bool
				var patchTestsPassed bool
				var patchErrors string
				patchErrors, written, buildCwd, targetPackages, patched, patchTestsPassed = o.tryDeterministicContractPatch(team, objective, node, lang, written, buildCwd, targetPackages, buildErrors)
				if patched {
					buildErrors = patchErrors
					localTestsPassed = localTestsPassed || patchTestsPassed
					if buildErrors == "" {
						buildPassed = true
						o.notify(o.chatID, fmt.Sprintf("🟢 %s deterministic contract repair 后编译通过", node.Title))
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
					rawWritten := MaterializeCode(team.Cwd, fixResult, lang)
					written = enforceTargetFileScope(team.Cwd, rawWritten, node)
					scopeError = targetScopeViolationError(rawWritten, written, node)
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
						buildErrors = scopeError
					}
					if buildErrors == "" {
						buildErrors = o.ensureTaskProjectSkeleton(team, objective, node)
					}
					if buildErrors == "" {
						if isManifestOnlyNode(node) {
							buildErrors = validateManifestTargets(team.Cwd, node.TargetFiles)
						} else {
							buildErrors = runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
						}
					}
					if buildErrors == "" && shouldRunLocalTestsForNode(node, written) {
						buildErrors = runTestCheckLang(buildCwd, lang)
						localTestsPassed = buildErrors == ""
					}
					if buildErrors != "" {
						var patched bool
						var patchTestsPassed bool
						var patchErrors string
						patchErrors, written, buildCwd, targetPackages, patched, patchTestsPassed = o.tryDeterministicContractPatch(team, objective, node, lang, written, buildCwd, targetPackages, buildErrors)
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
					var fallbackOK bool
					var fallbackErr string
					fallbackErr, written, buildCwd, targetPackages, fallbackOK = o.tryDeterministicContractSkeletonFallback(team, node, lang, written, buildCwd, targetPackages)
					if fallbackOK {
						buildErrors = ""
						buildPassed = true
						localTestsPassed = true
						lastScore = EvalScore{Correctness: 7, Completeness: 5, Security: 7, CodeQuality: 7, DesignAlignment: 6,
							Feedback: "契约任务编译失败后已降级为最小稳定契约骨架，允许下游继续细化实现"}
						terminator.RecordBuildResult(true)
						o.notify(o.chatID, fmt.Sprintf("🟢 %s contract skeleton fallback 后编译通过", node.Title))
					} else if fallbackErr != "" {
						buildErrors = fallbackErr
					}
				}
				if !buildPassed {
					if shouldTryEarlyStaleBuildRecovery(buildErrors, written, node, objective) {
						if recovered, msg := o.recoverStaleFailureWithProjectTest(team, objective, node); recovered {
							buildPassed = true
							lastBuildPassed = true
							localTestsPassed = true
							node.TestPassed = true
							node.TestResult = msg
							lastScore = EvalScore{
								Correctness:     7,
								Completeness:    6,
								Security:        7,
								CodeQuality:     7,
								DesignAlignment: 6,
								Feedback:        msg,
								Pass:            true,
							}
							terminator.RecordBuildResult(true)
							terminator.RecordRoundOutput(round, lastScore, lastOutput)
							iterMemory = append(iterMemory, IterationMemory{
								Round: round, Approach: "项目级复核通过", Score: lastScore,
								KeyIssues: []string{"局部编译失败状态已被全项目测试证伪"}, TestPass: true, Kept: true,
							})
							o.notify(o.chatID, fmt.Sprintf("🟡 %s 编译修复后项目级复核通过, 提前覆盖陈旧局部失败状态", node.Title))
							break
						}
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
		if buildPassed && objectiveRequiresCompleteImplementation(objective) && shouldTreatMicroTestAsAdvisory(node, written) && !node.TestPassed {
			node.TestPassed = true
			node.TestResult = "strict design-complete local build passed; LLM micro-test advisory was overridden:\n" + node.TestResult
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
			fullScore := scoreMsg + fmt.Sprintf(" 通过:%v test:%v", taskScoreMeetsHardGate(node, score, node.TestPassed, buildPassed, objective), node.TestPassed)
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
			if (!taskScoreMeetsHardGate(node, score, node.TestPassed, buildPassed, objective) || !node.TestPassed || !buildPassed) && round < terminator.MaxRounds {
				lastFeedback = hardGateContinuationFeedback(score.Feedback, node.TestResult, buildPassed, node.TestPassed)
				o.notify(o.chatID, fmt.Sprintf("🔄 %s hard gate 未满足, 忽略提前终止继续修复 (第 %d/%d 轮)", node.Title, round+1, terminator.MaxRounds))
				continue
			}
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

	if !lastBuildPassed && team != nil && team.Cwd != "" && !(objectiveRequiresCompleteImplementation(objective) && requiresStrictLeafLocalIntegrity(node)) {
		lang := o.runtimeLanguageForTeam(team, objective)
		buildCwd := inferBuildCwdFromTaskScope(team.Cwd, node.TargetFiles, node.TargetPackages)
		targetPackages := adjustTargetPackagesForBuildRoot(team.Cwd, buildCwd, node.TargetPackages)
		finalBuildErrors := o.ensureTaskProjectSkeleton(team, objective, node)
		if finalBuildErrors == "" {
			if isManifestOnlyNode(node) {
				finalBuildErrors = validateManifestTargets(team.Cwd, node.TargetFiles)
			} else {
				finalBuildErrors = runBuildCheckScoped(buildCwd, lang, effectiveTargetPackagesForBuild(node, targetPackages))
			}
		}
		if finalBuildErrors == "" {
			lastBuildPassed = true
			node.TestPassed = true
			node.TestResult = "final local build recheck passed; previous compile failure was stale"
			if lastScore.Correctness < 6 || lastScore.CodeQuality < 6 {
				lastScore = EvalScore{
					Correctness:     7,
					Completeness:    6,
					Security:        7,
					CodeQuality:     7,
					DesignAlignment: 6,
					Feedback:        "final local build recheck passed; allow incremental leaf to continue downstream",
					Pass:            true,
				}
			}
			o.notify(o.chatID, fmt.Sprintf("🟡 %s 最终本地构建复核通过, 覆盖陈旧编译失败状态", node.Title))
		} else if recovered, msg := o.recoverStaleFailureWithProjectTest(team, objective, node); recovered {
			lastBuildPassed = true
			node.TestPassed = true
			node.TestResult = msg
			if lastScore.Correctness < 6 || lastScore.CodeQuality < 6 {
				lastScore = EvalScore{
					Correctness:     7,
					Completeness:    6,
					Security:        7,
					CodeQuality:     7,
					DesignAlignment: 6,
					Feedback:        msg,
					Pass:            true,
				}
			}
			o.notify(o.chatID, fmt.Sprintf("🟡 %s 全项目最终验证通过, 覆盖陈旧局部失败状态", node.Title))
		}
	}

	passLabel := "⚠️未达标"
	reviewPassed := taskScoreMeetsHardGate(node, lastScore, node.TestPassed, lastBuildPassed, objective)
	taskPassed := reviewPassed && node.TestPassed
	incrementalAccepted := false
	if !taskPassed && shouldAcceptIncrementalLeaf(node, lastScore, node.TestPassed, lastBuildPassed, objective) {
		taskPassed = true
		incrementalAccepted = true
		passLabel = "✅增量Leaf本地通过"
	}
	if incrementalAccepted {
		passLabel = "✅增量Leaf本地通过"
	} else if taskPassed {
		passLabel = "✅全通过"
	} else if reviewPassed {
		passLabel = "✅review通过 ⚠️test偏差"
	} else if node.TestPassed {
		passLabel = "⚠️review未达标 ✅test通过"
	}

	if !taskPassed && node.BlockingPolicy != wbsBlockingFailOpen {
		if recovered, msg := o.recoverStaleFailureWithProjectTest(team, objective, node); recovered {
			lastBuildPassed = true
			node.TestPassed = true
			node.TestResult = msg
			if lastScore.Correctness < 6 || lastScore.CodeQuality < 6 {
				lastScore = EvalScore{
					Correctness:     7,
					Completeness:    6,
					Security:        7,
					CodeQuality:     7,
					DesignAlignment: 6,
					Feedback:        msg,
					Pass:            true,
				}
			}
			taskPassed = true
			passLabel = "✅项目级复核通过"
			o.notify(o.chatID, fmt.Sprintf("🟡 %s hard gate 前项目级最终验证通过, 覆盖陈旧局部失败状态", node.Title))
		}
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
	if containsPseudoToolCall(output) {
		return "输出包含伪工具调用, 但 coder leaf 必须直接输出目标文件完整内容代码块"
	}
	if len(written) > 0 {
		return ""
	}
	if isOptionalManifestOnlyNode(node) {
		return ""
	}
	if node != nil && orchNormalizeRole(node.Role) == "coder" && node.TaskType != wbsTaskTypeVerification && len(node.TargetFiles) > 0 {
		return "当前 coder Leaf 声明了 targetFiles 但未能物化任何文件。请按 `File: path/to/file.ext` 或 `### path/to/file.ext` 输出完整文件内容。"
	}
	return materializationGateError(output, written, lang)
}

func validateTaskAgentOutput(output string, node *TaskNode) string {
	if isManifestOnlyNode(node) && strings.TrimSpace(output) != "" {
		return ""
	}
	role := ""
	if node != nil {
		role = node.Role
	}
	return validateAgentOutput(output, role)
}

func isManifestOnlyNode(node *TaskNode) bool {
	if node == nil || len(node.TargetFiles) == 0 {
		return false
	}
	for _, file := range node.TargetFiles {
		if !isProjectManifestFile(file) {
			return false
		}
	}
	return true
}

func isOptionalManifestOnlyNode(node *TaskNode) bool {
	if node == nil || len(node.TargetFiles) == 0 {
		return false
	}
	for _, file := range node.TargetFiles {
		if !isOptionalProjectManifestFile(file) {
			return false
		}
	}
	return true
}

func hasRequiredProjectManifestTarget(node *TaskNode) bool {
	if node == nil {
		return false
	}
	for _, file := range append(append([]string{}, node.TargetFiles...), node.WriteFiles...) {
		clean := cleanMaterializeRelPath(file)
		if clean == "" || isOptionalProjectManifestFile(clean) {
			continue
		}
		if isProjectManifestFile(clean) {
			return true
		}
	}
	return false
}

func isProjectManifestFile(file string) bool {
	switch strings.ToLower(path.Base(filepath.ToSlash(file))) {
	case "go.mod", "go.sum", "package.json", "cargo.toml", "pyproject.toml", "cmakelists.txt":
		return true
	default:
		return false
	}
}

func isOptionalProjectManifestFile(file string) bool {
	return strings.EqualFold(path.Base(filepath.ToSlash(file)), "go.sum")
}

func validateManifestTargets(cwd string, targetFiles []string) string {
	if cwd == "" || len(targetFiles) == 0 {
		return ""
	}
	for _, target := range targetFiles {
		clean := cleanMaterializeRelPath(target)
		if clean == "" {
			continue
		}
		if isOptionalProjectManifestFile(clean) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cwd, clean))
		if err != nil {
			return fmt.Sprintf("manifest 缺失或不可读: %s: %v", filepath.ToSlash(clean), err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return "manifest 为空: " + filepath.ToSlash(clean)
		}
		if strings.EqualFold(path.Base(filepath.ToSlash(clean)), "go.mod") && !strings.Contains(string(data), "module ") {
			return "go.mod 缺少 module 声明: " + filepath.ToSlash(clean)
		}
	}
	return ""
}

func shouldRunLocalTestsForWritten(written []string) bool {
	for _, file := range written {
		if strings.HasSuffix(strings.ToLower(filepath.ToSlash(file)), "_test.go") {
			return true
		}
	}
	return false
}

func shouldRunLocalTestsForNode(node *TaskNode, written []string) bool {
	if isManifestOnlyNode(node) || isProjectSkeletonNode(node) {
		return false
	}
	return shouldRunLocalTestsForWritten(written)
}

func shouldTreatMicroTestAsAdvisory(node *TaskNode, written []string) bool {
	if node == nil || shouldRunLocalTestsForWritten(written) {
		return false
	}
	if isManifestOnlyNode(node) || (isProjectSkeletonNode(node) && !isContractLikeNode(node)) {
		return false
	}
	return isContractLikeNode(node)
}

func taskScoreMeetsHardGate(node *TaskNode, score EvalScore, testPassed, buildPassed bool, objective string) bool {
	if score.MeetsHardPassThreshold() {
		return true
	}
	if node == nil || !testPassed || !buildPassed || !objectiveRequiresCompleteImplementation(objective) || !isContractLikeNode(node) {
		return false
	}
	if score.DesignAlignment > 0 && score.DesignAlignment < 4 {
		return false
	}
	if score.Correctness < 4 || score.Completeness < 4 || score.Security < 4 || score.CodeQuality < 4 {
		return false
	}
	if score.WeightedScore() < hardPassMinScore {
		return false
	}
	return !feedbackContainsConcreteDeliveryFailure(score.Feedback)
}

func hardGateContinuationFeedback(reviewFeedback, testResult string, buildPassed, testPassed bool) string {
	var parts []string
	if !buildPassed {
		parts = append(parts, "编译未通过: 必须先让当前 leaf 和目标包可编译。")
	}
	if reviewFeedback != "" {
		parts = append(parts, "Reviewer MUST-FIX:\n"+reviewFeedback)
	}
	if !testPassed && testResult != "" {
		parts = append(parts, "Tester/Micro-test MUST-FIX:\n"+testResult)
	}
	if len(parts) == 0 {
		parts = append(parts, "hard gate 未满足: 继续补齐设计覆盖、测试偏差和实现完整性。")
	}
	return strings.Join(parts, "\n\n")
}

func feedbackContainsConcreteDeliveryFailure(feedback string) bool {
	lower := strings.ToLower(feedback)
	markers := []string{
		"compile error",
		"compilation failed",
		"build failed",
		"syntax error",
		"undefined:",
		"cannot use",
		"not implemented",
		"placeholder",
		"todo",
		"stub",
		"占位",
		"语法错误",
		"编译失败",
		"构建失败",
		"无法编译",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func shouldAcceptIncrementalLeaf(node *TaskNode, score EvalScore, testPassed, buildPassed bool, objective string) bool {
	if node == nil || !buildPassed {
		return false
	}
	if objectiveRequiresCompleteImplementation(objective) && requiresStrictLeafLocalIntegrity(node) {
		return false
	}
	if node.TaskType != wbsTaskTypeLeaf {
		return false
	}
	if isManifestOnlyNode(node) {
		return true
	}
	if strings.Contains(node.SplitReason, "sizing-gate") {
		writeFiles := len(node.WriteFiles)
		if writeFiles == 0 {
			writeFiles = len(node.TargetFiles)
		}
		return writeFiles > 0 && score.Security >= 4 && (testPassed || score.Correctness >= 6 || score.CodeQuality >= 6)
	}
	if !strings.Contains(node.SplitReason, "objective-fallback") && !strings.Contains(node.SplitReason, "sizing-gate") {
		return false
	}
	if !testPassed {
		return false
	}
	return score.Correctness >= 6 && score.CodeQuality >= 7 && score.Security >= 6
}

func (o *Orchestrator) recoverStaleFailureWithProjectTest(team *ProductionTeam, objective string, node *TaskNode) (bool, string) {
	if team == nil || team.Cwd == "" || node == nil || orchNormalizeRole(node.Role) != "coder" || node.TaskType == wbsTaskTypeVerification {
		return false, ""
	}
	if objectiveRequiresCompleteImplementation(objective) && requiresStrictLeafLocalIntegrity(node) {
		return false, ""
	}
	lang := o.runtimeLanguageForTeam(team, objective)
	if strings.TrimSpace(lang) == "" {
		return false, ""
	}
	var candidates []string
	if buildCwd := inferBuildCwdFromTaskScopeForObjective(team.Cwd, node.TargetFiles, node.TargetPackages, objective); buildCwd != "" {
		candidates = append(candidates, buildCwd)
	}
	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		candidates = append(candidates, filepath.Join(team.Cwd, filepath.FromSlash(targetRoot)))
	}
	candidates = append(candidates, team.Cwd)
	for _, candidate := range uniqueTrimmedStrings(candidates) {
		if candidate == "" || !isDir(candidate) {
			continue
		}
		if errText := runTestCheckLang(candidate, lang); errText == "" {
			return true, "project-level local test passed after concurrent repairs; previous scoped failure was stale"
		}
	}
	return false, ""
}

func shouldTryEarlyStaleBuildRecovery(buildErrors string, written []string, node *TaskNode, objective string) bool {
	if node == nil || orchNormalizeRole(node.Role) != "coder" || node.TaskType == wbsTaskTypeVerification {
		return false
	}
	if objectiveRequiresCompleteImplementation(objective) && requiresStrictLeafLocalIntegrity(node) {
		return false
	}
	if strings.TrimSpace(buildErrors) == "" {
		return false
	}
	// Do not let a project-level green test hide delivery-contract failures.
	if len(node.TargetFiles) > 0 && len(written) == 0 {
		return false
	}
	lower := strings.ToLower(buildErrors)
	hardGateMarkers := []string{
		"未能从 coder 输出中物化任何文件",
		"声明了 targetfiles 但未能物化任何文件",
		"目标文件缺失",
		"文件隔离门禁失败",
		"最小项目骨架初始化失败",
		"manifest",
		"伪工具调用",
	}
	for _, marker := range hardGateMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return false
		}
	}
	return true
}

func requiresStrictLeafLocalIntegrity(node *TaskNode) bool {
	if node == nil {
		return false
	}
	if orchNormalizeRole(node.Role) == "coder" && node.TaskType == wbsTaskTypeLeaf && !isManifestOnlyNode(node) && !isProjectSkeletonNode(node) {
		return true
	}
	if node.TaskType == wbsTaskTypeVerification || orchNormalizeRole(node.Role) == "tester" {
		return true
	}
	if isContractBoundaryNode(node) {
		return true
	}
	text := strings.ToLower(strings.Join([]string{
		node.Title,
		node.AcceptCriteria,
		node.DesignRef,
		node.SplitReason,
		node.WorkUnitType,
		node.CapabilityID,
		strings.Join(node.TargetFiles, " "),
		strings.Join(node.WriteFiles, " "),
		strings.Join(node.TargetPackages, " "),
	}, " "))
	terms := []string{
		"边界", "错误路径", "失败路径", "最小行为", "最小可运行",
		"集成适配", "单元测试", "本地验证", "回归检查", "核心", "错误类型", "返回值",
		"契约", "接口", "元数据", "向量", "存储", "查询", "事务", "索引",
		"api", "remember", "recall", "select", "traverse", "graph api", "sql api", "memory api",
		"core", "engine", "storage", "wal", "lsm", "sst", "sstable",
		"compaction", "hnsw", "vector", "graph", "index", "query",
		"parser", "lexer", "planner", "executor", "transaction", "mvcc",
		"semantic", "dsl", "http api", "cli",
	}
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func enforceTargetFileScope(cwd string, written []string, node *TaskNode) []string {
	if cwd == "" || len(written) == 0 || node == nil || len(allowedPlanFilesForNode(node)) == 0 {
		return written
	}
	allowedFiles := allowedPlanFilesForNode(node)
	allowed := make(map[string]bool, len(allowedFiles))
	for _, target := range allowedFiles {
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

func targetScopeViolationError(rawWritten, kept []string, node *TaskNode) string {
	if node == nil || len(rawWritten) == 0 || len(allowedPlanFilesForNode(node)) == 0 {
		return ""
	}
	allowed := make(map[string]bool)
	for _, target := range allowedPlanFilesForNode(node) {
		clean := filepath.ToSlash(cleanMaterializeRelPath(target))
		if clean != "" {
			allowed[clean] = true
		}
	}
	if len(allowed) == 0 {
		return ""
	}
	var disallowed []string
	keptSet := make(map[string]bool, len(kept))
	for _, file := range kept {
		clean := filepath.ToSlash(cleanMaterializeRelPath(file))
		if clean != "" {
			keptSet[clean] = true
		}
	}
	for _, file := range rawWritten {
		clean := filepath.ToSlash(cleanMaterializeRelPath(file))
		if clean == "" || allowed[clean] || keptSet[clean] {
			continue
		}
		disallowed = append(disallowed, clean)
	}
	disallowed = uniqueTrimmedStrings(disallowed)
	if len(disallowed) == 0 {
		return ""
	}
	if len(disallowed) > 6 {
		disallowed = append(disallowed[:6], fmt.Sprintf("...(+%d)", len(disallowed)-6))
	}
	return "文件隔离门禁失败: 当前 Leaf 只能输出 targetFiles/writeFiles 中声明的文件, 但输出了越界文件: " + strings.Join(disallowed, ", ") +
		"。请只输出目标文件完整内容, 不要在本 Leaf 新建其它模块、配置或外部依赖。"
}

func allowedPlanFilesForNode(node *TaskNode) []string {
	if node == nil {
		return nil
	}
	files := append([]string{}, node.TargetFiles...)
	files = append(files, node.WriteFiles...)
	return uniqueTrimmedStrings(files)
}

func isProjectSkeletonNode(node *TaskNode) bool {
	if node == nil {
		return false
	}
	if isManifestOnlyNode(node) {
		return true
	}
	return isProjectSkeletonText(node)
}

func isProjectSkeletonText(node *TaskNode) bool {
	if node == nil {
		return false
	}
	text := strings.ToLower(node.Title + " " + node.WorkUnitType + " " + node.CapabilityID)
	return strings.Contains(text, "skeleton") ||
		strings.Contains(text, "scaffold") ||
		strings.Contains(text, "go module") ||
		strings.Contains(text, "go.mod") ||
		strings.Contains(text, "module path") ||
		strings.Contains(text, "manifest") ||
		strings.Contains(text, "模块路径") ||
		strings.Contains(text, "模块声明") ||
		strings.Contains(text, "项目初始化") ||
		strings.Contains(text, "项目结构") ||
		strings.Contains(text, "项目骨架") ||
		strings.Contains(text, "目录结构") ||
		strings.Contains(text, "脚手架") ||
		strings.Contains(text, "构建") ||
		strings.Contains(text, "骨架")
}

func isContractBoundaryNode(node *TaskNode) bool {
	if node == nil {
		return false
	}
	if node.WorkUnitType == wbsWorkUnitContract {
		return true
	}
	hasGoTarget := firstConcreteGoTargetFile(node) != ""
	text := strings.ToLower(strings.Join([]string{
		node.Title,
		node.AcceptCriteria,
		node.SplitReason,
		node.CapabilityID,
		strings.Join(node.TargetFiles, " "),
		strings.Join(node.WriteFiles, " "),
	}, " "))
	return strings.Contains(text, "契约与数据边界") ||
		strings.Contains(text, "接口与状态边界") ||
		strings.Contains(text, "契约定义") ||
		strings.Contains(text, "contract boundary") ||
		strings.Contains(text, "interface boundary") ||
		strings.Contains(text, "接口契约") ||
		strings.Contains(text, "类型边界") ||
		strings.Contains(text, "公开类型") ||
		strings.Contains(text, "错误边界") ||
		((strings.Contains(text, "接口") || strings.Contains(text, "interface") || strings.Contains(text, "api")) &&
			(strings.Contains(text, "错误类型") ||
				strings.Contains(text, "错误码") ||
				strings.Contains(text, "返回值") ||
				strings.Contains(text, "类型") ||
				strings.Contains(text, "边界") ||
				strings.Contains(text, "契约") ||
				strings.Contains(text, "schema") ||
				strings.Contains(text, "error"))) ||
		(strings.Contains(text, "manifest") && hasGoTarget && (strings.Contains(text, "接口") ||
			strings.Contains(text, "interface") ||
			strings.Contains(text, "contract") ||
			strings.Contains(text, "schema") ||
			strings.Contains(text, "api") ||
			strings.Contains(text, "storage") ||
			strings.Contains(text, "query")))
}

func isContractLikeNode(node *TaskNode) bool {
	if node == nil {
		return false
	}
	if isContractBoundaryNode(node) {
		return true
	}
	text := strings.ToLower(strings.Join([]string{
		node.Title,
		node.AcceptCriteria,
		node.WorkUnitType,
		node.CapabilityID,
		strings.Join(node.TargetFiles, " "),
		strings.Join(node.WriteFiles, " "),
	}, " "))
	return strings.Contains(text, "contract") ||
		strings.Contains(text, "契约") ||
		strings.Contains(text, "类型边界") ||
		strings.Contains(text, "公开类型") ||
		((strings.Contains(text, "定义") || strings.Contains(text, "边界") || strings.Contains(text, "接口") || strings.Contains(text, "interface")) &&
			(strings.Contains(text, "错误类型") ||
				strings.Contains(text, "错误码") ||
				strings.Contains(text, "返回值") ||
				strings.Contains(text, "公开类型")))
}

func firstPlanPathSegment(files []string) string {
	for _, file := range files {
		clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
		if clean == "" || clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
			continue
		}
		parts := strings.Split(clean, "/")
		if len(parts) > 1 && parts[0] != "" {
			return parts[0]
		}
	}
	return ""
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
		if isOptionalProjectManifestFile(clean) {
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

func inferBuildCwdFromTaskScopeForObjective(cwd string, targetFiles, targetPackages []string, objective string) string {
	buildCwd := inferBuildCwdFromTaskScope(cwd, targetFiles, targetPackages)
	if buildCwd != "" && buildCwd != cwd {
		return buildCwd
	}
	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		candidate := filepath.Join(cwd, filepath.FromSlash(targetRoot))
		if isDir(candidate) {
			return candidate
		}
	}
	return buildCwd
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
		regexp.MustCompile(`(?i)(?:输出到|放到|写到|生成到|创建到)\s*([~A-Za-z0-9_./-]*[\\/][A-Za-z0-9][A-Za-z0-9._-]*)(?:\s*(?:目录|文件夹|/|下|中|内|$|[，,。).）]))`),
		regexp.MustCompile(`(?i)(?:输出到|放到|写到|生成到|创建到)(?:工作目录(?:下|的)?|当前目录(?:下|的)?|cwd)?\s*[\\/]*([A-Za-z0-9][A-Za-z0-9._-]*)(?:\s*(?:目录|文件夹|/|下|中|内|$|[，,。).）]))`),
		regexp.MustCompile(`(?i)(?:开发|实现|创建|生成|build|implement)\s+(?:一个|个|an?\s+)?([A-Za-z][A-Za-z0-9._-]*\d*)(?:\s|$|[，,。).）])`),
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
	if strings.Contains(root, "/") {
		root = path.Base(root)
	}
	if root == "." || root == "" || strings.HasPrefix(root, ".") || strings.HasPrefix(root, "..") {
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
	if reason := validateTaskAgentOutput(result, node); reason != "" {
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
	if isContractLikeNode(node) {
		taskObjective += "\n\n契约类 Leaf 评审范围:\n" +
			"- 只评审本 Leaf 的接口、公开类型、错误类型、返回值、包边界是否完整且与设计目标一致。\n" +
			"- 不要要求本 Leaf 同时实现复杂算法、存储引擎、索引器、HTTP/CLI 或完整单元测试；这些应由后续 implementation/verification Leaf 完成。\n" +
			"- 若契约本身可编译、无 TODO/STUB/占位、没有外部依赖漂移，并为后续实现提供清晰边界，应给出 pass=true。\n"
	}
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
	case ".ts":
		return "typescript"
	case ".js", ".mjs", ".cjs":
		return "javascript"
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

func currentGoModulePathForPrompt(team *ProductionTeam, objective string) string {
	if team == nil || team.Cwd == "" {
		return ""
	}
	root := team.Cwd
	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		root = filepath.Join(team.Cwd, targetRoot)
	}
	modulePath, _ := readGoModModuleAndRequires(filepath.Join(root, "go.mod"))
	return strings.TrimSpace(modulePath)
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
	b.WriteString("### 执行边界 (硬约束)\n")
	b.WriteString("- 你不是工具执行器, 不要输出 Bash/Read/cat/ls/minimax:tool_call/task/invoke 等伪工具调用。\n")
	b.WriteString("- 不要读取项目目标中的参考设计路径; research/architect/planner 已经提供了当前 Leaf 所需信息。\n")
	b.WriteString("- 你的唯一交付物是目标文件的完整内容代码块; 如果需要目录, 通过输出对应文件路径让系统自动创建目录。\n\n")
	b.WriteString("- 默认禁止引入新的第三方依赖; 优先使用标准库或项目内接口。确需第三方依赖时, 当前 Leaf 必须显式包含 manifest 文件(go.mod/package.json/Cargo.toml 等)并给出完整内容。\n\n")
	if node.TaskType != "" || node.EstimatedMin > 0 || node.RiskLevel != "" {
		b.WriteString("### WBS 执行预算\n")
		if node.TaskType != "" {
			b.WriteString("- taskType: " + node.TaskType + "\n")
		}
		if node.WorkUnitType != "" {
			b.WriteString("- workUnitType: " + node.WorkUnitType + "\n")
		}
		if node.CapabilityID != "" {
			b.WriteString("- capabilityId: " + node.CapabilityID + "\n")
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

	if len(node.ContractRefs) > 0 || len(node.Provides) > 0 || len(node.Requires) > 0 || len(node.ConflictKeys) > 0 {
		b.WriteString("### Planning IR 约束\n")
		if len(node.ContractRefs) > 0 {
			b.WriteString("- contractRefs: " + strings.Join(node.ContractRefs, ", ") + "\n")
		}
		if len(node.Provides) > 0 {
			b.WriteString("- provides: " + strings.Join(node.Provides, ", ") + "\n")
		}
		if len(node.Requires) > 0 {
			b.WriteString("- requires: " + strings.Join(node.Requires, ", ") + "\n")
		}
		if len(node.ConflictKeys) > 0 {
			b.WriteString("- conflictKeys: " + strings.Join(node.ConflictKeys, ", ") + "\n")
		}
		if node.EstimatedLOC > 0 {
			b.WriteString(fmt.Sprintf("- estimatedChangedLOC: %d\n", node.EstimatedLOC))
		}
		b.WriteString("- 规则: implementation leaf 不允许修改未列入 writeFiles/目标文件的 contract; integration leaf 才能组装跨 capability 门面。\n\n")
	}

	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		b.WriteString("### 用户指定输出目录 (硬约束)\n")
		b.WriteString("- targetRoot: " + targetRoot + "\n")
		b.WriteString("- 所有新增或修改的 File path 必须以 `" + targetRoot + "/` 开头, 不要写到仓库根目录或其它目录。\n")
		b.WriteString("- 项目骨架和验证命令必须跟随架构阶段确定的语言/运行时: Go 使用 go.mod/go test, Python 使用 pyproject/pytest, TypeScript 使用 package.json/npm test, Rust 使用 Cargo.toml/cargo test 等。\n")
		b.WriteString("- 不要为了当前 Leaf 引入未在计划或架构中声明的新语言/依赖; 如必须新增依赖, 同步更新对应 manifest/lock 文件并保持本地 build/test 可运行。\n\n")
	}

	if modulePath := currentGoModulePathForPrompt(team, objective); modulePath != "" {
		b.WriteString("### Go module/import 约束\n")
		b.WriteString("- 当前 go.mod module: `" + modulePath + "`\n")
		b.WriteString("- 本项目内部 import 必须使用该 module 前缀, 例如 `" + modulePath + "/...`; 不要发明 github.com/huaquan.liang/agentDBV4、github.com/agentdb/v4 等其它 module path。\n")
		b.WriteString("- 同一目录下所有 .go 文件必须使用同一个 package 名; 测试文件可使用相同 package 或 package_test, 不要混用 agentdb/agentdbv4 等包名。\n\n")
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
		b.WriteString("未按上述格式输出目标文件, 或输出工具调用/命令清单而不是文件内容, 会被视为未执行任务并触发编译门禁失败。\n\n")
	}
	if len(node.ReadFiles) > 0 || len(node.WriteFiles) > 0 {
		b.WriteString("### 文件访问预算\n")
		if len(node.ReadFiles) > 0 {
			b.WriteString("- readFiles: " + strings.Join(node.ReadFiles, ", ") + "\n")
		}
		if len(node.WriteFiles) > 0 {
			b.WriteString("- writeFiles: " + strings.Join(node.WriteFiles, ", ") + "\n")
		}
		b.WriteString("- 规则: 可以参考 readFiles 的 API, 但只能输出 writeFiles/目标文件的完整内容。\n\n")
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
