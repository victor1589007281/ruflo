// graph_internal_bridge_trace_test.go — R-H7: Event 的耗时/结果/失败形态要真的
// 出现在总线 payload 上。既有键 (hook/turn/error) 的语义不变是前提。
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/graph"
)

type payloadBus struct{ evs []graph.HookEvent }

func (b *payloadBus) Emit(_ context.Context, ev graph.HookEvent) graph.HookDecision {
	b.evs = append(b.evs, ev)
	return graph.HookDecision{}
}

func TestInternalHookBridgeTracePayload(t *testing.T) {
	bus := &payloadBus{}
	b := &internalHookBridge{bus: bus, runID: "run-h7"}

	b.Observe(context.Background(), internal_hook.Event{
		Scope: "turn", Phase: "PreRequest", Hook: "memory_inject", TurnCount: 2,
		Err: "", Duration: 7 * time.Millisecond,
		Result: "prompt_modified", Panic: false, TimedOut: false,
	})

	if len(bus.evs) != 1 {
		t.Fatalf("应投递 1 条事件, 实际 %d", len(bus.evs))
	}
	p := bus.evs[0].Payload
	if got := p["duration_ms"]; got != int64(7) {
		t.Errorf("duration_ms 应为 7, 实际 %#v", got)
	}
	if got := p["result"]; got != "prompt_modified" {
		t.Errorf("result 应为 prompt_modified, 实际 %#v", got)
	}
	if got := p["panic"]; got != false {
		t.Errorf("panic 应为 false, 实际 %#v", got)
	}
	if got := p["timed_out"]; got != false {
		t.Errorf("timed_out 应为 false, 实际 %#v", got)
	}
	// 既有键语义不变。
	if got := p["hook"]; got != "memory_inject" {
		t.Errorf("hook 键被改动: %#v", got)
	}
	if got := p["turn"]; got != 2 {
		t.Errorf("turn 键被改动: %#v", got)
	}
	if got := p["error"]; got != "" {
		t.Errorf("error 键被改动: %#v", got)
	}

	// 失败/超时形态同样透传。
	b.Observe(context.Background(), internal_hook.Event{
		Scope: "tool", Phase: "PreToolUse", Hook: "retrieval",
		Err: "deadline exceeded", Duration: 30 * time.Millisecond,
		Panic: true, TimedOut: true,
	})
	p2 := bus.evs[1].Payload
	if got := p2["panic"]; got != true {
		t.Errorf("panic 应为 true, 实际 %#v", got)
	}
	if got := p2["timed_out"]; got != true {
		t.Errorf("timed_out 应为 true, 实际 %#v", got)
	}
	if got := p2["duration_ms"]; got != int64(30) {
		t.Errorf("duration_ms 应为 30, 实际 %#v", got)
	}
}
