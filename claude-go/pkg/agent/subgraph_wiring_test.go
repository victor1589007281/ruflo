package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// 本文件是 21.8.12 P1/D4 的接线回归测试。
//
// 改造前的事实: pkg/graph 里 NodeKindSubgraph 的引擎实现、校验、命名空间、挂起传播
// 全都齐备, RegisterGraph/UnregisterGraph/LookupGraph 也都在, 但 **RegisterGraph
// 全仓零调用**, 且直译器的 kind 白名单里没有 subgraph。于是"子流程复用"在生产上
// 完全不可达 —— 组件都在, 只差一根线。这正是本仓库反复出现的
// "声明了但没人读"里最隐蔽的一种: 没有任何一处会报错, 因为你没办法表达"我要引用它"。

// subgraphFixture 造一个两级工作流: 被引用的基础图 + 引用它的父图。
func subgraphFixture(t *testing.T, parent, child string) {
	t.Helper()
	roles := NewRoleRegistry(t.TempDir())

	base := &WorkflowDef{
		Name: child,
		Mode: "graph",
		Stages: []StageDef{
			{Name: "child-step", Agent: "coder", Prompt: "做一件事: {objective}"},
		},
	}
	if err := RegisterWorkflow(base, roles); err != nil {
		t.Fatalf("注册被引用的图 %q: %v", child, err)
	}

	parentDef := &WorkflowDef{
		Name: parent,
		Mode: "graph",
		Stages: []StageDef{
			{Name: "produce", Agent: "coder", Prompt: "先准备: {objective}"},
			// 角色留空是**刻意的**: subgraph 节点的产出由被引用的图给, 它自己不需要
			// 角色。Validate 只要求"角色能解析 **或** 有内联 prompt", 但 RoleDef 的
			// 取法 (roles.Get) 对空串返回 nil —— 所以要给一句内联 prompt 占位。
			// 这条提示词在 subgraph 节点上不会被消费 (拿它当 prompt 会跑一次多余的
			// LLM 调用), 但去掉它会让整个声明期校验失败。
			{Name: "reuse", DependsOn: []string{"produce"}, Prompt: "本阶段由被引用的子图产出"},
		},
	}
	if err := RegisterWorkflow(parentDef, roles); err != nil {
		t.Fatalf("注册父图 %q: %v", parent, err)
	}
	// subgraph 声明走覆盖表 (与 map/loop/condition 同一入口)。
	RegisterGraphOverride(parent, WorkflowGraphOverride{
		Stages: map[string]StageGraphOverride{
			"reuse": {Kind: string(graph.NodeKindSubgraph), Subgraph: &graph.SubgraphSpec{Graph: child, ResultFrom: "child-step"}},
		},
	})
}

func cleanupGraphs(t *testing.T, names ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, n := range names {
			UnregisterWorkflow(n)
			UnregisterGraphOverride(n)
		}
	})
}

// TestRegisterWorkflowRegistersGraph 断言注册动态工作流时它**同时**进了图注册表。
//
// 这是 D4 的核心接线: 断了它, subgraph 节点引用任何名字都会解析失败,
// 而"引用失败"看起来与"那张图本来就不存在"一模一样。
func TestRegisterWorkflowRegistersGraph(t *testing.T) {
	const child = "sg-child-reg"
	cleanupGraphs(t, child)
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{
		Name:   child,
		Mode:   "graph",
		Stages: []StageDef{{Name: "s1", Agent: "coder", Prompt: "x {objective}"}},
	}
	if err := RegisterWorkflow(def, roles); err != nil {
		t.Fatal(err)
	}
	spec, ok := graph.LookupGraph(child)
	if !ok {
		t.Fatal("RegisterWorkflow 之后图注册表里应有它 (缺这步 ⇒ subgraph 引用永远解析失败, 而没有任何报错)")
	}
	if spec.Name != child {
		t.Fatalf("注册名应与工作流名一致 (引用键就是它): %q", spec.Name)
	}
	if len(spec.Nodes) != 1 || spec.Nodes[0].ID != "s1" {
		t.Fatalf("注册的图应是本工作流的直译结果: %+v", spec.Nodes)
	}
}

