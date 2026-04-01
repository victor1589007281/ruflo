// Package api 定义 Ruflo 编排跨模块共享的领域类型：代理、任务、记忆、消息、蜂群、钩子、LLM、治理与学习等。
//
// 设计思路：JSON 标签与 CLI/MCP 交换格式对齐；部分运行态字段（如 Embedding、TTL）用 json:"-" 避免无谓序列化。
package api

import "time"

// --- Agent（代理）------------------------------------------------------------

// AgentState 表示代理进程或注册项的生命周期状态。
type AgentState string

const (
	AgentStateUnknown   AgentState = "unknown"
	AgentStateIdle      AgentState = "idle"
	AgentStateStarting  AgentState = "starting"
	AgentStateRunning   AgentState = "running"
	AgentStateBusy      AgentState = "busy"
	AgentStateStopping  AgentState = "stopping"
	AgentStateStopped   AgentState = "stopped"
	AgentStateFailed    AgentState = "failed"
	AgentStateSuspended AgentState = "suspended"
)

// AgentType 区分蜂群中代理承担的角色类型。
type AgentType string

const (
	AgentTypeCoordinator         AgentType = "coordinator"
	AgentTypeCoder               AgentType = "coder"
	AgentTypeTester              AgentType = "tester"
	AgentTypeReviewer            AgentType = "reviewer"
	AgentTypeArchitect           AgentType = "architect"
	AgentTypeResearcher          AgentType = "researcher"
	AgentTypeSecurityArchitect   AgentType = "security-architect"
	AgentTypeSecurityAuditor     AgentType = "security-auditor"
	AgentTypeMemorySpecialist    AgentType = "memory-specialist"
	AgentTypePerformanceEngineer AgentType = "performance-engineer"
	AgentTypePerformance         AgentType = "performance"
	AgentTypePlanner             AgentType = "planner"
	AgentTypeCustom              AgentType = "custom"
	AgentTypeQueen               AgentType = "queen"
)

// AgentDomain 有界上下文标签，用于路由、配额与策略绑定。
type AgentDomain string

const (
	AgentDomainCore        AgentDomain = "core"
	AgentDomainSwarm       AgentDomain = "swarm"
	AgentDomainMemory      AgentDomain = "memory"
	AgentDomainHooks       AgentDomain = "hooks"
	AgentDomainNeural      AgentDomain = "neural"
	AgentDomainSecurity    AgentDomain = "security"
	AgentDomainProviders   AgentDomain = "providers"
	AgentDomainGuidance    AgentDomain = "guidance"
	AgentDomainIntegration AgentDomain = "integration"
	AgentDomainQueen       AgentDomain = "queen"
	AgentDomainSupport     AgentDomain = "support"
)

// AgentStatus 粗粒度健康与可用性信号（与 State 互补：State 偏流程，Status 偏观测）。
type AgentStatus string

const (
	AgentStatusHealthy    AgentStatus = "healthy"
	AgentStatusDegraded   AgentStatus = "degraded"
	AgentStatusUnhealthy  AgentStatus = "unhealthy"
	AgentStatusUnknown    AgentStatus = "unknown"
	AgentStatusSpawning   AgentStatus = "spawning"
	AgentStatusBusy       AgentStatus = "busy"
	AgentStatusTerminated AgentStatus = "terminated"
)

// AgentCapabilities 描述代理可调用的工具、技能与并发能力上限等。
type AgentCapabilities struct {
	Tools       []string          `json:"tools,omitempty"`        // 工具名或 MCP 工具 ID 列表
	Skills      []string          `json:"skills,omitempty"`       // 技能/工作流标识
	MaxParallel int               `json:"max_parallel,omitempty"` // 最大并行任务数
	Metadata    map[string]string `json:"metadata,omitempty"`     // 扩展键值
}

