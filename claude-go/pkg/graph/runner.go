package graph

// runner.go —— 节点执行接口 (design/01 §4.9 AgentRuntime 的本地化前身)。
//
// 引擎不 import pkg/agent (防循环依赖): 节点如何变成一次 LLM agent 执行
// (QueryEngine 隔离实例、prompt 占位替换、工具画像) 全部由注入的 NodeRunner
// 决定。引擎只负责调度/循环/重试/journal/hook 生命周期。

import "context"

// 节点终态 (design/01 §4.3)。skipped 表示未执行 (OR-join 无满足入边 / hook deny)。
const (
	NodeStatusCompleted = "completed"
	NodeStatusFailed    = "failed"
	NodeStatusSkipped   = "skipped"
)

// NodeInput 一次节点执行的输入。
type NodeInput struct {
	Objective string            // 整图目标 (RunOpts.Objective 透传)
	Params    map[string]string // 图参数 (RunOpts.Params 透传, runner 只读)
	// PrevOutputs 按节点 ID 的上游产出, 仅含直接前驱中 completed 的
	// (与入边条件是否满足无关, 见 engine.go 语义5)。
	PrevOutputs map[string]string
	Iteration   int    // 节点级 loop 轮次, 0 起 (design/01 §4.4)
	Feedback    string // loop 回灌 (Feedback 模板中 {prev_output} 已被引擎替换)

	// GroupIteration loop-group 组级轮次 (0 起), 仅组内成员节点非零。
	// 与 Iteration 分开是必须的: 成员节点自己也可能声明节点级 Loop, 那会覆写
	// Iteration —— 两个轮次挤一个字段会让 runner 分不清"第几轮组循环"。
	GroupIteration int
	// Shard 本次执行是 map 节点的一个分片时非 nil (§4.2)。
	// 分片内容如何进 prompt 由 runner 决定 (引擎不做占位替换)。
	Shard *ShardInput
	// Shards reduce 节点聚合的上游分片结果 (按 map 节点 + 分片序), 非 reduce 节点为空。
	Shards []ShardResult
	// NodeRef 本次执行在 journal/hook 里的**限定 ID** (顶层 = node.ID; map 分片 =
	// <map>#<i>; loop-group 组内 = <组>#it<轮次>/<成员>)。
	// 为什么不直接把它塞进 node.ID: 宿主有一堆按"阶段名"生效的既有逻辑 (产出校验、
	// 角色推断), 换成限定 ID 会静默改变那些判定; 但宿主又需要限定 ID 才能把自己的
	// 记录与 journal 对齐。故分两个字段: node.ID 是语义名, NodeRef 是归因名。
	NodeRef string

	// nested 引擎内部调度标记: 本次执行属于嵌套层叶子 (map 分片 / loop-group 组内),
	// 需要先取 run 级并发票。不导出 —— 它是调度细节, 不属于 runner 契约。
	nested bool
}

// ShardInput map 分片的输入 (design/01 §4.2)。
type ShardInput struct {
	NodeID string // 分片节点 ID: <map节点>#<序号>
	MapID  string // 所属 map 节点 ID
	Index  int    // 分片序号, 0 起
	Total  int    // 本次扇出的分片总数
	Value  string // 分片内容 (切分后的集合元素原文)
}

// ShardResult 一个 map 分片的执行结果 (进 map 节点的 NodeResult.Shards,
// 再经 NodeInput.Shards 供 reduce 聚合)。
type ShardResult struct {
	NodeID string  `json:"node_id"`
	MapID  string  `json:"map_id"`
	Index  int     `json:"index"`
	Input  string  `json:"input,omitempty"`
	Status string  `json:"status"`
	Output string  `json:"output,omitempty"`
	Score  float64 `json:"score,omitempty"`
	Err    string  `json:"err,omitempty"`
}

// NodeResult 节点执行结果。
type NodeResult struct {
	Status string  // "completed"|"failed"|"skipped"
	Output string  // 节点产出 (进 journal, 供下游 PrevOutputs / 条件求值)
	Score  float64 // gate 节点评分, 无则 0
	Err    string  // 失败/跳过原因

	// Tokens 本次执行消耗的 token (runner 可选回报, 供 BudgetManager 记账,
	// design/01 §4.10)。**0 表示"未回报"而非"没花"** —— 两者必须可区分, 否则
	// "用量回报尚未实现"会被当成"这次免费", 预算闸形同虚设。
	Tokens int64

	// Shards map 节点各分片结果 (引擎填, runner 不必理); 经 journal 往返以支持 resume。
	Shards []ShardResult
	// Expansion runner 请求追加进运行图的子图 (design/01 §4.2 动态展开)。
	// **仅当该节点声明了 NodeSpec.Expand 时被引擎接受**, 且要过深度/条数/总量
	// 三道闸与"约束只收窄"检查; 被拒绝不影响本节点自身的终态, 只记 journal + 日志。
	Expansion *Expansion
}

// Expansion 一次动态展开的载荷 (design/01 §4.2)。
// 节点 ID 会被引擎命名空间化为 <父节点>/<子节点ID> (防重名 + 归因),
// Edges 的 From/To 用**未命名空间化**的子节点 ID 或父节点 ID 书写。
type Expansion struct {
	Nodes []NodeSpec `json:"nodes"`
	Edges []EdgeSpec `json:"edges,omitempty"`
}

// NodeRunner 节点执行器接口。实现方须遵守 ctx 取消/超时
// (引擎已注入 trace NodeID 与 TimeoutSec 派生的 deadline)。
type NodeRunner interface {
	RunNode(ctx context.Context, node NodeSpec, in NodeInput) NodeResult
}
