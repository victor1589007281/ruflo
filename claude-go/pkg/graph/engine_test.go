package graph

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 可编程 stub runner: 记录调用序列+次数+输入, 按节点 ID 返回预设结果。
// ---------------------------------------------------------------------------

type stubRunner struct {
	mu     sync.Mutex
	calls  []string               // 节点调用序列
	count  map[string]int         // 每节点调用次数
	inputs map[string][]NodeInput // 每节点历次输入
	fn     map[string]func(call int, in NodeInput) NodeResult
}

func newStub() *stubRunner {
	return &stubRunner{
		count:  map[string]int{},
		inputs: map[string][]NodeInput{},
		fn:     map[string]func(int, NodeInput) NodeResult{},
	}
}

func (s *stubRunner) RunNode(_ context.Context, node NodeSpec, in NodeInput) NodeResult {
	s.mu.Lock()
	call := s.count[node.ID]
	s.count[node.ID]++
	s.calls = append(s.calls, node.ID)
	s.inputs[node.ID] = append(s.inputs[node.ID], in)
	f := s.fn[node.ID]
	s.mu.Unlock()
	if f == nil {
		// 默认: 成功并产出 "<id>-out"
		return NodeResult{Status: NodeStatusCompleted, Output: node.ID + "-out"}
	}
	return f(call, in)
}

func (s *stubRunner) callCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count[id]
}

func (s *stubRunner) input(id string, i int) NodeInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputs[id][i]
}

// —— 测试辅助 ——

func fastEngine(r NodeRunner, j Journal) *Engine {
	return &Engine{
		Runner:  r,
		Journal: j,
		// 测试不真睡: 重试退避直接放行 (ctx 已取消时仍返回 false)。
		sleepFn: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
	}
}

func countEvents(t *testing.T, j Journal, typ string) int {
	t.Helper()
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func findSkipReason(t *testing.T, j Journal, nodeID string) (string, bool) {
	t.Helper()
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, ev := range evs {
		if ev.Type == EvNodeSkipped && ev.NodeID == nodeID {
			r, _ := ev.Data["reason"].(string)
			return r, true
		}
	}
	return "", false
}

func linearSpec(ids ...string) GraphSpec {
	g := GraphSpec{Name: "linear"}
	for i, id := range ids {
		g.Nodes = append(g.Nodes, vNode(id))
		if i > 0 {
			g.Edges = append(g.Edges, EdgeSpec{From: ids[i-1], To: id})
		}
	}
	return g
}

// ---------------------------------------------------------------------------
// 1. 线性 pipeline: 顺序执行 + PrevOutputs 传递
// ---------------------------------------------------------------------------

func TestLinearPipeline(t *testing.T) {
	st := newStub()
	e := fastEngine(st, NewMemoryJournal())
	res, err := e.Run(context.Background(), linearSpec("a", "b", "c"),
		RunOpts{Objective: "测试目标"})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, 期望 completed", res.Status)
	}
	st.mu.Lock()
	calls := append([]string(nil), st.calls...)
	st.mu.Unlock()
	if len(calls) != 3 || calls[0] != "a" || calls[1] != "b" || calls[2] != "c" {
		t.Fatalf("调用序列 = %v, 期望 [a b c]", calls)
	}
	if in := st.input("a", 0); len(in.PrevOutputs) != 0 || in.Objective != "测试目标" {
		t.Fatalf("a 输入异常: %+v", in)
	}
	if in := st.input("b", 0); in.PrevOutputs["a"] != "a-out" || len(in.PrevOutputs) != 1 {
		t.Fatalf("b 的 PrevOutputs = %v, 期望 {a: a-out}", in.PrevOutputs)
	}
	if in := st.input("c", 0); in.PrevOutputs["b"] != "b-out" || len(in.PrevOutputs) != 1 {
		t.Fatalf("c 的 PrevOutputs = %v, 期望 {b: b-out}", in.PrevOutputs)
	}
	if len(res.Order) != 3 || res.Order[2] != "c" {
		t.Fatalf("Order = %v, 期望以 c 收尾的 3 节点序", res.Order)
	}
}

// ---------------------------------------------------------------------------
// 2. 菱形 a→(b,c)→d: b、c 真并发, d 等待两者
// ---------------------------------------------------------------------------

