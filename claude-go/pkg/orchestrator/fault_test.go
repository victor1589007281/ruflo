package orchestrator

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

// Fault injection: randomly fail tasks to test resilience.
func TestEngine_FaultInjection_RandomFailures(t *testing.T) {
	var totalCalls int32

	chaosRunner := NewFuncRunner("chaos", func(_ context.Context, task *Task, _ ReadOnlyBlackboard) (any, error) {
		atomic.AddInt32(&totalCalls, 1)
		if rand.Float64() < 0.3 {
			return nil, fmt.Errorf("random failure in %s", task.ID)
		}
		return fmt.Sprintf("ok-%s", task.ID), nil
	})

	g := NewGraph("chaos", "chaos")
	g.AddTask(&Task{ID: "root", Runner: "chaos"})
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("w%d", i)
		g.AddTask(&Task{ID: id, Runner: "chaos"})
		g.AddEdge(Edge{From: "root", To: id, Kind: EdgeDependency})
	}

	cfg := testConfig()
	cfg.RetryPolicy.MaxRetries = 5
	e := NewEngine(cfg)
	e.Runners().Register(chaosRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, _ := e.Run(ctx, g)

	t.Logf("Chaos test: %d calls, %d completed, %d failed, %d cancelled, %d retries",
		atomic.LoadInt32(&totalCalls),
		result.Metrics.CompletedTasks,
		result.Metrics.FailedTasks,
		result.Metrics.CancelledTasks,
		result.Metrics.TotalRetries)

	// With 5 retries, most tasks should eventually succeed
	if result.Metrics.CompletedTasks == 0 {
		t.Error("expected at least some completions")
	}
}

// Fault injection: transient failures followed by success.
func TestEngine_FaultInjection_TransientThenSuccess(t *testing.T) {
	callCounts := make(map[string]*int32)
	for i := 0; i < 5; i++ {
		var n int32
		callCounts[fmt.Sprintf("t%d", i)] = &n
	}

	flakyRunner := NewFuncRunner("flaky", func(_ context.Context, task *Task, _ ReadOnlyBlackboard) (any, error) {
		counter, ok := callCounts[task.ID]
		if !ok {
			return "ok", nil
		}
		n := atomic.AddInt32(counter, 1)
		if n <= 3 {
			return nil, fmt.Errorf("429 rate limit exceeded (attempt %d)", n)
		}
		return fmt.Sprintf("success-%s", task.ID), nil
	})

	g := makeLinearGraph(5)
	for _, t := range g.Tasks {
		t.Runner = "flaky"
	}

	cfg := testConfig()
	e := NewEngine(cfg)
	e.Runners().Register(flakyRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}

	if !result.Success {
		t.Errorf("expected all tasks to eventually succeed, errors: %v", result.Errors)
	}
	if result.Metrics.TotalRetries < 15 {
		t.Errorf("expected at least 15 total retries (3 per task * 5), got %d", result.Metrics.TotalRetries)
	}
	t.Logf("Flaky test: %d retries, %d completed", result.Metrics.TotalRetries, result.Metrics.CompletedTasks)
}

// Fault injection: fatal error stops everything immediately.
func TestEngine_FaultInjection_FatalStopsAll(t *testing.T) {
	fatalRunner := NewFuncRunner("fatal", func(_ context.Context, task *Task, _ ReadOnlyBlackboard) (any, error) {
		if task.ID == "t1" {
			return nil, fmt.Errorf("unauthorized: invalid api key")
		}
		return "ok", nil
	})

	g := makeLinearGraph(5)
	for _, t := range g.Tasks {
		t.Runner = "fatal"
	}

	e := NewEngine(testConfig())
	e.Runners().Register(fatalRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, _ := e.Run(ctx, g)
	if result.Success {
		t.Error("expected failure")
	}
	if result.Metrics.TotalRetries != 0 {
		t.Errorf("fatal errors should have 0 retries, got %d", result.Metrics.TotalRetries)
	}
	if result.Metrics.CancelledTasks < 3 {
		t.Errorf("expected at least 3 cancelled tasks (downstream of t1), got %d", result.Metrics.CancelledTasks)
	}
}

// Stress test: many small independent tasks.
func TestEngine_Stress_ManyIndependent(t *testing.T) {
	n := 200
	g := NewGraph("stress", "stress")
	for i := 0; i < n; i++ {
		g.AddTask(&Task{ID: fmt.Sprintf("t%d", i), Runner: "echo"})
	}

	cfg := testConfig()
	cfg.MaxParallel = 50
	e := NewEngine(cfg)
	e.Runners().Register(echoRunner())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	result, err := e.Run(ctx, g)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected all tasks to succeed, got %d failed", result.Metrics.FailedTasks)
	}
	if result.Metrics.CompletedTasks != n {
		t.Errorf("expected %d completed, got %d", n, result.Metrics.CompletedTasks)
	}
	t.Logf("Stress test: %d tasks completed in %v (%.0f tasks/sec)",
		n, elapsed, float64(n)/elapsed.Seconds())
}

// Stress test: deep chain (serialization pressure).
func TestEngine_Stress_DeepChain(t *testing.T) {
	n := 100
	g := makeLinearGraph(n)

	cfg := testConfig()
	cfg.CheckpointEvery = 10
	e := NewEngine(cfg)
	e.Runners().Register(echoRunner())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	result, err := e.Run(ctx, g)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected success, errors: %v", result.Errors)
	}
	if result.Metrics.CompletedTasks != n {
		t.Errorf("expected %d completed, got %d", n, result.Metrics.CompletedTasks)
	}
	t.Logf("Deep chain: %d tasks completed in %v", n, elapsed)
}

