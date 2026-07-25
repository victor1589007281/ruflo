package graph

// loop-group 行为回归 (design/01 §4.4 组级循环)。
// 重点断言: 组真的整轮迭代 (不是只跑一轮)、组内保持 DAG 语义 (并行/条件边/skipped)、
// 轮次进 journal 且 NodeID 可按轮归因、resume 不重跑已完成的轮次。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// advGroup 对抗模式的标准表达: gen(生成) → critic(评分门禁), 整组循环到评分达标。
func advGroup(maxIter int, feedback string) NodeSpec {
	return NodeSpec{
		ID: "adv", Kind: NodeKindLoopGroup, Agent: AgentSpec{Role: "coordinator"},
		Group: &GroupPolicy{
			Nodes: []NodeSpec{
				{ID: "gen", Kind: NodeKindAgent, Agent: AgentSpec{Role: "writer"}},
				{ID: "critic", Kind: NodeKindGate, Agent: AgentSpec{Role: "critic"}},
			},
			Edges:      []EdgeSpec{{From: "gen", To: "critic"}},
			Loop:       LoopPolicy{MaxIterations: maxIter, Until: "score >= 75", Feedback: feedback},
			ResultFrom: "critic",
		},
	}
}

// ---------------------------------------------------------------------------
// 1. 组整体迭代到 Until 满足; 每轮成员都真跑; 轮次可按 NodeID 归因
// ---------------------------------------------------------------------------

