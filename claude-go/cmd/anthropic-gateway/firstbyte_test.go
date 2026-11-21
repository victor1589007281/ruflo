package main

// 首字节闸门的端到端验证 (2026-11-18 事故: opencode:deepseek-v4.1-flash 透传请求
// 上游 3m35s 无任何字节, 网关静默等待, 会话看门狗靠"本地无活动"判死后 SIGKILL,
// 用户只看到 code=null)。这里钉住三件事:
//   1. 上游连响应头都不给 → 预算到点回 504, 不是无限挂起;
//   2. 上游给了 200 头但不吐正文 → 同样 504, **且没有先写出 200** (否则客户端看到的是断流);
//   3. 非流式请求与正常流式响应不受影响。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// withFirstByteTimeout 临时改全局旗标值, 测试结束恢复。
func withFirstByteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := *firstByteTimeout
	*firstByteTimeout = d
	t.Cleanup(func() { *firstByteTimeout = old })
}

func streamingBody() []byte {
	return []byte(`{"model":"p:m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestPassthroughFirstByteTimeoutNoHeaders: 上游既不给头也不给正文 → 504 + 明确错误类型。
func TestPassthroughFirstByteTimeoutNoHeaders(t *testing.T) {
	withFirstByteTimeout(t, 120*time.Millisecond)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 模拟上游静默: 既无头也无正文
	}))
	defer up.Close()
	defer close(release)

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	start := time.Now()
	rec := runPassthrough(g, streamingBody())
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("首字节超时应回 504, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "upstream_first_byte_timeout") {
		t.Fatalf("错误体应带 upstream_first_byte_timeout, got %s", body)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("预算 120ms 应在秒级内收尾, 实际 %v", elapsed)
	}
}

// TestPassthroughFirstByteTimeoutNoBody: 头已到但没有正文 → 也必须 504,
// 且响应头不能已经被写成 200 (WriteRecorder 会记录第一次 WriteHeader 的状态)。
func TestPassthroughFirstByteTimeoutNoBody(t *testing.T) {
	withFirstByteTimeout(t, 120*time.Millisecond)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // 头立刻发出, 正文继续沉默
		<-release
	}))
	defer up.Close()
	defer close(release)

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	rec := runPassthrough(g, streamingBody())

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("无正文应回 504, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_first_byte_timeout") {
		t.Fatalf("错误体应带 upstream_first_byte_timeout, got %s", rec.Body.String())
	}
}

// TestPassthroughFirstByteStreamsNormally: 正常流式上游仍逐字节透传 (闸门不误伤)。
func TestPassthroughFirstByteStreamsNormally(t *testing.T) {
	withFirstByteTimeout(t, 2*time.Second)
	const sse = "data: {\"type\":\"message_stop\"}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	rec := runPassthrough(g, streamingBody())
	if rec.Code != http.StatusOK || rec.Body.String() != sse {
		t.Fatalf("正常流式响应应原样透传, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestPassthroughFirstByteIgnoredForNonStream: 非流式请求没有"首字节"语义,
// 上游慢于预算也必须成功 (整包由 http.Client.Timeout 兜底)。
func TestPassthroughFirstByteIgnoredForNonStream(t *testing.T) {
	withFirstByteTimeout(t, 100*time.Millisecond)
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{})
	rec := runPassthrough(g, passthroughBody()) // 无 stream 字段
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("非流式请求不应被首字节闸门掐断, got %d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("非流式请求不应重试, 上游调用次数应为 1, got %d", n)
	}
}

// TestPassthroughFirstByteModelOverride: 模型级 firstTokenTimeoutSec 优先于旗标,
// 与 claude-go CLI 的 Client.FirstTokenTimeout 同口径。
func TestPassthroughFirstByteModelOverride(t *testing.T) {
	withFirstByteTimeout(t, 150*time.Second)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()

	g := retryTestGateway(t, up.URL, modelconfig.ModelConfig{FirstTokenTimeoutSec: 42})
	rc := g.resolver.ResolveAlias("p:m")
	if got := firstByteBudget(rc); got != 42*time.Second {
		t.Fatalf("模型级 firstTokenTimeoutSec 应优先, got %v", got)
	}

	// 未配置模型级值时用旗标默认值。
	rc.FirstTokenTimeoutSec = 0
	if got := firstByteBudget(rc); got != 150*time.Second {
		t.Fatalf("未配置时应回落到旗标, got %v", got)
	}
	// 旗标 0 = 关闭。
	*firstByteTimeout = 0
	if got := firstByteBudget(rc); got != 0 {
		t.Fatalf("旗标 0 应关闭闸门, got %v", got)
	}
}

// TestReadFirstChunkReturnsImmediately: 首块已到时不得等满预算。
func TestReadFirstChunkReturnsImmediately(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("hello"))
	}()
	defer pr.Close()
	buf := make([]byte, 16)
	n, err, timedOut := readFirstChunk(pr, buf, 2*time.Second)
	if timedOut || err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("应立即读到首块, got n=%d err=%v timedOut=%v", n, err, timedOut)
	}
}
