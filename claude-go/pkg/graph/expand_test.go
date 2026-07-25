package graph

// 动态展开 ExpandSpec 行为回归 (design/01 §4.2)。
// 重点断言: 子图真的并入并被调度、父节点原下游真的等到子图跑完、四道边界闸真的拦住、
// 约束真的只收窄、resume 后按 journal 重建**不重新问 runner**且图与首跑一致。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// planSpec planner (可展开) → sum (原有下游)。
func planSpec(expand *ExpandSpec) GraphSpec {
	return GraphSpec{
		Name: "expand",
		Nodes: []NodeSpec{
			{ID: "planner", Kind: NodeKindAgent, Agent: AgentSpec{Role: "planner"}, Expand: expand},
			vNode("sum"),
		},
		Edges: []EdgeSpec{{From: "planner", To: "sum"}},
	}
}

// twoSubtasks 两个子任务的子图 (t1、t2 都挂在父节点下)。
func twoSubtasks() *Expansion {
	return &Expansion{
		Nodes: []NodeSpec{
			{ID: "t1", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
			{ID: "t2", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}},
		},
		Edges: []EdgeSpec{{From: "planner", To: "t1"}, {From: "planner", To: "t2"}},
	}
}

func expandedIDs(res RunResult) []string {
	var out []string
	for _, n := range res.Graph.Nodes {
		if strings.Contains(n.ID, expandedNodeIDSeparator) {
			out = append(out, n.ID)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 1. 展开生效: 子图并入运行图并被调度; 父节点的原下游等到子图跑完
// ---------------------------------------------------------------------------

func TestExpandAppendsSubgraphAndRewiresDownstream(t *testing.T) {
	st := newStub()
	st.fn["planner"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "分解完成", Expansion: twoSubtasks()}
	}
	j := NewMemoryJournal()
	res, err := fastEngine(st, j).Run(context.Background(),
		planSpec(&ExpandSpec{MaxNodes: 5}), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, nodes=%+v", res.Status, res.Nodes)
	}
	// 子节点 ID 被命名空间化为 <父>/<子>
	if got := expandedIDs(res); len(got) != 2 || got[0] != "planner/t1" || got[1] != "planner/t2" {
		t.Fatalf("展开产物 ID = %v, 期望 [planner/t1 planner/t2]", got)
	}
	for _, id := range []string{"planner/t1", "planner/t2"} {
		if st.callCount(id) != 1 {
			t.Fatalf("展开节点 %s 调用次数 = %d, 期望 1", id, st.callCount(id))
		}
		if res.Nodes[id].Status != NodeStatusCompleted {
			t.Fatalf("展开节点 %s = %+v", id, res.Nodes[id])
		}
		// 子节点看得到父节点的产出 (它是父的直接下游)
		if in := st.input(id, 0); in.PrevOutputs["planner"] != "分解完成" {
			t.Fatalf("%s 的 PrevOutputs = %v, 期望含父节点产出", id, in.PrevOutputs)
		}
	}
	// 重接线: sum 必须等到两个子任务都终态, 且拿得到它们的产出
	in := st.input("sum", 0)
	for _, id := range []string{"planner/t1", "planner/t2"} {
		if in.PrevOutputs[id] != id+"-out" {
			t.Fatalf("sum 的 PrevOutputs 缺 %s: %v (下游没等子图 ⇒ 汇总拿不到子任务产出)", id, in.PrevOutputs)
		}
	}
	st.mu.Lock()
	idx := map[string]int{}
	for i, id := range st.calls {
		idx[id] = i
	}
	st.mu.Unlock()
	if idx["sum"] < idx["planner/t1"] || idx["sum"] < idx["planner/t2"] {
		t.Fatalf("sum 未等待展开子图: 调用序 %v", st.calls)
	}
	// journal: 一条 graph.expanded, 带父节点/深度/条数
	exp := findEvents(t, j, EvGraphExpanded)
	if len(exp) != 1 {
		t.Fatalf("graph.expanded 事件数 = %d, 期望 1", len(exp))
	}
	if exp[0].Data["parent"] != "planner" || evInt(t, exp[0].Data, "count") != 2 || evInt(t, exp[0].Data, "depth") != 1 {
		t.Fatalf("graph.expanded Data = %v, 期望 parent=planner count=2 depth=1", exp[0].Data)
	}
	if s, _ := exp[0].Data["subgraph"].(string); !strings.Contains(s, "planner/t1") {
		t.Fatalf("graph.expanded 未记下命名空间化后的子图: %q", s)
	}
}

// 展开出来的节点自己也能是 map 节点 (形态校验对展开产物同样生效, 通过的就该真跑)。
func TestExpandCanIntroduceMapNode(t *testing.T) {
	st := newStub()
	st.fn["planner"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "甲\n乙", Expansion: &Expansion{
			Nodes: []NodeSpec{{ID: "fan", Kind: NodeKindMap, Agent: AgentSpec{Role: "worker"},
				Map: &MapPolicy{MaxShards: 4, Source: "prev:planner"}}},
			Edges: []EdgeSpec{{From: "planner", To: "fan"}},
		}}
	}
	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(),
		planSpec(&ExpandSpec{MaxNodes: 5}), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, nodes=%+v", res.Status, res.Nodes)
	}
	for i := 0; i < 2; i++ {
		id := shardNodeID("planner/fan", i)
		if st.callCount(id) != 1 {
			t.Fatalf("展开出来的 map 的分片 %s 未执行 (%d 次)", id, st.callCount(id))
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 四道边界闸 + 结构校验: 每一条都真的拦住, 且记 graph.expand_rejected
// ---------------------------------------------------------------------------

func TestExpandBoundsRejected(t *testing.T) {
	cases := []struct {
		name     string
		expand   *ExpandSpec // 父节点的展开授权
		sub      *Expansion
		policies GraphPolicies
		wantSub  string // 拒绝原因必须包含的子串
	}{
		{
			name: "未授权展开", expand: nil, sub: twoSubtasks(),
			wantSub: "未声明 Expand",
		},
		{
			name: "单次条数超限", expand: &ExpandSpec{MaxNodes: 1}, sub: twoSubtasks(),
			wantSub: "超过上限",
		},
		{
			name:     "运行图总量超限",
			expand:   &ExpandSpec{MaxNodes: 5},
			sub:      twoSubtasks(),
			policies: GraphPolicies{MaxTotalNodes: 3}, // 已有 planner+sum=2, 再加 2 就超
			wantSub:  "运行图节点总数",
		},
		{
			name:   "悬空子节点",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{vNode("t1"), vNode("t2")},
				Edges: []EdgeSpec{{From: "planner", To: "t1"}}, // t2 没有入边 → 会抢先执行
			},
			wantSub: "未从父节点",
		},
		{
			name:   "子图内有环",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{vNode("a"), vNode("b")},
				Edges: []EdgeSpec{{From: "planner", To: "a"}, {From: "a", To: "b"}, {From: "b", To: "a"}},
			},
			wantSub: "有向环",
		},
		{
			name:   "边指向图里已有节点",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{vNode("t1")},
				Edges: []EdgeSpec{{From: "planner", To: "t1"}, {From: "t1", To: "sum"}},
			},
			wantSub: "终点必须是本次展开的新节点",
		},
		{
			// router 是**不实现**的形态: 混进展开产物照样被拒 (原案例用 human,
			// human 现已实现, 见下一个案例 —— 它换了一条更强的理由被拒)。
			name:   "未实现的Kind混进来",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{{ID: "r", Kind: NodeKindRouter}},
				Edges: []EdgeSpec{{From: "planner", To: "r"}},
			},
			wantSub: "尚未实现",
		},
		{
			// human 已实现, 但**不许由 LLM 产出塞进来**: 那等于让模型自己插入一个
			// 审批闸并决定"这次运行到哪结束"。
			name:   "LLM产出里塞human节点",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{{ID: "h", Kind: NodeKindHuman}},
				Edges: []EdgeSpec{{From: "planner", To: "h"}},
			},
			wantSub: "human 不得出现在组内/派生子图/展开产物里",
		},
		{
			// 同理: 声明了 suspend 的展开产物也被拒 (模型不能让运行停在半路)。
			name:   "LLM产出里声明suspend",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{{ID: "t1", Kind: NodeKindAgent,
					Agent: AgentSpec{Role: "w"}, Suspend: &SuspendSpec{}}},
				Edges: []EdgeSpec{{From: "planner", To: "t1"}},
			},
			wantSub: "声明了 suspend",
		},
		{
			name:   "子图内ID重复",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{vNode("t1"), vNode("t1")},
				Edges: []EdgeSpec{{From: "planner", To: "t1"}},
			},
			wantSub: "重复",
		},
		{
			name:   "子节点ID带保留分隔符",
			expand: &ExpandSpec{MaxNodes: 5},
			sub: &Expansion{
				Nodes: []NodeSpec{vNode("a#0")},
				Edges: []EdgeSpec{{From: "planner", To: "a#0"}},
			},
			wantSub: "保留分隔符",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := planSpec(tc.expand)
			spec.Policies = tc.policies
			st := newStub()
			sub := tc.sub
			st.fn["planner"] = func(int, NodeInput) NodeResult {
				return NodeResult{Status: NodeStatusCompleted, Output: "分解", Expansion: sub}
			}
			j := NewMemoryJournal()
			res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{})
			if err != nil {
				t.Fatalf("Run 出错: %v", err)
			}
			// 展开被拒不影响父节点自身终态 (它已经跑完了)
			if res.Nodes["planner"].Status != NodeStatusCompleted {
				t.Fatalf("父节点 = %+v, 期望展开被拒仍 completed", res.Nodes["planner"])
			}
			// 一个展开产物都不该进运行图 (整段拒绝, 不做部分接纳)
			if got := expandedIDs(res); len(got) != 0 {
				t.Fatalf("展开被拒后仍出现节点 %v", got)
			}
			if n := len(findEvents(t, j, EvGraphExpanded)); n != 0 {
				t.Fatalf("展开被拒却记了 %d 条 graph.expanded", n)
			}
			rej := findEvents(t, j, EvExpandRejected)
			if len(rej) != 1 {
				t.Fatalf("graph.expand_rejected 事件数 = %d, 期望 1 (拒绝必须留痕, 否则'模型给了子图但图没变大'无法解释)", len(rej))
			}
			reason, _ := rej[0].Data["reason"].(string)
			if !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("拒绝原因 = %q, 期望含 %q", reason, tc.wantSub)
			}
		})
	}
}

