package swarm

import (
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// CoordinatorConfig configures UnifiedSwarmCoordinator.
type CoordinatorConfig struct {
	Topology        api.TopologyType
	ConsensusAlgo   api.ConsensusAlgorithm
	AgentPoolMin    int
	AgentPoolMax    int
	HeartbeatMS     int
	MetricsInterval time.Duration
}

// CoordinatorState is runtime coordinator snapshot.
type CoordinatorState struct {
	mu           sync.RWMutex
	Status       api.SwarmStatus
	Topology     api.TopologyType
	AgentCount   int
	PendingTasks int
	LastElection time.Time
	StartedAt    time.Time
}

// CoordinatorMetrics aggregates coordinator KPIs.
type CoordinatorMetrics struct {
	mu                 sync.RWMutex
	TasksSubmitted     int64
	TasksAssigned      int64
	TasksCompleted     int64
	ConsensusProposals int64
	HealthRecoveries   int64
	MessagesProcessed  int64
	UpdatedAt          time.Time
}

// DomainConfig describes a 15-agent mesh slice.
type DomainConfig struct {
	Name         api.AgentDomain
	AgentNumbers []int
	Priority     int
	Capabilities []string
}

// TaskAssignment binds a task to an agent/domain.
type TaskAssignment struct {
	TaskID   string
	AgentID  string
	Domain   api.AgentDomain
	Score    float64
	Assigned time.Time
}

// ParallelExecutionResult summarizes concurrent task runs.
type ParallelExecutionResult struct {
	TaskID    string
	AgentID   string
	Success   bool
	Err       error
	Duration  time.Duration
	StartedAt time.Time
}

// DomainStatus per-domain utilization.
type DomainStatus struct {
	Domain      api.AgentDomain
	AgentCount  int
	ActiveTasks int
	AvgLoad     float64
	AvgHealth   float64
}

// TopologyConfig for TopologyManager.
type TopologyConfig struct {
	Type              api.TopologyType
	MaxMeshDegree     int
	HybridRandomPeers int
	RolePriorityOrder []string
}

// TopologyNode is a vertex in the coordination graph.
type TopologyNode struct {
	AgentID   string
	Role      string
	Neighbors map[string]struct{}
	JoinedAt  time.Time
}

// TopologyState is a snapshot of graph shape and elected leader.
type TopologyState struct {
	Type      api.TopologyType
	NodeCount int
	EdgeCount int
	Leader    string
}

// ConsensusConfig for engine construction (converted to consensus.Config; see pkg/swarm/consensus).
type ConsensusConfig struct {
	Algorithm    api.ConsensusAlgorithm
	NodeID       string
	Peers        []string
	ByzantineF   int
	GossipFanout int
	GossipTTL    int
}

// ConsensusProposal is an in-flight agreement unit (coordinator observability).
type ConsensusProposal struct {
	ID        string
	Value     []byte
	Proposer  string
	Term      uint64
	View      uint64
	CreatedAt time.Time
}

// ConsensusVote records a participant vote (coordinator observability).
type ConsensusVote struct {
	ProposalID string
	VoterID    string
	Approve    bool
	Term       uint64
	View       uint64
	Timestamp  time.Time
}

// ConsensusResult is the outcome of AwaitConsensus (coordinator API).
type ConsensusResult struct {
	ProposalID string
	Committed  bool
	Value      []byte
	Term       uint64
	Err        error
	FinishedAt time.Time
}

// MessageBusConfig for per-agent queues.
type MessageBusConfig struct {
	QueueCapacityPerAgent int
	ProcessBatchMax       int
	ProcessInterval       time.Duration
	SlidingWindowSecs     int
}

// MessageBusStats rolling throughput (snapshot; no mutex — copy safely).
type MessageBusStats struct {
	MessagesSent      int64
	MessagesDropped   int64
	MessagesACKed     int64
	MessagesProcessed int64
	MessagesPerSec    float64
	LastWindowStart   time.Time
	WindowCount       int64
	UpdatedAt         time.Time
}

// PooledAgent is a worker slot managed by AgentPool (distinct from api.Agent registration).
type PooledAgent struct {
	ID            string
	Type          api.AgentType
	Domain        api.AgentDomain
	State         api.AgentState
	Health        float64
	Load          float64
	LastHeartbeat time.Time
	CreatedAt     time.Time
	Metadata      map[string]string
}

// AgentPoolConfig sizing and timing.
type AgentPoolConfig struct {
	MinSize          int
	MaxSize          int
	HeartbeatTimeout time.Duration
	ScaleCooldown    time.Duration
	HealthDecayRate  float64
}
