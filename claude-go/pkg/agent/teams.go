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
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/trace"
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
	// TeamStatusRefining 标记团队正带着运行后反馈进行精修迭代 (completed → refining → running)。
	TeamStatusRefining TeamStatus = "refining"
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
	teams        map[string]*ProductionTeam
	mu           sync.RWMutex
	baseDir      string
	cwd          string // 项目工作目录 (传递给团队, 用于编译验证)
	factory      CreateAgentFunc
	notify       NotifyFunc
	mediaNotify  MediaNotifyFunc
	taskTracker  TaskTracker          // 复用 V2 Task 系统
	pool         *AgentPool           // Agent 池 (动态扩缩)
	llm          LLMClient            // LLM 客户端 (蜂群分解)
	evolution    *EvolutionEngine     // 自动进化引擎
	evoLoop      *EvolutionLoop       // 统一学习调度循环 (design/03 §4.3); nil = 回落到直调路径
	traceStore   *tracestore.Store    // 轨迹底座 (design/03 §4.1); nil = 不采集 gate Span
	dreamer      DreamRecorder        // Dreaming 接口 (覆盖 team agent 会话)
	skillCreator SkillAutoCreator     // 技能自创建器 (团队干净成功后提炼 shadow 技能)
	roles        *RoleRegistry        // 角色注册表
	memWriter    MemoryWriter         // 记忆写入 (团队完成后写入高权重记忆)
	metrics      *metrics.Collector   // 持续观测指标采集器
	concurrency  ConcurrencySuggestor // 动态并发建议 (基于 API 流控状态)

	planCfgResolver *PlanConfigResolver // 模型/连接参数解析器 (可选)
	hookRunner      *hooks.Runner       // Hook 执行器 (TeammateIdle / TaskCompleted)

	// starting 防止并发 resume/run 同一个团队 (race condition 保护)
	startingMu sync.Mutex
	starting   map[string]bool // key=team name, value=是否正在启动中
}

// TeamManagerConfig 团队管理器配置。
type TeamManagerConfig struct {
	BaseDir     string
	Cwd         string // 项目工作目录 (用于编译验证)
	Factory     CreateAgentFunc
	Notify      NotifyFunc
	MediaNotify MediaNotifyFunc
	TaskTracker TaskTracker
	Pool        *AgentPool
	LLM         LLMClient
	Evolution   *EvolutionEngine
	// EvolutionLoop 统一学习循环 (design/03 §4.3)。非 nil 时团队完成的学习经它调度
	// (去重/预算/串行/空闲期整理); nil 则回落到直调, 见 submitLearn。
	EvolutionLoop *EvolutionLoop
	// TraceStore 轨迹底座 (design/03 §4.1)。非 nil 时门禁判定写 gate Span ——
	// 这是 §4.1 五种 Kind 里唯一必须由 pkg/agent 写的一种 (门禁在团队层, 引擎看不到)。
	TraceStore         *tracestore.Store
	Dreamer            DreamRecorder
	Roles              *RoleRegistry
	MemWriter          MemoryWriter
	Concurrency        ConcurrencySuggestor
	PlanConfigResolver *PlanConfigResolver // 模型/连接参数解析器 (可选)
	HookConfigs        []types.HookConfig  // Hook 配置 (用于 TeammateIdle / TaskCompleted 等)
	SkillCreator       SkillAutoCreator    // 技能自创建器 (可选; design/03 §1.2 开环2 接线)
}

// SkillAutoCreator 技能自创建接口 (实现者: skills.AutoCreator, duck typing 解耦包依赖)。
// 历史缺陷: AutoCreator 仅在 feishu bot 被构造、全仓库零调用点 (design/03 §1.2 开环2);
// 现由团队干净成功路径触发, 产物 frontmatter 带 status: shadow 供进化门禁裁决晋升。
type SkillAutoCreator interface {
	MaybeCreate(ctx context.Context, objective, approach, outcome string) (string, error)
}

// allStagesCompleted 判断全部阶段是否干净通过 (技能提炼触发条件)。
func allStagesCompleted(results []StageResult) bool {
	for _, r := range results {
		if r.Status != TaskCompleted {
			return false
		}
	}
	return true
}

// skillDistillAllowed 判定是否允许把本次运行提炼成 shadow 技能 (design/03 §4.6 必过闸)。
// hasEvidence=false (本 run 无门禁类奖励) 时一律放行, 保持未接奖励源工作流的原行为;
// 有证据则要求加权门禁分不为负。
func skillDistillAllowed(gateScore float64, hasEvidence bool) bool {
	if !hasEvidence {
		return true
	}
	return gateScore >= 0
}

// summarizeStageApproach 把阶段序列压成"方法"描述, 供技能提炼 prompt 使用。
func summarizeStageApproach(results []StageResult) string {
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. [%s/%s] 产出 %d 字\n", i+1, r.Name, r.Role, len(r.Output))
	}
	return b.String()
}

// lastNonEmptyOutput 取最后一个非空阶段产出 (截断), 作为技能提炼的"结果"示例。
func lastNonEmptyOutput(results []StageResult, maxLen int) string {
	for i := len(results) - 1; i >= 0; i-- {
		if out := strings.TrimSpace(results[i].Output); out != "" {
			return truncateResult(out, maxLen)
		}
	}
	return ""
}

// SetMemoryWriter 注入记忆写入器 (在 Bot 初始化后调用)。
func (ptm *ProductionTeamManager) SetMemoryWriter(mw MemoryWriter) {
	ptm.memWriter = mw
}

// Metrics 返回内部指标采集器 (供外部模块注入使用)。
func (ptm *ProductionTeamManager) Metrics() *metrics.Collector {
	return ptm.metrics
}

