// 弹性工具（resilience.go）
//
// 设计思路：
//   - 重试：对瞬时故障（网络抖动、锁竞争）用固定延迟或指数退避放大间隔，避免惊群与打满下游。
//   - 熔断：当下游持续失败时快速失败（fail-fast），给系统恢复时间；恢复后再试探性放行（半开）。
//
// 熔断状态机（CircuitBreakerState）：
//   - Closed：正常调用；连续失败计数 failures，达 threshold 则 → Open，并设置 openUntil = now+timeout。
//   - Open：在 openUntil 之前直接拒绝（Call 返回 circuit open）；超时后 reconcile → Half-Open。
//   - Half-Open：允许一次真实调用；若失败则立即回到 Open 并重置 openUntil；若成功则 failures=0 → Closed。
// 与经典熔断器一致：Closed→Open 由失败阈值触发；Open→Half-Open 由超时触发；Half-Open→Closed 由成功触发；Half-Open→Open 由失败触发。
package shared

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Retry 固定间隔重试：第 k 次失败后若 k+1<maxAttempts 且 delay>0，则 select ctx.Done 或 time.After(delay)。
// maxAttempts<1 规范为 1。返回最后一次错误；ctx 取消优先返回 ctx.Err()。
func Retry(ctx context.Context, maxAttempts int, delay time.Duration, fn func() error) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = fn()
		if last == nil {
			return nil
		}
		if attempt+1 < maxAttempts && delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return last
}

// RetryWithBackoff 指数退避：首次失败后等待 baseDelay，每次再失败等待时长翻倍（线性于 2^k），上限由尝试次数截断。
// 若 baseDelay=0 则连续立即重试（慎用）。成功返回 nil。
func RetryWithBackoff(ctx context.Context, maxAttempts int, baseDelay time.Duration, fn func() error) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var last error
	d := baseDelay
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = fn()
		if last == nil {
			return nil
		}
		if attempt+1 < maxAttempts && d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
			d *= 2
		}
	}
	return last
}

// CircuitBreakerState 三态熔断器内核字段；state 为字符串便于观测与日志。
type CircuitBreakerState struct {
	mu        sync.Mutex      // Call/State/reconcile 共用互斥锁（非 RWMutex，状态更新频繁）
	threshold int             // Closed 下连续失败达阈值则 Open；Half-Open 下任意一次失败立即 Open
	timeout   time.Duration   // Open 持续时间长度，决定 openUntil = now + timeout
	failures  int             // 连续失败计数；任意一次成功调用后清零
	openUntil time.Time       // Open 状态下拒绝截止时刻；过期后 reconcile 切至 Half-Open
	state     string          // "closed" | "open" | "half-open"
}

// CircuitBreaker 工厂：threshold<1 规范为 1；initial state 为 closed。
func CircuitBreaker(threshold int, timeout time.Duration) *CircuitBreakerState {
	if threshold < 1 {
		threshold = 1
	}
	return &CircuitBreakerState{
		threshold: threshold,
		timeout:   timeout,
		state:     "closed",
	}
}

// State 先 reconcileLocked（Open 超时则转 Half-Open），再返回 state 字符串快照。
func (cb *CircuitBreakerState) State() string {
	if cb == nil {
		return ""
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.reconcileLocked()
	return cb.state
}

// reconcileLocked 已持锁的时间驱动迁移：仅处理 Open→Half-Open（当 !openUntil.After(now)）；
// Half-Open/Closed 不变；未知字符串状态重置为 closed。
func (cb *CircuitBreakerState) reconcileLocked() {
	now := time.Now()
	switch cb.state {
	case "open":
		if !cb.openUntil.After(now) {
			cb.state = "half-open"
		}
	case "half-open", "closed":
		// noop
	default:
		cb.state = "closed"
	}
}

// Call 两阶段加锁：① 前置检查 Open 且仍在冷却期则快速失败；② Unlock 后执行 fn（避免长时间持锁）；③ Lock 后根据 err 更新状态。
// 成功：failures=0、state=closed、清空 openUntil。
// 失败：failures++；若当前 Half-Open → 立刻 Open 并设 openUntil；若 Closed 且 failures≥threshold → Open 并设 openUntil。
func (cb *CircuitBreakerState) Call(fn func() error) error {
	if cb == nil {
		return errors.New("shared: nil circuit breaker")
	}
	if fn == nil {
		return errors.New("shared: nil fn")
	}
	cb.mu.Lock()
	cb.reconcileLocked()
	now := time.Now()
	if cb.state == "open" && cb.openUntil.After(now) {
		cb.mu.Unlock()
		return errors.New("shared: circuit open")
	}
	cb.mu.Unlock()

	err := fn()

	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.reconcileLocked()
	now = time.Now()

	if err == nil {
		cb.failures = 0
		cb.state = "closed"
		cb.openUntil = time.Time{}
		return nil
	}

	cb.failures++
	if cb.state == "half-open" {
		cb.state = "open"
		cb.openUntil = now.Add(cb.timeout)
		return err
	}
	if cb.failures >= cb.threshold {
		cb.state = "open"
		cb.openUntil = now.Add(cb.timeout)
	}
	return err
}
