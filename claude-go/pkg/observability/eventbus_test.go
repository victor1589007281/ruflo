package observability

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBusEmitAndSubscribe(t *testing.T) {
	b := NewBus()
	var count int64
	b.SubscribeFunc(func(e Event) {
		if e.Type == EvtLLMCallComplete {
			atomic.AddInt64(&count, 1)
		}
	})

	b.Emit(Event{Type: EvtLLMCallComplete, Module: "llm", Name: "success"})
	b.Emit(Event{Type: EvtLLMCallComplete, Module: "llm", Name: "success"})
	b.Emit(Event{Type: EvtStageStart, Module: "workflow", Name: "design"})

	// 给 subscriber 一点时间执行 (subscriber 是同步的, 但 goroutine 调度可能延迟)
	time.Sleep(10 * time.Millisecond)

	if atomic.LoadInt64(&count) != 2 {
		t.Fatalf("期望 count=2, 实际 %d", count)
	}
}

func TestBusMultipleSubscribers(t *testing.T) {
	b := NewBus()
	var c1, c2 int64
	b.SubscribeFunc(func(e Event) { atomic.AddInt64(&c1, 1) })
	b.SubscribeFunc(func(e Event) { atomic.AddInt64(&c2, 1) })

	b.Emit(Event{Type: EvtLLMCallComplete})
	time.Sleep(10 * time.Millisecond)

	if atomic.LoadInt64(&c1) != 1 || atomic.LoadInt64(&c2) != 1 {
		t.Fatalf("期望两个 subscriber 各收到 1 条, 实际 c1=%d c2=%d", c1, c2)
	}
}

func TestBusPanicRecovery(t *testing.T) {
	b := NewBus()
	var called int64
	b.SubscribeFunc(func(e Event) {
		panic(" subscriber panic")
	})
	b.SubscribeFunc(func(e Event) {
		atomic.AddInt64(&called, 1)
	})

	// Emit 不应 panic, 第二个 subscriber 仍能执行
	b.Emit(Event{Type: EvtLLMCallComplete})
	time.Sleep(10 * time.Millisecond)

	if atomic.LoadInt64(&called) != 1 {
		t.Fatalf("panic subscriber 不应影响后续 subscriber, called=%d", called)
	}
}

func TestBusTypedEmit(t *testing.T) {
	b := NewBus()
	var received Event
	b.SubscribeFunc(func(e Event) {
		received = e
	})

	b.EmitTyped(EvtStageStart, "trace-1", "span-1", "workflow", "design", map[string]interface{}{"key": "val"})
	time.Sleep(10 * time.Millisecond)

	if received.Type != EvtStageStart {
		t.Fatalf("期望类型 %s, 实际 %s", EvtStageStart, received.Type)
	}
	if received.TraceID != "trace-1" || received.SpanID != "span-1" {
		t.Fatalf("trace/span 不匹配")
	}
	if received.Module != "workflow" || received.Name != "design" {
		t.Fatalf("module/name 不匹配")
	}
	if received.Payload["key"] != "val" {
		t.Fatalf("payload 不匹配")
	}
	if received.Timestamp.IsZero() {
		t.Fatalf("Timestamp 应自动填充")
	}
}

func TestGlobalBus(t *testing.T) {
	// 保存旧状态
	oldBus := globalBus
	defer func() { globalBus = oldBus }()

	globalBus = NewBus()
	var count int64
	SubscribeFunc(func(e Event) {
		if e.Type == EvtTeamStart {
			atomic.AddInt64(&count, 1)
		}
	})

	Emit(Event{Type: EvtTeamStart, Module: "team", Name: "dev"})
	time.Sleep(10 * time.Millisecond)

	if atomic.LoadInt64(&count) != 1 {
		t.Fatalf("全局 bus 发射失败, count=%d", count)
	}
}

func TestBusStats(t *testing.T) {
	b := NewBus()
	subs, _ := b.Stats()
	if subs != 0 {
		t.Fatalf("期望 0 个 subscriber, 实际 %d", subs)
	}
	b.SubscribeFunc(func(e Event) {})
	subs, _ = b.Stats()
	if subs != 1 {
		t.Fatalf("期望 1 个 subscriber, 实际 %d", subs)
	}
}
