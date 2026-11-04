package llmgw

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
)

// newRetryUpstream 返回一个按 calls 顺序应答的假上游: 第 1 次按 firstStatus 拒绝,
// 之后返回 200(status=200) 或传入的 body。
func newRetryUpstream(t *testing.T, firstStatus int, okBody string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(firstStatus)
			_, _ = io.WriteString(w, `{"error":{"message":"transient"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestGatewayRetries400Then200: llm-gateway 的默认策略含 400, 写响应前重试。
func TestGatewayRetries400Then200(t *testing.T) {
	up, calls := newRetryUpstream(t, http.StatusBadRequest,
		`{"id":"m1","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":5,"output_tokens":3}}`)
	dir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, dir).Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "pong") {
		t.Fatalf("400 应被重试并最终 200, got %d body=%s", resp.StatusCode, body)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
	rec := readFirstRecord(t, filepath.Join(dir, "access.jsonl"))
	if rec.Attempts != 2 || rec.Status != 200 {
		t.Fatalf("访问日志应记录 attempts=2 status=200, got %+v", rec)
	}
}

// TestGatewayStream200NotRetried: 2xx 流开始回传后不再重试。
func TestGatewayStream200NotRetried(t *testing.T) {
	const sse = "data: {\"type\":\"message_stop\"}\n\n"
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()
	gw := httptest.NewServer(newTestServer(t, up.URL, "").Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != sse {
		t.Fatalf("流式响应应原样透传, got %d body=%q", resp.StatusCode, body)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("2xx 流不应重试, 上游调用次数应为 1, got %d", n)
	}
}

// TestGatewayRetryDisabled: SetRetryPolicy 可关闭重试 (旧调用点可显式回到 0 次)。
func TestGatewayRetryDisabled(t *testing.T) {
	up, calls := newRetryUpstream(t, http.StatusInternalServerError, `{}`)
	srv := newTestServer(t, up.URL, "")
	srv.SetRetryPolicy(api.RetryPolicy{MaxRetries: 0})
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("禁用重试后应回传 500, got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("禁用重试后上游调用次数应为 1, got %d", n)
	}
}

// TestGateway401NotRetried: 401 不在默认可重试集合, 必须立即回传。
func TestGateway401NotRetried(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer up.Close()
	gw := httptest.NewServer(newTestServer(t, up.URL, "").Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应回传 401, got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("401 不应重试, got %d", n)
	}
}

// newRouteRetryServer 用带模型级重试配置的 route 建网关。
//
// 直接调 NewServer 而不是复用 newTestServer: 后者钉死了 Route 的字段, 改它会
// 影响本文件之外所有用例, 而这里要验的恰恰是"配置有没有到达 route"。
func newRouteRetryServer(t *testing.T, upstreamURL string, rt Route) *Server {
	t.Helper()
	rt.Provider = "kimi"
	rt.BaseURL = upstreamURL
	rt.APIKey = "sk-kimi"
	rt.Models = []string{"k3"}
	srv, err := NewServer([]Route{rt}, "kimi", "")
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// TestGatewayRouteRetryConfigApplied: 模型里的 retryStatuses/retryCount 必须生效
// (配置加上但没人读, 是这类字段最常见的死法)。
func TestGatewayRouteRetryConfigApplied(t *testing.T) {
	up, calls := newRetryUpstream(t, http.StatusTeapot, `{"ok":true}`)
	srv := newRouteRetryServer(t, up.URL, Route{
		RetryCount: 1, RetryBaseSec: 1, RetryStatuses: []int{418},
	})
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("route 显式配置的 418 应被重试, got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
}

// TestGatewayRouteRetryDisabled: retryCount:-1 在 route 上必须能压过网关默认。
func TestGatewayRouteRetryDisabled(t *testing.T) {
	up, calls := newRetryUpstream(t, http.StatusInternalServerError, `{}`)
	srv := newRouteRetryServer(t, up.URL, Route{RetryCount: -1})
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("route 禁用重试后应原样回传 500, got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("route 禁用重试后上游调用次数应为 1, got %d", n)
	}
}
