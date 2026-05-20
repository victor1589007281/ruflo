// query.go — 查询路由器（v3.1）。
//
// 路由策略：
//   1. 优先调用 GitNexus / Graphify CLI（外部索引）
//   2. 缓存最近 60s 的查询结果（避免重复 exec）
//   3. GitNexus 返回 ambiguous / not_found → 自动降级到 Native 兜底
//   4. Graphify 返回空结果 → 提示用户可能未索引
package codeintel

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Engine 查询路由器。
type Engine struct {
	GitNexus       *GitNexus
	Graphify       *Graphify
	GraphifyNoLLM  *GraphifyNoLLM
	Native         *NativeEngine
	Store          *Store
	cache          *queryCache
	vectorCache    *VectorCache
	metrics        *MetricsCollector
	globalIndex      *GlobalIndex
	repoID           string
	incrementalMgr   *IncrementalIndexManager
	implicitFeedback *ImplicitFeedbackCollector
}

// NewEngine 创建查询路由器。
func NewEngine(repoPath string) *Engine {
	return &Engine{
		GitNexus:      NewGitNexus(repoPath),
		Graphify:      NewGraphify(repoPath),
		GraphifyNoLLM: NewGraphifyNoLLM(repoPath),
		Native:        NewNativeEngine(repoPath),
		Store:         NewStore(repoPath),
		cache:         newQueryCache(60 * time.Second),
		vectorCache:   NewVectorCache(repoPath),
		metrics:       NewMetricsCollector(repoPath),
	}
}

// SetGlobalIndex 绑定全局索引管理器，用于查询前 readiness 检查。
func (e *Engine) SetGlobalIndex(gi *GlobalIndex, repoID string) {
	e.globalIndex = gi
	e.repoID = repoID
}

// SetIncrementalManager 绑定增量索引管理器。
func (e *Engine) SetIncrementalManager(mgr *IncrementalIndexManager) {
	e.incrementalMgr = mgr
}

// SetImplicitFeedbackCollector 绑定隐式反馈采集器。
func (e *Engine) SetImplicitFeedbackCollector(collector *ImplicitFeedbackCollector) {
	e.implicitFeedback = collector
}

// Navigate 符号导航 → GitNexus context → Native 兜底。
func (e *Engine) Navigate(branchName string, q NavigateQuery) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.GitNexus.Navigate(branchName, q)
	fallback := false
	if err != nil || isEmptyResult(qr) {
		qr, err = e.Native.FallbackSearch(q.Symbol)
		fallback = true
	} else if isGitNexusMiss(qr) {
		qr, err = e.Native.FallbackSearch(q.Symbol)
		fallback = true
	}
	e.recordQuery("navigate", "gitnexus", qr, err, fallback, time.Since(start).Milliseconds())
	return qr, err
}

// Impact 影响分析 → GitNexus impact → Native 兜底。
func (e *Engine) Impact(branchName string, q ImpactQuery) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.GitNexus.ImpactQuery(branchName, q)
	fallback := false
	if err != nil || isEmptyResult(qr) {
		qr, err = e.Native.FallbackSearch(q.FilePath)
		fallback = true
	} else if isGitNexusMiss(qr) {
		qr, err = e.Native.FallbackSearch(q.FilePath)
		fallback = true
	}
	e.recordQuery("impact", "gitnexus", qr, err, fallback, time.Since(start).Milliseconds())
	return qr, err
}

// FindRefs 符号引用 → GitNexus context → Native 兜底。
func (e *Engine) FindRefs(branchName string, q NavigateQuery) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.GitNexus.FindRefs(branchName, q)
	fallback := false
	if err != nil || isEmptyResult(qr) {
		qr, err = e.Native.FindReferences(q.Symbol, 50)
		fallback = true
	} else if isGitNexusMiss(qr) {
		qr, err = e.Native.FindReferences(q.Symbol, 50)
		fallback = true
	}
	e.recordQuery("find_refs", "gitnexus", qr, err, fallback, time.Since(start).Milliseconds())
	return qr, err
}

// Communities 社区列表 → Graphify query。
func (e *Engine) Communities(branchName string, q CommunityQuery) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.Graphify.Communities(branchName, q)
	e.recordQuery("communities", "graphify", qr, err, false, time.Since(start).Milliseconds())
	return qr, err
}

// GodNodes 高度数节点 → Graphify query。
func (e *Engine) GodNodes(branchName, shardName string, topN int) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.Graphify.GodNodes(branchName, shardName, topN)
	e.recordQuery("god_nodes", "graphify", qr, err, false, time.Since(start).Milliseconds())
	return qr, err
}

// Path 最短路径 → Graphify path。
func (e *Engine) Path(branchName string, src, dst string) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.Graphify.Path(src, dst)
	e.recordQuery("path", "graphify", qr, err, false, time.Since(start).Milliseconds())
	return qr, err
}

// Surprises 异常边 → Graphify query。
func (e *Engine) Surprises(branchName, shardName string, topN int) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.Graphify.Surprises(branchName, shardName, topN)
	e.recordQuery("surprises", "graphify", qr, err, false, time.Since(start).Milliseconds())
	return qr, err
}

