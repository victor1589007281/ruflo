// 共享类型定义（swarm 包）：编排器、拓扑、总线、池与共识相关的配置与快照结构体集中于此，供各子模块引用。
package swarm

import (
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// CoordinatorConfig UnifiedSwarmCoordinator 的静态参数。
type CoordinatorConfig struct {
	Topology        api.TopologyType       // 拓扑枚举：Mesh/Hierarchical 等
	ConsensusAlgo   api.ConsensusAlgorithm // 共识算法选择
	AgentPoolMin    int                    // 池预热最小 Agent 数
	AgentPoolMax    int                    // 池硬上限
	HeartbeatMS     int                    // 协调器健康巡检间隔（毫秒）
	MetricsInterval time.Duration          // 指标刷新周期
}

// CoordinatorState 对外暴露的协调器运行时摘要（内部 mu 字段在拷贝快照时常被忽略）。
type CoordinatorState struct {
	mu           sync.RWMutex     // 保护并发读写（部分路径未使用）
	Status       api.SwarmStatus  // 蜂群健康/排空等枚举
	Topology     api.TopologyType // 当前拓扑类型
	AgentCount   int              // 已注册 Agent 数
	PendingTasks int              // 任务表长度近似待办
	LastElection time.Time        // 上次选主时间（可选）
	StartedAt    time.Time        // 协调器启动时刻
}

// CoordinatorMetrics 累计型 KPI，供监控拉取。
type CoordinatorMetrics struct {
	mu                 sync.RWMutex
	TasksSubmitted     int64 // 提交任务次数
	TasksAssigned      int64 // 指派次数
	TasksCompleted     int64 // 完成任务次数（由业务层递增）
	ConsensusProposals int64 // 共识提案次数
	HealthRecoveries   int64 // 健康恢复/自愈计数
	MessagesProcessed  int64 // 总线已处理消息数
	UpdatedAt          time.Time
}

// DomainConfig 描述 15-Agent 划分中某一域的编号区间与能力标签。
type DomainConfig struct {
	Name         api.AgentDomain // 域枚举：Queen/Security/Core 等
	AgentNumbers []int           // 文档化用的逻辑编号列表
	Priority     int             // 域优先级（越小越核心）
	Capabilities []string        // 该域宣称可处理的任务/技能标签
}

// TaskAssignment 记录任务到 Agent/域的绑定及打分。
type TaskAssignment struct {
	TaskID   string          // 任务 id
	AgentID  string          // 指派目标
	Domain   api.AgentDomain // 任务所属域快照
	Score    float64         // 指派时启发式得分
	Assigned time.Time       // 指派时间
}

// ParallelExecutionResult ExecuteParallel 单任务的提交结果摘要。
type ParallelExecutionResult struct {
	TaskID    string        // 任务 id
	AgentID   string        // SubmitTask 成功后写入 task.AgentID
	Success   bool          // err==nil
	Err       error         // 失败原因
	Duration  time.Duration // SubmitTask 调用耗时
	StartedAt time.Time     // 开始时间
}

// DomainStatus 按域聚合的资源利用率视图（扩展用）。
type DomainStatus struct {
	Domain      api.AgentDomain // 域 id
	AgentCount  int             // Agent 数
	ActiveTasks int             // 活跃任务估计
	AvgLoad     float64         // 平均负载
	AvgHealth   float64         // 平均健康
}

// TopologyConfig TopologyManager 行为参数。
type TopologyConfig struct {
	Type              api.TopologyType // 拓扑种类
	MaxMeshDegree     int              // Mesh 每点最大度数
	HybridRandomPeers int              // 混合拓扑额外随机边数
	RolePriorityOrder []string         // ElectLeader 角色优先级表
}

// TopologyNode 协调图上的顶点：Agent、角色字符串与无向邻接集合。
type TopologyNode struct {
	AgentID   string              // 结点 id
	Role      string              // queen/coordinator/coder 等
	Neighbors map[string]struct{} // 邻接点 id 集合
	JoinedAt  time.Time           // 入图时间
}

// TopologyState 图规模与当前选主结果快照。
type TopologyState struct {
	Type      api.TopologyType // 拓扑类型
	NodeCount int              // 顶点数
	EdgeCount int              // 无向边数
	Leader    string           // ElectLeader 结果
}

// ConsensusConfig 构造底层 consensus.Engine 的参数镜像（详见 pkg/swarm/consensus）。
type ConsensusConfig struct {
	Algorithm    api.ConsensusAlgorithm // Raft/Gossip 等
	NodeID       string                 // 本节点标识
	Peers        []string               // 对端列表
	ByzantineF   int                    // 拜占庭容错参数 f
	GossipFanout int                    // 流言扇出
	GossipTTL    int                    // 流言跳数上限
}

// ConsensusProposal 飞行中提案的可观测视图。
type ConsensusProposal struct {
	ID        string // 提案 id
	Value     []byte // 载荷
	Proposer  string // 提议者
	Term      uint64 // 逻辑任期
	View      uint64 // 视图号
	CreatedAt time.Time
}

// ConsensusVote 单次投票记录。
type ConsensusVote struct {
	ProposalID string // 关联提案
	VoterID    string // 投票者
	Approve    bool   // 是否赞成
	Term       uint64
	View       uint64
	Timestamp  time.Time
}

// ConsensusResult AwaitConsensus 返回的提交结果。
type ConsensusResult struct {
	ProposalID string // 提案 id
	Committed  bool   // 是否达成提交
	Value      []byte // 共识值
	Term       uint64
	Err        error     // 底层错误
	FinishedAt time.Time // 结束时间
}

// MessageBusConfig 每 Agent 队列与调度参数。
type MessageBusConfig struct {
	QueueCapacityPerAgent int           // 单队列容量
	ProcessBatchMax       int           // 每轮每 Agent 最多处理条数
	ProcessInterval       time.Duration // drain 节拍
	SlidingWindowSecs     int           // 吞吐统计窗口秒数
}

// MessageBusStats 总线统计快照（拷贝返回，无需持锁读取已拷贝值）。
type MessageBusStats struct {
	MessagesSent      int64     // 入队发送计数
	MessagesDropped   int64     // 丢弃（满/TTL）
	MessagesACKed     int64     // ACK 次数
	MessagesProcessed int64     // 成功投递次数
	MessagesPerSec    float64   // 窗口内估算 QPS
	LastWindowStart   time.Time // 当前窗口起点
	WindowCount       int64     // 窗口内处理条数
	UpdatedAt         time.Time
}

// PooledAgent AgentPool 槽位的简化视图（与 api.Agent 注册表概念分离）。
type PooledAgent struct {
	ID            string          // 池内 id
	Type          api.AgentType   // 类型
	Domain        api.AgentDomain // 域
	State         api.AgentState  // 状态机
	Health        float64         // 健康度 0~1
	Load          float64         // 负载 0~1
	LastHeartbeat time.Time
	CreatedAt     time.Time
	Metadata      map[string]string // 扩展元数据
}

// AgentPoolConfig 池容量、心跳与扩缩容冷却。
type AgentPoolConfig struct {
	MinSize          int           // 最小保持实例数
	MaxSize          int           // 最大实例数
	HeartbeatTimeout time.Duration // 判定失联阈值
	ScaleCooldown    time.Duration // 两次扩缩容最小间隔
	HealthDecayRate  float64       // 每 tick 健康衰减量
}
