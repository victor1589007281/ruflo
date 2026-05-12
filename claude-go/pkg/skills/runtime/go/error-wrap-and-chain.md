# Skill: Error Wrap and Chain

## 场景 (When to use)
当需要在错误传播过程中添加上下文信息，同时保留原始错误供调用方检查（如使用 `errors.Is` / `errors.As`）。

## 代码模板 (Template)
```go
package errors

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound    = errors.New("resource not found")
	ErrInvalidInput = errors.New("invalid input")
	ErrInternal    = errors.New("internal error")
)

type AppError struct {
	Op   string
	Err  error
	Code int
}

func (e *AppError) Error() string {
	return fmt.Sprintf("%s: %v (code=%d)", e.Op, e.Err, e.Code)
}

func (e *AppError) Unwrap() error {
	return e.Err
}

func NewAppError(op string, code int, err error) error {
	return &AppError{Op: op, Code: code, Err: err}
}

func Wrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", msg, err)
}

func ExampleUsage() {
	// TODO: replace with actual repository call
	_, err := fetchUser("123")
	if err != nil {
		wrapped := Wrap(err, "failed to load user")
		if errors.Is(wrapped, ErrNotFound) {
			// TODO: handle not found
		}
		// TODO: return or log wrapped error
		_ = wrapped
	}
}

func fetchUser(id string) (string, error) {
	// TODO: implement fetch logic
	return "", ErrNotFound
}
```

## 反模式警告 (Anti-patterns)
- 不要使用 `%v` 包装错误，否则调用方无法使用 `errors.Is`
- 不要丢弃原始错误，始终通过 `%w` 或自定义 `Unwrap` 保留
- 不要创建过多层级的错误嵌套，通常 2-3 层足够

## 契约要求 (Contract Requirements)
- 自定义错误类型必须实现 `error` 接口
- 如需支持 `errors.Is`/`errors.As`，必须实现 `Unwrap() error`

## 测试模板 (Test Template)
```go
func TestWrap(t *testing.T) {
	base := ErrNotFound
	wrapped := Wrap(base, "fetch failed")

	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("expected errors.Is to match base error")
	}

	var appErr *AppError
	if errors.As(Wrap(NewAppError("db", 500, base), "service"), &appErr) {
		if appErr.Code != 500 {
			t.Fatalf("expected code 500, got %d", appErr.Code)
		}
	} else {
		t.Fatal("expected errors.As to match AppError")
	}
}
```
