package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockLLM 模拟 LLM 调用, 返回固定响应 (带调用计数)。
type mockLLM struct {
	calls    atomic.Int32
	response func(sys, user string) string
}

func newMockLLM(respFn func(sys, user string) string) *mockLLM {
	return &mockLLM{response: respFn}
}

func (m *mockLLM) SimpleComplete(_ context.Context, sys, user string) (string, error) {
	m.calls.Add(1)
	if m.response != nil {
		return m.response(sys, user), nil
	}
	return "mock response", nil
}

func TestLLMRunner_Basic(t *testing.T) {
	llm := newMockLLM(func(sys, user string) string {
		return fmt.Sprintf("system=%s user=%s", sys, user)
	})
	runner := NewLLMRunner("test-llm", llm)

	task := &Task{
		ID: "t1",
		Config: map[string]any{
			"system_prompt": "你是审查员",
			"user_prompt":   "审查这段代码: {objective}",
			"objective":     "func main() {}",
		},
	}

	bb := NewBlackboard()
	out, err := runner.Execute(context.Background(), task, bb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp := out.(string)
	if !strings.Contains(resp, "system=你是审查员") {
		t.Errorf("system prompt not passed: %s", resp)
	}
	if !strings.Contains(resp, "func main()") {
		t.Errorf("objective not resolved: %s", resp)
	}
}

func TestLLMRunner_TemplateResolution(t *testing.T) {
	llm := newMockLLM(func(_, user string) string { return user })
	runner := NewLLMRunner("test-llm", llm)

	bb := NewBlackboard()
	bb.Write("dep-a/output", "结果A", WriteMeta{Author: "test"})

	task := &Task{
		ID:        "t2",
		DependsOn: []string{"dep-a"},
		Config: map[string]any{
			"system_prompt": "test",
			"user_prompt":   "上游: {prev_result}\n指定: {dep:dep-a}",
			"objective":     "obj",
		},
	}

	out, err := runner.Execute(context.Background(), task, bb)
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(string)
	if !strings.Contains(resp, "结果A") {
		t.Errorf("prev_result not resolved: %s", resp)
	}
}

func TestQualityTermination_PassThreshold(t *testing.T) {
	scorer := &JSONQualityScorer{}
	policy := NewQualityTermination(7.0, 0.5, scorer)

	// 未达标: 不终止
	if policy.ShouldTerminate(1, `{"pass":false,"overall":5.0}`, nil) {
		t.Error("should not terminate on low score")
	}

	// 达标: 终止
	if !policy.ShouldTerminate(2, `{"pass":true,"overall":8.0}`, nil) {
		t.Error("should terminate on passing score")
	}
}

func TestQualityTermination_Convergence(t *testing.T) {
	policy := NewQualityTermination(10.0, 0.3, &JSONQualityScorer{})

	policy.ShouldTerminate(0, `{"pass":false,"overall":6.0}`, nil)
	policy.ShouldTerminate(1, `{"pass":false,"overall":6.1}`, nil)
	result := policy.ShouldTerminate(2, `{"pass":false,"overall":6.15}`, nil)

	if !result {
		t.Error("should terminate on convergence (delta < 0.3 for 3 rounds)")
	}
}

func TestAdversarialRunner_MultiRound(t *testing.T) {
	roundCount := 0
	llm := newMockLLM(func(sys, user string) string {
		roundCount++
		if strings.Contains(sys, "审查") || strings.Contains(user, "review_target") {
			if roundCount <= 4 {
				return `{"pass":false,"overall":5.0,"feedback":"需要改进"}`
			}
			return `{"pass":true,"overall":8.0,"feedback":"通过"}`
		}
		return fmt.Sprintf("generated code round %d", roundCount)
	})

	generator := NewLLMRunner("gen", llm)
	reviewer := NewLLMRunner("rev", llm)
	policy := NewQualityTermination(7.0, 0.5, nil)

	runner := NewAdversarialRunner("adv", generator, []TaskRunner{reviewer}, policy, 5)

	bb := NewBlackboard()
	task := &Task{
		ID: "adv-task",
		Config: map[string]any{
			"system_prompt": "生成代码",
			"user_prompt":   "实现排序",
			"objective":     "quicksort",
		},
	}

	out, err := runner.Execute(context.Background(), task, bb)
	if err != nil {
		t.Fatalf("adversarial error: %v", err)
	}
	if out == nil {
		t.Fatal("output should not be nil")
	}

	// 验证多轮对抗确实发生了
	if roundCount < 2 {
		t.Errorf("expected multiple rounds, got %d", roundCount)
	}
}

func TestRunnerPool_ConcurrencyLimit(t *testing.T) {
	pool := NewRunnerPool()
	pool.SetLimit("limited", 2)

	ctx := context.Background()
	if err := pool.Acquire(ctx, "limited"); err != nil {
		t.Fatal(err)
	}
	if err := pool.Acquire(ctx, "limited"); err != nil {
		t.Fatal(err)
	}

	// 第 3 个 acquire 应该阻塞
	ctx2, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	err := pool.Acquire(ctx2, "limited")
	if err == nil {
		t.Error("should have blocked (limit=2)")
		pool.Release("limited")
	}

	// 释放一个后可以获取
	pool.Release("limited")
	if err := pool.Acquire(ctx, "limited"); err != nil {
		t.Fatalf("should acquire after release: %v", err)
	}

	pool.Release("limited")
	pool.Release("limited")
}

func TestRunnerPool_Unlimited(t *testing.T) {
	pool := NewRunnerPool()
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		if err := pool.Acquire(ctx, "unlimited"); err != nil {
			t.Fatal(err)
		}
	}

	if pool.ActiveCount("unlimited") != 100 {
		t.Errorf("expected 100 active, got %d", pool.ActiveCount("unlimited"))
	}

	for i := 0; i < 100; i++ {
		pool.Release("unlimited")
	}
}

