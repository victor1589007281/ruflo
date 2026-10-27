package search

// sharedcache.go —— Manager 的 L1/L2 缓存组合。
//
// 读取顺序恒为本地 L1 -> Redis L2 -> provider/HTTP。L2 命中会按剩余 fresh
// TTL 回填 L1；所有出口都返回 clone。Redis 故障不阻断免费检索或抓取，而是
// 通过 degraded 显式标记并继续使用本地缓存与真实源。
import (
	"context"
	"time"
)

func cloneSearchResponse(resp SearchResponse) SearchResponse {
	out := resp
	// results 是稳定 JSON 数组契约: 即使上游给 nil, 对外也必须是 [] 而不是 null。
	out.Results = append([]Result{}, resp.Results...)
	out.Providers = append([]string(nil), resp.Providers...)
	out.Degraded = append([]string(nil), resp.Degraded...)
	return out
}

func fetchCacheKey(raw string) string {
	return "f|" + cacheDigest(CanonicalURL(raw))
}

func (m *Manager) noteSharedCacheError(kind, operation string) {
	m.metrics.Inc("shared_cache_errors_total", map[string]string{
		"cache_type": kind, "operation": operation,
	})
}

// cachedSearch 返回本次调用无需新付费的缓存结果。缓存条目仍保留原始实际
// 成本，只有返回给本次调用方的副本把 CostUSD 清零。
func (m *Manager) cachedSearch(ctx context.Context, key string) (SearchResponse, bool, []string) {
	if cached, ok := m.cache.get(key); ok {
		cached.CacheHit = true
		cached.CostUSD = 0
		m.metrics.Inc("cache_hit_total", map[string]string{"cache_type": "search"})
		return cached, true, nil
	}
	if m.shared == nil {
		return SearchResponse{}, false, nil
	}
	cached, remaining, ok, err := m.shared.loadSearch(ctx, key)
	if err != nil {
		m.noteSharedCacheError("search", "get")
		return SearchResponse{}, false, []string{sharedCacheDegraded}
	}
	if !ok {
		return SearchResponse{}, false, nil
	}
	m.cache.put(key, cached, remaining)
	out := cloneSearchResponse(cached)
	out.CacheHit = true
	out.CostUSD = 0
	m.metrics.Inc("cache_hit_total", map[string]string{"cache_type": "search"})
	return out, true, nil
}

// putSearchCache 先把不带瞬时 Redis 降级标记的完整响应写入缓存，再把
// 写入错误只附加到本次响应，避免一次抖动污染后续缓存命中。
func (m *Manager) putSearchCache(ctx context.Context, key string, resp SearchResponse, ttl time.Duration) SearchResponse {
	m.cache.put(key, resp, ttl)
	if m.shared != nil {
		if err := m.shared.saveSearch(ctx, key, resp, ttl); err != nil {
			m.noteSharedCacheError("search", "set")
			resp.Degraded = append(resp.Degraded, sharedCacheDegraded)
		}
	}
	return resp
}

func (m *Manager) cachedDocument(ctx context.Context, key string) (*Document, bool, []string) {
	if cached, ok := m.fetchCache.get(key); ok {
		cached.CacheHit = true
		m.metrics.Inc("cache_hit_total", map[string]string{"cache_type": "fetch"})
		return cached, true, nil
	}
	if m.shared == nil {
		return nil, false, nil
	}
	cached, remaining, found, fresh, err := m.shared.loadDocument(ctx, key)
	if err != nil {
		m.noteSharedCacheError("fetch", "get")
		return nil, false, []string{sharedCacheDegraded}
	}
	if !found || !fresh {
		return nil, false, nil
	}
	m.fetchCache.put(key, cached, remaining)
	out := cloneDocument(cached)
	out.CacheHit = true
	m.metrics.Inc("cache_hit_total", map[string]string{"cache_type": "fetch"})
	return out, true, nil
}

// staleDocument 只在调用方已遇到临时网络故障时调用。新鲜的 L2 文档不应
// 在这里伪装成 stale；它通常已由缓存二次检查接走。
func (m *Manager) staleDocument(ctx context.Context, key string) (*Document, bool, []string) {
	if stale, ok := m.fetchCache.getStale(key); ok {
		return stale, true, nil
	}
	if m.shared == nil {
		return nil, false, nil
	}
	stale, _, found, fresh, err := m.shared.loadDocument(ctx, key)
	if err != nil {
		m.noteSharedCacheError("fetch", "get_stale")
		return nil, false, []string{sharedCacheDegraded}
	}
	if !found || fresh {
		return nil, false, nil
	}
	return stale, true, nil
}

func (m *Manager) putDocumentCache(ctx context.Context, key string, doc *Document, ttl time.Duration) []string {
	m.fetchCache.put(key, doc, ttl)
	if m.shared != nil {
		if err := m.shared.saveDocument(ctx, key, doc, ttl); err != nil {
			m.noteSharedCacheError("fetch", "set")
			return []string{sharedCacheDegraded}
		}
	}
	return nil
}
