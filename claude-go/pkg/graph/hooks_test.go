package graph

// hook 载荷回归 (design/01 §4.5)。
//
// 为什么要为"载荷内容"写测试: hook 是**唯一**能同步拿到"某节点刚刚结束"的时机,
// 宿主 (pkg/agent 的 teamGraphHooks) 要靠这一次事件落业务侧阶段记录 + 指标
// (产出/耗时/重试次数)。若载荷缺字段, 宿主只能回头解 journal —— 既慢又拿不到与
// 本次运行一一对应的耗时。所以载荷字段属于对外契约, 必须锁死。

import (
	"context"
	"sync"
	"testing"
)

// recordBus 记录全部事件 (含载荷) 的总线。
type recordBus struct {
	mu  sync.Mutex
	evs []HookEvent
}

func (b *recordBus) Emit(_ context.Context, ev HookEvent) HookDecision {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 载荷 map 由引擎每次新建, 直接留引用即可。
	b.evs = append(b.evs, ev)
	return HookDecision{}
}

func (b *recordBus) find(scope HookScope, phase, nodeID string) (HookEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range b.evs {
		if ev.Scope == scope && ev.Phase == phase && ev.NodeID == nodeID {
			return ev, true
		}
	}
	return HookEvent{}, false
}

// TestHookPayloadNodeLifecycle node pre/post 载荷携带宿主桥接所需的全部字段。
func TestHookPayloadNodeLifecycle(t *testing.T) {
	st := newStub()
	st.fn["a"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "产出正文", Score: 88}
	}
	bus := &recordBus{}
	e := fastEngine(st, NewMemoryJournal())
	e.Hooks = bus
	spec := GraphSpec{Name: "payload", Nodes: []NodeSpec{
		{ID: "a", Kind: NodeKindGate, Agent: AgentSpec{Role: "critic"}},
	}}
	if _, err := e.Run(context.Background(), spec, RunOpts{RunID: "run-payload"}); err != nil {
		t.Fatalf("Run 出错: %v", err)
	}

	pre, ok := bus.find(ScopeNode, "pre", "a")
	if !ok {
		t.Fatal("缺 node pre 事件")
	}
	if pre.Payload["kind"] != string(NodeKindGate) || pre.Payload["role"] != "critic" {
		t.Errorf("pre 载荷应带 kind/role 供宿主建占位记录, got %v", pre.Payload)
	}

	post, ok := bus.find(ScopeNode, "post", "a")
	if !ok {
		t.Fatal("缺 node post 事件")
	}
	if post.Payload["status"] != NodeStatusCompleted {
		t.Errorf("post.status = %v, 期望 completed", post.Payload["status"])
	}
	if post.Payload["output"] != "产出正文" {
		t.Errorf("post.output = %v, 期望完整产出 (宿主要写进业务侧阶段记录)", post.Payload["output"])
	}
	if post.Payload["output_len"] != len("产出正文") {
		t.Errorf("post.output_len = %v, 期望 %d", post.Payload["output_len"], len("产出正文"))
	}
	if post.Payload["score"] != 88.0 {
		t.Errorf("post.score = %v, 期望 88", post.Payload["score"])
	}
	if post.Payload["attempts"] != 1 || post.Payload["iterations"] != 1 {
		t.Errorf("单次成功应 attempts=1 iterations=1, got attempts=%v iterations=%v",
			post.Payload["attempts"], post.Payload["iterations"])
	}
	if _, ok := post.Payload["duration_ms"].(int64); !ok {
		t.Errorf("post 载荷缺 duration_ms (int64), got %T", post.Payload["duration_ms"])
	}
}

// TestHookPayloadRetryAndLoopCounts 重试与循环轮数如实进载荷 (宿主据此记重试指标)。
func TestHookPayloadRetryAndLoopCounts(t *testing.T) {
	st := newStub()
	st.fn["fail"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusFailed, Err: "桩故障"}
	}
	bus := &recordBus{}
	e := fastEngine(st, NewMemoryJournal())
	e.Hooks = bus
	spec := GraphSpec{Name: "counts", Nodes: []NodeSpec{
		// 恒失败 + 2 次重试, 每次尝试内跑 3 轮 loop ⇒ attempts=3, iterations=9。
		{ID: "fail", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"},
			Retry: &RetryPolicy{MaxRetries: 2, BackoffSec: 1},
			Loop:  &LoopPolicy{MaxIterations: 3}},
	}}
	if _, err := e.Run(context.Background(), spec, RunOpts{RunID: "run-counts"}); err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	ev, ok := bus.find(ScopeNode, "failure", "fail")
	if !ok {
		t.Fatal("缺 node failure 事件")
	}
	if ev.Payload["attempts"] != 3 {
		t.Errorf("attempts = %v, 期望 3 (1 次 + 2 次重试)", ev.Payload["attempts"])
	}
	if ev.Payload["iterations"] != 9 {
		t.Errorf("iterations = %v, 期望 9 (3 次尝试 × 每次 3 轮 loop)", ev.Payload["iterations"])
	}
	if ev.Payload["error"] != "桩故障" {
		t.Errorf("failure 载荷应带 error, got %v", ev.Payload["error"])
	}
	if st.callCount("fail") != 9 {
		t.Errorf("runner 实际调用 %d 次, 期望 9 (与 iterations 自洽)", st.callCount("fail"))
	}
}

// TestHookPayloadGraphScope 整图 pre/post 载荷带图名/节点数/终态, 供宿主打点。
func TestHookPayloadGraphScope(t *testing.T) {
	bus := &recordBus{}
	e := fastEngine(newStub(), NewMemoryJournal())
	e.Hooks = bus
	if _, err := e.Run(context.Background(), linearSpec("a", "b"), RunOpts{RunID: "run-graph-scope"}); err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	pre, ok := bus.find(ScopeGraph, "pre", "")
	if !ok {
		t.Fatal("缺 graph pre 事件")
	}
	if pre.Payload["graph"] != "linear" || pre.Payload["nodes"] != 2 {
		t.Errorf("graph pre 载荷 = %v, 期望 graph=linear nodes=2", pre.Payload)
	}
	post, ok := bus.find(ScopeGraph, "post", "")
	if !ok {
		t.Fatal("缺 graph post 事件")
	}
	if post.Payload["status"] != RunStatusCompleted || post.Payload["completed"] != 2 {
		t.Errorf("graph post 载荷 = %v, 期望 status=completed completed=2", post.Payload)
	}
}
