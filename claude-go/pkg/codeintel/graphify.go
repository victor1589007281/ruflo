// graphify.go — Graphify 语义查询引擎。
//
// 职责: 语义层面的图谱分析，基于图数据（社区检测、路径分析、异常检测）。
// 查询类型:
//   - communities  → Leiden 社区列表
//   - god_nodes    → 高度数枢纽节点
//   - path         → 两个符号间的最短路径
//   - surprises    → 异常边（跨社区高权重边）
//
// 零 LLM：纯图算法（Leiden + PageRank + BFS）。
package codeintel

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GraphifyEngine 语义查询引擎。
type GraphifyEngine struct {
	Store *Store
}

// NewGraphifyEngine 创建 Graphify 引擎。
func NewGraphifyEngine(repoPath string) *GraphifyEngine {
	return &GraphifyEngine{Store: NewStore(repoPath)}
}

// Communities 社区列表查询。
func (e *GraphifyEngine) Communities(branchName string, q CommunityQuery) (*QueryResult, error) {
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
func (e *GraphifyEngine) GodNodes(branchName, shardName string, topN int) (*QueryResult, error) {
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

// Path 两个符号间的最短路径（BFS）。
func (e *GraphifyEngine) Path(branchName string, src, dst string) (*QueryResult, error) {
	start := time.Now()

	// 加载所有分片的调用图做全局 BFS
	shards, _ := e.listShards(branchName)
	graph := NewGraph()
	for _, shardName := range shards {
		cgPath := e.Store.CallgraphPath(branchName, shardName)
		data, err := os.ReadFile(cgPath)
		if err != nil {
			continue
		}
		var cg struct {
			Edges []map[string]interface{} `json:"edges"`
		}
		if err := json.Unmarshal(data, &cg); err != nil {
			continue
		}
		for _, edge := range cg.Edges {
			s, _ := edge["source"].(string)
			t, _ := edge["target"].(string)
			if s != "" && t != "" {
				graph.AddNode(s, s, "function", "", 0)
				graph.AddNode(t, t, "function", "", 0)
				graph.AddEdge(s, t, 1.0)
			}
		}
	}

	path := bfsPath(graph, src, dst)
	result := map[string]interface{}{
		"source":      src,
		"target":      dst,
		"path":        path,
		"path_length": len(path),
		"found":       len(path) > 0,
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "path",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// Surprises 异常边检测：跨社区的高权重边、意外的调用关系。
func (e *GraphifyEngine) Surprises(branchName, shardName string, topN int) (*QueryResult, error) {
	start := time.Now()

	// 加载调用图和社区信息
	cgPath := e.Store.CallgraphPath(branchName, shardName)
	data, err := os.ReadFile(cgPath)
	if err != nil {
		return nil, fmt.Errorf("load callgraph: %w", err)
	}
	var cg struct {
		Nodes []map[string]interface{} `json:"nodes"`
		Edges []map[string]interface{} `json:"edges"`
	}
	if err := json.Unmarshal(data, &cg); err != nil {
		return nil, err
	}

	idx, err := e.Store.LoadShardIndex(branchName, shardName)
	if err != nil {
		return nil, err
	}

	// 构建符号到社区的映射
	symbolComm := make(map[string]int)
	for _, comm := range idx.Communities {
		for _, node := range comm.CoreNodes {
			symbolComm[node] = comm.ID
		}
	}

	// 找出跨社区边和高权重边
	var surprises []map[string]interface{}
	for _, edge := range cg.Edges {
		src, _ := edge["source"].(string)
		dst, _ := edge["target"].(string)
		weight, _ := edge["weight"].(float64)
		if weight == 0 {
			weight = 1.0
		}

		srcComm, sOK := symbolComm[src]
		dstComm, dOK := symbolComm[dst]
		crossComm := sOK && dOK && srcComm != dstComm
		highWeight := weight > 5.0

		if crossComm || highWeight {
			surprises = append(surprises, map[string]interface{}{
				"source":       src,
				"target":       dst,
				"weight":       weight,
				"cross_comm":   crossComm,
				"src_comm":     srcComm,
				"dst_comm":     dstComm,
			})
		}
	}

	// 按权重排序，取 topN
	sort.Slice(surprises, func(i, j int) bool {
		wi, _ := surprises[i]["weight"].(float64)
		wj, _ := surprises[j]["weight"].(float64)
		return wi > wj
	})
	if topN > 0 && topN < len(surprises) {
		surprises = surprises[:topN]
	}

	result := map[string]interface{}{
		"shard":     shardName,
		"surprises": surprises,
		"count":     len(surprises),
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "surprises",
		Shard:     shardName,
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// LoadShardGraph 加载分片的 NetworkX 风格图数据（JSON）。
func (e *GraphifyEngine) LoadShardGraph(branchName, shardName string) (map[string]interface{}, error) {
	path := filepath.Join(e.Store.GraphifyPath(branchName, shardName), "graph.json")
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

// ============================================================================
// 私有辅助
// ============================================================================

func (e *GraphifyEngine) listShards(branchName string) ([]string, error) {
	idx, err := e.Store.LoadBranchIndex(branchName)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range idx.Shards {
		names = append(names, name)
	}
	return names, nil
}

// bfsPath 在图中找最短路径（无向 BFS）。
func bfsPath(g *Graph, src, dst string) []string {
	if src == dst {
		return []string{src}
	}
	visited := make(map[string]bool)
	parent := make(map[string]string)
	queue := []string{src}
	visited[src] = true

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		// 出边
		for neighbor := range g.Edges[curr] {
			if !visited[neighbor] {
				visited[neighbor] = true
				parent[neighbor] = curr
				if neighbor == dst {
					return reconstructPath(parent, src, dst)
				}
				queue = append(queue, neighbor)
			}
		}
		// 入边（反向搜索）
		for from, edges := range g.Edges {
			if _, ok := edges[curr]; ok && !visited[from] {
				visited[from] = true
				parent[from] = curr
				if from == dst {
					return reconstructPath(parent, src, dst)
				}
				queue = append(queue, from)
			}
		}
	}
	return nil
}

func reconstructPath(parent map[string]string, src, dst string) []string {
	var path []string
	curr := dst
	for curr != src {
		path = append([]string{curr}, path...)
		curr = parent[curr]
	}
	path = append([]string{src}, path...)
	return path
}

// approxBetweenness 近似 Betweenness Centrality（抽样 BFS）。
func approxBetweenness(g *Graph, sampleCount int) map[string]float64 {
	nodes := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		nodes = append(nodes, id)
	}
	if len(nodes) == 0 {
		return nil
	}
	if sampleCount > len(nodes) {
		sampleCount = len(nodes)
	}

	bc := make(map[string]float64)
	for i := 0; i < sampleCount; i++ {
		s := nodes[i]
		// BFS 从 s 出发
		visited := make(map[string]bool)
		dist := make(map[string]int)
		pred := make(map[string][]string)
		sigma := make(map[string]float64)
		queue := []string{s}
		visited[s] = true
		dist[s] = 0
		sigma[s] = 1.0

		var order []string
		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			order = append(order, v)
			for w := range g.Edges[v] {
				if !visited[w] {
					visited[w] = true
					dist[w] = dist[v] + 1
					queue = append(queue, w)
				}
				if dist[w] == dist[v]+1 {
					sigma[w] += sigma[v]
					pred[w] = append(pred[w], v)
				}
			}
		}

		delta := make(map[string]float64)
		for i := len(order) - 1; i >= 0; i-- {
			w := order[i]
			for _, v := range pred[w] {
				delta[v] += (sigma[v] / sigma[w]) * (1.0 + delta[w])
			}
			if w != s {
				bc[w] += delta[w]
			}
		}
	}

	// 归一化
	n := float64(len(nodes))
	if n > 2 {
		scale := 2.0 / ((n - 1) * (n - 2))
		for v := range bc {
			bc[v] *= scale
		}
	}
	return bc
}

// communityModularity 计算给定社区划分的模块度。
func communityModularity(g *Graph, communities map[string]int) float64 {
	totalWeight := g.TotalWeight()
	if totalWeight == 0 {
		return 0
	}

	// 计算每个社区的内外边权重
	commWeights := make(map[int]float64)
	commInternal := make(map[int]float64)

	for src, edges := range g.Edges {
		srcComm := communities[src]
		for dst, w := range edges {
			dstComm := communities[dst]
			commWeights[srcComm] += w
			if srcComm == dstComm {
				commInternal[srcComm] += w
			}
		}
	}

	modularity := 0.0
	for comm, internal := range commInternal {
		total := commWeights[comm]
		if total > 0 {
			modularity += internal/totalWeight - math.Pow(total/(2*totalWeight), 2)
		}
	}
	return modularity
}
