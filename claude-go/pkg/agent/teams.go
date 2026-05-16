// Production Agent Teams — 生产级多 Agent 协作系统 (v2)。
//
// 架构改进 (参考业界最佳实践):
//
//  1. TaskTracker 集成 — 复用 claude-go V2 Task 系统, 不重复造轮子。
//     阶段状态通过 TaskCreate/TaskUpdate 管理, LLM 可见。
//
//  2. Blackboard 共享上下文 (bMAS 架构) — Agent 间通过黑板间接通信,
//     所有 Agent 执行前读取完整黑板, 执行后写回结果。
//     参考: "bMAS: Blackboard LLM Multi-Agent System" (2025)
//
//  3. Structured Handoff — 阶段间传递结构化上下文而非原始文本。
//     参考: AG2 Framework (2026), Anthropic Harness Design (2026)
//
//  4. 意图识别层 — 用户发送中文自然语言即可驱动团队,
//     无需记忆 /team 命令。IntentRecognizer 自动拆解执行。
//
//     ┌────────────────────────────────────────────────────┐
//     │ ProductionTeamManager (进程级单例)                  │
//     │  - TaskTracker: 复用 V2 Task (LLM 可通过 TaskList 看到)│
//     │  - 文件持久化: .claude/teams/{name}/                │
//     │  - 通知回调: 向飞书推送进度                         │
//     ├────────────────────────────────────────────────────┤
//     │ ProductionTeam                                     │
//     │  - Blackboard: 共享黑板 (bMAS 架构)                 │
//     │  - Workflow: pipeline / fanout / adversarial        │
//     │  - V2 TaskIDs: 每个阶段对应一个 V2 Task             │
//     └────────────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/types"
)

// TaskTracker 抽象 V2 任务管理, 与 builtin.TaskStore 通过 duck typing 对接。
type TaskTracker interface {
	AddTask(subject, description, owner string) (string, error)
	SetTaskStatus(id, status string) error
}

// DAGTaskTracker 扩展 TaskTracker, 暴露 V2 TaskStore 的 DAG 调度能力。
// 统一 Orchestrator 和 Swarm 的 DAG 实现, 消除三套重复的拓扑排序/就绪队列。
// 实现者: builtin.TaskStore (已有全部方法)
type DAGTaskTracker interface {
	TaskTracker
	AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error)
	ReadyTasks() []DAGTaskSummary
	SetTaskStatusAndUnblock(id, status string) (int, error)
	GetAllTasks() []DAGTaskSummary
	ReevaluateBlockedTasks() int
}

// DAGTaskSummary 面向调度器的任务摘要 (与 builtin.TaskSummary 结构对齐)。
type DAGTaskSummary struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Owner       string   `json:"owner"`
	DependsOn   []string `json:"dependsOn,omitempty"`
	Priority    int      `json:"priority,omitempty"`
}

// TaskStoreForDAG 是 builtin.TaskStore 暴露给 agent 层的最小接口。
// 解决 builtin.TaskStore.ReadyTasks() 返回 builtin.TaskSummary 而非 agent.DAGTaskSummary 的类型不匹配。
type TaskStoreForDAG interface {
	TaskTracker
	AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error)
	ReadyTasksRaw() []map[string]interface{}
	SetTaskStatusAndUnblock(id, status string) (int, error)
}

// TaskStoreDAGAdapter 将 builtin.TaskStore 适配为 DAGTaskTracker。
// builtin.TaskStore 已实现所有方法，仅 ReadyTasks 返回类型不同 ([]builtin.TaskSummary vs []DAGTaskSummary)。
type TaskStoreDAGAdapter struct {
	store interface {
		AddTask(subject, description, owner string) (string, error)
		SetTaskStatus(id, status string) error
		AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error)
		SetTaskStatusAndUnblock(id, status string) (int, error)
		ReevaluateBlockedTasks() int
	}
	readyFunc  func() []DAGTaskSummary
	getAllFunc func() []DAGTaskSummary
}

// NewTaskStoreDAGAdapter 创建适配器。
// readyFn 由调用方提供，负责从 builtin.TaskStore.ReadyTasks() 转换类型。
func NewTaskStoreDAGAdapter(
	store interface {
		AddTask(subject, description, owner string) (string, error)
		SetTaskStatus(id, status string) error
		AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error)
		SetTaskStatusAndUnblock(id, status string) (int, error)
		ReevaluateBlockedTasks() int
	},
	readyFn func() []DAGTaskSummary,
	getAllFn func() []DAGTaskSummary,
) *TaskStoreDAGAdapter {
	return &TaskStoreDAGAdapter{store: store, readyFunc: readyFn, getAllFunc: getAllFn}
}

func (a *TaskStoreDAGAdapter) AddTask(subject, description, owner string) (string, error) {
	return a.store.AddTask(subject, description, owner)
}
func (a *TaskStoreDAGAdapter) SetTaskStatus(id, status string) error {
	return a.store.SetTaskStatus(id, status)
}
func (a *TaskStoreDAGAdapter) AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error) {
	return a.store.AddTaskWithDeps(subject, description, owner, dependsOn, priority)
}
func (a *TaskStoreDAGAdapter) ReadyTasks() []DAGTaskSummary {
	return a.readyFunc()
}
func (a *TaskStoreDAGAdapter) SetTaskStatusAndUnblock(id, status string) (int, error) {
	return a.store.SetTaskStatusAndUnblock(id, status)
}
func (a *TaskStoreDAGAdapter) GetAllTasks() []DAGTaskSummary {
	if a.getAllFunc != nil {
		return a.getAllFunc()
	}
	return nil
}
func (a *TaskStoreDAGAdapter) ReevaluateBlockedTasks() int {
	return a.store.ReevaluateBlockedTasks()
}

// DreamRecorder Dreaming 记录接口, 解耦 dreaming 包依赖。
// 实现者: dreaming.Dreamer (通过 duck typing)。
type DreamRecorder interface {
	RecordSession(record DreamSessionRecord)
	AfterQuery(ctx context.Context)
}

// DreamSessionRecord 与 dreaming.SessionRecord 结构对齐。
type DreamSessionRecord struct {
	ChatID  string
	EndTime time.Time
	Summary string
}

// CreateAgentFunc 创建 Agent 运行器的工厂函数。
type CreateAgentFunc func(ctx context.Context, role, systemPrompt string) (AgentRunner, error)

// AgentRunner 后台 Agent 执行接口
type AgentRunner interface {
	Execute(ctx context.Context, userPrompt string) (string, error)
}

// NotifyFunc 通知回调 (向飞书推送消息)
type NotifyFunc func(chatID, message string)

// MediaNotifyFunc 多媒体通知回调 (发送图片/文件到飞书)
type MediaNotifyFunc func(chatID string, mediaData []byte, filename, mediaType string) error

// TeamStatus 团队状态
type TeamStatus string

const (
	TeamStatusCreated   TeamStatus = "created"
	TeamStatusRunning   TeamStatus = "running"
	TeamStatusCompleted TeamStatus = "completed"
	// TeamStatusDeliveredWithRemediation means final deterministic gates passed after one
	// or more failed intermediate stages were remediated by E2E/local repair.
	TeamStatusDeliveredWithRemediation TeamStatus = "delivered_with_remediation"
	TeamStatusFailed                   TeamStatus = "failed"
	TeamStatusStopped                  TeamStatus = "stopped"
)

// AgentStatus Agent 状态
type AgentStatus string

const (
	AgentStatusIdle      AgentStatus = "idle"
	AgentStatusRunning   AgentStatus = "running"
	AgentStatusCompleted AgentStatus = "completed"
	AgentStatusFailed    AgentStatus = "failed"
)

// TaskStatus 任务状态
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
)

