package agent

// 逐节点放置的接线回归 (design/01 §4.9)。
//
// 断言这条链一节不断:
//
//	覆盖表 StageGraphOverride.Placement
//	  → TranslateWorkflow → graph.AgentSpec.Placement
//	    → stageNodeRunner.RunNode → NodeExecHints.Placement (ctx)
//	      → pkg/worker 的 runtimeRunner.Placement() (那一段在 pkg/worker 里断言)
//
// 中间任何一节漏掉, 症状都是"声明写了但没生效"—— 而放置是 fail-closed 的治理项,
// 没生效意味着一个声明了 browser 的节点被派到没有浏览器的机器上跑出假产出。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

func TestPlacement_覆盖表声明直译进图(t *testing.T) {
	const name = "graph-placement-xlate"
	wf := &WorkflowDef{
		Name: name, Mode: "graph",
		Stages: []StageDef{
			{Name: "plan", Role: "planner", Prompt: "规划 {objective}"},
			{Name: "render", Role: "designer", Prompt: "渲染 {prev_result}", DependsOn: []string{"plan"}},
		},
	}
	want := &graph.PlacementSpec{Require: []string{"browser"}, Prefer: "remote:render-pool"}
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"render": {Placement: want},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	byID := map[string]graph.NodeSpec{}
	for _, n := range spec.Nodes {
		byID[n.ID] = n
	}
	got := byID["render"].Agent.Placement
	if got == nil {
		t.Fatal("覆盖表声明的 placement 没进 GraphSpec")
	}
	if len(got.Require) != 1 || got.Require[0] != "browser" || got.Prefer != want.Prefer {
		t.Errorf("placement = %+v, 期望 %+v", got, want)
	}
	// 未声明的阶段必须保持 nil (未声明 = 走进程级默认, 行为与改造前一致)。
	if byID["plan"].Agent.Placement != nil {
		t.Errorf("未声明放置的阶段被塞了默认值: %+v", byID["plan"].Agent.Placement)
	}
	// 图必须仍然合法 (Validate 会校验 placement 形状)。
	if err := spec.Validate(); err != nil {
		t.Fatalf("带 placement 的图开图失败: %v", err)
	}
}

// 覆盖表里的非法 placement 必须在开图时被拒 (而不是跑起来才发现没钉住)。
func TestPlacement_非法声明开图即拒(t *testing.T) {
	const name = "graph-placement-bad"
	wf := &WorkflowDef{Name: name, Mode: "graph",
		Stages: []StageDef{{Name: "render", Role: "designer", Prompt: "x"}}}
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"render": {Placement: &graph.PlacementSpec{Prefer: "remote:"}},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	// 直译器自己就会跑一遍 Validate: 非法放置在这里就被拦下, 不会跑到调度器。
	if _, err := TranslateWorkflow(wf); err == nil {
		t.Error("prefer=\"remote:\" (缺 runtime 名) 应在直译期就被拒")
	} else if !strings.Contains(err.Error(), "缺少 runtime 名") {
		t.Errorf("报错应点名问题所在: %v", err)
	}
}

// PlacementFromSpec 是镜像结构体的唯一转换点: 字段一一对应且不共享底层数组。
func TestPlacementFromSpec_镜像转换(t *testing.T) {
	if PlacementFromSpec(nil) != nil {
		t.Error("nil 应转成 nil (未声明 = 没有这个约束)")
	}
	src := &graph.PlacementSpec{Require: []string{"browser", "gpu"},
		Prefer: " remote:pool ", Affinity: "team", AffinityKey: "t1"}
	got := PlacementFromSpec(src)
	if got.Prefer != "remote:pool" || got.Affinity != "team" || got.AffinityKey != "t1" {
		t.Errorf("字段未一一搬过来: %+v", got)
	}
	if len(got.Require) != 2 || got.Require[0] != "browser" {
		t.Errorf("Require 未搬过来: %+v", got.Require)
	}
	// 值拷贝: 下游 (worker 侧的合并) 改了副本不该污染图规格。
	got.Require[0] = "被改了"
	if src.Require[0] != "browser" {
		t.Error("Require 与图规格共享了底层数组")
	}
}

