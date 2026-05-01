// Package observability 提供基于 Hook + EventBus 的无侵入式可观测体系。
//
// 设计目标:
//   - 非侵入: 现有代码通过全局 Bus 发射事件, 无需修改函数签名
//   - 可扩展: 新增观测维度只需注册新 subscriber
//   - 零耦合: 业务代码只依赖本包的 Emit 函数, 不依赖 metrics/prometheus
//   - 全链路: TraceID 贯穿 LLM -> Stage -> Task -> Agent 调用链
package observability

import (
	"sync"
	"time"
)

// EventType 事件类型枚举。
type EventType string

const (
	EvtLLMCallStart     EventType = "llm:call:start"
	EvtLLMCallComplete  EventType = "llm:call:complete"
	EvtLLMCallError     EventType = "llm:call:error"
	EvtLLMCallFallback  EventType = "llm:call:fallback"
	EvtLLMCacheHit      EventType = "llm:cache:hit"
	EvtLLMCacheMiss     EventType = "llm:cache:miss"

	EvtStageStart    EventType = "stage:start"
	EvtStageComplete EventType = "stage:complete"
	EvtStageFail     EventType = "stage:fail"
	EvtStageRetry    EventType = "stage:retry"

	EvtTaskStart    EventType = "task:start"
	EvtTaskComplete EventType = "task:complete"
	EvtTaskFail     EventType = "task:fail"
	EvtTaskRetry    EventType = "task:retry"
	EvtTaskReady    EventType = "task:ready"

	EvtTeamStart    EventType = "team:start"
	EvtTeamComplete EventType = "team:complete"
	EvtTeamFail     EventType = "team:fail"
	EvtTeamStage    EventType = "team:stage"

	EvtCollabMessage   EventType = "collab:message"
	EvtCollabReview    EventType = "collab:review"
	EvtCollabConsensus EventType = "collab:consensus"
	EvtCollabHandoff   EventType = "collab:handoff"

	EvtPromptRender   EventType = "prompt:render"
	EvtPromptVersion  EventType = "prompt:version"
	EvtPromptCompare  EventType = "prompt:compare"
	EvtPromptABStart  EventType = "prompt:ab:start"
	EvtPromptABResult EventType = "prompt:ab:result"

	EvtTraceStart EventType = "trace:start"
	EvtTraceSpan  EventType = "trace:span"
	EvtTraceEnd   EventType = "trace:end"
)

// Event 统一事件结构。
type Event struct {
	Type      EventType              `json:"type"`
	Timestamp time.Time              `json:"ts"`
	TraceID   string                 `json:"trace_id,omitempty"`
	SpanID    string                 `json:"span_id,omitempty"`
	ParentID  string                 `json:"parent_id,omitempty"`
	Module    string                 `json:"module"`
	Name      string                 `json:"name"`
	Payload   map[string]interface{} `json:"payload,omitempty"`
}

// Subscriber 事件订阅者接口。
type Subscriber interface {
	OnEvent(e Event)
}

// SubscriberFunc 函数式订阅者。
type SubscriberFunc func(e Event)

func (f SubscriberFunc) OnEvent(e Event) { f(e) }

// Bus 线程安全的事件总线, 支持多订阅者和非阻塞发布。
type Bus struct {
	mu          sync.RWMutex
	subs        []Subscriber
	dropCount   uint64
	enqueueCount uint64
}

// NewBus 创建空事件总线。
func NewBus() *Bus {
	return &Bus{}
}

// Subscribe 注册订阅者。
func (b *Bus) Subscribe(s Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, s)
}

// SubscribeFunc 用函数注册订阅者。
func (b *Bus) SubscribeFunc(fn func(e Event)) {
	b.Subscribe(SubscriberFunc(fn))
}

// UnsubscribeAll 清空所有订阅者 (主要用于测试)。
func (b *Bus) UnsubscribeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = nil
}

// Emit 发布事件到所有订阅者。若某个 subscriber  panic, 捕获并继续。
// 非阻塞: subscriber 处理是同步的, 但预期 subscriber 自身实现异步缓冲。
func (b *Bus) Emit(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	b.mu.RLock()
	subs := make([]Subscriber, len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()

	for _, s := range subs {
		func() {
			defer func() { _ = recover() }()
			s.OnEvent(e)
		}()
	}
}

// EmitTyped 便捷方法: 按给定类型和 payload 构造事件并发射。
func (b *Bus) EmitTyped(et EventType, traceID, spanID, module, name string, payload map[string]interface{}) {
	b.Emit(Event{
		Type:     et,
		TraceID:  traceID,
		SpanID:   spanID,
		Module:   module,
		Name:     name,
		Payload:  payload,
	})
}

// Stats 返回总线统计。
func (b *Bus) Stats() (subscribers int, drops uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs), b.dropCount
}

// globalBus 进程级默认事件总线。
var globalBus = NewBus()

// GlobalBus 返回全局事件总线。
func GlobalBus() *Bus { return globalBus }

// Emit 向全局总线发射事件。
func Emit(e Event) { globalBus.Emit(e) }

// EmitTyped 向全局总线发射带类型的事件。
func EmitTyped(et EventType, traceID, spanID, module, name string, payload map[string]interface{}) {
	globalBus.EmitTyped(et, traceID, spanID, module, name, payload)
}

// Subscribe 向全局总线注册订阅者。
func Subscribe(s Subscriber) { globalBus.Subscribe(s) }

// SubscribeFunc 向全局总线注册函数订阅者。
func SubscribeFunc(fn func(e Event)) { globalBus.SubscribeFunc(fn) }
