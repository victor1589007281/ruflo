package graph

// hooks.go —— Hook 总线最小版 (design/01 §4.5 三套合一的 v1 骨架)。
//
// v1 只落地 graph|node 两个作用域与 pre|post|failure 三个相位:
//   - node pre: 返回 deny 则该节点置 skipped 并记 journal node.skipped (reason 入 Data);
//   - node post / failure: 通知性发射, 决策被忽略;
//   - graph pre/post: 在 Run 首尾发射, v1 不解释其决策 (只做观测挂点)。
//
// turn|tool|session 作用域、mutate/approve 决策、HookBinding/on_error 显式
// fail-open/fail-closed 均属 design/01 §4.5 全量形态, 由后续里程碑接入。
// 总线实现必须并发安全: 引擎会从多个节点 goroutine 同时 Emit。

import "context"

// HookScope 作用域 (design/01 §4.5 双维度: 作用域 × 相位)。
type HookScope string

const (
	ScopeGraph HookScope = "graph"
	ScopeNode  HookScope = "node"
)

// HookDeny 是 v1 唯一被引擎解释的决策动作 (node pre 相位)。
const HookDeny = "deny"

// HookEvent 总线事件 (design/01 §4.5)。
type HookEvent struct {
	Scope   HookScope
	Phase   string // pre|post|failure
	RunID   string
	NodeID  string
	Payload map[string]any
}

// HookDecision 总线决策。Action 为空 = 放行。
type HookDecision struct {
	Action string // ""|"deny"
	Reason string
}

// HookBus Hook 总线接口。
type HookBus interface {
	Emit(context.Context, HookEvent) HookDecision
}

// NopBus 空总线: 全部放行。Engine.Hooks 为 nil 时的默认实现。
type NopBus struct{}

// Emit 恒放行。
func (NopBus) Emit(context.Context, HookEvent) HookDecision { return HookDecision{} }