// WrapAgentFactory 用 wrap 包装 agent 执行工厂 (design/02 §3.3 远程 runtime 接线点)。
//
// 为什么需要它: 阶段执行的唯一出口是 `we.factory(stageCtx, role, "")`
// (workflow.go:1580), 而 `we.factory` 来自 `ptm.factory` (本文件 executeWorkflow
// 里构造 WorkflowExecutor 时传入)。把远程放置能力接进来最小的改动就是在这里换掉
// 工厂 —— 于是门禁/journal/重试/黑板全部不动, 只有"这次 agent 由谁执行"变了。
// wrap 收到当前工厂 (通常是本地执行体), 应当把它作为回退包在新工厂里。
//
// # 它同时接管 swarm 路径 (design/02 §3.3 剩余项)
//
// 阶段执行不是唯一的 agent 创建出口: swarm 模式的子任务走 `AgentPool.factory`
// (`swarm.go:702` 的 `s.pool.Acquire` → `pool.go` 的 `currentFactory()`), 那是一个
// **完全独立的字段**。上一轮只换了 ptm.factory, 于是 swarm 无论怎么配
// `--placement-prefer` 都只在本机跑。这里一并换掉, 装配处不需要多写一行 ——
// 多一个接线点就多一个"某个部署忘了调"的静默降级面, 而症状 (swarm 子任务不去
// 远程) 与"远程 worker 没上线"长得一模一样, 极难归因。
//
// **wrap 只会被调用一次**(两个字段同源时), 这是刻意的: wrap 通常带副作用 ——
// 生产装配处 (`cmd/claude-go/main.go:1346`) 在里面用收到的本地工厂注册
// `local-session` runtime, 调两次就会用**另一个**本地工厂按同名覆盖前一次注册。
// 两个字段不同源时才分别 wrap (那时两次注册各自对应真正不同的本地执行体, 是正确的)。
// 同源判定用 reflect 取函数码指针: Go 不允许直接比较函数值。它对方法值只比较
// 方法本身而不比较接收者, 这里可接受 —— 唯一的后果是"local runtime 背后是哪个
// 本地执行体", 而生产里两处传的就是同一个 `bot.sessions.CreateAgentRunner`
// (`bot.go:447` 与 `bot.go:559`)。
//
// ⚠️ 只能在**任何团队启动之前**调用 (进程装配阶段): ptm.factory 在运行期被
// 读取且不持锁, 运行中替换是数据竞争。
func (ptm *ProductionTeamManager) WrapAgentFactory(wrap func(CreateAgentFunc) CreateAgentFunc) {
	if ptm == nil || wrap == nil {
		return
	}
	ptm.mu.Lock()
	prev := ptm.factory
	next := wrap(prev)
	if next != nil {
		ptm.factory = next
	}
	pool := ptm.pool
	ptm.mu.Unlock()

	if pool == nil {
		return
	}
	if next != nil && sameFactory(pool.Factory(), prev) {
		pool.setFactory(next) // 同源: 复用已包好的那个, 不重复触发 wrap 的副作用
		return
	}
	pool.WrapFactory(wrap)
}

// sameFactory 判两个工厂是否同源 (见 WrapAgentFactory 的说明)。
// 双 nil 视为同源 —— 那时 wrap 收到的 prev 与 pool.factory 都是 nil, 结果等价。
func sameFactory(a, b CreateAgentFunc) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// AgentFactory 返回当前 agent 执行工厂 (装配期只读用途)。
func (ptm *ProductionTeamManager) AgentFactory() CreateAgentFunc {
	if ptm == nil {
		return nil
	}
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()
	return ptm.factory
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
		evoLoop:         cfg.EvolutionLoop,
		traceStore:      cfg.TraceStore,
		dreamer:         cfg.Dreamer,
		skillCreator:    cfg.SkillCreator,
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
	Progress   *ProgressState      `json:"progress,omitempty"` // 实时进展快照 (运行中由 Coordinator 心跳回填)
	TaskIDs    map[string]string   `json:"taskIds,omitempty"`  // stageName → V2 taskID
	CreatedAt  time.Time           `json:"createdAt"`
	StartedAt  time.Time           `json:"startedAt,omitempty"`
	FinishedAt time.Time           `json:"finishedAt,omitempty"`
	Mailbox    []MailMessage       `json:"mailbox,omitempty"`
	Error      string              `json:"error,omitempty"`
	Cwd        string              `json:"cwd,omitempty"`      // 工作目录 (用于编译验证和文件清单)
	Language   string              `json:"language,omitempty"` // 编程语言 ("go","cpp","rust","python"), 空=""go"

	// LastRunID 最近一次执行的 trace RunID (design/03 §4.1 四元组的 episode 键)。
	//
	// 必须持久化: 用户显式评分 (/team rate) 与精修负信号都发生在 run **结束之后**,
	// 那时 ctx 里已经没有 trace 了。没有这个字段, 这两类奖励只能写出 RunID 为空的
	// 事件, 而 AggregateRewards 强制要求 RunID —— 落盘即死数据。跨重启同理。
	LastRunID string `json:"lastRunId,omitempty"`

	// 持续优化 (RefineTeam): 运行后用户反馈驱动的精修迭代
	RefineHistory   []RefineEntry `json:"refineHistory,omitempty"`   // 历次精修留痕 (可追溯)
	PendingFeedback string        `json:"pendingFeedback,omitempty"` // 本轮待处理的用户反馈 (注入重跑阶段, 完成后清空)
	FeedbackTarget  string        `json:"feedbackTarget,omitempty"`  // 本轮精修的起始阶段 (空=整体重跑)

	Blackboard *Blackboard `json:"-"` // 共享黑板 (不序列化, 独立持久化)
	mu         sync.Mutex
	cancel     context.CancelFunc
	mgr        *ProductionTeamManager
	dataDir    string
	doneCh     chan struct{} // 关闭信号: 工作流执行完毕时 close

	// materializedFiles MaterializeCode 写到 team.Cwd 下的文件 (相对路径, 已去重)。
	// cwd 是进程级共享目录, 靠 mtime 猜归属会混进用户自己和别的团队的文件; 这份
	// 由平台亲手记账的清单是唯一的硬归属证据, 供 artifacts.go 生成产物清单时使用。
	// 不序列化: 只在本进程本次运行内有意义, 跨重启由 ARTIFACTS.json 承载。
	materializedFiles []string
	materializedSeen  map[string]bool

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
	// 实时心跳 (运行中由 Coordinator 心跳回填, 供 team status / dashboard 观测团队内部)。
	Phase    string    `json:"phase,omitempty"`    // 当前阶段: LLM生成/编译/测试/等待
	LastBeat time.Time `json:"lastBeat,omitempty"` // 上次心跳时间
}

