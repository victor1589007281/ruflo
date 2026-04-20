package orchestrator

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// ErrorKind 将错误分为三个层级, 对应不同的处理策略。
//
//	致命 (Fatal)  → 立即停止, 不重试
//	瞬态 (Transient) → 值得等待和重试 (限流/网络/超时)
//	永久 (Permanent) → 需要修复逻辑后重试 (代码缺陷/验证失败)
type ErrorKind int

const (
	ErrorTransient ErrorKind = iota // 429/网络/超时 — 值得等待重试
	ErrorPermanent                  // 验证失败/代码质量 — 需要 LLM 或人工修复
	ErrorFatal                      // API Key 无效/配额耗尽 — 立即停止
)

func (k ErrorKind) String() string {
	switch k {
	case ErrorTransient:
		return "transient"
	case ErrorPermanent:
		return "permanent"
	case ErrorFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// ClassifyError 从 error 接口分类错误。
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return ErrorPermanent
	}
	return ClassifyErrorMsg(err.Error())
}

// ClassifyErrorMsg 从错误消息字符串分类错误。
//
// 匹配优先级: 致命 > 瞬态 > 永久
// 使用 strings.Contains 进行关键词匹配, 支持中英文模式。
func ClassifyErrorMsg(msg string) ErrorKind {
	if msg == "" {
		return ErrorPermanent
	}
	lower := strings.ToLower(msg)

	// 致命错误: API 认证/计费问题, 不可能通过重试解决
	fatalPatterns := []string{
		"invalid api key", "invalid_api_key", "authentication",
		"quota exceeded", "billing", "unauthorized", "forbidden",
	}
	for _, p := range fatalPatterns {
		if strings.Contains(lower, p) {
			return ErrorFatal
		}
	}

	// 瞬态错误: 网络/限流/过载问题, 等待后通常会恢复
	transientPatterns := []string{
		"429", "rate", "throttl", "限流", "频率",
		"timeout", "deadline exceeded", "超时",
		"connection refused", "connection reset", "网络错误",
		"503", "529", "overloaded", "过载",
		"temporary", "unavailable", "econnreset",
		"broken pipe", "eof",
	}
	for _, p := range transientPatterns {
		if strings.Contains(lower, p) {
			return ErrorTransient
		}
	}

	return ErrorPermanent
}

// RetryPolicy 定义任务的重试行为。
type RetryPolicy struct {
	MaxRetries    int           // 永久错误最大重试次数
	MaxTransient  int           // 瞬态错误额外重试次数 (总次数 = MaxRetries + MaxTransient)
	BaseDelay     time.Duration // 永久错误指数退避的基础延迟
	MaxDelay      time.Duration // 最大延迟上限
	TransientBase time.Duration // 瞬态错误基础延迟 (通常更长, 给限流恢复时间)
}

// DefaultRetryPolicy 返回生产环境推荐默认值。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:    2,
		MaxTransient:  5,
		BaseDelay:     2 * time.Second,
		MaxDelay:      2 * time.Minute,
		TransientBase: 10 * time.Second,
	}
}

// RetryDecision 重试决策结果。
type RetryDecision struct {
	ShouldRetry bool          // 是否应该重试
	Delay       time.Duration // 建议的等待时间
	Kind        ErrorKind     // 错误类型
}

// ShouldRetry 根据错误消息和已尝试次数决定是否重试。
//
// 决策逻辑:
//
//	致命 → 永不重试
//	瞬态 → 允许 MaxRetries + MaxTransient 次重试, 使用 TransientBase 退避
//	永久 → 允许 MaxRetries 次重试, 使用 BaseDelay 退避
func (p RetryPolicy) ShouldRetry(errMsg string, attempt int) RetryDecision {
	kind := ClassifyErrorMsg(errMsg)
	switch kind {
	case ErrorFatal:
		return RetryDecision{ShouldRetry: false, Kind: kind}
	case ErrorTransient:
		if attempt < p.MaxRetries+p.MaxTransient {
			return RetryDecision{
				ShouldRetry: true,
				Delay:       backoffWithJitter(p.TransientBase, attempt, p.MaxDelay),
				Kind:        kind,
			}
		}
		return RetryDecision{ShouldRetry: false, Kind: kind}
	default: // ErrorPermanent
		if attempt < p.MaxRetries {
			return RetryDecision{
				ShouldRetry: true,
				Delay:       backoffWithJitter(p.BaseDelay, attempt, p.MaxDelay),
				Kind:        kind,
			}
		}
		return RetryDecision{ShouldRetry: false, Kind: kind}
	}
}

// backoffWithJitter 计算带抖动的指数退避延迟。
//
// 算法: Full Jitter (AWS 推荐的退避策略)
//
//	delay = base * 2^attempt
//	jitter = random(0, min(delay, maxDelay))
//
// Full Jitter 比 Equal Jitter 更能分散重试风暴, 适合多客户端竞争场景。
func backoffWithJitter(base time.Duration, attempt int, maxDelay time.Duration) time.Duration {
	exp := math.Pow(2, float64(attempt))
	delay := time.Duration(float64(base) * exp)
	if delay > maxDelay {
		delay = maxDelay
	}
	jitter := time.Duration(rand.Float64() * float64(delay))
	return jitter
}

// TaskError 带分类和任务上下文的错误包装。
type TaskError struct {
	TaskID  string    // 任务 ID
	Kind    ErrorKind // 错误类型
	Message string    // 原始错误消息
	Attempt int       // 当前尝试次数
}

func (e *TaskError) Error() string {
	return fmt.Sprintf("[%s] 任务 %s (第 %d 次尝试): %s", e.Kind, e.TaskID, e.Attempt, e.Message)
}

// NewTaskError 从 error 创建带分类的 TaskError。
func NewTaskError(taskID string, err error, attempt int) *TaskError {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return &TaskError{
		TaskID:  taskID,
		Kind:    ClassifyErrorMsg(msg),
		Message: msg,
		Attempt: attempt,
	}
}