func TestDiamondParallel(t *testing.T) {
	g := GraphSpec{
		Name:  "diamond",
		Nodes: []NodeSpec{vNode("a"), vNode("b"), vNode("c"), vNode("d")},
		Edges: []EdgeSpec{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: "d"}, {From: "c", To: "d"},
		},
	}
	st := newStub()
	// b/c 互相等待对方进场后才返回: 若引擎串行执行, 3s 超时报失败。
	var mu sync.Mutex
	entered := 0
	both := make(chan struct{})
	rendezvous := func(id string) NodeResult {
		mu.Lock()
		entered++
		if entered == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
			return NodeResult{Status: NodeStatusCompleted, Output: id + "-out"}
		case <-time.After(3 * time.Second):
			return NodeResult{Status: NodeStatusFailed, Err: "b/c 未并发执行"}
		}
	}
	st.fn["b"] = func(int, NodeInput) NodeResult { return rendezvous("b") }
	st.fn["c"] = func(int, NodeInput) NodeResult { return rendezvous("c") }

	e := fastEngine(st, NewMemoryJournal())
	res, err := e.Run(context.Background(), g, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q (b/c 可能未并发), Nodes=%+v", res.Status, res.Nodes)
	}
	in := st.input("d", 0)
	if in.PrevOutputs["b"] != "b-out" || in.PrevOutputs["c"] != "c-out" || len(in.PrevOutputs) != 2 {
		t.Fatalf("d 的 PrevOutputs = %v, 期望 b、c 双产出", in.PrevOutputs)
	}
	// d 必须在 b、c 都终态之后才执行。
	st.mu.Lock()
	idx := map[string]int{}
	for i, id := range st.calls {
		idx[id] = i
	}
	st.mu.Unlock()
	if idx["d"] < idx["b"] || idx["d"] < idx["c"] {
		t.Fatalf("d 未等待 b/c: 调用序 %v", st.calls)
	}
}

// ---------------------------------------------------------------------------
// 3. 条件路由: score=80 走通过分支, 落选分支 skipped 且级联
// ---------------------------------------------------------------------------

