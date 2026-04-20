package orchestrator

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func makeLinearGraph(n int) *Graph {
	g := NewGraph("linear", "linear")
	for i := 0; i < n; i++ {
		g.AddTask(&Task{
			ID:     fmt.Sprintf("t%d", i),
			Name:   fmt.Sprintf("Task %d", i),
			Runner: "echo",
		})
	}
	for i := 0; i < n-1; i++ {
		g.AddEdge(Edge{
			From: fmt.Sprintf("t%d", i),
			To:   fmt.Sprintf("t%d", i+1),
			Kind: EdgeDependency,
		})
	}
	return g
}

func makeDiamondGraph() *Graph {
	g := NewGraph("diamond", "diamond")
	g.AddTask(&Task{ID: "root", Runner: "echo"})
	g.AddTask(&Task{ID: "left", Runner: "echo"})
	g.AddTask(&Task{ID: "right", Runner: "echo"})
	g.AddTask(&Task{ID: "join", Runner: "echo"})

	g.AddEdge(Edge{From: "root", To: "left", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "root", To: "right", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "left", To: "join", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "right", To: "join", Kind: EdgeDependency})
	return g
}

func echoRunner() *FuncRunner {
	return NewFuncRunner("echo", func(_ context.Context, t *Task, _ ReadOnlyBlackboard) (any, error) {
		return fmt.Sprintf("output-%s", t.ID), nil
	})
}

func testConfig() EngineConfig {
	cfg := DefaultEngineConfig()
	cfg.RPM = 600000
	cfg.RPMBurst = 10000
	cfg.QueueDepth = 1000
	cfg.RetryPolicy.BaseDelay = 1 * time.Millisecond
	cfg.RetryPolicy.TransientBase = 1 * time.Millisecond
	cfg.RetryPolicy.MaxDelay = 10 * time.Millisecond
	return cfg
}

func TestEngine_LinearDAG(t *testing.T) {
	e := NewEngine(testConfig())
	e.Runners().Register(echoRunner())

	g := makeLinearGraph(5)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected success, got errors: %v", result.Errors)
	}
	if result.Metrics.CompletedTasks != 5 {
		t.Errorf("expected 5 completed, got %d", result.Metrics.CompletedTasks)
	}
	if result.Metrics.FailedTasks != 0 {
		t.Errorf("expected 0 failed, got %d", result.Metrics.FailedTasks)
	}
}

func TestEngine_DiamondDAG(t *testing.T) {
	e := NewEngine(testConfig())
	e.Runners().Register(echoRunner())

	g := makeDiamondGraph()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if result.Metrics.CompletedTasks != 4 {
		t.Errorf("expected 4 completed, got %d", result.Metrics.CompletedTasks)
	}
}

func TestEngine_ParallelFanout(t *testing.T) {
	g := NewGraph("fanout", "fanout")
	g.AddTask(&Task{ID: "start", Runner: "echo"})
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("worker-%d", i)
		g.AddTask(&Task{ID: id, Runner: "echo"})
		g.AddEdge(Edge{From: "start", To: id, Kind: EdgeDependency})
	}
	g.AddTask(&Task{ID: "join", Runner: "echo"})
	for i := 0; i < 10; i++ {
		g.AddEdge(Edge{From: fmt.Sprintf("worker-%d", i), To: "join", Kind: EdgeDependency})
	}

	e := NewEngine(testConfig())
	e.Runners().Register(echoRunner())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if result.Metrics.CompletedTasks != 12 {
		t.Errorf("expected 12 completed, got %d", result.Metrics.CompletedTasks)
	}
}

func TestEngine_SingleTask(t *testing.T) {
	g := NewGraph("single", "single")
	g.AddTask(&Task{ID: "only", Runner: "echo"})

	e := NewEngine(testConfig())
	e.Runners().Register(echoRunner())

	ctx := context.Background()
	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Metrics.CompletedTasks != 1 {
		t.Error("single task should succeed")
	}
}

func TestEngine_ContextCancellation(t *testing.T) {
	slowRunner := NewFuncRunner("slow", func(ctx context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Minute):
			return "done", nil
		}
	})

	g := NewGraph("cancel", "cancel")
	g.AddTask(&Task{ID: "slow", Runner: "slow"})

	e := NewEngine(testConfig())
	e.Runners().Register(slowRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := e.Run(ctx, g)
	if err == nil {
		t.Error("expected context error")
	}
}

