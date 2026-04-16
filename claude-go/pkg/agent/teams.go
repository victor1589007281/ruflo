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
//	┌────────────────────────────────────────────────────┐
//	│ ProductionTeamManager (进程级单例)                  │
//	│  - TaskTracker: 复用 V2 Task (LLM 可通过 TaskList 看到)│
//	│  - 文件持久化: .claude/teams/{name}/                │
//	│  - 通知回调: 向飞书推送进度                         │
//	├────────────────────────────────────────────────────┤
//	│ ProductionTeam                                     │
//	│  - Blackboard: 共享黑板 (bMAS 架构)                 │
//	│  - Workflow: pipeline / fanout / adversarial        │
//	│  - V2 TaskIDs: 每个阶段对应一个 V2 Task             │
//	└────────────────────────────────────────────────────┘
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

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
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

// DreamRecorder Dreaming 记录接口, 解耦 dreaming 包依赖。
// 实现者: dreaming.Dreamer (通过 duck typing)。
type DreamRecorder interface {
	RecordSession(record DreamSessionRecord)
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
	TeamStatusFailed    TeamStatus = "failed"
	TeamStatusStopped   TeamStatus = "stopped"
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

// ProductionTeamManager 生产级团队管理器
type ProductionTeamManager struct {
	teams       map[string]*ProductionTeam
	mu          sync.RWMutex
	baseDir     string
	cwd         string // 项目工作目录 (传递给团队, 用于编译验证)
	factory     CreateAgentFunc
	notify      NotifyFunc
	mediaNotify MediaNotifyFunc
	taskTracker TaskTracker      // 复用 V2 Task 系统
	pool        *AgentPool       // Agent 池 (动态扩缩)
	llm         LLMClient        // LLM 客户端 (蜂群分解)
	evolution   *EvolutionEngine // 自动进化引擎
	dreamer     DreamRecorder    // Dreaming 接口 (覆盖 team agent 会话)
	roles       *RoleRegistry    // 角色注册表
	memWriter   MemoryWriter     // 记忆写入 (团队完成后写入高权重记忆)
	metrics     *metrics.Collector // 持续观测指标采集器
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
	Dreamer     DreamRecorder
	Roles       *RoleRegistry
	MemWriter   MemoryWriter
}

// SetMemoryWriter 注入记忆写入器 (在 Bot 初始化后调用)。
func (ptm *ProductionTeamManager) SetMemoryWriter(mw MemoryWriter) {
	ptm.memWriter = mw
}

// Metrics 返回内部指标采集器 (供外部模块注入使用)。
func (ptm *ProductionTeamManager) Metrics() *metrics.Collector {
	return ptm.metrics
}

// NewProductionTeamManager 创建生产级团队管理器。
func NewProductionTeamManager(cfg TeamManagerConfig) *ProductionTeamManager {
	if cfg.Notify == nil {
		cfg.Notify = func(_, _ string) {}
	}
	// 指标采集器: 从 BaseDir 推导 stateDir (teams 目录的父目录)
	stateDir := filepath.Dir(cfg.BaseDir)
	ptm := &ProductionTeamManager{
		teams:       make(map[string]*ProductionTeam),
		baseDir:     cfg.BaseDir,
		cwd:         cfg.Cwd,
		factory:     cfg.Factory,
		notify:      cfg.Notify,
		mediaNotify: cfg.MediaNotify,
		taskTracker: cfg.TaskTracker,
		pool:        cfg.Pool,
		llm:         cfg.LLM,
		evolution:   cfg.Evolution,
		dreamer:     cfg.Dreamer,
		roles:       cfg.Roles,
		metrics:     metrics.NewCollector(stateDir),
	}
	ptm.loadPersistedTeams()
	return ptm
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
	Cwd        string              `json:"cwd,omitempty"` // 工作目录 (用于编译验证和文件清单)

	Blackboard *Blackboard `json:"-"` // 共享黑板 (不序列化, 独立持久化)
	mu         sync.Mutex
	cancel     context.CancelFunc
	mgr        *ProductionTeamManager
	dataDir    string
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
			return nil, fmt.Errorf("未知工作流 %q, 可选: development, research, debate, swarm, finance, techblog, creative", workflow)
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

// RunTeam 启动团队执行
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
	team.Objective = objective
	team.Status = TeamStatusRunning
	team.StartedAt = time.Now()
	team.Stages = nil
	team.Error = ""
	ctx, cancel := context.WithCancel(context.Background())
	team.cancel = cancel
	team.mu.Unlock()

	// 更新黑板上的目标
	team.Blackboard.Write("objective", objective, "system", "context")
	team.persist()
	ptm.notify(team.ChatID, fmt.Sprintf("🚀 团队 **%s** 开始执行\n目标: %s\n工作流: %s", name, objective, team.Workflow))

	go ptm.executeWorkflow(ctx, team)
	return nil
}

// executeWorkflow 在后台执行工作流
func (ptm *ProductionTeamManager) executeWorkflow(ctx context.Context, team *ProductionTeam) {
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
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       team.dataDir,
		ChatID:        team.ChatID,
	})
	coord.ClearCheckpoints()

	executor := &WorkflowExecutor{
		factory:     ptm.factory,
		notify:      ptm.notify,
		chatID:      team.ChatID,
		taskTracker: ptm.taskTracker,
		evolution:   ptm.evolution,
		roles:       ptm.roles,
		metrics:     ptm.metrics,
		pool:        ptm.pool,
		checkpoints: coord, // 注入 Coordinator 作为 CheckpointStore
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

	team.mu.Lock()
	team.Status = TeamStatusCompleted
	team.FinishedAt = time.Now()
	team.Stages = results
	team.mu.Unlock()
	team.persist()

	// 结构化运行报告 (可观测性: 供后续 AI 分析团队运行效果)
	report := logging.TeamRunReport{
		TeamName: team.Name, Workflow: team.Workflow, Objective: team.Objective,
		StartTime: team.StartedAt, EndTime: team.FinishedAt,
		DurationSec: team.FinishedAt.Sub(team.StartedAt).Seconds(),
		Status: string(team.Status),
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
		labels := map[string]string{"workflow": team.Workflow}
		ptm.metrics.RecordRun("team", metrics.MTeamRunCount, 1, team.Name, labels)
		ptm.metrics.RecordRun("team", metrics.MTeamDurationSec, report.DurationSec, team.Name, labels)
		if team.Status == TeamStatusCompleted {
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

// executeSwarm 蜂群模式执行
func (ptm *ProductionTeamManager) executeSwarm(ctx context.Context, team *ProductionTeam) {
	swarm := NewSwarmOrchestrator(ptm.llm, ptm.pool, ptm.taskTracker, ptm.notify, team.ChatID, 8)
	swarm.evolution = ptm.evolution
	swarm.roles = ptm.roles

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
	team.mu.Unlock()
	team.persist()
	ptm.notify(team.ChatID, fmt.Sprintf("❌ 团队 **%s** 执行失败: %s", team.Name, reason))
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
	return s[:max] + "..."
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
	team.mu.Unlock()

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
