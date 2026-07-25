// Package graph 实现 design/01《统一 Agent 编排引擎》§4 的图引擎内核 v1 (M1 范围):
// 纯数据 GraphSpec (§4.1-4.2) + 事件溯源 Journal (§4.3) + 节点级 Loop (§4.4)
// + Hook 总线最小版 (§4.5) + ready-set 并行调度器 (§4.3)。
//
// 设计约束:
//   - 仅依赖标准库 + pkg/trace; 不 import pkg/agent (防循环依赖),
//     节点执行经注入的 NodeRunner 接口 (§4.9 的本地化前身)。
//   - 图是纯数据, 可 JSON 序列化; prompt 占位符 ({objective}/{prev_result}/
//     {node.<id>}) 的替换由 runner 负责, 引擎只透传。
package graph

// GraphSpec 图的纯数据描述 (design/01 §4.1)。可 JSON 序列化, 引擎对其只读。
type GraphSpec struct {
	Name     string        `json:"name"`
	Version  string        `json:"version,omitempty"`
	Nodes    []NodeSpec    `json:"nodes"`
	Edges    []EdgeSpec    `json:"edges"`
	Policies GraphPolicies `json:"policies,omitempty"`
	Meta     GraphMeta     `json:"meta,omitempty"`
}

// GraphMeta 声明式门禁元数据 (design/01 §4.1)。
// 继承 workflow.go 的教训: 门禁凭元数据不凭工作流名白名单。
type GraphMeta struct {
	ProducesCode bool   `json:"produces_code,omitempty"` // 是否产出代码 (取代按工作流名白名单)
	QualityGate  string `json:"quality_gate,omitempty"`  // "content"|"none"
}

// GraphPolicies 图级默认策略 (design/01 §4.1)。
type GraphPolicies struct {
	MaxParallel  int          `json:"max_parallel,omitempty"`  // 并发上限, 0=默认4
	DefaultRetry *RetryPolicy `json:"default_retry,omitempty"` // 节点未声明 Retry 时的图级默认
	// MaxTotalNodes 一次运行的**运行图**节点总数上限 (含动态展开产物与 map 分片)。
	// 0 → DefaultMaxTotalNodes。这是"有界展开"的最后一道闸 (design/01 §4.2):
	// 单个 ExpandSpec 的深度/条数上限只能约束一次展开, 挡不住"每层都只加 3 个
	// 但加了 20 层"这种由 LLM 产出驱动的累积膨胀。
	MaxTotalNodes int `json:"max_total_nodes,omitempty"`
}

// NodeKind 节点形态 (design/01 §4.1 全表 8 种)。
//
// 已实现: agent | gate | map | reduce | loop-group。
// 未实现: router | subgraph | human —— Validate 对它们**显式报错**而不是静默接受
// (静默接受等于把一个什么都不做的节点混进图里, 表现为"整条分支莫名 skipped")。
type NodeKind string

const (
	NodeKindAgent NodeKind = "agent" // LLM agent 节点
	NodeKindGate  NodeKind = "gate"  // 门禁节点 (确定性代码或 LLM 评分, 输出 score)

	// —— 扇出/汇聚 (§4.2 map/reduce) ——
	NodeKindMap    NodeKind = "map"    // 按集合切分并发派生 N 个同构分片
	NodeKindReduce NodeKind = "reduce" // 等其 map 组全部终态后聚合
	// —— 组级循环 (§4.4) ——
	NodeKindLoopGroup NodeKind = "loop-group" // 一组节点 (内嵌子图) 整体循环

	// 以下 Kind 为 design/01 §4.1 全表预留, 尚未实现, Validate 明确拒绝。
	NodeKindRouter   NodeKind = "router"
	NodeKindSubgraph NodeKind = "subgraph"
	NodeKindHuman    NodeKind = "human"
)

// NodeSpec 节点描述 (design/01 §4.1)。所有 Kind 共享同一 Agent 载体。
type NodeSpec struct {
	ID    string       `json:"id"`
	Kind  NodeKind     `json:"kind"`
	Agent AgentSpec    `json:"agent"`
	Loop  *LoopPolicy  `json:"loop,omitempty"`  // 节点级循环 (§4.4)
	Retry *RetryPolicy `json:"retry,omitempty"` // 唯一一层重试 (§4.3 重试单层化)
	// TimeoutSec 节点**总预算**(秒), 0=不限 (由 runner 自己的角色级超时兜底)。
	// 注意作用域: 引擎在 retry 环与 loop 环**之外**施加这个 deadline (engine.go
	// execNode), 即它包住"全部重试 + 全部循环轮次"的墙钟总时长, 不是单次尝试的
	// 上限——填单次尝试的超时会让第二次重试必然撞墙。
	TimeoutSec int `json:"timeout_sec,omitempty"`

	// Map 仅 Kind=="map" 有效: 分片来源与切分策略 (§4.2)。
	Map *MapPolicy `json:"map,omitempty"`
	// Reduce 仅 Kind=="reduce" 有效: 聚合来源与策略 (§4.2)。
	Reduce *ReducePolicy `json:"reduce,omitempty"`
	// Group 仅 Kind=="loop-group" 有效: 组内子图 + 组级循环 (§4.4)。
	Group *GroupPolicy `json:"group,omitempty"`
	// Expand 声明本节点**可以**动态展开子图 (§4.2)。
	// 未声明时 runner 返回的 Expansion 被引擎忽略 —— 展开能力必须由图显式授予,
	// 否则任何 runner 都能凭产出往运行图里塞节点。
	Expand *ExpandSpec `json:"expand,omitempty"`
}