// AgentMetrics 聚合代理运行期计数与延迟分位数，用于调度与容量规划。
type AgentMetrics struct {
	TasksCompleted   int64              `json:"tasks_completed"`         // 成功完成任务数
	TasksFailed      int64              `json:"tasks_failed"`            // 失败任务数
	CPUMillis        int64              `json:"cpu_millis,omitempty"`    // 累计 CPU 毫秒（可选）
	MemoryBytes      int64              `json:"memory_bytes,omitempty"`  // 常驻内存估算
	LatencyP50Millis float64            `json:"latency_p50_millis,omitempty"`
	LatencyP99Millis float64            `json:"latency_p99_millis,omitempty"`
	LastHeartbeat    time.Time          `json:"last_heartbeat,omitempty"`
	Extra            map[string]float64 `json:"extra,omitempty"` // 自定义浮点指标
}

// Agent 描述已注册的一条编排代理实例（可持久化或经 API 返回）。
type Agent struct {
	ID           string            `json:"id"`           // 唯一 ID
	Name         string            `json:"name"`         // 显示名
	Type         AgentType         `json:"type"`         // 角色类型
	Domain       AgentDomain       `json:"domain"`       // 所属领域
	State        AgentState        `json:"state"`        // 生命周期状态
	Status       AgentStatus       `json:"status"`       // 健康/可用摘要
	Capabilities AgentCapabilities `json:"capabilities"`
	Metrics      AgentMetrics      `json:"metrics"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Namespace    string            `json:"namespace,omitempty"` // 记忆/隔离命名空间
	Labels       map[string]string `json:"labels,omitempty"`    // 调度与筛选标签
}

// --- Task（任务）------------------------------------------------------------

// TaskType 对工作单元做语义分类，便于路由到专门代理或策略。
type TaskType string

const (
	TaskTypeImplementation TaskType = "implementation"
	TaskTypeResearch       TaskType = "research"
	TaskTypeReview         TaskType = "review"
	TaskTypeTest           TaskType = "test"
	TaskTypeOrchestration  TaskType = "orchestration"
	TaskTypeMaintenance    TaskType = "maintenance"
	TaskTypeCustom         TaskType = "custom"
	TaskTypeCoding         TaskType = "coding"
	TaskTypeTesting        TaskType = "testing"
	TaskTypeSecurity       TaskType = "security"
	TaskTypeDeployment     TaskType = "deployment"
)

// TaskStatus 任务生命周期状态（队列、执行、终态等）。
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusQueued    TaskStatus = "queued"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusBlocked   TaskStatus = "blocked"
	TaskStatusSucceeded TaskStatus = "succeeded"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
	TaskStatusRetrying  TaskStatus = "retrying"
	TaskStatusTimeout   TaskStatus = "timeout"
	TaskStatusAssigned  TaskStatus = "assigned"
	TaskStatusCompleted TaskStatus = "completed"
)

// TaskPriority 相对调度权重，数值越大越优先（与具体调度算法结合使用）。
type TaskPriority int

const (
	TaskPriorityLowest   TaskPriority = 0
	TaskPriorityLow      TaskPriority = 25
	TaskPriorityNormal   TaskPriority = 50
	TaskPriorityHigh     TaskPriority = 75
	TaskPriorityCritical TaskPriority = 100
)

// TaskDefinition is a portable task spec.
type TaskDefinition struct {
	ID          string            `json:"id"`
	Type        TaskType          `json:"type"`
	Status      TaskStatus        `json:"status"`
	Priority    TaskPriority      `json:"priority"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	AgentID     string            `json:"agent_id,omitempty"`
	Domain      AgentDomain       `json:"domain,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Payload     map[string]any    `json:"payload,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	DueAt       *time.Time        `json:"due_at,omitempty"`
}

// --- Memory ------------------------------------------------------------------

