package market

import (
	"errors"
	"fmt"
)

// ErrorKind 是归一化的数据面错误类别。
type ErrorKind string

const (
	ErrTimeout       ErrorKind = "timeout"
	ErrRateLimited   ErrorKind = "rate_limited"
	ErrBlocked       ErrorKind = "blocked"
	ErrUnavailable   ErrorKind = "unavailable"
	ErrUnsupported   ErrorKind = "unsupported"
	ErrUnresolved    ErrorKind = "symbol_unresolved"
	ErrNotAvailable  ErrorKind = "data_not_available"
	ErrInvalidPeriod ErrorKind = "invalid_period"
)

// Error 是带 provider 与类别的数据面错误。
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

// KindOf 提取错误类别; 非 *Error 按 unavailable 处理。
func KindOf(err error) ErrorKind {
	var me *Error
	if errors.As(err, &me) {
		return me.Kind
	}
	return ErrUnavailable
}

// Wrap 构造归一化错误。
func Wrap(provider string, kind ErrorKind, err error) *Error {
	return &Error{Provider: provider, Kind: kind, Err: err}
}
