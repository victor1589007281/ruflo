# Skill: Table-Driven Test Pattern

## 场景 (When to use)
当需要对同一函数的多组输入输出进行测试，使用表格驱动测试可以清晰组织用例，减少重复代码。

## 代码模板 (Template)
```go
package calc

import (
	"testing"
)

func Add(a, b int) int {
	// TODO: implement business logic
	return a + b
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name     string
		a, b     int
		expected int
	}{
		{"positive", 1, 2, 3},
		{"negative", -1, -2, -3},
		{"zero", 0, 5, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Add(tt.a, tt.b)
			if got != tt.expected {
				t.Errorf("Add(%d, %d) = %d; want %d", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}
```

## 反模式警告 (Anti-patterns)
- 不要在 `t.Run` 内部使用 `continue` 或 `break`，每个子测试应独立运行
- 不要在循环中捕获循环变量而不重新赋值（Go 1.22+ 已修复，但旧版本需注意）
- 不要忽略错误返回值，所有错误都必须断言

## 契约要求 (Contract Requirements)
- 测试函数签名必须匹配 `func TestXxx(t *testing.T)`
- 子测试命名应清晰描述场景

## 测试模板 (Test Template)
```go
func TestAdd_ErrorCase(t *testing.T) {
	tests := []struct {
		name    string
		a, b    int
		wantErr bool
	}{
		{"overflow", math.MaxInt64, 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SafeAdd(tt.a, tt.b)
			if (err != nil) != tt.wantErr {
				t.Errorf("SafeAdd(%d, %d) error = %v, wantErr %v", tt.a, tt.b, err, tt.wantErr)
			}
		})
	}
}

func SafeAdd(a, b int) (int, error) {
	// TODO: implement safe add with overflow check
	return a + b, nil
}
```
