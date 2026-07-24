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
}

// NodeKind 节点形态 (design/01 §4.1)。v1 只支持 agent|gate;
// map/subgraph/human 预留常量但 Validate 拒绝。
type NodeKind string

const (
	NodeKindAgent NodeKind = "agent" // LLM agent 节点
	NodeKindGate  NodeKind = "gate"  // 门禁节点 (确定性代码或 LLM 评分, 输出 score)

	// 以下 Kind 为 design/01 §4.1 全表预留, v1 的 Validate 明确拒绝。
	NodeKindMap      NodeKind = "map"
	NodeKindSubgraph NodeKind = "subgraph"
	NodeKindHuman    NodeKind = "human"
)

// NodeSpec 节点描述 (design/01 §4.1)。所有 Kind 共享同一 Agent 载体。
type NodeSpec struct {
	ID         string       `json:"id"`
	Kind       NodeKind     `json:"kind"`
	Agent      AgentSpec    `json:"agent"`
	Loop       *LoopPolicy  `json:"loop,omitempty"`        // 节点级循环 (§4.4)
	Retry      *RetryPolicy `json:"retry,omitempty"`       // 唯一一层重试 (§4.3 重试单层化)
	TimeoutSec int          `json:"timeout_sec,omitempty"` // 0=不限 (由 runner 自己的角色级超时兜底)
}

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