// 深度上限: 子节点想再展开一层, MaxDepth=1 时其 Expand 被清空 ⇒ 孙子图被拒。
func TestExpandDepthLimit(t *testing.T) {
	st := newStub()
	st.fn["planner"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "一层", Expansion: &Expansion{
			// 子节点自带 Expand 声明, 想再展开
			Nodes: []NodeSpec{{ID: "t1", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"},
				Expand: &ExpandSpec{MaxNodes: 5}}},
			Edges: []EdgeSpec{{From: "planner", To: "t1"}},
		}}
	}
	st.fn["planner/t1"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "二层", Expansion: &Expansion{
			Nodes: []NodeSpec{vNode("g1")},
			Edges: []EdgeSpec{{From: "t1", To: "g1"}},
		}}
	}
	j := NewMemoryJournal()
	res, err := fastEngine(st, j).Run(context.Background(),
		planSpec(&ExpandSpec{MaxNodes: 5, MaxDepth: 1}), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("planner/t1") != 1 {
		t.Fatalf("第一层展开未生效")
	}
	for _, n := range res.Graph.Nodes {
		if strings.Contains(n.ID, "g1") {
			t.Fatalf("MaxDepth=1 下出现了第二层展开产物 %s", n.ID)
		}
		if n.ID == "planner/t1" && n.Expand != nil {
			t.Fatalf("深度已用尽的子节点仍保留 Expand 授权: %+v", n.Expand)
		}
	}
	rej := findEvents(t, j, EvExpandRejected)
	if len(rej) != 1 || !strings.Contains(fmt.Sprint(rej[0].Data["reason"]), "未声明 Expand") {
		t.Fatalf("第二层展开应被拒 (授权已被收窄掉), 实得 %+v", rej)
	}
}