// TestRegisterWorkflowGraphIsIdempotentOnReregister 断言**改写一个工作流时, 它注册的图也跟着换新**。
//
// pkg/graph.RegisterGraph 的既有口径是"重名一律拒绝"(因为一张被引用的图被静默换掉,
// 等于让全部引用它的图在不改一个字的情况下换行为)。工作流这一侧改写时要换图 ——
// 两者不冲突的前提是改写路径里有一部显式的注销。
//
// 没有那一部时: 改写同名工作流, 工作流本身成功、图注册**每抛一次错只进日志**,
// 于是注册表里留着的是**上一版**的图。引用它的节点会跑一份没人记得的旧定义,
// 而这件事不会有任何用户可见的信号。
//
// ★ 本用例在 21.8.12 P3 之前是直接调两次 RegisterWorkflow —— 那正是当时唯一的改写方式,
// 也就是"同名 re-POST 静默覆盖"那个洞本身。守卫装上后那条路改成报错, 这里于是改走
// UpdateWorkflow。**两条断言都要留着**: 一条保护"图会跟着换"这个性质不被回退,
// 另一条保护"旧的那条静默覆盖的路真的被关掉了"——只留前者的话, 把守卫拆掉用例照样全绿,
// 而那正是本仓 [[redundant-defenses-mask-each-other-in-mutation]] 记过的形态。
func TestRegisterWorkflowGraphIsIdempotentOnReregister(t *testing.T) {
	const child = "sg-child-re"
	cleanupGraphs(t, child)
	roles := NewRoleRegistry(t.TempDir())
	mk := func(prompt string) *WorkflowDef {
		return &WorkflowDef{Name: child, Mode: "graph",
			Stages: []StageDef{{Name: "s1", Agent: "coder", Prompt: prompt}}}
	}
	if err := RegisterWorkflow(mk("第一版 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	// 守卫: 同名再走创建口必须被拒, 而不是静默覆盖。
	if err := RegisterWorkflow(mk("走错门的第二版 {objective}"), roles); err == nil {
		t.Fatal("同名再注册必须被拒 (这条查的就是守卫本身); 静默覆盖会让被覆盖的那一版不可撤销")
	} else if !errors.Is(err, ErrWorkflowRevisionConflict) {
		t.Fatalf("重名应报版本冲突 (dashboard 侧映射成 409), got %v", err)
	}
	// 走错门那次必须**一点痕迹都不留**。
	if got := getCustomWorkflow(child); got == nil || got.Stages[0].Prompt != "第一版 {objective}" {
		t.Fatalf("被拒的提交不得改动注册表, got %+v", got)
	}
	// 正确的改写路径: 带上读到的版本。
	if _, err := UpdateWorkflow(mk("第二版 {objective}"), roles, RevisionOf(child)); err != nil {
		t.Fatalf("带版本的改写应成功 (注销那一步没做的话图注册会一直失败): %v", err)
	}
	spec, ok := graph.LookupGraph(child)
	if !ok {
		t.Fatal("改写后图应仍在注册表里")
	}
	if got := spec.Nodes[0].Agent.Prompt; got != "第二版 {objective}" {
		t.Fatalf("注册表里应是最新版, got %q —— 留着旧版会让引用方跑一份没人记得的定义, 且无任何信号", got)
	}
}

// TestUnregisterWorkflowRemovesItsGraph 断言删除工作流会连同它的图一起摘掉。
// 留着的话, 一个"已删除"的工作流仍能被子图节点引用到。
func TestUnregisterWorkflowRemovesItsGraph(t *testing.T) {
	const child = "sg-child-un"
	cleanupGraphs(t, child)
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{Name: child, Mode: "graph",
		Stages: []StageDef{{Name: "s1", Agent: "coder", Prompt: "x {objective}"}}}
	if err := RegisterWorkflow(def, roles); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.LookupGraph(child); !ok {
		t.Fatal("前置条件不成立: 应已注册成图")
	}
	if err := UnregisterWorkflow(child); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.LookupGraph(child); ok {
		t.Fatal("删除工作流后它的图应一并摘掉 (否则已删除的工作流仍可被 subgraph 引用)")
	}
}

// TestTranslateSubgraphNode 断言 kind=subgraph 的声明真的直译成一个 subgraph 节点。
//
// 改造前这条**不可能通过**: 直译器的 kind 白名单里没有 subgraph, 声明会被判非法。
func TestTranslateSubgraphNode(t *testing.T) {
	const parent, child = "sg-parent-tr", "sg-child-tr"
	subgraphFixture(t, parent, child)
	cleanupGraphs(t, parent, child)

	wf := getCustomWorkflow(parent)
	if wf == nil {
		t.Fatal("父图未注册")
	}
	spec, err := graphSpecForWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	var found *graph.NodeSpec
	for i := range spec.Nodes {
		if spec.Nodes[i].ID == "reuse" {
			found = &spec.Nodes[i]
		}
	}
	if found == nil {
		t.Fatal("未找到 reuse 节点")
	}
	if found.Kind != graph.NodeKindSubgraph {
		t.Fatalf("reuse 应是 subgraph 节点, got %q (白名单里没有 subgraph 时这里会是 agent)", found.Kind)
	}
	if found.Subgraph == nil || found.Subgraph.Graph != child {
		t.Fatalf("引用的图名不对: %+v", found.Subgraph)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("带 subgraph 的图应通过校验: %v", err)
	}
}

// TestSubgraphKindRequiresPolicy 断言声明 kind=subgraph 却不给图名时报错。
// 与 map 同一口径: 猜错的后果是"跑了一张完全不相干的图", 而它不会报错。
func TestSubgraphKindRequiresPolicy(t *testing.T) {
	const parent = "sg-parent-bad"
	cleanupGraphs(t, parent)
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{Name: parent, Mode: "graph",
		Stages: []StageDef{{Name: "s1", Agent: "coder", Prompt: "x {objective}"}}}
	if err := RegisterWorkflow(def, roles); err != nil {
		t.Fatal(err)
	}
	RegisterGraphOverride(parent, WorkflowGraphOverride{
		Stages: map[string]StageGraphOverride{
			"s1": {Kind: string(graph.NodeKindSubgraph)}, // 缺 Subgraph 策略
		},
	})
	if _, err := graphSpecForWorkflow(getCustomWorkflow(parent)); err == nil {
		t.Fatal("kind=subgraph 但没有 graph 图名应报错")
	}
}

// TestSubgraphUnknownGraphFailsAtValidate 断言引用一张**没注册**的图在开图时失败。
//
// 这是"注册这一步真的接上了"的反向判据: 如果 RegisterGraph 从没被调用过,
// 那么引用任何名字都会走这条错误分支 —— 上面那条正向测试与这条必须同时成立,
// 才说明"能引用到的"确实是注册进去的那张图, 而不是碰巧没报错。
func TestSubgraphUnknownGraphFailsAtValidate(t *testing.T) {
	const parent = "sg-parent-unknown"
	cleanupGraphs(t, parent)
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{Name: parent, Mode: "graph",
		Stages: []StageDef{{Name: "s1", Agent: "coder", Prompt: "x {objective}"}}}
	if err := RegisterWorkflow(def, roles); err != nil {
		t.Fatal(err)
	}
	RegisterGraphOverride(parent, WorkflowGraphOverride{
		Stages: map[string]StageGraphOverride{
			"s1": {Kind: string(graph.NodeKindSubgraph),
				Subgraph: &graph.SubgraphSpec{Graph: "这个图根本不存在"}},
		},
	})
	spec, err := graphSpecForWorkflow(getCustomWorkflow(parent))
	if err != nil {
		return // 直译期就拒了也可以, 只要不是静默通过
	}
	if err := spec.Validate(); err == nil {
		t.Fatal("引用一张未注册的图必须在开图时失败")
	}
}

// TestSubgraphErrorListsRegisteredCandidates 是 21.8.12 P1/D4 验收的**负 5**。
//
// 原话: "P1 之后, 写一个 subgraph 节点不再报'已注册的有：（空列表）'"。
//
// ---------------------------------------------------------------------------
// 为什么"报错"与"报错里有没有候选"是两件事
// ---------------------------------------------------------------------------
//
// 上面那条 TestSubgraphUnknownGraphFailsAtValidate 只断言 `err != nil` —— 而在
// 接线之前, 引一个**存在的**名字同样会 `err != nil`, 报的还就是这条错误, 只不过
// 末尾是"(已注册: )" (空列表)。两句错误文案在 `!= nil` 这个断言下完全同形。
//
// 所以判据必须落在**错误文本里有没有候选**上: 有一串候选, 说明注册表真的被填过
// (接线通了); 空列表说明这张图注册不进去或者压根没注册过。候选人一眼能看出
// "我要引的那个是在, 还是名字打错了" —— 这正是这条错误消息存在的意义。
func TestSubgraphErrorListsRegisteredCandidates(t *testing.T) {
	const parent, child = "sg-parent-cand", "sg-child-cand"
	subgraphFixture(t, parent, child) // 注册了 child, 并把 parent 的 reuse 指向它
	cleanupGraphs(t, parent, child)

	// 把 reuse 指向一个**不存在的**名字, 制造那条错误。
	RegisterGraphOverride(parent, WorkflowGraphOverride{
		Stages: map[string]StageGraphOverride{
			"reuse": {Kind: string(graph.NodeKindSubgraph),
				Subgraph: &graph.SubgraphSpec{Graph: "sg-名字打错了"}},
		},
	})
	spec, err := graphSpecForWorkflow(getCustomWorkflow(parent))
	if err == nil {
		err = spec.Validate()
	}
	if err == nil {
		t.Fatal("引用一张不存在的图必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "已注册") {
		t.Fatalf("错误里必须带上已注册的候选 (没有候选的话, '名字打错了'与'注册根本没接线'分不开): %v", msg)
	}
	if !strings.Contains(msg, child) {
		t.Fatalf("候选清单里必须有 %q —— 它是刚刚注册进去的, 也在错误里被点名说明注册表是活的: %v", child, msg)
	}
	// 空列表的形态 (接线断掉时会出现的那个) 必须被这条用例挡下。
	if strings.Contains(msg, "已注册: )") || strings.Contains(msg, "已注册: \n") {
		t.Fatalf("候选清单是空的 ⇒ 注册表没被填 (这正是改造前的形态): %v", msg)
	}
}
