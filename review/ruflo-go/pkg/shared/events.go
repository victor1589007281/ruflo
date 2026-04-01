// Package shared 提供跨模块复用的轻量基础设施：线程安全事件总线、重试与熔断器等，无业务域耦合。
//
// 事件子系统（events.go）采用发布-订阅模式：发布方按 Type 广播 Event，订阅方注册 EventHandler；
// 同类型多处理器顺序同步调用，发布与订阅解耦，避免模块间硬编码依赖。
package shared

import (
	"reflect"
	"sync"
	"time"
)

// Event 为总线上的轻量消息：类型键、来源标识、任意负载与时间戳。
type Event struct {
	Type      string         // 事件类型，用于路由到对应处理器列表
	Source    string         // 发布方标识
	Payload   map[string]any // 结构化负载
	Timestamp time.Time      // 事件发生时间
}

// EventHandler 处理单个 Event 的回调类型。
type EventHandler func(Event)

// EventBus 维护 eventType → 处理器切片映射；Publish 时复制切片再调用，避免持锁执行用户代码过久。
type EventBus struct {
	mu       sync.RWMutex              // 保护 handlers
	handlers map[string][]EventHandler // 事件类型到处理器列表
}

// NewEventBus 创建空总线。
func NewEventBus() *EventBus {
	return &EventBus{handlers: make(map[string][]EventHandler)}
}

// Subscribe 在写锁下将 handler 追加到 eventType 对应切片（允许多个订阅者，顺序即调用顺序）。
func (b *EventBus) Subscribe(eventType string, handler EventHandler) {
	if b == nil || handler == nil || eventType == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], handler)
}

// Publish 读锁下拷贝 e.Type 的处理器列表，释锁后按注册顺序同步调用（同一类型内串行）。
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

// Unsubscribe 用 reflect 比较函数指针，移除第一个与 handler 相同的注册项（需同一函数值地址）。
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
