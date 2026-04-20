package orchestrator

import (
	"testing"
)

func TestNewGraph(t *testing.T) {
	g := NewGraph("test-1", "Test Graph")
	if g.ID != "test-1" {
		t.Errorf("expected ID test-1, got %s", g.ID)
	}
	if g.TaskCount() != 0 {
		t.Errorf("expected 0 tasks, got %d", g.TaskCount())
	}
}

func TestGraph_AddTask(t *testing.T) {
	g := NewGraph("g1", "test")
	err := g.AddTask(&Task{ID: "a", Name: "Task A", Runner: "noop"})
	if err != nil {
		t.Fatal(err)
	}
	if g.TaskCount() != 1 {
		t.Errorf("expected 1 task, got %d", g.TaskCount())
	}

	// Duplicate
	err = g.AddTask(&Task{ID: "a", Name: "Task A Dup", Runner: "noop"})
	if err == nil {
		t.Error("expected duplicate error")
	}
}

func TestGraph_AddEdge_Invalid(t *testing.T) {
	g := NewGraph("g1", "test")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	err := g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	if err == nil {
		t.Error("expected error for missing target task")
	}
	err = g.AddEdge(Edge{From: "c", To: "a", Kind: EdgeDependency})
	if err == nil {
		t.Error("expected error for missing source task")
	}
}

func TestGraph_Build_CycleDetection(t *testing.T) {
	g := NewGraph("g1", "cycle")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.AddTask(&Task{ID: "b", Runner: "noop"})
	g.AddTask(&Task{ID: "c", Runner: "noop"})

	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "b", To: "c", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "c", To: "a", Kind: EdgeDependency})

	err := g.Build()
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
}

func TestGraph_Build_LinearDAG(t *testing.T) {
	g := NewGraph("g1", "linear")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.AddTask(&Task{ID: "b", Runner: "noop"})
	g.AddTask(&Task{ID: "c", Runner: "noop"})

	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "b", To: "c", Kind: EdgeDependency})

	if err := g.Build(); err != nil {
		t.Fatal(err)
	}

	// "a" should be Ready, "b" and "c" should be Blocked
	if g.Tasks["a"].State() != TaskReady {
		t.Errorf("expected a=Ready, got %s", g.Tasks["a"].State())
	}
	if g.Tasks["b"].State() != TaskBlocked {
		t.Errorf("expected b=Blocked, got %s", g.Tasks["b"].State())
	}
	if g.Tasks["c"].State() != TaskBlocked {
		t.Errorf("expected c=Blocked, got %s", g.Tasks["c"].State())
	}
}

func TestGraph_Build_DiamondDAG(t *testing.T) {
	//   a
	//  / \
	// b   c
	//  \ /
	//   d
	g := NewGraph("g1", "diamond")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.AddTask(&Task{ID: "b", Runner: "noop"})
	g.AddTask(&Task{ID: "c", Runner: "noop"})
	g.AddTask(&Task{ID: "d", Runner: "noop"})

	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "a", To: "c", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "b", To: "d", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "c", To: "d", Kind: EdgeDependency})

	if err := g.Build(); err != nil {
		t.Fatal(err)
	}

	if g.Tasks["a"].State() != TaskReady {
		t.Error("a should be Ready")
	}
	if g.Tasks["d"].State() != TaskBlocked {
		t.Error("d should be Blocked")
	}

	if w := g.DAGWidth(); w != 2 {
		t.Errorf("expected DAGWidth 2, got %d", w)
	}
}

func TestGraph_CriticalPath(t *testing.T) {
	g := NewGraph("g1", "cp")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.AddTask(&Task{ID: "b", Runner: "noop"})
	g.AddTask(&Task{ID: "c", Runner: "noop"})
	g.AddTask(&Task{ID: "d", Runner: "noop"})

	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "b", To: "c", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "a", To: "d", Kind: EdgeDependency})

	g.Build()
	cp := g.CriticalPath()
	if len(cp) != 3 {
		t.Errorf("expected critical path length 3, got %d: %v", len(cp), cp)
	}
}

func TestGraph_ReadyTasks(t *testing.T) {
	g := NewGraph("g1", "ready")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.AddTask(&Task{ID: "b", Runner: "noop"})
	g.AddTask(&Task{ID: "c", Runner: "noop"})

	g.AddEdge(Edge{From: "a", To: "c", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "b", To: "c", Kind: EdgeDependency})

	g.Build()

	ready := g.ReadyTasks()
	if len(ready) != 2 {
		t.Errorf("expected 2 ready tasks, got %d", len(ready))
	}
}

func TestGraph_Stats(t *testing.T) {
	g := NewGraph("g1", "stats")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.AddTask(&Task{ID: "b", Runner: "noop"})
	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	g.Build()

	stats := g.Stats()
	if stats[TaskReady] != 1 {
		t.Errorf("expected 1 ready, got %d", stats[TaskReady])
	}
	if stats[TaskBlocked] != 1 {
		t.Errorf("expected 1 blocked, got %d", stats[TaskBlocked])
	}
}

func TestTaskState_String(t *testing.T) {
	tests := []struct {
		state    TaskState
		expected string
	}{
		{TaskPending, "pending"},
		{TaskBlocked, "blocked"},
		{TaskReady, "ready"},
		{TaskRunning, "running"},
		{TaskCompleted, "completed"},
		{TaskFailed, "failed"},
		{TaskCancelled, "cancelled"},
		{TaskSuspended, "suspended"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("TaskState(%d).String() = %s, want %s", tt.state, got, tt.expected)
		}
	}
}

func TestTaskState_IsTerminal(t *testing.T) {
	if !TaskCompleted.IsTerminal() {
		t.Error("completed should be terminal")
	}
	if !TaskFailed.IsTerminal() {
		t.Error("failed should be terminal")
	}
	if !TaskCancelled.IsTerminal() {
		t.Error("cancelled should be terminal")
	}
	if TaskRunning.IsTerminal() {
		t.Error("running should not be terminal")
	}
	if TaskSuspended.IsTerminal() {
		t.Error("suspended should not be terminal")
	}
}
