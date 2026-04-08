// teamtools.go 实现 Agent Teams 风格的团队与信箱工具：SendMessage、TeamCreate、TeamDelete。
//
// 数据全部驻留内存：teams 映射保存团队元数据，每个团队下再有按接收方（to）划分的信箱队列。
// “当前团队”在成功执行 TeamCreate 后自动切换为新团队；TeamDelete 删除当前团队并清空当前指针。
//
// 并发: teamStore 使用 sync.Mutex，所有工具在 IsConcurrencySafe 上返回 true。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	sendMessageToolName = "SendMessage"
	teamCreateToolName  = "TeamCreate"
	teamDeleteToolName  = "TeamDelete"
)

// teamMessage 表示信箱中的一条消息。
type teamMessage struct {
	To      string `json:"to"`
	Message string `json:"message"`
	Summary string `json:"summary,omitempty"`
	SentAt  string `json:"sentAt"`
}

// teamEntity 表示一个团队及其信箱。
type teamEntity struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description,omitempty"`
	Mailboxes   map[string][]teamMessage `json:"-"`
}

// teamStore 共享的团队与信箱存储。
type teamStore struct {
	mu          sync.Mutex
	teams       map[string]*teamEntity
	currentTeam string
}

// NewTeamStore 构造空的团队存储。
func NewTeamStore() *teamStore {
	return &teamStore{
		teams: make(map[string]*teamEntity),
	}
}

// --- SendMessage ---

type sendMessageInput struct {
	To      string `json:"to"`
	Message string `json:"message"`
	Summary string `json:"summary,omitempty"`
}

// SendMessageTool 向指定接收方（代理人名称）投递一条消息到当前团队的信箱。
type SendMessageTool struct {
	store *teamStore
}

// NewSendMessageTool 构造 SendMessage 工具。
func NewSendMessageTool(store *teamStore) *SendMessageTool {
	return &SendMessageTool{store: store}
}

func (t *SendMessageTool) Name() string { return sendMessageToolName }

func (t *SendMessageTool) Description() string {
	return `向当前团队内某成员的“信箱”发送一条消息，可选摘要字段便于检索。`
}

func (t *SendMessageTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"to": {"type": "string", "description": "接收方（例如队友名称）。"},
			"message": {"type": "string", "description": "消息正文。"},
			"summary": {"type": "string", "description": "可选短摘要。"}
		},
		"required": ["to", "message"]
	}`)
}

func (t *SendMessageTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *SendMessageTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *SendMessageTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止发送团队消息"}
	}
	return nil
}

func (t *SendMessageTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in sendMessageInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.To == "" || in.Message == "" {
		return &tool.ToolResult{Content: "to 与 message 不能为空", IsError: true}, nil
	}

	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.store.currentTeam == "" {
		return &tool.ToolResult{Content: "当前没有活动团队，请先调用 TeamCreate", IsError: true}, nil
	}
	ent, ok := t.store.teams[t.store.currentTeam]
	if !ok {
		return &tool.ToolResult{Content: "当前团队不存在，请先调用 TeamCreate", IsError: true}, nil
	}
	if ent.Mailboxes == nil {
		ent.Mailboxes = make(map[string][]teamMessage)
	}
	msg := teamMessage{
		To:      in.To,
		Message: in.Message,
		Summary: in.Summary,
		SentAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	ent.Mailboxes[in.To] = append(ent.Mailboxes[in.To], msg)

	return &tool.ToolResult{Content: fmt.Sprintf("已投递消息给 %q（团队 %q）", in.To, ent.Name)}, nil
}

// --- TeamCreate ---

type teamCreateInput struct {
	TeamName    string `json:"team_name"`
	Description string `json:"description,omitempty"`
}

// TeamCreateTool 创建新团队并设为当前团队。
type TeamCreateTool struct {
	store *teamStore
}

// NewTeamCreateTool 构造 TeamCreate 工具。
func NewTeamCreateTool(store *teamStore) *TeamCreateTool {
	return &TeamCreateTool{store: store}
}

func (t *TeamCreateTool) Name() string { return teamCreateToolName }

func (t *TeamCreateTool) Description() string {
	return `创建一个新团队（名称唯一），并自动切换为当前活动团队。`
}

func (t *TeamCreateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"team_name": {"type": "string", "description": "团队名称（唯一键）。"},
			"description": {"type": "string", "description": "团队描述（可选）。"}
		},
		"required": ["team_name"]
	}`)
}

func (t *TeamCreateTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *TeamCreateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TeamCreateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止创建团队"}
	}
	return nil
}

func (t *TeamCreateTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in teamCreateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.TeamName == "" {
		return &tool.ToolResult{Content: "team_name 不能为空", IsError: true}, nil
	}

	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	key := in.TeamName
	if _, exists := t.store.teams[key]; exists {
		return &tool.ToolResult{Content: fmt.Sprintf("团队已存在: %q", key), IsError: true}, nil
	}
	ent := &teamEntity{
		Name:        in.TeamName,
		Description: in.Description,
		Mailboxes:   make(map[string][]teamMessage),
	}
	t.store.teams[key] = ent
	t.store.currentTeam = key

	resp := map[string]string{
		"team_name": key,
		"status":    "created",
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// --- TeamDelete ---

// TeamDeleteTool 删除当前活动团队（无需参数）。
type TeamDeleteTool struct {
	store *teamStore
}

// NewTeamDeleteTool 构造 TeamDelete 工具。
func NewTeamDeleteTool(store *teamStore) *TeamDeleteTool {
	return &TeamDeleteTool{store: store}
}

func (t *TeamDeleteTool) Name() string { return teamDeleteToolName }

func (t *TeamDeleteTool) Description() string {
	return `删除当前活动团队及其信箱数据；若无当前团队则报错。`
}

func (t *TeamDeleteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

func (t *TeamDeleteTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *TeamDeleteTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TeamDeleteTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止删除团队"}
	}
	return nil
}

func (t *TeamDeleteTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.store.currentTeam == "" {
		return &tool.ToolResult{Content: "没有当前团队可删除", IsError: true}, nil
	}
	name := t.store.currentTeam
	delete(t.store.teams, name)
	t.store.currentTeam = ""
	return &tool.ToolResult{Content: fmt.Sprintf("已删除团队 %q", name)}, nil
}
