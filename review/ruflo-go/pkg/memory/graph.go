// 知识图谱（memory 包）：邻接表 edges[from][]*GraphEdge 表示有向边，nodes 存顶点；边上 Relation/Weight 表达语义关系与强度。
// FindPath 用 BFS 求最短 hop 路径；GetSubgraph/Traverse 限制深度，并沿入边反向扩展使子图在无向意义上连通。
package memory

import (
	"sync"
)

// MemoryGraph 进程内图结构：nodes 为顶点表，edges 为邻接表（出边列表）。
type MemoryGraph struct {
	mu    sync.RWMutex            // 读写锁隔离并发
	nodes map[string]*GraphNode   // 顶点 id -> 结点
	edges map[string][]*GraphEdge // 起点 id -> 出边切片
}

// GraphNode 表示与某条记忆或实体绑定的顶点。
type GraphNode struct {
	ID         string            // 顶点全局唯一 id
	EntryKey   string            // 可选：关联的记忆键
	Namespace  string            // 可选：记忆命名空间
	Label      string            // 人类可读标签
	Properties map[string]string // 任意字符串属性
}

// GraphEdge 有向边，表达主体-关系-客体三元组的一部分语义。
type GraphEdge struct {
	From     string  // 起点顶点 id
	To       string  // 终点顶点 id
	Relation string  // 关系类型/谓词
	Weight   float64 // 边权（可选，用于排序或衰减）
}

// NewMemoryGraph 创建空图。
func NewMemoryGraph() *MemoryGraph {
	return &MemoryGraph{
		nodes: make(map[string]*GraphNode),
		edges: make(map[string][]*GraphEdge),
	}
}

func cloneNode(n *GraphNode) *GraphNode {
	if n == nil {
		return nil
	}
	cp := *n
	if n.Properties != nil {
		cp.Properties = make(map[string]string, len(n.Properties))
		for k, v := range n.Properties {
			cp.Properties[k] = v
		}
	}
	return &cp
}

// AddNode inserts or replaces a node by ID.
func (g *MemoryGraph) AddNode(n *GraphNode) {
	if g == nil || n == nil || n.ID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = cloneNode(n)
}

// AddEdge appends an edge from From to To.
func (g *MemoryGraph) AddEdge(e *GraphEdge) {
	if g == nil || e == nil || e.From == "" || e.To == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ec := *e
	g.edges[e.From] = append(g.edges[e.From], &ec)
}

// RemoveNode 删除顶点并清扫所有指向该点的入边。
func (g *MemoryGraph) RemoveNode(id string) {
	if g == nil || id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.nodes, id)
	delete(g.edges, id)
	for from, list := range g.edges {
		kept := list[:0]
		for _, e := range list {
			if e != nil && e.To != id {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(g.edges, from)
		} else {
			g.edges[from] = kept
		}
	}
}

// GetNode 返回结点防御性拷贝，不存在则 nil。
func (g *MemoryGraph) GetNode(id string) *GraphNode {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return cloneNode(g.nodes[id])
}

// GetNeighbors returns outgoing edges from id (copies).
func (g *MemoryGraph) GetNeighbors(id string) []*GraphEdge {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	list := g.edges[id]
	out := make([]*GraphEdge, 0, len(list))
	for _, e := range list {
		if e == nil {
			continue
		}
		ec := *e
		out = append(out, &ec)
	}
	return out
}

// FindPath returns the shortest path From -> To using BFS (node IDs), or nil.
func (g *MemoryGraph) FindPath(from, to string) []string {
	if g == nil || from == "" || to == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.nodes[from] == nil || g.nodes[to] == nil {
		return nil
	}
	if from == to {
		return []string{from}
	}
	type qitem struct {
		id   string
		path []string
	}
	queue := []qitem{{id: from, path: []string{from}}}
	seen := map[string]struct{}{from: {}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.edges[cur.id] {
			if e == nil {
				continue
			}
			next := e.To
			if _, ok := seen[next]; ok {
				continue
			}
			path := append(append([]string(nil), cur.path...), next)
			if next == to {
				return path
			}
			if g.nodes[next] != nil {
				seen[next] = struct{}{}
				queue = append(queue, qitem{id: next, path: path})
			}
		}
	}
	return nil
}

// GetSubgraph collects nodes reachable within depth undirected hops from startId.
func (g *MemoryGraph) GetSubgraph(startId string, depth int) map[string]*GraphNode {
	out := make(map[string]*GraphNode)
	if g == nil || startId == "" || depth < 0 {
		return out
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.nodes[startId] == nil {
		return out
	}
	type st struct {
		id    string
		depth int
	}
	queue := []st{{id: startId, depth: 0}}
	seen := map[string]int{startId: 0}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if n := g.nodes[cur.id]; n != nil {
			out[cur.id] = cloneNode(n)
		}
		if cur.depth >= depth {
			continue
		}
		for _, e := range g.edges[cur.id] {
			if e == nil {
				continue
			}
			nd := cur.depth + 1
			if prev, ok := seen[e.To]; !ok || nd < prev {
				seen[e.To] = nd
				if g.nodes[e.To] != nil {
					queue = append(queue, st{id: e.To, depth: nd})
				}
			}
		}
		for from, list := range g.edges {
			for _, e := range list {
				if e == nil || e.To != cur.id {
					continue
				}
				nd := cur.depth + 1
				if prev, ok := seen[from]; !ok || nd < prev {
					seen[from] = nd
					if g.nodes[from] != nil {
						queue = append(queue, st{id: from, depth: nd})
					}
				}
			}
		}
	}
	return out
}

// Traverse visits nodes BFS up to depth from startId.
func (g *MemoryGraph) Traverse(startId string, depth int, fn func(node *GraphNode)) {
	if g == nil || fn == nil || startId == "" || depth < 0 {
		return
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.nodes[startId] == nil {
		return
	}
	type st struct {
		id    string
		depth int
	}
	queue := []st{{id: startId, depth: 0}}
	seen := map[string]int{startId: 0}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if n := g.nodes[cur.id]; n != nil {
			fn(cloneNode(n))
		}
		if cur.depth >= depth {
			continue
		}
		for _, e := range g.edges[cur.id] {
			if e == nil {
				continue
			}
			nd := cur.depth + 1
			if prev, ok := seen[e.To]; !ok || nd < prev {
				seen[e.To] = nd
				if g.nodes[e.To] != nil {
					queue = append(queue, st{id: e.To, depth: nd})
				}
			}
		}
		for from, list := range g.edges {
			for _, e := range list {
				if e == nil || e.To != cur.id {
					continue
				}
				nd := cur.depth + 1
				if prev, ok := seen[from]; !ok || nd < prev {
					seen[from] = nd
					if g.nodes[from] != nil {
						queue = append(queue, st{id: from, depth: nd})
					}
				}
			}
		}
	}
}
