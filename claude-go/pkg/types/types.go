// Package types 定义了 Claude Code 客户端的核心数据类型。
// 对应 TS 源码: review/claude/src/types/ 和 review/claude/src/Tool.ts
//
// 这些类型忠实复刻了 Claude Code (Tengu) 的类型系统，
// 包括消息体系、工具定义、权限控制、Hook 事件等核心结构。
package types

import (
	"encoding/json"
	"time"
)

// ============================================================================
// 消息类型 (对应 TS: types/message.ts)
// ============================================================================

// MessageType 标识消息的发送方/类型
type MessageType string

const (
	MessageTypeUser       MessageType = "user"
	MessageTypeAssistant  MessageType = "assistant"
	MessageTypeSystem     MessageType = "system"
	MessageTypeAttachment MessageType = "attachment"
	MessageTypeProgress   MessageType = "progress"
	MessageTypeTombstone  MessageType = "tombstone"
)

// ContentBlockType 标识内容块的类型
type ContentBlockType string

const (
	ContentBlockText         ContentBlockType = "text"
	ContentBlockToolUse      ContentBlockType = "tool_use"
	ContentBlockToolResult   ContentBlockType = "tool_result"
	ContentBlockThinking     ContentBlockType = "thinking"
	ContentBlockImage        ContentBlockType = "image"
	ContentBlockDocument     ContentBlockType = "document"
)

// ContentBlock 表示消息中的一个内容块。
// 对应 TS: @anthropic-ai/sdk 中的 ContentBlockParam / ToolUseBlock 等。
// 使用统一结构体+类型标签的方式，而非 TS 中的联合类型。
type ContentBlock struct {
	Type ContentBlockType `json:"type"`

	// text 类型的内容
	Text string `json:"text,omitempty"`

	// tool_use 类型的字段
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result 类型的字段
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// thinking 类型的字段
	Thinking string `json:"thinking,omitempty"`
}

