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
	Iteration   int    // loop 轮次, 0 起 (design/01 §4.4)
	Feedback    string // loop 回灌 (Feedback 模板中 {prev_output} 已被引擎替换)
}

// NodeResult 节点执行结果。
type NodeResult struct {
	Status string  // "completed"|"failed"|"skipped"
	Output string  // 节点产出 (进 journal, 供下游 PrevOutputs / 条件求值)
	Score  float64 // gate 节点评分, 无则 0
	Err    string  // 失败/跳过原因
}

// NodeRunner 节点执行器接口。实现方须遵守 ctx 取消/超时
// (引擎已注入 trace NodeID 与 TimeoutSec 派生的 deadline)。
type NodeRunner interface {
	RunNode(ctx context.Context, node NodeSpec, in NodeInput) NodeResult
}
