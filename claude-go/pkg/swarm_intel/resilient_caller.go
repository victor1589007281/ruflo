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
	deadlineRetryBase time.Duration

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
	// 首token超时分离: FirstTokenTTL 用于等待模型首token, OverallTTL 用于单次调用整体上限。
	// 若 FirstTokenTTL <= 0, 退回到 PerCallTTL。
	FirstTokenTTL time.Duration
	OverallTTL    time.Duration
	// DeadlineRetryBase 针对 context deadline exceeded / timeout 的基础退避 (默认 10s)
	DeadlineRetryBase time.Duration
}

// DefaultResilientConfig 返回弹性调用层的默认配置 (3 次重试、2s 起指数退避、180s 首 token、5 次熔断)。
func DefaultResilientConfig() ResilientCallerConfig {
	return ResilientCallerConfig{
		MaxRetries:        3,
		BaseDelay:         2 * time.Second,
		MaxDelay:          30 * time.Second,
		PerCallTTL:        180 * time.Second, // 首token等待: 180s (dashscope 首token慢)
		FirstTokenTTL:     180 * time.Second,
		OverallTTL:        600 * time.Second,
		CircuitThreshold:  5,
		DeadlineRetryBase: 10 * time.Second,
	}
}

// NewResilientCaller 构造弹性调用层, 把各字段的零值补齐为默认值, 并保证 notify 非 nil。
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
		cfg.PerCallTTL = 180 * time.Second
	}
	if cfg.FirstTokenTTL <= 0 {
		cfg.FirstTokenTTL = cfg.PerCallTTL
	}
	if cfg.OverallTTL <= 0 {
		cfg.OverallTTL = cfg.PerCallTTL
	}
	if cfg.CircuitThreshold <= 0 {
		cfg.CircuitThreshold = 5
	}
	if cfg.DeadlineRetryBase <= 0 {
		cfg.DeadlineRetryBase = 10 * time.Second
	}
	notify := cfg.Notify
	if notify == nil {
		notify = func(_, _ string) {}
	}
	return &ResilientCaller{
		inner:             inner,
		maxRetries:        cfg.MaxRetries,
		baseDelay:         cfg.BaseDelay,
		maxDelay:          cfg.MaxDelay,
		perCallTTL:        cfg.PerCallTTL,
		deadlineRetryBase: cfg.DeadlineRetryBase,
		circuitThreshold:  cfg.CircuitThreshold,
		notify:            notify,
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
			delay := rc.computeDelay(attempt, errClass, err)
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

// classifyError 把错误分成 可重试 / 过载 / 致命 三类, 决定退避基数与是否继续重试。
//
// 算法解读：
//
//	原理：只看错误文本里的状态码与关键词做分类, 不依赖具体 provider 的错误类型。
//	关键不变量：default 落在**可重试**而不是致命 —— 分类器的职责是别把瞬态错误判成永久错误;
//	      反过来 (把永久错误当瞬态) 的代价只是多几次退避重试, 由 maxRetries 兜底。
//	代价与边界：纯字符串匹配, 依赖 provider 把状态码写进错误文本; 认不出时按可重试处理。
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

// computeDelay 计算第 attempt 次重试的退避时长: 基数 × 2^attempt, 封顶 maxDelay, 再叠加 ±25% 抖动。
//
// 算法解读：
//
//	原理：指数退避 + 抖动。按错误类别选基数, 按尝试次数指数放大, 再叠加抖动打散同一时刻的
//	      重试 —— 多个 worker 同时被 429 后会同步重试, 抖动是唯一的解药。
//	关键不变量：退避恒有下界 (基数 > 0) 且恒有上界 —— 无上界会让第 7 次重试等到几分钟之后,
//	      而那时上层早已超时。
//	代价与边界：抖动用全局 rand, 并发下无需同步; 基数按类分档 (过载 3 倍、deadline 用专门
//	      基数), 类别判错只会让退避偏长或偏短, 不影响正确性。
//	设计权衡：没有引入退避表/令牌桶 —— 调用方 (swarm 各 agent) 的并发度本身不高, 简单公式
//	      比"精确的速率控制"更容易解释和调参。
func (rc *ResilientCaller) computeDelay(attempt int, class errClass, err error) time.Duration {
	base := rc.baseDelay
	if class == errClassOverload {
		base = rc.baseDelay * 3 // 429/503 用更长的基础退避
	}
	// 针对 deadline exceeded / timeout 使用专门的退避基数
	if err != nil && isDeadlineError(err) {
		base = rc.deadlineRetryBase
		if base <= 0 {
			base = 10 * time.Second
		}
	}
	// 先封顶再抖动: 抖动施加在封顶后的值上, 因此实际等待最长可达 maxDelay 的 1.25 倍。

	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)))
	if delay > rc.maxDelay {
		delay = rc.maxDelay
	}

	// 加 jitter (±25%)
	jitter := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	return jitter
}

// isDeadlineError 判断错误文本里是否含 deadline/timeout 字样。
func isDeadlineError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadline") || strings.Contains(msg, "timeout")
}

// --- 熔断器 ---

// isCircuitOpen 读熔断器状态: 冷却期已过则半开复位, 允许一次试探; 否则继续拒绝。
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

// recordSuccess 复位连续失败计数并关闭熔断器。
func (rc *ResilientCaller) recordSuccess() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.consecutiveFails = 0
	rc.circuitOpen = false
}

// recordFailure 累加连续失败; 达到阈值则打开熔断器并记 30s 冷却。
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

// truncateErr 把错误文本截到 80 字符以内, 供通知展示。
func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