// MemoryWriter 写入记忆的抽象接口 (解耦对 memory 包的依赖)。
type MemoryWriter interface {
	AddTeamMemory(teamName, workflow, objective, summary string)
}

// ConcurrencySuggestor 基于 LLM API 流控状态建议并发度 (解耦 api.RateLimitGuard)。
type ConcurrencySuggestor interface {
	SuggestConcurrency() int
	CurrentMaxParallel() int
}

// ProductionTeamManager 生产级团队管理器
type ProductionTeamManager struct {
	teams       map[string]*ProductionTeam
	mu          sync.RWMutex
	baseDir     string
	cwd         string // 项目工作目录 (传递给团队, 用于编译验证)
	factory     CreateAgentFunc
	notify      NotifyFunc
	mediaNotify MediaNotifyFunc
	taskTracker TaskTracker          // 复用 V2 Task 系统
	pool        *AgentPool           // Agent 池 (动态扩缩)
	llm         LLMClient            // LLM 客户端 (蜂群分解)
	evolution   *EvolutionEngine     // 自动进化引擎
	dreamer     DreamRecorder        // Dreaming 接口 (覆盖 team agent 会话)
	roles       *RoleRegistry        // 角色注册表
	memWriter   MemoryWriter         // 记忆写入 (团队完成后写入高权重记忆)
	metrics     *metrics.Collector   // 持续观测指标采集器
	concurrency ConcurrencySuggestor // 动态并发建议 (基于 API 流控状态)

	planCfgResolver *PlanConfigResolver // 模型/连接参数解析器 (可选)
	hookRunner      *hooks.Runner       // Hook 执行器 (TeammateIdle / TaskCompleted)

	// starting 防止并发 resume/run 同一个团队 (race condition 保护)
	startingMu sync.Mutex
	starting   map[string]bool // key=team name, value=是否正在启动中
}

// TeamManagerConfig 团队管理器配置。
type TeamManagerConfig struct {
	BaseDir            string
	Cwd                string // 项目工作目录 (用于编译验证)
	Factory            CreateAgentFunc
	Notify             NotifyFunc
	MediaNotify        MediaNotifyFunc
	TaskTracker        TaskTracker
	Pool               *AgentPool
	LLM                LLMClient
	Evolution          *EvolutionEngine
	Dreamer            DreamRecorder
	Roles              *RoleRegistry
	MemWriter          MemoryWriter
	Concurrency        ConcurrencySuggestor
	PlanConfigResolver *PlanConfigResolver // 模型/连接参数解析器 (可选)
	HookConfigs        []types.HookConfig  // Hook 配置 (用于 TeammateIdle / TaskCompleted 等)
}

// SetMemoryWriter 注入记忆写入器 (在 Bot 初始化后调用)。
func (ptm *ProductionTeamManager) SetMemoryWriter(mw MemoryWriter) {
	ptm.memWriter = mw
}

// Metrics 返回内部指标采集器 (供外部模块注入使用)。
func (ptm *ProductionTeamManager) Metrics() *metrics.Collector {
	return ptm.metrics
}

// SetCwd updates the default working directory used by subsequently created teams.
func (ptm *ProductionTeamManager) SetCwd(cwd string) {
	if ptm == nil || strings.TrimSpace(cwd) == "" {
		return
	}
	ptm.mu.Lock()
	ptm.cwd = cwd
	ptm.mu.Unlock()
}

// NewProductionTeamManager 创建生产级团队管理器。
func NewProductionTeamManager(cfg TeamManagerConfig) *ProductionTeamManager {
	if cfg.Notify == nil {
		cfg.Notify = func(_, _ string) {}
	}
	// 指标采集器: 从 BaseDir 推导 stateDir (teams 目录的父目录)
	stateDir := filepath.Dir(cfg.BaseDir)
	ptm := &ProductionTeamManager{
		teams:           make(map[string]*ProductionTeam),
		baseDir:         cfg.BaseDir,
		cwd:             cfg.Cwd,
		factory:         cfg.Factory,
		notify:          cfg.Notify,
		mediaNotify:     cfg.MediaNotify,
		taskTracker:     cfg.TaskTracker,
		pool:            cfg.Pool,
		llm:             cfg.LLM,
		evolution:       cfg.Evolution,
		dreamer:         cfg.Dreamer,
		roles:           cfg.Roles,
		metrics:         metrics.NewCollector(stateDir),
		concurrency:     cfg.Concurrency,
		planCfgResolver: cfg.PlanConfigResolver,
		starting:        make(map[string]bool),
	}
	if len(cfg.HookConfigs) > 0 {
		ptm.hookRunner = hooks.NewRunner(cfg.HookConfigs, "")
		// Agent 释放时触发 TeammateIdle Hook
		if ptm.pool != nil {
			ptm.pool.OnRelease = func(agent *PooledAgent) {
				if ptm.hookRunner != nil {
					ptm.hookRunner.ExecuteTeammateIdleHooks(agent.Role)
				}
			}
		}
	}
	ptm.loadPersistedTeams()
	return ptm
}

// tryStartTeam 原子性检查: 团队是否已在启动中, 若否则标记为启动中。
// 返回 true 表示本次请求获得了启动权, false 表示已有其他请求在启动 (应跳过)。
// 修复: 两个并发 resume 请求同时看到 stopped 状态, 都继续执行导致重复工作流。
// 解决: manager 级的 starting 标志, 保证同一团队同一时刻只有一个启动流程。
func (ptm *ProductionTeamManager) tryStartTeam(name string) bool {
	ptm.startingMu.Lock()
	defer ptm.startingMu.Unlock()
	if ptm.starting[name] {
		return false // 已有其他请求在启动中
	}
	ptm.starting[name] = true
	return true
}

// clearStarting 清除团队的启动标记 (工作流 goroutine 结束后调用)
func (ptm *ProductionTeamManager) clearStarting(name string) {
	ptm.startingMu.Lock()
	delete(ptm.starting, name)
	ptm.startingMu.Unlock()
}

// ProductionTeam 生产级团队
type ProductionTeam struct {
	Name       string              `json:"name"`
	Workflow   string              `json:"workflow"`
	Objective  string              `json:"objective"`
	ChatID     string              `json:"chatId"`
	Status     TeamStatus          `json:"status"`
	Agents     map[string]*BGAgent `json:"agents"`
	Stages     []StageResult       `json:"stages"`
	TaskIDs    map[string]string   `json:"taskIds,omitempty"` // stageName → V2 taskID
	CreatedAt  time.Time           `json:"createdAt"`
	StartedAt  time.Time           `json:"startedAt,omitempty"`
	FinishedAt time.Time           `json:"finishedAt,omitempty"`
	Mailbox    []MailMessage       `json:"mailbox,omitempty"`
	Error      string              `json:"error,omitempty"`
	Cwd        string              `json:"cwd,omitempty"`      // 工作目录 (用于编译验证和文件清单)
	Language   string              `json:"language,omitempty"` // 编程语言 ("go","cpp","rust","python"), 空=""go"

	Blackboard *Blackboard `json:"-"` // 共享黑板 (不序列化, 独立持久化)
	mu         sync.Mutex
	cancel     context.CancelFunc
	mgr        *ProductionTeamManager
	dataDir    string
	doneCh     chan struct{} // 关闭信号: 工作流执行完毕时 close

	// EngineRunning 标记 orchestrated 引擎是否正在运行 (供 Coordinator 心跳检测使用)。
	// 在 executeOrchestrated() 开始时设为 true, 结束时设为 false。
	EngineRunning bool `json:"-"`
}

// metrics 返回 team 所属 manager 的指标采集器 (可能为 nil)。
// Coordinator 在 stage 级别增量上报指标时使用。
func (t *ProductionTeam) metrics() *metrics.Collector {
	if t == nil || t.mgr == nil {
		return nil
	}
	return t.mgr.metrics
}