// submitLearn 提交一次团队完成的学习请求。
//
// 这里刻意保留**两条路径**: 装配了 EvolutionLoop 就走循环 (受去重/预算/串行约束,
// design/03 §4.3), 否则回落到原来的 `go func(){LearnFromTeam;Consolidate}` 直调。
// 回落不是临时兼容而是长期契约 —— CLI/headless 与各下游平台的装配各不相同, 一刀切
// 要求必须先建循环会让任何漏装配的调用方**静默丢失全部学习**, 那正是 design/03
// §1.2 开环 1 的原始形态。
func (ptm *ProductionTeamManager) submitLearn(teamName string) {
	if ptm.evolution == nil {
		return
	}
	if ptm.evoLoop != nil {
		ptm.evoLoop.Submit(LearnRequest{Kind: LearnTeamDone, Team: teamName})
		return
	}
	mc := ptm.metrics
	go func() {
		ptm.evolution.LearnFromTeam(context.Background(), teamName)
		ptm.evolution.Consolidate()
		if mc != nil {
			ptm.evolution.CollectMetrics(mc)
		}
	}()
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

// RefineEntry 一次精修迭代的记录 (供审计与 Evolution 经验闭环)。
type RefineEntry struct {
	At          time.Time `json:"at"`
	Feedback    string    `json:"feedback"`
	TargetStage string    `json:"targetStage,omitempty"`
	FromStatus  string    `json:"fromStatus,omitempty"`
	Accepted    *bool     `json:"accepted,omitempty"` // 精修运行是否成功 (供进化学习区分有效/无效反馈)
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
			return nil, fmt.Errorf("未知工作流 %q, 可选: %s", workflow, strings.Join(AvailableWorkflowNames(), ", "))
		}
		_ = wf
	}

	dataDir := filepath.Join(ptm.baseDir, name)
	os.MkdirAll(dataDir, 0755)

	team := &ProductionTeam{
		Name:      name,
		Workflow:  workflow,
		Objective: objective,
		ChatID:    chatID,
		Status:    TeamStatusCreated,
		Agents:    make(map[string]*BGAgent),
		TaskIDs:   make(map[string]string),
		CreatedAt: time.Now(),
		// 工作区: 默认仍是进程 cwd; 开了 CLAUDE_GO_TEAM_WORKSPACE 才每团队独占一份
		// (并发产码团队互相覆盖, design/02 §1.2)。见 team_workspace.go。
		Cwd:        teamWorkspaceDir(ptm.cwd, name),
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
//
// ---------------------------------------------------------------------------
// 这个函数曾是 282 行的巨函数 (design/01 §4.10 点名的那一处)
// ---------------------------------------------------------------------------
//
// 现在它只做四件事: 起 trace → 分发专用编排器 → 装配 (Coordinator + Executor) 并跑
// 主体 → 把收尾交给**运行级拦截器链** (run_interceptors.go)。
//
// 门禁 / 指标 / 进化 / 记忆 / 通知这五件横切事从这里搬到了那条链上。收益不是行数,
// 是那五件事的**次序与开关变成一张能读的表** (runBuiltinPhases), 而且第三方
// (design/01 点名的 aiops 权限桥) 可以追加一环而不必改这个 8+ 下游平台共用的主路径。
//
// 拆分口径: 一次写入都没有换位置, 一条文案都没有改。等价性由
// run_interceptors_equiv_test.go 的九维金标准钉住 (含变异反证)。
func (ptm *ProductionTeamManager) executeWorkflow(ctx context.Context, team *ProductionTeam, isResume ...bool) {
	ctx, endSpan := ptm.beginRun(ctx, team)
	defer endSpan()

	// 专用编排器 (蜂群 / 群体智能预测) 自带完整收尾, 不走下面这条主路径。
	if ptm.dispatchDedicatedWorkflow(ctx, team) {
		return
	}

	wf := GetWorkflow(team.Workflow)
	if wf == nil {
		ptm.failTeam(team, "未知工作流: "+team.Workflow)
		return
	}

	coord := ptm.newRunCoordinator(team, len(isResume) > 0 && isResume[0])
	executor := ptm.newRunExecutor(team, coord)

	// 黑板 Watch 的生产订阅方 (design/01 §4.11; 默认关, 见 blackboard_watch.go)。
	// 起在这里而不是 RunWithRecovery 里: 这是**唯一**一处"团队 + Coordinator 都已
	// 装配好、且无论走哪条执行路径 (pipeline/图/专用模式) 都会经过"的位置。
	boardWatch := startTeamBoardWatcher(team, coord)
	defer boardWatch.stop() // nil 安全 (未开启时返回 nil)

	results, err := coord.RunWithRecovery(ctx, wf, team.Objective, team, executor)
	if err != nil {
		if ctx.Err() != nil {
			ptm.failTeam(team, "用户停止")
			return
		}
		ptm.failTeam(team, err.Error())
		return
	}

	rc, ok := ptm.computeRunDelivery(team, wf, executor, results)
	if !ok {
		return // 存在阻塞性失败阶段, computeRunDelivery 已判团队失败
	}
	ptm.runAfterChain(ctx, rc)
}

// beginRun 起 trace 四元组与团队级 span, 并记开跑事件。返回收尾函数 (调用方 defer)。
func (ptm *ProductionTeamManager) beginRun(ctx context.Context, team *ProductionTeam) (context.Context, func()) {
	// 注入 trace, 确保团队全生命周期有唯一 traceID
	ctx = logging.WithTrace(ctx)
	ctx, endSpan := logging.WithSpan(ctx, "team."+team.Name+".execute")
	// trace 四元组 RunID (design/03 §4.1 E0): 复用 logging traceID 作后缀,
	// llm.jsonl 的 RunID 可直接 join 结构化日志; 下游 stage/engine 逐层补 NodeID/TurnID。
	ctx = trace.With(ctx, trace.IDs{RunID: fmt.Sprintf("run-%s-%s", team.Name, logging.TraceID(ctx))})
	// 记住本轮 RunID: 运行后才发生的奖励 (用户评分 / 精修负信号) 只能靠它归因。
	team.mu.Lock()
	team.LastRunID = trace.From(ctx).RunID
	team.mu.Unlock()
	logging.Event(ctx, "team.start", "team", team.Name, "workflow", team.Workflow, "objective", team.Objective)
	logging.IncrCounter("team.start." + team.Workflow)
	// run Span (design/03 §4.1 第 6 种 Kind) 写在收尾闭包里而不是这里: 它记的是这次
	// episode 的**自述** (objective / 终态 / 耗时 / 阶段数), 只有跑完才知道。挂在这个
	// defer 上是因为它是唯一"无论从哪条路径退出都会走一遍"的位置 —— 门禁判失败、
	// 未知工作流、专用编排器全都覆盖得到。见 run_span.go。
	runStart := time.Now()
	return ctx, func() {
		ptm.writeRunSpan(ctx, team, runStart)
		endSpan()
	}
}

// dispatchDedicatedWorkflow 分发自带完整收尾的专用编排器。返回 true 表示已处理完毕。
//
// 这两个 mode **不进运行级拦截器链**, 与拆分前一致: 它们各自在 executeSwarm /
// executePrediction 里写终态、发通知。把它们并进链是另一件事 (要先让它们产出
// []StageResult 形态的结果), 不在本轮"行为一字不变"的范围内。
func (ptm *ProductionTeamManager) dispatchDedicatedWorkflow(ctx context.Context, team *ProductionTeam) bool {
	switch team.Workflow {
	case "swarm": // 蜂群模式: 使用 SwarmOrchestrator
		ptm.executeSwarm(ctx, team)
		return true
	case "predict": // 群体智能预测模式: 使用 SwarmIntelligenceEngine
		ptm.executePrediction(ctx, team)
		return true
	}
	return false
}

// newRunCoordinator 造本次运行的 Coordinator (带重试/检查点/自动恢复)。
func (ptm *ProductionTeamManager) newRunCoordinator(team *ProductionTeam, resuming bool) *Coordinator {
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
	if !resuming {
		coord.ClearCheckpoints() // 内存检查点表 + checkpoints.json
		// 磁盘上的**其余**进度真源也必须一起清 —— 尤其 graph-journal。
		// 只清 checkpoints.json 会让"新的一轮"在图路径 (orchestrated 无条件命中)
		// 变成"重放上一轮": 零节点执行 + 直接返回旧目标的产出。见 clearRunProgress。
		clearRunProgress(team.dataDir)
	} else {
		// 恢复模式: 保留已完成阶段的检查点, 从失败处继续
		restored := coord.CompletedCount()
		if restored > 0 {
			ptm.notify(team.ChatID, fmt.Sprintf("♻️ 检查点恢复: %d 个已完成阶段将跳过", restored))
		}
	}
	return coord
}

// newRunExecutor 造本次运行的 WorkflowExecutor。
//
// 这里每一个字段都会影响阶段提示词或执行形态 (roles 决定角色模板、evolution 决定经验
// 注入、planCfgResolver 决定模型/并发), 所以等价性测试的第 ④ 维 (提示词逐字) 主要就是
// 在钉这个构造 —— 拆分时漏一个字段, 提示词立刻变形。
func (ptm *ProductionTeamManager) newRunExecutor(team *ProductionTeam, coord *Coordinator) *WorkflowExecutor {
	return &WorkflowExecutor{
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
		traceStore:      ptm.traceStore, // design/03 §4.1: 让图节点也产 node Span
	}
}

// computeRunDelivery 由阶段结果定交付终态, 并造运行级拦截器链的上下文。
// 返回 false 表示已判团队失败 (调用方直接返回)。
//
// 为什么这一步**不在** GateEnforcer 环里, 尽管 §4.10 表把 delivered_with_remediation
// 归给它: 这是"有阻塞性失败阶段就判失败"的终局判定, 而门禁环是可经开关关掉的。若把它
// 放进门禁环, 一旦有人用 CLAUDE_GO_RUN_INTERCEPTORS 关掉门禁做回滚, 失败阶段会被
// 静默当成交付成功 —— 关掉门禁的语义只该是"不跑编译/测试/内容门禁", 绝不该是"把失败
// 当成功"。所以判定留在主路径, 门禁环只做它自己那三道闸。
func (ptm *ProductionTeamManager) computeRunDelivery(
	team *ProductionTeam, wf *WorkflowDef, executor *WorkflowExecutor, results []StageResult,
) (*teamRunContext, bool) {
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
			return nil, false
		}
	}
	return &teamRunContext{
		ptm: ptm, team: team, wf: wf, executor: executor,
		results: results, status: deliveryStatus,
	}, true
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

