# Skill: Rate Limiter (Token Bucket)

## 场景 (When to use)
当需要限制某个操作的执行频率，例如 API 请求限流、资源访问控制，使用令牌桶算法实现平滑限流。

## 代码模板 (Template)
```go
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type TokenBucket struct {
	capacity int64
	tokens   int64
	rate     time.Duration
	mu       sync.Mutex
	last     time.Time
}

func NewTokenBucket(capacity int64, rate time.Duration) *TokenBucket {
	return &TokenBucket{
		capacity: capacity,
		tokens:   capacity,
		rate:     rate,
		last:     time.Now(),
	}
}

func (tb *TokenBucket) Allow(ctx context.Context) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.last)
	tb.last = now

	tb.tokens += int64(elapsed / tb.rate)
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}

	if tb.tokens > 0 {
		tb.tokens--
		return true
	}
	return false
}

func (tb *TokenBucket) Wait(ctx context.Context) error {
	for {
		if tb.Allow(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tb.rate):
		}
	}
}
```

## 反模式警告 (Anti-patterns)
- 不要在热路径频繁创建新的 `time.After`，考虑使用 `time.NewTicker` 优化
- 不要在无锁场景下直接读写 `tokens`
- 不要设置过大的 `capacity` 导致突发流量失控

## 契约要求 (Contract Requirements)
- 无特殊接口要求

## 测试模板 (Test Template)
```go
func TestTokenBucket(t *testing.T) {
	tb := NewTokenBucket(2, 100*time.Millisecond)
	ctx := context.Background()

	if !tb.Allow(ctx) {
		t.Fatal("expected first allow to succeed")
	}
	if !tb.Allow(ctx) {
		t.Fatal("expected second allow to succeed")
	}
	if tb.Allow(ctx) {
		t.Fatal("expected third allow to fail")
	}

	time.Sleep(150 * time.Millisecond)
	if !tb.Allow(ctx) {
		t.Fatal("expected allow after refill to succeed")
	}
}
```