// Message 表示对话中的一条消息。
// 对应 TS: types/message.ts 中的 Message 联合类型。
// TS 中是一组 discriminated union (UserMessage | AssistantMessage | ...),
// Go 中用单一结构体 + Type 字段区分。
type Message struct {
	Type    MessageType    `json:"type"`
	UUID    string         `json:"uuid"`
	Content []ContentBlock `json:"content,omitempty"`

	// 仅 assistant 类型
	Model   string `json:"model,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Usage   *Usage `json:"usage,omitempty"`

	// API 错误信息
	IsApiErrorMessage bool   `json:"isApiErrorMessage,omitempty"`
	ApiError          string `json:"apiError,omitempty"`

	// 元数据标记 (isMeta: 由系统注入的恢复消息)
	IsMeta bool `json:"isMeta,omitempty"`

	// tool_result 关联
	ToolUseResult        string `json:"toolUseResult,omitempty"`
	SourceToolAssistantUUID string `json:"sourceToolAssistantUUID,omitempty"`

	// compact boundary 标记
	IsCompactBoundary bool `json:"isCompactBoundary,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

// Usage 记录 API token 使用量
// 对应 TS: usage 字段 (Anthropic SDK 的 Usage 类型)
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// StreamEventKind 流式事件类型 (用于实时 token-by-token 输出)。
type StreamEventKind int

const (
	StreamEventDelta       StreamEventKind = iota // 文本增量 (逐 token)
	StreamEventBlockDone                          // 一个 content block 完成
	StreamEventMessageDone                        // 整条 assistant 消息完成
	StreamEventToolStart                          // 工具调用开始
	StreamEventToolDone                           // 工具调用完成
	StreamEventError                              // 错误
)

// StreamEvent 统一流式事件 (支持 token-by-token 输出)。
type StreamEvent struct {
	Kind       StreamEventKind
	DeltaText  string   // Kind=Delta 时的文本片段
	BlockIndex int      // 当前 content block 索引
	Message    *Message // Kind=MessageDone 时的完整消息
	ToolName   string   // Kind=ToolStart/ToolDone
	ToolInput  string   // Kind=ToolStart 时的工具输入摘要
	ToolResult string   // Kind=ToolDone 时的工具结果摘要
	IsThinking bool     // thinking delta (区别于普通文本)
	Error      error    // Kind=Error 时的错误
}

// ============================================================================
// 工具类型 (对应 TS: Tool.ts)
// ============================================================================

// ToolInputSchema JSON Schema 的简化表示
type ToolInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

// ToolPermissionBehavior 定义工具的权限行为
// 对应 TS: Tool.ts 中的 checkPermissions 返回的行为控制
type ToolPermissionBehavior string

const (
	PermissionAllow ToolPermissionBehavior = "allow"
	PermissionDeny  ToolPermissionBehavior = "deny"
	PermissionAsk   ToolPermissionBehavior = "ask"
)

// ============================================================================
// QueryEngine 类型 (对应 TS: QueryEngine.ts, query.ts)
// ============================================================================

// QueryParams 查询循环参数
// 对应 TS: query.ts 中的 QueryParams 类型
type QueryParams struct {
	Messages      []Message         `json:"messages"`
	SystemPrompt  []string          `json:"systemPrompt"`
	UserContext   map[string]string `json:"userContext"`
	SystemContext map[string]string `json:"systemContext"`
	FallbackModel string            `json:"fallbackModel,omitempty"`
	QuerySource   string            `json:"querySource"`
	MaxTurns      int               `json:"maxTurns,omitempty"`
	TaskBudget    *TaskBudget       `json:"taskBudget,omitempty"`
}

// TaskBudget 任务预算限制
type TaskBudget struct {
	Total     int `json:"total"`
	Remaining int `json:"remaining,omitempty"`
}

// Terminal 表示 queryLoop 正常结束的原因
// 对应 TS: query/transitions.ts 中的 Terminal
type Terminal struct {
	Reason string `json:"reason"`
	Error  error  `json:"-"`
}

// Continue 表示 queryLoop 继续迭代的原因
// 对应 TS: query/transitions.ts 中的 Continue
type Continue struct {
	Reason string `json:"reason"`
}

// ============================================================================
// ToolUseContext (对应 TS: Tool.ts 中的 ToolUseContext)
// ============================================================================

// ToolUseContextOptions queryLoop 中工具执行的上下文选项
// 对应 TS: ToolUseContext.options 字段
type ToolUseContextOptions struct {
	Debug              bool              `json:"debug"`
	MainLoopModel      string            `json:"mainLoopModel"`
	Verbose            bool              `json:"verbose"`
	IsNonInteractive   bool              `json:"isNonInteractiveSession"`
	MCPClients         []MCPConnection   `json:"mcpClients,omitempty"`
	CustomSystemPrompt string            `json:"customSystemPrompt,omitempty"`
	AppendSystemPrompt string            `json:"appendSystemPrompt,omitempty"`
}

// MCPConnection MCP 服务器连接信息 (客户端视角)
// 对应 TS: services/mcp/types.ts 中的 MCPServerConnection
type MCPConnection struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // connected, pending, error
	Transport string `json:"transport"` // stdio, http, sse, ws
	Command   string `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
}

// ============================================================================
// 权限类型 (对应 TS: types/permissions.ts, utils/permissions/)
// ============================================================================

// PermissionMode 权限模式
// 对应 TS: types/permissions.ts 中的 PermissionMode
type PermissionMode string

const (
	PermissionModeDefault     PermissionMode = "default"      // 默认: 每次询问
	PermissionModeAuto        PermissionMode = "auto"         // 自动: 只读放行，写入需确认
	PermissionModePlan        PermissionMode = "plan"         // 计划: 只读
	PermissionModeBypass      PermissionMode = "bypass"       // 旁路: 全部允许
	PermissionModeDontAsk     PermissionMode = "dontAsk"      // 不再询问: 本会询问的调用自动拒绝
	PermissionModeAcceptEdits PermissionMode = "acceptEdits"  // 自动接受文件类编辑工具
)

// PermissionResult 权限检查结果
// 对应 TS: utils/permissions/PermissionResult.ts
type PermissionResult struct {
	Behavior ToolPermissionBehavior `json:"behavior"`
	Reason   string                 `json:"reason,omitempty"`
	Source   string                 `json:"source,omitempty"`
}

// PermissionRule 单条权限规则
// 对应 TS: utils/permissions/PermissionRule.ts
type PermissionRule struct {
	ToolName string                 `json:"tool_name"`
	Behavior ToolPermissionBehavior `json:"behavior"`
	Pattern  string                 `json:"pattern,omitempty"`
	Source   string                 `json:"source,omitempty"`
}

// ContentRule 基于工具输入内容的规则（在工具名匹配后按 Pattern 匹配 JSON/路径文本）
type ContentRule struct {
	ToolName string                 `json:"tool_name"`
	Pattern  string                 `json:"pattern"`
	Behavior ToolPermissionBehavior `json:"behavior"`
	Source   string                 `json:"source,omitempty"`
}

// ============================================================================
// Hook 类型 (对应 TS: types/hooks.ts, utils/hooks.ts)
// ============================================================================

// HookEvent hook 事件类型
// 对应 TS: types/hooks.ts 中的 HookEvent
type HookEvent string

const (
	HookEventPreToolUse         HookEvent = "PreToolUse"
	HookEventPostToolUse        HookEvent = "PostToolUse"
	HookEventPostToolUseFailure HookEvent = "PostToolUseFailure"
	HookEventStop               HookEvent = "Stop"
	HookEventStopFailure        HookEvent = "StopFailure"
	HookEventPreCompact         HookEvent = "PreCompact"
	HookEventPostCompact        HookEvent = "PostCompact"
	HookEventSessionStart       HookEvent = "SessionStart"
	HookEventSessionEnd         HookEvent = "SessionEnd"
	HookEventSubagentStart      HookEvent = "SubagentStart"
	HookEventSubagentStop       HookEvent = "SubagentStop"
	HookEventTeammateIdle       HookEvent = "TeammateIdle"
	HookEventTaskCompleted      HookEvent = "TaskCompleted"
	HookEventNotification       HookEvent = "Notification"

	// 以下为新补充的 HookEvent（覆盖业界生命周期管理缺失点）
	HookEventPreTurn           HookEvent = "PreTurn"
	HookEventPostTurn          HookEvent = "PostTurn"
	HookEventPreRequest        HookEvent = "PreRequest"
	HookEventPostRequest       HookEvent = "PostRequest"
	HookEventOnContextOverflow HookEvent = "OnContextOverflow"
	HookEventOnMaxTurnsReached HookEvent = "OnMaxTurnsReached"
	HookEventOnError           HookEvent = "OnError"
	HookEventOnRecovery        HookEvent = "OnRecovery"
	HookEventOnRateLimit       HookEvent = "OnRateLimit"
	HookEventOnRetry           HookEvent = "OnRetry"
	HookEventOnMessageFilter   HookEvent = "OnMessageFilter"
)

// HookType 定义 hook 的执行方式。
// 空值按 command 处理，保持向后兼容。
type HookType string

const (
	HookTypeCommand  HookType = "command"
	HookTypePrompt   HookType = "prompt"
	HookTypeHTTP     HookType = "http"
	HookTypeMCP      HookType = "mcp"
	HookTypePlugin   HookType = "plugin"
	HookTypeOPA      HookType = "opa"
	HookTypeFunction HookType = "function"
	HookTypeGRPC     HookType = "grpc"
)

// HookConfig 用户自定义 hook 配置
// 对应 TS: utils/hooks/hooksConfigManager.ts 中的 hook 定义
type HookConfig struct {
	Event    HookEvent `json:"event"`
	HookType HookType  `json:"hook_type,omitempty"`
	// If 非空时仅当 tool_name 匹配该 glob 模式（如 "Read*"）时才运行；会话类事件无 tool 名时不匹配。
	If      string `json:"if,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // 毫秒
	URL     string `json:"url,omitempty"`     // http 类型时可显式指定 URL；否则使用 Command 作为 URL

	// MCP 类型专用：通过 MCP 协议调用外部工具
	MCPServer string `json:"mcp_server,omitempty"` // MCP 服务器名称或 URL
	MCPTool   string `json:"mcp_tool,omitempty"`   // 要调用的 MCP 工具名

	// Plugin 类型专用：通过 Go plugin 加载 .so
	PluginPath   string `json:"plugin_path,omitempty"`   // .so 文件路径
	PluginSymbol string `json:"plugin_symbol,omitempty"` // 导出符号名（默认 "Hook"）

	// OPA 类型专用：通过 OPA eval 执行 Rego 策略
	OPAPolicy string `json:"opa_policy,omitempty"` // Rego 策略文件路径
	OPAQuery  string `json:"opa_query,omitempty"`  // Rego 查询表达式（默认 "data.hook.allow"）

	// Function 类型专用：通过 HTTP 调用函数端点
	FunctionName string `json:"function_name,omitempty"` // 函数名

	// gRPC 类型专用
	GRPCService string `json:"grpc_service,omitempty"` // gRPC 服务完整名（如 my.hook.PolicyService）
	GRPCMethod  string `json:"grpc_method,omitempty"`  // gRPC 方法名（如 Evaluate）
}

