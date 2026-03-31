package memory

import "testing"

func TestMemoryGraph_AddAndGet(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph()
	g.AddNode(&GraphNode{ID: "a", Label: "A"})
	g.AddNode(&GraphNode{ID: "b", Label: "B"})
	g.AddEdge(&GraphEdge{From: "a", To: "b", Weight: 1})
	n := g.GetNeighbors("a")
	if len(n) != 1 || n[0].To != "b" || n[0].Weight != 1 {
		t.Fatalf("neighbors %+v", n)
	}
}

func TestMemoryGraph_FindPath(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph()
	for _, id := range []string{"a", "b", "c"} {
		g.AddNode(&GraphNode{ID: id})
	}
	g.AddEdge(&GraphEdge{From: "a", To: "b"})
	g.AddEdge(&GraphEdge{From: "b", To: "c"})
	p := g.FindPath("a", "c")
	if len(p) != 3 || p[0] != "a" || p[2] != "c" {
		t.Fatalf("path %v", p)
	}
}

func TestMemoryGraph_GetSubgraph(t *testing.T) {
	t.Parallel()
	g := NewMemoryGraph()
	for _, id := range []string{"a", "b", "c"} {
		g.AddNode(&GraphNode{ID: id})
	}
	g.AddEdge(&GraphEdge{From: "a", To: "b"})
	g.AddEdge(&GraphEdge{From: "b", To: "c"})
	sub := g.GetSubgraph("a", 2)
	if len(sub) < 3 {
		t.Fatalf("subgraph size %d", len(sub))
	}
}