// 节点声明必须一路进到宿主 factory 看到的 ctx —— 生产 factory
// (feishu.SessionManager.CreateAgentRunner / worker.RuntimeFactory) 是在**创建时**
// 从 ctx 读声明的, 少这一步等于放置声明在图层之后就消失了。
func TestPlacement_节点声明下传给factory(t *testing.T) {
	const name = "graph-placement-hints"
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"render": {Placement: &graph.PlacementSpec{Require: []string{"browser"}, Prefer: "remote:render-pool"}},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "plan", Role: "planner", Prompt: "规划 {objective}"},
		{Name: "render", Role: "designer", Prompt: "渲染 {prev_result}", DependsOn: []string{"plan"}},
	}}
	var mu sync.Mutex
	seen := map[string]*Placement{}
	we := &WorkflowExecutor{
		notify: func(_, _ string) {},
		factory: func(ctx context.Context, role, _ string) (AgentRunner, error) {
			h := NodeExecHintsFromContext(ctx)
			mu.Lock()
			seen[h.Node] = h.Placement
			mu.Unlock()
			return &progStageRunner{role: role, fn: func(_ context.Context, r, _ string) (string, error) {
				return stubStageOutput(r), nil
			}}, nil
		},
	}
	if _, err := we.executeGraph(context.Background(), wf, "测试目标", newStubTeam(t, "placement")); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	got := seen["render"]
	if got == nil {
		t.Fatal("render 节点的放置声明没进 ctx")
	}
	if len(got.Require) != 1 || got.Require[0] != "browser" || got.Prefer != "remote:render-pool" {
		t.Errorf("factory 看到的 placement = %+v", got)
	}
	// 未声明的节点必须是 nil, 否则宿主分不清"没声明"与"声明了空约束"。
	if p, ok := seen["plan"]; !ok || p != nil {
		t.Errorf("未声明放置的节点应给 nil, 实得 %+v (present=%v)", p, ok)
	}
}

// ---------------------------------------------------------------------------
// 组级终止器: 五个 mode 的 AdaptiveTerminator 在图上的表达 (design/01 §4.4)
// ---------------------------------------------------------------------------