// workflowProducesCode 判定工作流是否产出可编译代码 (决定是否跑编译/测试门禁)。
// 写作/调研/分析类工作流产出文本, 不应跑 go build 门禁。
func workflowProducesCode(workflow string) bool {
	// 改为白名单 (原黑名单会把新增/分析类工作流误判为代码类, 触发无意义的
	// go build/test 门禁 + coder 修复死循环, 单团队曾因此烧掉数百万 token)。
	// 只有确实产出可编译代码的工作流才跑编译/测试门禁。
	// 动态(自定义)工作流: 读声明式字段, 不能按名字 switch(否则静默丢门禁)。
	if produces, _, isCustom := customWorkflowFlags(workflow); isCustom {
		return produces
	}
	switch strings.ToLower(strings.TrimSpace(workflow)) {
	case "development", "app", "game", "ml-training", "testing", "adversarial-dev":
		return true
	default:
		// trading-v2 / sector-scan / industry-map / finance / techblog /
		// research / creative / novel 等分析或写作类工作流一律不跑代码门禁。
		return false
	}
}

// runGlobalCompileGate runs a global compile check for the team.
// Returns empty string on success, error message on failure.
//
// 注: 用 exec.CommandContext + WithTimeout 包一层超时.
// 否则 AI 生成的 build.go / cgo 之类的死循环会让整个团队卡死.
func (ptm *ProductionTeamManager) runGlobalCompileGate(ctx context.Context, team *ProductionTeam) string {
	if team == nil || team.Cwd == "" {
		return ""
	}
	// 兜底: 无 go.mod 的目录不是 Go 模块, go build ./... 必然报 "no main module", 跳过。
	if _, err := os.Stat(filepath.Join(team.Cwd, "go.mod")); err != nil {
		// 跳过也留轨迹 (不发奖励): 否则事后无法分清"编译过了"与"根本没编译",
		// 而 §4.5 的教训正是"静默跳过会让人以为门禁过了"。
		ptm.writeGateSpan(ctx, team, gateSpanInput{
			Gate: RewardSourceGateCompile, Node: RewardSourceGateCompile,
			Input: team.Cwd, Detail: "无 go.mod, 非 Go 模块, 跳过编译门禁", Skipped: true,
		})
		return ""
	}
	// 执行超时刻意不挂在 ctx 上 (保持原语义: 门禁自己限时, 不受上游取消影响);
	// ctx 只用来取 trace RunID 给奖励事件归因。
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "build", "./...")
	cmd.Dir = team.Cwd
	out, err := cmd.CombinedOutput()
	gateErr := ""
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		gateErr = fmt.Sprintf("go build ./... timed out after 5m\n%s", string(out))
	case err != nil:
		gateErr = fmt.Sprintf("go build ./... failed: %v\n%s", err, string(out))
	}
	ptm.recordGateReward(ctx, team, RewardSourceGateCompile, gateErr)
	return gateErr
}