// 父节点带条件出边时整段拒绝 (条件针对父节点结果, 无法安全转嫁给 sink 子节点)。
func TestExpandRejectedWhenParentHasConditionalEdge(t *testing.T) {
	spec := planSpec(&ExpandSpec{MaxNodes: 5})
	spec.Edges = []EdgeSpec{{From: "planner", To: "sum", Condition: "score >= 75"}}
	st := newStub()
	st.fn["planner"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "分解", Score: 80, Expansion: twoSubtasks()}
	}
	j := NewMemoryJournal()
	res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if len(expandedIDs(res)) != 0 {
		t.Fatalf("父节点带条件出边时不应展开, 实得 %v", expandedIDs(res))
	}
	rej := findEvents(t, j, EvExpandRejected)
	if len(rej) != 1 || !strings.Contains(fmt.Sprint(rej[0].Data["reason"]), "带条件的出边") {
		t.Fatalf("拒绝原因 = %+v, 期望提到带条件的出边", rej)
	}
	// 条件路由本身照常生效
	if res.Nodes["sum"].Status != NodeStatusCompleted {
		t.Fatalf("sum = %+v, 期望 score=80 满足条件仍执行", res.Nodes["sum"])
	}
}

// ---------------------------------------------------------------------------
// 3. 单调收窄: 展开产物只能继承或收紧父节点约束
// ---------------------------------------------------------------------------

