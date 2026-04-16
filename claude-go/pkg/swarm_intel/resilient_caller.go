package swarm_intel

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ═══════════════════════════════════════════════════════════════════
// C. ResilientCaller — LLM 调用弹性层
//
// 设计参考:
//   - Qwen-code retry.ts: 7 次指数退避, 429/5xx 分轨, retry budget 隔离
//   - DeepSeek Rate Limit: keep-alive 心跳, 10min 无推理断连
//   - Anthropic SDK: 429/5xx 退避 + 幂等键
//   - 通用: 熔断器(Circuit Breaker) + 令牌桶(Token Bucket)
//
// 核心: 包装 LLMClient, 自动处理重试/退避/熔断/超时,
// 使上层业务代码无需关心 429/503 等瞬态错误。
// ═══════════════════════════════════════════════════════════════════

type ResilientCaller struct {
	inner      LLMClient
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	perCallTTL time.Duration

	// 熔断器
	mu               sync.Mutex
	consecutiveFails int
	circuitOpen      bool
	circuitOpenUntil time.Time
	circuitThreshold int

	// 统计
	totalCalls   atomic.Int64
	totalRetries atomic.Int64
	totalFails   atomic.Int64
	circuitTrips atomic.Int64

	notify NotifyFunc
}

type ResilientCallerConfig struct {
	MaxRetries       int
	BaseDelay        time.Duration
	MaxDelay         time.Duration
	PerCallTTL       time.Duration
	CircuitThreshold int
	Notify           NotifyFunc
}

func DefaultResilientConfig() ResilientCallerConfig {
	return ResilientCallerConfig{
		MaxRetries:       3,
		BaseDelay:        2 * time.Second,
		MaxDelay:         30 * time.Second,
		PerCallTTL:       90 * time.Second,
		CircuitThreshold: 5,
	}
}

func NewResilientCaller(inner LLMClient, cfg ResilientCallerConfig) *ResilientCaller {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 2 * time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 30 * time.Second
	}
	if cfg.PerCallTTL <= 0 {
		cfg.PerCallTTL = 90 * time.Second
	}
	if cfg.CircuitThreshold <= 0 {
		cfg.CircuitThreshold = 5
	}
	notify := cfg.Notify
	if notify == nil {
		notify = func(_, _ string) {}
	}
	return &ResilientCaller{
		inner:            inner,
		maxRetries:       cfg.MaxRetries,
		baseDelay:        cfg.BaseDelay,
		maxDelay:         cfg.MaxDelay,
		perCallTTL:       cfg.PerCallTTL,
		circuitThreshold: cfg.CircuitThreshold,
		notify:           notify,
	}
}

// Call 带重试/退避/熔断的 LLM 调用。
func (rc *ResilientCaller) Call(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	rc.totalCalls.Add(1)

	if rc.isCircuitOpen() {
		rc.notify("", "🔴 熔断器开启, 跳过 LLM 调用")
		return "", fmt.Errorf("circuit breaker open: too many consecutive failures")
	}

	var lastErr error
	for attempt := 0; attempt <= rc.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		callCtx, callCancel := context.WithTimeout(ctx, rc.perCallTTL)
		resp, err := rc.inner.SimpleComplete(callCtx, systemPrompt, userPrompt)
		callCancel()

		if err == nil {
			rc.recordSuccess()
			return resp, nil
		}

		lastErr = err
		errClass := classifyError(err)

		if errClass == errClassFatal {
			rc.recordFailure()
			return "", fmt.Errorf("non-retryable error: %w", err)
		}

		if attempt < rc.maxRetries {
			rc.totalRetries.Add(1)
			delay := rc.computeDelay(attempt, errClass)
			rc.notify("", fmt.Sprintf("🔄 LLM 调用失败(尝试 %d/%d): %s, %.1fs 后重试",
				attempt+1, rc.maxRetries+1, truncateErr(err), delay.Seconds()))

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}

	rc.recordFailure()
	rc.totalFails.Add(1)
	return "", fmt.Errorf("all %d attempts failed, last: %w", rc.maxRetries+1, lastErr)
}

// SimpleComplete 实现 LLMClient 接口, 使 ResilientCaller 可直接替换 LLMClient。
func (rc *ResilientCaller) SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return rc.Call(ctx, systemPrompt, userPrompt)
}

// Stats 返回调用统计。
func (rc *ResilientCaller) Stats() map[string]int64 {
	return map[string]int64{
		"total_calls":   rc.totalCalls.Load(),
		"total_retries": rc.totalRetries.Load(),
		"total_fails":   rc.totalFails.Load(),
		"circuit_trips": rc.circuitTrips.Load(),
	}
}

// --- 错误分类 ---

type errClass int

const (
	errClassRetryable errClass = iota // 429, 500, timeout
	errClassOverload                  // 503, 过载
	errClassFatal                     // 400, 401, 403
)

func classifyError(err error) errClass {
	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(msg, "429") || strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "throttl") || strings.Contains(lower, "quota"):
		return errClassOverload
	case strings.Contains(msg, "503") || strings.Contains(lower, "overload") ||
		strings.Contains(lower, "unavailable"):
		return errClassOverload
	case strings.Contains(msg, "500") || strings.Contains(lower, "internal"):
		return errClassRetryable
	case strings.Contains(msg, "400") || strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") || strings.Contains(lower, "invalid"):
		return errClassFatal
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		return errClassRetryable
	default:
		return errClassRetryable
	}
}

func (rc *ResilientCaller) computeDelay(attempt int, class errClass) time.Duration {
	base := rc.baseDelay
	if class == errClassOverload {
		base = rc.baseDelay * 3 // 429/503 用更长的基础退避
	}

	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)))
	if delay > rc.maxDelay {
		delay = rc.maxDelay
	}

	// 加 jitter (±25%)
	jitter := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	return jitter
}

// --- 熔断器 ---

func (rc *ResilientCaller) isCircuitOpen() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if !rc.circuitOpen {
		return false
	}

	// 半开: 如果冷却时间已过, 允许一次试探
	if time.Now().After(rc.circuitOpenUntil) {
		rc.circuitOpen = false
		rc.consecutiveFails = 0
		return false
	}
	return true
}

func (rc *ResilientCaller) recordSuccess() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.consecutiveFails = 0
	rc.circuitOpen = false
}

func (rc *ResilientCaller) recordFailure() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.consecutiveFails++
	if rc.consecutiveFails >= rc.circuitThreshold && !rc.circuitOpen {
		rc.circuitOpen = true
		rc.circuitOpenUntil = time.Now().Add(30 * time.Second)
		rc.circuitTrips.Add(1)
	}
}

func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
