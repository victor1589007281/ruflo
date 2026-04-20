package orchestrator

import (
	"testing"
)

func buildDiamondGraph() *Graph {
	g := NewGraph("sched-test", "diamond")
	g.AddTask(&Task{ID: "a", Name: "A", Runner: "noop", Priority: 1})
	g.AddTask(&Task{ID: "b", Name: "B", Runner: "noop", Priority: 5})
	g.AddTask(&Task{ID: "c", Name: "C", Runner: "noop", Priority: 3})
	g.AddTask(&Task{ID: "d", Name: "D", Runner: "noop", Priority: 2})

	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "a", To: "c", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "b", To: "d", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "c", To: "d", Kind: EdgeDependency})
	g.Build()
	return g
}

func TestScheduler_InitialSchedule(t *testing.T) {
	g := buildDiamondGraph()
	s := NewScheduler()

	ctx := &SchedulerContext{
		Graph:      g,
		RunningIDs: map[string]bool{},
	}

	batch := s.Schedule(ctx, 10)
	if len(batch) != 1 {
		t.Errorf("expected 1 task (only 'a' is ready), got %d", len(batch))
	}
	if batch[0].ID != "a" {
		t.Errorf("expected task 'a', got %s", batch[0].ID)
	}
}

func TestScheduler_PriorityOrdering(t *testing.T) {
	g := NewGraph("prio", "priority test")
	g.AddTask(&Task{ID: "low", Name: "Low", Runner: "noop", Priority: 1})
	g.AddTask(&Task{ID: "high", Name: "High", Runner: "noop", Priority: 10})
	g.AddTask(&Task{ID: "mid", Name: "Mid", Runner: "noop", Priority: 5})
	g.Build()

	s := NewScheduler()
	ctx := &SchedulerContext{
		Graph:      g,
		RunningIDs: map[string]bool{},
	}

	batch := s.Schedule(ctx, 10)
	if len(batch) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(batch))
	}
	if batch[0].ID != "high" {
		t.Errorf("expected 'high' first, got %s", batch[0].ID)
	}
}

func TestScheduler_MaxBatch(t *testing.T) {
	g := NewGraph("batch", "batch test")
	for i := 0; i < 20; i++ {
		g.AddTask(&Task{ID: "t" + string(rune('a'+i)), Runner: "noop"})
	}
	g.Build()

	s := NewScheduler()
	ctx := &SchedulerContext{
		Graph:      g,
		RunningIDs: map[string]bool{},
	}

	batch := s.Schedule(ctx, 5)
	if len(batch) != 5 {
		t.Errorf("expected max 5 tasks, got %d", len(batch))
	}
}

func TestScheduler_EmptyGraph(t *testing.T) {
	g := NewGraph("empty", "empty")
	g.Build()

	s := NewScheduler()
	ctx := &SchedulerContext{
		Graph:      g,
		RunningIDs: map[string]bool{},
	}

	batch := s.Schedule(ctx, 10)
	if len(batch) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(batch))
	}
}

func TestScheduler_CustomFilter(t *testing.T) {
	g := NewGraph("custom", "custom filter")
	g.AddTask(&Task{ID: "a", Runner: "noop", Labels: map[string]string{"team": "alpha"}})
	g.AddTask(&Task{ID: "b", Runner: "noop", Labels: map[string]string{"team": "beta"}})
	g.Build()

	s := NewScheduler()

	teamFilter := NewFuncRunner("team", nil) // abuse for test
	_ = teamFilter

	// Custom filter: only schedule "alpha" team
	s.AddFilter(&labelFilter{key: "team", value: "alpha"})

	ctx := &SchedulerContext{
		Graph:      g,
		RunningIDs: map[string]bool{},
	}

	batch := s.Schedule(ctx, 10)
	if len(batch) != 1 || batch[0].ID != "a" {
		t.Errorf("expected only 'a' (alpha team), got %v", batch)
	}
}

type labelFilter struct {
	key, value string
}

func (f *labelFilter) Name() string { return "label" }
func (f *labelFilter) Filter(task *Task, _ *SchedulerContext) bool {
	if task.Labels == nil {
		return false
	}
	return task.Labels[f.key] == f.value
}

func TestDependencyFilter(t *testing.T) {
	g := buildDiamondGraph()
	f := &DependencyFilter{}
	ctx := &SchedulerContext{Graph: g}

	// "a" has no deps → pass
	if !f.Filter(g.Tasks["a"], ctx) {
		t.Error("a should pass dependency filter")
	}
	// "b" depends on "a" which is Ready, not Completed → fail
	if f.Filter(g.Tasks["b"], ctx) {
		t.Error("b should fail dependency filter (a not completed)")
	}
}

func TestPriorityQueue(t *testing.T) {
	pq := NewPriorityTaskQueue()
	pq.Push(&Task{ID: "low"}, 10)
	pq.Push(&Task{ID: "high"}, 100)
	pq.Push(&Task{ID: "mid"}, 50)

	if pq.Len() != 3 {
		t.Errorf("expected 3, got %d", pq.Len())
	}

	first := pq.Pop()
	if first.ID != "high" {
		t.Errorf("expected 'high', got %s", first.ID)
	}

	second := pq.Pop()
	if second.ID != "mid" {
		t.Errorf("expected 'mid', got %s", second.ID)
	}

	third := pq.Pop()
	if third.ID != "low" {
		t.Errorf("expected 'low', got %s", third.ID)
	}

	if pq.Pop() != nil {
		t.Error("expected nil from empty queue")
	}
}