// HookInput hook 执行时的输入数据
// 对应 TS: types/hooks.ts 中的各种 HookInput 变体
type HookInput struct {
	Event       HookEvent       `json:"event"`
	SessionID   string          `json:"session_id"`
	ToolName    string          `json:"tool_name,omitempty"`
	ToolInput   json.RawMessage `json:"tool_input,omitempty"`
	ToolResult  string          `json:"tool_result,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
	Messages    []Message       `json:"messages,omitempty"`
	Transcript  string          `json:"transcript_path,omitempty"`

	// 以下为扩展字段，供新 HookEvent 使用
	TurnCount   int    `json:"turn_count,omitempty"`
	Model       string `json:"model,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	BackoffMs   int    `json:"backoff_ms,omitempty"`
	BudgetLevel int    `json:"budget_level,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Role        string `json:"role,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	Success     bool   `json:"success,omitempty"`
}

// HookOutput hook 执行后的输出
// 对应 TS: types/hooks.ts 中的 HookJSONOutput
type HookOutput struct {
	// 同步 hook 输出
	Decision    string `json:"decision,omitempty"`    // "block", "approve", "deny"
	Reason      string `json:"reason,omitempty"`

	// 异步 hook 输出
	ContinueDecision string `json:"continueDecision,omitempty"` // "block"

	// AdditionalContext prompt 类型 hook 或扩展输出：附加给调用方的说明性上下文（不执行 shell）
	AdditionalContext string `json:"additional_context,omitempty"`
}

