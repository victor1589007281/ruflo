// graphify_nollm.go — Graphify 纯算法查询（零 LLM）。
//
// 职责: 直接读取 graphify-out/graph.json，在 Go 中实现 BFS/DFS/最短路径，
//       完全不调用 graphify query/path/explain（这些命令内部使用 LLM 生成摘要）。
// 存储: 使用 DiskGraphIndex 按需磁盘加载 + LRU 缓存，避免全量载入内存。
// 适用场景: 用户明确要求"构建零 LLM"时的查询兜底，或作为 graphify CLI 的加速缓存。
package codeintel

import (
	"sort"
	"strings"
	"time"
)

// ============================================================================
// Graph JSON 数据结构（NetworkX node-link 格式）
// ============================================================================

// GraphNode 图谱节点。
type GraphNode struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	FileType       string `json:"file_type"`
	SourceFile     string `json:"source_file"`
	SourceLocation string `json:"source_location"`
	Community      string `json:"community,omitempty"`
}

// GraphLink 图谱边。
type GraphLink struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Relation   string  `json:"relation"`
	Confidence string  `json:"confidence"`
	SourceFile string  `json:"source_file,omitempty"`
	SourceLoc  string  `json:"source_location,omitempty"`
	Weight     float64 `json:"weight"`
}

// ============================================================================
// 零 LLM 查询实现
// ============================================================================

// GraphifyNoLLM 零 LLM 查询引擎。
type GraphifyNoLLM struct {
	RepoPath string
	disk     *DiskGraphIndex
}

// NewGraphifyNoLLM 创建零 LLM 查询引擎。
func NewGraphifyNoLLM(repoPath string) *GraphifyNoLLM {
	return &GraphifyNoLLM{RepoPath: repoPath}
}

func (g *GraphifyNoLLM) ensureDisk() (*DiskGraphIndex, error) {
	if g.disk != nil {
		return g.disk, nil
	}
	disk, err := OpenDiskGraphIndex(g.RepoPath, 10000, 50000)
	if err != nil {
		return nil, err
	}
	g.disk = disk
	return disk, nil
}

// Close 关闭底层磁盘索引。
func (g *GraphifyNoLLM) Close() error {
	if g.disk != nil {
		return g.disk.Close()
	}
	return nil
}

// QueryBFS BFS 遍历查询（零 LLM）。
// 匹配 label 包含 question 关键词的节点，做 BFS 遍历。
func (g *GraphifyNoLLM) QueryBFS(question string, maxDepth int, relationFilter []string) (*QueryResult, error) {
	start := time.Now()
	disk, err := g.ensureDisk()
	if err != nil {
		return nil, err
	}
	if maxDepth <= 0 {
		maxDepth = 2
	}

	// 1. 找到起始节点（label 匹配 question 中的关键词）
	startNodes := g.findStartNodes(disk, question)
	if len(startNodes) == 0 {
		return wrapResult("graphify_bfs_nollm", mustJSON(map[string]interface{}{
			"question": question,
			"status":   "not_found",
			"message":  "no matching nodes in graph",
		}), start), nil
	}

	// 2. BFS
	visited := make(map[string]bool)
	var resultNodes []*GraphNode
	var resultEdges []*GraphLink
	queue := []bfsItem{}

	for _, n := range startNodes {
		if !visited[n.ID] {
			visited[n.ID] = true
			queue = append(queue, bfsItem{nodeID: n.ID, depth: 0})
			resultNodes = append(resultNodes, n)
		}
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if item.depth >= maxDepth {
			continue
		}
		links := disk.GetEdgesBySource(item.nodeID)
		for _, link := range links {
			if !matchRelationFilter(link.Relation, relationFilter) {
				continue
			}
			resultEdges = append(resultEdges, link)
			if !visited[link.Target] {
				visited[link.Target] = true
				if targetNode, ok := disk.GetNode(link.Target); ok {
					resultNodes = append(resultNodes, targetNode)
					queue = append(queue, bfsItem{nodeID: link.Target, depth: item.depth + 1})
				}
			}
		}
	}

	result := map[string]interface{}{
		"question":        question,
		"algorithm":       "bfs",
		"max_depth":       maxDepth,
		"start_nodes":     len(startNodes),
		"nodes_found":     len(resultNodes),
		"edges_found":     len(resultEdges),
		"relation_filter": relationFilter,
		"nodes":           graphNodesToMaps(resultNodes),
		"edges":           graphLinksToMaps(resultEdges),
		"engine":          "graphify_nollm",
		"llm_tokens":      0,
	}
	return wrapResult("graphify_bfs_nollm", mustJSON(result), start), nil
}

type bfsItem struct {
	nodeID string
	depth  int
}