func TestExpandNarrowsConstraintsMonotonically(t *testing.T) {
	parent := NodeSpec{
		ID: "planner", Kind: NodeKindAgent,
		Agent:      AgentSpec{Role: "planner", ToolProfile: "readonly", MaxTurns: 5},
		Retry:      &RetryPolicy{MaxRetries: 2, BackoffSec: 1},
		Loop:       &LoopPolicy{MaxIterations: 3},
		TimeoutSec: 100,
		Expand:     &ExpandSpec{MaxNodes: 4, MaxDepth: 2},
	}
	spec := GraphSpec{Name: "narrow", Nodes: []NodeSpec{parent, vNode("sum")},
		Edges: []EdgeSpec{{From: "planner", To: "sum"}}}
	st := newStub()
	st.fn["planner"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "分解", Expansion: &Expansion{
			Nodes: []NodeSpec{
				{ // 处处想放宽
					ID: "greedy", Kind: NodeKindAgent,
					Agent:      AgentSpec{Role: "worker", ToolProfile: "danger-all", MaxTurns: 99},
					Retry:      &RetryPolicy{MaxRetries: 99},
					Loop:       &LoopPolicy{MaxIterations: 99},
					TimeoutSec: 99999,
					Expand:     &ExpandSpec{MaxNodes: 99, MaxDepth: 99},
				},
				{ // 什么都不声明 → 继承
					ID: "quiet", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"},
				},
			},
			Edges: []EdgeSpec{{From: "planner", To: "greedy"}, {From: "planner", To: "quiet"}},
		}}
	}
	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	byID := map[string]NodeSpec{}
	for _, n := range res.Graph.Nodes {
		byID[n.ID] = n
	}
	g, ok := byID["planner/greedy"]
	if !ok {
		t.Fatalf("展开产物缺失, 图 = %v", res.Graph.Nodes)
	}
	if g.TimeoutSec != 100 {
		t.Errorf("TimeoutSec = %d, 期望被夹到父节点的 100", g.TimeoutSec)
	}
	if g.Retry == nil || g.Retry.MaxRetries != 2 {
		t.Errorf("Retry = %+v, 期望被夹到父节点的 2", g.Retry)
	}
	if g.Loop == nil || g.Loop.MaxIterations != 3 {
		t.Errorf("Loop.MaxIterations = %+v, 期望被夹到父节点的 3", g.Loop)
	}
	if g.Agent.ToolProfile != "readonly" {
		t.Errorf("ToolProfile = %q, 期望强制继承父节点的 readonly (换画像=放宽权限)", g.Agent.ToolProfile)
	}
	if g.Agent.MaxTurns != 5 {
		t.Errorf("MaxTurns = %d, 期望夹到父节点的 5", g.Agent.MaxTurns)
	}
	if g.Expand == nil || g.Expand.MaxNodes != 4 || g.Expand.MaxDepth != 2 {
		t.Errorf("Expand = %+v, 期望 MaxNodes/MaxDepth 都被夹到父节点额度内", g.Expand)
	}
	q := byID["planner/quiet"]
	if q.TimeoutSec != 100 || q.Retry == nil || q.Retry.MaxRetries != 2 || q.Agent.ToolProfile != "readonly" {
		t.Errorf("未声明约束的子节点应继承父节点: %+v / retry=%+v", q, q.Retry)
	}
}

