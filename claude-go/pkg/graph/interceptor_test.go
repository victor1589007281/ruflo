package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// icRunner 可编程 runner: 记录每个节点被调用次数, 按节点 ID 返回预设结果。
type icRunner struct {
	mu     sync.Mutex
	calls  map[string]int
	out    map[string]NodeResult
	fail   map[string]bool
	tokens int64 // 每次执行回报的 token, 0 = 不回报
}

func newICRunner() *icRunner {
	return &icRunner{calls: map[string]int{}, out: map[string]NodeResult{}, fail: map[string]bool{}}
}

func (r *icRunner) RunNode(_ context.Context, node NodeSpec, _ NodeInput) NodeResult {
	r.mu.Lock()
	r.calls[node.ID]++
	n := r.calls[node.ID]
	preset, hasPreset := r.out[node.ID]
	shouldFail := r.fail[node.ID]
	tk := r.tokens
	r.mu.Unlock()

	if hasPreset {
		return preset
	}
	if shouldFail {
		return NodeResult{Status: NodeStatusFailed, Err: "runner: 预设失败", Tokens: tk}
	}
	return NodeResult{Status: NodeStatusCompleted, Output: fmt.Sprintf("%s#%d", node.ID, n), Tokens: tk}
}

func (r *icRunner) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

// —— 链语义 ——

// 顺序必须确定且 [0] 在最外层。这不是审美问题: BudgetManager 必须在
// EvolutionRecorder 外层, 否则超预算被拒的节点也会被记轨迹, 污染学习数据。
func TestInterceptorChain_顺序确定且0在最外层(t *testing.T) {
	var order []string
	var mu sync.Mutex
	mk := func(name string) NodeInterceptor {
		return FuncInterceptor{N: name, Fn: func(ctx context.Context, n NodeSpec, in NodeInput, next NodeExec) NodeResult {
			mu.Lock()
			order = append(order, "→"+name)
			mu.Unlock()
			res := next(ctx, n, in)
			mu.Lock()
			order = append(order, "←"+name)
			mu.Unlock()
			return res
		}}
	}
	e := &Engine{Runner: newICRunner(), Interceptors: []NodeInterceptor{mk("a"), mk("b"), mk("c")}}
	if _, err := e.Run(context.Background(), linearSpec("n1"), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(order, " ")
	want := "→a →b →c ←c ←b ←a"
	if got != want {
		t.Errorf("链顺序 = %q, 期望 %q", got, want)
	}
}

// 拦截器能修饰产出, 且修饰后的产出要真的传给下游 (进 journal / PrevOutputs)。
func TestInterceptor_修饰产出传给下游(t *testing.T) {
	r := newICRunner()
	var seenByDownstream string
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "upper", Fn: func(ctx context.Context, n NodeSpec, in NodeInput, next NodeExec) NodeResult {
			if n.ID == "n2" {
				seenByDownstream = in.PrevOutputs["n1"]
			}
			res := next(ctx, n, in)
			res.Output = strings.ToUpper(res.Output)
			return res
		}},
	}}
	res, err := e.Run(context.Background(), linearSpec("n1", "n2"), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Nodes["n1"].Output != "N1#1" {
		t.Errorf("n1 产出 = %q, 期望被修饰为 N1#1", res.Nodes["n1"].Output)
	}
	if seenByDownstream != "N1#1" {
		t.Errorf("下游看到的上游产出 = %q, 期望是修饰后的 N1#1 —— 修饰若不传下去, 拦截器就只是个观测器", seenByDownstream)
	}
}

// 忘了调 next 且没给终态 = 节点被吞掉。必须报错而不是静默 —— 拦截器是第三方
// 注入点, 静默吞节点会让排查无从下手。
func TestInterceptor_忘调next被检出(t *testing.T) {
	r := newICRunner()
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "swallow", Fn: func(context.Context, NodeSpec, NodeInput, NodeExec) NodeResult {
			return NodeResult{} // 既不调 next 也不给终态
		}},
	}}
	res, _ := e.Run(context.Background(), linearSpec("n1"), RunOpts{})
	if res.Nodes["n1"].Status != NodeStatusFailed {
		t.Fatalf("被吞的节点状态 = %q, 期望 failed", res.Nodes["n1"].Status)
	}
	if !strings.Contains(res.Nodes["n1"].Err, "未调用 next") || !strings.Contains(res.Nodes["n1"].Err, "swallow") {
		t.Errorf("错误须点名拦截器: %q", res.Nodes["n1"].Err)
	}
	if r.count("n1") != 0 {
		t.Errorf("runner 不应被调用, 实得 %d 次", r.count("n1"))
	}
}