// SetLanguage 设置团队的编程语言 (go/cpp/rust/python)。
// 必须在 StartTeam 之前调用。影响编译门禁、文件物化和 prompt 模板。
func (t *ProductionTeam) SetLanguage(lang string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Language = lang
}

// WaitDone 阻塞直到团队执行完毕。如果 doneCh 尚未初始化则立刻返回。
func (t *ProductionTeam) WaitDone() {
	if t.doneCh != nil {
		<-t.doneCh
	}
}

// BGAgent 后台 Agent
type BGAgent struct {
	Name   string      `json:"name"`
	Role   string      `json:"role"`
	Status AgentStatus `json:"status"`
	Result string      `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// StageResult 阶段执行结果
type StageResult struct {
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Status    TaskStatus `json:"status"`
	Input     string     `json:"input,omitempty"`
	Output    string     `json:"output,omitempty"`
	Error     string     `json:"error,omitempty"`
	V2TaskID  string     `json:"v2TaskId,omitempty"` // 关联的 V2 Task ID
	StartedAt time.Time  `json:"startedAt,omitempty"`
	Duration  string     `json:"duration,omitempty"`
}

// MailMessage 邮箱消息
type MailMessage struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Content   string    `json:"content"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
}

// CreateTeam 创建团队
func (ptm *ProductionTeamManager) CreateTeam(name, workflow, objective, chatID string) (*ProductionTeam, error) {
	ptm.mu.Lock()
	defer ptm.mu.Unlock()

	if _, exists := ptm.teams[name]; exists {
		return nil, fmt.Errorf("团队 %q 已存在", name)
	}

	// 蜂群模式不需要预定义工作流
	if workflow != "swarm" {
		wf := GetWorkflow(workflow)
		if wf == nil {
			return nil, fmt.Errorf("未知工作流 %q, 可选: development, research, debate, swarm, finance, techblog, creative, creative-v2, novel-v2, novel-v3, trading-v2, predict, ml-training, app, game, code-review, testing, parenting, hiring", workflow)
		}
		_ = wf
	}

	dataDir := filepath.Join(ptm.baseDir, name)
	os.MkdirAll(dataDir, 0755)

	team := &ProductionTeam{
		Name:       name,
		Workflow:   workflow,
		Objective:  objective,
		ChatID:     chatID,
		Status:     TeamStatusCreated,
		Agents:     make(map[string]*BGAgent),
		TaskIDs:    make(map[string]string),
		CreatedAt:  time.Now(),
		Cwd:        ptm.cwd,
		Blackboard: NewBlackboard(name, dataDir),
		mgr:        ptm,
		dataDir:    dataDir,
	}

	// 蜂群模式: 初始 Agent 由 LLM 动态决定
	if workflow != "swarm" {
		wf := GetWorkflow(workflow)
		if wf != nil {
			for _, stage := range wf.Stages {
				team.Agents[stage.Role] = &BGAgent{
					Name:   stage.Role,
					Role:   stage.Role,
					Status: AgentStatusIdle,
				}
			}
		}
	}

	// 在黑板上写入初始上下文
	team.Blackboard.Write("objective", objective, "system", "context")
	team.Blackboard.Write("workflow", workflow, "system", "context")

	ptm.teams[name] = team
	team.persist()
	return team, nil
}

// CreateTeamWithUniquePrefix creates a team for quick-start flows and retries
// name collisions caused by persisted historical teams.
func (ptm *ProductionTeamManager) CreateTeamWithUniquePrefix(prefix, workflow, objective, chatID string) (*ProductionTeam, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	if prefix == "" {
		prefix = "team"
	}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
		if attempt > 0 {
			name = fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), attempt)
		}
		team, err := ptm.CreateTeam(name, workflow, objective, chatID)
		if err == nil {
			return team, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "已存在") {
			return nil, err
		}
		time.Sleep(time.Millisecond)
	}
	return nil, fmt.Errorf("创建唯一团队失败: %w", lastErr)
}

// RunTeam 启动团队执行。
// 如果团队之前因 LLM 限流/错误而失败, 重新激活时会从上次的检查点恢复 (跳过已完成的阶段)。
// 修复: 使用 starting 标志防止并发 run 导致的双重工作流执行。
func (ptm *ProductionTeamManager) RunTeam(name, objective string) error {
	ptm.mu.RLock()
	team, ok := ptm.teams[name]
	ptm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}

	team.mu.Lock()
	if team.Status == TeamStatusRunning {
		team.mu.Unlock()
		return fmt.Errorf("团队 %q 正在执行中", name)
	}

	isResume := team.Status == TeamStatusFailed && team.Objective == objective
	team.mu.Unlock()

	// 原子性检查: 是否已有其他请求在启动同一个团队
	if !ptm.tryStartTeam(name) {
		return fmt.Errorf("团队 %q 正在启动中，请稍后再试", name)
	}

	// 重新锁定 team.mu 修改状态
	team.mu.Lock()
	// 双重检查
	if team.Status == TeamStatusRunning {
		team.mu.Unlock()
		ptm.clearStarting(name)
		return fmt.Errorf("团队 %q 正在执行中", name)
	}

	team.Objective = objective
	team.Status = TeamStatusRunning
	team.StartedAt = time.Now()
	if !isResume {
		team.Stages = nil
	}
	team.Error = ""
	ctx, cancel := context.WithCancel(context.Background())
	team.cancel = cancel
	team.doneCh = make(chan struct{})
	team.mu.Unlock()

	// 更新黑板上的目标
	team.Blackboard.Write("objective", objective, "system", "context")
	team.persist()

	if isResume {
		ptm.notify(team.ChatID, fmt.Sprintf("♻️ 团队 **%s** 从检查点恢复执行\n目标: %s\n工作流: %s", name, objective, team.Workflow))
	} else {
		ptm.notify(team.ChatID, fmt.Sprintf("🚀 团队 **%s** 开始执行\n目标: %s\n工作流: %s", name, objective, team.Workflow))
	}

	go func() {
		defer ptm.clearStarting(name)
		defer func() { close(team.doneCh) }()
		ptm.executeWorkflow(ctx, team, isResume)
	}()
	return nil
}