func TestLoopGroupIteratesWholeGroup(t *testing.T) {
	st := newStub()
	scores := []float64{50, 65, 80}
	// 组内成员的 NodeID 带轮次前缀, 但 runner 看到的 node.ID 仍是成员名 (同构)。
	st.fn["critic"] = func(call int, in NodeInput) NodeResult {
		if in.PrevOutputs["gen"] == "" {
			t.Errorf("第 %d 轮 critic 没收到 gen 的产出 (组内 DAG 语义丢了)", call)
		}
		return NodeResult{Status: NodeStatusCompleted, Output: fmt.Sprintf("评审%d", call), Score: scores[call]}
	}
	j := NewMemoryJournal()
	spec := GraphSpec{Name: "adversarial", Nodes: []NodeSpec{advGroup(5, "按评审改: {prev_output}")}}
	res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{Objective: "写点东西"})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, nodes=%+v", res.Status, res.Nodes)
	}
	if st.callCount("gen") != 3 || st.callCount("critic") != 3 {
		t.Fatalf("成员调用次数 gen=%d critic=%d, 期望各 3 轮 (第三轮 80 触发 Until)",
			st.callCount("gen"), st.callCount("critic"))
	}
	// 组结果 = 最后一轮 ResultFrom 的结果
	gr := res.Nodes["adv"]
	if gr.Status != NodeStatusCompleted || gr.Score != 80 || gr.Output != "评审2" {
		t.Fatalf("组结果 = %+v, 期望取最后一轮 critic (score=80)", gr)
	}
	// journal: 3 条 loop.group.iteration, 挂在组节点上
	its := findEvents(t, j, EvGroupIteration)
	if len(its) != 3 {
		t.Fatalf("loop.group.iteration 事件数 = %d, 期望 3", len(its))
	}
	for i, ev := range its {
		if ev.NodeID != "adv" || evInt(t, ev.Data, "iteration") != i {
			t.Fatalf("第 %d 条组轮次事件 = %+v, 期望 NodeID=adv iteration=%d", i, ev, i)
		}
	}
	// 成员事件按轮次独立归因: adv#it0/gen … adv#it2/critic
	got := map[string]bool{}
	for _, ev := range findEvents(t, j, EvNodeCompleted) {
		got[ev.NodeID] = true
	}
	for i := 0; i < 3; i++ {
		for _, m := range []string{"gen", "critic"} {
			want := fmt.Sprintf("adv%s%d%s%s", GroupIterIDInfix, i, GroupMemberIDSep, m)
			if !got[want] {
				t.Errorf("journal 缺成员事件 %s (无法按轮次归因), 实有: %v", want, keysOf(got))
			}
		}
	}
	// 节点级 loop.iteration 不该出现 —— 这是组级循环, 不是节点级
	if n := len(findEvents(t, j, EvLoopIteration)); n != 0 {
		t.Errorf("组级循环误记了 %d 条节点级 loop.iteration", n)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. MaxIterations 耗尽即停 (无界循环违法) + Feedback 回灌给全部成员
// ---------------------------------------------------------------------------

func TestLoopGroupMaxIterationsAndFeedback(t *testing.T) {
	st := newStub()
	st.fn["critic"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: fmt.Sprintf("差评%d", call), Score: 10} // 永不达标
	}
	spec := GraphSpec{Name: "adv2", Nodes: []NodeSpec{advGroup(2, "上轮评审: {prev_output}")}}
	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("gen") != 2 || st.callCount("critic") != 2 {
		t.Fatalf("gen=%d critic=%d, 期望 MaxIterations=2 耗尽即停", st.callCount("gen"), st.callCount("critic"))
	}
	if res.Nodes["adv"].Score != 10 {
		t.Fatalf("组结果 = %+v, 期望最后一轮 (score=10)", res.Nodes["adv"])
	}
	// 第 2 轮: 回灌上一轮 ResultFrom 的产出, **全部成员**都收到 + GroupIteration 递增
	for _, m := range []string{"gen", "critic"} {
		if in := st.input(m, 0); in.Feedback != "" || in.GroupIteration != 0 {
			t.Fatalf("%s 第 1 轮输入 = %+v, 期望无回灌 GroupIteration=0", m, in)
		}
		in := st.input(m, 1)
		if in.Feedback != "上轮评审: 差评0" {
			t.Fatalf("%s 第 2 轮 Feedback = %q, 期望回灌上一轮 critic 产出", m, in.Feedback)
		}
		if in.GroupIteration != 1 {
			t.Fatalf("%s 第 2 轮 GroupIteration = %d, 期望 1", m, in.GroupIteration)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. 组内保持完整 DAG 语义: 并行 + 条件边 + skipped 不算失败
// ---------------------------------------------------------------------------

func TestLoopGroupKeepsDAGSemanticsInside(t *testing.T) {
	// 组内: gate →(score>=75) pass / →(score<75) redo, ResultFrom=gate。
	// 首轮 60 走 redo 分支 (pass skipped), 次轮 90 走 pass (redo skipped)。
	group := NodeSpec{
		ID: "grp", Kind: NodeKindLoopGroup, Agent: AgentSpec{Role: "coordinator"},
		Group: &GroupPolicy{
			Nodes: []NodeSpec{
				{ID: "g", Kind: NodeKindGate, Agent: AgentSpec{Role: "gate"}},
				vNode("pass"), vNode("redo"),
			},
			Edges: []EdgeSpec{
				{From: "g", To: "pass", Condition: "score >= 75"},
				{From: "g", To: "redo", Condition: "score < 75"},
			},
			Loop:       LoopPolicy{MaxIterations: 3, Until: "score >= 75"},
			ResultFrom: "g",
		},
	}
	st := newStub()
	scores := []float64{60, 90}
	st.fn["g"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "门禁", Score: scores[call]}
	}
	j := NewMemoryJournal()
	res, err := fastEngine(st, j).Run(context.Background(),
		GraphSpec{Name: "grp-dag", Nodes: []NodeSpec{group}}, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, 期望 completed (组内 skipped 分支不该把整图判死)", res.Status)
	}
	if st.callCount("redo") != 1 || st.callCount("pass") != 1 {
		t.Fatalf("组内条件边未生效: redo=%d pass=%d, 期望各 1 (首轮 redo, 次轮 pass)",
			st.callCount("redo"), st.callCount("pass"))
	}
	if res.Nodes["grp"].Status != NodeStatusCompleted || res.Nodes["grp"].Score != 90 {
		t.Fatalf("组结果 = %+v, 期望 completed score=90", res.Nodes["grp"])
	}
	// 被路由掉的分支记 node.skipped, 且**不产生** node.failed
	skipped := map[string]bool{}
	for _, ev := range findEvents(t, j, EvNodeSkipped) {
		skipped[ev.NodeID] = true
	}
	if !skipped["grp"+GroupIterIDInfix+"0"+GroupMemberIDSep+"pass"] {
		t.Errorf("首轮 pass 应记 skipped, 实有: %v", keysOf(skipped))
	}
	if !skipped["grp"+GroupIterIDInfix+"1"+GroupMemberIDSep+"redo"] {
		t.Errorf("次轮 redo 应记 skipped, 实有: %v", keysOf(skipped))
	}
	if n := len(findEvents(t, j, EvNodeFailed)); n != 0 {
		t.Errorf("组内条件路由产生了 %d 条 node.failed (skipped 不是失败)", n)
	}
}

// 组内无依赖成员真并行 (复用同一个 ready-set 调度器)。
func TestLoopGroupMembersRunInParallel(t *testing.T) {
	group := NodeSpec{
		ID: "par", Kind: NodeKindLoopGroup, Agent: AgentSpec{Role: "coordinator"},
		Group: &GroupPolicy{
			Nodes: []NodeSpec{vNode("x"), vNode("y"), vNode("join")},
			Edges: []EdgeSpec{{From: "x", To: "join"}, {From: "y", To: "join"}},
			Loop:  LoopPolicy{MaxIterations: 1},
		},
	}
	st := newStub()
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
			return NodeResult{Status: NodeStatusFailed, Err: "组内成员未并发"}
		}
	}
	st.fn["x"] = func(int, NodeInput) NodeResult { return rendezvous("x") }
	st.fn["y"] = func(int, NodeInput) NodeResult { return rendezvous("y") }
	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(),
		GraphSpec{Name: "grp-par", Nodes: []NodeSpec{group}}, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q (组内成员可能未并发)", res.Status)
	}
	if in := st.input("join", 0); in.PrevOutputs["x"] != "x-out" || in.PrevOutputs["y"] != "y-out" {
		t.Fatalf("join 的 PrevOutputs = %v, 期望组内两个上游产出", in.PrevOutputs)
	}
}

