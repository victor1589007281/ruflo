# Skill: Worker Pool with Context

## 场景 (When to use)
当需要并发处理大量任务，同时需要控制并发数量、支持取消和超时，以及优雅地等待所有任务完成。

## 代码模板 (Template)
```go
package workerpool

import (
	"context"
	"fmt"
	"sync"
)

type Task func(ctx context.Context) error

type Pool struct {
	workers int
	tasks   chan Task
	wg      sync.WaitGroup
}

func NewPool(workers, queueSize int) *Pool {
	return &Pool{
		workers: workers,
		tasks:   make(chan Task, queueSize),
	}
}

func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(id int) {
			defer p.wg.Done()
			for task := range p.tasks {
				select {
				case <-ctx.Done():
					return
				default:
					if err := task(ctx); err != nil {
						fmt.Printf("worker %d error: %v\n", id, err)
					}
				}
			}
		}(i)
	}
}

func (p *Pool) Submit(task Task) {
	p.tasks <- task
}

func (p *Pool) Stop() {
	close(p.tasks)
	p.wg.Wait()
}
```

## 反模式警告 (Anti-patterns)
- 不要在没有 `ctx.Done()` 退出的 goroutine 中无限阻塞
- 不要在 `Stop()` 之前重复调用 `close(p.tasks)`
- 不要提交依赖共享可变状态且未加锁的任务

## 契约要求 (Contract Requirements)
- `Task` 类型签名必须接受 `context.Context` 并返回 `error`

## 测试模板 (Test Template)
```go
func TestPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewPool(2, 10)
	p.Start(ctx)

	var counter int64
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		p.Submit(func(ctx context.Context) error {
			mu.Lock()
			counter++
			mu.Unlock()
			return nil
		})
	}

	p.Stop()
	if counter != 5 {
		t.Fatalf("expected 5, got %d", counter)
	}
}
```
