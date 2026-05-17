// error_classifier.go — 错误分类与重试策略 (G4)。
//
// 对标:
//   - Qwen 3.6 (qwen-code) retry.ts: 独立重试层, 7 次指数退避, 错误族隔离 budget
//   - Claude 4.6/4.7: 429/5xx/400 不同策略, PTL 走 reactive compact
//   - DeepSeek V3 API 限流: keep-alive 心跳 + 队列排队区分真失败
//
// 当前 engine.consecutiveErrors 不区分错误类型:
//   - 400 (逻辑错) 也计入预算, 消耗重试额度
//   - 429/503 与 5xx 使用同样的退避
//   - PTL 虽有专门分支, 但恢复后计数未重置
//
// 本组件按错误族分桶, 各族独立 budget + 独立 backoff 曲线。
package internal_hook

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// ErrorFamily 错误分类。
type ErrorFamily int

const (
	ErrFamilyUnknown    ErrorFamily = iota
	ErrFamilyNetwork                // 网络抖动 / connection reset
	ErrFamilyRateLimit              // 429
	ErrFamilyOverload               // 503
	ErrFamilyServer                 // 5xx (非 503)
	ErrFamilyTimeout                // context deadline / 408
	ErrFamilyBadRequest             // 400 / 422 — 不重试
	ErrFamilyPTL                    // prompt_too_long — 走 reactive compact
	ErrFamilyAuth                   // 401/403 — 不重试
)

// String 返回族名。
func (f ErrorFamily) String() string {
	switch f {
	case ErrFamilyNetwork:
		return "network"
	case ErrFamilyRateLimit:
		return "rate_limit"
	case ErrFamilyOverload:
		return "overload"
	case ErrFamilyServer:
		return "server"
	case ErrFamilyTimeout:
		return "timeout"
	case ErrFamilyBadRequest:
		return "bad_request"
	case ErrFamilyPTL:
		return "prompt_too_long"
	case ErrFamilyAuth:
		return "auth"
	default:
		return "unknown"
	}
}

// FamilyBudget 单族的重试预算与退避策略。
type FamilyBudget struct {
	MaxAttempts  int           // 最大重试次数 (0 = 不重试)
	BaseBackoff  time.Duration // 基础退避
	MaxBackoff   time.Duration // 退避上限
	Jitter       bool          // 是否加 jitter (±20%)
	UseCompact   bool          // 是否尝试 reactive compact (仅 PTL/Timeout)
}

// DefaultBudgets 返回各族默认预算。
func DefaultBudgets() map[ErrorFamily]FamilyBudget {
	return map[ErrorFamily]FamilyBudget{
		ErrFamilyNetwork:    {MaxAttempts: 3, BaseBackoff: 1 * time.Second, MaxBackoff: 8 * time.Second, Jitter: true},
		ErrFamilyRateLimit:  {MaxAttempts: 5, BaseBackoff: 2 * time.Second, MaxBackoff: 32 * time.Second, Jitter: true},
		ErrFamilyOverload:   {MaxAttempts: 3, BaseBackoff: 5 * time.Second, MaxBackoff: 30 * time.Second, Jitter: true},
		ErrFamilyServer:     {MaxAttempts: 3, BaseBackoff: 1 * time.Second, MaxBackoff: 10 * time.Second, Jitter: true},
		ErrFamilyTimeout:    {MaxAttempts: 2, BaseBackoff: 2 * time.Second, MaxBackoff: 5 * time.Second, UseCompact: true},
		ErrFamilyBadRequest: {MaxAttempts: 0},
		ErrFamilyAuth:       {MaxAttempts: 0},
		ErrFamilyPTL:        {MaxAttempts: 1, UseCompact: true},
		ErrFamilyUnknown:    {MaxAttempts: 2, BaseBackoff: 2 * time.Second, MaxBackoff: 10 * time.Second, Jitter: true},
	}
}

// ErrorClassifier 按族隔离的错误分类器。
type ErrorClassifier struct {
	mu       sync.Mutex
	budgets  map[ErrorFamily]FamilyBudget
	counters map[ErrorFamily]int
}

// NewErrorClassifier 构造带默认预算的分类器。
func NewErrorClassifier() *ErrorClassifier {
	return &ErrorClassifier{
		budgets:  DefaultBudgets(),
		counters: map[ErrorFamily]int{},
	}
}

