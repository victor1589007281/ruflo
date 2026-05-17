// query.go — 三引擎查询路由器。
//
// 查询路由策略（Graphify + GitNexus + Native 互补）：
//   - navigate / impact / find_refs / cross_shard → GitNexus 结构查询（SQLite AST 索引 + 调用图 JSON）
//   - communities / god_nodes / path / surprises   → Graphify 语义查询（图数据 + 社区检测）
//   - native_grep / native_read / native_fallback  → Native 原生兜底（实时 Grep/Read）
//
// 降级策略：
//   1. 优先查询索引引擎（GitNexus / Graphify）
//   2. 索引缺失、未命中或报错 → 自动降级到 Native 引擎
//   3. 同时返回 "engine" 字段标识实际使用的引擎
package codeintel

import (
	"encoding/json"
	"fmt"
	"time"
)

// Engine 三引擎查询路由器。
type Engine struct {
	GitNexus *GitNexusEngine
	Graphify *GraphifyEngine
	Native   *NativeEngine
	Store    *Store
}

// NewEngine 创建三引擎路由器。
func NewEngine(repoPath string) *Engine {
	store := NewStore(repoPath)
	return &Engine{
		GitNexus: NewGitNexusEngine(repoPath),
		Graphify: NewGraphifyEngine(repoPath),
		Native:   NewNativeEngine(repoPath),
		Store:    store,
	}
}

// Navigate 符号导航 → GitNexus（索引）→ Native（兜底）。
func (e *Engine) Navigate(branchName string, q NavigateQuery) (*QueryResult, error) {
	qr, err := e.GitNexus.Navigate(branchName, q)
	if err != nil {
		return e.nativeFallback("navigate", q.Symbol, err)
	}
	return qr, nil
}

// Impact 影响分析 → GitNexus。
func (e *Engine) Impact(branchName string, q ImpactQuery) (*QueryResult, error) {
	qr, err := e.GitNexus.Impact(branchName, q)
	if err != nil {
		return e.nativeFallback("impact", q.FilePath, err)
	}
	return qr, nil
}

// FindRefs 符号引用 → GitNexus → Native（兜底）。
func (e *Engine) FindRefs(branchName string, q NavigateQuery) (*QueryResult, error) {
	qr, err := e.GitNexus.FindRefs(branchName, q)
	if err != nil {
		return e.nativeFallback("find_refs", q.Symbol, err)
	}
	return qr, nil
}

// Communities 社区列表 → Graphify。
func (e *Engine) Communities(branchName string, q CommunityQuery) (*QueryResult, error) {
	qr, err := e.Graphify.Communities(branchName, q)
	if err != nil {
		return e.nativeFallback("communities", q.Shard, err)
	}
	return qr, nil
}

// GodNodes 高度数节点 → Graphify。
func (e *Engine) GodNodes(branchName, shardName string, topN int) (*QueryResult, error) {
	qr, err := e.Graphify.GodNodes(branchName, shardName, topN)
	if err != nil {
		return e.nativeFallback("god_nodes", shardName, err)
	}
	return qr, nil
}

// Path 最短路径 → Graphify。
func (e *Engine) Path(branchName string, src, dst string) (*QueryResult, error) {
	qr, err := e.Graphify.Path(branchName, src, dst)
	if err != nil {
		return e.nativeFallback("path", src+"->"+dst, err)
	}
	return qr, nil
}

// Surprises 异常边 → Graphify。
func (e *Engine) Surprises(branchName, shardName string, topN int) (*QueryResult, error) {
	qr, err := e.Graphify.Surprises(branchName, shardName, topN)
	if err != nil {
		return e.nativeFallback("surprises", shardName, err)
	}
	return qr, nil
}

// CrossShard 跨片查询 → GitNexus。
func (e *Engine) CrossShard(branchName string, q CrossShardQuery) (*QueryResult, error) {
	qr, err := e.GitNexus.CrossShard(branchName, q)
	if err != nil {
		return e.nativeFallback("cross_shard", q.Symbol, err)
	}
	return qr, nil
}

