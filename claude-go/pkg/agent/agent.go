// Package agent 实现 Agent/Task 子代理系统。
// 对应 TS 源码: review/claude/src/tools/AgentTool/AgentTool.tsx
//                review/claude/src/tools/AgentTool/runAgent.ts
//
// Agent 工具是 Claude Code 最强大的功能之一:
//   - LLM 可以通过 "Agent" 工具派生子代理
//   - 子代理拥有独立的对话循环 (嵌套 queryLoop)
//   - 子代理可以拥有自己的工具集 (工具过滤)
//   - 子代理可以连接额外的 MCP 服务器
//   - 子代理执行完毕后，结果返回给父循环
//
// Agent Teams:
//   - 多个子代理可以协作 (Team)
//   - 通过 mailbox 进行消息传递
//   - 支持 InProcess 和 Tmux 两种后端
//
// 旧名称: "Task" (现在 "Task" 是 LEGACY_AGENT_TOOL_NAME)
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	AgentToolName  = "Agent"
	LegacyToolName = "Task" // 向后兼容
)

// agentInput Agent 工具的输入
type agentInput struct {
	Prompt       string `json:"prompt"`
	Description  string `json:"description,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
	Model        string `json:"model,omitempty"`
	ReadOnly     bool   `json:"readonly,omitempty"`
	Resume       string `json:"resume,omitempty"` // 恢复之前的代理
}

// RunAgentFunc 子代理执行函数的签名
// 外部注入，用于避免循环依赖 (engine → agent → engine)
type RunAgentFunc func(ctx context.Context, prompt string, opts RunOptions) (string, error)

// RunOptions 子代理运行选项
type RunOptions struct {
	SubagentType string
	Model        string
	ReadOnly     bool
	ParentID     types.AgentID
	// MaxTurns 子代理最大循环回合数; 0 = 不限制 (沿用主会话 Config.MaxTurns)。
	// delegate_task 用它给每个子代理兜底, 防跑量任务失控。
	MaxTurns int
}

// AgentTool Agent/Task 工具实现
type AgentTool struct {
	runAgent RunAgentFunc
	// trace 图外派生的轨迹底座 (design/01 §4.8, 见 subagent_span.go)。
	// 可空 —— 未注入时整套可见性采集是零成本 no-op, 行为与改造前一字不变。
	trace *tracestore.Store
}

// NewAgentTool 创建 Agent 工具
func NewAgentTool(runFn RunAgentFunc) *AgentTool {
	return &AgentTool{runAgent: runFn}
}

var _ tool.AliasedTool = (*AgentTool)(nil)

func (t *AgentTool) Name() string { return AgentToolName }

// Aliases returns legacy tool names accepted by the API (e.g. "Task").
func (t *AgentTool) Aliases() []string { return []string{LegacyToolName} }

func (t *AgentTool) Description() string {
	return `Launch a new agent to handle complex, multi-step tasks autonomously.
The agent runs in a separate context and returns results when complete.`
}

func (t *AgentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string", "description": "The task for the agent to perform."},
			"description": {"type": "string", "description": "A short description of the task."},
			"subagent_type": {"type": "string", "description": "Subagent type to use."},
			"model": {"type": "string", "description": "Optional model to use."},
			"readonly": {"type": "boolean", "description": "If true, agent runs in read-only mode."},
			"resume": {"type": "string", "description": "Agent ID to resume from."}
		},
		"required": ["prompt"]
	}`)
}