// executeWorkflow 在后台执行工作流。isResume=true 时保留检查点, 从上次失败步骤继续。
func (ptm *ProductionTeamManager) executeWorkflow(ctx context.Context, team *ProductionTeam, isResume ...bool) {
	// 注入 trace, 确保团队全生命周期有唯一 traceID
	ctx = logging.WithTrace(ctx)
	ctx, endSpan := logging.WithSpan(ctx, "team."+team.Name+".execute")
	defer endSpan()
	logging.Event(ctx, "team.start", "team", team.Name, "workflow", team.Workflow, "objective", team.Objective)
	logging.IncrCounter("team.start." + team.Workflow)

	// 蜂群模式: 使用 SwarmOrchestrator
	if team.Workflow == "swarm" {
		ptm.executeSwarm(ctx, team)
		return
	}

	// 群体智能预测模式: 使用 SwarmIntelligenceEngine
	if team.Workflow == "predict" {
		ptm.executePrediction(ctx, team)
		return
	}

	wf := GetWorkflow(team.Workflow)
	if wf == nil {
		ptm.failTeam(team, "未知工作流: "+team.Workflow)
		return
	}

	// 使用 Coordinator 带重试和检查点执行
	coord := NewCoordinator(ptm.pool, ptm.taskTracker, ptm.notify, CoordinatorConfig{
		MaxRetries:    6, // 限流场景需要更多重试 (429 + AIMD 退避后可恢复)
		HeartbeatFreq: 30 * time.Second,
		DataDir:       team.dataDir,
		ChatID:        team.ChatID,
	})
	// 注入自动恢复回调: 当 Coordinator watchdog 检测到瞬态错误导致团队失败时,
	// 自动调用 ResumeTeam 从检查点恢复, 无需用户手动干预。
	coord.SetAutoResumeCallback(func(teamName string) {
		// 延迟 10 秒后恢复, 给 LLM API 限流/超时一些冷却时间
		time.Sleep(10 * time.Second)
		if err := ptm.ResumeTeam(teamName); err != nil {
			ptm.notify(team.ChatID, fmt.Sprintf("⚠️ 团队 **%s** 自动恢复失败: %v", teamName, err))
		}
	})
	resuming := len(isResume) > 0 && isResume[0]
	if !resuming {
		coord.ClearCheckpoints()
	} else {
		// 恢复模式: 保留已完成阶段的检查点, 从失败处继续
		restored := coord.CompletedCount()
		if restored > 0 {
			ptm.notify(team.ChatID, fmt.Sprintf("♻️ 检查点恢复: %d 个已完成阶段将跳过", restored))
		}
	}

	executor := &WorkflowExecutor{
		factory:         ptm.factory,
		planCfgResolver: ptm.planCfgResolver,
		notify:          ptm.notify,
		chatID:          team.ChatID,
		llm:             ptm.llm, // 供 swarm_intel.Engine 等直接 LLM 调用
		taskTracker:     ptm.taskTracker,
		evolution:       ptm.evolution,
		roles:           ptm.roles,
		metrics:         ptm.metrics,
		pool:            ptm.pool,
		checkpoints:     coord, // 注入 Coordinator 作为 CheckpointStore
		concurrency:     ptm.concurrency,
	}

	results, err := coord.RunWithRecovery(ctx, wf, team.Objective, team, executor)
	if err != nil {
		if ctx.Err() != nil {
			ptm.failTeam(team, "用户停止")
			return
		}
		ptm.failTeam(team, err.Error())
		return
	}

	// 检查是否有失败阶段: 默认 err==nil 但存在 TaskFailed 仍标记团队失败。
	// development 工作流例外: 若最终本地 E2E 门禁已通过 build/test, 则说明中间
	// leaf 的失败已被后置确定性门禁修复/兜底, 团队按交付成功处理并保留失败阶段供复盘。
	var failedStages []string
	for _, r := range results {
		if r.Status == TaskFailed {
			failedStages = append(failedStages, fmt.Sprintf("%s(%s): %s", r.Name, r.Role, r.Error))
		}
	}
	deliveryStatus := TeamStatusCompleted
	if len(failedStages) > 0 {
		if team.Workflow == "development" && hasPassingLocalE2EGate(results) && !hasBlockingDevelopmentFailure(results) {
			deliveryStatus = TeamStatusDeliveredWithRemediation
			ptm.notify(team.ChatID, fmt.Sprintf("🟡 团队 **%s** 存在 %d 个中间阶段失败, 但最终 E2E 本地门禁已通过 build/test, 按交付成功处理", team.Name, len(failedStages)))
		} else {
			errStr := fmt.Sprintf("%d 个阶段失败: %s", len(failedStages), strings.Join(failedStages, "; "))
			ptm.failTeam(team, errStr)
			return
		}
	}

	// Global Gates: compile, test, and consistency checks after workflow stages complete.
	//
	// 注: 当 compile / test gate 失败时, 跑 1-2 轮"修复尝试", 由 coder 角色
	// 拿到具体错误输出去修源码 (修 bug / 删重复声明 / 修 vet 警告 / 删死循环).
	// 修完后再跑 gate. 这样团队就有自我恢复能力, 不会因为单个 vet 警告或
	// 一处明显 bug 就把 30 分钟的工作直接判废.
	if team.Cwd != "" {
		if gateErr := ptm.tryGateWithRemediation(ctx, team, executor, "compile",
			ptm.runGlobalCompileGate, 2); gateErr != "" {
			ptm.failTeam(team, fmt.Sprintf("全局编译门禁失败: %s", gateErr))
			return
		}
		if gateErr := ptm.tryGateWithRemediation(ctx, team, executor, "test",
			ptm.runGlobalTestGate, 2); gateErr != "" {
			ptm.failTeam(team, fmt.Sprintf("全局测试门禁失败: %s", gateErr))
			return
		}
		if gateErr := ptm.runGlobalConsistencyCheck(team); gateErr != "" {
			ptm.failTeam(team, fmt.Sprintf("全局一致性检查失败: %s", gateErr))
			return
		}
	}

	team.mu.Lock()
	team.Status = deliveryStatus
	team.FinishedAt = time.Now()
	team.Stages = results
	team.mu.Unlock()
	team.persist()
	ptm.runTaskCompletedHooks(results)

	// 结构化运行报告 (可观测性: 供后续 AI 分析团队运行效果)
	report := logging.TeamRunReport{
		TeamName: team.Name, Workflow: team.Workflow, Objective: team.Objective,
		StartTime: team.StartedAt, EndTime: team.FinishedAt,
		DurationSec: team.FinishedAt.Sub(team.StartedAt).Seconds(),
		Status:      string(team.Status),
	}
	for _, r := range results {
		durSec := 0.0
		if d, err := time.ParseDuration(r.Duration); err == nil {
			durSec = d.Seconds()
		}
		report.Stages = append(report.Stages, logging.StageReport{
			Name: r.Name, Role: r.Role, DurationSec: durSec,
			Status: string(r.Status), OutputLen: len(r.Output), Error: r.Error,
		})
	}
	logging.LogTeamRun(ctx, report)
	logging.IncrCounter("team.complete." + team.Workflow)

	// 持续观测指标: 团队运行质量
	if ptm.metrics != nil {
		labels := map[string]string{"workflow": team.Workflow, "status": string(team.Status)}
		ptm.metrics.RecordRun("team", metrics.MTeamRunCount, 1, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamDurationSec, report.DurationSec, team.Name, labels)
		if isSuccessfulTeamStatus(team.Status) {
			ptm.metrics.RecordRun("team", metrics.MTeamSuccessCount, 1, team.Name, labels)
		} else {
			ptm.metrics.RecordRun("team", metrics.MTeamFailCount, 1, team.Name, labels)
		}
		// 阶段通过率
		total, passed := 0, 0
		totalOutLen := 0
		for _, s := range report.Stages {
			total++
			if s.Status == string(TaskCompleted) {
				passed++
			}
			totalOutLen += s.OutputLen
		}
		if total > 0 {
			ptm.metrics.RecordRun("team", metrics.MTeamStagePassRate, float64(passed)/float64(total), team.Name, labels)
			ptm.metrics.RecordRun("team", metrics.MTeamOutputAvgLen, float64(totalOutLen)/float64(total), team.Name, labels)
		}
	}

	// 触发进化学习 (DISTILL: 从轨迹中提炼经验) + 采集进化指标
	if ptm.evolution != nil {
		mc := ptm.metrics
		go func() {
			ptm.evolution.LearnFromTeam(context.Background(), team.Name)
			ptm.evolution.Consolidate()
			if mc != nil {
				ptm.evolution.CollectMetrics(mc)
			}
		}()
	}

	// 触发 Dreaming 记录 (覆盖 team agent 会话盲区)
	if ptm.dreamer != nil {
		for _, r := range results {
			ptm.dreamer.RecordSession(DreamSessionRecord{
				ChatID:  team.ChatID,
				EndTime: time.Now(),
				Summary: fmt.Sprintf("[Team:%s] [%s/%s] %s", team.Name, r.Name, r.Role, truncateResult(r.Output, 300)),
			})
		}
		// 团队完成后触发 Dreaming 检查 (解决仅有团队工作流时不触发的问题)
		ptm.dreamer.AfterQuery(context.Background())
	}

	// 持久化完整报告文件 (解决产出散落、无法检索的问题)
	reportPath := ptm.saveTeamReport(team, results)

	// 写入高权重记忆 (解决"失忆"问题: 团队名+目标+结果摘要可被 BM25 检索)
	if ptm.memWriter != nil {
		var stageSummary string
		for _, r := range results {
			if r.Output != "" {
				stageSummary += fmt.Sprintf("[%s/%s] %s\n", r.Name, r.Role, truncateResult(r.Output, 200))
			}
		}
		ptm.memWriter.AddTeamMemory(team.Name, team.Workflow, team.Objective, stageSummary)
	}

	var summary string
	for _, r := range results {
		if r.Output != "" {
			summary += fmt.Sprintf("\n\n**[%s]**\n%s", r.Role, truncateResult(r.Output, 500))
		}
	}
	reportNote := ""
	if reportPath != "" {
		reportNote = fmt.Sprintf("\n\n📄 **完整报告**: `%s`", reportPath)
	}
	ptm.notify(team.ChatID, fmt.Sprintf("✅ 团队 **%s** 执行完成 (耗时 %v)\n\n**成果汇总:**%s%s",
		team.Name, time.Since(team.StartedAt).Round(time.Second), summary, reportNote))

	// Creative 工作流: 提取 SVG/HTML 多媒体资产，通过媒体通道发送
	if ptm.mediaNotify != nil && (team.Workflow == "creative") {
		ptm.sendMediaAssets(team, results)
	}
}

