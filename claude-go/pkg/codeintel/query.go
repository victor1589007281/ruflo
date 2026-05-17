// query.go — 查询引擎。
//
// 提供结构查询（类 GitNexus）和语义查询（类 Graphify）的统一入口。
// 查询路由：
//   - navigate / impact / find_refs → 结构查询（SQLite AST 索引 + 调用图 JSON）
//   - communities / god_nodes / path / surprises → 语义查询（图数据）
//   - cross_shard → 跨片查询（cross_edges.json）
package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Engine 查询引擎。
type Engine struct {
	Store *Store
}

// NewEngine 创建查询引擎。
func NewEngine(repoPath string) *Engine {
	return &Engine{Store: NewStore(repoPath)}
}

// Navigate 符号导航：定义位置 + 调用方/被调用方。
func (e *Engine) Navigate(branchName string, q NavigateQuery) (*QueryResult, error) {
	start := time.Now()

	shards := e.resolveShards(branchName, q.Shard)
	if len(shards) == 0 {
		return nil, fmt.Errorf("no shards available")
	}

	var results []map[string]interface{}
	for _, shardName := range shards {
		// 1. 从 SQLite 查定义
		def, err := e.findSymbolDefInShard(branchName, shardName, q.Symbol)
		if err != nil {
			continue
		}

		// 2. 从 SQLite 查调用方/被调用方
		callers, _ := e.findCallersInShard(branchName, shardName, q.Symbol, q.Depth)
		callees, _ := e.findCalleesInShard(branchName, shardName, q.Symbol, q.Depth)

		results = append(results, map[string]interface{}{
			"shard":   shardName,
			"symbol":  q.Symbol,
			"def":     def,
			"callers": callers,
			"callees": callees,
		})
	}

	content, _ := json.MarshalIndent(results, "", "  ")
	return &QueryResult{
		QueryType: "navigate",
		Shard:     q.Shard,
		Results:   results,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// Impact 影响分析：给定文件的依赖半径。
func (e *Engine) Impact(branchName string, q ImpactQuery) (*QueryResult, error) {
	start := time.Now()

	// 加载调用图 JSON
	shards := e.resolveShards(branchName, "")
	directDeps := make(map[string]bool)
	transitive := make(map[string]bool)
	var topCallers []map[string]interface{}

	for _, shardName := range shards {
		cgPath := e.Store.CallgraphPath(branchName, shardName)
		data, err := os.ReadFile(cgPath)
		if err != nil {
			continue
		}
		var cg struct {
			Nodes []map[string]interface{} `json:"nodes"`
			Edges []map[string]interface{} `json:"edges"`
		}
		if err := json.Unmarshal(data, &cg); err != nil {
			continue
		}

		// 找到文件中定义的符号
		fileSymbols := make(map[string]bool)
		for _, node := range cg.Nodes {
			if file, _ := node["file"].(string); file == q.FilePath {
				if id, _ := node["id"].(string); id != "" {
					fileSymbols[id] = true
				}
			}
		}

		// BFS 找依赖
		for sym := range fileSymbols {
			for _, edge := range cg.Edges {
				src, _ := edge["source"].(string)
				dst, _ := edge["target"].(string)
				if src == sym && !fileSymbols[dst] {
					directDeps[dst] = true
				}
				if dst == sym && !fileSymbols[src] {
					directDeps[src] = true
				}
			}
		}
	}

	// 简化：transitive = direct (不做深层 BFS 避免性能问题)
	for dep := range directDeps {
		transitive[dep] = true
	}

	var directList, transList []string
	for d := range directDeps {
		directList = append(directList, d)
	}
	for t := range transitive {
		transList = append(transList, t)
	}

	result := map[string]interface{}{
		"file":        q.FilePath,
		"direct_deps": directList,
		"transitive":  transList,
		"top_callers": topCallers,
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "impact",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// Communities 社区列表查询。
func (e *Engine) Communities(branchName string, q CommunityQuery) (*QueryResult, error) {
	start := time.Now()
	idx, err := e.Store.LoadShardIndex(branchName, q.Shard)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{
		"shard":        q.Shard,
		"communities":  idx.Communities,
		"god_nodes":    idx.GodNodes,
		"file_count":   idx.FileCount,
		"symbol_count": idx.SymbolCount,
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "communities",
		Shard:     q.Shard,
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// GodNodes 高度数节点查询。
func (e *Engine) GodNodes(branchName, shardName string, topN int) (*QueryResult, error) {
	start := time.Now()
	idx, err := e.Store.LoadShardIndex(branchName, shardName)
	if err != nil {
		return nil, err
	}
	gods := idx.GodNodes
	if topN > 0 && topN < len(gods) {
		gods = gods[:topN]
	}
	result := map[string]interface{}{
		"shard":     shardName,
		"god_nodes": gods,
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "god_nodes",
		Shard:     shardName,
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// CrossShard 跨片查询。
func (e *Engine) CrossShard(branchName string, q CrossShardQuery) (*QueryResult, error) {
	start := time.Now()

	// 加载跨片边 JSON
	crossPath := e.Store.CrossEdgesPath(branchName) + ".json"
	var crossData struct {
		CrossEdges []struct {
			Src  string `json:"src"`
			Dst  string `json:"dst"`
			Kind string `json:"kind"`
		} `json:"cross_edges"`
	}

	data, err := os.ReadFile(crossPath)
	if err == nil {
		_ = json.Unmarshal(data, &crossData)
	}

	var related []map[string]string
	for _, edge := range crossData.CrossEdges {
		if q.Symbol != "" && (edge.Src == q.Symbol || edge.Dst == q.Symbol) {
			related = append(related, map[string]string{
				"src":  edge.Src,
				"dst":  edge.Dst,
				"kind": edge.Kind,
			})
		}
		if q.TargetShard != "" && (edge.Src == q.TargetShard || edge.Dst == q.TargetShard) {
			related = append(related, map[string]string{
				"src":  edge.Src,
				"dst":  edge.Dst,
				"kind": edge.Kind,
			})
		}
	}

	result := map[string]interface{}{
		"symbol":       q.Symbol,
		"target_shard": q.TargetShard,
		"call_sites":   related,
		"total_edges":  len(crossData.CrossEdges),
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "cross_shard",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
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

// ============================================================================
// 私有辅助 — 真实 SQLite 查询
// ============================================================================

func (e *Engine) resolveShards(branchName, shard string) []string {
	if shard != "" {
		return []string{shard}
	}
	idx, err := e.Store.LoadBranchIndex(branchName)
	if err != nil {
		return nil
	}
	var names []string
	for name := range idx.Shards {
		names = append(names, name)
	}
	return names
}

func (e *Engine) findSymbolDefInShard(branchName, shardName, symbol string) (map[string]interface{}, error) {
	db, err := e.Store.OpenShardDB(branchName, shardName)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var name, kind, file string
	var line int
	err = db.QueryRow("SELECT name, kind, file, line FROM symbols WHERE name = ? LIMIT 1", symbol).Scan(&name, &kind, &file, &line)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"name": name,
		"kind": kind,
		"file": file,
		"line": line,
	}, nil
}

func (e *Engine) findCallersInShard(branchName, shardName, symbol string, depth int) ([]map[string]interface{}, error) {
	db, err := e.Store.OpenShardDB(branchName, shardName)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT src_symbol, file, line FROM edges WHERE dst_symbol = ? AND kind = 'call' LIMIT 50", symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var src, file string
		var line int
		if err := rows.Scan(&src, &file, &line); err != nil {
			continue
		}
		results = append(results, map[string]interface{}{
			"caller": src,
			"file":   file,
			"line":   line,
		})
	}
	return results, rows.Err()
}

func (e *Engine) findCalleesInShard(branchName, shardName, symbol string, depth int) ([]map[string]interface{}, error) {
	db, err := e.Store.OpenShardDB(branchName, shardName)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT dst_symbol, file, line FROM edges WHERE src_symbol = ? AND kind = 'call' LIMIT 50", symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var dst, file string
		var line int
		if err := rows.Scan(&dst, &file, &line); err != nil {
			continue
		}
		results = append(results, map[string]interface{}{
			"callee": dst,
			"file":   file,
			"line":   line,
		})
	}
	return results, rows.Err()
}

// LoadShardGraph 加载分片的 NetworkX 风格图数据（JSON）。
func LoadShardGraph(graphifyPath string) (map[string]interface{}, error) {
	path := filepath.Join(graphifyPath, "graph.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g map[string]interface{}
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return g, nil
}

// QueryType 枚举。
const (
	QueryNavigate    = "navigate"
	QueryImpact      = "impact"
	QueryCommunities = "communities"
	QueryGodNodes    = "god_nodes"
	QueryCrossShard  = "cross_shard"
	QueryStatus      = "status"
)
