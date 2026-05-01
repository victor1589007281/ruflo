package observability

import (
	"context"
	"strings"
	"testing"
)

func TestNewTraceID(t *testing.T) {
	id1 := NewTraceID()
	id2 := NewTraceID()
	if len(id1) != 32 {
		t.Fatalf("trace ID 长度应为 32, 实际 %d", len(id1))
	}
	if id1 == id2 {
		t.Fatalf("两个 trace ID 不应相同")
	}
}

func TestNewSpanID(t *testing.T) {
	id := NewSpanID()
	if len(id) != 16 {
		t.Fatalf("span ID 长度应为 16, 实际 %d", len(id))
	}
}

func TestTraceContextWithBaggage(t *testing.T) {
	tc := NewRootTrace()
	if tc.TraceID == "" || tc.SpanID == "" {
		t.Fatalf("根 trace 应生成 TraceID 和 SpanID")
	}
	if tc.Baggage == nil {
		t.Fatalf("Baggage 应自动初始化")
	}

	tc2 := tc.WithBaggage("team", "go-dev")
	if tc2.BaggageValue("team") != "go-dev" {
		t.Fatalf("baggage 写入失败")
	}
	// 原 trace 不应被修改
	if tc.BaggageValue("team") != "" {
		t.Fatalf("WithBaggage 不应修改原 trace")
	}
}

func TestTraceContextNewChild(t *testing.T) {
	parent := NewRootTrace().WithBaggage("key", "val")
	child := parent.NewChild()

	if child.TraceID != parent.TraceID {
		t.Fatalf("子 trace 应继承 TraceID")
	}
	if child.SpanID == parent.SpanID {
		t.Fatalf("子 trace 应有新的 SpanID")
	}
	if child.ParentID != parent.SpanID {
		t.Fatalf("子 trace 的 ParentID 应为父 SpanID")
	}
	if child.BaggageValue("key") != "val" {
		t.Fatalf("子 trace 应继承 baggage")
	}
}

func TestWithTraceAndFromContext(t *testing.T) {
	ctx := context.Background()
	tc := NewRootTrace()
	ctx = WithTrace(ctx, tc)

	got := TraceFromContext(ctx)
	if got.TraceID != tc.TraceID {
		t.Fatalf("从 context 提取的 trace 不匹配")
	}

	// 空 context 应返回空值
	empty := TraceFromContext(context.Background())
	if empty.TraceID != "" {
		t.Fatalf("空 context 应返回空 trace")
	}
}

func TestEnsureTrace(t *testing.T) {
	ctx := context.Background()
	ctx, tc := EnsureTrace(ctx)
	if tc.TraceID == "" {
		t.Fatalf("EnsureTrace 应创建新 trace")
	}

	// 已有 trace 时不应覆盖
	ctx2, tc2 := EnsureTrace(ctx)
	if tc2.TraceID != tc.TraceID {
		t.Fatalf("EnsureTrace 不应覆盖已有 trace")
	}
	if ctx2 != ctx {
		t.Fatalf("EnsureTrace 应返回相同 context")
	}
}

func TestStartSpan(t *testing.T) {
	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test", "op1")
	if span.Name != "op1" || span.Module != "test" {
		t.Fatalf("span 名称/模块不匹配")
	}
	if span.TraceID == "" || span.SpanID == "" {
		t.Fatalf("span 应包含 trace/span ID")
	}
	if span.StartTime.IsZero() {
		t.Fatalf("span 应有开始时间")
	}

	// context 应包含子 trace
	tc := TraceFromContext(ctx)
	if tc.SpanID != span.SpanID {
		t.Fatalf("context 中的 span ID 不匹配")
	}

	span.Finish()
	if span.EndTime.IsZero() {
		t.Fatalf("Finish 后应有结束时间")
	}
}

func TestTraceContextString(t *testing.T) {
	tc := NewRootTrace()
	s := tc.String()
	if !strings.HasPrefix(s, "trace:") {
		t.Fatalf("String 应以 trace: 开头")
	}

	empty := TraceContext{}
	if empty.String() != "trace:none" {
		t.Fatalf("空 trace 应输出 trace:none")
	}
}

func TestActiveSpanStore(t *testing.T) {
	store := NewActiveSpanStore()
	span := &Span{
		TraceContext: NewRootTrace(),
		Name:         "test",
		Module:       "mod",
	}
	store.Add(span)
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("期望 1 个活跃 span, 实际 %d", len(list))
	}

	store.Remove(span.SpanID)
	list = store.List()
	if len(list) != 0 {
		t.Fatalf("移除后应为 0, 实际 %d", len(list))
	}
}
