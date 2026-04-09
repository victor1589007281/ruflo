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
	"path/filepath"
	"sync"
	"time"
)

// TaskTracker 抽象 V2 任务管理, 与 builtin.TaskStore 通过 duck typing 对接。
type TaskTracker interface {
	AddTask(subject, description, owner string) (string, error)
	SetTaskStatus(id, status string) error
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

// ProductionTeamManager 生产级团队管理器
type ProductionTeamManager struct {
	teams       map[string]*ProductionTeam
	mu          sync.RWMutex
	baseDir     string
	factory     CreateAgentFunc
	notify      NotifyFunc
	taskTracker TaskTracker // 复用 V2 Task 系统
	pool        *AgentPool       // Agent 池 (动态扩缩)
	llm         LLMClient        // LLM 客户端 (蜂群分解)
	evolution   *EvolutionEngine // 自动进化引擎
	dreamer     DreamRecorder    // Dreaming 接口 (覆盖 team agent 会话)
}

// TeamManagerConfig 团队管理器配置。
type TeamManagerConfig struct {
	BaseDir     string
	Factory     CreateAgentFunc
	Notify      NotifyFunc
	TaskTracker TaskTracker
	Pool        *AgentPool
	LLM         LLMClient
	Evolution   *EvolutionEngine
	Dreamer     DreamRecorder
}

// NewProductionTeamManager 创建生产级团队管理器。
func NewProductionTeamManager(cfg TeamManagerConfig) *ProductionTeamManager {
	if cfg.Notify == nil {
		cfg.Notify = func(_, _ string) {}
	}
	ptm := &ProductionTeamManager{
		teams:       make(map[string]*ProductionTeam),
		baseDir:     cfg.BaseDir,
		factory:     cfg.Factory,
		notify:      cfg.Notify,
		taskTracker: cfg.TaskTracker,
		pool:        cfg.Pool,
		llm:         cfg.LLM,
		evolution:   cfg.Evolution,
		dreamer:     cfg.Dreamer,
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
			return nil, fmt.Errorf("未知工作流 %q, 可选: development, research, debate, swarm", workflow)
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
	// 蜂群模式: 使用 SwarmOrchestrator
	if team.Workflow == "swarm" {
		ptm.executeSwarm(ctx, team)
		return
	}

	wf := GetWorkflow(team.Workflow)
	if wf == nil {
		ptm.failTeam(team, "未知工作流: "+team.Workflow)
		return
	}

	executor := &WorkflowExecutor{
		factory:     ptm.factory,
		notify:      ptm.notify,
		chatID:      team.ChatID,
		taskTracker: ptm.taskTracker,
		evolution:   ptm.evolution,
	}

	// 使用 Coordinator 带重试和检查点执行
	coord := NewCoordinator(ptm.pool, ptm.taskTracker, ptm.notify, CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       team.dataDir,
		ChatID:        team.ChatID,
	})
	coord.ClearCheckpoints()

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

	// 触发进化学习 (DISTILL: 从轨迹中提炼经验)
	if ptm.evolution != nil {
		go func() {
			ptm.evolution.LearnFromTeam(context.Background(), team.Name)
			ptm.evolution.Consolidate()
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

	var summary string
	for _, r := range results {
		if r.Output != "" {
			summary += fmt.Sprintf("\n\n**[%s]**\n%s", r.Role, truncateResult(r.Output, 500))
		}
	}
	ptm.notify(team.ChatID, fmt.Sprintf("✅ 团队 **%s** 执行完成 (耗时 %v)\n\n**成果汇总:**%s",
		team.Name, time.Since(team.StartedAt).Round(time.Second), summary))
}

// executeSwarm 蜂群模式执行
func (ptm *ProductionTeamManager) executeSwarm(ctx context.Context, team *ProductionTeam) {
	swarm := NewSwarmOrchestrator(ptm.llm, ptm.pool, ptm.taskTracker, ptm.notify, team.ChatID, 8)
	swarm.evolution = ptm.evolution

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

	// 触发进化学习 + Dreaming
	if ptm.evolution != nil {
		go func() {
			ptm.evolution.LearnFromTeam(context.Background(), team.Name)
			ptm.evolution.Consolidate()
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

	var summary string
	for _, r := range results {
		if r.Output != "" {
			summary += fmt.Sprintf("\n\n**[%s] %s**\n%s", r.Role, r.Name, truncateResult(r.Output, 500))
		}
	}
	ptm.notify(team.ChatID, fmt.Sprintf("🐝 蜂群团队 **%s** 执行完成 (耗时 %v)\n\n**成果汇总:**%s",
		team.Name, time.Since(team.StartedAt).Round(time.Second), summary))
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

// StopFirstRunning 停止第一个正在运行的团队 (意图识别用)
func (ptm *ProductionTeamManager) StopFirstRunning() (string, error) {
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()

	for _, team := range ptm.teams {
		if team.Status == TeamStatusRunning {
			name := team.Name
			ptm.mu.RUnlock()
			err := ptm.StopTeam(name)
			ptm.mu.RLock()
			return name, err
		}
	}
	return "", fmt.Errorf("无正在运行的团队")
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

// persist 持久化团队状态到文件
func (t *ProductionTeam) persist() {
	if t.dataDir == "" {
		return
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		log.Printf("[Teams] 持久化失败 (%s): %v", t.Name, err)
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
