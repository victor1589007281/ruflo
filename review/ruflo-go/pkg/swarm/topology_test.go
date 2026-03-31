package swarm

import (
	"testing"

	"github.com/ruflo/ruflo-go/api"
)

func TestTopologyGetState(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyMesh})
	_ = tm.AddNode("n1", "coder")
	_ = tm.AddNode("n2", "coder")
	st := tm.GetState()
	if st.NodeCount != 2 || st.Type != api.TopologyMesh {
		t.Fatalf("GetState: %#v", st)
	}
	if st.EdgeCount < 1 {
		t.Fatalf("expected edges in mesh: %#v", st)
	}
}

func TestTopologyUpdateNode(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyHierarchical})
	_ = tm.AddNode("root", "queen")
	_ = tm.AddNode("leaf", "coder")
	if err := tm.UpdateNode("leaf", api.AgentTypeArchitect); err != nil {
		t.Fatal(err)
	}
	n, ok := tm.GetNode("leaf")
	if !ok || n.Role != "architect" {
		t.Fatalf("role: %#v ok=%v", n, ok)
	}
}

func TestTopologyGetNode(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyMesh})
	_ = tm.AddNode("solo", "worker")
	n, ok := tm.GetNode("solo")
	if !ok || n.AgentID != "solo" {
		t.Fatalf("GetNode: %#v", n)
	}
}

func TestTopologyGetNodesByRole(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyMesh})
	_ = tm.AddNode("c1", "coder")
	_ = tm.AddNode("c2", "coder")
	_ = tm.AddNode("r1", "reviewer")
	nodes := tm.GetNodesByRole(api.AgentTypeCoder)
	if len(nodes) != 2 {
		t.Fatalf("coders: %d", len(nodes))
	}
}

func TestTopologyGetActiveNodes(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyMesh})
	_ = tm.AddNode("a", "x")
	_ = tm.AddNode("b", "y")
	if len(tm.GetActiveNodes()) != 2 {
		t.Fatalf("active: %d", len(tm.GetActiveNodes()))
	}
}

func TestTopologyIsConnected(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyHierarchical})
	_ = tm.AddNode("q", "queen")
	_ = tm.AddNode("w1", "coder")
	_ = tm.AddNode("w2", "coder")
	if !tm.IsConnected() {
		t.Fatal("expected connected star")
	}
}

func TestTopologyIsConnectedDisconnected(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyHierarchical})
	_ = tm.AddNode("q", "queen")
	_ = tm.AddNode("w1", "coder")
	_ = tm.AddNode("w2", "coder")
	_ = tm.RemoveNode("q")
	if tm.IsConnected() {
		t.Fatal("expected disconnected graph after removing hub")
	}
}

func TestTopologyGetConnectionCount(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyHierarchical})
	_ = tm.AddNode("hub", "queen")
	_ = tm.AddNode("a", "coder")
	_ = tm.AddNode("b", "coder")
	ec := tm.GetConnectionCount()
	if ec != 2 {
		t.Fatalf("edges: %d", ec)
	}
}

func TestTopologyGetAverageConnections(t *testing.T) {
	t.Parallel()
	tm := NewTopologyManager(TopologyConfig{Type: api.TopologyHierarchical})
	_ = tm.AddNode("hub", "queen")
	_ = tm.AddNode("a", "coder")
	avg := tm.GetAverageConnections()
	if avg <= 0 {
		t.Fatalf("avg: %v", avg)
	}
}