func TestEngine_TaskTimeout(t *testing.T) {
	slowRunner := NewFuncRunner("slow", func(ctx context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Minute):
			return "done", nil
		}
	})

	g := NewGraph("timeout", "timeout")
	g.AddTask(&Task{ID: "slow", Runner: "slow", Timeout: 100 * time.Millisecond})

	cfg := testConfig()
	cfg.RetryPolicy.MaxRetries = 0
	cfg.RetryPolicy.MaxTransient = 0
	e := NewEngine(cfg)
	e.Runners().Register(slowRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, _ := e.Run(ctx, g)
	if result.Success {
		t.Error("expected failure due to timeout")
	}
}

func TestEngine_PermanentFailure_Cascade(t *testing.T) {
	failRunner := NewFuncRunner("fail", func(_ context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		return nil, fmt.Errorf("permanent: code quality issue")
	})

	g := NewGraph("cascade", "cascade")
	g.AddTask(&Task{ID: "fail", Runner: "fail"})
	g.AddTask(&Task{ID: "child1", Runner: "echo"})
	g.AddTask(&Task{ID: "child2", Runner: "echo"})
	g.AddTask(&Task{ID: "grandchild", Runner: "echo"})

	g.AddEdge(Edge{From: "fail", To: "child1", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "fail", To: "child2", Kind: EdgeDependency})
	g.AddEdge(Edge{From: "child1", To: "grandchild", Kind: EdgeDependency})

	cfg := testConfig()
	cfg.RetryPolicy.MaxRetries = 0
	e := NewEngine(cfg)
	e.Runners().Register(echoRunner())
	e.Runners().Register(failRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, _ := e.Run(ctx, g)
	if result.Success {
		t.Error("expected failure")
	}

	// "fail" → Failed, "child1","child2","grandchild" → Cancelled
	if result.Metrics.FailedTasks != 1 {
		t.Errorf("expected 1 failed, got %d", result.Metrics.FailedTasks)
	}
	if result.Metrics.CancelledTasks != 3 {
		t.Errorf("expected 3 cancelled, got %d", result.Metrics.CancelledTasks)
	}
}

func TestEngine_TransientFailure_Retry(t *testing.T) {
	var calls int32

	transientRunner := NewFuncRunner("transient", func(_ context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			return nil, fmt.Errorf("429 rate limit exceeded")
		}
		return "success after retries", nil
	})

	g := NewGraph("retry", "retry")
	g.AddTask(&Task{ID: "flaky", Runner: "transient"})

	cfg := testConfig()
	e := NewEngine(cfg)
	e.Runners().Register(transientRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected success after retries, errors: %v", result.Errors)
	}
	if result.Metrics.TotalRetries < 2 {
		t.Errorf("expected at least 2 retries, got %d", result.Metrics.TotalRetries)
	}
}

func TestEngine_FatalError_NoRetry(t *testing.T) {
	fatalRunner := NewFuncRunner("fatal", func(_ context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
		return nil, fmt.Errorf("invalid api key")
	})

	g := NewGraph("fatal", "fatal")
	g.AddTask(&Task{ID: "doomed", Runner: "fatal"})

	e := NewEngine(testConfig())
	e.Runners().Register(fatalRunner)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, _ := e.Run(ctx, g)
	if result.Success {
		t.Error("expected failure")
	}
	if result.Metrics.TotalRetries != 0 {
		t.Errorf("fatal errors should not retry, got %d retries", result.Metrics.TotalRetries)
	}
}

func TestEngine_Checkpoint(t *testing.T) {
	cs := NewMemoryCheckpointStore()

	g := makeLinearGraph(3)
	cfg := testConfig()
	cfg.CheckpointEvery = 1
	e := NewEngine(cfg)
	e.Runners().Register(echoRunner())
	e.SetCheckpointStore(cs)

	ctx := context.Background()
	result, err := e.Run(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("expected success")
	}

	ids, err := cs.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Error("expected at least one checkpoint")
	}

	state, err := cs.Load(result.ExecID)
	if err != nil {
		t.Fatal(err)
	}
	if state.GraphID != "linear" {
		t.Errorf("expected graphID linear, got %s", state.GraphID)
	}
}