// —— map/reduce (design/01 §4.2 "对集合展开 N 个并行子节点 / 聚合") ——

// 分片切分策略 (MapPolicy.Split)。
const (
	SplitLines     = "lines"      // 按行 (去空行), 默认
	SplitParagraph = "paragraphs" // 按空行分段
	SplitJSONArray = "json_array" // 解析为 JSON 数组 (元素为字符串 / 对象则原样序列化)
	SplitWhole     = "whole"      // 整体作为唯一分片 (退化为 1 分片, 便于灰度对比)
)

// 分片来源 (MapPolicy.Source)。前缀式来源见 SourcePrevPrefix / SourceParamPrefix。
const (
	SourcePrev        = "prev"      // 全部 completed 上游中最长的那份产出 (默认)
	SourceObjective   = "objective" // 整图目标
	SourcePrevPrefix  = "prev:"     // prev:<节点ID> 指定上游
	SourceParamPrefix = "param:"    // param:<键> 取 RunOpts.Params
)

// MapPolicy map 节点的扇出策略 (design/01 §4.2)。
//
// 引擎只做**确定性**切分: 集合从哪来 (Source)、怎么切 (Split)、最多切几片
// (MaxShards)。分片内容如何进 prompt 由 runner 负责 (引擎经 NodeInput.Shard 透传),
// 与"占位符替换归 runner"的既有分工一致。
type MapPolicy struct {
	Source string `json:"source,omitempty"` // 见 Source* 常量, 空=prev
	Split  string `json:"split,omitempty"`  // 见 Split* 常量, 空=lines
	// MaxShards 分片数硬上限, 必填 >0, Validate 强制。
	// 为什么必填: 集合来自上游 LLM 产出, "一行一片"遇到 800 行的产出就是 800 次
	// LLM 调用。超出上限的部分被丢弃并记 journal (map.expanded 带 truncated)。
	MaxShards int `json:"max_shards"`
	// MinShards 分片数下限 (0=不限)。切出的分片少于此数即整节点 failed,
	// 用于"必须至少扇出 N 路"的评审团/集成抽取场景。
	MinShards int `json:"min_shards,omitempty"`
}

// 聚合策略 (ReducePolicy.Strategy)。
const (
	ReduceRunner  = "runner"  // 默认: 分片经 NodeInput.Shards 交给 runner 聚合 (LLM)
	ReduceConcat  = "concat"  // 确定性: 按分片序拼接 (零 LLM)
	ReduceLongest = "longest" // 确定性: 取最长分片 (零 LLM)
)

// ReducePolicy reduce 节点的聚合策略 (design/01 §4.2)。
type ReducePolicy struct {
	// From 指定聚合哪些 map 节点 (空=全部 map 类直接前驱)。
	From []string `json:"from,omitempty"`
	// Strategy 见 Reduce* 常量, 空=runner。
	// concat/longest 是**确定性**策略: 引擎直接算出结果, 零 LLM 调用, 但仍走完整
	// 节点生命周期 (hook/journal/预算), 与 design/01 §4.1 对 Deterministic 的要求一致。
	Strategy string `json:"strategy,omitempty"`
	// Separator concat 分隔符, 空=DefaultReduceSeparator。
	Separator string `json:"separator,omitempty"`
	// RequireAll true=任一分片非 completed 即 reduce 失败;
	// false (默认)=尽力聚合已成功的分片 (扇出场景里单片失败不该毁掉整轮)。
	RequireAll bool `json:"require_all,omitempty"`
}

