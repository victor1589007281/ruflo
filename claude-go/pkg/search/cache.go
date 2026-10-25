package search

// cache.go —— 带 TTL 的进程内 LRU 查询缓存。
//
// 只缓存成功响应 (含"成功但零结果"), 不缓存错误: 一次 429/超时不值得在整个
// TTL 内被放大成确定性失败。key 包含 query、候选源名、语言与时效, 任一变化
// 都会重新打源; 新闻类查询由 manager.cacheTTL 选择更短的 TTL。
import (
	"container/list"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// searchCache 是带 TTL 的 LRU 查询缓存。
//
// 只缓存成功完成的 SearchResponse; 错误不缓存, 避免把一次抖动放大成
// 整个 TTL 的失败。容量按条目数计, 超限淘汰最久未使用项。
type searchCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
	now   func() time.Time // 可注入, 测试用
}

// cacheItem 是 LRU 节点值。
type cacheItem struct {
	key     string
	resp    SearchResponse
	expires time.Time
}

// newSearchCache 创建容量为 max 的 LRU; max <= 0 时用 5000。
func newSearchCache(max int) *searchCache {
	if max <= 0 {
		max = 5000
	}
	return &searchCache{
		max:   max,
		ll:    list.New(),
		items: make(map[string]*list.Element),
		now:   time.Now,
	}
}

// get 取缓存; 命中时把节点移到队首。过期项就地删除。
func (c *searchCache) get(key string) (SearchResponse, bool) {
	if c == nil {
		return SearchResponse{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return SearchResponse{}, false
	}
	item := el.Value.(*cacheItem)
	if !c.now().Before(item.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return SearchResponse{}, false
	}
	c.ll.MoveToFront(el)
	resp := item.resp
	resp.Results = append([]Result(nil), item.resp.Results...)
	resp.Providers = append([]string(nil), item.resp.Providers...)
	resp.Degraded = append([]string(nil), item.resp.Degraded...)
	return resp, true
}

// put 写入缓存并淘汰超限的最久未使用项。
func (c *searchCache) put(key string, resp SearchResponse, ttl time.Duration) {
	if c == nil || key == "" {
		return
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		item := el.Value.(*cacheItem)
		item.resp = resp
		item.expires = c.now().Add(ttl)
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheItem{key: key, resp: resp, expires: c.now().Add(ttl)})
	c.items[key] = el
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		if last == nil {
			break
		}
		item := last.Value.(*cacheItem)
		c.ll.Remove(last)
		delete(c.items, item.key)
	}
}

// len 返回当前条目数, 测试用。
func (c *searchCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// searchCacheKey 生成查询缓存键。
//
// key = 规范化 query + provider 集合 + 语言 + freshness。provider 集合
// 排序后拼接, 保证不同选择顺序得到同一个 key; 集合不同则视为不同请求,
// 因为结果质量与来源直接相关。
func searchCacheKey(query string, providers []string, language, freshness string) string {
	names := append([]string(nil), providers...)
	sort.Strings(names)
	q := normalizeQuery(query)
	lang := normalizeLang(language)
	if lang == "" {
		lang = "auto"
	}
	fresh := strings.ToLower(strings.TrimSpace(freshness))
	if fresh == "" {
		fresh = "any"
	}
	return fmt.Sprintf("s|%s|%s|%s|%s", q, strings.Join(names, ","), lang, fresh)
}

// normalizeQuery 规范化 query: 去首尾空白、压缩连续空白、转小写。
func normalizeQuery(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// flightGroup 是自己实现的 singleflight。
//
// 同一 key 的并发请求只执行一次 fn, 其余等待并复用结果。不引入
// golang.org/x/sync 是为了不增加依赖, 且这里只需要"合并同 key 调用"。
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

// flightCall 是一次在途调用。
type flightCall struct {
	done chan struct{}
	resp SearchResponse
	err  error
}

// Do 执行或等待同 key 调用。shared 表示本次是否复用了别人的结果。
func (g *flightGroup) Do(ctx context.Context, key string, fn func() (SearchResponse, error)) (SearchResponse, error, bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightCall)
	}
	if call, ok := g.m[key]; ok {
		g.mu.Unlock()
		select {
		case <-call.done:
			return call.resp, call.err, true
		case <-ctx.Done():
			return SearchResponse{}, Wrap("", ErrTimeout, ctx.Err()), true
		}
	}
	call := &flightCall{done: make(chan struct{})}
	g.m[key] = call
	g.mu.Unlock()

	// 用闭包保证 panic 时也能唤醒等待者并清理表项。
	func() {
		defer func() {
			if r := recover(); r != nil {
				call.err = Wrap("", ErrUnavailable, fmt.Errorf("panic in singleflight: %v", r))
			}
		}()
		call.resp, call.err = fn()
	}()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	close(call.done)
	return call.resp, call.err, false
}