// ---------------------------------------------------------------------------
// 4. 组与外层的接线: 上游产出下发进组, 组产出流向下游
// ---------------------------------------------------------------------------

func TestLoopGroupWiredWithOuterGraph(t *testing.T) {
	st := newStub()
	st.fn["critic"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "评审通过", Score: 90}
	}
	spec := GraphSpec{
		Name:  "outer",
		Nodes: []NodeSpec{vNode("plan"), advGroup(3, ""), vNode("ship")},
		Edges: []EdgeSpec{{From: "plan", To: "adv"}, {From: "adv", To: "ship"}},
	}
	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, nodes=%+v", res.Status, res.Nodes)
	}
	if in := st.input("gen", 0); in.PrevOutputs["plan"] != "plan-out" {
		t.Fatalf("组内成员未收到组的上游产出: %v", in.PrevOutputs)
	}
	if in := st.input("ship", 0); in.PrevOutputs["adv"] != "评审通过" {
		t.Fatalf("下游未收到组产出: %v, 期望 {adv: 评审通过}", in.PrevOutputs)
	}
}

// ---------------------------------------------------------------------------
// 5. resume: 已完成的轮次不重跑
// ---------------------------------------------------------------------------

func TestLoopGroupResumeSkipsFinishedIterations(t *testing.T) {
	spec := GraphSpec{Name: "adv-resume", Nodes: []NodeSpec{advGroup(4, "改: {prev_output}")}}
	j := NewMemoryJournal()
	// 手工构造"跑完 2 轮后被 kill"的 journal (两轮都没达标)。
	for _, ev := range []Event{
		{Type: EvRunCreated, RunID: "run-grp"},
		{Type: EvGroupIteration, RunID: "run-grp", NodeID: "adv",
			Data: map[string]any{"iteration": 0, "status": NodeStatusCompleted, "output": "一轮稿", "score": 40.0}},
		{Type: EvGroupIteration, RunID: "run-grp", NodeID: "adv",
			Data: map[string]any{"iteration": 1, "status": NodeStatusCompleted, "output": "二轮稿", "score": 60.0}},
	} {
		if err := j.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	st := newStub()
	st.fn["critic"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "三轮评审", Score: 88}
	}
	res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{RunID: "run-grp", Resume: true})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("gen") != 1 || st.callCount("critic") != 1 {
		t.Fatalf("gen=%d critic=%d, 期望只跑第 3 轮 (前两轮已在 journal 里)",
			st.callCount("gen"), st.callCount("critic"))
	}
	// 续跑的这一轮拿到 journal 里最后一轮的产出做回灌
	if in := st.input("gen", 0); in.Feedback != "改: 二轮稿" || in.GroupIteration != 2 {
		t.Fatalf("续跑第一轮输入 = %+v, 期望 Feedback=改: 二轮稿 GroupIteration=2", in)
	}
	if res.Nodes["adv"].Score != 88 {
		t.Fatalf("组结果 = %+v, 期望第 3 轮的 88", res.Nodes["adv"])
	}
	if n := len(findEvents(t, j, EvGroupIteration)); n != 3 {
		t.Fatalf("loop.group.iteration 总数 = %d, 期望 3 (前 2 条 + 新 1 条)", n)
	}
}

