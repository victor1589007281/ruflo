// Package api defines shared domain types for Ruflo orchestration (agents, tasks, memory, MCP, LLM, guidance, learning).
package api

import "time"

// --- Agent -------------------------------------------------------------------

// AgentState is the lifecycle state of an agent process or registration.
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

// AgentType classifies the role of an agent in a swarm.
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

// AgentDomain is a bounded context label for routing and policy.
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

// AgentStatus is a coarse health/availability signal.
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

// AgentCapabilities describes what an agent can do.
type AgentCapabilities struct {
	Tools       []string          `json:"tools,omitempty"`
	Skills      []string          `json:"skills,omitempty"`
	MaxParallel int               `json:"max_parallel,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// AgentMetrics captures runtime counters for an agent.
type AgentMetrics struct {
	TasksCompleted   int64              `json:"tasks_completed"`
	TasksFailed      int64              `json:"tasks_failed"`
	CPUMillis        int64              `json:"cpu_millis,omitempty"`
	MemoryBytes      int64              `json:"memory_bytes,omitempty"`
	LatencyP50Millis float64            `json:"latency_p50_millis,omitempty"`
	LatencyP99Millis float64            `json:"latency_p99_millis,omitempty"`
	LastHeartbeat    time.Time          `json:"last_heartbeat,omitempty"`
	Extra            map[string]float64 `json:"extra,omitempty"`
}

// Agent describes a registered orchestration agent.
type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         AgentType         `json:"type"`
	Domain       AgentDomain       `json:"domain"`
	State        AgentState        `json:"state"`
	Status       AgentStatus       `json:"status"`
	Capabilities AgentCapabilities `json:"capabilities"`
	Metrics      AgentMetrics      `json:"metrics"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Namespace    string            `json:"namespace,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// --- Task --------------------------------------------------------------------

// TaskType categorizes work units.
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

// TaskStatus is task lifecycle.
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

// TaskPriority is relative scheduling weight (higher = more urgent).
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

// --- Messaging ---------------------------------------------------------------

// MessageType classifies inter-agent or system messages.
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

// MessagePriority for delivery ordering hints.
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

// SwarmEvent is emitted by the orchestrator.
type SwarmEvent struct {
	Type      SwarmEventType `json:"type"`
	SwarmID   string         `json:"swarm_id"`
	AgentID   string         `json:"agent_id,omitempty"`
	TaskID    string         `json:"task_id,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// --- Hooks -------------------------------------------------------------------

// HookPriority orders hook execution.
type HookPriority int

const (
	HookPriorityLow    HookPriority = 10
	HookPriorityNormal HookPriority = 50
	HookPriorityHigh   HookPriority = 90
)

// HookContext is passed into hook handlers.
type HookContext struct {
	HookName  string            `json:"hook_name"`
	SessionID string            `json:"session_id,omitempty"`
	TaskID    string            `json:"task_id,omitempty"`
	AgentID   string            `json:"agent_id,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Args      map[string]any    `json:"args,omitempty"`
}

// HookEvent classifies hook invocations.
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

// HookResult is the outcome of a hook.
type HookResult struct {
	OK       bool           `json:"ok"`
	Message  string         `json:"message,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Duration time.Duration  `json:"-"`
}

// --- LLM ---------------------------------------------------------------------

// LLMProvider names a model backend.
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

// LLMMessage is one chat turn.
type LLMMessage struct {
	Role    string `json:"role"` // system, user, assistant, tool
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
}

// LLMRequest is a unified completion request.
type LLMRequest struct {
	Provider    LLMProvider       `json:"provider"`
	Model       string            `json:"model"`
	Messages    []LLMMessage      `json:"messages"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	TopP        float64           `json:"top_p,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	Tools       []map[string]any  `json:"tools,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// LLMUsage reports token accounting when available.
type LLMUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// LLMResponse is a normalized model output.
type LLMResponse struct {
	Provider     LLMProvider    `json:"provider"`
	Model        string         `json:"model"`
	Text         string         `json:"text"`
	FinishReason string         `json:"finish_reason,omitempty"`
	ToolCalls    []LLMToolCall  `json:"tool_calls,omitempty"`
	Usage        LLMUsage       `json:"usage"`
	Raw          map[string]any `json:"raw,omitempty"`
}

// LLMToolCall is a structured tool invocation from the model.
type LLMToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	RawJSON   string         `json:"raw_json,omitempty"`
}

// --- Guidance / policy -------------------------------------------------------

// GuidanceRule is a single enforceable rule.
type GuidanceRule struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Severity    string            `json:"severity,omitempty"` // info, warn, block
	Expression  string            `json:"expression,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// RuleShard partitions rules for distribution.
type RuleShard struct {
	ShardID string         `json:"shard_id"`
	Rules   []GuidanceRule `json:"rules"`
	Version string         `json:"version"`
}

// PolicyBundle is a versioned set of shards.
type PolicyBundle struct {
	ID        string      `json:"id"`
	Version   string      `json:"version"`
	Shards    []RuleShard `json:"shards"`
	CreatedAt time.Time   `json:"created_at"`
}

// --- Learning (ReasoningBank-style) -----------------------------------------

// Pattern is a distilled reusable outcome.
type Pattern struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Score       float64           `json:"score,omitempty"`
	Embedding   []float32         `json:"-"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// TrajectoryStep is one step in an agent trajectory.
type TrajectoryStep struct {
	StepIndex int               `json:"step_index"`
	Action    string            `json:"action"`
	Reward    float64           `json:"reward,omitempty"`
	Obs       map[string]any    `json:"obs,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// Trajectory is a full episode.
type Trajectory struct {
	ID        string           `json:"id"`
	Steps     []TrajectoryStep `json:"steps"`
	Verdict   string           `json:"verdict,omitempty"` // success, failure, partial
	CreatedAt time.Time        `json:"created_at"`
}

// --- Swarm topology / consensus (pkg/swarm) -----------------------------------

// TopologyType names coordination graph shape.
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

// ConsensusAlgorithm selects agreement strategy.
type ConsensusAlgorithm string

const (
	ConsensusRaft      ConsensusAlgorithm = "raft"
	ConsensusByzantine ConsensusAlgorithm = "byzantine"
	ConsensusGossip    ConsensusAlgorithm = "gossip"
	ConsensusCRDT      ConsensusAlgorithm = "crdt"
	ConsensusQuorum    ConsensusAlgorithm = "quorum"
)
