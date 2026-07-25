package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrace_零值判定(t *testing.T) {
	if !(Trace{}).IsZero() {
		t.Error("空 Trace 应为 zero")
	}
	for _, tr := range []Trace{
		{RunID: "r"}, {NodeID: "n"}, {TurnID: "t"}, {CallID: "c"},
	} {
		if tr.IsZero() {
			t.Errorf("%+v 不该判成 zero", tr)
		}
	}
}

// WithTrace 传零值时必须原样返回 ctx: 否则每层适配器都会往 ctx 里塞一个空值,
// 后面真 trace 反而被覆盖掉。
func TestTrace_零值不入ctx(t *testing.T) {
	base := WithTrace(context.Background(), Trace{RunID: "keep"})
	got := WithTrace(base, Trace{})
	if TraceFromContext(got).RunID != "keep" {
		t.Error("零值 WithTrace 覆盖了已有 trace")
	}
}

func TestTrace_出站落成四个头(t *testing.T) {
	ctx := WithTrace(context.Background(), Trace{
		RunID: "r1", NodeID: "n1", TurnID: "t1", CallID: "c1",
	})
	req := httptest.NewRequest(http.MethodPost, "http://x/messages", nil)
	setTraceHeaders(req, ctx)

	for h, want := range map[string]string{
		TraceHeaderRunID:  "r1",
		TraceHeaderNodeID: "n1",
		TraceHeaderTurnID: "t1",
		TraceHeaderCallID: "c1",
	} {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, 期望 %q", h, got, want)
		}
	}
}

// 空字段不发头 (而不是发空值): 直连 provider 的默认形态必须一字节不变。
func TestTrace_空字段不发头(t *testing.T) {
	ctx := WithTrace(context.Background(), Trace{RunID: "only-run"})
	req := httptest.NewRequest(http.MethodPost, "http://x/messages", nil)
	setTraceHeaders(req, ctx)

	if req.Header.Get(TraceHeaderRunID) != "only-run" {
		t.Error("RunID 头缺失")
	}
	for _, h := range []string{TraceHeaderNodeID, TraceHeaderTurnID, TraceHeaderCallID} {
		if _, ok := req.Header[http.CanonicalHeaderKey(h)]; ok {
			t.Errorf("%s 不该出现", h)
		}
	}
}

// 未设 trace 的 ctx: 一个头都不加。
func TestTrace_未设置时零副作用(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://x/messages", nil)
	before := len(req.Header)
	setTraceHeaders(req, context.Background())
	if len(req.Header) != before {
		t.Errorf("头数量从 %d 变成 %d", before, len(req.Header))
	}
	setTraceHeaders(nil, context.Background()) // 不该 panic
}

// trace 与 LLMMetricsContext 各用一个 key, 互不覆盖 ——
// 这是把 trace 单独放一个 context key 而不是塞进 LLMMetricsContext 的理由。
func TestTrace_与指标上下文互不覆盖(t *testing.T) {
	ctx := WithLLMMetrics(context.Background(), LLMMetricsContext{Source: "feishu", Purpose: "p"})
	ctx = WithTrace(ctx, Trace{RunID: "r"})
	if m := llmMetricsFromContext(ctx); m.Source != "feishu" || m.Purpose != "p" {
		t.Errorf("WithTrace 覆盖了指标上下文: %+v", m)
	}
	// 反向: 后设指标不该抹掉 trace
	ctx = WithLLMMetrics(ctx, LLMMetricsContext{Source: "dashboard"})
	if TraceFromContext(ctx).RunID != "r" {
		t.Error("WithLLMMetrics 抹掉了 trace")
	}
}