// recordGateReward 把确定性门禁结果发到 RewardBus (design/03 §4.2 价值排第 1 的信号源)。
//
// 为什么值得单独发: runGlobalCompileGate/runGlobalTestGate 真跑 go build / go test -race,
// 只信 exit code, 是全系统最不可能被 AI 说服的信号; 但此前这两处 RecordReward 调用数为 0,
// 奖励总线上只有 LLM 打分和 episode 终态两类软信号。
//
// NodeID 用门禁名而不是某个阶段名: 全局门禁是 run 级信号, 挂到某个阶段上会让该阶段
// (以及门禁失败后触发的修复阶段) 的学习反馈被"它正要修的失败"污染。
// 跳过的门禁 (无 go.mod / 无 cwd) 不发奖励 —— 没跑过就不是证据。
func (ptm *ProductionTeamManager) recordGateReward(ctx context.Context, team *ProductionTeam, source, gateErr string) {
	if ptm == nil || team == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	value, raw := 1.0, any("pass")
	if gateErr != "" {
		value, raw = -1.0, any(truncateResult(gateErr, 300))
	}
	// 奖励与轨迹的守卫**分开**: 只装了 TraceStore 没装 Evolution 的宿主 (或反之)
	// 仍应各得其一。合成一个 `evolution == nil → return` 会让 gate Span 悄悄跟着
	// 奖励一起消失, 那正是"实现了但零生产调用"的经典成因。
	if ptm.evolution != nil {
		ptm.evolution.RecordReward(RewardEvent{
			RunID:  trace.From(ctx).RunID,
			NodeID: source,
			Source: source,
			Value:  value,
			Raw:    raw,
			Team:   team.Name,
		})
	}
	// gate Span (design/03 §4.1 Kind=gate): 与奖励同一处发出, 保证"奖励有值但
	// 轨迹里查不到它凭什么"这种断链不会发生。Input 记被判定的工作目录 (门禁的判定
	// 对象是磁盘上的代码, 不是某段文本), Output 记报错全文供蒸馏读。
	ptm.writeGateSpan(ctx, team, gateSpanInput{
		Gate:   source,
		Node:   source,
		Input:  team.Cwd,
		Detail: gateErr,
		Score:  value,
		Raw:    raw,
		Pass:   gateErr == "",
	})
}

