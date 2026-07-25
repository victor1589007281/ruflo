package graph

// hooks_scope_test.go —— design/01 §4.5 总线模型的两条硬性质:
//   ① 决策判据单一 (Blocks): deny 与 block 同等处理, 不留 fail-open 缺口;
//   ② turn|tool 作用域是**纯观测**: 它们的决策不得影响节点执行。

import (
	"context"
	"testing"
)

func TestHookDecisionBlocks判据(t *testing.T) {
	cases := []struct {
		action string
		want   bool
		why    string
	}{
		{"", false, "空 = 放行"},
		{HookDeny, true, "deny 拦下"},
		{HookBlock, true, "block 是外部 hook 的拦下语义, 必须与 deny 同等 —— 只认 deny 就是 fail-open"},
		{"allow", false, "未知动作按放行处理 (与改造前一致, 不静默收紧)"},
	}
	for _, c := range cases {
		if got := (HookDecision{Action: c.action}).Blocks(); got != c.want {
			t.Errorf("Action=%q Blocks()=%v want %v (%s)", c.action, got, c.want, c.why)
		}
	}
}

// scopedDenyBus 只对指定作用域返回 deny。
type scopedDenyBus struct {
	scope  HookScope
	action string
	seen   []HookEvent
}

func (b *scopedDenyBus) Emit(_ context.Context, ev HookEvent) HookDecision {
	b.seen = append(b.seen, ev)
	if ev.Scope == b.scope {
		return HookDecision{Action: b.action, Reason: "test"}
	}
	return HookDecision{}
}

// TestNodePre的block被当作deny —— fail-closed 不许退化。
// 改造前引擎只认 `deny`; 一个明确写了 block 的治理 hook 会被静默放行。
func TestNodePre的block被当作deny(t *testing.T) {
	for _, action := range []string{HookDeny, HookBlock} {
		bus := &scopedDenyBus{scope: ScopeNode, action: action}
		eng := &Engine{Runner: scopeStubRunner(func() NodeResult {
			t.Fatalf("action=%s: 节点不该被执行", action)
			return NodeResult{}
		}), Hooks: bus, Journal: NewMemoryJournal()}

		spec := GraphSpec{Name: "g", Nodes: []NodeSpec{{ID: "a", Kind: NodeKindAgent}}}
		rr, err := eng.Run(context.Background(), spec, RunOpts{RunID: "r1"})
		if err != nil {
			t.Fatalf("action=%s: Run: %v", action, err)
		}
		if rr.Nodes["a"].Status != NodeStatusSkipped {
			t.Errorf("action=%s: 节点应被 hook 拦下置 skipped, got %s", action, rr.Nodes["a"].Status)
		}
	}
}

// TestTurn与Tool作用域不影响执行 —— 这两个作用域是 internal_hook 的观测出口,
// 即便总线对它们返回 deny 也**不得**变成新的阻塞路径 (灰度期纪律)。
// 引擎侧的保证是"根本不在这两个作用域上取决策"; 桥接侧还有一道丢弃 (双保险)。
func TestTurn与Tool作用域不影响执行(t *testing.T) {
	for _, scope := range []HookScope{ScopeTurn, ScopeTool} {
		ran := false
		bus := &scopedDenyBus{scope: scope, action: HookDeny}
		eng := &Engine{Runner: scopeStubRunner(func() NodeResult {
			ran = true
			return NodeResult{Status: NodeStatusCompleted, Output: "ok"}
		}), Hooks: bus, Journal: NewMemoryJournal()}

		spec := GraphSpec{Name: "g", Nodes: []NodeSpec{{ID: "a", Kind: NodeKindAgent}}}
		rr, err := eng.Run(context.Background(), spec, RunOpts{RunID: "r1"})
		if err != nil {
			t.Fatalf("scope=%s: Run: %v", scope, err)
		}
		if !ran || rr.Nodes["a"].Status != NodeStatusCompleted {
			t.Errorf("scope=%s 的 deny 不应阻断节点执行, status=%s ran=%v",
				scope, rr.Nodes["a"].Status, ran)
		}
	}
}

// scopeStubRunner 把无参函数适配成 NodeRunner (本文件只关心"跑没跑")。
type scopeStubRunner func() NodeResult

func (f scopeStubRunner) RunNode(context.Context, NodeSpec, NodeInput) NodeResult { return f() }