// GroupPolicy loop-group 组级循环 (design/01 §4.4)。
//
// 与节点级 Loop 的边界 (谁管谁), 这是本形态最容易搞混的地方:
//   - 节点级 Loop: **单个节点**自我迭代, Until 对该节点自己的结果求值,
//     journal 记 loop.iteration。典型 = 同一 agent 反复自评改写。
//   - loop-group: **一组节点**(组内保持 DAG 语义) 整体反复, Until 对组产出节点
//     (ResultFrom) 的结果求值, journal 记 loop.group.iteration。典型 = 对抗模式
//     "生成→评审"两节点整轮重来。
//
// 两者**不允许并存于同一节点** (Validate 强制): 组节点自己再套一层 Loop 会让轮次
// 相乘 (MaxIterations×MaxIterations), 预算无法推理。组内也不允许再嵌套 loop-group,
// 同理。
type GroupPolicy struct {
	// Nodes/Edges 组内子图。内嵌 (而非引用外部节点 ID) 是刻意的: 组成员若是顶层
	// 节点, 顶层 Validate 的"入口可达 / 无环"检查会把它们当孤岛报错, 且调度器会
	// 把它们当普通节点各跑一次。内嵌后组内子图按同一套规则**递归校验**。
	Nodes []NodeSpec `json:"nodes"`
	Edges []EdgeSpec `json:"edges,omitempty"`
	// Loop 组级循环策略 (MaxIterations 必填 >0; Until 对 ResultFrom 结果求值;
	// Feedback 的 {prev_output} 替换为上一轮 ResultFrom 的产出, 下发给**全部**成员)。
	Loop LoopPolicy `json:"loop"`
	// ResultFrom 组产出取哪个成员节点。空 = 组内唯一出度 0 节点;
	// 有多个出度 0 节点时 Validate 强制显式声明 (否则"组产出是谁"取决于声明顺序,
	// 是个隐蔽的不确定性)。
	ResultFrom string `json:"result_from,omitempty"`
}

// ExpandSpec 动态展开授权与边界 (design/01 §4.2)。
//
// 有界是硬要求: 展开的内容来自 LLM 产出 (GoalTree HTN 分解 / WBS / swarm 分解),
// 无界展开等于把运行图的规模交给模型即兴决定。四道闸:
//  1. MaxDepth   展开深度 (父→子→孙…), 默认 DefaultExpandDepth=1;
//  2. MaxNodes   单次展开可追加的节点数 (必填 >0);
//  3. MaxTotalNodes (图级) 运行图节点总数;
//  4. 单调收窄  展开产物只继承/收窄父节点约束, 不得放宽 (见 expand.go)。
type ExpandSpec struct {
	MaxDepth int `json:"max_depth,omitempty"` // 0 → DefaultExpandDepth
	MaxNodes int `json:"max_nodes"`           // 必填 >0
}

// 展开/扇出的内置上限与默认值。
const (
	DefaultExpandDepth      = 1                // ExpandSpec.MaxDepth 缺省: 只允许展开一层
	DefaultMaxTotalNodes    = 200              // GraphPolicies.MaxTotalNodes 缺省
	DefaultReduceSeparator  = "\n\n---\n\n"    // ReducePolicy.Separator 缺省
	ShardIDSep              = "#"              // 分片 NodeID: <map节点>#<序号>
	GroupIterIDInfix        = "#it"            // 组内 NodeID: <组节点>#it<轮次>/<成员>
	GroupMemberIDSep        = "/"              //
	expandedNodeIDSeparator = GroupMemberIDSep // 展开产物 NodeID: <父节点>/<子节点>
)

// AgentSpec 节点的 Agent 载体 (design/01 §4.1)。
type AgentSpec struct {
	Role        string `json:"role"`
	Prompt      string `json:"prompt,omitempty"`       // 模板, 支持 {objective}/{prev_result}/{node.<id>} 占位 (替换由 runner 负责, 引擎只透传)
	ToolProfile string `json:"tool_profile,omitempty"` // 显式声明工具画像, 替代角色名子串匹配
	MaxTurns    int    `json:"max_turns,omitempty"`
	// Deterministic gate 节点纯代码策略 (零 LLM, design/01 §4.1)。
	// 引擎不区分, runner 自解释——但仍走同一节点生命周期 (hook/journal/trace)。
	Deterministic bool `json:"deterministic,omitempty"`
}

// EdgeSpec 有向边 (design/01 §4.2)。Condition 为空表示无条件恒真;
// 语法见 condition.go (v1 极简确定性条件集, 非 CEL)。
type EdgeSpec struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// LoopPolicy 节点级循环 (design/01 §4.4)。
type LoopPolicy struct {
	// MaxIterations 循环硬上限, 必填 >0, Validate 强制 (无界循环违法, design/01 §4.4)。
	MaxIterations int `json:"max_iterations"`
	// Until 对本节点结果求值的退出条件 (语法见 condition.go)。
	// 空 = 无退出条件, 循环直到 MaxIterations 耗尽。
	Until string `json:"until,omitempty"`
	// Feedback 每轮回灌上一轮产出的模板, 含 {prev_output} 占位
	// (引擎替换为上一轮 Output 后经 NodeInput.Feedback 传给 runner)。
	Feedback string `json:"feedback,omitempty"`
}

// RetryPolicy 节点重试策略 (design/01 §4.3 重试单层化: 重试只存在于此层)。
type RetryPolicy struct {
	MaxRetries int `json:"max_retries"`
	BackoffSec int `json:"backoff_sec,omitempty"` // 指数退避基数 (秒), BackoffSec*2^attempt, 默认2
}
