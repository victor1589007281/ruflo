package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// retryTestGateway 构造一个 provider 指向 httptest 上游的网关, 用于透传路径
// 的端到端重试验证 (不访问外网, 不需要真实 API key)。
func retryTestGateway(t *testing.T, upstreamURL string, mc modelconfig.ModelConfig) *gateway {
	t.Helper()
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "") // 避免 ApplyGatewayOverride 改写 BaseURL
	cfg := modelconfig.ConfigJSON{
		Providers: map[string]modelconfig.ProviderConfig{
			"p": {
				Name:    "p",
				BaseURL: upstreamURL,
				APIKey:  "sk-test",
				Models:  map[string]modelconfig.ModelConfig{"p:m": mc},
			},
		},
	}
	cfg.AI.DefaultModelAlias = "p:m"
	registry, resolver, err := modelconfig.LoadFromConfig(cfg)
	if err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	return &gateway{registry: registry, resolver: resolver, tryPrefix: "p"}
}

func passthroughBody() []byte {
	return []byte(`{"model":"p:m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
}

// runPassthrough 直接调用透传处理器 (与 handleMessages 的 anthropic 分支一致)。
func runPassthrough(g *gateway, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	rc := g.resolver.ResolveAlias("p:m")
	g.proxyPassthrough(rec, req, body, rc)
	return rec
}

// TestProxyPassthroughRetries500Then200: 上游先 500 再 200, 客户端最终拿到 200,
// 上游被调用 2 次 (退避约 1s, 默认策略)。
func TestProxyPassthroughRetries500Then200(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	start := time.Now()
	rec := runPassthrough(g, passthroughBody())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("最终应透传 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Fatalf("重试前应退避 (~1s), 实际只用了 %v", elapsed)
	}
}

// TestProxyPassthroughRetries400Then200: 400 属于网关默认可重试集合 (用户要求)。
func TestProxyPassthroughRetries400Then200(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"transient 400"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	rec := runPassthrough(g, passthroughBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("400 应被重试并最终 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
}

// TestProxyPassthroughStream200NotRetried: 2xx 流一旦开始回传就不再重试。
func TestProxyPassthroughStream200NotRetried(t *testing.T) {
	var calls int32
	const sse = "data: {\"type\":\"message_stop\"}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	body := []byte(`{"model":"p:m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := runPassthrough(g, body)
	if rec.Code != http.StatusOK || rec.Body.String() != sse {
		t.Fatalf("流式响应应逐字节透传, got %d body=%q", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("2xx 流不应重试, 上游调用次数应为 1, got %d", n)
	}
}

// TestProxyPassthroughContextCancelStopsRetry: ctx 取消后立即停止, 不写第二次响应。
func TestProxyPassthroughContextCancelStopsRetry(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"always"}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	body := passthroughBody()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	g.proxyPassthrough(rec, req, body, g.resolver.ResolveAlias("p:m"))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ctx 取消后应立即返回, 实际用了 %v", elapsed)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("取消前只应发出 1 次请求, got %d", n)
	}
	// 取消后不得再写第二次响应 (否则会看到 502 错误信封)。
	if rec.Body.Len() != 0 {
		t.Fatalf("取消后不应写响应体, got %q", rec.Body.String())
	}
}

// TestProxyPassthroughHonorsRetryAfter: Retry-After 大于计算退避时以它为准。
// 上游先用 500 + Retry-After: 2 拒绝, 再 200。
func TestProxyPassthroughHonorsRetryAfter(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	start := time.Now()
	rec := runPassthrough(g, passthroughBody())
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("应最终 200, got %d", rec.Code)
	}
	if elapsed < 1800*time.Millisecond {
		t.Fatalf("应等待 Retry-After=2s, 实际 %v", elapsed)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
}

// TestProxyPassthroughRetryDisabled: retryCount:-1 显式禁用 (400 也不重试)。
func TestProxyPassthroughRetryDisabled(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"always"}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{RetryCount: -1})
	rec := runPassthrough(g, passthroughBody())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("禁用重试后应直接回传 500, got %d", rec.Code)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("禁用重试后上游调用次数应为 1, got %d", n)
	}
}

