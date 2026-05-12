# Skill: Mock Generation for Interfaces

## 场景 (When to use)
当需要为接口生成 mock 实现，用于单元测试中隔离依赖，推荐使用 `go.uber.org/mock` 或 `github.com/stretchr/testify/mock`。

## 代码模板 (Template)
```go
package store

import (
	"context"
	"testing"
)

// Repository is the interface to mock.
type Repository interface {
	GetUser(ctx context.Context, id string) (*User, error)
	SaveUser(ctx context.Context, user *User) error
}

type User struct {
	ID   string
	Name string
}

//go:generate go run go.uber.org/mock/mockgen -source=$GOFILE -destination=mock_store.go -package=store

// Manual mock example (if mockgen is unavailable)
type MockRepository struct {
	GetUserFunc  func(ctx context.Context, id string) (*User, error)
	SaveUserFunc func(ctx context.Context, user *User) error
}

func (m *MockRepository) GetUser(ctx context.Context, id string) (*User, error) {
	if m.GetUserFunc != nil {
		return m.GetUserFunc(ctx, id)
	}
	return nil, nil
}

func (m *MockRepository) SaveUser(ctx context.Context, user *User) error {
	if m.SaveUserFunc != nil {
		return m.SaveUserFunc(ctx, user)
	}
	return nil
}
```

## 反模式警告 (Anti-patterns)
- 不要为具体结构体生成 mock，只 mock 接口
- 不要在 mock 中实现真实业务逻辑，mock 应只返回预设值
- 不要忽略 mock 的调用次数验证，使用 `mock.AssertExpectations`

## 契约要求 (Contract Requirements)
- 被 mock 的必须是接口类型
- 接口方法应接受 `context.Context` 作为第一个参数

## 测试模板 (Test Template)
```go
func TestService_GetUser(t *testing.T) {
	mockRepo := &MockRepository{
		GetUserFunc: func(ctx context.Context, id string) (*User, error) {
			if id == "123" {
				return &User{ID: "123", Name: "Alice"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
	}

	// TODO: inject mockRepo into service and test
	user, err := mockRepo.GetUser(context.Background(), "123")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "Alice" {
		t.Fatalf("expected Alice, got %s", user.Name)
	}
}
```