func hasPassingLocalE2EGate(results []StageResult) bool {
	for i := len(results) - 1; i >= 0; i-- {
		r := results[i]
		if r.Name != "e2e-local-gate" {
			continue
		}
		return r.Status == TaskCompleted && strings.Contains(r.Output, "E2E 本地门禁通过")
	}
	return false
}

func hasBlockingDevelopmentFailure(results []StageResult) bool {
	for _, r := range results {
		if r.Status != TaskFailed {
			continue
		}
		text := strings.ToLower(r.Name + "\n" + r.Error + "\n" + r.Output)
		if strings.Contains(text, "hard gate") ||
			strings.Contains(text, "local verification failed") ||
			strings.Contains(text, "本地验证失败") ||
			strings.Contains(text, "级联阻塞") ||
			strings.Contains(text, "cascade") ||
			strings.Contains(text, "orchestrator 启动失败") ||
			strings.Contains(text, "wbs 解析失败") {
			return true
		}
	}
	return false
}

func isSuccessfulTeamStatus(status TeamStatus) bool {
	return status == TeamStatusCompleted || status == TeamStatusDeliveredWithRemediation
}

// runGlobalCompileGate runs a global compile check for the team.
// Returns empty string on success, error message on failure.
//
// 注: 用 exec.CommandContext + WithTimeout 包一层超时.
// 否则 AI 生成的 build.go / cgo 之类的死循环会让整个团队卡死.
func (ptm *ProductionTeamManager) runGlobalCompileGate(team *ProductionTeam) string {
	if team == nil || team.Cwd == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "build", "./...")
	cmd.Dir = team.Cwd
	out, err := cmd.CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("go build ./... timed out after 5m\n%s", string(out))
	}
	if err != nil {
		return fmt.Sprintf("go build ./... failed: %v\n%s", err, string(out))
	}
	return ""
}

// runGlobalTestGate runs a global test check with race detection for the team.
// Returns empty string on success, error message on failure.
//
// 注: 用 exec.CommandContext + WithTimeout 包一层超时.
// 否则 AI 生成的代码里有死循环 (比如 kmeans 中 k > len(vectors) 的 for{} ),
// 整个团队会卡在这条命令上数十分钟. 12 分钟的超时足够正常单元测试跑完,
// 又能在病态情况下及时止血.
func (ptm *ProductionTeamManager) runGlobalTestGate(team *ProductionTeam) string {
	if team == nil || team.Cwd == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "test", "-race", "-timeout", "120s", "./...")
	cmd.Dir = team.Cwd
	out, err := cmd.CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("go test -race ./... timed out after 12m (very likely an infinite loop in generated code)\n%s", string(out))
	}
	if err != nil {
		return fmt.Sprintf("go test -race ./... failed: %v\n%s", err, string(out))
	}
	return ""
}

// runGlobalConsistencyCheck runs a global consistency check using the ContractStore.
// Returns empty string on success, error message on failure.
func (ptm *ProductionTeamManager) runGlobalConsistencyCheck(team *ProductionTeam) string {
	if team == nil || team.Cwd == "" {
		return ""
	}
	cs := NewContractStore(team.Cwd, "")
	if err := cs.BuildFromRepo(); err != nil {
		return fmt.Sprintf("contract store build failed: %v", err)
	}
	inconsistencies, err := cs.GlobalConsistencyScan()
	if err != nil {
		return fmt.Sprintf("global consistency scan failed: %v", err)
	}
	if len(inconsistencies) > 0 {
		var msgs []string
		for _, inc := range inconsistencies {
			msgs = append(msgs, fmt.Sprintf("[%s] %s: %s", inc.Severity, inc.Type, inc.Message))
		}
		return "global consistency issues found:\n" + strings.Join(msgs, "\n")
	}
	return ""
}

// tryGateWithRemediation 跑一次 gate, 失败就让 coder 修, 最多 maxAttempts 次.
//
// 行为:
//  1. 先跑 gate(). 通过 → 直接返回 "".
//  2. 不通过 → 用 gate 输出作为 feedback, 触发一个 coder 阶段去修.
//  3. coder 跑完后 (不论 status 如何), 再跑一次 gate. 若仍失败, 进入下一轮.
//  4. 用完 maxAttempts 仍失败, 返回最后一次的错误.
//
// 这个机制让团队具备 "门禁失败 → 自动修复 → 再校验" 的闭环, 能消化掉绝大多数
// 由 AI 生成代码引入的低级错误 (vet 警告 / 重复声明 / 易触发死循环的边界等).
//
// 注意: 这里使用 executor.ExecuteSingleStage 直接调度一个临时 coder stage,
// 不写入 workflow.results, 也不被 reviewer 重新审一遍, 避免无限套娃.
func (ptm *ProductionTeamManager) tryGateWithRemediation(
	ctx context.Context,
	team *ProductionTeam,
	executor *WorkflowExecutor,
	gateName string,
	gateFn func(*ProductionTeam) string,
	maxAttempts int,
) string {
	if gateFn == nil {
		return ""
	}
	gateErr := gateFn(team)
	if gateErr == "" {
		return ""
	}
	if maxAttempts <= 0 || executor == nil || ctx == nil {
		return gateErr
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return gateErr
		}
		ptm.notify(team.ChatID, fmt.Sprintf(
			"🛠 全局 %s 门禁失败, 启动第 %d/%d 次自动修复尝试...",
			gateName, attempt, maxAttempts))

		fixPrompt := fmt.Sprintf(`你处于"全局 %s 门禁失败 → 自动修复"阶段.

刚才执行 `+"`go %s`"+` 失败, 错误输出如下 (来自仓库根目录):

%s

请按下列要求修复:
1. **使用 Bash 工具** 在仓库根 (%s) 下重现错误: `+"`go %s`"+`.
2. **使用 Read 工具** 打开错误信息中提到的源文件.
3. **使用 Edit/Write 工具** 直接修改源码 (不要只输出 patch 文字, 必须真的写入磁盘).
4. **再次运行** `+"`go %s`"+` 验证修复. 仍失败则继续修, 最多再尝试 3 次.
5. 完成后, 列出修改过的文件路径和每处改动的简要原因.

禁止事项:
- 不要 *删除* 失败的测试以伪造 PASS.
- 不要把 main 包的逻辑改回未实现状态来回避错误.
- 不要交付一份 "我建议这样改" 的 markdown - 必须真的修改文件.`,
			gateName,
			gateCommandFor(gateName),
			gateErr,
			team.Cwd,
			gateCommandFor(gateName),
			gateCommandFor(gateName),
		)

		fixStage := StageDef{
			Name:   fmt.Sprintf("remediation-%s-%d", gateName, attempt),
			Role:   "coder",
			Prompt: fixPrompt,
		}
		// 给修复阶段一个相对充裕的超时 (25min), 走 executor 的 retry/timeout 机制.
		fixCtx, fixCancel := context.WithTimeout(ctx, 25*time.Minute)
		_ = executor.ExecuteSingleStage(fixCtx, fixStage, team.Objective, nil, team)
		fixCancel()

		// 重跑 gate. 如果 ctx 已取消 (用户停止), 直接退出.
		if ctx.Err() != nil {
			return gateErr
		}
		newErr := gateFn(team)
		if newErr == "" {
			ptm.notify(team.ChatID, fmt.Sprintf(
				"✅ 第 %d 次修复成功, 全局 %s 门禁通过", attempt, gateName))
			return ""
		}
		gateErr = newErr
	}
	return gateErr
}