func TestPooledRunner_Integration(t *testing.T) {
	pool := NewRunnerPool()
	pool.SetLimit("inner", 1)

	callCount := 0
	inner := &funcRunner{
		name: "inner",
		fn: func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
			callCount++
			return callCount, nil
		},
	}

	pooled := NewPooledRunner(inner, pool)
	bb := NewBlackboard()
	task := &Task{ID: "test"}

	out, err := pooled.Execute(context.Background(), task, bb)
	if err != nil {
		t.Fatal(err)
	}
	if out.(int) != 1 {
		t.Errorf("expected 1, got %v", out)
	}
}

func TestLLMExpander_NoExpand(t *testing.T) {
	llm := newMockLLM(nil)
	expander := NewLLMExpander(llm, "llm-stage", 5)

	task := &Task{ID: "t1", Config: map[string]any{}}
	tasks, edges := expander.OnTaskComplete(task, "output")

	if len(tasks) != 0 || len(edges) != 0 {
		t.Error("should not expand when expandable=false")
	}
	if llm.calls.Load() != 0 {
		t.Error("LLM should not be called when expandable=false")
	}
}

func TestLLMExpander_WithExpand(t *testing.T) {
	llm := newMockLLM(func(_, _ string) string {
		return `{"tasks":[{"id":"sub-1","name":"子任务1","prompt":"做事","priority":5}]}`
	})
	expander := NewLLMExpander(llm, "llm-stage", 5)

	task := &Task{
		ID:   "parent",
		Name: "父任务",
		Config: map[string]any{
			"expandable": true,
		},
	}
	tasks, edges := expander.OnTaskComplete(task, "需要拆分的输出")

	if len(tasks) != 1 {
		t.Fatalf("expected 1 sub-task, got %d", len(tasks))
	}
	if tasks[0].ID != "parent/sub-1" {
		t.Errorf("unexpected task ID: %s", tasks[0].ID)
	}
	if len(edges) != 1 || edges[0].From != "parent" || edges[0].To != "parent/sub-1" {
		t.Errorf("unexpected edge: %+v", edges)
	}
}

// funcRunner 用于测试的简单 Runner。
type funcRunner struct {
	name string
	fn   func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error)
}

func (r *funcRunner) Name() string { return r.name }
func (r *funcRunner) Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
	return r.fn(ctx, task, bb)
}

// ---- 集成测试: LLMRunner + Engine ----

func TestEngine_WithLLMRunner(t *testing.T) {
	cfg := testConfig()
	cfg.MaxParallel = 4
	eng := NewEngine(cfg)

	llm := newMockLLM(func(sys, user string) string {
		return "done: " + sys[:20]
	})
	eng.Runners().Register(NewLLMRunner("llm", llm))

	g := NewGraph("test-graph", "LLM Integration")
	_ = g.AddTask(&Task{
		ID: "analyze", Name: "代码分析", Runner: "llm",
		Config: map[string]any{
			"system_prompt": "你是代码分析师。分析以下代码的结构和质量。",
			"user_prompt":   "分析: {objective}",
			"objective":     "func main() { fmt.Println(\"hello\") }",
		},
	})
	_ = g.AddTask(&Task{
		ID: "review", Name: "审查", Runner: "llm",
		DependsOn: []string{"analyze"},
		Config: map[string]any{
			"system_prompt": "你是代码审查员。基于分析结果进行审查。",
			"user_prompt":   "基于分析: {prev_result}\n给出审查意见",
			"objective":     "review",
		},
	})
	_ = g.AddEdge(Edge{From: "analyze", To: "review"})

	result, err := eng.Run(context.Background(), g)
	if err != nil {
		t.Fatalf("engine run failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, errors: %v", result.Errors)
	}
	if llm.calls.Load() != 2 {
		t.Errorf("expected 2 LLM calls, got %d", llm.calls.Load())
	}
}

func TestEngine_WithAdversarialRunner(t *testing.T) {
	cfg := testConfig()
	eng := NewEngine(cfg)

	round := 0
	llm := newMockLLM(func(sys, user string) string {
		round++
		if round <= 2 {
			return `{"pass":false,"overall":5.0,"feedback":"需改进"}`
		}
		return `{"pass":true,"overall":8.5,"feedback":"通过"}`
	})

	gen := NewLLMRunner("gen", llm)
	rev := NewLLMRunner("rev", llm)
	policy := NewQualityTermination(7.0, 0.5, nil)
	adv := NewAdversarialRunner("adversarial", gen, []TaskRunner{rev}, policy, 5)

	eng.Runners().Register(adv)

	g := NewGraph("adv-test", "Adversarial Test")
	_ = g.AddTask(&Task{
		ID: "adv-task", Name: "对抗任务", Runner: "adversarial",
		Config: map[string]any{
			"system_prompt": "生成代码",
			"user_prompt":   "写排序",
			"objective":     "quicksort",
		},
	})

	result, err := eng.Run(context.Background(), g)
	if err != nil {
		t.Fatalf("engine run failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, errors: %v", result.Errors)
	}
	if round < 3 {
		t.Errorf("expected at least 3 rounds, got %d", round)
	}
}