// Status 返回仓库索引全局状态。
func (e *Engine) Status(branchName string) (*QueryResult, error) {
	start := time.Now()
	cfg, err := e.Store.LoadConfig()
	if err != nil {
		return nil, err
	}
	branches, _ := e.Store.ListBranches()

	var branchInfo []map[string]interface{}
	for _, b := range branches {
		idx, err := e.Store.LoadBranchIndex(b)
		if err != nil {
			continue
		}
		shardNames := make([]string, 0, len(idx.Shards))
		for name := range idx.Shards {
			shardNames = append(shardNames, name)
		}
		branchInfo = append(branchInfo, map[string]interface{}{
			"name":       b,
			"commit":     idx.CommitHash,
			"shards":     shardNames,
			"updated_at": idx.UpdatedAt,
		})
	}

	result := map[string]interface{}{
		"repo_path":   cfg.RepoPath,
		"repo_hash":   cfg.RepoHash,
		"shard_count": len(cfg.Shards),
		"branches":    branchInfo,
		"llm_enhance": cfg.LLMEnhance,
		"auto_shard":  cfg.AutoShard,
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "status",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// UnifiedQuery 统一查询入口（支持所有查询类型）。
func (e *Engine) UnifiedQuery(branchName string, q UnifiedQuery) (*QueryResult, error) {
	switch q.QueryType {
	case QueryNavigate:
		return e.Navigate(branchName, NavigateQuery{Symbol: q.Symbol, Depth: q.Depth, Shard: q.Shard})
	case QueryImpact:
		return e.Impact(branchName, ImpactQuery{FilePath: q.FilePath, Depth: q.Depth})
	case QueryFindRefs:
		return e.FindRefs(branchName, NavigateQuery{Symbol: q.Symbol, Shard: q.Shard})
	case QueryCommunities:
		return e.Communities(branchName, CommunityQuery{Shard: q.Shard, TopN: q.TopN})
	case QueryGodNodes:
		return e.GodNodes(branchName, q.Shard, q.TopN)
	case QueryPath:
		return e.Path(branchName, q.Symbol, q.TargetSymbol)
	case QuerySurprises:
		return e.Surprises(branchName, q.Shard, q.TopN)
	case QueryCrossShard:
		return e.CrossShard(branchName, CrossShardQuery{Symbol: q.Symbol, TargetShard: q.TargetShard})
	case QueryNativeGrep:
		return e.Native.Grep(q.Symbol, GrepOptions{Dir: q.FilePath, MaxResults: q.TopN})
	case QueryNativeRead:
		return e.Native.ReadFile(q.FilePath, ReadOptions{Offset: q.Depth, Limit: q.TopN})
	default:
		return nil, fmt.Errorf("unknown query_type: %s", q.QueryType)
	}
}

// nativeFallback 索引引擎失败时降级到 Native 引擎。
func (e *Engine) nativeFallback(queryType, target string, originalErr error) (*QueryResult, error) {
	start := time.Now()
	qr, err := e.Native.FallbackSearch(target)
	if err != nil {
		// Native 也失败了，返回原始错误
		return nil, fmt.Errorf("%s failed (index: %v; native fallback: %v)", queryType, originalErr, err)
	}
	qr.QueryType = queryType + "_fallback"
	qr.LatencyMs = time.Since(start).Milliseconds()
	// 注入降级标记
	if m, ok := qr.Results.(map[string]interface{}); ok {
		m["_fallback"] = true
		m["_original_error"] = originalErr.Error()
		m["engine"] = "native"
	}
	return qr, nil
}

// ============================================================================
// 统一查询参数
// ============================================================================

// UnifiedQuery 统一查询参数（用于 Router 分发）。
type UnifiedQuery struct {
	QueryType    string `json:"query_type"`
	Shard        string `json:"shard,omitempty"`
	Symbol       string `json:"symbol,omitempty"`
	TargetSymbol string `json:"target_symbol,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	Depth        int    `json:"depth,omitempty"`
	TopN         int    `json:"top_n,omitempty"`
	TargetShard  string `json:"target_shard,omitempty"`
}

// QueryType 枚举。
const (
	QueryNavigate    = "navigate"
	QueryImpact      = "impact"
	QueryFindRefs    = "find_refs"
	QueryCommunities = "communities"
	QueryGodNodes    = "god_nodes"
	QueryPath        = "path"
	QuerySurprises   = "surprises"
	QueryCrossShard  = "cross_shard"
	QueryStatus      = "status"
	QueryNativeGrep  = "native_grep"
	QueryNativeRead  = "native_read"
)
