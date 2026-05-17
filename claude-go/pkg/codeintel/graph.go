// graph.go — 图数据结构 + 社区检测 + God Node 识别。
//
// 零 LLM：纯图算法。
//   - 邻接表图结构
//   - 简化版 Leiden 社区检测（基于模块度优化）
//   - God Node 识别（度数 + PageRank 近似）
package codeintel

import (
	"math"
	"sort"
)

// ============================================================================
// 图数据结构
// ============================================================================

// Graph 邻接表图（有向，支持权重）。
type Graph struct {
	Nodes map[string]*GraphNode
	Edges map[string]map[string]float64 // src -> dst -> weight
}

// GraphNode 图中的节点。
type GraphNode struct {
	ID       string
	Label    string
	Kind     string // function, type, variable
	File     string
	Line     int
	Degree   int
	InDegree int
	OutDegree int
}

// NewGraph 创建空图。
func NewGraph() *Graph {
	return &Graph{
		Nodes: make(map[string]*GraphNode),
		Edges: make(map[string]map[string]float64),
	}
}

// AddNode 添加节点。
func (g *Graph) AddNode(id, label, kind, file string, line int) {
	if _, ok := g.Nodes[id]; !ok {
		g.Nodes[id] = &GraphNode{
			ID:    id,
			Label: label,
			Kind:  kind,
			File:  file,
			Line:  line,
		}
	}
}

// AddEdge 添加边（自动累加权重）。
func (g *Graph) AddEdge(src, dst string, weight float64) {
	if weight == 0 {
		weight = 1.0
	}
	if g.Edges[src] == nil {
		g.Edges[src] = make(map[string]float64)
	}
	g.Edges[src][dst] += weight

	// 更新度数
	if srcNode, ok := g.Nodes[src]; ok {
		srcNode.OutDegree++
		srcNode.Degree++
	}
	if dstNode, ok := g.Nodes[dst]; ok {
		dstNode.InDegree++
		dstNode.Degree++
	}
}

// Neighbors 返回节点的邻居。
func (g *Graph) Neighbors(node string) []string {
	var result []string
	for dst := range g.Edges[node] {
		result = append(result, dst)
	}
	return result
}

// EdgeWeight 返回边的权重。
func (g *Graph) EdgeWeight(src, dst string) float64 {
	if g.Edges[src] == nil {
		return 0
	}
	return g.Edges[src][dst]
}

// TotalWeight 返回图的总权重。
func (g *Graph) TotalWeight() float64 {
	total := 0.0
	for _, edges := range g.Edges {
		for _, w := range edges {
			total += w
		}
	}
	return total
}

// ============================================================================
// 从解析结果构建图
// ============================================================================

// BuildGraphFromParsed 从解析结果构建调用图。
func BuildGraphFromParsed(files []ParsedFile) *Graph {
	g := NewGraph()
	for _, pf := range files {
		// 添加符号节点
		for _, sym := range pf.Symbols {
			g.AddNode(sym.Name, sym.Name, sym.Kind, sym.File, sym.Line)
		}
		// 添加类型节点
		for _, t := range pf.Types {
			g.AddNode(t.Name, t.Name, t.Kind, t.File, t.Line)
		}
	}
	// 添加调用边
	for _, pf := range files {
		for _, call := range pf.Calls {
			if call.Caller != "" && call.Callee != "" {
				g.AddEdge(call.Caller, call.Callee, 1.0)
			}
		}
	}
	return g
}

// ============================================================================
// 简化版 Leiden 社区检测
//
// 基于 Louvain 算法的思想，分两个阶段迭代：
//   1. 局部移动：每个节点移动到使其模块度增益最大的社区
//   2. 社区聚合：将每个社区聚合为一个超级节点，重建图
//
// 与标准 Leiden 的区别：
//   - 使用简化版的 refine 步骤（快速局部移动）
//   - 不做严格的子社区保证（为了性能）
// ============================================================================

// LeidenCommunities 执行简化版 Leiden 算法。
func (g *Graph) LeidenCommunities(resolution float64, maxIterations int) map[string]int {
	if resolution == 0 {
		resolution = 1.0
	}
	if maxIterations == 0 {
		maxIterations = 10
	}

	// 初始化：每个节点一个社区
	communities := make(map[string]int)
	nodeList := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		communities[id] = len(nodeList)
		nodeList = append(nodeList, id)
	}

	totalWeight := g.TotalWeight()
	if totalWeight == 0 {
		return communities
	}

	for iter := 0; iter < maxIterations; iter++ {
		changed := g.localMovePhase(communities, resolution, totalWeight)
		if !changed {
			break
		}
		// 社区聚合阶段
		communities = g.aggregateCommunities(communities)
	}

	return communities
}