// PathDijkstra 最短路径（零 LLM）。
// 边权均为 1.0，实现为 BFS 最短路径。
func (g *GraphifyNoLLM) PathDijkstra(src, dst string) (*QueryResult, error) {
	start := time.Now()
	disk, err := g.ensureDisk()
	if err != nil {
		return nil, err
	}

	srcNodes := disk.GetNodesByLabel(src)
	dstNodes := disk.GetNodesByLabel(dst)
	if len(srcNodes) == 0 || len(dstNodes) == 0 {
		return wrapResult("graphify_path_nollm", mustJSON(map[string]interface{}{
			"source":  src,
			"target":  dst,
			"status":  "not_found",
			"message": "source or target node not in graph",
		}), start), nil
	}

	// 对每个 src-dst 组合尝试找最短路径，取 hops 最少的一条
	var bestPath []string
	var bestEdges []map[string]interface{}
	bestLen := -1

	for _, sNode := range srcNodes {
		for _, dNode := range dstNodes {
			path, edges := bfsShortestPath(disk, sNode.ID, dNode.ID)
			if len(path) > 0 && (bestLen == -1 || len(path) < bestLen) {
				bestPath = path
				bestEdges = edges
				bestLen = len(path)
			}
		}
	}

	if bestLen == -1 {
		return wrapResult("graphify_path_nollm", mustJSON(map[string]interface{}{
			"source":  src,
			"target":  dst,
			"status":  "no_path",
			"message": "no path found between source and target",
		}), start), nil
	}

	// 将 ID path 转为 label path
	labelPath := make([]string, len(bestPath))
	for i, id := range bestPath {
		if n, ok := disk.GetNode(id); ok {
			labelPath[i] = n.Label
		} else {
			labelPath[i] = id
		}
	}

	result := map[string]interface{}{
		"source":     src,
		"target":     dst,
		"hops":       len(bestPath) - 1,
		"path_ids":   bestPath,
		"path":       labelPath,
		"edges":      bestEdges,
		"engine":     "graphify_nollm",
		"llm_tokens": 0,
	}
	return wrapResult("graphify_path_nollm", mustJSON(result), start), nil
}

// ExplainNode 节点解释（零 LLM）。
// 返回节点的邻居（1-hop incoming + outgoing）。
func (g *GraphifyNoLLM) ExplainNode(nodeLabel string) (*QueryResult, error) {
	start := time.Now()
	disk, err := g.ensureDisk()
	if err != nil {
		return nil, err
	}

	nodes := disk.GetNodesByLabel(nodeLabel)
	if len(nodes) == 0 {
		return wrapResult("graphify_explain_nollm", mustJSON(map[string]interface{}{
			"node":    nodeLabel,
			"status":  "not_found",
			"message": "node not in graph",
		}), start), nil
	}

	var allNeighbors []map[string]interface{}
	for _, n := range nodes {
		neighbors := explainSingleNode(disk, n)
		allNeighbors = append(allNeighbors, neighbors...)
	}

	result := map[string]interface{}{
		"node":       nodeLabel,
		"matches":    len(nodes),
		"neighbors":  allNeighbors,
		"engine":     "graphify_nollm",
		"llm_tokens": 0,
	}
	return wrapResult("graphify_explain_nollm", mustJSON(result), start), nil
}

// Communities 社区列表（零 LLM）。
// 读取 graph.json 中节点的 community 字段，按社区聚合。
func (g *GraphifyNoLLM) Communities(topN int) (*QueryResult, error) {
	start := time.Now()
	disk, err := g.ensureDisk()
	if err != nil {
		return nil, err
	}

	communityMap := make(map[string][]*GraphNode)
	for _, id := range disk.GetAllNodeIDs() {
		n, ok := disk.GetNode(id)
		if !ok {
			continue
		}
		comm := n.Community
		if comm == "" {
			comm = "uncategorized"
		}
		communityMap[comm] = append(communityMap[comm], n)
	}

	type commStat struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	var stats []commStat
	for name, nodes := range communityMap {
		stats = append(stats, commStat{Name: name, Count: len(nodes)})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Count > stats[j].Count
	})
	if topN > 0 && len(stats) > topN {
		stats = stats[:topN]
	}

	result := map[string]interface{}{
		"communities": stats,
		"total":       len(communityMap),
		"engine":      "graphify_nollm",
		"llm_tokens":  0,
	}
	return wrapResult("graphify_communities_nollm", mustJSON(result), start), nil
}

// GodNodes 高度数节点（零 LLM）。
func (g *GraphifyNoLLM) GodNodes(topN int) (*QueryResult, error) {
	start := time.Now()
	disk, err := g.ensureDisk()
	if err != nil {
		return nil, err
	}

	// 利用内存中的索引计数，无需读取边内容
	degree := make(map[string]int)
	for _, id := range disk.GetAllNodeIDs() {
		// out-degree + in-degree，直接从索引条目计数
		deg := disk.NodeDegree(id)
		if deg > 0 {
			degree[id] = deg
		}
	}

	type nodeDegree struct {
		Node   *GraphNode `json:"node"`
		Degree int        `json:"degree"`
	}
	var list []nodeDegree
	for id, d := range degree {
		if n, ok := disk.GetNode(id); ok {
			list = append(list, nodeDegree{Node: n, Degree: d})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Degree > list[j].Degree
	})
	if topN > 0 && len(list) > topN {
		list = list[:topN]
	}

	result := map[string]interface{}{
		"top_nodes":  list,
		"engine":     "graphify_nollm",
		"llm_tokens": 0,
	}
	return wrapResult("graphify_god_nodes_nollm", mustJSON(result), start), nil
}