// 真跑一遍 loop-group + adaptive 终止器: 轮数由**判据**决定而不是由 MaxIterations 决定。
//
// 这条路径是 creative_media / app_composite / game_composite / novel_writing /
// swarm_novel 五个 mode 折进图之后的形态 (它们现在都是手写 for 循环 + 手调
// terminator.ShouldTerminate)。
//
// 诚实说明本测试的边界: 这里的组产出节点是普通 agent (不是 gate), 所以 Score 恒 0,
// 触发的是"相邻两轮分差 0 < ε ⇒ 收敛"这一路。**分数真进判据**这件事由
// pkg/graph/terminator_test.go 断言 (那里能直接摆布 NodeResult.Score);
// 本测试要锁死的是接线: 覆盖表里的 terminator 声明真的改变了组的轮数。
// 对照组 (不声明 terminator) 必须跑满 MaxIterations —— 否则"早停"可能只是别的原因。
func TestGroupTerminator_轮数由判据决定(t *testing.T) {
	run := func(t *testing.T, name string, term *graph.TerminatorSpec) int {
		t.Helper()
		wf := &WorkflowDef{
			Name: name, Mode: "graph",
			Stages: []StageDef{
				{Name: "gen", Role: "writer", Prompt: "写稿"},
				{Name: "check", Role: "critic", Prompt: "审 {prev_result}", DependsOn: []string{"gen"}},
			},
		}
		RegisterGraphOverride(name, WorkflowGraphOverride{Groups: []StageGroupOverride{{
			ID: "adv", Members: []string{"gen", "check"}, MaxIterations: 4, ResultFrom: "check",
			Feedback: "按上轮意见改: {prev_output}", Terminator: term,
		}}})
		t.Cleanup(func() { UnregisterGraphOverride(name) })

		var mu sync.Mutex
		rounds := 0
		we := newProgExecutor(func(ctx context.Context, role, _ string) (string, error) {
			if NodeExecHintsFromContext(ctx).Node == "check" {
				mu.Lock()
				rounds++
				mu.Unlock()
			}
			return stubStageOutput(role), nil
		})
		if _, err := we.executeGraph(context.Background(), wf, "写点东西", newStubTeam(t, name)); err != nil {
			t.Fatalf("图执行失败: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return rounds
	}

	// 对照组: 无终止器无 Until ⇒ 跑满 MaxIterations。
	if got := run(t, "graph-group-noterm", nil); got != 4 {
		t.Fatalf("无终止器时组轮数 = %d, 期望跑满 4 轮", got)
	}
	// 实验组: adaptive 判据在第 2 轮判定收敛 ⇒ 早停。
	got := run(t, "graph-group-term", &graph.TerminatorSpec{
		Name: graph.TerminatorAdaptive, PassScore: 90, ConvergeEpsilon: 2,
		DegradeDelta: 3, DegradeRounds: 2,
	})
	if got != 2 {
		t.Fatalf("声明终止器后组轮数 = %d, 期望 2 (收敛早停)", got)
	}
}

// 覆盖表里的组级终止器声明必须真的落进 GroupPolicy.Loop, 且与 Until 并存时开图即拒。
func TestGroupTerminator_声明落图与互斥(t *testing.T) {
	const name = "graph-group-term-xlate"
	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "gen", Role: "writer", Prompt: "写稿"},
		{Name: "check", Role: "critic", Prompt: "审", DependsOn: []string{"gen"}},
	}}
	term := &graph.TerminatorSpec{Name: graph.TerminatorAdaptive, PassScore: 90,
		ConvergeEpsilon: 2, DegradeDelta: 3, DegradeRounds: 2, AllowRollback: true}
	RegisterGraphOverride(name, WorkflowGraphOverride{Groups: []StageGroupOverride{{
		ID: "adv", Members: []string{"gen", "check"}, MaxIterations: 4,
		ResultFrom: "check", Terminator: term,
	}}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	for _, n := range spec.Nodes {
		if n.ID != "adv" {
			continue
		}
		got := n.Group.Loop.Terminator
		if got == nil || got.Name != graph.TerminatorAdaptive || !got.AllowRollback {
			t.Fatalf("组级终止器声明没进 GroupPolicy.Loop: %+v", got)
		}
		return
	}
	t.Fatal("组节点 adv 不存在")
}

// Until 与终止器并存: 直译期 (它内部会 Validate) 就该被拒。
func TestGroupTerminator_与Until并存开图即拒(t *testing.T) {
	const name = "graph-group-term-conflict"
	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "gen", Role: "writer", Prompt: "写稿"},
		{Name: "check", Role: "critic", Prompt: "审", DependsOn: []string{"gen"}},
	}}
	RegisterGraphOverride(name, WorkflowGraphOverride{Groups: []StageGroupOverride{{
		ID: "adv", Members: []string{"gen", "check"}, MaxIterations: 4, ResultFrom: "check",
		Until: "ok", Terminator: &graph.TerminatorSpec{Name: graph.TerminatorAdaptive,
			PassScore: 90, ConvergeEpsilon: 2, DegradeDelta: 3, DegradeRounds: 2},
	}}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	if _, err := TranslateWorkflow(wf); err == nil || !strings.Contains(err.Error(), "二选一") {
		t.Errorf("until 与 terminator 并存应在直译期被拒, 实得 %v", err)
	}
}