// localMovePhase 局部移动阶段。
func (g *Graph) localMovePhase(communities map[string]int, resolution, totalWeight float64) bool {
	changed := false
	nodeDegrees := make(map[string]float64)
	for id := range g.Nodes {
		deg := 0.0
		for _, w := range g.Edges[id] {
			deg += w
		}
		// 也计算入边
		for src, edges := range g.Edges {
			if w, ok := edges[id]; ok {
				deg += w
				_ = src
			}
		}
		nodeDegrees[id] = deg
	}

	// 计算每个社区的总度数
	commDegree := make(map[int]float64)
	for id, comm := range communities {
		commDegree[comm] += nodeDegrees[id]
	}

	for id := range g.Nodes {
		currentComm := communities[id]
		bestComm := currentComm
		bestGain := 0.0

		// 计算移动到每个邻居社区的增益
		commWeights := make(map[int]float64)
		for dst, w := range g.Edges[id] {
			commWeights[communities[dst]] += w
		}
		// 也考虑入边邻居
		for src, edges := range g.Edges {
			if w, ok := edges[id]; ok {
				commWeights[communities[src]] += w
			}
		}

		for comm, weightToComm := range commWeights {
			if comm == currentComm {
				continue
			}
			// 模块度增益近似公式
			gain := weightToComm - resolution*nodeDegrees[id]*commDegree[comm]/(2*totalWeight)
			if gain > bestGain {
				bestGain = gain
				bestComm = comm
			}
		}

		if bestComm != currentComm {
			commDegree[currentComm] -= nodeDegrees[id]
			commDegree[bestComm] += nodeDegrees[id]
			communities[id] = bestComm
			changed = true
		}
	}

	return changed
}

// aggregateCommunities 社区聚合阶段：将社区压缩为超级节点。
func (g *Graph) aggregateCommunities(communities map[string]int) map[string]int {
	// 重新编号社区
	commMap := make(map[int]int)
	nextID := 0
	for _, comm := range communities {
		if _, ok := commMap[comm]; !ok {
			commMap[comm] = nextID
			nextID++
		}
	}

	newCommunities := make(map[string]int)
	for id, comm := range communities {
		newCommunities[id] = commMap[comm]
	}
	return newCommunities
}

// ============================================================================
// God Node 识别
// ============================================================================

// GodNodeResult God Node 识别结果。
type GodNodeResult struct {
	Name       string
	Kind       string
	Degree     int
	InDegree   int
	OutDegree  int
	File       string
	PageRank   float64
}

// IdentifyGodNodes 识别图中的 God Nodes。
// 综合指标：度数 + 近似 PageRank。
func (g *Graph) IdentifyGodNodes(topN int) []GodNodeResult {
	pr := g.approxPageRank(20, 0.85)

	var results []GodNodeResult
	for id, node := range g.Nodes {
		results = append(results, GodNodeResult{
			Name:      node.Label,
			Kind:      node.Kind,
			Degree:    node.Degree,
			InDegree:  node.InDegree,
			OutDegree: node.OutDegree,
			File:      node.File,
			PageRank:  pr[id],
		})
	}

	// 按综合分数排序：PageRank * log(degree+1)
	sort.Slice(results, func(i, j int) bool {
		scoreI := results[i].PageRank * math.Log1p(float64(results[i].Degree))
		scoreJ := results[j].PageRank * math.Log1p(float64(results[j].Degree))
		return scoreI > scoreJ
	})

	if topN > 0 && topN < len(results) {
		results = results[:topN]
	}
	return results
}

// approxPageRank 近似 PageRank（幂迭代法）。
func (g *Graph) approxPageRank(iterations int, damping float64) map[string]float64 {
	n := len(g.Nodes)
	if n == 0 {
		return nil
	}

	pr := make(map[string]float64)
	for id := range g.Nodes {
		pr[id] = 1.0 / float64(n)
	}

	for iter := 0; iter < iterations; iter++ {
		newPR := make(map[string]float64)
		for id := range g.Nodes {
			newPR[id] = (1 - damping) / float64(n)
		}

		for src, edges := range g.Edges {
			outWeight := 0.0
			for _, w := range edges {
				outWeight += w
			}
			if outWeight == 0 {
				continue
			}
			for dst, w := range edges {
				newPR[dst] += damping * pr[src] * w / outWeight
			}
		}
		pr = newPR
	}

	return pr
}

// ============================================================================
// 社区结果转换
// ============================================================================

// ToCommunities 将社区映射转换为 Community 列表。
func (g *Graph) ToCommunities(communities map[string]int) []Community {
	commNodes := make(map[int][]string)
	commFiles := make(map[int]map[string]bool)
	for id, comm := range communities {
		commNodes[comm] = append(commNodes[comm], id)
		if node, ok := g.Nodes[id]; ok {
			if commFiles[comm] == nil {
				commFiles[comm] = make(map[string]bool)
			}
			commFiles[comm][node.File] = true
		}
	}

	var result []Community
	for commID, nodes := range commNodes {
		files := make([]string, 0, len(commFiles[commID]))
		for f := range commFiles[commID] {
			files = append(files, f)
		}

		// 找出社区内度数最高的节点作为 core nodes
		var coreNodes []string
		maxDeg := 0
		for _, id := range nodes {
			if node, ok := g.Nodes[id]; ok {
				if node.Degree > maxDeg {
					maxDeg = node.Degree
					coreNodes = []string{id}
				} else if node.Degree == maxDeg && maxDeg > 0 {
					coreNodes = append(coreNodes, id)
				}
			}
		}
		if len(coreNodes) > 5 {
			coreNodes = coreNodes[:5]
		}

		// 统计边数
		edgeCount := 0
		for _, src := range nodes {
			for dst := range g.Edges[src] {
				if communities[dst] == commID {
					edgeCount++
				}
			}
		}

		result = append(result, Community{
			ID:        commID,
			Files:     files,
			CoreNodes: coreNodes,
			NodeCount: len(nodes),
			EdgeCount: edgeCount,
		})
	}
	return result
}
