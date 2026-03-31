package swarm

import (
	"sync"
	"testing"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

func TestSendReceive(t *testing.T) {
	cfg := MessageBusConfig{ProcessInterval: 5 * time.Millisecond, ProcessBatchMax: 10}
	b := NewMessageBus(cfg)
	defer b.Shutdown()

	var mu sync.Mutex
	var got []api.Message
	b.Subscribe("agent-a", func(m api.Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}, nil)

	_ = b.Send(api.Message{To: "agent-a", Type: api.MessageTypeDirect, Payload: map[string]any{"x": 1}})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	x := got[0].Payload["x"]
	if x != 1 && x != float64(1) {
		t.Fatalf("payload: %#v", got[0].Payload)
	}
}

func TestPriority(t *testing.T) {
	cfg := MessageBusConfig{ProcessInterval: 5 * time.Millisecond, ProcessBatchMax: 10}
	b := NewMessageBus(cfg)
	defer b.Shutdown()

	var order []api.MessagePriority
	var mu sync.Mutex
	b.Subscribe("p", func(m api.Message) {
		mu.Lock()
		order = append(order, m.Priority)
		mu.Unlock()
	}, nil)

	_ = b.Send(api.Message{To: "p", Priority: api.MessagePriorityNormal, Payload: map[string]any{"n": 1}})
	_ = b.Send(api.Message{To: "p", Priority: api.MessagePriorityCritical, Payload: map[string]any{"n": 2}})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(order))
	}
	if order[0] != api.MessagePriorityCritical {
		t.Fatalf("critical first, got %v then %v", order[0], order[1])
	}
}

func TestBroadcast(t *testing.T) {
	// Long interval so queues still have keys when Broadcast enumerates recipients.
	cfg := MessageBusConfig{ProcessInterval: 500 * time.Millisecond, ProcessBatchMax: 10}
	b := NewMessageBus(cfg)
	defer b.Shutdown()

	agents := []string{"a1", "a2", "a3"}
	var mu sync.Mutex
	counts := map[string]int{}
	for _, id := range agents {
		aid := id
		b.Subscribe(aid, func(m api.Message) {
			if m.Type == api.MessageTypeEvent {
				mu.Lock()
				counts[aid]++
				mu.Unlock()
			}
		}, nil)
	}

	for _, id := range agents {
		_ = b.Send(api.Message{To: id, Type: api.MessageTypeHeartbeat})
	}
	b.Broadcast(api.Message{Type: api.MessageTypeEvent, Payload: map[string]any{"k": "v"}}, "sender")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := counts["a1"] >= 1 && counts["a2"] >= 1 && counts["a3"] >= 1
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range agents {
		if counts[id] < 1 {
			t.Fatalf("agent %s missing broadcast: %#v", id, counts)
		}
	}
}

func TestUnsubscribe(t *testing.T) {
	cfg := MessageBusConfig{ProcessInterval: 200 * time.Millisecond, ProcessBatchMax: 10}
	b := NewMessageBus(cfg)
	defer b.Shutdown()

	var mu sync.Mutex
	var n int
	b.Subscribe("u-agent", func(m api.Message) {
		mu.Lock()
		n++
		mu.Unlock()
	}, nil)

	_ = b.Send(api.Message{To: "u-agent", Type: api.MessageTypeDirect, Payload: map[string]any{"k": 1}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := n >= 1
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if n < 1 {
		mu.Unlock()
		t.Fatal("expected first message delivered")
	}
	mu.Unlock()

	b.Unsubscribe("u-agent")
	_ = b.Send(api.Message{To: "u-agent", Type: api.MessageTypeDirect, Payload: map[string]any{"k": 2}})
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 1 {
		t.Fatalf("after unsubscribe expected 1 delivery, got %d", got)
	}
}

func TestGetQueueDepth(t *testing.T) {
	cfg := MessageBusConfig{ProcessInterval: 10 * time.Second, ProcessBatchMax: 1}
	b := NewMessageBus(cfg)
	defer b.Shutdown()

	_ = b.Send(api.Message{To: "depth-a", Type: api.MessageTypeDirect, Payload: map[string]any{}})
	_ = b.Send(api.Message{To: "depth-a", Type: api.MessageTypeDirect, Payload: map[string]any{}})
	_ = b.Send(api.Message{To: "depth-a", Type: api.MessageTypeDirect, Payload: map[string]any{}})

	if d := b.GetQueueDepth("depth-a"); d != 3 {
		t.Fatalf("GetQueueDepth: %d", d)
	}
}

func TestHasPendingMessages(t *testing.T) {
	cfg := MessageBusConfig{ProcessInterval: 10 * time.Second, ProcessBatchMax: 1}
	b := NewMessageBus(cfg)
	defer b.Shutdown()

	_ = b.Send(api.Message{To: "pend-x", Type: api.MessageTypeDirect, Payload: map[string]any{}})
	if !b.HasPendingMessages("pend-x") {
		t.Fatal("expected pending messages")
	}
}

