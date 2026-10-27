package search

// fetchcache.go —— 规范化全文的进程内 LRU 与 singleflight。
//
// 缓存发生在 prompt 选段和 max_content_tokens 截断之前，因此同一 URL 的不同
// 问题、预算和 continuation 都复用一次网络抓取，而不会互相污染返回内容。
// 除条目数外还限制总字节数，避免长文档把进程内存顶到 GiB 级。

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	maxFetchCacheEntries = 128
	maxFetchCacheBytes   = 64 << 20
)

type documentCache struct {
	mu      sync.Mutex
	max     int
	maxByte int
	bytes   int
	ll      *list.List
	items   map[string]*list.Element
	now     func() time.Time
}

type documentCacheItem struct {
	key     string
	doc     *Document
	size    int
	expires time.Time
}

func newDocumentCache(max int) *documentCache {
	if max <= 0 || max > maxFetchCacheEntries {
		max = maxFetchCacheEntries
	}
	return &documentCache{max: max, maxByte: maxFetchCacheBytes, ll: list.New(), items: make(map[string]*list.Element), now: time.Now}
}

func cloneDocument(doc *Document) *Document {
	if doc == nil {
		return nil
	}
	out := *doc
	out.Evidence = append([]Evidence(nil), doc.Evidence...)
	return &out
}

func (c *documentCache) get(key string) (*Document, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	item := el.Value.(*documentCacheItem)
	if !c.now().Before(item.expires) {
		return nil, false // 过期项留给显式 stale-if-error，LRU 淘汰时再释放。
	}
	c.ll.MoveToFront(el)
	return cloneDocument(item.doc), true
}

func (c *documentCache) getStale(key string) (*Document, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return cloneDocument(el.Value.(*documentCacheItem).doc), true
	}
	return nil, false
}

func (c *documentCache) put(key string, doc *Document, ttl time.Duration) {
	if c == nil || key == "" || doc == nil {
		return
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	size := len(doc.Markdown)
	if size > c.maxByte {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		item := el.Value.(*documentCacheItem)
		c.bytes -= item.size
		item.doc, item.size = cloneDocument(doc), size
		item.expires = c.now().Add(ttl)
		c.bytes += size
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&documentCacheItem{key: key, doc: cloneDocument(doc), size: size, expires: c.now().Add(ttl)})
		c.items[key] = el
		c.bytes += size
	}
	for c.ll.Len() > c.max || c.bytes > c.maxByte {
		last := c.ll.Back()
		if last == nil {
			break
		}
		item := last.Value.(*documentCacheItem)
		c.bytes -= item.size
		delete(c.items, item.key)
		c.ll.Remove(last)
	}
}

// fetchFlightResult 在抓取层传递全文与 stale 标记，选段在调用方独立完成。
type fetchFlightResult struct {
	doc      *Document
	degraded []string
	cacheHit bool
}

type fetchFlightCall struct {
	done chan struct{}
	res  fetchFlightResult
	err  error
}

type fetchFlightGroup struct {
	mu sync.Mutex
	m  map[string]*fetchFlightCall
}

func (g *fetchFlightGroup) Do(ctx context.Context, key string, fn func() (fetchFlightResult, error)) (fetchFlightResult, error, bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*fetchFlightCall)
	}
	if call, ok := g.m[key]; ok {
		g.mu.Unlock()
		select {
		case <-call.done:
			res := call.res
			res.doc = cloneDocument(res.doc)
			res.degraded = append([]string(nil), res.degraded...)
			return res, call.err, true
		case <-ctx.Done():
			return fetchFlightResult{}, Wrap("", ErrTimeout, ctx.Err()), true
		}
	}
	call := &fetchFlightCall{done: make(chan struct{})}
	g.m[key] = call
	g.mu.Unlock()
	func() {
		defer func() {
			if r := recover(); r != nil {
				call.err = Wrap("", ErrUnavailable, fmt.Errorf("panic in fetch singleflight: %v", r))
			}
		}()
		call.res, call.err = fn()
	}()
	call.res.doc = cloneDocument(call.res.doc)
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	close(call.done)
	return call.res, call.err, false
}
