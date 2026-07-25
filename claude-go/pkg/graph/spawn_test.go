package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// spawnRunner 一个会在指定节点上发起派生的 runner。
type spawnRunner struct {
	mu    sync.Mutex
	calls map[string]int
	// req 在哪个节点发起派生, 以及派生什么
	from string
	req  SpawnRequest
	// 收集派生结果与错误, 供断言
	got     SpawnResult
	spawnEr error
	// times 在同一个节点里派生几次 (测 MaxSpawns)
	times int
	// noAuth true 时记录 in.Spawn 是否为 nil
	sawNilSpawn bool
}

func newSpawnRunner(from string, req SpawnRequest) *spawnRunner {
	return &spawnRunner{calls: map[string]int{}, from: from, req: req, times: 1}
}

func (r *spawnRunner) RunNode(ctx context.Context, node NodeSpec, in NodeInput) NodeResult {
	r.mu.Lock()
	r.calls[in.NodeRef]++
	r.mu.Unlock()

	if node.ID == r.from {
		if in.Spawn == nil {
			r.mu.Lock()
			r.sawNilSpawn = true
			r.mu.Unlock()
			return NodeResult{Status: NodeStatusCompleted, Output: "no-spawn"}
		}
		for i := 0; i < r.times; i++ {
			res, err := in.Spawn.Spawn(ctx, r.req)
			r.mu.Lock()
			if err != nil {
				r.spawnEr = err
			} else {
				r.got = res
			}
			r.mu.Unlock()
		}
	}
	return NodeResult{Status: NodeStatusCompleted, Output: node.ID + ":done"}
}

func (r *spawnRunner) count(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[ref]
}

// spawnSpec 一个 parent 节点声明了 Spawn 的图。
func spawnParentSpec(sp *SpawnSpec) GraphSpec {
	return GraphSpec{
		Name: "spawn-test",
		Nodes: []NodeSpec{{
			ID: "parent", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}, Spawn: sp,
		}},
	}
}