// ============================================================================
// Compact 类型 (对应 TS: services/compact/)
// ============================================================================

// CompactionResult 压缩结果
// 对应 TS: services/compact/compact.ts 中的返回结构
type CompactionResult struct {
	SummaryMessages          []Message `json:"summaryMessages"`
	Attachments              []Message `json:"attachments"`
	HookResults              []Message `json:"hookResults"`
	PreCompactTokenCount     int       `json:"preCompactTokenCount"`
	PostCompactTokenCount    int       `json:"postCompactTokenCount"`
	TruePostCompactTokenCount int      `json:"truePostCompactTokenCount,omitempty"`
}

// ============================================================================
// Agent 类型 (对应 TS: tools/AgentTool/)
// ============================================================================

// AgentDefinition 代理定义
// 对应 TS: tools/AgentTool/loadAgentsDir.ts 中的 AgentDefinition
type AgentDefinition struct {
	AgentType       string            `json:"agentType"`
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	SystemPrompt    string            `json:"systemPrompt,omitempty"`
	AllowedTools    []string          `json:"allowedTools,omitempty"`
	DisallowedTools []string          `json:"disallowedTools,omitempty"`
	Model           string            `json:"model,omitempty"`
	MCPServers      []string          `json:"mcpServers,omitempty"`
	Memory          string            `json:"memory,omitempty"`
	Source          string            `json:"source,omitempty"` // built-in, user, plugin
}

