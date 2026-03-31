package shared

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Retry runs fn up to maxAttempts times with fixed delay between failures.
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

// RetryWithBackoff retries fn with exponentially increasing delay (base * 2^i).
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

// CircuitBreakerState implements a three-state breaker.
type CircuitBreakerState struct {
	mu        sync.Mutex
	threshold int
	timeout   time.Duration
	failures  int
	openUntil time.Time
	state     string
}

// CircuitBreaker constructs a breaker that opens after threshold consecutive failures.
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

// State returns "closed", "open", or "half-open".
func (cb *CircuitBreakerState) State() string {
	if cb == nil {
		return ""
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.reconcileLocked()
	return cb.state
}

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

// Call runs fn when the breaker allows it; failures count toward opening.
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