// 子图 2 节点串联。
func twoNodeReq() SpawnRequest {
	return SpawnRequest{
		Nodes: []NodeSpec{
			{ID: "sub-a", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
			{ID: "sub-b", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
		},
		Edges: []EdgeSpec{{From: "sub-a", To: "sub-b"}},
	}
}

// 未声明 Spawn 的节点必须拿到 nil —— 授权缺失表现为"没这个能力", 不是调了才报错。
func TestSpawn_未授权时入口为nil(t *testing.T) {
	r := newSpawnRunner("parent", twoNodeReq())
	e := &Engine{Runner: r}
	if _, err := e.Run(context.Background(), spawnParentSpec(nil), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if !r.sawNilSpawn {
		t.Error("未声明 Spawn 的节点却拿到了非 nil 派生入口 —— 等于把调度权交给模型")
	}
}

// 派生的子图节点必须真跑, 且进 journal 带命名空间前缀 —— 这是"对编排层可见"的实证。
func TestSpawn_子图真跑且进journal(t *testing.T) {
	j := NewMemoryJournal()
	r := newSpawnRunner("parent", twoNodeReq())
	e := &Engine{Runner: r, Journal: j, Interceptors: []NodeInterceptor{}}
	if _, err := e.Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if r.spawnEr != nil {
		t.Fatalf("派生失败: %v", r.spawnEr)
	}
	ns := r.got.Namespace
	if !strings.HasPrefix(ns, "parent"+SpawnIDInfix) || !strings.HasSuffix(ns, SpawnMemberIDSep) {
		t.Fatalf("命名空间 = %q, 期望形如 parent~sp<指纹>/", ns)
	}
	// 两个成员都真被执行 (按限定 ID 计数)
	for _, id := range []string{"sub-a", "sub-b"} {
		if r.count(ns+id) != 1 {
			t.Errorf("成员 %s 执行 %d 次, 期望 1", id, r.count(ns+id))
		}
	}
	// journal 里能看到派生事件与成员节点事件
	evs, _ := j.ReadAll()
	var spawned int
	memberEvents := map[string]bool{}
	for _, ev := range evs {
		if ev.Type == EvSubgraphSpawned {
			spawned++
			if ev.Data["namespace"] != ns {
				t.Errorf("spawned 事件 namespace = %v, 期望 %q", ev.Data["namespace"], ns)
			}
			if ev.Data["result_from"] != "sub-b" {
				t.Errorf("result_from = %v, 期望 sub-b (唯一出度 0 节点)", ev.Data["result_from"])
			}
		}
		if ev.Type == EvNodeCompleted && strings.HasPrefix(ev.NodeID, ns) {
			memberEvents[ev.NodeID] = true
		}
	}
	if spawned != 1 {
		t.Errorf("subgraph.spawned = %d 条, 期望 1", spawned)
	}
	if len(memberEvents) != 2 {
		t.Errorf("成员 node.completed 事件 %d 条, 期望 2 —— 少了就说明子图对编排层不可见", len(memberEvents))
	}
	// 结果取 ResultFrom 成员
	if r.got.Status != NodeStatusCompleted || r.got.Output != "sub-b:done" {
		t.Errorf("派生结果 = %+v, 期望取 sub-b 的产出", r.got)
	}
}

// 派生的开销必须计入父运行的预算 —— 否则一个节点可以在派生里烧掉任意多 token。
func TestSpawn_计入父运行预算(t *testing.T) {
	r := newSpawnRunner("parent", twoNodeReq())
	bm := NewBudgetManager(Budget{}, time.Now())
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}
	if _, err := e.Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	runs, _, _ := bm.Spent()
	// parent 1 次 + 子图 2 次 = 3
	if runs != 3 {
		t.Errorf("预算台账记了 %d 次执行, 期望 3 (父 1 + 子图 2) —— 派生若绕过拦截器链就是预算黑洞", runs)
	}
}

// 预算超限必须能掐住派生的子图 (fail-closed)。
func TestSpawn_预算能掐住子图(t *testing.T) {
	r := newSpawnRunner("parent", twoNodeReq())
	bm := NewBudgetManager(Budget{MaxNodeRuns: 2}, time.Now())
	e := &Engine{Runner: r, Interceptors: []NodeInterceptor{bm}}
	if _, err := e.Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	ns := r.got.Namespace
	// parent(1) + sub-a(2) 用满, sub-b 应被拒
	if r.count(ns+"sub-b") != 0 {
		t.Errorf("预算已满仍执行了 sub-b (%d 次)", r.count(ns+"sub-b"))
	}
	if ok, why := bm.Exceeded(); !ok {
		t.Errorf("预算应已超限: %v %q", ok, why)
	}
}

// 深度闸: 默认子图里的节点不能再派生。
func TestSpawn_深度闸(t *testing.T) {
	// 让子图成员自己也声明 Spawn 并尝试派生
	inner := SpawnRequest{Nodes: []NodeSpec{
		{ID: "leaf", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
	}}
	child := SpawnRequest{Nodes: []NodeSpec{
		{ID: "mid", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}, Spawn: &SpawnSpec{}},
	}}

	var innerErr error
	var mu sync.Mutex
	rn := &funcRunner{fn: func(ctx context.Context, node NodeSpec, in NodeInput) NodeResult {
		switch node.ID {
		case "parent":
			if in.Spawn != nil {
				if _, err := in.Spawn.Spawn(ctx, child); err != nil {
					mu.Lock()
					innerErr = err
					mu.Unlock()
				}
			}
		case "mid":
			if in.Spawn != nil {
				_, err := in.Spawn.Spawn(ctx, inner)
				mu.Lock()
				innerErr = err
				mu.Unlock()
			}
		}
		return NodeResult{Status: NodeStatusCompleted, Output: node.ID}
	}}
	j := NewMemoryJournal()
	e := &Engine{Runner: rn, Journal: j}
	if _, err := e.Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := innerErr
	mu.Unlock()
	if !errors.Is(got, ErrSpawnTooDeep) {
		t.Fatalf("二层派生应被深度闸拒绝, 实得 %v", got)
	}
	// 拒绝必须留痕, 否则"agent 说它派生了但图里什么都没有"无从解释
	evs, _ := j.ReadAll()
	found := false
	for _, ev := range evs {
		if ev.Type == EvSubgraphRejected && ev.Data["kind"] == "max_depth" {
			found = true
		}
	}
	if !found {
		t.Error("深度闸拒绝未记 subgraph.rejected")
	}
}

// funcRunner 任意逻辑的 runner。
type funcRunner struct {
	fn func(context.Context, NodeSpec, NodeInput) NodeResult
}

func (f *funcRunner) RunNode(ctx context.Context, n NodeSpec, in NodeInput) NodeResult {
	return f.fn(ctx, n, in)
}

// 单次节点数闸与派生次数闸是两道不同的闸 (后者挡"派生 500 次每次 1 个节点")。
func TestSpawn_条数与次数两道闸(t *testing.T) {
	big := SpawnRequest{}
	for i := 0; i < 5; i++ {
		big.Nodes = append(big.Nodes, NodeSpec{
			ID: fmt.Sprintf("n%d", i), Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"},
		})
	}
	big.ResultFrom = "n0"

	r := newSpawnRunner("parent", big)
	e := &Engine{Runner: r}
	if _, err := e.Run(context.Background(), spawnParentSpec(&SpawnSpec{MaxNodes: 3}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(r.spawnEr, ErrSpawnTooLarge) {
		t.Errorf("5 节点超 MaxNodes=3 应被拒, 实得 %v", r.spawnEr)
	}

	// 次数闸: 派生 3 次但上限 2 —— 每次请求内容不同, 否则会命中同请求复用
	callN := 0
	var mu sync.Mutex
	var lastErr error
	rn := &funcRunner{fn: func(ctx context.Context, node NodeSpec, in NodeInput) NodeResult {
		if node.ID == "parent" && in.Spawn != nil {
			for i := 0; i < 3; i++ {
				req := SpawnRequest{Nodes: []NodeSpec{{
					ID: fmt.Sprintf("s%d", i), Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"},
				}}}
				_, err := in.Spawn.Spawn(ctx, req)
				mu.Lock()
				if err != nil {
					lastErr = err
				} else {
					callN++
				}
				mu.Unlock()
			}
		}
		return NodeResult{Status: NodeStatusCompleted, Output: node.ID}
	}}
	if _, err := (&Engine{Runner: rn}).Run(context.Background(), spawnParentSpec(&SpawnSpec{MaxSpawns: 2}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if callN != 2 {
		t.Errorf("成功派生 %d 次, 期望 2 (MaxSpawns)", callN)
	}
	if !errors.Is(lastErr, ErrSpawnTooMany) {
		t.Errorf("第 3 次应被次数闸拒, 实得 %v", lastErr)
	}
}

// 同一次执行里请求同一个子图 → 复用, 不重复烧 LLM。
func TestSpawn_同请求复用(t *testing.T) {
	r := newSpawnRunner("parent", twoNodeReq())
	r.times = 3 // 同样的请求发三次
	e := &Engine{Runner: r}
	if _, err := e.Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if r.spawnEr != nil {
		t.Fatalf("同请求复用不该报错 (更不该撞次数闸): %v", r.spawnEr)
	}
	ns := r.got.Namespace
	if got := r.count(ns + "sub-a"); got != 1 {
		t.Errorf("sub-a 执行 %d 次, 期望 1 —— 同请求应复用而不是重跑", got)
	}
}

// 指纹只依赖请求内容: 声明顺序不同但内容相同 → 同一命名空间 (否则白丢 resume 缓存)。
func TestSpawnFingerprint_与声明顺序无关(t *testing.T) {
	a := twoNodeReq()
	b := SpawnRequest{
		Nodes: []NodeSpec{a.Nodes[1], a.Nodes[0]}, // 顺序反过来
		Edges: a.Edges,
	}
	if spawnFingerprint(a) != spawnFingerprint(b) {
		t.Error("同内容不同声明顺序算出不同指纹 —— resume 永远命中不了缓存")
	}
	// 内容不同必须不同指纹, 否则会把上一次的产出错配给这次
	c := twoNodeReq()
	c.Nodes[0].Agent.Role = "reviewer"
	if spawnFingerprint(a) == spawnFingerprint(c) {
		t.Error("不同内容算出相同指纹 —— resume 会错配产出")
	}
	d := twoNodeReq()
	d.Params = map[string]string{"k": "v"}
	if spawnFingerprint(a) == spawnFingerprint(d) {
		t.Error("Params 未进指纹")
	}
}

// 结构非法一律整段拒绝 (fail-closed), 不"尽力跑一部分"。
func TestSpawn_结构非法整段拒(t *testing.T) {
	cases := []struct {
		name string
		req  SpawnRequest
		want string
	}{
		{"空 ID", SpawnRequest{Nodes: []NodeSpec{{ID: "  ", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}}}}, "空节点 ID"},
		{
			"ID 重复",
			SpawnRequest{Nodes: []NodeSpec{
				{ID: "x", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}},
				{ID: "x", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}},
			}},
			"重复",
		},
		{
			"边指向子图外",
			SpawnRequest{
				Nodes: []NodeSpec{{ID: "x", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}}},
				Edges: []EdgeSpec{{From: "x", To: "parent"}},
			},
			"必须都在子图内",
		},
		{
			"多个出度0未声明 result_from",
			SpawnRequest{Nodes: []NodeSpec{
				{ID: "p", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}},
				{ID: "q", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}},
			}},
			"result_from",
		},
		{
			"result_from 不在子图内",
			SpawnRequest{
				Nodes:      []NodeSpec{{ID: "x", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}}},
				ResultFrom: "ghost",
			},
			"不在派生子图内",
		},
		{
			"未实现的 Kind",
			SpawnRequest{Nodes: []NodeSpec{{ID: "x", Kind: NodeKindRouter, Agent: AgentSpec{Role: "w"}}}},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSpawnRunner("parent", tc.req)
			if _, err := (&Engine{Runner: r}).Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{}); err != nil {
				t.Fatal(err)
			}
			if r.spawnEr == nil {
				t.Fatalf("非法请求未被拒: %+v", tc.req)
			}
			if tc.want != "" && !strings.Contains(r.spawnEr.Error(), tc.want) {
				t.Errorf("错误 = %q, 期望含 %q", r.spawnEr.Error(), tc.want)
			}
		})
	}
}