func (t *AgentTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *AgentTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *AgentTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 执行 Agent 工具调用 — 派生子代理。
// 对应 TS: AgentTool.tsx 中的 call() → runAgent.ts 中的 runAgent()
//
// 流程:
//   1. 解析输入
//   2. 创建子代理上下文 (createSubagentContext)
//   3. 调用 runAgent (本质是嵌套的 queryLoop)
//   4. 收集子代理的所有输出
//   5. 返回最终结果
func (t *AgentTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in agentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	if t.runAgent == nil {
		return &tool.ToolResult{Content: "Agent 运行函数未配置", IsError: true}, nil
	}

	// 图外派生的可见性收编 (design/01 §4.8, 见 subagent_span.go)。
	//
	// 挂在**这里**而不是各 runNestedAgent 里: 全仓每一次子代理派生都必经本方法
	// (feishu/session.go:390,621 与 main.go:3065 的 RunAgentFunc 唯一调用方就是它),
	// 挂一处就覆盖两条路径, 且将来新增入口自动获得同样的可见性。分别在两个
	// runNestedAgent 里各写一遍则必然漂移 —— 那正是 §1.2 记的那种漂移形态。
	start := time.Now()
	tokensBefore := subagentTokensBefore(ctx)
	depth := SubagentDepth(ctx) + 1
	ctx = withSubagentDepth(ctx, depth)

	result, err := t.runAgent(ctx, in.Prompt, RunOptions{
		SubagentType: in.SubagentType,
		Model:        in.Model,
		ReadOnly:     in.ReadOnly,
		ParentID:     tctx.AgentID,
	})
	// 成败两路都要记: 失败的派生同样烧了 token、同样占了墙钟, 只记成功的会让
	// "为什么这个节点跑了 8 分钟才产出两行"永远解释不了。
	observeSubagent(t.trace, ctx, in, depth, result, err, start, tokensBefore)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("Agent 执行失败: %v", err), IsError: true}, nil
	}

	return &tool.ToolResult{Content: result}, nil
}

// ============================================================================
// Agent Teams (团队管理)
// 对应 TS: tools/TeamCreateTool/, tools/TeamDeleteTool/, tools/SendMessageTool/
// ============================================================================

// Team 代理团队
type Team struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Members     map[string]*TeamMember `json:"members"`
	Mailbox     []TeamMessage          `json:"mailbox"`
	CreatedAt   time.Time              `json:"createdAt"`
	mu          sync.Mutex
}

// TeamMember 团队成员
type TeamMember struct {
	ID     types.AgentID `json:"id"`
	Name   string        `json:"name"`
	Status string        `json:"status"` // idle, busy, stopped
}

// TeamMessage 团队消息
type TeamMessage struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Content   string    `json:"content"`
	Type      string    `json:"type"` // message, shutdown_request
	Timestamp time.Time `json:"timestamp"`
}

// TeamManager 团队管理器
// 对应 TS: utils/teamDiscovery.ts + utils/teammateMailbox.ts
type TeamManager struct {
	teams map[string]*Team
	mu    sync.Mutex
}

// NewTeamManager 创建团队管理器
func NewTeamManager() *TeamManager {
	return &TeamManager{
		teams: make(map[string]*Team),
	}
}

// CreateTeam 创建团队
func (tm *TeamManager) CreateTeam(name, description string) *Team {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	team := &Team{
		Name:        name,
		Description: description,
		Members:     make(map[string]*TeamMember),
		CreatedAt:   time.Now(),
	}
	tm.teams[name] = team
	return team
}

// DeleteTeam 删除团队
func (tm *TeamManager) DeleteTeam(name string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if _, ok := tm.teams[name]; ok {
		delete(tm.teams, name)
		return true
	}
	return false
}

// SendMessage 发送消息到指定成员
func (tm *TeamManager) SendMessage(teamName, from, to, content, msgType string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	team, ok := tm.teams[teamName]
	if !ok {
		return fmt.Errorf("团队 %q 不存在", teamName)
	}

	team.mu.Lock()
	defer team.mu.Unlock()

	team.Mailbox = append(team.Mailbox, TeamMessage{
		From:      from,
		To:        to,
		Content:   content,
		Type:      msgType,
		Timestamp: time.Now(),
	})
	return nil
}

// GetMessages 获取指定成员的消息
func (tm *TeamManager) GetMessages(teamName, memberName string) []TeamMessage {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	team, ok := tm.teams[teamName]
	if !ok {
		return nil
	}

	team.mu.Lock()
	defer team.mu.Unlock()

	var msgs []TeamMessage
	for _, msg := range team.Mailbox {
		if msg.To == memberName || msg.To == "" {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

// ListTeams 列出所有团队
func (tm *TeamManager) ListTeams() []*Team {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	result := make([]*Team, 0, len(tm.teams))
	for _, t := range tm.teams {
		result = append(result, t)
	}
	return result
}