// TestProxyOpenAIFormatRetries: OpenAI 格式透传同样在写响应前重试 400。
func TestProxyOpenAIFormatRetries(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"transient"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	body := []byte(`{"model":"p:m","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	g.proxyOpenAIFormat(rec, req, "/chat/completions")
	if rec.Code != http.StatusOK {
		t.Fatalf("应最终 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("上游应被调用 2 次, got %d", n)
	}
}

// TestRetryPolicyFromResolvedConfig: 模型配置里的 retryCount/-1 经 ResolvedConfig
// 到达透传路径 (避免"配置加了但没人读")。
func TestRetryPolicyFromResolvedConfig(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{
		RetryCount:    7,
		RetryBaseSec:  3,
		RetryMaxSec:   40,
		RetryStatuses: []int{400, 502},
	})
	rc := g.resolver.ResolveAlias("p:m")
	if rc.RetryCount != 7 || rc.RetryBaseSec != 3 || rc.RetryMaxSec != 40 {
		t.Fatalf("重试标量未到达 ResolvedConfig: %+v", rc)
	}
	if len(rc.RetryStatuses) != 2 || rc.RetryStatuses[0] != 400 {
		t.Fatalf("retryStatuses 未到达 ResolvedConfig: %+v", rc.RetryStatuses)
	}
}

// TestBuildClientRetryDefaults 钉住翻译路径 (protocol 非 anthropic) 的 api.Client
// 默认策略与透传路径一致。
//
// 改造前这里沿用 api.Client 的历史默认 (4 次 / 3s / 60s, 不含 400): 同一个上游
// 400, 模型配了 protocol: openai 就直接失败, 不配反而会重试。用户报告的正是这种
// "网关层直接失败" 的形态。
func TestBuildClientRetryDefaults(t *testing.T) {
	c := buildClient(modelconfig.ResolvedConfig{
		BaseURL: "https://example.com/v1", APIKey: "sk-test", ProviderName: "p",
	})
	if c.RetryCount != 2 {
		t.Fatalf("未配置时额外重试应为 2, 实际 %d", c.RetryCount)
	}
	if c.RetryBase != time.Second || c.RetryMax != 8*time.Second {
		t.Fatalf("退避默认应为 1s/8s, 实际 %v/%v", c.RetryBase, c.RetryMax)
	}
	if !hasRetryStatus(c.RetryStatuses, http.StatusBadRequest) {
		t.Fatalf("默认可重试集合必须包含 400, 实际 %v", c.RetryStatuses)
	}
	for _, code := range []int{429, 500, 502, 503, 504, 529} {
		if !hasRetryStatus(c.RetryStatuses, code) {
			t.Fatalf("默认可重试集合缺 %d, 实际 %v", code, c.RetryStatuses)
		}
	}
}

// TestBuildClientRetryConfigured: 显式配置覆盖默认值, -1 关闭重试。
func TestBuildClientRetryConfigured(t *testing.T) {
	c := buildClient(modelconfig.ResolvedConfig{
		BaseURL: "https://example.com/v1", APIKey: "sk-test", ProviderName: "p",
		RetryCount: 5, RetryBaseSec: 2, RetryMaxSec: 30,
		RetryStatuses: []int{400, 502},
	})
	if c.RetryCount != 5 || c.RetryBase != 2*time.Second || c.RetryMax != 30*time.Second {
		t.Fatalf("显式配置未生效: count=%d base=%v max=%v", c.RetryCount, c.RetryBase, c.RetryMax)
	}
	if !hasRetryStatus(c.RetryStatuses, 400) || !hasRetryStatus(c.RetryStatuses, 502) {
		t.Fatalf("显式状态码集合未生效: %v", c.RetryStatuses)
	}
	if hasRetryStatus(c.RetryStatuses, 500) {
		t.Fatalf("显式状态码集合应完全替换默认集合: %v", c.RetryStatuses)
	}

	off := buildClient(modelconfig.ResolvedConfig{
		BaseURL: "https://example.com/v1", APIKey: "sk-test", ProviderName: "p",
		RetryCount: -1,
	})
	if off.RetryCount >= 0 {
		t.Fatalf("retryCount:-1 应禁用重试, 实际 RetryCount=%d", off.RetryCount)
	}
}

func hasRetryStatus(list []int, code int) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}
