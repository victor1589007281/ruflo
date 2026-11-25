package api

import (
	"testing"
	"time"
)

// 上游 (opencode 免费模型) 在额度耗尽时会回 "日配额重置" 量级的 Retry-After
// (实测 62860s)。等待必须收敛到 MaxHonorRetryAfter, 否则一次 429 会让整段
// agent 调用睡到必然超时 —— 而实际的 RPM 桶几十秒就回补。
func TestHonorRetryAfterCapsDailyQuotaReset(t *testing.T) {
	cases := []struct {
		in   float64
		want time.Duration
	}{
		{-1, 0},
		{0, 0},
		{5, 5 * time.Second},
		{30, 30 * time.Second},
		{31, MaxHonorRetryAfter},
		{62860, MaxHonorRetryAfter},
	}
	for _, c := range cases {
		if got := HonorRetryAfter(c.in); got != c.want {
			t.Errorf("HonorRetryAfter(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

// 全局退避与单次等待必须用同一个上限: 否则 HTTP 客户端不再睡十几小时,
// 但所有请求仍会在 Guard 里排队到日配额重置。
func TestGuardPauseCappedForDailyQuotaReset(t *testing.T) {
	g := NewRateLimitGuard(GuardConfig{RPM: 50, MaxParallel: 2, MinParallel: 1})
	g.On429(62860)

	g.mu.Lock()
	until := g.pauseUntil
	g.mu.Unlock()

	if wait := time.Until(until); wait > MaxHonorRetryAfter {
		t.Fatalf("On429(62860) 暂停 %v, 超过上限 %v", wait, MaxHonorRetryAfter)
	}
}
