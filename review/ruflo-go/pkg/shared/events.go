package shared

import (
	"reflect"
	"sync"
	"time"
)

// Event is a lightweight message on the bus.
type Event struct {
	Type      string
	Source    string
	Payload   map[string]any
	Timestamp time.Time
}

// EventHandler receives published events.
type EventHandler func(Event)

// EventBus dispatches events to subscribers by type.
type EventBus struct {
	mu       sync.RWMutex
	handlers map[string][]EventHandler
}

// NewEventBus creates an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{handlers: make(map[string][]EventHandler)}
}

// Subscribe registers a handler for eventType (multiple handlers allowed).
func (b *EventBus) Subscribe(eventType string, handler EventHandler) {
	if b == nil || handler == nil || eventType == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], handler)
}

// Publish invokes all handlers for e.Type in registration order.
func (b *EventBus) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.RLock()
	list := append([]EventHandler(nil), b.handlers[e.Type]...)
	b.mu.RUnlock()
	for _, h := range list {
		if h != nil {
			h(e)
		}
	}
}

// Unsubscribe removes the first handler equal to the given function pointer (reflect-based).
func (b *EventBus) Unsubscribe(eventType string, handler EventHandler) {
	if b == nil || handler == nil || eventType == "" {
		return
	}
	target := reflect.ValueOf(handler).Pointer()
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.handlers[eventType]
	var kept []EventHandler
	for _, h := range cur {
		if h == nil {
			continue
		}
		if reflect.ValueOf(h).Pointer() == target {
			continue
		}
		kept = append(kept, h)
	}
	if len(kept) == 0 {
		delete(b.handlers, eventType)
	} else {
		b.handlers[eventType] = kept
	}
}