// 明示拒绝 (不调 next 但给出终态) 是正当用法, 不能被当成"忘了调 next"。
// 预算拦截器就靠这条路径工作。
func TestInterceptor_明示拒绝不算误用(t *testing.T) {
	r := newICRunner()
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "denier", Fn: func(context.Context, NodeSpec, NodeInput, NodeExec) NodeResult {
			return NodeResult{Status: NodeStatusSkipped, Err: "策略拒绝"}
		}},
	}}
	res, _ := e.Run(context.Background(), linearSpec("n1"), RunOpts{})
	if res.Nodes["n1"].Status != NodeStatusSkipped {
		t.Errorf("状态 = %q, 期望 skipped", res.Nodes["n1"].Status)
	}
	if res.Nodes["n1"].Err != "策略拒绝" {
		t.Errorf("原因被改写成了 %q —— 明示拒绝的理由不该被链覆盖", res.Nodes["n1"].Err)
	}
	if r.count("n1") != 0 {
		t.Errorf("runner 不应被调用")
	}
}

// 重复调 next 会绕过引擎的重试计数, 必须被检出。
func TestInterceptor_重复调next被检出(t *testing.T) {
	r := newICRunner()
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "double", Fn: func(ctx context.Context, n NodeSpec, in NodeInput, next NodeExec) NodeResult {
			next(ctx, n, in)
			return next(ctx, n, in) // 第二次
		}},
	}}
	res, _ := e.Run(context.Background(), linearSpec("n1"), RunOpts{})
	if res.Nodes["n1"].Status != NodeStatusFailed {
		t.Fatalf("状态 = %q, 期望 failed", res.Nodes["n1"].Status)
	}
	if !strings.Contains(res.Nodes["n1"].Err, "重复调用 next") {
		t.Errorf("错误须说明重复调用: %q", res.Nodes["n1"].Err)
	}
}

// 第三方拦截器 panic 不能带走整个图运行, 但也不能被当成成功放行。
func TestInterceptor_panic不穿透且转失败(t *testing.T) {
	e := &Engine{Runner: newICRunner(), Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "boom", Fn: func(context.Context, NodeSpec, NodeInput, NodeExec) NodeResult {
			panic("第三方拦截器炸了")
		}},
	}}
	res, _ := e.Run(context.Background(), linearSpec("n1", "n2"), RunOpts{})
	if res.Nodes["n1"].Status != NodeStatusFailed {
		t.Fatalf("状态 = %q, 期望 failed (panic 不能算放行)", res.Nodes["n1"].Status)
	}
	if !strings.Contains(res.Nodes["n1"].Err, "boom") || !strings.Contains(res.Nodes["n1"].Err, "panic") {
		t.Errorf("错误须点名拦截器与 panic: %q", res.Nodes["n1"].Err)
	}
}

// 空链必须零行为改变 —— 这是"默认不改变现状"的硬要求。
func TestInterceptor_空链行为不变(t *testing.T) {
	spec := linearSpec("n1", "n2", "n3")
	r1, r2 := newICRunner(), newICRunner()
	res1, err1 := (&Engine{Runner: r1}).Run(context.Background(), spec, RunOpts{})
	res2, err2 := (&Engine{Runner: r2, Interceptors: nil}).Run(context.Background(), spec, RunOpts{})
	if err1 != nil || err2 != nil {
		t.Fatalf("err1=%v err2=%v", err1, err2)
	}
	if res1.Status != res2.Status || len(res1.Nodes) != len(res2.Nodes) {
		t.Errorf("空链改变了行为: %+v vs %+v", res1.Status, res2.Status)
	}
	for id, n := range res1.Nodes {
		if res2.Nodes[id].Output != n.Output || res2.Nodes[id].Status != n.Status {
			t.Errorf("节点 %s 结果不一致", id)
		}
	}
}

// 重名拦截器在开跑前就要被拒: Name 是 journal 归因的键, 重名让归因永久不可考。
func TestInterceptor_重名与空名被拒(t *testing.T) {
	for _, tc := range []struct {
		name string
		ics  []NodeInterceptor
		want string
	}{
		{"重名", []NodeInterceptor{FuncInterceptor{N: "x"}, FuncInterceptor{N: "x"}}, "重名"},
		{"空名", []NodeInterceptor{FuncInterceptor{N: "   "}}, "Name 为空"},
		{"nil", []NodeInterceptor{nil}, "为 nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{Runner: newICRunner(), Interceptors: tc.ics}
			_, err := e.Run(context.Background(), linearSpec("n1"), RunOpts{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, 期望含 %q", err, tc.want)
			}
		})
	}
}

