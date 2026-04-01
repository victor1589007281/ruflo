// LRU 热缓存（memory 包）：双向链表表示 LRU 顺序，map[string]*Element 指向链表结点，读写删除均摊 O(1)。
// 可配置条目 TTL：Set 时写入 expiresAt，Get/evictExpiredLocked 剔除过期；超容量时从尾部驱逐最久未访问键。
package memory

import (
	"container/list"
	"sync"
	"time"
)

// cacheEntry 为链表结点承载的业务数据：键、值与可选绝对过期时间。
type cacheEntry struct {
	key       string    // 缓存键
	value     any       // 任意缓存值（如 *api.MemoryEntry）
	expiresAt time.Time // 零值表示永不过期（仅 LRU）
}

// LRUCache 定容 LRU；ttl>0 时新写入条目带统一存活时长。
type LRUCache struct {
	mu       sync.Mutex               // 保护 map 与链表
	capacity int                      // 最大条目数
	ttl      time.Duration            // 全局默认 TTL，<=0 则仅按容量驱逐
	items    map[string]*list.Element // 键到链表结点指针
	order    *list.List               // 前端为最近使用，后端为最久未使用
}

// NewLRUCache 创建缓存；capacity 最小为 1。
func NewLRUCache(capacity int, ttl time.Duration) *LRUCache {
	if capacity < 1 {
		capacity = 1
	}
	return &LRUCache{
		capacity: capacity,
		ttl:      ttl,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// evictExpiredLocked 从链表尾部向头部扫描，移除已过期结点（调用方已持锁）。
func (c *LRUCache) evictExpiredLocked() {
	now := time.Now()
	for e := c.order.Back(); e != nil; {
		prev := e.Prev()
		ce := e.Value.(*cacheEntry)
		if !ce.expiresAt.IsZero() && !now.Before(ce.expiresAt) {
			c.order.Remove(e)
			delete(c.items, ce.key)
		}
		e = prev
	}
}

// Get 若命中且未过期则将结点移到表头并返回值；过期则摘除并返回 false。
func (c *LRUCache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	ce := el.Value.(*cacheEntry)
	if !ce.expiresAt.IsZero() && time.Now().After(ce.expiresAt) {
		c.order.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.order.MoveToFront(el)
	return ce.value, true
}

// Set 更新已存在键或 PushFront 新结点；超容量时反复移除 Back 直至满足上限。
func (c *LRUCache) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked()
	exp := time.Time{}
	if c.ttl > 0 {
		exp = time.Now().Add(c.ttl)
	}
	if el, ok := c.items[key]; ok {
		ce := el.Value.(*cacheEntry)
		ce.value = value
		ce.expiresAt = exp
		c.order.MoveToFront(el)
		return
	}
	ce := &cacheEntry{key: key, value: value, expiresAt: exp}
	el := c.order.PushFront(ce)
	c.items[key] = el
	for c.order.Len() > c.capacity {
		back := c.order.Back()
		if back == nil {
			break
		}
		old := back.Value.(*cacheEntry)
		delete(c.items, old.key)
		c.order.Remove(back)
	}
}

// Delete O(1) 从 map 与链表中摘除指定键。
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		ce := el.Value.(*cacheEntry)
		c.order.Remove(el)
		delete(c.items, ce.key)
	}
}