// runGlobalTestGate runs a global test check with race detection for the team.
// Returns empty string on success, error message on failure.
//
// 注: 用 exec.CommandContext + WithTimeout 包一层超时.
// 否则 AI 生成的代码里有死循环 (比如 kmeans 中 k > len(vectors) 的 for{} ),
// 整个团队会卡在这条命令上数十分钟. 12 分钟的超时足够正常单元测试跑完,
// 又能在病态情况下及时止血.
func (ptm *ProductionTeamManager) runGlobalTestGate(ctx context.Context, team *ProductionTeam) string {
	if team == nil || team.Cwd == "" {
		return ""
	}
	// 同 runGlobalCompileGate: 执行超时独立于 ctx, ctx 只用于奖励事件的 trace 归因。
	cctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "test", "-race", "-timeout", "120s", "./...")
	cmd.Dir = team.Cwd
	out, err := cmd.CombinedOutput()
	gateErr := ""
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		gateErr = fmt.Sprintf("go test -race ./... timed out after 12m (very likely an infinite loop in generated code)\n%s", string(out))
	case err != nil:
		gateErr = fmt.Sprintf("go test -race ./... failed: %v\n%s", err, string(out))
	}
	ptm.recordGateReward(ctx, team, RewardSourceGateTest, gateErr)
	return gateErr
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
	gateFn func(context.Context, *ProductionTeam) string,
	maxAttempts int,
) string {
	if gateFn == nil {
		return ""
	}
	// gateFn 收 ctx 只为把门禁结论按 trace RunID 发到 RewardBus (见 recordGateReward);
	// 门禁自身的执行超时仍由各 gate 内部独立控制。
	if ctx == nil {
		ctx = context.Background()
	}
	gateErr := gateFn(ctx, team)
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
		newErr := gateFn(ctx, team)
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
		recordTeamRun(ptm.metrics, "team", metrics.MTeamRunCount, 1, team, labels)
		recordTeamRun(ptm.metrics, "team", metrics.MTeamDurationSec, dur, team, labels)
		recordTeamRun(ptm.metrics, "team", metrics.MTeamSuccessCount, 1, team, labels)
		total, passed := 0, 0
		for _, s := range results {
			total++
			if s.Status == TaskCompleted {
				passed++
			}
		}
		if total > 0 {
			recordTeamRun(ptm.metrics, "team", metrics.MTeamStagePassRate, float64(passed)/float64(total), team, labels)
		}
	}

	// 触发进化学习 + Dreaming + 进化指标采集 (同 pipeline 路径, 经统一循环)
	ptm.submitLearn(team.Name)
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
	ptm.finishRefine(team, false) // 若本轮是精修: 清空反馈并留痕"未采纳"
	team.persist()

	// 失败路径也要补齐整团队级指标, 否则 dashboard 的成败对比/故障分析会缺数据。
	if ptm.metrics != nil {
		labels := map[string]string{"workflow": team.Workflow, "status": "failed"}
		recordTeamRun(ptm.metrics, "team", metrics.MTeamRunCount, 1, team, labels)
		recordTeamRun(ptm.metrics, "team", metrics.MTeamFailCount, 1, team, labels)
		if !startedAt.IsZero() {
			recordTeamRun(ptm.metrics, "team", metrics.MTeamDurationSec, time.Since(startedAt).Seconds(), team, labels)
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
	if team.cancel != nil {
		team.cancel()
	}
	team.Status = TeamStatusStopped
	team.FinishedAt = time.Now()
	team.mu.Unlock()
	team.persist() // persist 内部自锁, 先解锁避免重入死锁
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

// RefineTeam 让一个已产出结果的团队带着"运行后反馈"继续迭代优化。
//
// 这是"团队跑完后无法持续优化"痛点的核心入口: 与 RunTeam(需新目标)/ResumeTeam(仅失败恢复、
// 不收反馈) 不同, RefineTeam 接受已完成(completed/delivered)、失败或停止的团队 + 一段反馈,
// 复用检查点机制做"增量重入":
//   - targetStage 为空 → 整体带反馈重跑 (清空检查点)。
//   - targetStage 指定 → 仅失效该阶段及其后续, 之前阶段从检查点复用 (省 token, 不从头再生成)。
//
// 反馈通过 team.PendingFeedback 注入本轮所有重跑阶段的 prompt (见 workflow.go executeStage 的
// {user_feedback} 注入)。完成后清空反馈并把"反馈→是否采纳"留痕 (供 Evolution 经验闭环)。
func (ptm *ProductionTeamManager) RefineTeam(name, feedback, targetStage string) error {
	ptm.mu.RLock()
	team, ok := ptm.teams[name]
	ptm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return fmt.Errorf("精修反馈不能为空")
	}

	team.mu.Lock()
	switch team.Status {
	case TeamStatusRunning, TeamStatusRefining:
		team.mu.Unlock()
		return fmt.Errorf("团队 %q 正在执行中", name)
	case TeamStatusCreated:
		team.mu.Unlock()
		return fmt.Errorf("团队 %q 尚未运行过, 请先 /team run <名称> <目标>", name)
	}
	fromStatus := string(team.Status)
	// 上一轮的 RunID 必须在这里取: 下面 executeWorkflow 会把 LastRunID 覆盖成新一轮的,
	// 那时再取就把"用户对旧产出的不满"记到了新一轮头上 —— 归因反了。
	prevRunID := team.LastRunID
	team.mu.Unlock()

	// user.steer 负信号 (design/03 §4.2 第 4 行): 用户主动来精修, 定义上就是对上一轮
	// 产出的显式否定。此前这条最直接的人类反馈只写进 RefineHistory 供人看, 不入学习。
	ptm.recordSteerReward(team, prevRunID, feedback, targetStage)

	if !ptm.tryStartTeam(name) {
		return fmt.Errorf("团队 %q 正在启动中，请稍后再试", name)
	}

	// 计算精修范围: 决定哪些检查点失效 (即哪些阶段重跑)。
	wf := GetWorkflow(team.Workflow)
	var invalidated []string
	// 按阶段增量精修仅对 pipeline 模式可靠: 对抗(adversarial*)/编排(orchestrated) 模式的检查点
	// 键带轮次后缀(如 implement-round3)或图结构, 按声明阶段名失效会"静默失配"。非 pipeline 一律
	// 转整体重跑 (清空检查点), 保证反馈一定生效, 只是不增量复用。
	if targetStage != "" && wf != nil && !strings.EqualFold(wf.Mode, "pipeline") {
		ptm.notify(team.ChatID, fmt.Sprintf("ℹ️ 工作流 %q (%s 模式) 不支持按阶段增量精修, 已转为整体带反馈重跑", team.Workflow, wf.Mode))
		targetStage = ""
	}
	if targetStage != "" {
		invalidated = stagesFromTarget(wf, targetStage)
		if len(invalidated) == 0 {
			ptm.clearStarting(name)
			return fmt.Errorf("未找到阶段 %q (可用: %s)", targetStage, strings.Join(stageNamesOf(wf), ", "))
		}
		_ = InvalidateCheckpoints(team.dataDir, invalidated)
		// 图引擎路径的进度真源是 graph-journal, 不是 checkpoints.json。
		// 这里曾有一个静默失效的真 bug: 灰度开关 (CLAUDE_GO_GRAPH_ENGINE) 开着时
		// wf.Mode 仍是 "pipeline", 于是上面那道"非 pipeline 转整体重跑"的闸放行了
		// 按阶段精修, 但只失效 checkpoints.json 而 graph-journal 原样保留 →
		// Resume 重放全部 node.completed → 零节点执行、直接返回旧产出,
		// **用户的反馈静默消失**。用失效事件补上 (design/01 §4.3 InvalidateFrom)。
		invalidateGraphJournal(context.Background(), team, invalidated, "refine:"+targetStage)
	} else {
		// 整体重跑: 清空全部进度真源。
		// 图引擎路径的进度真源是 graph-journal 而非 checkpoints.json, 必须一并
		// 清掉——否则 Resume 会重放上一轮的 node.completed, 使"整体重跑"变成
		// 零节点执行并直接返回旧产出 (Replay 已按 run 隔离, 这里再断掉基线,
		// 两道一起才能保证重跑真的重跑)。
		// 与 newRunCoordinator 的"新的一轮"共用同一个清空口 (clearRunProgress):
		// 这里曾是两行内联删除, 与那边各写一份 —— 两份里只改了一份正是本轮修掉的缺陷。
		clearRunProgress(team.dataDir)
	}

	team.mu.Lock()
	if team.Status == TeamStatusRunning {
		team.mu.Unlock()
		ptm.clearStarting(name)
		return fmt.Errorf("团队 %q 正在执行中", name)
	}
	team.PendingFeedback = feedback
	team.FeedbackTarget = targetStage
	team.RefineHistory = append(team.RefineHistory, RefineEntry{
		At: time.Now(), Feedback: feedback, TargetStage: targetStage, FromStatus: fromStatus,
	})
	objective := team.Objective
	team.Status = TeamStatusRunning
	team.StartedAt = time.Now()
	team.FinishedAt = time.Time{}
	team.Error = ""
	ctx, cancel := context.WithCancel(context.Background())
	team.cancel = cancel
	team.doneCh = make(chan struct{})
	team.mu.Unlock()

	team.Blackboard.Write("objective", objective, "system", "context")
	team.Blackboard.Write("user-feedback", feedback, "user", "context")
	team.persist()

	ptm.notify(team.ChatID, fmt.Sprintf("🛠️ 团队 **%s** 进入精修迭代\n反馈: %s\n范围: %s",
		name, feedback, refineScopeLabel(targetStage, invalidated)))

	go func() {
		defer ptm.clearStarting(name)
		defer func() { close(team.doneCh) }()
		ptm.executeWorkflow(ctx, team, true) // isResume=true: 保留未失效阶段的检查点, 只重跑目标及后续
	}()
	return nil
}

// finishRefine 在团队本轮执行结束(成功/失败)时收尾精修: 清空待处理反馈、给本次 RefineEntry
// 标注是否采纳, 并把"已采纳的反馈"写入高权重记忆形成经验闭环 (供后续团队首跑规避同类问题)。
// 若本轮不是精修 (PendingFeedback 为空) 则直接返回。
func (ptm *ProductionTeamManager) finishRefine(team *ProductionTeam, accepted bool) {
	team.mu.Lock()
	fb := team.PendingFeedback
	if fb == "" {
		team.mu.Unlock()
		return
	}
	team.PendingFeedback = ""
	team.FeedbackTarget = ""
	if n := len(team.RefineHistory); n > 0 {
		a := accepted
		team.RefineHistory[n-1].Accepted = &a
	}
	workflow, objective := team.Workflow, team.Objective
	team.mu.Unlock()

	if accepted && ptm.memWriter != nil {
		ptm.memWriter.AddTeamMemory(team.Name, workflow, objective,
			fmt.Sprintf("[精修反馈·已采纳] 用户要求: %s", truncateResult(fb, 300)))
	}
}

// LatestFinishedTeam 返回某会话中最近一个"已结束"(completed/delivered/failed/stopped) 的团队,
// 供"完成态消息→精修"路由使用。无则返回 nil。
func (ptm *ProductionTeamManager) LatestFinishedTeam(chatID string) *ProductionTeam {
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()
	var latest *ProductionTeam
	for _, t := range ptm.teams {
		if chatID != "" && t.ChatID != chatID {
			continue
		}
		switch t.Status {
		case TeamStatusCompleted, TeamStatusDeliveredWithRemediation, TeamStatusFailed, TeamStatusStopped:
		default:
			continue
		}
		if latest == nil || t.FinishedAt.After(latest.FinishedAt) {
			latest = t
		}
	}
	return latest
}

// ForkTeam 把一个已运行过的团队复制为新团队 (含产出/检查点), 便于保留原版的前提下迭代精修。
func (ptm *ProductionTeamManager) ForkTeam(src, dst string) (*ProductionTeam, error) {
	source := ptm.GetTeam(src)
	if source == nil {
		return nil, fmt.Errorf("源团队 %q 不存在", src)
	}
	source.mu.Lock()
	workflow, objective, chatID, lang := source.Workflow, source.Objective, source.ChatID, source.Language
	srcStages := append([]StageResult(nil), source.Stages...)
	srcStatus := source.Status
	srcDataDir := source.dataDir
	source.mu.Unlock()

	nt, err := ptm.CreateTeam(dst, workflow, objective, chatID)
	if err != nil {
		return nil, err
	}
	if lang != "" {
		nt.SetLanguage(lang)
	}
	// 复制检查点, 使 fork 能从源团队的阶段产出继续增量精修。
	//
	// ⚠️ 已知的两源不对称 (刻意**不**在本轮一并改): 这里只复制 checkpoints.json,
	// 不复制 graph-journal。于是图路径上 fork 出来的团队没有可复用的进度, 精修时
	// 从头全跑一遍 —— 与旧路径不等价, 但方向是**多跑**而不是吃旧产出, 属 fail-safe。
	// 补齐它要连 team.LastRunID 一起搬 (按阶段失效靠它定位 run), 而 LastRunID 同时
	// 是奖励归因的键 (recordSteerReward): 搬过去会把 fork 的精修负信号记到**源团队**
	// 那一轮头上, 归因反了。那是 design/03 侧的一次语义决策, 不该顺手在这里做掉。
	if srcDataDir != "" && nt.dataDir != "" {
		if data, e := os.ReadFile(filepath.Join(srcDataDir, checkpointsFileName)); e == nil {
			_ = os.WriteFile(filepath.Join(nt.dataDir, checkpointsFileName), data, 0644)
		}
	}
	nt.mu.Lock()
	nt.Stages = srcStages
	if isSuccessfulTeamStatus(srcStatus) {
		nt.Status = srcStatus // 标为已完成副本, 可直接 RefineTeam
	}
	nt.mu.Unlock()
	nt.persist()
	return nt, nil
}

// stagesFromTarget 返回从 target 阶段(含)起、按声明顺序及其后的所有阶段名 (用于增量失效)。
func stagesFromTarget(wf *WorkflowDef, target string) []string {
	idx := -1
	for i, s := range wf.Stages {
		if s.Name == target || stripRoundSuffix(s.Name) == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	var out []string
	for i := idx; i < len(wf.Stages); i++ {
		out = append(out, wf.Stages[i].Name)
	}
	return out
}

func stageNamesOf(wf *WorkflowDef) []string {
	var out []string
	for _, s := range wf.Stages {
		out = append(out, s.Name)
	}
	return out
}

func refineScopeLabel(target string, invalidated []string) string {
	if target == "" {
		return "整体带反馈重跑"
	}
	return fmt.Sprintf("从阶段 `%s` 起重跑 (%d 个阶段, 之前阶段复用检查点)", target, len(invalidated))
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

// WriteTeamBlackboard 向**进程内**团队黑板写一条条目 (design/01 §4.11)。
//
// ---------------------------------------------------------------------------
// 它修的是哪个缺陷
// ---------------------------------------------------------------------------
//
// `POST /api/teams/:name/blackboard` (dashboard/v13_handlers.go) 此前只改磁盘上的
// `blackboard.json`, 而 `pkg/agent.Blackboard` **只在构造时 load 一次**、之后靠
// debounce 全量覆盖写盘。后果有两级:
//
//	① 运行中的团队看不到这次写入 (它读的是内存 entries);
//	② 更糟: 下一次 debounce 刷盘会用内存快照**整份覆盖**磁盘文件, 于是刚写进去的
//	   条目**被静默抹掉** —— 调用方拿到的是 200 OK。
//
// 那个接口已经把动作排进 :7777 队列 (`blackboard.write`), 但消费方
// (`Bot.DashboardTeamAction`) 的 switch 里没有这一支 ⇒ 落到 default 报"未知操作"。
// 本方法就是那一支缺的实现。
//
// 找不到内存实例时**返回错误而不是退回改磁盘**: 退回磁盘正是上面那个静默丢数据的
// 形态, 而报错至少让调用方知道"这次写入没生效"。
func (ptm *ProductionTeamManager) WriteTeamBlackboard(name, key, value, author, category string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("黑板写入需要 key")
	}
	team := ptm.GetTeam(name)
	if team == nil {
		return fmt.Errorf("团队 %q 不在本进程内 (无内存黑板可写)", name)
	}
	if team.Blackboard == nil {
		return fmt.Errorf("团队 %q 无黑板实例", name)
	}
	if author == "" {
		author = "dashboard"
	}
	if category == "" {
		category = "dashboard_write"
	}
	// 走 Write 而不是直接改 entries: Write 会 markDirty (于是这条会随下一次 debounce
	// 一起落盘, 不再被覆盖掉) **并派发 Watch 事件** —— 订阅方 (blackboard_watch.go)
	// 因此也能看到外部注入的条目。
	team.Blackboard.Write(key, value, author, category)
	return nil
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
	writeErr := os.WriteFile(reportPath, []byte(sb.String()), 0644)

	// 产物清单必须在报告落盘【之后】生成 (这样 REPORT.md 自身也进清单), 且与报告
	// 成败解耦: 报告写失败时更需要清单留住"产物到底有没有、采集有没有失败"的证据。
	// 清单同时覆盖 dataDir 与 team.Cwd 两个落点, 避免下游只扫一处漏采 (媒锻曾因此
	// 把已完成的音乐成品误置 review)。
	if manifestPath, err := WriteArtifactManifest(team); err != nil {
		log.Printf("[Teams] 产物清单生成失败: %v", err)
	} else {
		log.Printf("[Teams] 产物清单已保存: %s", manifestPath)
	}

	if writeErr != nil {
		log.Printf("[Teams] 保存报告失败: %v", writeErr)
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

// persist 持久化团队状态到文件。
// marshal 在 t.mu 下做一致性快照, 避免与 runAgent / 心跳等并发写者产生撕裂
// 快照或数据竞争; 文件 I/O 放锁外以免长时间持锁。
// 约定: 调用方【不得】持有 t.mu(本函数内部自锁, 否则重入死锁)。
//
// ---------------------------------------------------------------------------
// team.json 的定位: 下游契约 + 投影产物, **运行进度不再是它说了算**
// ---------------------------------------------------------------------------
//
// design/01 §4.3 定的是"journal 是唯一进度真源"。在图引擎路径上这条已经落地:
// 阶段进度 (Stages / StartedAt / FinishedAt / LastRunID) 由
// `<dataDir>/graph-journal/journal.jsonl` 派生, team.json 只是它的**缓存/投影**
// (投影函数与逐字段核实见 team_projection.go)。两处若不一致, **以 journal 为准**。
//
// 但 team.json 的**格式与文件位置是下游契约**, 不能删也不能改形:
// pkg/dashboard 直接读盘 (v14_handlers.go:651 合成实时 DAG、run_feedback.go:216
// 靠 lastRunId 认领 run), `team status` / 飞书卡片 / 8+ 下游平台都按现有形状分支。
//
// 仍然**只**由 team.json 承载 (journal 里没有, 也不该有) 的是**声明与团队层自己的账**:
// Name/Workflow/Objective/ChatID/Cwd/Language/CreatedAt/TaskIDs/Mailbox/
// RefineHistory/PendingFeedback/FeedbackTarget/Error/Status/Agents/Progress。
// 逐条理由见 team_projection.go 文件头二。
//
// ⚠️ 未覆盖: pipeline 路径不产 journal (它的进度真源仍是 checkpoints.json),
// 那条路上 team.json 依然是独立真源。边界记在 design/01 §六。
func (t *ProductionTeam) persist() {
	if t.dataDir == "" {
		return
	}
	t.mu.Lock()
	data, err := json.MarshalIndent(t, "", "  ")
	t.mu.Unlock()
	if err != nil {
		logging.For("teams").Warn("持久化失败", "team", t.Name, "err", err)
		return
	}
	os.MkdirAll(t.dataDir, 0755)
	_ = os.WriteFile(filepath.Join(t.dataDir, "team.json"), data, 0644)
}

// updateHeartbeat 将 Coordinator 的实时进展快照回填到 team 及运行中 agent 的
// 心跳字段并落盘。目的: 让既有展示入口(team status / dashboard, 都读 team.json)
// 能看到团队内部"此刻在做什么"(阶段/轮次/累计产出/心跳时间), 而不再只有结束态,
// 解决"看不清团队内部运行情况"。由 Coordinator.checkTeamHealth 每个心跳周期调用。
func (t *ProductionTeam) updateHeartbeat(prog ProgressState) {
	t.mu.Lock()
	snap := prog
	t.Progress = &snap
	now := time.Now()
	for _, ag := range t.Agents {
		// pipeline 模式: 运行中的 agent 已由 runAgent 置为"执行中"; 心跳周期刷新
		// LastBeat 证明存活, 并在有更细进展(orchestrated 经 ReportProgress)时升级
		// Phase —— 但不用空值覆盖 runAgent 已置的阶段。
		// orchestrated 模式 agent 多为 idle, 由 t.Progress 承载团队级可见性。
		if ag.Status == AgentStatusRunning {
			if prog.Phase != "" {
				ag.Phase = prog.Phase
			}
			ag.LastBeat = now
		}
	}
	t.mu.Unlock()
	t.persist() // persist 内部自锁做一致性快照, 故此处先解锁避免重入死锁
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
			// 运行进度以 journal 为准 (design/01 §六 投影化的**恢复路径**接线点;
			// 默认关, 见 team_projection.go)。放在状态机之前是刻意的: 补进度不改
			// team.Status, 而下面那段要按 Status 决定是否落盘 —— 先补再判, 补进去的
			// 阶段才会随那次 persist 一起落盘, 不必额外多写一次盘。
			backfilled := backfillTeamProgressFromJournal(&team)
			if team.Status == TeamStatusRunning {
				team.Status = TeamStatusFailed
				team.Error = "进程重启"
				team.FinishedAt = time.Now()
				team.persist()
			} else if backfilled > 0 {
				team.persist()
			}
			if backfilled > 0 {
				logging.For("teams").Info("按 journal 补齐团队运行进度",
					"team", team.Name, "added", backfilled)
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
		recordTeamRun(ptm.metrics, "team", metrics.MTeamRunCount, 1, team, labels)
		recordTeamRun(ptm.metrics, "team", metrics.MTeamSuccessCount, 1, team, labels)
		recordTeamRun(ptm.metrics, "team", metrics.MTeamDurationSec,
			team.FinishedAt.Sub(team.StartedAt).Seconds(), team, labels)
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