// MemoryEntry is a stored memory record with optional vector for semantic search.
type MemoryEntry struct {
	ID         int64             `json:"id,omitempty"`
	Key        string            `json:"key"`
	Value      string            `json:"value"`
	Namespace  string            `json:"namespace"`
	Tags       []string          `json:"tags,omitempty"`
	Embedding  []float32         `json:"-"` // not always serialized
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	TTLSeconds *int64            `json:"ttl_seconds,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// MemoryQuery is a structured memory lookup.
type MemoryQuery struct {
	Namespace string   `json:"namespace"`
	Tags      []string `json:"tags,omitempty"`
	Text      string   `json:"text,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Offset    int      `json:"offset,omitempty"`
}

// SearchOptions tune vector and filter search.
type SearchOptions struct {
	Namespace    string            `json:"namespace,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	K            int               `json:"k,omitempty"`
	EF           int               `json:"ef,omitempty"`
	MinScore     float64           `json:"min_score,omitempty"`
	Offset       int               `json:"offset,omitempty"`
	OrderBy      string            `json:"order_by,omitempty"`
	Descending   bool              `json:"descending,omitempty"`
	Filters      map[string]string `json:"filters,omitempty"`
}

// SearchResult is one ranked memory hit.
type SearchResult struct {
	Entry *MemoryEntry `json:"entry"`
	Score float64      `json:"score"`
}

// MemoryType classifies memory storage tier or backend mode.
type MemoryType string

const (
	MemoryTypeSession    MemoryType = "session"
	MemoryTypePersistent MemoryType = "persistent"
	MemoryTypeVector     MemoryType = "vector"
	MemoryTypeCache      MemoryType = "cache"
	MemoryTypePattern    MemoryType = "pattern"
)

// --- Messaging（消息）--------------------------------------------------------

// MessageType 区分代理间或系统消息的语义类别。
type MessageType string

const (
	MessageTypeDirect    MessageType = "direct"
	MessageTypeBroadcast MessageType = "broadcast"
	MessageTypeEvent     MessageType = "event"
	MessageTypeCommand   MessageType = "command"
	MessageTypeReply     MessageType = "reply"
	MessageTypeTask      MessageType = "task"
	MessageTypeControl   MessageType = "control"
	MessageTypeHeartbeat MessageType = "heartbeat"
	MessageTypeConsensus MessageType = "consensus"
	MessageTypeACK       MessageType = "ack"
)

// MessagePriority 投递优先级提示（总线可实现抢占或加权队列）。
type MessagePriority int

const (
	MessagePriorityBackground MessagePriority = 0
	MessagePriorityNormal     MessagePriority = 50
	MessagePriorityHigh       MessagePriority = 80
	MessagePriorityCritical   MessagePriority = 100
)

// Message is a generic envelope.
type Message struct {
	ID          string            `json:"id"`
	Type        MessageType       `json:"type"`
	Priority    MessagePriority   `json:"priority"`
	From        string            `json:"from,omitempty"`
	To          string            `json:"to,omitempty"`
	Topic       string            `json:"topic,omitempty"`
	Payload     map[string]any    `json:"payload,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
	TTL         time.Duration     `json:"-"`
	ExpiresAt   time.Time         `json:"expires_at,omitempty"`
	RequiresACK bool              `json:"requires_ack,omitempty"`
	ACKID       string            `json:"ack_id,omitempty"`
	RetryCount  int               `json:"-"`
}

// --- Swarm -------------------------------------------------------------------

// SwarmEventType describes swarm lifecycle events.
type SwarmEventType string

const (
	SwarmEventInit       SwarmEventType = "init"
	SwarmEventAgentJoin  SwarmEventType = "agent_join"
	SwarmEventAgentLeave SwarmEventType = "agent_leave"
	SwarmEventTaskAssign SwarmEventType = "task_assign"
	SwarmEventConsensus  SwarmEventType = "consensus"
	SwarmEventShutdown   SwarmEventType = "shutdown"
	SwarmEventError      SwarmEventType = "error"
)

// SwarmStatus is aggregate swarm health.
type SwarmStatus string