// AgentID 代理唯一标识
type AgentID string

// ============================================================================
// Task/Todo 类型 (对应 TS: utils/tasks.ts, utils/todo/types.ts)
// ============================================================================

// TodoItem TodoWrite 工具使用的待办项
// 对应 TS: utils/todo/types.ts 中的 TodoItem
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed, cancelled
}

// Task Agent Teams 的任务
// 对应 TS: utils/tasks.ts 中的 Task
type Task struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"` // pending, in_progress, completed
	AssignedTo  string `json:"assignedTo,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// ============================================================================
// Memory 类型 (对应 TS: utils/claudemd.ts, memdir/)
// ============================================================================

// MemoryFile CLAUDE.md 文件条目
// 对应 TS: utils/claudemd.ts 中的加载结果
type MemoryFile struct {
	Path     string     `json:"path"`
	Content  string     `json:"content"`
	Type     MemoryType `json:"type"`
	Priority int        `json:"priority"`
}

// MemoryType 记忆文件类型
// 对应 TS: utils/memory/types.ts 中的 MemoryType
type MemoryType string

const (
	MemoryTypeManaged MemoryType = "managed" // /etc/claude-code/CLAUDE.md
	MemoryTypeUser    MemoryType = "user"    // ~/.claude/CLAUDE.md
	MemoryTypeProject MemoryType = "project" // CLAUDE.md, .claude/CLAUDE.md, .claude/rules/*.md
	MemoryTypeLocal   MemoryType = "local"   // CLAUDE.local.md
)

// ============================================================================
// API 类型 (对应 TS: services/api/claude.ts)
// ============================================================================

// CacheControl Anthropic prompt caching 控制 (顶层自动缓存)。
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// APIRequest Anthropic Messages API 请求
type APIRequest struct {
	Model         string         `json:"model"`
	Messages      []APIMessage   `json:"messages"`
	System        interface{}    `json:"system,omitempty"`
	Tools         []APITool      `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens"`
	Stream        bool           `json:"stream,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
	CacheControl  *CacheControl  `json:"cache_control,omitempty"`
}

// APIMessage API 格式的消息
type APIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// APITool API 格式的工具定义
type APITool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// APIResponse API 响应 (非流式)
type APIResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	Usage        *Usage         `json:"usage"`
}

// StreamDelta SSE 流式增量事件
type StreamDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index,omitempty"`

	// content_block_start
	ContentBlock *ContentBlock `json:"content_block,omitempty"`

	// content_block_delta
	Delta *DeltaContent `json:"delta,omitempty"`

	// message_start
	Message *APIResponse `json:"message,omitempty"`

	// message_delta
	Usage *Usage `json:"usage,omitempty"`
}

// DeltaContent SSE delta 的内容
type DeltaContent struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	// signature_delta: 代码签名增量 (用于验证工具调用来源)
	Signature    string `json:"signature,omitempty"`
}

// ServerToolUseBlock 服务端工具调用 (如 web_search_tool)
// 对应 TS: content_block 中 type="server_tool_use" 的块
type ServerToolUseBlock struct {
	Type      ContentBlockType `json:"type"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ServerID  string           `json:"server_id,omitempty"`
}

// APIError API 错误结构
// 对应 TS: services/api/errors.ts
type APIError struct {
	Type    string `json:"type"`
	Error   struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// StopReason 模型停止原因
type StopReason string

const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonToolUse      StopReason = "tool_use"
	StopReasonStopSequence StopReason = "stop_sequence"
	StopReasonRefusal      StopReason = "refusal"
)

// MemoryTypeEntrypoint MEMORY.md 入口文件类型
const MemoryTypeEntrypoint MemoryType = "entrypoint"

// ContentBlockServerToolUse 服务端工具调用块类型
const ContentBlockServerToolUse ContentBlockType = "server_tool_use"

// ContentBlockServerToolResult 服务端工具结果块类型
const ContentBlockServerToolResult ContentBlockType = "server_tool_result"
