package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// newRetryTestClient 构造一个退避极短的真实客户端 (httptest 上游, 不访问外网)。
func newRetryTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClient(srv.URL, "test-key", "test-model")
	c.Client = srv.Client()
	c.RetryCount = 1
	c.RetryBase = time.Millisecond
	c.RetryMax = 5 * time.Millisecond
	return c
}

func textMessages() []types.APIMessage {
	return []types.APIMessage{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}}
}

const okResponseBody = `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`

// TestSendMessageRetriesConfigured400: 配置 400 后, 首次 400 会重试并最终成功。
func TestSendMessageRetriesConfigured400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"transient 400"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, okResponseBody)
	}))
	defer srv.Close()

	c := newRetryTestClient(t, srv)
	c.RetryStatuses = []int{400}
	resp, err := c.SendMessage(context.Background(), textMessages(), nil, nil, 16)
	if err != nil {
		t.Fatalf("配置 400 重试后应成功: %v", err)
	}
	if resp == nil || len(resp.Content) == 0 {
		t.Fatalf("响应为空: %+v", resp)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", got)
	}
}

// TestSendMessageDefault400NotRetried: 未配置时 400 仍然一次失败 (旧行为不变)。
func TestSendMessageDefault400NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"boom"}}`)
	}))
	defer srv.Close()

	c := newRetryTestClient(t, srv) // 没有 RetryStatuses
	if _, err := c.SendMessage(context.Background(), textMessages(), nil, nil, 16); err == nil {
		t.Fatal("未配置 400 重试时应失败")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("400 不应重试, 上游调用次数应为 1, got %d", got)
	}
}

// TestSendMessage401NotRetried: 认证错误必须立即失败, 不能被重试拖长。
func TestSendMessage401NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"bad key"}}`)
	}))
	defer srv.Close()

	c := newRetryTestClient(t, srv)
	c.RetryCount = 3
	if _, err := c.SendMessage(context.Background(), textMessages(), nil, nil, 16); err == nil {
		t.Fatal("401 应立即失败")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("401 不应重试, 上游调用次数应为 1, got %d", got)
	}
}

// TestSendMessageCacheRelated400DisablesBeforeRetry: cache_control 相关的 400
// 必须**优先**走去断点自适应, 而不是被当作普通 400 重试 (即使 400 可重试)。
//
// 判据是第二次请求体里**没有 cache_control**, 且调用方拿到 200。
func TestSendMessageCacheRelated400DisablesBeforeRetry(t *testing.T) {
	var calls int32
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			// 确认第一次确实带了 cache_control, 否则这条测试测不到"去断点"。
			if !strings.Contains(string(body), "cache_control") {
				t.Errorf("首次请求应带 cache_control, body=%s", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"cache_control is not supported for this model"}}`)
			return
		}
		secondBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, okResponseBody)
	}))
	defer srv.Close()

	c := newRetryTestClient(t, srv)
	c.PromptCacheMode = "on" // 强制带上 cache_control
	// 同时把 400 配成可重试: 自适应必须赢, 不能先按普通重试发同一份带缓存的请求。
	c.RetryStatuses = []int{400}
	resp, err := c.SendMessage(context.Background(), textMessages(), []string{"system prompt"}, nil, 16)
	if err != nil {
		t.Fatalf("cache 自适应后应成功: %v", err)
	}
	if resp == nil {
		t.Fatal("响应为空")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应恰好重发 1 次, got %d", got)
	}
	if strings.Contains(secondBody, "cache_control") {
		t.Fatalf("自适应重试的请求体不应再带 cache_control: %s", secondBody)
	}
}

// TestStreamMessageRetriesConfigured400: 流式路径同样按配置重试 400, 且
// 重试发生在向调用方交出任何事件之前。
func TestStreamMessageRetriesConfigured400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"transient"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := newRetryTestClient(t, srv)
	c.RetryStatuses = []int{400}
	events, errs := c.StreamMessage(context.Background(), textMessages(), nil, nil, 16)

	var got int
	for range events {
		got++
	}
	if err := <-errs; err != nil {
		t.Fatalf("流式重试后应成功: %v", err)
	}
	if got == 0 {
		t.Fatal("应至少收到一个事件")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
}
