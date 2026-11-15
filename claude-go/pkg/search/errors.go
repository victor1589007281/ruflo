package search

// errors.go —— 检索错误的归一化分类。
//
// provider 的原始错误形态各异 (HTTP 状态、超时、挑战页、JSON 解析失败),
// 调度器只关心"该重试、该熔断还是该降级", 因此统一收敛成 ErrorKind:
// timeout / rate_limited / blocked / unavailable / not_found / invalid_input /
// unsupported / no_provider。KindOf 用 errors.As 穿透 Wrap 包装, 不依赖错误
// 字符串匹配。
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
	// ErrNotFound 是目标资源已死 (404/410)。与 ErrInvalid 分开: 后者是
	// 请求本身非法 (SSRF/协议/参数校验), 绝不可换个出口重试; 前者值得回退
	// 归档快照 (Wayback), 但不配用旧缓存伪装成功。
	ErrNotFound    ErrorKind = "not_found"
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
