package search

import (
	"errors"
	"fmt"
)

// ErrorKind 是归一化后的错误类别。调度器按类别决定重试、熔断或降级。
type ErrorKind string

const (
	ErrTimeout     ErrorKind = "timeout"
	ErrRateLimited ErrorKind = "rate_limited"
	ErrBlocked     ErrorKind = "blocked"
	ErrUnavailable ErrorKind = "unavailable"
	ErrInvalid     ErrorKind = "invalid_input"
	ErrUnsupported ErrorKind = "unsupported"
	ErrNoProvider  ErrorKind = "no_provider"
)

// Error 是带 provider 与类别的检索错误。
type Error struct {
	Provider string
	Kind     ErrorKind
	Err      error
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Provider, e.Kind)
	}
	return fmt.Sprintf("%s: %s: %v", e.Provider, e.Kind, e.Err)
}

// Unwrap 支持 errors.Is/As。
func (e *Error) Unwrap() error { return e.Err }

// KindOf 提取错误类别; 非 *Error 时按 unavailable 处理。
func KindOf(err error) ErrorKind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return ErrUnavailable
}

// Wrap 构造归一化错误。
func Wrap(provider string, kind ErrorKind, err error) *Error {
	return &Error{Provider: provider, Kind: kind, Err: err}
}

// IsBlocked 判断是否被目标站拒绝 (403/CAPTCHA/挑战页)。
func IsBlocked(err error) bool { return KindOf(err) == ErrBlocked }
