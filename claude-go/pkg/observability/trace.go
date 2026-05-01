package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// TraceContext 贯穿调用链的追踪上下文, 兼容 OpenTelemetry 语义。
type TraceContext struct {
	TraceID  string
	SpanID   string
	ParentID string
	Baggage  map[string]string // 业务透传键值对
}

// NewTraceID 生成 16-byte hex 随机 trace ID。
func NewTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSpanID 生成 8-byte hex 随机 span ID。
func NewSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewRootTrace 创建根追踪上下文。
func NewRootTrace() TraceContext {
	return TraceContext{
		TraceID: NewTraceID(),
		SpanID:  NewSpanID(),
		Baggage: make(map[string]string),
	}
}

// NewChild 从当前 trace 创建子 span。
func (t TraceContext) NewChild() TraceContext {
	return TraceContext{
		TraceID:  t.TraceID,
		SpanID:   NewSpanID(),
		ParentID: t.SpanID,
		Baggage:  copyMap(t.Baggage),
	}
}

// WithBaggage 向 baggage 添加键值对并返回新 TraceContext。
func (t TraceContext) WithBaggage(k, v string) TraceContext {
	nt := t
	if nt.Baggage == nil {
		nt.Baggage = make(map[string]string)
	} else {
		nt.Baggage = copyMap(nt.Baggage)
	}
	nt.Baggage[k] = v
	return nt
}

// BaggageValue 读取 baggage 值。
func (t TraceContext) BaggageValue(k string) string {
	if t.Baggage == nil {
		return ""
	}
	return t.Baggage[k]
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// traceKey 是 context.Context 中保存 TraceContext 的 key 类型 (非导出, 避免冲突)。
type traceKey struct{}

// WithTrace 将 TraceContext 注入 context。
func WithTrace(ctx context.Context, tc TraceContext) context.Context {
	return context.WithValue(ctx, traceKey{}, tc)
}

// TraceFromContext 从 context 提取 TraceContext, 不存在时返回空值。
func TraceFromContext(ctx context.Context) TraceContext {
	if tc, ok := ctx.Value(traceKey{}).(TraceContext); ok {
		return tc
	}
	return TraceContext{}
}

// EnsureTrace 确保 context 中存在 TraceContext, 不存在则新建根 trace。
func EnsureTrace(ctx context.Context) (context.Context, TraceContext) {
	tc := TraceFromContext(ctx)
	if tc.TraceID == "" {
		tc = NewRootTrace()
		ctx = WithTrace(ctx, tc)
	}
	return ctx, tc
}

// Span 表示一个可记录起止时间的 span。
type Span struct {
	TraceContext
	Name      string
	StartTime time.Time
	EndTime   time.Time
	Module    string
}

// StartSpan 从 context 创建新 span, 同时注入子 context。
func StartSpan(ctx context.Context, module, name string) (context.Context, *Span) {
	ctx, tc := EnsureTrace(ctx)
	child := tc.NewChild()
	ctx = WithTrace(ctx, child)
	span := &Span{
		TraceContext: child,
		Name:         name,
		StartTime:    time.Now(),
		Module:       module,
	}
	Emit(Event{
		Type:      EvtTraceSpan,
		TraceID:   child.TraceID,
		SpanID:    child.SpanID,
		ParentID:  child.ParentID,
		Module:    module,
		Name:      name,
		Timestamp: span.StartTime,
		Payload: map[string]interface{}{
			"event": "start",
			"name":  name,
		},
	})
	return ctx, span
}

// Finish 结束 span 并发射完成事件。
func (s *Span) Finish() {
	s.EndTime = time.Now()
	dur := s.EndTime.Sub(s.StartTime).Seconds()
	Emit(Event{
		Type:      EvtTraceSpan,
		TraceID:   s.TraceID,
		SpanID:    s.SpanID,
		ParentID:  s.ParentID,
		Module:    s.Module,
		Name:      s.Name,
		Timestamp: s.EndTime,
		Payload: map[string]interface{}{
			"event":    "end",
			"name":     s.Name,
			"duration": dur,
		},
	})
}

// ActiveSpanStore 内存中的活跃 span 存储, 供 dashboard 查询当前进行中的调用。
type ActiveSpanStore struct {
	mu    sync.RWMutex
	spans map[string]*Span // key = spanID
}

// NewActiveSpanStore 创建活跃 span 存储。
func NewActiveSpanStore() *ActiveSpanStore {
	return &ActiveSpanStore{spans: make(map[string]*Span)}
}

// Add 添加活跃 span。
func (s *ActiveSpanStore) Add(sp *Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans[sp.SpanID] = sp
}

// Remove 移除活跃 span。
func (s *ActiveSpanStore) Remove(spanID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.spans, spanID)
}

// List 返回当前所有活跃 span。
func (s *ActiveSpanStore) List() []Span {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Span, 0, len(s.spans))
	for _, sp := range s.spans {
		out = append(out, *sp)
	}
	return out
}

// String returns a compact trace identifier for logging.
func (t TraceContext) String() string {
	if t.TraceID == "" {
		return "trace:none"
	}
	if t.ParentID == "" {
		return fmt.Sprintf("trace:%s span:%s", t.TraceID[:8], t.SpanID)
	}
	return fmt.Sprintf("trace:%s span:%s parent:%s", t.TraceID[:8], t.SpanID, t.ParentID)
}
