# Skill: sync.Map vs sync.RWMutex

## 场景 (When to use)
当需要在多个 goroutine 之间共享 map，根据访问模式选择 `sync.Map`（读多写少、键类型多样）或 `map + sync.RWMutex`（类型安全、频繁迭代）。

## 代码模板 (Template)
```go
package cache

import (
	"sync"
)

// TypedCache uses map + RWMutex for type safety.
type TypedCache struct {
	mu    sync.RWMutex
	items map[string]string
}

func NewTypedCache() *TypedCache {
	return &TypedCache{items: make(map[string]string)}
}

func (c *TypedCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.items[key]
	return val, ok
}

func (c *TypedCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = value
}

// GenericCache uses sync.Map for heterogeneous keys.
type GenericCache struct {
	m sync.Map
}

func (c *GenericCache) Get(key any) (any, bool) {
	return c.m.Load(key)
}

func (c *GenericCache) Set(key, value any) {
	c.m.Store(key, value)
}

func (c *GenericCache) Range(fn func(key, value any) bool) {
	c.m.Range(fn)
}
```

## 反模式警告 (Anti-patterns)
- 不要在 `sync.Map` 中存储需要类型断言的大量数据，会失去编译期类型检查
- 不要在持有 `RLock` 时执行写操作，会导致死锁
- 不要对普通 map 进行并发读写，即使读操作也会触发 data race

## 契约要求 (Contract Requirements)
- `TypedCache` 提供编译期类型安全
- `GenericCache` 适合键值类型不固定的场景

## 测试模板 (Test Template)
```go
func TestTypedCache_Concurrent(t *testing.T) {
	c := NewTypedCache()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", n)
			c.Set(key, "value")
			_, _ = c.Get(key)
		}(i)
	}

	wg.Wait()
}
```