func TestConditionalRouting(t *testing.T) {
	g := GraphSpec{
		Name: "route",
		Nodes: []NodeSpec{
			{ID: "g", Kind: NodeKindGate, Agent: AgentSpec{Role: "gate"}},
			vNode("pass"), vNode("redo"), vNode("after"),
		},
		Edges: []EdgeSpec{
			{From: "g", To: "pass", Condition: "score >= 75"},
			{From: "g", To: "redo", Condition: "score < 75"},
			{From: "redo", To: "after"},
		},
	}
	st := newStub()
	st.fn["g"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "评分", Score: 80}
	}
	j := NewMemoryJournal()
	e := fastEngine(st, j)
	res, err := e.Run(context.Background(), g, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusPartial {
		t.Fatalf("Status = %q, 期望 partial (有 skipped)", res.Status)
	}
	if res.Nodes["pass"].Status != NodeStatusCompleted {
		t.Fatalf("pass = %+v, 期望 completed", res.Nodes["pass"])
	}
	if res.Nodes["redo"].Status != NodeStatusSkipped {
		t.Fatalf("redo = %+v, 期望 skipped", res.Nodes["redo"])
	}
	if res.Nodes["after"].Status != NodeStatusSkipped {
		t.Fatalf("after = %+v, 期望级联 skipped", res.Nodes["after"])
	}
	if st.callCount("redo") != 0 || st.callCount("after") != 0 {
		t.Fatalf("skipped 节点被执行: redo=%d after=%d", st.callCount("redo"), st.callCount("after"))
	}
	for _, id := range []string{"redo", "after"} {
		if _, ok := findSkipReason(t, j, id); !ok {
			t.Fatalf("journal 缺少 %s 的 node.skipped 事件", id)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. OR-join: 重做边+通过边汇入同一节点, 仅一条满足也执行
// ---------------------------------------------------------------------------

func TestORJoin(t *testing.T) {
	g := GraphSpec{
		Name: "orjoin",
		Nodes: []NodeSpec{
			{ID: "g", Kind: NodeKindGate, Agent: AgentSpec{Role: "gate"}},
			vNode("redo"), vNode("done"),
		},
		Edges: []EdgeSpec{
			{From: "g", To: "redo", Condition: "score < 75"},
			{From: "g", To: "done", Condition: "score >= 75"},
			{From: "redo", To: "done"}, // 重做路径也汇入 done
		},
	}
	st := newStub()
	st.fn["g"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "g-out", Score: 80}
	}
	e := fastEngine(st, NewMemoryJournal())
	res, err := e.Run(context.Background(), g, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	// redo skipped, 但 done 经 g→done 边满足 → OR-join 仍执行。
	if res.Nodes["redo"].Status != NodeStatusSkipped {
		t.Fatalf("redo = %+v, 期望 skipped", res.Nodes["redo"])
	}
	if res.Nodes["done"].Status != NodeStatusCompleted {
		t.Fatalf("done = %+v, 期望 OR-join 下执行", res.Nodes["done"])
	}
	in := st.input("done", 0)
	if in.PrevOutputs["g"] != "g-out" || len(in.PrevOutputs) != 1 {
		t.Fatalf("done 的 PrevOutputs = %v, 期望仅含 completed 前驱 g", in.PrevOutputs)
	}
}

// ---------------------------------------------------------------------------
// 5. loop: Until 命中停 / MaxIterations 耗尽停 / Feedback 替换
// ---------------------------------------------------------------------------

func TestLoopUntil(t *testing.T) {
	g := GraphSpec{Name: "loop", Nodes: []NodeSpec{{
		ID: "l", Kind: NodeKindGate, Agent: AgentSpec{Role: "gate"},
		Loop: &LoopPolicy{MaxIterations: 5, Until: "score >= 75", Feedback: "改进: {prev_output}"},
	}}}
	st := newStub()
	scores := []float64{60, 70, 80}
	st.fn["l"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: []string{"o0", "o1", "o2"}[call], Score: scores[call]}
	}
	j := NewMemoryJournal()
	e := fastEngine(st, j)
	res, err := e.Run(context.Background(), g, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("l") != 3 {
		t.Fatalf("调用次数 = %d, 期望 3 (第三轮 80 触发 Until)", st.callCount("l"))
	}
	if res.Nodes["l"].Score != 80 || res.Nodes["l"].Output != "o2" {
		t.Fatalf("最终结果 = %+v, 期望最后一轮", res.Nodes["l"])
	}
	// Feedback: {prev_output} 被替换为上一轮 Output; Iteration 递增。
	if in := st.input("l", 1); in.Feedback != "改进: o0" || in.Iteration != 1 {
		t.Fatalf("第 2 轮输入 = %+v, 期望 Feedback=改进: o0 / Iteration=1", in)
	}
	if in := st.input("l", 2); in.Feedback != "改进: o1" || in.Iteration != 2 {
		t.Fatalf("第 3 轮输入 = %+v, 期望 Feedback=改进: o1 / Iteration=2", in)
	}
	if n := countEvents(t, j, EvLoopIteration); n != 2 {
		t.Fatalf("loop.iteration 事件数 = %d, 期望 2", n)
	}
}

func TestLoopMaxIterationsExhausted(t *testing.T) {
	g := GraphSpec{Name: "loop2", Nodes: []NodeSpec{{
		ID: "l", Kind: NodeKindGate, Agent: AgentSpec{Role: "gate"},
		Loop: &LoopPolicy{MaxIterations: 2, Until: "score >= 75"},
	}}}
	st := newStub()
	st.fn["l"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Score: 10} // 永不达标
	}
	e := fastEngine(st, NewMemoryJournal())
	if _, err := e.Run(context.Background(), g, RunOpts{}); err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("l") != 2 {
		t.Fatalf("调用次数 = %d, 期望 MaxIterations=2 耗尽即停", st.callCount("l"))
	}
}

// ---------------------------------------------------------------------------
// 6. retry: 前 2 次 failed 第 3 次 completed, journal 有 node.retried×2
// ---------------------------------------------------------------------------

func TestRetry(t *testing.T) {
	g := GraphSpec{Name: "retry", Nodes: []NodeSpec{{
		ID: "r", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"},
		Retry: &RetryPolicy{MaxRetries: 3, BackoffSec: 1},
	}}}
	st := newStub()
	st.fn["r"] = func(call int, _ NodeInput) NodeResult {
		if call < 2 {
			return NodeResult{Status: NodeStatusFailed, Err: "瞬态失败"}
		}
		return NodeResult{Status: NodeStatusCompleted, Output: "终成"}
	}
	j := NewMemoryJournal()
	e := fastEngine(st, j)
	res, err := e.Run(context.Background(), g, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted || res.Nodes["r"].Output != "终成" {
		t.Fatalf("结果 = %+v, 期望重试后 completed", res.Nodes["r"])
	}
	if st.callCount("r") != 3 {
		t.Fatalf("调用次数 = %d, 期望 3", st.callCount("r"))
	}
	if n := countEvents(t, j, EvNodeRetried); n != 2 {
		t.Fatalf("node.retried 事件数 = %d, 期望 2", n)
	}
}

// ---------------------------------------------------------------------------
// 7. journal replay resume: a、b 走缓存不重跑, 只重跑失败的 c
// ---------------------------------------------------------------------------

func TestResumeFromJournal(t *testing.T) {
	dir := t.TempDir()
	spec := linearSpec("a", "b", "c")

	// 第一次: c 恒失败 → partial。
	j1, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	st1 := newStub()
	st1.fn["c"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusFailed, Err: "第一趟失败"}
	}
	res1, err := fastEngine(st1, j1).Run(context.Background(), spec, RunOpts{RunID: "run-fix"})
	if err != nil {
		t.Fatalf("首跑出错: %v", err)
	}
	if res1.Status != RunStatusPartial {
		t.Fatalf("首跑 Status = %q, 期望 partial", res1.Status)
	}
	j1.Close()

	// 第二次: 同一 journal 目录, Resume=true, c 恢复正常。
	j2, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("重开 journal: %v", err)
	}
	defer j2.Close()
	st2 := newStub()
	res2, err := fastEngine(st2, j2).Run(context.Background(), spec,
		RunOpts{RunID: "run-fix", Resume: true})
	if err != nil {
		t.Fatalf("resume 跑出错: %v", err)
	}
	if res2.Status != RunStatusCompleted {
		t.Fatalf("resume Status = %q, 期望 completed", res2.Status)
	}
	if st2.callCount("a") != 0 || st2.callCount("b") != 0 {
		t.Fatalf("缓存节点被重跑: a=%d b=%d, 期望 0/0", st2.callCount("a"), st2.callCount("b"))
	}
	if st2.callCount("c") != 1 {
		t.Fatalf("c 调用次数 = %d, 期望 1", st2.callCount("c"))
	}
	// 缓存产出经 journal 恢复并喂给下游。
	if in := st2.input("c", 0); in.PrevOutputs["b"] != "b-out" {
		t.Fatalf("resume 后 c 的 PrevOutputs = %v, 期望缓存的 {b: b-out}", in.PrevOutputs)
	}
	if res2.Nodes["a"].Output != "a-out" {
		t.Fatalf("缓存节点 a 结果 = %+v, 期望 journal 恢复的产出", res2.Nodes["a"])
	}
}

// ---------------------------------------------------------------------------
// 11. hook deny → 节点 skipped (级联下游)
// ---------------------------------------------------------------------------

type denyBus struct{ deny map[string]string }

func (b denyBus) Emit(_ context.Context, ev HookEvent) HookDecision {
	if ev.Scope == ScopeNode && ev.Phase == "pre" {
		if reason, ok := b.deny[ev.NodeID]; ok {
			return HookDecision{Action: HookDeny, Reason: reason}
		}
	}
	return HookDecision{}
}

func TestHookDeny(t *testing.T) {
	st := newStub()
	j := NewMemoryJournal()
	e := fastEngine(st, j)
	e.Hooks = denyBus{deny: map[string]string{"b": "策略拒绝: 演练"}}
	res, err := e.Run(context.Background(), linearSpec("a", "b", "c"), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusPartial {
		t.Fatalf("Status = %q, 期望 partial", res.Status)
	}
	if res.Nodes["b"].Status != NodeStatusSkipped {
		t.Fatalf("b = %+v, 期望 hook deny → skipped", res.Nodes["b"])
	}
	if res.Nodes["c"].Status != NodeStatusSkipped {
		t.Fatalf("c = %+v, 期望级联 skipped", res.Nodes["c"])
	}
	if st.callCount("b") != 0 {
		t.Fatalf("被 deny 的 b 仍被执行 %d 次", st.callCount("b"))
	}
	reason, ok := findSkipReason(t, j, "b")
	if !ok || reason != "策略拒绝: 演练" {
		t.Fatalf("journal node.skipped reason = %q (found=%v), 期望 hook 原因入 Data", reason, ok)
	}
}

// ---------------------------------------------------------------------------
// 语义8: ctx 取消 → 未跑节点不调度, Status=failed, error=ctx.Err()
// ---------------------------------------------------------------------------

func TestContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := newStub()
	st.fn["a"] = func(int, NodeInput) NodeResult {
		cancel() // a 执行中触发取消
		return NodeResult{Status: NodeStatusCompleted, Output: "a-out"}
	}
	e := fastEngine(st, NewMemoryJournal())
	res, err := e.Run(ctx, linearSpec("a", "b", "c"), RunOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, 期望含 context.Canceled", err)
	}
	if res.Status != RunStatusFailed {
		t.Fatalf("Status = %q, 期望取消一律 failed", res.Status)
	}
	if st.callCount("b") != 0 || st.callCount("c") != 0 {
		t.Fatalf("取消后仍调度了 b/c: %d/%d", st.callCount("b"), st.callCount("c"))
	}
	if _, ok := res.Nodes["b"]; ok {
		t.Fatalf("未调度节点不应出现在 Nodes: %+v", res.Nodes)
	}
}