// ---------------------------------------------------------------------------
// 4. resume: 按 journal 重建, 不重新问 runner, 图与首跑一致, 不重复展开
// ---------------------------------------------------------------------------

func TestExpandReplayRebuildsSameGraphWithoutReExpanding(t *testing.T) {
	dir := t.TempDir()
	spec := planSpec(&ExpandSpec{MaxNodes: 5})

	// —— 首跑: planner 展开 t1/t2; t2 失败 → partial ——
	j1, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	st1 := newStub()
	plannerCalls := 0
	st1.fn["planner"] = func(int, NodeInput) NodeResult {
		plannerCalls++
		return NodeResult{Status: NodeStatusCompleted, Output: "分解完成", Expansion: twoSubtasks()}
	}
	st1.fn["planner/t2"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusFailed, Err: "子任务2第一趟失败"}
	}
	// sum 首跑也失败: 否则它在首跑就 completed 进了缓存, 续跑时不再执行,
	// 就验不到"恢复后重接线是否还在" (OR-join 下 t2 失败并不阻止 sum 执行)。
	st1.fn["sum"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusFailed, Err: "汇总第一趟失败"}
	}
	res1, err := fastEngine(st1, j1).Run(context.Background(), spec, RunOpts{RunID: "run-exp"})
	if err != nil {
		t.Fatalf("首跑出错: %v", err)
	}
	if res1.Status != RunStatusPartial {
		t.Fatalf("首跑 Status = %q, 期望 partial", res1.Status)
	}
	firstGraph := expandedIDs(res1)
	j1.Close()

	// —— 续跑: planner 若被重问会给出**完全不同的**子图, 用它反证"图来自 journal" ——
	j2, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer j2.Close()
	st2 := newStub()
	st2.fn["planner"] = func(int, NodeInput) NodeResult {
		t.Error("planner 已在 journal 里 completed, 不该被重新问 (否则恢复出的图与首跑不同)")
		return NodeResult{Status: NodeStatusCompleted, Output: "另一种分解", Expansion: &Expansion{
			Nodes: []NodeSpec{vNode("zzz")},
			Edges: []EdgeSpec{{From: "planner", To: "zzz"}},
		}}
	}
	res2, err := fastEngine(st2, j2).Run(context.Background(), spec, RunOpts{RunID: "run-exp", Resume: true})
	if err != nil {
		t.Fatalf("续跑出错: %v", err)
	}
	if res2.Status != RunStatusCompleted {
		t.Fatalf("续跑 Status = %q, 期望 completed, nodes=%+v", res2.Status, res2.Nodes)
	}
	// 图与首跑一致
	secondGraph := expandedIDs(res2)
	if len(secondGraph) != 2 || firstGraph[0] != secondGraph[0] || firstGraph[1] != secondGraph[1] {
		t.Fatalf("续跑的运行图 = %v, 首跑 = %v, 期望一致", secondGraph, firstGraph)
	}
	// 只有首跑那一条 graph.expanded
	if n := len(findEvents(t, j2, EvGraphExpanded)); n != 1 {
		t.Fatalf("graph.expanded 事件数 = %d, 期望 1 (resume 不得重复展开)", n)
	}
	// 已成功的 t1 走缓存, 失败的 t2 重跑
	if st2.callCount("planner/t1") != 0 {
		t.Fatalf("已成功的展开节点 t1 被重跑 %d 次", st2.callCount("planner/t1"))
	}
	if st2.callCount("planner/t2") != 1 {
		t.Fatalf("失败的展开节点 t2 执行 %d 次, 期望 1", st2.callCount("planner/t2"))
	}
	// 重接线在恢复后依然成立: sum 拿得到两个子任务产出
	in := st2.input("sum", 0)
	if in.PrevOutputs["planner/t1"] == "" || in.PrevOutputs["planner/t2"] == "" {
		t.Fatalf("恢复后 sum 的 PrevOutputs = %v, 期望含两个展开节点产出 (重接线丢了)", in.PrevOutputs)
	}
	if plannerCalls != 1 {
		t.Fatalf("planner 总共被问了 %d 次, 期望 1", plannerCalls)
	}
}

