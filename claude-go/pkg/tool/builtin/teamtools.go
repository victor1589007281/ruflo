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
	teamCreateToolName = "TeamCreate"
	teamDeleteToolName = "TeamDelete"
)

// teamEntity 表示一个团队。
//
// 曾经这里还有一份 Mailboxes map[string][]teamMessage (json:"-")，由 TeamMailbox
// 工具写入。它在 2026-09-25 被删除，理由是**它是一份空承诺而不是一份冗余**:
// 该工具已注册进工具池，模型真的会调，工具返回「已投递消息给 %q」，而那条消息
// 落进一个**全仓没有任何读者**的 map —— 模型因此相信消息已送达，不再用别的方式
// 传达。这比"未接线"更糟: 未接线只是没效果，这个是**主动误导**。
//
// 全仓的信箱收敛为 pkg/agent 的 PersistentMailbox 一份 (见 pkg/agent/mailbox.go)。
type teamEntity struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// teamStore 共享的团队存储。
type teamStore struct {
	mu          sync.Mutex
	teams       map[string]*teamEntity
	currentTeam string
	lastCreate  time.Time // 团队创建限流
}

// NewTeamStore 构造空的团队存储。
func NewTeamStore() *teamStore {
	return &teamStore{
		teams: make(map[string]*teamEntity),
	}
}

// --- SendMessage ---

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

// Name 返回工具名 "TeamCreate"。
func (t *TeamCreateTool) Name() string { return teamCreateToolName }

// Description 返回工具描述 (创建团队并自动切换为当前活动团队)。
func (t *TeamCreateTool) Description() string {
	return `创建一个新团队（名称唯一），并自动切换为当前活动团队。`
}

// InputSchema 返回 JSON Schema (team_name 必填, description 可选)。
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

// IsReadOnly 恒为 false —— TeamCreate 会写入团队存储。
func (t *TeamCreateTool) IsReadOnly(_ json.RawMessage) bool        { return false }
// IsConcurrencySafe 恒为 true —— 内部靠 teamStore 的互斥锁串行化。
func (t *TeamCreateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 在计划模式下拒绝创建团队, 其余情况放行。
func (t *TeamCreateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止创建团队"}
	}
	return nil
}

// Call 校验名称非空与 30 秒创建冷却, 成功后写入团队并切换当前团队指针。
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

	// 限流: 30秒内只允许创建一个团队，防止 LLM 基于历史上下文重复创建
	if !t.store.lastCreate.IsZero() && time.Since(t.store.lastCreate) < 30*time.Second {
		return &tool.ToolResult{Content: "团队创建过于频繁，请稍后再试（30秒冷却）", IsError: true}, nil
	}

	key := in.TeamName
	if _, exists := t.store.teams[key]; exists {
		return &tool.ToolResult{Content: fmt.Sprintf("团队已存在: %q", key), IsError: true}, nil
	}
	ent := &teamEntity{
		Name:        in.TeamName,
		Description: in.Description,
	}
	t.store.teams[key] = ent
	t.store.currentTeam = key
	t.store.lastCreate = time.Now()

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

// Name 返回工具名 "TeamDelete"。
func (t *TeamDeleteTool) Name() string { return teamDeleteToolName }

// Description 返回工具描述 (删除当前活动团队)。
func (t *TeamDeleteTool) Description() string {
	return `删除当前活动团队及其信箱数据；若无当前团队则报错。`
}

// InputSchema 返回空对象 Schema (TeamDelete 不接受参数, 只删当前团队)。
func (t *TeamDeleteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

// IsReadOnly 恒为 false —— TeamDelete 会删除团队数据。
func (t *TeamDeleteTool) IsReadOnly(_ json.RawMessage) bool        { return false }
// IsConcurrencySafe 恒为 true —— 内部靠 teamStore 的互斥锁串行化。
func (t *TeamDeleteTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 在计划模式下拒绝删除团队, 其余情况放行。
func (t *TeamDeleteTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止删除团队"}
	}
	return nil
}

// Call 删除当前团队并清空当前指针; 无当前团队时报错。
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