// 单调收窄: 子图不得放宽父节点的约束 (复用 expand 的 narrowToParent)。
func TestSpawn_约束只能收窄(t *testing.T) {
	parent := NodeSpec{
		ID: "parent", Kind: NodeKindAgent,
		Agent:      AgentSpec{Role: "worker", MaxTurns: 5},
		TimeoutSec: 60,
		Spawn:      &SpawnSpec{},
	}
	req := SpawnRequest{Nodes: []NodeSpec{{
		ID: "child", Kind: NodeKindAgent,
		Agent:      AgentSpec{Role: "worker", MaxTurns: 999}, // 想放宽
		TimeoutSec: 9999,                                     // 想放宽
	}}}
	sub, _, err := prepareSpawn(parent, req, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	c := sub.Nodes[0]
	if c.Agent.MaxTurns > parent.Agent.MaxTurns {
		t.Errorf("MaxTurns 被放宽到 %d (父 %d)", c.Agent.MaxTurns, parent.Agent.MaxTurns)
	}
	if c.TimeoutSec > parent.TimeoutSec {
		t.Errorf("TimeoutSec 被放宽到 %d (父 %d)", c.TimeoutSec, parent.TimeoutSec)
	}
}

// Spawn 的上限允许 0 (取缺省) 但负值必须在 Validate 阶段拒 ——
// 负数会被 "<=0 取缺省" 静默当成"没设", 一个写错的 -1 就绕过了边界。
func TestSpawn_负上限被Validate拒(t *testing.T) {
	for _, sp := range []*SpawnSpec{{MaxDepth: -1}, {MaxNodes: -1}, {MaxSpawns: -1}} {
		g := spawnParentSpec(sp)
		if err := g.Validate(); err == nil {
			t.Errorf("负上限 %+v 未被拒", sp)
		}
	}
	if err := spawnParentSpec(&SpawnSpec{}).Validate(); err != nil {
		t.Errorf("全 0 (取缺省) 应合法: %v", err)
	}
}

// 派生的子图节点算运行图总量, 撞总量闸时整段拒 (不部分接纳 —— 那会得到断图)。
func TestSpawn_撞总量闸整段拒(t *testing.T) {
	g := spawnParentSpec(&SpawnSpec{})
	g.Policies.MaxTotalNodes = 1 // 只够 parent 自己
	r := newSpawnRunner("parent", twoNodeReq())
	if _, err := (&Engine{Runner: r}).Run(context.Background(), g, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(r.spawnEr, ErrSpawnNoRoom) {
		t.Errorf("应撞总量闸, 实得 %v", r.spawnEr)
	}
	ns := "" // 派生被拒, 无命名空间
	_ = ns
	if len(r.got.Nodes) != 0 {
		t.Error("被拒的派生却返回了成员结果")
	}
}

// 派生请求可带本层参数, 且同名覆盖运行级参数。
func TestSpawn_本层参数覆盖(t *testing.T) {
	req := SpawnRequest{
		Nodes:  []NodeSpec{{ID: "child", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}}},
		Params: map[string]string{"lang": "zh", "extra": "1"},
	}
	var seen map[string]string
	var mu sync.Mutex
	rn := &funcRunner{fn: func(ctx context.Context, node NodeSpec, in NodeInput) NodeResult {
		if node.ID == "parent" && in.Spawn != nil {
			in.Spawn.Spawn(ctx, req)
		}
		if node.ID == "child" {
			mu.Lock()
			seen = in.Params
			mu.Unlock()
		}
		return NodeResult{Status: NodeStatusCompleted, Output: node.ID}
	}}
	_, err := (&Engine{Runner: rn}).Run(context.Background(), spawnParentSpec(&SpawnSpec{}), RunOpts{
		Params: map[string]string{"lang": "en", "keep": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["lang"] != "zh" {
		t.Errorf("lang = %q, 期望被本层参数覆盖为 zh", seen["lang"])
	}
	if seen["keep"] != "yes" {
		t.Errorf("keep = %q, 运行级参数应仍可见", seen["keep"])
	}
	if seen["extra"] != "1" {
		t.Errorf("extra = %q, 本层新增参数应可见", seen["extra"])
	}
}

// 运行级参数不能被派生污染 (合并要产生新 map, 不能改 rc.params)。
func TestSpawn_不污染运行级参数(t *testing.T) {
	runParams := map[string]string{"lang": "en"}
	req := SpawnRequest{
		Nodes:  []NodeSpec{{ID: "child", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}}},
		Params: map[string]string{"lang": "zh"},
	}
	rn := &funcRunner{fn: func(ctx context.Context, node NodeSpec, in NodeInput) NodeResult {
		if node.ID == "parent" && in.Spawn != nil {
			in.Spawn.Spawn(ctx, req)
		}
		return NodeResult{Status: NodeStatusCompleted, Output: node.ID}
	}}
	if _, err := (&Engine{Runner: rn}).Run(context.Background(), spawnParentSpec(&SpawnSpec{}),
		RunOpts{Params: runParams}); err != nil {
		t.Fatal(err)
	}
	if runParams["lang"] != "en" {
		t.Errorf("运行级参数被派生改成了 %q", runParams["lang"])
	}
}
