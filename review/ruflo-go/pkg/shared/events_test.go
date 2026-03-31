package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func handlerInc(n *atomic.Int32) EventHandler {
	return func(e Event) {
		n.Add(1)
		_ = e.Type
	}
}

func TestEventBus_PublishSubscribe(t *testing.T) {
	t.Parallel()
	b := NewEventBus()
	var n atomic.Int32
	h := handlerInc(&n)
	b.Subscribe("x", h)
	b.Publish(Event{Type: "x", Payload: map[string]any{"k": 1}, Timestamp: time.Now()})
	if n.Load() != 1 {
		t.Fatalf("count=%d", n.Load())
	}
	b.Unsubscribe("x", h)
	b.Publish(Event{Type: "x"})
	if n.Load() != 1 {
		t.Fatalf("after unsubscribe count=%d", n.Load())
	}
}

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	t.Parallel()
	cb := CircuitBreaker(2, 50*time.Millisecond)
	if cb.State() != "closed" {
		t.Fatalf("initial %q", cb.State())
	}
	_ = cb.Call(func() error { return errors.New("e1") })
	_ = cb.Call(func() error { return errors.New("e2") })
	if cb.State() != "open" {
		t.Fatalf("want open got %q", cb.State())
	}
	_ = cb.Call(func() error { return nil })
	time.Sleep(60 * time.Millisecond)
	_ = cb.Call(func() error { return nil })
	if cb.State() != "closed" {
		t.Fatalf("want closed after heal got %q", cb.State())
	}
}

func TestRetryWithBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var n atomic.Int32
	err := RetryWithBackoff(ctx, 4, time.Millisecond, func() error {
		if n.Add(1) < 3 {
			return errors.New("fail")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts=%d", n.Load())
	}
}