const (
	SwarmStatusInactive     SwarmStatus = "inactive"
	SwarmStatusStarting     SwarmStatus = "starting"
	SwarmStatusActive       SwarmStatus = "active"
	SwarmStatusDraining     SwarmStatus = "draining"
	SwarmStatusStopped      SwarmStatus = "stopped"
	SwarmStatusFailed       SwarmStatus = "failed"
	SwarmStatusInitializing SwarmStatus = "initializing"
	SwarmStatusHealthy      SwarmStatus = "healthy"
	SwarmStatusDegraded     SwarmStatus = "degraded"
	SwarmStatusShuttingDown SwarmStatus = "shutting_down"
)

// SwarmEvent 由编排器发出的事件负载，供日志、订阅与回放。
type SwarmEvent struct {
	Type      SwarmEventType `json:"type"`
	SwarmID   string         `json:"swarm_id"`
	AgentID   string         `json:"agent_id,omitempty"`
	TaskID    string         `json:"task_id,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// --- Hooks（钩子）------------------------------------------------------------

// HookPriority 钩子执行顺序权重（数值大者优先或靠后由实现约定，此处为排序键）。
type HookPriority int

const (
	HookPriorityLow    HookPriority = 10
	HookPriorityNormal HookPriority = 50
	HookPriorityHigh   HookPriority = 90
)

// HookContext 传入钩子处理器的上下文：会话、任务、代理与环境快照。
type HookContext struct {
	HookName  string            `json:"hook_name"`
	SessionID string            `json:"session_id,omitempty"`
	TaskID    string            `json:"task_id,omitempty"`
	AgentID   string            `json:"agent_id,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Args      map[string]any    `json:"args,omitempty"`
}

// HookEvent 钩子调用种类（任务前后、编辑前后、会话等）。
type HookEvent string

const (
	HookEventPreTask        HookEvent = "pre_task"
	HookEventPostTask       HookEvent = "post_task"
	HookEventPreEdit        HookEvent = "pre_edit"
	HookEventPostEdit       HookEvent = "post_edit"
	HookEventSession        HookEvent = "session"
	HookEventRoute          HookEvent = "route"
	HookEventPreToolUse     HookEvent = "pre_tool_use"
	HookEventPostToolUse    HookEvent = "post_tool_use"
	HookEventPreRead        HookEvent = "pre_read"
	HookEventPostRead       HookEvent = "post_read"
	HookEventTaskProgress   HookEvent = "task_progress"
	HookEventAgentSpawn     HookEvent = "agent_spawn"
	HookEventAgentTerminate HookEvent = "agent_terminate"
)

// HookResult 单次钩子执行结果与可选结构化数据。
type HookResult struct {
	OK       bool           `json:"ok"`
	Message  string         `json:"message,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Duration time.Duration  `json:"-"`
}

// --- LLM（大语言模型）--------------------------------------------------------

// LLMProvider 模型后端标识枚举（与 pkg/providers 注册名对齐）。
type LLMProvider string

const (
	LLMProviderAnthropic  LLMProvider = "anthropic"
	LLMProviderOpenAI     LLMProvider = "openai"
	LLMProviderAzure      LLMProvider = "azure"
	LLMProviderGoogle     LLMProvider = "google"
	LLMProviderOllama     LLMProvider = "ollama"
	LLMProviderLocal      LLMProvider = "local"
	LLMProviderCustom     LLMProvider = "custom"
	LLMProviderCohere     LLMProvider = "cohere"
	LLMProviderRuvector   LLMProvider = "ruvector"
	LLMProviderOpenRouter LLMProvider = "openrouter"
	LLMProviderLiteLLM    LLMProvider = "litellm"
)

// LLMMessage 多轮对话中的单条消息（OpenAI/类 Chat 语义）。
type LLMMessage struct {
	Role    string `json:"role"` // system、user、assistant、tool 等
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"` // 工具调用等场景下的名称
}