// CrossShard 跨片查询 → GitNexus query → Native 兜底。
func (e *Engine) CrossShard(branchName string, q CrossShardQuery) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.GitNexus.CrossShard(branchName, q)
	fallback := false
	if err != nil || isEmptyResult(qr) || isGitNexusMiss(qr) {
		qr, err = e.Native.FindReferences(q.Symbol, 50)
		fallback = true
	}
	e.recordQuery("cross_shard", "gitnexus", qr, err, fallback, time.Since(start).Milliseconds())
	return qr, err
}

// Status 返回两个工具的状态汇总。
func (e *Engine) Status(branchName string) (*QueryResult, error) {
	start := time.Now()

	gnStatus, gnErr := e.GitNexus.Status()
	gfStatus, gfErr := e.Graphify.IsIndexed(), error(nil)
	_ = gfErr

	state, _ := e.Store.LoadState()

	result := map[string]interface{}{
		"repo_path":        e.Store.RepoPath,
		"gitnexus_status":  safeResult(gnStatus),
		"gitnexus_error":   errString(gnErr),
		"graphify_indexed": gfStatus,
		"state":            state,
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "status",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// UnifiedQuery 统一查询入口。
func (e *Engine) UnifiedQuery(branchName string, q UnifiedQuery) (*QueryResult, error) {
	queryText := unifiedQueryText(q)

	// 隐式反馈：观察是否有重查行为
	if e.implicitFeedback != nil {
		e.implicitFeedback.ObserveQuery(queryText)
	}

	// L1: 精确查询缓存（TTL 60s）
	cacheKey := fmt.Sprintf("%s:%s:%s:%s", q.QueryType, q.Symbol, q.FilePath, q.Shard)
	if cached := e.cache.get(cacheKey); cached != nil {
		if e.metrics != nil {
			e.metrics.RecordQuery(QueryMetrics{
				Timestamp: time.Now(),
				QueryType: q.QueryType,
				Engine:    "l1_cache",
				Success:   true,
				CacheHit:  true,
			})
		}
		return cached, nil
	}

	// L1.5: 全局索引 readiness 检查（引导降级）
	if e.globalIndex != nil && e.repoID != "" {
		readiness := e.globalIndex.CheckQueryReady(e.repoID, branchName)
		if !readiness.Ready {
			result := map[string]interface{}{
				"ready":         false,
				"repo_exists":   readiness.RepoExists,
				"branch_exists": readiness.BranchExists,
				"commit_match":  readiness.CommitMatch,
				"current_head":  readiness.CurrentHead,
				"indexed_head":  readiness.IndexedHead,
				"suggestion":    readiness.Suggestion,
			}
			content, _ := json.MarshalIndent(result, "", "  ")
			return &QueryResult{
				QueryType: q.QueryType,
				Results:   result,
				Tokens:    len(content) / 4,
				LatencyMs: 0,
			}, nil
		}
	}

	// L2: 语义相似缓存（向量相似度）
	if cached, hit := e.vectorCache.FindSimilar(queryText); hit {
		if e.metrics != nil {
			e.metrics.RecordQuery(QueryMetrics{
				Timestamp: time.Now(),
				QueryType: q.QueryType,
				Engine:    "l2_cache",
				Success:   true,
				CacheHit:  true,
			})
		}
		return cached, nil
	}

	var qr *QueryResult
	var err error

	switch q.QueryType {
	case QueryNavigate:
		qr, err = e.Navigate(branchName, NavigateQuery{Symbol: q.Symbol, Depth: q.Depth, Shard: q.Shard})
	case QueryImpact:
		qr, err = e.Impact(branchName, ImpactQuery{FilePath: q.FilePath, Depth: q.Depth})
	case QueryFindRefs:
		qr, err = e.FindRefs(branchName, NavigateQuery{Symbol: q.Symbol, Shard: q.Shard})
	case QueryCommunities:
		qr, err = e.Communities(branchName, CommunityQuery{Shard: q.Shard, TopN: q.TopN})
	case QueryGodNodes:
		qr, err = e.GodNodes(branchName, q.Shard, q.TopN)
	case QueryPath:
		qr, err = e.Path(branchName, q.Symbol, q.TargetSymbol)
	case QuerySurprises:
		qr, err = e.Surprises(branchName, q.Shard, q.TopN)
	case QueryCrossShard:
		qr, err = e.CrossShard(branchName, CrossShardQuery{Symbol: q.Symbol, TargetShard: q.TargetShard})
	case QueryStatus:
		qr, err = e.Status(branchName)
	default:
		return nil, fmt.Errorf("unknown query_type: %s", q.QueryType)
	}

	// 增量索引合并：如果分支有增量变更且查询符号受影响，尝试合并增量结果
	if err == nil && qr != nil && e.incrementalMgr != nil && e.repoID != "" {
		if delta, ok := e.incrementalMgr.GetDelta(e.repoID, branchName); ok && delta.IsSymbolAffected(q.Symbol) {
			// 重新查询当前 HEAD（增量），与基线结果合并
			var deltaQr *QueryResult
			var deltaErr error
			switch q.QueryType {
			case QueryNavigate:
				deltaQr, deltaErr = e.Navigate(branchName, NavigateQuery{Symbol: q.Symbol, Depth: q.Depth, Shard: q.Shard})
			case QueryFindRefs:
				deltaQr, deltaErr = e.FindRefs(branchName, NavigateQuery{Symbol: q.Symbol, Shard: q.Shard})
			case QueryImpact:
				deltaQr, deltaErr = e.Impact(branchName, ImpactQuery{FilePath: q.FilePath, Depth: q.Depth})
			}
			if deltaErr == nil && deltaQr != nil {
				qr = MergeWithBaseline(qr, deltaQr)
			}
		}
	}

	if err == nil && qr != nil {
		e.cache.set(cacheKey, qr)
		e.vectorCache.Store(queryText, qr)
	}

	// 隐式反馈：记录本次查询，启动超时观察
	if e.implicitFeedback != nil {
		cacheHit := e.cache.get(cacheKey) != nil
		e.implicitFeedback.TrackQuery(queryText, cacheHit)
	}

	return qr, err
}

// RecordFeedback 记录用户对查询结果的反馈，用于 VectorCache 阈值自校准。
// 由外部 Tool / MCP handler 在获取用户反馈后调用。
func (e *Engine) RecordFeedback(queryText string, hit bool, rating int, clicked, resent bool) {
	if e.vectorCache != nil {
		e.vectorCache.RecordFeedback(queryText, hit, rating, clicked, resent)
	}
}

// unifiedQueryText 将 UnifiedQuery 转换为用于语义缓存的查询文本。
func unifiedQueryText(q UnifiedQuery) string {
	parts := []string{q.QueryType, q.Symbol, q.FilePath, q.Shard, q.TargetSymbol, q.TargetShard}
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, " ")
}

// ============================================================================
// 降级判断
// ============================================================================

func isEmptyResult(qr *QueryResult) bool {
	if qr == nil || qr.Results == nil {
		return true
	}
	return false
}

func isGitNexusMiss(qr *QueryResult) bool {
	if qr == nil || qr.Results == nil {
		return true
	}
	m, ok := qr.Results.(map[string]interface{})
	if !ok {
		return false
	}
	// GitNexus context/status 返回的 miss 标记
	if status, ok := m["status"].(string); ok {
		if status == "not_found" {
			return true
		}
		// ambiguous 但 candidates 非空 → 保留 GitNexus 结果（比 Native 更精确）
		if status == "ambiguous" {
			if cands, ok := m["candidates"].([]interface{}); ok && len(cands) > 0 {
				return false
			}
			if cands, ok := m["candidates"].([]map[string]interface{}); ok && len(cands) > 0 {
				return false
			}
			return true // ambiguous 但无 candidates，降级到 Native
		}
	}
	// 没有 symbol/target/candidates 字段才视为 miss
	if _, ok := m["symbol"]; !ok {
		if _, ok := m["target"]; !ok {
			if _, ok := m["candidates"]; !ok {
				return true
			}
		}
	}
	return false
}

// ============================================================================
// 简单内存缓存（TTL）
// ============================================================================

type cacheEntry struct {
	result    *QueryResult
	expiresAt time.Time
}

type queryCache struct {
	ttl     time.Duration
	entries map[string]*cacheEntry
}

func newQueryCache(ttl time.Duration) *queryCache {
	return &queryCache{
		ttl:     ttl,
		entries: make(map[string]*cacheEntry),
	}
}

func (c *queryCache) get(key string) *QueryResult {
	ent, ok := c.entries[key]
	if !ok {
		return nil
	}
	if time.Now().After(ent.expiresAt) {
		delete(c.entries, key)
		return nil
	}
	return ent.result
}

func (c *queryCache) set(key string, qr *QueryResult) {
	c.entries[key] = &cacheEntry{
		result:    qr,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// ============================================================================
// 私有辅助
// ============================================================================

func safeResult(qr *QueryResult) interface{} {
	if qr == nil {
		return nil
	}
	return qr.Results
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// recordQuery 记录查询指标。
func (e *Engine) recordQuery(queryType, engine string, qr *QueryResult, err error, fallback bool, latencyMs int64) {
	if e.metrics == nil {
		return
	}
	resultCount := 0
	if qr != nil && qr.Results != nil {
		switch v := qr.Results.(type) {
		case []interface{}:
			resultCount = len(v)
		case []map[string]interface{}:
			resultCount = len(v)
		case map[string]interface{}:
			resultCount = len(v)
		}
	}
	e.metrics.RecordQuery(QueryMetrics{
		Timestamp:   time.Now(),
		QueryType:   queryType,
		Engine:      engine,
		LatencyMs:   latencyMs,
		Success:     err == nil,
		Fallback:    fallback,
		ResultCount: resultCount,
	})
}
