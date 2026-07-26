package graph

// hooks.go —— Hook 总线 (design/01 §4.5 "三套合一" 的统一事件模型)。
//
// ---------------------------------------------------------------------------
// 总线上现在有哪几类事件, 谁是产生方
// ---------------------------------------------------------------------------
//
//	graph|pre,post        本引擎 (Run 首尾)
//	node |pre,post,failure 本引擎 (每个节点)
//	turn |...             pkg/engine 的 internal_hook.HookChain, 经 ctx 携带的
//	                      Observer 桥接 (见 pkg/agent/graph_internal_bridge.go);
//	                      以及 pkg/hooks 的外部 hook 每轮事件 (见下)
//	tool |...             同上 (PreToolUse / PostToolUse 两个 phase)
//	session|...           pkg/hooks 的外部 shell/HTTP/gRPC/OPA hook, 经 ctx 携带的
//	                      hooks.Observer 桥接 (见 pkg/agent/graph_external_bridge.go)
//
// design/01 §4.5 的定位是: `internal_hook` **保留在引擎内**(性能敏感、每 turn
// 触发), 但它的事件要注册进同一条总线, 使**事件对图层可见**。注意是"可见"不是
// "受图层管辖" —— turn|tool 事件在总线上是**纯观测**, 引擎不解释它们的决策。
// 理由见下面 "为什么 turn|tool 不给决策权"。
//
// ---------------------------------------------------------------------------
// 决策语义: 哪些 Action 真的生效
// ---------------------------------------------------------------------------
//
//	node |pre  →  Blocks() 为真 (deny / block) 时该节点置 skipped 并记 journal
//	             node.skipped (reason 入 Data)。这是**唯一**被引擎解释的决策点。
//	其余相位   →  决策被忽略, 纯通知。
//
// **这里刻意没有 `approve` / `mutate` 两个 Action 常量, 也没有 `Mutation` 载荷**,
// 尽管 design/01 §4.5 的全量形态列了它们。理由是安全属性而非工作量:
// 定义一个引擎不解释的决策动作, 等于给调用方一个"我拦住了/我改写了"的假象 ——
// 声明它的人以为生效, 实际静默放行。这正是本仓修过的一类缺陷 (refine 只失效
// checkpoints.json 而 journal 原样保留 → 反馈静默消失) 的同款形态, 方向是 fail-open。
// 等 mutate/approve 真有解释方 (HookBinding + on_error 那一步) 时再加常量,
// 那时它们一落地就是生效的。
//
// 总线实现必须并发安全: 引擎会从多个节点 goroutine 同时 Emit, 而 turn|tool
// 事件还会从各节点内部的 agent turn 循环并发打进来。

import "context"

// HookScope 作用域 (design/01 §4.5 双维度: 作用域 × 相位)。
type HookScope string

const (
	ScopeGraph HookScope = "graph"
	ScopeNode  HookScope = "node"
	// ScopeTurn 引擎内 turn 级事件 (compact / 记忆注入 / 错误分类 / turn 收尾),
	// 以及外部 hook 的每轮/每请求事件 (PreTurn/PreRequest/Stop/OnError...)。
	ScopeTurn HookScope = "turn"
	// ScopeTool 引擎内工具级事件 (ToolGate / LoopDetector / JSONRepair 等),
	// 以及外部 hook 的 PreToolUse/PostToolUse/PostToolUseFailure。
	ScopeTool HookScope = "tool"
	// ScopeSession 会话/子代理生命周期事件 (SessionStart/SessionEnd/
	// SubagentStart/SubagentStop)。
	//
	// 这个常量此前刻意不存在, 理由是"全仓没有产生方, 加一个没有产生方的常量只会让
	// 读者以为总线上有这类事件"。现在**有了产生方**: `pkg/hooks` 的 ExternalHook
	// 适配器 (design/01 §4.5 归宿表第 1 行, 见 pkg/agent/graph_external_bridge.go),
	// 于是按当初写下的条件"有了产生方再加"补上。
	ScopeSession HookScope = "session"
)

// Hook 决策动作。空串 = 放行。
const (
	// HookDeny 拒绝执行 (node pre 相位被引擎解释)。
	HookDeny = "deny"
	// HookBlock 与 deny 同义, 供 pkg/hooks 的外部 hook 语义直通 ——
	// 那套用 block 表达"拦下这次工具调用"。**必须与 deny 同等处理**:
	// 只认 deny 会让一个明确写了 block 的治理 hook 静默失效 (fail-open)。
	HookBlock = "block"
)

// HookEvent 总线事件 (design/01 §4.5)。
type HookEvent struct {
	Scope  HookScope
	Phase  string // pre|post|failure, 或 turn/tool 作用域下 internal_hook 的 phase 名
	RunID  string
	NodeID string
	// TurnID trace 链的第三段 (design/01 §4.5)。turn|tool 事件靠它与具体某一轮
	// 对话对齐 —— 只有 RunID/NodeID 时, 一个节点内几十轮 turn 的事件会糊成一团。
	TurnID  string
	Payload map[string]any
}

// HookDecision 总线决策。Action 为空 = 放行。
type HookDecision struct {
	Action string // ""|"deny"|"block"
	Reason string
}

// Blocks 该决策是否阻止执行。
//
// 单独提成方法而不是在调用点写 `d.Action == HookDeny`: 判据散在调用点时,
// 新增一个等价动作 (block) 必须记得改全部调用点, 漏一处就是一个 fail-open 缺口。
func (d HookDecision) Blocks() bool {
	return d.Action == HookDeny || d.Action == HookBlock
}

// HookBus Hook 总线接口。
type HookBus interface {
	Emit(context.Context, HookEvent) HookDecision
}

// NopBus 空总线: 全部放行。Engine.Hooks 为 nil 时的默认实现。
type NopBus struct{}

// Emit 恒放行。
func (NopBus) Emit(context.Context, HookEvent) HookDecision { return HookDecision{} }