// LLMRequest 统一补全请求：各 Provider 适配层负责映射到厂商 JSON。
type LLMRequest struct {
	Provider    LLMProvider       `json:"provider"`
	Model       string            `json:"model"`
	Messages    []LLMMessage      `json:"messages"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	TopP        float64           `json:"top_p,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	Tools       []map[string]any  `json:"tools,omitempty"` // 工具 schema
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// LLMUsage Token 用量统计（输入/输出/合计）。
type LLMUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// LLMResponse 归一化后的模型输出（文本、工具调用、用量与原始片段）。
type LLMResponse struct {
	Provider     LLMProvider    `json:"provider"`
	Model        string         `json:"model"`
	Text         string         `json:"text"`
	FinishReason string         `json:"finish_reason,omitempty"`
	ToolCalls    []LLMToolCall  `json:"tool_calls,omitempty"`
	Usage        LLMUsage       `json:"usage"`
	Raw          map[string]any `json:"raw,omitempty"`
}

// LLMToolCall 模型发起的结构化工具调用（参数可解析为 map，RawJSON 保留原文）。
type LLMToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	RawJSON   string         `json:"raw_json,omitempty"`
}

// --- Guidance / policy（治理与策略）----------------------------------------

// GuidanceRule 单条可执行规则：严重级别与表达式由上层引擎解释。
type GuidanceRule struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Severity    string            `json:"severity,omitempty"` // info、warn、block
	Expression  string            `json:"expression,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// RuleShard 规则分片，用于大规模规则集的分发与增量同步。
type RuleShard struct {
	ShardID string         `json:"shard_id"`
	Rules   []GuidanceRule `json:"rules"`
	Version string         `json:"version"`
}

// PolicyBundle 版本化的分片集合（一次策略发布的快照）。
type PolicyBundle struct {
	ID        string      `json:"id"`
	Version   string      `json:"version"`
	Shards    []RuleShard `json:"shards"`
	CreatedAt time.Time   `json:"created_at"`
}

// --- Learning（ReasoningBank 风格学习）--------------------------------------

// Pattern 蒸馏后的可复用模式（可挂向量用于相似检索）。
type Pattern struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Score       float64           `json:"score,omitempty"`
	Embedding   []float32         `json:"-"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// TrajectoryStep 代理轨迹中的单步：动作、奖励与观测。
type TrajectoryStep struct {
	StepIndex int               `json:"step_index"`
	Action    string            `json:"action"`
	Reward    float64           `json:"reward,omitempty"`
	Obs       map[string]any    `json:"obs,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// Trajectory 完整一条回合（episode）及总体裁决。
type Trajectory struct {
	ID        string           `json:"id"`
	Steps     []TrajectoryStep `json:"steps"`
	Verdict   string           `json:"verdict,omitempty"` // success、failure、partial
	CreatedAt time.Time        `json:"created_at"`
}

// --- Swarm topology / consensus（与 pkg/swarm 对应）-------------------------

// TopologyType 协调拓扑图形状名称。
type TopologyType string

const (
	TopologyHierarchical     TopologyType = "hierarchical"
	TopologyMesh             TopologyType = "mesh"
	TopologyHierarchicalMesh TopologyType = "hierarchical-mesh"
	TopologyRing             TopologyType = "ring"
	TopologyStar             TopologyType = "star"
	TopologyAdaptive         TopologyType = "adaptive"
	TopologyCentralized      TopologyType = "centralized"
	TopologyHybrid           TopologyType = "hybrid"
)

// ConsensusAlgorithm 多代理达成一致所采用的算法标识。
type ConsensusAlgorithm string

const (
	ConsensusRaft      ConsensusAlgorithm = "raft"
	ConsensusByzantine ConsensusAlgorithm = "byzantine"
	ConsensusGossip    ConsensusAlgorithm = "gossip"
	ConsensusCRDT      ConsensusAlgorithm = "crdt"
	ConsensusQuorum    ConsensusAlgorithm = "quorum"
)
