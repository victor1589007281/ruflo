package memory

import (
	"container/list"
	"sync"
	"time"
)

type cacheEntry struct {
	key       string
	value     any
	expiresAt time.Time
}

// LRUCache is a fixed-capacity cache with optional per-entry TTL.
type LRUCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[string]*list.Element
	order    *list.List
}

// NewLRUCache creates a cache. ttl <= 0 means entries do not expire by time (only LRU).
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

// Get returns a value and true if present and not expired.
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

// Set inserts or updates a key. Uses default TTL when configured.
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

// Delete removes a key.
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		ce := el.Value.(*cacheEntry)
		c.order.Remove(el)
		delete(c.items, ce.key)
	}
}
