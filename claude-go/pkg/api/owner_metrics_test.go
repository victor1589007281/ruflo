// owner_metrics_test.go — R-H6 的 metrics 侧验收。
//
// 改造前 LLMCallRecord 根本没有 owner 字段：多租户下"谁把额度烧完了"只能翻日志
// 按 turn 反查。这里钉住两条路径（流式 / 非流式）都把 ctx 里的 owner 带进记录，
// 且缺省时归一为 local（与 types.OwnerFromContext 同一口径）。
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestLLMMetricsCarryOwnerNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, okResponseBody)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	c.Client = srv.Client()
	c.RetryCount = 0
	got := make(chan LLMCallRecord, 1)
	c.OnLLMMetrics = func(rec LLMCallRecord) { got <- rec }

	ctx := types.WithOwner(context.Background(), "tenant-a")
	if _, err := c.SendMessage(ctx, textMessages(), nil, nil, 16); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-got:
		if rec.Owner != "tenant-a" {
			t.Fatalf("非流式记录应带 ctx owner, 实际 %q", rec.Owner)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到指标记录")
	}
}

func TestLLMMetricsCarryOwnerStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"m","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	c.Client = srv.Client()
	c.RetryCount = 0
	got := make(chan LLMCallRecord, 1)
	c.OnLLMMetrics = func(rec LLMCallRecord) { got <- rec }

	ctx := types.WithOwner(context.Background(), "tenant-b")
	ev, errc := c.StreamMessage(ctx, textMessages(), nil, nil, 16)
	for range ev {
	}
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case rec := <-got:
		if rec.Owner != "tenant-b" {
			t.Fatalf("流式记录应带 ctx owner, 实际 %q", rec.Owner)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到指标记录")
	}
}

// TestLLMMetricsOwnerDefaultsToLocal 钉住"空 owner 不破坏旧行为"。
func TestLLMMetricsOwnerDefaultsToLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, okResponseBody)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	c.Client = srv.Client()
	c.RetryCount = 0
	got := make(chan LLMCallRecord, 1)
	c.OnLLMMetrics = func(rec LLMCallRecord) { got <- rec }

	if _, err := c.SendMessage(context.Background(), textMessages(), nil, nil, 16); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-got:
		if rec.Owner != types.DefaultOwnerID {
			t.Fatalf("未显式设置 owner 时应归一为 %q, 实际 %q", types.DefaultOwnerID, rec.Owner)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到指标记录")
	}
}
