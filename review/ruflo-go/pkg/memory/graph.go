package memory

import (
	"sync"
)

// MemoryGraph is an in-memory directed graph over memory-related nodes.
type MemoryGraph struct {
	mu    sync.RWMutex
	nodes map[string]*GraphNode
	edges map[string][]*GraphEdge
}

// GraphNode is a vertex in the memory graph.
type GraphNode struct {
	ID         string
	EntryKey   string
	Namespace  string
	Label      string
	Properties map[string]string
}

// GraphEdge is a directed labeled edge.
type GraphEdge struct {
	From     string
	To       string
	Relation string
	Weight   float64
}

// NewMemoryGraph constructs an empty graph.
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

// RemoveNode deletes a node and edges touching it.
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

// GetNode returns a copy of the node, or nil.
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