func TestEngine_BlackboardOutput(t *testing.T) {
	e := NewEngine(testConfig())
	e.Runners().Register(echoRunner())

	g := NewGraph("bb", "bb")
	g.AddTask(&Task{ID: "t1", Runner: "echo"})

	ctx := context.Background()
	result, _ := e.Run(ctx, g)
	if !result.Success {
		t.Fatal("expected success")
	}

	val, _, err := e.BB().Read("t1/output")
	if err != nil {
		t.Fatal(err)
	}
	if val != "output-t1" {
		t.Errorf("expected output-t1 in blackboard, got %v", val)
	}
}

func TestEngine_LifecycleHooks(t *testing.T) {
	var started, completed, graphStarted, graphCompleted int32

	hook := &testHook{
		onGraphStart:    func(_ *Graph) { atomic.AddInt32(&graphStarted, 1) },
		onTaskStart:     func(_ *Task) { atomic.AddInt32(&started, 1) },
		onTaskComplete:  func(_ *Task, _ any) { atomic.AddInt32(&completed, 1) },
		onGraphComplete: func(_ *Graph, _ ExecutionMetrics) { atomic.AddInt32(&graphCompleted, 1) },
	}

	g := makeLinearGraph(3)
	e := NewEngine(testConfig())
	e.Runners().Register(echoRunner())
	e.SetHook(hook)

	ctx := context.Background()
	e.Run(ctx, g)

	if atomic.LoadInt32(&graphStarted) != 1 {
		t.Error("OnGraphStart should be called once")
	}
	if atomic.LoadInt32(&started) < 3 {
		t.Errorf("OnTaskStart should be called 3+ times, got %d", atomic.LoadInt32(&started))
	}
	if atomic.LoadInt32(&completed) != 3 {
		t.Errorf("OnTaskComplete should be called 3 times, got %d", atomic.LoadInt32(&completed))
	}
	if atomic.LoadInt32(&graphCompleted) != 1 {
		t.Error("OnGraphComplete should be called once")
	}
}

type testHook struct {
	NoopHook
	onGraphStart    func(*Graph)
	onTaskStart     func(*Task)
	onTaskComplete  func(*Task, any)
	onGraphComplete func(*Graph, ExecutionMetrics)
}

func (h *testHook) OnGraphStart(g *Graph) {
	if h.onGraphStart != nil {
		h.onGraphStart(g)
	}
}
func (h *testHook) OnTaskStart(t *Task) {
	if h.onTaskStart != nil {
		h.onTaskStart(t)
	}
}
func (h *testHook) OnTaskComplete(t *Task, out any) {
	if h.onTaskComplete != nil {
		h.onTaskComplete(t, out)
	}
}
func (h *testHook) OnGraphComplete(g *Graph, m ExecutionMetrics) {
	if h.onGraphComplete != nil {
		h.onGraphComplete(g, m)
	}
}

func TestEngine_MissingRunner(t *testing.T) {
	g := NewGraph("miss", "missing runner")
	g.AddTask(&Task{ID: "bad", Runner: "nonexistent"})

	cfg := testConfig()
	cfg.RetryPolicy.MaxRetries = 0
	e := NewEngine(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, _ := e.Run(ctx, g)
	if result.Success {
		t.Error("expected failure for missing runner")
	}
}

func TestEngine_LargeDAG(t *testing.T) {
	n := 50
	g := NewGraph("large", "large")
	g.AddTask(&Task{ID: "root", Runner: "echo"})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("w%d", i)
		g.AddTask(&Task{ID: id, Runner: "echo"})
		g.AddEdge(Edge{From: "root", To: id, Kind: EdgeDependency})
	}
	g.AddTask(&Task{ID: "join", Runner: "echo"})
	for i := 0; i < n; i++ {
		g.AddEdge(Edge{From: fmt.Sprintf("w%d", i), To: "join", Kind: EdgeDependency})
	}

	cfg := testConfig()
	cfg.MaxParallel = 20
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
	if result.Metrics.CompletedTasks != n+2 {
		t.Errorf("expected %d completed, got %d", n+2, result.Metrics.CompletedTasks)
	}
	t.Logf("Large DAG (%d tasks) completed in %v", n+2, elapsed)
}