// Classify 把 err 映射到错误族。
func (c *ErrorClassifier) Classify(err error) ErrorFamily {
	if err == nil {
		return ErrFamilyUnknown
	}

	// 直接类型匹配 (api 包的特定类型)
	var ptl *api.PromptTooLongError
	if errors.As(err, &ptl) {
		return ErrFamilyPTL
	}
	var over *api.OverloadedError
	if errors.As(err, &over) {
		return ErrFamilyOverload
	}

	// context 错误
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrFamilyTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ErrFamilyUnknown // 取消一般由上层处理, 这里不重试
	}

	// 字符串启发式 — 适配不同 API 的错误文案
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests"):
		return ErrFamilyRateLimit
	case strings.Contains(msg, "503") || strings.Contains(msg, "overloaded"):
		return ErrFamilyOverload
	case strings.Contains(msg, "408") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return ErrFamilyTimeout
	case strings.Contains(msg, "400") || strings.Contains(msg, "bad request") || strings.Contains(msg, "invalid request") || strings.Contains(msg, "422"):
		return ErrFamilyBadRequest
	case strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden"):
		return ErrFamilyAuth
	case strings.Contains(msg, "prompt_too_long") || strings.Contains(msg, "prompt is too long") || strings.Contains(msg, "context window"):
		return ErrFamilyPTL
	case strings.Contains(msg, "500") || strings.Contains(msg, "502") || strings.Contains(msg, "504") || strings.Contains(msg, "internal server") || strings.Contains(msg, "bad gateway") || strings.Contains(msg, "gateway timeout"):
		return ErrFamilyServer
	case strings.Contains(msg, "connection") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "reset") || strings.Contains(msg, "eof"):
		return ErrFamilyNetwork
	}
	return ErrFamilyUnknown
}

// Observe 记录一次错误, 返回 (是否应放弃, 下次重试延迟, 是否建议压缩)。
//
// 调用语义:
//   - abort=true: 该族 budget 已耗尽 (或属于不重试族), 上层应立即返回
//   - abort=false: 上层应 time.Sleep(backoff) 后重试
//   - useCompact: PTL/Timeout 建议先做一次 reactive compact
func (c *ErrorClassifier) Observe(err error) (family ErrorFamily, abort bool, backoff time.Duration, useCompact bool) {
	if c == nil || err == nil {
		return ErrFamilyUnknown, true, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	family = c.Classify(err)
	budget, ok := c.budgets[family]
	if !ok {
		budget = c.budgets[ErrFamilyUnknown]
	}

	c.counters[family]++
	attempt := c.counters[family]

	if attempt > budget.MaxAttempts {
		return family, true, 0, budget.UseCompact
	}
	backoff = computeBackoff(budget, attempt)
	return family, false, backoff, budget.UseCompact
}

// ResetFamily 重置某族计数 (成功响应后调用)。
func (c *ErrorClassifier) ResetFamily(family ErrorFamily) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.counters, family)
	c.mu.Unlock()
}

// ResetAll 重置所有族计数 (成功响应后调用)。
func (c *ErrorClassifier) ResetAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.counters = map[ErrorFamily]int{}
	c.mu.Unlock()
}

// Counters 返回各族当前计数 (测试用)。
func (c *ErrorClassifier) Counters() map[ErrorFamily]int {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[ErrorFamily]int, len(c.counters))
	for k, v := range c.counters {
		out[k] = v
	}
	return out
}

// computeBackoff 指数退避 + 可选 jitter。
func computeBackoff(b FamilyBudget, attempt int) time.Duration {
	if b.BaseBackoff <= 0 {
		return 0
	}
	// exp: base * 2^(attempt-1)
	d := float64(b.BaseBackoff) * math.Pow(2, float64(attempt-1))
	if b.MaxBackoff > 0 && d > float64(b.MaxBackoff) {
		d = float64(b.MaxBackoff)
	}
	if b.Jitter {
		// ±20% 伪随机 (用 attempt 做种子, 不引入 math/rand 全局状态)
		jitter := d * 0.2 * (float64(attempt%7)/6.0 - 0.5) * 2
		d += jitter
		if d < 0 {
			d = float64(b.BaseBackoff)
		}
	}
	return time.Duration(d)
}