// 幂等: 同一父节点重跑 (上一轮 failed) 并再次返回子图时不得二次展开。
func TestExpandIdempotentAcrossResume(t *testing.T) {
	spec := planSpec(&ExpandSpec{MaxNodes: 5})
	j := NewMemoryJournal()
	// 手工构造: planner 展开过 t1, 但 planner 自己**失败**了 (故不在缓存里)
	sub := Expansion{
		Nodes: []NodeSpec{{ID: "planner/t1", Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}}},
		Edges: []EdgeSpec{{From: "planner", To: "planner/t1"}, {From: "planner/t1", To: "sum"}},
	}
	for _, ev := range []Event{
		{Type: EvRunCreated, RunID: "run-idem"},
		{Type: EvGraphExpanded, RunID: "run-idem", NodeID: "planner",
			Data: map[string]any{"parent": "planner", "depth": 1, "count": 1, "subgraph": mustJSON(sub)}},
		{Type: EvNodeFailed, RunID: "run-idem", NodeID: "planner", Data: map[string]any{"error": "崩了"}},
	} {
		if err := j.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	st := newStub()
	st.fn["planner"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "再分解", Expansion: &Expansion{
			Nodes: []NodeSpec{vNode("t9")},
			Edges: []EdgeSpec{{From: "planner", To: "t9"}},
		}}
	}
	res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{RunID: "run-idem", Resume: true})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("planner") != 1 {
		t.Fatalf("planner 上轮 failed, 本轮应重跑 1 次, 实得 %d", st.callCount("planner"))
	}
	got := expandedIDs(res)
	if len(got) != 1 || got[0] != "planner/t1" {
		t.Fatalf("展开产物 = %v, 期望只有 journal 重建的 planner/t1 (二次展开会让两代子图并存)", got)
	}
	rej := findEvents(t, j, EvExpandRejected)
	if len(rej) != 1 || !strings.Contains(fmt.Sprint(rej[0].Data["reason"]), "已展开过") {
		t.Fatalf("二次展开应被幂等闸拦下并留痕, 实得 %+v", rej)
	}
}
