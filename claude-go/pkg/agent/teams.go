// Production Agent Teams — 生产级多 Agent 协作系统。
//
// 架构 (参考 CrewAI 角色编排 + LangGraph 状态机 + AutoGen 对话协议):
//
//	┌─────────────────────────────────────────────────┐
//	│ ProductionTeamManager (进程级单例)               │
//	│  - 管理所有 Team 的生命周期                      │
//	│  - 文件持久化: .claude/teams/{name}/team.json    │
//	│  - 通知回调: 向飞书推送进度                      │
//	├─────────────────────────────────────────────────┤
//	│ ProductionTeam                                  │
//	│  - Workflow: 定义协作模式 (pipeline/fan-out/debate)│
//	│  - Agents: 后台 goroutine, 独立 QueryEngine      │
//	│  - Tasks: 状态机 (pending→running→completed)     │
//	│  - Mailbox: 文件持久化, per-agent 收件箱          │
//	└─────────────────────────────────────────────────┘
//
// 飞书遥控命令:
//   /team create <name> --workflow <type> — 创建团队
//   /team run <name> <objective>          — 启动执行
//   /team status [name]                   — 查看状态
//   /team stop [name]                     — 停止团队
//   /team msg <team> <agent> <message>    — 发送消息
//   /team list                            — 列出所有团队
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

// CreateAgentFunc 创建 Agent 运行器的工厂函数。
// 由 feishu SessionManager 注入, 避免循环依赖。
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
	teams   map[string]*ProductionTeam
	mu      sync.RWMutex
	baseDir string          // .claude/teams/
	factory CreateAgentFunc // Agent 工厂
	notify  NotifyFunc      // 飞书通知回调
}

// NewProductionTeamManager 创建生产级团队管理器
func NewProductionTeamManager(baseDir string, factory CreateAgentFunc, notify NotifyFunc) *ProductionTeamManager {
	if notify == nil {
		notify = func(_, _ string) {}
	}
	ptm := &ProductionTeamManager{
		teams:   make(map[string]*ProductionTeam),
		baseDir: baseDir,
		factory: factory,
		notify:  notify,
	}
	ptm.loadPersistedTeams()
	return ptm
}

// ProductionTeam 生产级团队
type ProductionTeam struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Workflow    string            `json:"workflow"` // development, research, debate, custom
	Objective   string            `json:"objective"`
	ChatID      string            `json:"chatId"`
	Status      TeamStatus        `json:"status"`
	Agents      map[string]*BGAgent `json:"agents"`
	Stages      []StageResult     `json:"stages"`
	CreatedAt   time.Time         `json:"createdAt"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	FinishedAt  time.Time         `json:"finishedAt,omitempty"`
	Mailbox     []MailMessage     `json:"mailbox,omitempty"`
	Error       string            `json:"error,omitempty"`

	mu      sync.Mutex
	cancel  context.CancelFunc
	mgr     *ProductionTeamManager
	dataDir string
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
	StartedAt time.Time  `json:"startedAt,omitempty"`
	Duration  string     `json:"duration,omitempty"`
}

// MailMessage 邮箱消息
type MailMessage struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Content   string    `json:"content"`
	Type      string    `json:"type"` // message, result, error, shutdown
	Timestamp time.Time `json:"timestamp"`
}

// CreateTeam 创建团队
func (ptm *ProductionTeamManager) CreateTeam(name, workflow, description, chatID string) (*ProductionTeam, error) {
	ptm.mu.Lock()
	defer ptm.mu.Unlock()

	if _, exists := ptm.teams[name]; exists {
		return nil, fmt.Errorf("团队 %q 已存在", name)
	}

	wf := GetWorkflow(workflow)
	if wf == nil {
		return nil, fmt.Errorf("未知工作流 %q, 可选: development, research, debate", workflow)
	}

	dataDir := filepath.Join(ptm.baseDir, name)
	os.MkdirAll(dataDir, 0755)

	team := &ProductionTeam{
		Name:        name,
		Description: description,
		Workflow:    workflow,
		ChatID:      chatID,
		Status:      TeamStatusCreated,
		Agents:      make(map[string]*BGAgent),
		CreatedAt:   time.Now(),
		mgr:         ptm,
		dataDir:     dataDir,
	}

	// 根据 workflow 预创建 agent 角色
	for _, stage := range wf.Stages {
		team.Agents[stage.Role] = &BGAgent{
			Name:   stage.Role,
			Role:   stage.Role,
			Status: AgentStatusIdle,
		}
	}

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

	team.persist()
	ptm.notify(team.ChatID, fmt.Sprintf("🚀 团队 **%s** 开始执行\n目标: %s\n工作流: %s", name, objective, team.Workflow))

	go ptm.executeWorkflow(ctx, team)
	return nil
}

// executeWorkflow 在后台执行工作流
func (ptm *ProductionTeamManager) executeWorkflow(ctx context.Context, team *ProductionTeam) {
	wf := GetWorkflow(team.Workflow)
	if wf == nil {
		ptm.failTeam(team, "未知工作流: "+team.Workflow)
		return
	}

	executor := &WorkflowExecutor{
		factory: ptm.factory,
		notify:  ptm.notify,
		chatID:  team.ChatID,
	}

	results, err := executor.Execute(ctx, wf, team.Objective, team)
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

	// 汇总最终结果
	var summary string
	for _, r := range results {
		if r.Output != "" {
			summary += fmt.Sprintf("\n\n**[%s]**\n%s", r.Role, truncateResult(r.Output, 500))
		}
	}
	ptm.notify(team.ChatID, fmt.Sprintf("✅ 团队 **%s** 执行完成 (耗时 %v)\n\n**成果汇总:**%s",
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

// DeleteTeam 删除团队
func (ptm *ProductionTeamManager) DeleteTeam(name string) error {
	ptm.mu.Lock()
	defer ptm.mu.Unlock()

	team, ok := ptm.teams[name]
	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}
	if team.Status == TeamStatusRunning {
		if team.cancel != nil {
			team.cancel()
		}
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
	team.persist()
	return nil
}

// GetTeam 获取团队信息
func (ptm *ProductionTeamManager) GetTeam(name string) *ProductionTeam {
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()
	return ptm.teams[name]
}

// ListTeams 列出所有团队
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
			// 运行中的团队重启后标记为 failed
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