// gateCommandFor 把 gateName 映射成等价的 go 命令字符串, 用于喂给修复 prompt.
func gateCommandFor(gateName string) string {
	switch strings.ToLower(gateName) {
	case "compile":
		return "build ./..."
	case "test":
		return "test -race -timeout 120s ./..."
	default:
		return gateName
	}
}

// executeSwarm 蜂群模式执行
func (ptm *ProductionTeamManager) executeSwarm(ctx context.Context, team *ProductionTeam) {
	swarm := NewSwarmOrchestrator(ptm.llm, ptm.pool, ptm.taskTracker, ptm.notify, team.ChatID, 8)
	swarm.evolution = ptm.evolution
	swarm.roles = ptm.roles
	if team.dataDir != "" {
		swarm.stateDir = team.dataDir
	}

	results, err := swarm.Execute(ctx, team, team.Objective)
	if err != nil {
		if ctx.Err() != nil {
			ptm.failTeam(team, "用户停止")
			return
		}
		ptm.failTeam(team, err.Error())
		return
	}

	team.mu.Lock()
	team.Status = TeamStatusCompleted
	team.FinishedAt = time.Now()
	team.Stages = results
	team.mu.Unlock()
	team.persist()
	ptm.runTaskCompletedHooks(results)

	// 蜂群持续观测指标
	if ptm.metrics != nil {
		labels := map[string]string{"workflow": "swarm"}
		dur := team.FinishedAt.Sub(team.StartedAt).Seconds()
		ptm.metrics.RecordRun("team", metrics.MTeamRunCount, 1, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamDurationSec, dur, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamSuccessCount, 1, team.Name, labels)
		total, passed := 0, 0
		for _, s := range results {
			total++
			if s.Status == TaskCompleted {
				passed++
			}
		}
		if total > 0 {
			ptm.metrics.RecordRun("team", metrics.MTeamStagePassRate, float64(passed)/float64(total), team.Name, labels)
		}
	}

	// 触发进化学习 + Dreaming + 进化指标采集
	if ptm.evolution != nil {
		mc := ptm.metrics
		go func() {
			ptm.evolution.LearnFromTeam(context.Background(), team.Name)
			ptm.evolution.Consolidate()
			if mc != nil {
				ptm.evolution.CollectMetrics(mc)
			}
		}()
	}
	if ptm.dreamer != nil {
		for _, r := range results {
			ptm.dreamer.RecordSession(DreamSessionRecord{
				ChatID:  team.ChatID,
				EndTime: time.Now(),
				Summary: fmt.Sprintf("[Swarm:%s] [%s/%s] %s", team.Name, r.Name, r.Role, truncateResult(r.Output, 300)),
			})
		}
		ptm.dreamer.AfterQuery(context.Background())
	}

	// 持久化完整报告文件
	reportPath := ptm.saveTeamReport(team, results)

	// 写入高权重记忆
	if ptm.memWriter != nil {
		var stageSummary string
		for _, r := range results {
			if r.Output != "" {
				stageSummary += fmt.Sprintf("[%s/%s] %s\n", r.Name, r.Role, truncateResult(r.Output, 200))
			}
		}
		ptm.memWriter.AddTeamMemory(team.Name, team.Workflow, team.Objective, stageSummary)
	}

	var summary string
	for _, r := range results {
		if r.Output != "" {
			summary += fmt.Sprintf("\n\n**[%s] %s**\n%s", r.Role, r.Name, truncateResult(r.Output, 500))
		}
	}
	reportNote := ""
	if reportPath != "" {
		reportNote = fmt.Sprintf("\n\n📄 **完整报告**: `%s`", reportPath)
	}
	ptm.notify(team.ChatID, fmt.Sprintf("🐝 蜂群团队 **%s** 执行完成 (耗时 %v)\n\n**成果汇总:**%s%s",
		team.Name, time.Since(team.StartedAt).Round(time.Second), summary, reportNote))
}

// sendMediaAssets 从工作流输出中提取 SVG 并转换为 PNG 发送。
func (ptm *ProductionTeamManager) sendMediaAssets(team *ProductionTeam, results []StageResult) {
	for _, r := range results {
		if r.Output == "" {
			continue
		}
		svgData := extractSVGFromOutput(r.Output)
		if svgData == "" {
			continue
		}
		pngData := svgToPNG(svgData)
		if len(pngData) == 0 {
			// 无法转换时发送原始 SVG 文件
			if err := ptm.mediaNotify(team.ChatID, []byte(svgData), team.Name+".svg", "file"); err != nil {
				log.Printf("[Teams] 发送 SVG 文件失败: %v", err)
			}
			continue
		}
		if err := ptm.mediaNotify(team.ChatID, pngData, team.Name+".png", "image"); err != nil {
			log.Printf("[Teams] 发送图片失败: %v", err)
		}
	}
}

// extractSVGFromOutput 从阶段输出中提取 SVG 代码块。
func extractSVGFromOutput(s string) string {
	lower := strings.ToLower(s)
	start := strings.Index(lower, "<svg")
	if start < 0 {
		return ""
	}
	after := s[start:]
	end := strings.Index(strings.ToLower(after), "</svg>")
	if end < 0 {
		return ""
	}
	return after[:end+len("</svg>")]
}

// svgToPNG 将 SVG 转换为 PNG（使用系统工具 rsvg-convert 或 inkscape）。
// 返回空切片表示无可用转换工具。
func svgToPNG(svg string) []byte {
	// 尝试 rsvg-convert
	converters := []struct {
		cmd  string
		args []string
	}{
		{"rsvg-convert", []string{"-f", "png", "-w", "800"}},
		{"inkscape", []string{"--export-type=png", "--export-width=800", "--pipe"}},
	}

	for _, c := range converters {
		path, err := exec.LookPath(c.cmd)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, path, c.args...)
		cmd.Stdin = strings.NewReader(svg)
		out, err := cmd.Output()
		cancel()
		if err == nil && len(out) > 0 {
			return out
		}
	}
	return nil
}

