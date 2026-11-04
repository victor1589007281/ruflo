package api

import (
	"testing"
	"time"
)

// TestRetryableStatusDefaultAndConfigured 钉住两件事:
//  1. 未配置 RetryStatuses 时 400 **不**重试 (api.Client 的历史默认);
//  2. 显式配置集合后**完全替换**默认 —— 否则"400 可配置重试"会被历史默认
//     的 5xx 混在一起, 测不出配置到底有没有生效。
func TestRetryableStatusDefaultAndConfigured(t *testing.T) {
	c := &Client{}
	for _, code := range []int{429, 503, 529, 500, 502, 504} {
		if !c.retryableStatus(code) {
			t.Fatalf("默认集合应包含 %d", code)
		}
	}
	if c.retryableStatus(400) {
		t.Fatal("默认集合不应包含 400 (旧配置行为必须保持不变)")
	}
	if c.retryableStatus(401) || c.retryableStatus(403) {
		t.Fatal("默认集合不应包含 401/403")
	}

	c.RetryStatuses = []int{400, 502}
	if !c.retryableStatus(400) {
		t.Fatal("配置 400 后应可重试")
	}
	if !c.retryableStatus(502) {
		t.Fatal("配置 502 后应可重试")
	}
	if c.retryableStatus(429) {
		t.Fatal("显式集合应完全替换默认: 429 未列出就不该重试")
	}
	if c.retryableStatus(401) {
		t.Fatal("显式集合里没有 401")
	}
}

// TestMaxRetriesNegativeDisables 钉住"显式禁用"这个语义: 0 与不配置同为默认值,
// 只有负数才是关闭。
func TestMaxRetriesNegativeDisables(t *testing.T) {
	if got := (&Client{RetryCount: -1}).maxRetries(); got != 0 {
		t.Fatalf("RetryCount=-1 应禁用重试, got %d", got)
	}
	if got := (&Client{RetryCount: 0}).maxRetries(); got != 4 {
		t.Fatalf("RetryCount=0 应沿用默认 4, got %d", got)
	}
	if got := (&Client{RetryCount: 7}).maxRetries(); got != 7 {
		t.Fatalf("RetryCount=7 应原样生效, got %d", got)
	}
}

// TestRetryPolicyFromConfig 校验配置 -> 策略的映射:
// 0 用网关默认, -1 禁用, 空状态码集合按默认处理。
func TestRetryPolicyFromConfig(t *testing.T) {
	p := RetryPolicyFromConfig(0, 0, 0, nil)
	if p.MaxRetries != 2 || p.Base != time.Second || p.Max != 8*time.Second {
		t.Fatalf("默认策略错误: %+v", p)
	}
	if !p.Retryable(400) {
		t.Fatal("网关默认策略必须包含 400")
	}

	if got := RetryPolicyFromConfig(-1, 0, 0, nil); got.MaxRetries != 0 {
		t.Fatalf("retryCount=-1 应禁用, got %d", got.MaxRetries)
	}
	if got := RetryPolicyFromConfig(5, 2, 30, nil); got.MaxRetries != 5 ||
		got.Base != 2*time.Second || got.Max != 30*time.Second {
		t.Fatalf("显式配置未生效: %+v", got)
	}
	// 显式空数组按默认处理 (更安全: 写 "retryStatuses": [] 不应意外关掉全部重试)。
	if got := RetryPolicyFromConfig(0, 0, 0, []int{}); !got.Retryable(400) {
		t.Fatal("空数组应回落默认集合")
	}
	if got := RetryPolicyFromConfig(0, 0, 0, []int{418}); got.Retryable(400) || !got.Retryable(418) {
		t.Fatalf("显式集合应完全替换: %+v", got)
	}
}

// TestRetryPolicyBackoff 校验指数退避、429 基数放大与上限封顶。
func TestRetryPolicyBackoff(t *testing.T) {
	p := RetryPolicy{Base: 100 * time.Millisecond, Max: 10 * time.Second}
	if got := p.Backoff(0, 500); got != 100*time.Millisecond {
		t.Fatalf("attempt0 应为 base, got %v", got)
	}
	if got := p.Backoff(1, 500); got != 200*time.Millisecond {
		t.Fatalf("attempt1 应为 2*base, got %v", got)
	}
	if got := p.Backoff(0, 429); got != 300*time.Millisecond {
		t.Fatalf("429 基数应放大 3 倍, got %v", got)
	}
	if got := p.Backoff(0, 503); got != 300*time.Millisecond {
		t.Fatalf("503 基数应放大 3 倍, got %v", got)
	}

	capped := RetryPolicy{Base: time.Second, Max: 2 * time.Second}
	if got := capped.Backoff(10, 500); got != 2*time.Second {
		t.Fatalf("退避应封顶在 Max, got %v", got)
	}
}

// TestRetryPolicyDelayJitter 校验 ±25% 抖动范围与 Retry-After 优先。
func TestRetryPolicyDelayJitter(t *testing.T) {
	p := RetryPolicy{Base: time.Second, Max: 8 * time.Second}
	lo, hi := 750*time.Millisecond, 1250*time.Millisecond
	for i := 0; i < 200; i++ {
		got := p.Delay(0, 500, 0)
		if got < lo || got > hi {
			t.Fatalf("jitter 越界: got %v, want [%v, %v]", got, lo, hi)
		}
	}
	// Retry-After 优先于计算出的退避 (且不再被抖动到它之前)。
	if got := p.Delay(0, 500, 5*time.Second); got != 5*time.Second {
		t.Fatalf("应尊重 Retry-After=5s, got %v", got)
	}
}

// TestClientRetryDelay429AndCap 校验现有 Client.retryDelay 的两个关键性质。
func TestClientRetryDelay429AndCap(t *testing.T) {
	c := &Client{RetryBase: 100 * time.Millisecond, RetryMax: 10 * time.Second}
	// 普通 5xx: 100ms ±25%
	for i := 0; i < 100; i++ {
		d := c.retryDelay(0, 500, nil)
		if d < 75*time.Millisecond || d > 125*time.Millisecond {
			t.Fatalf("5xx jitter 越界: %v", d)
		}
	}
	// 429: 3x 基数 = 300ms ±25%
	for i := 0; i < 100; i++ {
		d := c.retryDelay(0, 429, nil)
		if d < 225*time.Millisecond || d > 375*time.Millisecond {
			t.Fatalf("429 jitter 越界: %v", d)
		}
	}
	// 封顶: attempt 很大时退避停在 RetryMax, 抖动围绕它。
	c = &Client{RetryBase: time.Second, RetryMax: 2 * time.Second}
	for i := 0; i < 100; i++ {
		d := c.retryDelay(10, 500, nil)
		if d < 1500*time.Millisecond || d > 2500*time.Millisecond {
			t.Fatalf("封顶后退避应在 [1.5s, 2.5s], got %v", d)
		}
	}
}
