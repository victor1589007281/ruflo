package orchestrator

import (
	"context"
	"sync"
	"testing"
)

type mockObserver struct {
	mu      sync.Mutex
	metrics []string
	events  []ObserverEvent
}

func (o *mockObserver) EmitMetric(name string, value float64, labels map[string]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.metrics = append(o.metrics, name)
}

func (o *mockObserver) EmitEvent(event ObserverEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func TestMetricsHook_CollectsEvents(t *testing.T) {
	obs := &mockObserver{}
	hook := NewMetricsHook(obs)

	g := NewGraph("test", "测试图")
	g.AddTask(&Task{ID: "a", Runner: "noop"})
	g.Build()

	hook.OnGraphStart(g)
	hook.OnTaskStart(g.Tasks["a"])

	events := hook.Events()
	if len(events) < 1 {
		t.Error("应该至少有 1 个事件")
	}
}

func TestConflictDetector_Basic(t *testing.T) {
	d := NewConflictDetector()

	d.DeclareAccess("write1", nil, []string{"file.go"})
	d.DeclareAccess("write2", nil, []string{"file.go"})
	d.DeclareAccess("reader", []string{"file.go"}, nil)
	d.DeclareAccess("other", nil, []string{"other.go"})

	// 写-写冲突
	if d.CanParallel("write1", "write2") {
		t.Error("写-写冲突应不可并行")
	}

	// 读-写冲突
	if d.CanParallel("reader", "write1") {
		t.Error("读-写冲突应不可并行")
	}

	// 无冲突
	if !d.CanParallel("write1", "other") {
		t.Error("无冲突应可并行")
	}
}

func TestConflictFilter_IntegrateWithScheduler(t *testing.T) {
	d := NewConflictDetector()
	d.DeclareAccess("running-1", nil, []string{"shared.go"})
	d.DeclareAccess("candidate", nil, []string{"shared.go"})
	d.DeclareAccess("safe-task", nil, []string{"other.go"})

	f := NewConflictFilter(d)

	ctx := &SchedulerContext{
		RunningIDs: map[string]bool{"running-1": true},
	}

	// candidate 与 running-1 冲突
	candidate := &Task{ID: "candidate"}
	if f.Filter(candidate, ctx) {
		t.Error("冲突任务应被过滤")
	}

	// safe-task 无冲突
	safe := &Task{ID: "safe-task"}
	if !f.Filter(safe, ctx) {
		t.Error("安全任务不应被过滤")
	}
}

func TestDynamicExpander_AddsTasks(t *testing.T) {
	g := NewGraph("dyn", "动态图")
	g.AddTask(&Task{ID: "root", Runner: "noop"})
	g.Build()

	expander := &testExpander{
		newTasks: []*Task{
			{ID: "child1", Runner: "noop"},
			{ID: "child2", Runner: "noop"},
		},
		newEdges: []Edge{
			{From: "root", To: "child1", Kind: EdgeDependency},
			{From: "root", To: "child2", Kind: EdgeDependency},
		},
	}

	hook := NewExpanderHook(expander, g)
	hook.OnTaskComplete(g.Tasks["root"], "done")

	if g.TaskCount() != 3 {
		t.Errorf("动态扩展后应有 3 个任务, 实际 %d", g.TaskCount())
	}
}

type testExpander struct {
	newTasks []*Task
	newEdges []Edge
}

func (e *testExpander) OnTaskComplete(task *Task, output any) ([]*Task, []Edge) {
	return e.newTasks, e.newEdges
}

func TestToolEngine_AsyncRun(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	te.Engine().Runners().Register(NewFuncRunner("fast", func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
		return "done", nil
	}))

	te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{
		"id": "async-test",
	}})
	te.HandleRequest(ctx, ToolRequest{Action: ActionAddTask, Params: map[string]any{
		"graph_id": "async-test", "task_id": "t1", "runner": "fast",
	}})

	resp := te.HandleRequest(ctx, ToolRequest{Action: ActionRunGraph, Params: map[string]any{
		"graph_id": "async-test", "async": true,
	}})
	if !resp.Success {
		t.Fatalf("异步执行启动失败: %s", resp.Error)
	}

	data := resp.Data.(map[string]any)
	if data["status"] != "running" {
		t.Errorf("期望 status=running, 实际 %v", data["status"])
	}
}