// journal 里最后一轮已达标: 组一轮都不跑 (resume 不该白烧一轮 LLM)。
func TestLoopGroupResumeUntilAlreadyMet(t *testing.T) {
	spec := GraphSpec{Name: "adv-met", Nodes: []NodeSpec{advGroup(4, "")}}
	j := NewMemoryJournal()
	for _, ev := range []Event{
		{Type: EvRunCreated, RunID: "run-met"},
		{Type: EvGroupIteration, RunID: "run-met", NodeID: "adv",
			Data: map[string]any{"iteration": 0, "status": NodeStatusCompleted, "output": "达标稿", "score": 90.0}},
	} {
		if err := j.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	st := newStub()
	res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{RunID: "run-met", Resume: true})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if st.callCount("gen") != 0 || st.callCount("critic") != 0 {
		t.Fatalf("已达标仍跑了组内成员: gen=%d critic=%d", st.callCount("gen"), st.callCount("critic"))
	}
	if r := res.Nodes["adv"]; r.Output != "达标稿" || r.Score != 90 {
		t.Fatalf("组结果 = %+v, 期望直接取 journal 的达标轮", r)
	}
}

// ---------------------------------------------------------------------------
// 6. hook 载荷: 组节点带轮次, 成员带 group / group_iteration
// ---------------------------------------------------------------------------

func TestLoopGroupHookPayloads(t *testing.T) {
	st := newStub()
	st.fn["critic"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "ok", Score: 90}
	}
	bus := &recordBus{}
	e := fastEngine(st, NewMemoryJournal())
	e.Hooks = bus
	if _, err := e.Run(context.Background(),
		GraphSpec{Name: "grp-hooks", Nodes: []NodeSpec{advGroup(2, "")}}, RunOpts{}); err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	post, ok := bus.find(ScopeNode, "post", "adv")
	if !ok {
		t.Fatal("缺组节点 node post 事件")
	}
	if post.Payload["group_iterations"] != 1 || post.Payload["result_from"] != "critic" {
		t.Errorf("组 post 载荷 = %v, 期望 group_iterations=1 result_from=critic", post.Payload)
	}
	if post.Payload["kind"] != string(NodeKindLoopGroup) {
		t.Errorf("组 post 载荷 kind = %v", post.Payload["kind"])
	}
	memberID := "adv" + GroupIterIDInfix + "0" + GroupMemberIDSep + "gen"
	pre, ok := bus.find(ScopeNode, "pre", memberID)
	if !ok {
		t.Fatalf("缺成员 %s 的 node pre 事件 (dashboard 看不到组内进度)", memberID)
	}
	if pre.Payload["group"] != "adv" || pre.Payload["group_iteration"] != 0 {
		t.Errorf("成员 pre 载荷 = %v, 期望 group=adv group_iteration=0", pre.Payload)
	}
}

// ---------------------------------------------------------------------------
// 7. 语义边界: 节点级 Loop 与组级 loop-group 不得并存 / 组内不得再套组
// ---------------------------------------------------------------------------

func TestLoopGroupSemanticBoundaries(t *testing.T) {
	base := advGroup(2, "")
	// (a) 组节点再挂节点级 Loop
	withLoop := base
	withLoop.Loop = &LoopPolicy{MaxIterations: 3}
	if err := (GraphSpec{Name: "x", Nodes: []NodeSpec{withLoop}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "不得再声明节点级 Loop") {
		t.Fatalf("组节点+节点级 Loop 应被拒绝, 实得 %v", err)
	}
	// (b) 组内再嵌套 loop-group
	nested := base
	g := *base.Group
	g.Nodes = append([]NodeSpec{advGroup(2, "")}, g.Nodes...)
	g.ResultFrom = "critic"
	nested.Group = &g
	if err := (GraphSpec{Name: "x", Nodes: []NodeSpec{nested}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "不允许再嵌套 loop-group") {
		t.Fatalf("组内嵌套 loop-group 应被拒绝, 实得 %v", err)
	}
	// (c) 组内多个出度 0 节点却未声明 ResultFrom
	ambiguous := base
	g2 := *base.Group
	g2.ResultFrom = ""
	g2.Edges = nil // gen 与 critic 都成了 sink
	ambiguous.Group = &g2
	if err := (GraphSpec{Name: "x", Nodes: []NodeSpec{ambiguous}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "result_from") {
		t.Fatalf("组产出歧义应被拒绝, 实得 %v", err)
	}
	// (d) 单 sink 时可省略 ResultFrom
	implicit := base
	g3 := *base.Group
	g3.ResultFrom = ""
	implicit.Group = &g3
	if err := (GraphSpec{Name: "x", Nodes: []NodeSpec{implicit}}).Validate(); err != nil {
		t.Fatalf("单 sink 组应允许省略 result_from, 实得 %v", err)
	}
}
