// cache_usage_record_test.go — 钉住"缓存命中数必须进 LLMCallRecord"。
//
// 背景：Anthropic 线格式里 message_start.message.usage 常常是全零骨架，
// 真实用量（含 cache_read_input_tokens）只在末帧 message_delta.usage 里。
// 流式采集原先只从 message_delta 抄 input/output，于是本地 llm:call:complete
// 的 CacheReadTokens 恒为 0 —— 审计据此得出"缓存完全没生效、每次按全价重发"
// 的相反结论（实测同一 1630 token 请求第二次是 94 fresh + 1536 cached）。
//
// 这个测试用真实的帧形状（start 全零、delta 带缓存数）把两件事钉死：
// 缓存字段必须落到记录上，且 TotalTokens 的口径包含它们。
package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamRecordCarriesCacheTokensFromMessageDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			// message_start 全零骨架 —— 与实测网关一致
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"m","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			// 真实用量只在末帧
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":78,"output_tokens":8,"cache_read_input_tokens":1152,"cache_creation_input_tokens":64}}`,
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

	ev, errc := c.StreamMessage(context.Background(), textMessages(), nil, nil, 16)
	for range ev {
	}
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}

	select {
	case rec := <-got:
		if rec.CacheReadTokens != 1152 {
			t.Fatalf("CacheReadTokens 应取末帧 usage 的 1152, 实际 %d（回退到 message_start 的全零骨架）", rec.CacheReadTokens)
		}
		if rec.CacheCreationTokens != 64 {
			t.Fatalf("CacheCreationTokens 应为 64, 实际 %d", rec.CacheCreationTokens)
		}
		if rec.InputTokens != 78 {
			t.Fatalf("InputTokens 应为 78, 实际 %d", rec.InputTokens)
		}
		// 口径：TotalTokens = fresh + output + cache_read + cache_creation
		if want := 78 + 8 + 1152 + 64; rec.TotalTokens != want {
			t.Fatalf("TotalTokens 应为 %d（含缓存）, 实际 %d", want, rec.TotalTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到指标记录")
	}
}