func (ptm *ProductionTeamManager) failTeam(team *ProductionTeam, reason string) {
	team.mu.Lock()
	team.Status = TeamStatusFailed
	team.FinishedAt = time.Now()
	team.Error = reason
	startedAt := team.StartedAt
	team.mu.Unlock()
	team.persist()

	// 失败路径也要补齐整团队级指标, 否则 dashboard 的成败对比/故障分析会缺数据。
	if ptm.metrics != nil {
		labels := map[string]string{"workflow": team.Workflow, "status": "failed"}
		ptm.metrics.RecordRun("team", metrics.MTeamRunCount, 1, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamFailCount, 1, team.Name, labels)
		if !startedAt.IsZero() {
			ptm.metrics.RecordRun("team", metrics.MTeamDurationSec, time.Since(startedAt).Seconds(), team.Name, labels)
		}
	}

	// 判断是否 LLM 限流/熔断导致, 给出恢复提示
	lower := strings.ToLower(reason)
	isLLMIssue := strings.Contains(reason, "429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "限流") ||
		strings.Contains(lower, "熔断") ||
		strings.Contains(lower, "circuit breaker") ||
		strings.Contains(lower, "overload") ||
		strings.Contains(lower, "全部失败")

	msg := fmt.Sprintf("❌ 团队 **%s** 执行失败: %s", team.Name, reason)
	if isLLMIssue {
		msg += fmt.Sprintf("\n\n💡 **恢复方式**: 等待限流解除后, 使用相同目标重新启动团队即可从检查点恢复:\n`/team go %s %s`\n已完成的阶段会自动跳过。", team.Name, team.Objective)
	}
	ptm.notify(team.ChatID, msg)
}

// runTaskCompletedHooks 对已完成阶段触发 TaskCompleted Hook。
func (ptm *ProductionTeamManager) runTaskCompletedHooks(results []StageResult) {
	if ptm.hookRunner == nil {
		return
	}
	for _, r := range results {
		if r.Status == TaskCompleted {
			ptm.hookRunner.ExecuteTaskCompletedHooks(r.Name, true)
		} else {
			ptm.hookRunner.ExecuteTaskCompletedHooks(r.Name, false)
		}
	}
}

// StopTeam 停止团队
func (ptm *ProductionTeamManager) StopTeam(name string) error {
	ptm.mu.RLock()
	team, ok := ptm.teams[name]
	ptm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}

	team.mu.Lock()
	defer team.mu.Unlock()
	if team.cancel != nil {
		team.cancel()
	}
	team.Status = TeamStatusStopped
	team.FinishedAt = time.Now()
	team.persist()
	return nil
}

// ResumeTeam 恢复已停止/失败的团队，从检查点继续执行。
// 与 RunTeam 不同，ResumeTeam 不需要新目标，直接使用团队现有目标。
// 修复: 使用 starting 标志防止并发 resume 导致的双重工作流执行。
func (ptm *ProductionTeamManager) ResumeTeam(name string) error {
	ptm.mu.RLock()
	team, ok := ptm.teams[name]
	ptm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}

	team.mu.Lock()
	if team.Status == TeamStatusRunning {
		team.mu.Unlock()
		// 幂等: 团队已在运行，视为恢复成功
		return nil
	}

	if isSuccessfulTeamStatus(team.Status) {
		team.mu.Unlock()
		return fmt.Errorf("团队 %q 已完成，无需恢复", name)
	}

	// 先释放 team.mu, 再获取 starting 锁 (避免锁序问题导致死锁)
	team.mu.Unlock()

	// 原子性检查: 是否已有其他请求在启动同一个团队
	if !ptm.tryStartTeam(name) {
		// 另一个并发请求已经在启动中, 视为幂等成功
		return nil
	}

	// 重新锁定 team.mu 修改状态
	team.mu.Lock()
	// 双重检查: 在获得 starting 锁后再次确认状态
	if team.Status == TeamStatusRunning {
		team.mu.Unlock()
		ptm.clearStarting(name)
		return nil
	}
	if isSuccessfulTeamStatus(team.Status) {
		team.mu.Unlock()
		ptm.clearStarting(name)
		return fmt.Errorf("团队 %q 已完成，无需恢复", name)
	}

	objective := team.Objective
	team.Status = TeamStatusRunning
	// 保留原始 StartedAt，避免恢复后看起来像重新开始
	if team.StartedAt.IsZero() {
		team.StartedAt = time.Now()
	}
	// 清除 stop 设置的 FinishedAt，避免产生负 duration
	team.FinishedAt = time.Time{}
	team.Error = ""
	ctx, cancel := context.WithCancel(context.Background())
	team.cancel = cancel
	team.doneCh = make(chan struct{})
	team.mu.Unlock()

	// 更新黑板上的目标
	team.Blackboard.Write("objective", objective, "system", "context")
	team.persist()

	ptm.notify(team.ChatID, fmt.Sprintf("♻️ 团队 **%s** 从检查点恢复执行\n目标: %s\n工作流: %s", name, objective, team.Workflow))

	go func() {
		defer ptm.clearStarting(name)
		defer func() { close(team.doneCh) }()
		ptm.executeWorkflow(ctx, team, true)
	}()
	return nil
}

// StopFirstRunning 停止第一个正在运行的团队 (意图识别用)。
func (ptm *ProductionTeamManager) StopFirstRunning() (string, error) {
	ptm.mu.RLock()
	var targetName string
	for _, team := range ptm.teams {
		if team.Status == TeamStatusRunning {
			targetName = team.Name
			break
		}
	}
	ptm.mu.RUnlock()

	if targetName == "" {
		return "", fmt.Errorf("无正在运行的团队")
	}
	err := ptm.StopTeam(targetName)
	return targetName, err
}

// DeleteTeam 删除团队
func (ptm *ProductionTeamManager) DeleteTeam(name string) error {
	ptm.mu.Lock()
	defer ptm.mu.Unlock()

	team, ok := ptm.teams[name]
	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}
	if team.Status == TeamStatusRunning && team.cancel != nil {
		team.cancel()
	}
	delete(ptm.teams, name)
	os.RemoveAll(filepath.Join(ptm.baseDir, name))
	return nil
}

// SendMailMessage 向团队 agent 发送消息
func (ptm *ProductionTeamManager) SendMailMessage(teamName, from, to, content string) error {
	ptm.mu.RLock()
	team, ok := ptm.teams[teamName]
	ptm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("团队 %q 不存在", teamName)
	}

	team.mu.Lock()
	team.Mailbox = append(team.Mailbox, MailMessage{
		From: from, To: to, Content: content,
		Type: "message", Timestamp: time.Now(),
	})
	team.mu.Unlock()

	// 同时写入黑板, 使消息对所有 Agent 可见
	team.Blackboard.Write(
		fmt.Sprintf("msg-%s-%d", to, time.Now().UnixMilli()),
		fmt.Sprintf("From %s: %s", from, content),
		from, "context",
	)
	team.persist()
	return nil
}

// GetTeam 获取团队信息
func (ptm *ProductionTeamManager) GetTeam(name string) *ProductionTeam {
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()
	return ptm.teams[name]
}

// ListAllTeams 列出所有团队
func (ptm *ProductionTeamManager) ListAllTeams() []*ProductionTeam {
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()
	result := make([]*ProductionTeam, 0, len(ptm.teams))
	for _, t := range ptm.teams {
		result = append(result, t)
	}
	return result
}

