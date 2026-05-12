# Skill: Channel Pipeline Pattern

## 场景 (When to use)
当需要将数据处理流程拆分为多个阶段，每个阶段通过 channel 连接，实现并发流水线处理，提高吞吐量。

## 代码模板 (Template)
```go
package pipeline

import (
	"context"
)

type Stage func(ctx context.Context, in <-chan int) <-chan int

func Pipeline(ctx context.Context, source <-chan int, stages ...Stage) <-chan int {
	out := source
	for _, stage := range stages {
		out = stage(ctx, out)
	}
	return out
}

func Generate(ctx context.Context, nums ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, n := range nums {
			select {
			case out <- n:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func Multiply(ctx context.Context, factor int) Stage {
	return func(ctx context.Context, in <-chan int) <-chan int {
		out := make(chan int)
		go func() {
			defer close(out)
			for n := range in {
				select {
				case out <- n * factor:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out
	}
}

func Filter(ctx context.Context, predicate func(int) bool) Stage {
	return func(ctx context.Context, in <-chan int) <-chan int {
		out := make(chan int)
		go func() {
			defer close(out)
			for n := range in {
				if !predicate(n) {
					continue
				}
				select {
				case out <- n:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out
	}
}
```

## 反模式警告 (Anti-patterns)
- 不要在发送方和接收方都关闭 channel，只应由发送方关闭
- 不要在 stage 中泄漏 goroutine，确保 `ctx.Done()` 能退出
- 不要创建过深的 pipeline，通常 3-5 个 stage 为宜

## 契约要求 (Contract Requirements)
- 每个 `Stage` 必须关闭输出 channel
- 每个 `Stage` 必须响应 `ctx.Done()`

## 测试模板 (Test Template)
```go
func TestPipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := Generate(ctx, 1, 2, 3, 4, 5)
	out := Pipeline(ctx, src,
		Multiply(ctx, 2),
		Filter(ctx, func(n int) bool { return n > 4 }),
	)

	var result []int
	for n := range out {
		result = append(result, n)
	}

	expected := []int{6, 8, 10}
	if len(result) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, result)
	}
	for i := range expected {
		if result[i] != expected[i] {
			t.Fatalf("expected %v, got %v", expected, result)
		}
	}
}
```
