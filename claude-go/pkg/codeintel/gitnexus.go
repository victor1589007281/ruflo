// gitnexus.go — GitNexus 结构查询引擎。
//
// 职责: 精确的结构查询，基于 SQLite AST 索引和调用图 JSON。
// 查询类型:
//   - navigate    → 符号定义 + 调用方/被调用方
//   - impact      → 文件依赖半径
//   - find_refs   → 符号引用位置
//   - cross_shard → 跨片引用
//
// 零 LLM：纯 SQLite + JSON 静态数据。
package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// GitNexusEngine 结构查询引擎（精确导航）。
type GitNexusEngine struct {
	Store *Store
}

// NewGitNexusEngine 创建 GitNexus 引擎。
func NewGitNexusEngine(repoPath string) *GitNexusEngine {
	return &GitNexusEngine{Store: NewStore(repoPath)}
}

// Navigate 符号导航：定义位置 + 调用方/被调用方。
func (e *GitNexusEngine) Navigate(branchName string, q NavigateQuery) (*QueryResult, error) {
	start := time.Now()

	shards := e.resolveShards(branchName, q.Shard)
	if len(shards) == 0 {
		return nil, fmt.Errorf("no shards available")
	}

	var results []map[string]interface{}
	for _, shardName := range shards {
		def, err := e.findSymbolDefInShard(branchName, shardName, q.Symbol)
		if err != nil {
			continue
		}
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
func (e *GitNexusEngine) Impact(branchName string, q ImpactQuery) (*QueryResult, error) {
	start := time.Now()

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

		fileSymbols := make(map[string]bool)
		for _, node := range cg.Nodes {
			if file, _ := node["file"].(string); file == q.FilePath {
				if id, _ := node["id"].(string); id != "" {
					fileSymbols[id] = true
				}
			}
		}

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

// FindRefs 查找符号的所有引用位置。
func (e *GitNexusEngine) FindRefs(branchName string, q NavigateQuery) (*QueryResult, error) {
	start := time.Now()
	shards := e.resolveShards(branchName, q.Shard)

	var allRefs []map[string]interface{}
	for _, shardName := range shards {
		db, err := e.Store.OpenShardDB(branchName, shardName)
		if err != nil {
			continue
		}
		rows, err := db.Query("SELECT src_symbol, dst_symbol, kind, file, line FROM edges WHERE src_symbol = ? OR dst_symbol = ? LIMIT 100", q.Symbol, q.Symbol)
		if err != nil {
			db.Close()
			continue
		}
		for rows.Next() {
			var src, dst, kind, file string
			var line int
			if err := rows.Scan(&src, &dst, &kind, &file, &line); err != nil {
				continue
			}
			refType := "caller"
			if dst == q.Symbol {
				refType = "callee"
			}
			allRefs = append(allRefs, map[string]interface{}{
				"type":   refType,
				"symbol": q.Symbol,
				"peer":   map[string]string{"src": src, "dst": dst, "kind": kind},
				"file":   file,
				"line":   line,
				"shard":  shardName,
			})
		}
		rows.Close()
		db.Close()
	}

	result := map[string]interface{}{
		"symbol": q.Symbol,
		"refs":   allRefs,
		"count":  len(allRefs),
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "find_refs",
		Shard:     q.Shard,
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// CrossShard 跨片查询。
func (e *GitNexusEngine) CrossShard(branchName string, q CrossShardQuery) (*QueryResult, error) {
	start := time.Now()

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

// ============================================================================
// 私有辅助
// ============================================================================

func (e *GitNexusEngine) resolveShards(branchName, shard string) []string {
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

func (e *GitNexusEngine) findSymbolDefInShard(branchName, shardName, symbol string) (map[string]interface{}, error) {
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

func (e *GitNexusEngine) findCallersInShard(branchName, shardName, symbol string, depth int) ([]map[string]interface{}, error) {
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

func (e *GitNexusEngine) findCalleesInShard(branchName, shardName, symbol string, depth int) ([]map[string]interface{}, error) {
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