// Stress test: wide fan-out with realistic delays.
func TestEngine_Stress_WideFanout(t *testing.T) {
	width := 30

	delayRunner := NewFuncRunner("delay", func(ctx context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		delay := time.Duration(rand.Intn(20)) * time.Millisecond
		select {
		case <-time.After(delay):
			return "done", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	g := NewGraph("wide", "wide")
	g.AddTask(&Task{ID: "start", Runner: "delay"})
	for i := 0; i < width; i++ {
		id := fmt.Sprintf("w%d", i)
		g.AddTask(&Task{ID: id, Runner: "delay"})
		g.AddEdge(Edge{From: "start", To: id, Kind: EdgeDependency})
	}
	g.AddTask(&Task{ID: "join", Runner: "delay"})
	for i := 0; i < width; i++ {
		g.AddEdge(Edge{From: fmt.Sprintf("w%d", i), To: "join", Kind: EdgeDependency})
	}

	cfg := testConfig()
	cfg.MaxParallel = 15
	e := NewEngine(cfg)
	e.Runners().Register(delayRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	result, err := e.Run(ctx, g)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected success, errors: %v", result.Errors)
	}
	t.Logf("Wide fan-out (%d width): %d tasks in %v", width, result.Metrics.CompletedTasks, elapsed)
}

// Composite runner (adversarial loop simulation).
func TestEngine_CompositeRunner(t *testing.T) {
	var generatorCalls, evaluatorCalls int32

	generator := NewFuncRunner("gen", func(_ context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		n := atomic.AddInt32(&generatorCalls, 1)
		return fmt.Sprintf("code-v%d", n), nil
	})
	evaluator := NewFuncRunner("eval", func(_ context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		n := atomic.AddInt32(&evaluatorCalls, 1)
		return fmt.Sprintf("score-%d", n), nil
	})

	adversarial := NewCompositeRunner("adversarial",
		[]TaskRunner{generator, evaluator},
		&MaxIterTermination{Max: 3},
		5,
	)

	g := NewGraph("adv", "adversarial")
	g.AddTask(&Task{ID: "code", Runner: "adversarial"})

	e := NewEngine(testConfig())
	e.Runners().Register(adversarial)

	ctx := context.Background()
	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("expected success")
	}

	if atomic.LoadInt32(&generatorCalls) != 3 {
		t.Errorf("expected 3 generator calls, got %d", atomic.LoadInt32(&generatorCalls))
	}
	if atomic.LoadInt32(&evaluatorCalls) != 3 {
		t.Errorf("expected 3 evaluator calls, got %d", atomic.LoadInt32(&evaluatorCalls))
	}
}

// Runner registry test.
func TestRunnerRegistry(t *testing.T) {
	r := NewRunnerRegistry()
	r.Register(&NoopRunner{})

	runner, err := r.Get("noop")
	if err != nil {
		t.Fatal(err)
	}
	if runner.Name() != "noop" {
		t.Error("wrong runner name")
	}

	_, err = r.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent runner")
	}

	names := r.List()
	if len(names) != 1 {
		t.Errorf("expected 1 runner, got %d", len(names))
	}

	// Duplicate should panic
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate register")
		}
	}()
	r.Register(&NoopRunner{})
}

// Checkpoint store tests.
func TestCheckpointStore_Memory(t *testing.T) {
	cs := NewMemoryCheckpointStore()

	state := &ExecutionState{
		ExecID:    "exec-1",
		GraphID:   "g1",
		GraphName: "test",
		Tasks: map[string]TaskSnapshot{
			"t1": {ID: "t1", State: TaskCompleted},
		},
	}

	if err := cs.Save("exec-1", state); err != nil {
		t.Fatal(err)
	}

	loaded, err := cs.Load("exec-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GraphID != "g1" {
		t.Error("wrong graph ID")
	}

	ids, _ := cs.List()
	if len(ids) != 1 {
		t.Errorf("expected 1 checkpoint, got %d", len(ids))
	}

	_, err = cs.Load("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent checkpoint")
	}
}

func TestCheckpointStore_File(t *testing.T) {
	dir := t.TempDir()
	cs := NewFileCheckpointStore(dir)

	state := &ExecutionState{
		ExecID:    "exec-1",
		GraphID:   "g1",
		GraphName: "test",
		Tasks: map[string]TaskSnapshot{
			"t1": {ID: "t1", State: TaskCompleted},
			"t2": {ID: "t2", State: TaskFailed, Error: "oops"},
		},
		Metrics: ExecutionMetrics{
			TotalTasks:     2,
			CompletedTasks: 1,
			FailedTasks:    1,
		},
	}

	if err := cs.Save("exec-1", state); err != nil {
		t.Fatal(err)
	}

	loaded, err := cs.Load("exec-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metrics.TotalTasks != 2 {
		t.Error("wrong metrics")
	}
	if loaded.Tasks["t2"].Error != "oops" {
		t.Error("wrong error in snapshot")
	}

	ids, _ := cs.List()
	if len(ids) != 1 {
		t.Errorf("expected 1 checkpoint, got %d", len(ids))
	}
}