// ============================================================================
// DiskGraphIndex 扩展方法（供 GraphifyNoLLM 使用）
// ============================================================================

// NodeDegree 返回节点的总度数（出度 + 入度），利用内存索引直接计数。
func (d *DiskGraphIndex) NodeDegree(id string) int {
	return len(d.idx.EdgesBySource[id]) + len(d.idx.EdgesByTarget[id])
}

// ============================================================================
// 私有辅助
// ============================================================================

func (g *GraphifyNoLLM) findStartNodes(disk *DiskGraphIndex, question string) []*GraphNode {
	words := strings.Fields(strings.ToLower(question))
	var startNodes []*GraphNode
	seen := make(map[string]bool)

	for _, word := range words {
		word = strings.TrimRight(word, "?.,!;:")
		if len(word) < 3 {
			continue
		}
		// 遍历所有节点 ID，检查 label 是否匹配
		for _, id := range disk.GetAllNodeIDs() {
			if seen[id] {
				continue
			}
			n, ok := disk.GetNode(id)
			if !ok {
				continue
			}
			if strings.Contains(strings.ToLower(n.Label), word) {
				seen[id] = true
				startNodes = append(startNodes, n)
			}
		}
	}
	return startNodes
}

func matchRelationFilter(relation string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if strings.EqualFold(relation, f) || strings.Contains(strings.ToLower(relation), strings.ToLower(f)) {
			return true
		}
	}
	return false
}

func bfsShortestPath(disk *DiskGraphIndex, srcID, dstID string) ([]string, []map[string]interface{}) {
	if srcID == dstID {
		return []string{srcID}, nil
	}

	visited := make(map[string]string) // node -> previous node
	edgeUsed := make(map[string]*GraphLink)
	queue := []string{srcID}
	visited[srcID] = srcID

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		links := disk.GetEdgesBySource(curr)
		for _, link := range links {
			next := link.Target
			if _, ok := visited[next]; !ok {
				visited[next] = curr
				edgeUsed[next] = link
				if next == dstID {
					// 回溯路径
					path := []string{dstID}
					edges := []map[string]interface{}{}
					for p := dstID; p != srcID; p = visited[p] {
						path = append([]string{visited[p]}, path...)
						if e, ok := edgeUsed[p]; ok {
							edges = append([]map[string]interface{}{graphLinkToMap(e)}, edges...)
						}
					}
					return path, edges
				}
				queue = append(queue, next)
			}
		}
	}
	return nil, nil
}

func explainSingleNode(disk *DiskGraphIndex, n *GraphNode) []map[string]interface{} {
	var out []map[string]interface{}
	for _, link := range disk.GetEdgesBySource(n.ID) {
		target, ok := disk.GetNode(link.Target)
		if !ok {
			continue
		}
		out = append(out, map[string]interface{}{
			"direction":  "outgoing",
			"relation":   link.Relation,
			"node":       graphNodeToMap(target),
			"confidence": link.Confidence,
		})
	}
	for _, link := range disk.GetEdgesByTarget(n.ID) {
		source, ok := disk.GetNode(link.Source)
		if !ok {
			continue
		}
		out = append(out, map[string]interface{}{
			"direction":  "incoming",
			"relation":   link.Relation,
			"node":       graphNodeToMap(source),
			"confidence": link.Confidence,
		})
	}
	return out
}

func graphNodeToMap(n *GraphNode) map[string]interface{} {
	return map[string]interface{}{
		"id":              n.ID,
		"label":           n.Label,
		"file_type":       n.FileType,
		"source_file":     n.SourceFile,
		"source_location": n.SourceLocation,
		"community":       n.Community,
	}
}

func graphNodesToMaps(nodes []*GraphNode) []map[string]interface{} {
	out := make([]map[string]interface{}, len(nodes))
	for i, n := range nodes {
		out[i] = graphNodeToMap(n)
	}
	return out
}

func graphLinkToMap(l *GraphLink) map[string]interface{} {
	return map[string]interface{}{
		"source":     l.Source,
		"target":     l.Target,
		"relation":   l.Relation,
		"confidence": l.Confidence,
		"weight":     l.Weight,
	}
}

func graphLinksToMaps(links []*GraphLink) []map[string]interface{} {
	out := make([]map[string]interface{}, len(links))
	for i, l := range links {
		out[i] = graphLinkToMap(l)
	}
	return out
}