// 链构成要进 run.created 的 Data, 且**只发一条** run.created ——
// Replay 靠定位最后一条 run.created 划定本次运行, 多发会让重放范围错位。
func TestInterceptor_链构成进run_created且不重复发(t *testing.T) {
	j := NewMemoryJournal()
	e := &Engine{Runner: newICRunner(), Journal: j, Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "budget"}, FuncInterceptor{N: "metrics"},
	}}
	if _, err := e.Run(context.Background(), linearSpec("n1"), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	evs, _ := j.ReadAll()
	created := 0
	var names []string
	for _, ev := range evs {
		if ev.Type != EvRunCreated {
			continue
		}
		created++
		if v, ok := ev.Data["interceptors"].([]string); ok {
			names = v
		}
	}
	if created != 1 {
		t.Errorf("run.created 条数 = %d, 必须恰好 1 (否则 Replay 范围错位)", created)
	}
	if len(names) != 2 || names[0] != "budget" || names[1] != "metrics" {
		t.Errorf("链构成 = %v, 期望按注册顺序 [budget metrics]", names)
	}
}

// 拦截器必须覆盖重试的每一轮 —— 预算记账漏掉重试就等于没有预算。
func TestInterceptor_覆盖重试每一轮(t *testing.T) {
	r := newICRunner()
	r.fail["n1"] = true
	seen := 0
	var mu sync.Mutex
	e := &Engine{
		Runner:  r,
		sleepFn: func(context.Context, time.Duration) bool { return true }, // 免退避等待
		Interceptors: []NodeInterceptor{FuncInterceptor{N: "count", Fn: func(ctx context.Context, n NodeSpec, in NodeInput, next NodeExec) NodeResult {
			mu.Lock()
			seen++
			mu.Unlock()
			return next(ctx, n, in)
		}}},
	}
	spec := linearSpec("n1")
	spec.Nodes[0].Retry = &RetryPolicy{MaxRetries: 2, BackoffSec: 1}
	if _, err := e.Run(context.Background(), spec, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if seen != 3 || r.count("n1") != 3 {
		t.Errorf("拦截器看到 %d 次 / runner 被调 %d 次, 期望都是 3 (重试每一轮都要过链)", seen, r.count("n1"))
	}
}

// map 分片与 loop-group 组内节点也必须过链 (它们走的是嵌套调度路径)。
func TestInterceptor_覆盖map分片(t *testing.T) {
	r := newICRunner()
	r.out["src"] = NodeResult{Status: NodeStatusCompleted, Output: "a\nb\nc"}
	var mu sync.Mutex
	seen := map[string]int{}
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{
		FuncInterceptor{N: "spy", Fn: func(ctx context.Context, n NodeSpec, in NodeInput, next NodeExec) NodeResult {
			mu.Lock()
			key := n.ID
			if in.Shard != nil {
				// 分片的节点 ID 是 <map节点>#<序号>, 归并到一个键上计数
				key = "shard:" + in.Shard.MapID
			}
			seen[key]++
			mu.Unlock()
			return next(ctx, n, in)
		}},
	}}
	spec := GraphSpec{
		Name: "map-ic",
		Nodes: []NodeSpec{
			{ID: "src", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
			{ID: "m", Kind: NodeKindMap, Agent: AgentSpec{Role: "worker"}, Map: &MapPolicy{Source: "prev:src", Split: SplitLines, MaxShards: 8}},
		},
		Edges: []EdgeSpec{{From: "src", To: "m"}},
	}
	if _, err := e.Run(context.Background(), spec, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["shard:m"] != 3 {
		t.Errorf("map 分片过链 %d 次, 期望 3 —— 嵌套调度路径若绕过链, 扇出就成了预算黑洞", seen["shard:m"])
	}
}

// —— BudgetManager ——

// 节点执行次数预算: 超限后拒绝放行, 且拒绝要留证据 (budget.exceeded 事件)。
func TestBudget_节点次数超限拒绝并留痕(t *testing.T) {
	r := newICRunner()
	j := NewMemoryJournal()
	// 记账函数由 Engine.Run 自动注入 —— 这正是 journalAware 存在的理由:
	// 若靠构造参数传, 外部包 (pkg/agent) 造不出 evAppender, 事件一条不落。
	bm2 := NewBudgetManager(Budget{MaxNodeRuns: 2}, time.Now())
	e := &Engine{Runner: r, Journal: j, Interceptors: []NodeInterceptor{bm2}}

	res, _ := e.Run(context.Background(), linearSpec("n1", "n2", "n3"), RunOpts{})
	if r.count("n1") != 1 || r.count("n2") != 1 {
		t.Errorf("前两个节点应正常执行: n1=%d n2=%d", r.count("n1"), r.count("n2"))
	}
	if r.count("n3") != 0 {
		t.Errorf("第三个节点应被预算拒绝, 实得执行 %d 次", r.count("n3"))
	}
	if res.Nodes["n3"].Status != NodeStatusFailed {
		t.Errorf("n3 状态 = %q, 期望 failed", res.Nodes["n3"].Status)
	}
	if !strings.Contains(res.Nodes["n3"].Err, "node_runs") {
		t.Errorf("拒绝原因应点明是哪一项预算: %q", res.Nodes["n3"].Err)
	}
	if ok, why := bm2.Exceeded(); !ok || !strings.Contains(why, "node_runs") {
		t.Errorf("Exceeded() = %v, %q", ok, why)
	}
	// 留痕: 必须能区分"节点失败"与"没让它跑"
	evs, _ := j.ReadAll()
	var consumed, exceeded int
	for _, ev := range evs {
		switch ev.Type {
		case EvBudgetConsumed:
			consumed++
		case EvBudgetExceeded:
			exceeded++
		}
	}
	if consumed != 2 {
		t.Errorf("budget.consumed = %d 条, 期望 2 (每次真执行记一条)", consumed)
	}
	if exceeded != 1 {
		t.Errorf("budget.exceeded = %d 条, 期望 1", exceeded)
	}
}

// OnExceed=skip: 超限的节点置 skipped 而不是 failed (让余图跑完)。
func TestBudget_OnExceed_skip(t *testing.T) {
	r := newICRunner()
	bm := NewBudgetManager(Budget{MaxNodeRuns: 1, OnExceed: "skip"}, time.Now())
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}
	res, _ := e.Run(context.Background(), linearSpec("n1", "n2"), RunOpts{})
	if res.Nodes["n2"].Status != NodeStatusSkipped {
		t.Errorf("n2 状态 = %q, 期望 skipped", res.Nodes["n2"].Status)
	}
}

// 判定必须在执行**前**: 执行后判定意味着预算总会被超出至少一个节点的开销。
func TestBudget_执行前判定不透支(t *testing.T) {
	r := newICRunner()
	bm := NewBudgetManager(Budget{MaxNodeRuns: 3}, time.Now())
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}
	if _, err := e.Run(context.Background(), linearSpec("a", "b", "c", "d", "e"), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	runs, _, _ := bm.Spent()
	if runs != 3 {
		t.Errorf("实际执行 %d 次, 期望恰好 3 次 —— 多一次就是透支", runs)
	}
}

// 单节点次数上限不该钉住全局: 别的节点还该继续跑。
func TestBudget_单节点超限不拖累其他节点(t *testing.T) {
	r := newICRunner()
	r.fail["n1"] = true
	bm := NewBudgetManager(Budget{PerNodeMaxRuns: 2}, time.Now())
	e := &Engine{
		Runner: r, Interceptors: []NodeInterceptor{bm},
		sleepFn: func(context.Context, time.Duration) bool { return true },
	}
	spec := GraphSpec{
		Name: "per-node",
		Nodes: []NodeSpec{
			{ID: "n1", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}, Retry: &RetryPolicy{MaxRetries: 4, BackoffSec: 1}},
			{ID: "n2", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
		},
	}
	if _, err := e.Run(context.Background(), spec, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if r.count("n1") != 2 {
		t.Errorf("n1 执行 %d 次, 期望被 PerNodeMaxRuns 卡在 2", r.count("n1"))
	}
	if r.count("n2") != 1 {
		t.Errorf("n2 执行 %d 次, 期望 1 —— 单节点超限不该钉住全局", r.count("n2"))
	}
	if ok, _ := bm.Exceeded(); ok {
		t.Error("单节点超限不应置全局 exceeded")
	}
}

// token 预算只在 runner 真回报时生效; 回报 0 必须与"没花"可区分。
func TestBudget_token只在真回报时记账(t *testing.T) {
	// 不回报: MaxTokens 形同不设, 全部节点都该跑
	silent := newICRunner()
	bmS := NewBudgetManager(Budget{MaxTokens: 10}, time.Now())
	if _, err := (&Engine{Runner: silent, Interceptors: []NodeInterceptor{bmS}}).
		Run(context.Background(), linearSpec("a", "b", "c"), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if silent.count("c") != 1 {
		t.Error("runner 不回报用量时不该凭空拦人 —— 那会把'未实现回报'变成'免费'的反面误判")
	}
	if _, tk, _ := bmS.Spent(); tk != 0 {
		t.Errorf("未回报时 tokens = %d, 期望 0", tk)
	}

	// 回报: 第三个节点应被拦
	loud := newICRunner()
	loud.tokens = 6
	bmL := NewBudgetManager(Budget{MaxTokens: 10}, time.Now())
	res, _ := (&Engine{Runner: loud, Interceptors: []NodeInterceptor{bmL}}).
		Run(context.Background(), linearSpec("a", "b", "c"), RunOpts{})
	if loud.count("c") != 0 {
		t.Errorf("累计 12 > 10 应拦下 c, 实得执行 %d 次", loud.count("c"))
	}
	if !strings.Contains(res.Nodes["c"].Err, "tokens") {
		t.Errorf("拒绝原因 = %q, 期望点明 tokens", res.Nodes["c"].Err)
	}
}

// 墙钟预算与节点级 TimeoutSec 是两回事: 后者管单节点卡死,
// 前者管"每个节点都不超时但总共跑了六小时"。
func TestBudget_墙钟预算(t *testing.T) {
	r := newICRunner()
	// started 设在过去 → 首个节点就该被拦
	bm := NewBudgetManager(Budget{MaxWallClock: 50 * time.Millisecond}, time.Now().Add(-time.Second))
	res, _ := (&Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}).
		Run(context.Background(), linearSpec("n1"), RunOpts{})
	if r.count("n1") != 0 {
		t.Errorf("墙钟已超应拦下, 实得执行 %d 次", r.count("n1"))
	}
	if !strings.Contains(res.Nodes["n1"].Err, "wall_clock") {
		t.Errorf("拒绝原因 = %q, 期望点明 wall_clock", res.Nodes["n1"].Err)
	}
}

// 零值 Budget = 不限制。缺省不改变现状行为是硬要求。
func TestBudget_零值不限制(t *testing.T) {
	r := newICRunner()
	bm := NewBudgetManager(Budget{}, time.Now())
	res, err := (&Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}).
		Run(context.Background(), linearSpec("a", "b", "c", "d"), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunStatusCompleted {
		t.Errorf("状态 = %q, 期望 completed", res.Status)
	}
	if runs, _, _ := bm.Spent(); runs != 4 {
		t.Errorf("执行 %d 次, 期望 4 次全跑", runs)
	}
	if ok, _ := bm.Exceeded(); ok {
		t.Error("零值预算不该超限")
	}
}

// 并发安全: 多节点 goroutine 同时过链与记账。
func TestBudget_并发记账(t *testing.T) {
	r := newICRunner()
	bm := NewBudgetManager(Budget{}, time.Now())
	spec := GraphSpec{Name: "par", Policies: GraphPolicies{MaxParallel: 8}}
	for i := 0; i < 24; i++ {
		spec.Nodes = append(spec.Nodes, NodeSpec{ID: fmt.Sprintf("n%02d", i), Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}})
	}
	if _, err := (&Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}).
		Run(context.Background(), spec, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if runs, _, _ := bm.Spent(); runs != 24 {
		t.Errorf("并发记账 = %d, 期望 24", runs)
	}
}

// 预算台账**不从 journal 重建**: resume 后续跑应按新预算重新计,
// 否则一个跑了六轮的图永远无法 resume (旧账已把预算吃满)。
func TestBudget_resume不吃旧账(t *testing.T) {
	j := NewMemoryJournal()
	r1 := newICRunner()
	r1.fail["n2"] = true
	spec := linearSpec("n1", "n2", "n3")
	e1 := &Engine{Runner: r1, Journal: j, Interceptors: []NodeInterceptor{
		NewBudgetManager(Budget{MaxNodeRuns: 2}, time.Now()),
	}}
	if _, err := e1.Run(context.Background(), spec, RunOpts{RunID: "r-b"}); err != nil {
		t.Fatal(err)
	}

	// 续跑: 新的预算管理器从 0 起算, n2 应能被重试
	r2 := newICRunner()
	e2 := &Engine{Runner: r2, Journal: j, Interceptors: []NodeInterceptor{
		NewBudgetManager(Budget{MaxNodeRuns: 5}, time.Now()),
	}}
	if _, err := e2.Run(context.Background(), spec, RunOpts{RunID: "r-b", Resume: true}); err != nil {
		t.Fatal(err)
	}
	if r2.count("n2") == 0 {
		t.Error("resume 后 n2 未被重跑 —— 预算台账若从 journal 重建, 旧账会让续跑永远超限")
	}
}