// saveTeamReport 将团队完整执行结果保存为 Markdown 报告文件。
// 解决"产出散落在 blackboard/team.json 中无法检索"的问题。
func (ptm *ProductionTeamManager) saveTeamReport(team *ProductionTeam, results []StageResult) string {
	if team.dataDir == "" {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 团队报告: %s\n\n", team.Name))
	sb.WriteString(fmt.Sprintf("- **工作流**: %s\n", team.Workflow))
	sb.WriteString(fmt.Sprintf("- **目标**: %s\n", team.Objective))
	sb.WriteString(fmt.Sprintf("- **状态**: %s\n", team.Status))
	sb.WriteString(fmt.Sprintf("- **开始时间**: %s\n", team.StartedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("- **完成时间**: %s\n", team.FinishedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("- **耗时**: %v\n", team.FinishedAt.Sub(team.StartedAt).Round(time.Second)))
	sb.WriteString(fmt.Sprintf("- **Agent数**: %d\n\n", len(team.Agents)))

	// 各阶段产出（完整版，不截断）
	sb.WriteString("---\n\n## 各阶段产出\n\n")
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("### %d. %s (%s) — %s\n\n", i+1, r.Name, r.Role, r.Status))
		if r.Duration != "" {
			sb.WriteString(fmt.Sprintf("**耗时**: %s\n\n", r.Duration))
		}
		if r.Error != "" {
			sb.WriteString(fmt.Sprintf("**错误**: %s\n\n", r.Error))
		}
		if r.Output != "" {
			sb.WriteString(r.Output)
			sb.WriteString("\n\n")
		}
		sb.WriteString("---\n\n")
	}

	reportPath := filepath.Join(team.dataDir, "REPORT.md")
	if err := os.WriteFile(reportPath, []byte(sb.String()), 0644); err != nil {
		log.Printf("[Teams] 保存报告失败: %v", err)
		return ""
	}
	log.Printf("[Teams] 报告已保存: %s", reportPath)
	return reportPath
}

// GetTeamReport 读取团队的完整报告文件。
// 返回报告内容和路径，找不到时返回空字符串。
func (ptm *ProductionTeamManager) GetTeamReport(name string) (content string, path string) {
	team := ptm.GetTeam(name)
	if team == nil || team.dataDir == "" {
		return "", ""
	}
	reportPath := filepath.Join(team.dataDir, "REPORT.md")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return "", ""
	}
	return string(data), reportPath
}

// persist 持久化团队状态到文件
func (t *ProductionTeam) persist() {
	if t.dataDir == "" {
		return
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		logging.For("teams").Warn("持久化失败", "team", t.Name, "err", err)
		return
	}
	os.MkdirAll(t.dataDir, 0755)
	_ = os.WriteFile(filepath.Join(t.dataDir, "team.json"), data, 0644)
}

// loadPersistedTeams 从磁盘恢复团队状态
func (ptm *ProductionTeamManager) loadPersistedTeams() {
	entries, err := os.ReadDir(ptm.baseDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(ptm.baseDir, entry.Name(), "team.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var team ProductionTeam
		if json.Unmarshal(data, &team) == nil {
			team.mgr = ptm
			team.dataDir = filepath.Join(ptm.baseDir, entry.Name())
			team.Blackboard = NewBlackboard(team.Name, team.dataDir)
			if team.Status == TeamStatusRunning {
				team.Status = TeamStatusFailed
				team.Error = "进程重启"
				team.FinishedAt = time.Now()
				team.persist()
			}
			ptm.teams[team.Name] = &team
		}
	}
}

// FormatStatus 格式化团队状态为可读文本
func (t *ProductionTeam) FormatStatus() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	status := fmt.Sprintf("**团队: %s** [%s]\n", t.Name, t.Status)
	status += fmt.Sprintf("工作流: %s\n", t.Workflow)
	if t.Objective != "" {
		status += fmt.Sprintf("目标: %s\n", t.Objective)
	}
	status += fmt.Sprintf("创建: %s\n", t.CreatedAt.Format("01-02 15:04"))

	if !t.StartedAt.IsZero() {
		elapsed := time.Since(t.StartedAt)
		if !t.FinishedAt.IsZero() {
			elapsed = t.FinishedAt.Sub(t.StartedAt)
		}
		status += fmt.Sprintf("耗时: %v\n", elapsed.Round(time.Second))
	}

	if len(t.Agents) > 0 {
		status += "\n**Agent:**\n"
		for _, a := range t.Agents {
			status += fmt.Sprintf("- %s [%s]\n", a.Name, a.Status)
		}
	}

	if len(t.Stages) > 0 {
		status += "\n**阶段:**\n"
		for _, s := range t.Stages {
			icon := "⏳"
			switch s.Status {
			case TaskCompleted:
				icon = "✅"
			case TaskFailed:
				icon = "❌"
			case TaskRunning:
				icon = "🔄"
			}
			status += fmt.Sprintf("- %s %s [%s] %s\n", icon, s.Name, s.Role, s.Duration)
		}
	}

	if t.Error != "" {
		status += fmt.Sprintf("\n**错误:** %s\n", t.Error)
	}
	return status
}

func truncateResult(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 使用 rune 截断, 避免在多字节 UTF-8 字符中间切断
	runes := []rune(s)
	if len(runes) <= max {
		return s // rune 数未超限, 返回原文 (len(s) > max 但 rune 数 <= max)
	}
	return string(runes[:max]) + "..."
}

// executePrediction 群体智能预测执行。
// 使用 swarm_intel.Engine 的 5 阶段流水线: Decompose → Scout → Predict → Debate → Fuse
func (ptm *ProductionTeamManager) executePrediction(ctx context.Context, team *ProductionTeam) {
	ctx = logging.WithTrace(ctx)
	ctx, endSpan := logging.WithSpan(ctx, "team."+team.Name+".predict")
	defer endSpan()
	logging.Event(ctx, "predict.start", "team", team.Name, "objective", team.Objective)

	cfg := swarm_intel.DefaultConfig()
	cfg.Notify = func(_, msg string) {
		ptm.notify(team.ChatID, msg)
		if team.Blackboard != nil {
			team.Blackboard.Write("predict-progress", msg, "engine", "progress")
		}
	}

	engine := swarm_intel.NewEngine(ptm.llm, cfg)

	result, err := engine.Predict(ctx, team.ChatID, team.Objective)
	if err != nil {
		ptm.failTeam(team, fmt.Sprintf("预测失败: %v", err))
		return
	}

	team.mu.Lock()
	team.Status = TeamStatusCompleted
	team.FinishedAt = time.Now()
	team.Stages = []StageResult{{
		Name:   "predict",
		Role:   "swarm-intelligence",
		Status: TaskCompleted,
		Output: result.Summary,
	}}
	team.mu.Unlock()
	team.persist() // 补齐 predict 工作流的状态落盘

	// 补齐 predict 工作流的团队级指标 (之前遗漏)
	if ptm.metrics != nil {
		labels := map[string]string{"workflow": team.Workflow, "status": "completed"}
		ptm.metrics.RecordRun("team", metrics.MTeamRunCount, 1, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamSuccessCount, 1, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamDurationSec,
			team.FinishedAt.Sub(team.StartedAt).Seconds(), team.Name, labels)
	}

	if team.Blackboard != nil {
		resultJSON, _ := json.Marshal(result)
		team.Blackboard.Write("predict-result", string(resultJSON), "engine", "result")
		team.Blackboard.Write("predict-summary", result.Summary, "engine", "result")
	}

	var report strings.Builder
	report.WriteString(fmt.Sprintf("# 群体智能预测: %s\n\n", result.Question))
	report.WriteString(fmt.Sprintf("**共识度:** %.0f%%  |  **辩论轮数:** %d  |  **融合方法:** %s\n\n",
		result.Consensus*100, result.Rounds, result.Method))
	report.WriteString("## 预测结果\n\n")
	for _, o := range result.Outcomes {
		report.WriteString(fmt.Sprintf("| %s | **%.1f%%** | [%.1f%% ~ %.1f%%] |\n",
			o.Outcome, o.Probability*100, o.Lower95*100, o.Upper95*100))
	}
	if result.Summary != "" {
		report.WriteString(fmt.Sprintf("\n## 分析总结\n\n%s\n", result.Summary))
	}
	ptm.saveTeamReport(team, []StageResult{{
		Name: "predict", Role: "swarm-intelligence", Output: report.String(),
	}})

	ptm.notify(team.ChatID, fmt.Sprintf("✅ 群体智能预测完成 (%s)", team.Name))
	logging.Event(ctx, "predict.done", "team", team.Name, "consensus", fmt.Sprintf("%.2f", result.Consensus))
}
