// query.go — 查询引擎。
//
// 提供结构查询（类 GitNexus）和语义查询（类 Graphify）的统一入口。
// 查询路由：
//   - navigate / impact / find_refs → 结构查询（SQLite AST 索引）
//   - communities / god_nodes / path / surprises → 语义查询（图数据）
//   - cross_shard → 跨片查询（KuzuDB 占位）
package codeintel

import (
	"database/sql"
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

	// 未指定分片时，尝试在所有分片中查找
	shards := e.resolveShards(branchName, q.Shard)
	if len(shards) == 0 {
		return nil, fmt.Errorf("no shards available")
	}

	var results []map[string]interface{}
	for _, shardName := range shards {
		db, err := e.Store.OpenShardDB(branchName, shardName)
		if err != nil {
			continue
		}
		defer db.Close()

		def, err := e.findSymbolDef(db, q.Symbol)
		if err != nil {
			continue
		}
		callers, _ := e.findCallers(db, q.Symbol, q.Depth)
		callees, _ := e.findCallees(db, q.Symbol, q.Depth)

		results = append(results, map[string]interface{}{
			"shard":    shardName,
			"symbol":   q.Symbol,
			"def":      def,
			"callers":  callers,
			"callees":  callees,
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
	// TODO: real impact analysis using callgraph + type dependencies
	result := map[string]interface{}{
		"file":         q.FilePath,
		"direct_deps":  []string{},
		"transitive":   []string{},
		"top_callers":  []string{},
		"note":         "impact analysis placeholder — integrate callgraph for real results",
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
		"shard":       q.Shard,
		"communities": idx.Communities,
		"god_nodes":   idx.GodNodes,
		"file_count":  idx.FileCount,
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
		"shard":    shardName,
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
	// TODO: real cross-shard query via KuzuDB
	result := map[string]interface{}{
		"symbol":      q.Symbol,
		"target_shard": q.TargetShard,
		"call_sites":  []string{},
		"note":        "cross-shard query placeholder — integrate KuzuDB for real results",
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
		"repo_path":    cfg.RepoPath,
		"repo_hash":    cfg.RepoHash,
		"shard_count":  len(cfg.Shards),
		"branches":     branchInfo,
		"llm_enhance":  cfg.LLMEnhance,
		"auto_shard":   cfg.AutoShard,
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
// 私有辅助
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

func (e *Engine) findSymbolDef(db *sql.DB, symbol string) (map[string]interface{}, error) {
	// TODO: real SQLite query
	return map[string]interface{}{"symbol": symbol, "note": "placeholder"}, nil
}

func (e *Engine) findCallers(db *sql.DB, symbol string, depth int) ([]map[string]interface{}, error) {
	// TODO: real SQLite query
	return nil, nil
}

func (e *Engine) findCallees(db *sql.DB, symbol string, depth int) ([]map[string]interface{}, error) {
	// TODO: real SQLite query
	return nil, nil
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
	QueryNavigate   = "navigate"
	QueryImpact     = "impact"
	QueryCommunities = "communities"
	QueryGodNodes   = "god_nodes"
	QueryCrossShard = "cross_shard"
	QueryStatus     = "status"
)
