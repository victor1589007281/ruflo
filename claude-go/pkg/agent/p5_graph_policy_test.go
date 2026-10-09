package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// 本文件是 21.8.12 **P5 / X3** 的验收:
// "覆盖表补一个**非环境变量**的入口 (programmatic 注册 / 随工作流定义一并落盘)"
//
// ---------------------------------------------------------------------------
// 改造前的事实 (缺口 ②)
// ---------------------------------------------------------------------------
//
// 图覆盖表有两个投放面, 而两个都不适合"工作流定义自己声明":
//
//   - CLAUDE_GO_GRAPH_OVERRIDES=<file.json> —— 要改 env、要重启, 且**全局**一份文件:
//     一个工作流的声明住在所有工作流共用的文件里 (改 A 的风险等于改 B)。
//   - RegisterGraphOverride —— 全仓**零生产调用方**。graph_adapter.go 的文件头注释
//     写着它会在"内置或动态工作流注册时一并声明", 而代码里没有任何一处调它。
//
// 于是"给一个自定义工作流派 loop-group / 定图级并发"在**落盘 JSON 里根本没有写法**:
// foldStageGroups 全套在、TranslateWorkflow 也认, 缺的是"随定义走"这一条路。
//
// ---------------------------------------------------------------------------
// 这批用例的第一条纪律: 从 RegisterWorkflow / LoadWorkflowsFromDir 进, 不从
// RegisterGraphOverride 进
// ---------------------------------------------------------------------------
//
// 从内部那一步进, 测到的只是"RegisterGraphOverride 自己工作正常"—— 而它一直正常,
// 它只是**没人调**。P4 的 M12 变异就是这么活下来的 (用例直接调 NewRoleRegistryFrom,
// 于是把生产入口退回只看 cwd 也照样全绿)。所以这里每条都走定义进出的那两个口。
//
// ---------------------------------------------------------------------------
// 第二条纪律: env 与进程内是两个来源, 优先级必须是构造性的
// ---------------------------------------------------------------------------
//
// 改造前只有**一张表**, 而 env 文件的载入是**懒**的 (首次 Lookup 才发生)。于是谁赢
// 取决于**调用顺序**: 进程内先注册、env 后载入 ⇒ env 静默盖掉它。两个来源写同一个
// 位置而结果由时序决定, 这类缺陷在验收上是不可靠的 —— 同一个二进制在两次运行里
// 可以给出不同答案。分表之后优先级与时序无关, 那才是可断言的。

// ---- 夹具 ----------------------------------------------------------------

// p5RegisterDef 造一个带工作流级图能力声明的工作流并走**生产入口**注册。
func p5RegisterDef(t *testing.T, name string, gp *WorkflowGraphPolicy) *WorkflowDef {
	t.Helper()
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{
		Name: name, Mode: "graph", Graph: gp,
		Stages: []StageDef{
			{Name: "a", Agent: "writer", Prompt: "一 {objective}"},
			{Name: "b", Agent: "critic", Prompt: "二 {prev_result}", DependsOn: []string{"a"}},
		},
	}
	if err := RegisterWorkflow(def, roles); err != nil {
		t.Fatalf("注册工作流 %q: %v", name, err)
	}
	t.Cleanup(func() {
		UnregisterWorkflow(name)
		UnregisterGraphOverride(name)
	})
	return def
}

// p5CaptureLog 把 slog 默认输出接到一个 buffer 上 (logging 未 Init 时 For() 返回的
// 就是 slog.Default(), 见 pkg/logging/logging.go:140)。
func p5CaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// ---- 正向: 声明真的生效 ------------------------------------------------

// TestWorkflowGraphPolicyTakesEffectFromDefinition 是 X3 的核心正向判据:
// **只靠工作流定义本身**声明图能力, 直译结果里就要能看到它。
//
// 三件事一起断言, 因为它们在改造前是三个不同的缺口:
//   - MaxParallel 进了图策略;
//   - Groups 真的折成了 loop-group 节点 (不是"注册了但没人读");
//   - 全过程没有碰过 CLAUDE_GO_GRAPH_OVERRIDES。
func TestWorkflowGraphPolicyTakesEffectFromDefinition(t *testing.T) {
	const name = "p5-gp-effect"
	def := p5RegisterDef(t, name, &WorkflowGraphPolicy{
		MaxParallel:   3,
		MaxTotalNodes: 42,
		Groups: []StageGroupOverride{{
			ID: "rev", Members: []string{"a", "b"}, MaxIterations: 2,
		}},
	})

	ov, ok := LookupGraphOverride(name)
	if !ok {
		t.Fatal("工作流定义里声明的图能力必须进覆盖表 (改造前这条路不存在: " +
			"RegisterGraphOverride 全仓零生产调用方)")
	}
	if ov.MaxParallel != 3 || ov.MaxTotalNodes != 42 {
		t.Fatalf("工作流级字段没搬过去: %+v", ov)
	}
	if len(ov.Groups) != 1 || ov.Groups[0].MaxIterations != 2 {
		t.Fatalf("groups 没搬过去: %+v", ov.Groups)
	}

	spec, err := TranslateWorkflow(def)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	if spec.Policies.MaxParallel != 3 {
		t.Errorf("图级并发 = %d, 期望 3", spec.Policies.MaxParallel)
	}
	// 折组才是"声明被读到了"的证据: 只断言 Policies 的话, 一个只搬了三个 int、
	// 把 Groups 丢掉的实现照样能过。
	var loop *graph.NodeSpec
	for i := range spec.Nodes {
		if spec.Nodes[i].Kind == graph.NodeKindLoopGroup {
			loop = &spec.Nodes[i]
		}
	}
	if loop == nil {
		t.Fatalf("groups 声明没有折成 loop-group 节点 (顶层节点: %v) —— "+
			"这等于声明被搬进了表却没人读", p5NodeIDs(spec))
	}
	if loop.Group == nil || loop.Group.Loop.MaxIterations != 2 {
		t.Fatalf("折出来的组不对: %+v", loop.Group)
	}
}

// p5NodeIDs 列出图的顶层节点 id (报错时用, 免得只看到一个空指针)。
func p5NodeIDs(spec graph.GraphSpec) []string {
	out := make([]string, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		out = append(out, string(n.Kind)+":"+n.ID)
	}
	return out
}

// TestWorkflowGraphPolicyAbsentReleasesEnvEntry 断言**没写** graph 段的定义不遮蔽 env。
//
// 反面是很容易写出来的实现: 每次注册都无条件 `RegisterGraphOverride(name, &{}::toOverride())`,
// 于是每一条工作流都在进程内表里留一个空条目, 把 env 里那份真声明顶掉。
// 表现是"运维按 env 配好了, 重启之后没生效"—— 而且冲突告警还会响一堆**假的**。
func TestWorkflowGraphPolicyAbsentReleasesEnvEntry(t *testing.T) {
	const name = "p5-gp-absent"
	if err := LoadGraphOverridesJSON([]byte(`{"` + name + `":{"maxParallel":7}}`)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	p5RegisterDef(t, name, nil) // 定义里没有 graph 段

	ov, ok := LookupGraphOverride(name)
	if !ok {
		t.Fatal("env 里的条目应当仍然可见 —— 一条不声明图能力的工作流不该遮蔽它")
	}
	if ov.MaxParallel != 7 {
		t.Fatalf("env 里的声明被一条空声明顶掉了: %+v", ov)
	}
}

// ---- 优先级: 与时序无关 ------------------------------------------------

// TestWorkflowGraphPolicyBeatsEnvRegardlessOfOrder 钉住优先级**不依赖调用顺序**。
//
// 这是本次修的那个洞本身。改造前: 只有一张表 + 懒加载 env ⇒ 后发生的那个赢。
// 两种顺序都要成立, 只测一种的话, 一张表 + 不同加载时机照样能过其中一种。
func TestWorkflowGraphPolicyBeatsEnvRegardlessOfOrder(t *testing.T) {
	t.Run("先进程内后env", func(t *testing.T) {
		const name = "p5-gp-order-a"
		p5RegisterDef(t, name, &WorkflowGraphPolicy{MaxParallel: 1})
		// env 在这一步才载入 —— 改造前它会盖掉上面那条。
		if err := LoadGraphOverridesJSON([]byte(`{"` + name + `":{"maxParallel":9}}`)); err != nil {
			t.Fatal(err)
		}
		if ov, ok := LookupGraphOverride(name); !ok || ov.MaxParallel != 1 {
			t.Fatalf("定义里的声明应恒胜 env (先注册后载入也不行): %+v ok=%v", ov, ok)
		}
	})
	t.Run("先env后进程内", func(t *testing.T) {
		const name = "p5-gp-order-b"
		if err := LoadGraphOverridesJSON([]byte(`{"` + name + `":{"maxParallel":9}}`)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { UnregisterGraphOverride(name) })
		p5RegisterDef(t, name, &WorkflowGraphPolicy{MaxParallel: 1})
		if ov, ok := LookupGraphOverride(name); !ok || ov.MaxParallel != 1 {
			t.Fatalf("定义里的声明应恒胜 env: %+v ok=%v", ov, ok)
		}
	})
}

// TestWorkflowGraphPolicyShadowIsVisible 断言"env 被遮蔽"这件事**是可见的**。
//
// 优先级定下来之后, env 那一侧就存在一个"我配了但没生效"的状态 —— 它必须能解释
// 自己。这与本仓纪律 2 (降级必须可见) 是同一条: 一个静默失效的运维配置, 排查成本
// 全部落在"env 到底有没有被读"上, 而那正是最难自证的一件事。
func TestWorkflowGraphPolicyShadowIsVisible(t *testing.T) {
	const name = "p5-gp-shadow"
	buf := p5CaptureLog(t)
	if err := LoadGraphOverridesJSON([]byte(`{"` + name + `":{"maxParallel":9}}`)); err != nil {
		t.Fatal(err)
	}
	p5RegisterDef(t, name, &WorkflowGraphPolicy{MaxParallel: 1})

	LookupGraphOverride(name)
	if !strings.Contains(buf.String(), name) || !strings.Contains(buf.String(), "遮蔽") {
		t.Fatalf("env 条目被遮蔽必须留一条可见的告警, got: %q", buf.String())
	}

	// 逐名只报一次: 这条在直译热路径上, 每次都报会把日志刷爆, 而它对同一个名字
	// 恒真 —— 恒真的告警只会训练人跳过这一行。
	before := buf.Len()
	for i := 0; i < 5; i++ {
		LookupGraphOverride(name)
	}
	if buf.Len() != before {
		t.Fatalf("同一条遮蔽告警被重复打印了 (每次多 %d 字节): 恒真告警等于噪声",
			buf.Len()-before)
	}
}

// ---- 生命周期: 改写与删除都要跟着走 ------------------------------------

// TestWorkflowGraphPolicySurvivesUpdate 断言改写时新声明会**替换**旧的。
//
// 用 UpdateWorkflow (带版本) 而不是再 RegisterWorkflow: P3 起同名再注册被守卫拒绝,
// 那条路已经不是改写方式了。
func TestWorkflowGraphPolicySurvivesUpdate(t *testing.T) {
	const name = "p5-gp-update"
	roles := NewRoleRegistry(t.TempDir())
	def := p5RegisterDef(t, name, &WorkflowGraphPolicy{MaxParallel: 1})

	next := *def
	next.Graph = &WorkflowGraphPolicy{MaxParallel: 5}
	if _, err := UpdateWorkflow(&next, roles, RevisionOf(name)); err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if ov, _ := LookupGraphOverride(name); ov.MaxParallel != 5 {
		t.Fatalf("改写后的图能力没换新: %+v", ov)
	}
}

// TestWorkflowGraphPolicyRemovedWhenUpdateDropsIt 断言**去掉** graph 段之后声明也失效。
//
// 只做"加上/换新"而不做这一条, 会留下一个很隐蔽的状态: 用户删掉了那段 JSON、保存
// 成功、GET 回来也确实没有 graph 字段了, 而运行时**还在按上一版**跑。
// 这不是假设 —— P4 的 registerWorkflowGraph 注释里记着同一形态的坑
// (注册表里留着上一版的图, 而没有任何信号)。
func TestWorkflowGraphPolicyRemovedWhenUpdateDropsIt(t *testing.T) {
	const name = "p5-gp-drop"
	roles := NewRoleRegistry(t.TempDir())
	def := p5RegisterDef(t, name, &WorkflowGraphPolicy{MaxParallel: 4})

	next := *def
	next.Graph = nil
	if _, err := UpdateWorkflow(&next, roles, RevisionOf(name)); err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if ov, ok := LookupGraphOverride(name); ok && ov.MaxParallel == 4 {
		t.Fatalf("去掉了 graph 段, 运行时却还在按上一版跑: %+v", ov)
	}
}

// TestWorkflowGraphPolicyRemovedOnUnregister 断言删除工作流会摘掉它的图能力声明。
// 留着的话, 用同名**新建**一个工作流会继承上一版的 MaxParallel/Groups。
func TestWorkflowGraphPolicyRemovedOnUnregister(t *testing.T) {
	const name = "p5-gp-unreg"
	p5RegisterDef(t, name, &WorkflowGraphPolicy{MaxParallel: 6})
	if _, ok := LookupGraphOverride(name); !ok {
		t.Fatal("前置条件不成立: 应已注册")
	}
	if err := UnregisterWorkflow(name); err != nil {
		t.Fatal(err)
	}
	if ov, ok := LookupGraphOverride(name); ok {
		t.Fatalf("删除工作流后它的图能力声明应一并摘掉, got %+v", ov)
	}
}

// ---- 落盘: X3 原话"随工作流定义一并落盘" ------------------------------

// TestWorkflowGraphPolicyLoadsFromDisk 是本条的**落盘端到端**: 一个带 graph 段的
// 工作流 JSON 放进目录, 走真实的装载入口 (LoadWorkflowsFromDirReport), 声明生效。
//
// 为什么这条不能只靠上面的内存用例代替: X3 要解决的正是"不用改 env 重启" ——
// 而落盘 JSON 是那条通路唯一的形式。内存用例可以过, 落盘路径却可能因为
// **WorkflowDef 的 UnmarshalJSON 是自定义的** (D2 迁移用) 而把新字段吃掉。
func TestWorkflowGraphPolicyLoadsFromDisk(t *testing.T) {
	const name = "p5-gp-disk"
	dir := t.TempDir()
	body := `{
	  "name": "` + name + `",
	  "mode": "graph",
	  "custom": true,
	  "revision": 1,
	  "graph": {"maxParallel": 4, "groups": [{"id":"rev","members":["a","b"],"maxIterations":3}]},
	  "stages": [
	    {"name": "a", "agent": "writer", "prompt": "一 {objective}"},
	    {"name": "b", "agent": "critic", "prompt": "二 {prev_result}", "dependsOn": ["a"]}
	  ]
	}`
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		UnregisterWorkflow(name)
		UnregisterGraphOverride(name)
	})

	roles := NewRoleRegistry(t.TempDir())
	rep := LoadWorkflowsFromDirReport(dir, roles)
	if rep.Loaded == 0 {
		t.Fatalf("装载失败: %s / %v", rep.Summary(dir), rep.Details)
	}

	// 先确认 JSON 真的解出来了 —— 不然"声明生效"可能来自别处的残留。
	wf := getCustomWorkflow(name)
	if wf == nil {
		t.Fatal("装载后注册表里应有它")
	}
	if wf.Graph == nil {
		t.Fatal("落盘 JSON 的 graph 段没有解进 WorkflowDef (查 UnmarshalJSON 是否把新字段吃掉)")
	}
	if wf.Graph.MaxParallel != 4 || len(wf.Graph.Groups) != 1 {
		t.Fatalf("graph 段解错了: %+v", wf.Graph)
	}
	// 再看它有没有进运行时的表 —— 这一段才是 X3 的判据。
	if ov, ok := LookupGraphOverride(name); !ok || ov.MaxParallel != 4 {
		t.Fatalf("落盘工作流的图能力没有进覆盖表: %+v ok=%v", ov, ok)
	}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	folded := false
	for _, n := range spec.Nodes {
		if n.Kind == graph.NodeKindLoopGroup {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("落盘声明里的 groups 没有折成组: %v", p5NodeIDs(spec))
	}
}

// TestWorkflowGraphPolicyRejectsBadDeclarations 断言坏声明**在书写期就被拦下**。
//
// foldStageGroups 本来就会拒这一批, 但那是**直译期** —— 也就是唯一一次真的跑起来的
// 时候。一个声明写错的工作流若能存下来, 它就是一枚定时炸弹: POST 返回 200、GET 回来
// 一字不差、直到某次真跑才炸, 而那时看到的是图引擎的错误, 排查方向完全在别处。
//
// 这些检查**不依赖任何注册表状态** (成员是否属于本工作流、有没有被两个组收编、
// resultFrom 是不是成员、ID 与阶段是否撞名 —— 全由这份定义自己回答), 所以没有理由
// 留到开图时。分界就是这一句: **能不能只靠这份定义回答**。
// subgraph 的**存在性**不能 (被引用的图可能注册在本次之后), 那一条仍归开图时判。
func TestWorkflowGraphPolicyRejectsBadDeclarations(t *testing.T) {
	const name = "p5-gp-badgroup"
	twoStages := []StageDef{
		{Name: "a", Agent: "writer", Prompt: "一 {objective}"},
		{Name: "b", Agent: "critic", Prompt: "二 {prev_result}", DependsOn: []string{"a"}},
	}
	cases := []struct {
		why string
		gp  *WorkflowGraphPolicy
	}{
		{"无界循环", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "rev", Members: []string{"a", "b"}, MaxIterations: 0}}}},
		{"负的轮次", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "rev", Members: []string{"a", "b"}, MaxIterations: -1}}}},
		{"组没有成员", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "rev", MaxIterations: 2}}}},
		{"成员不是本工作流的阶段", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "rev", Members: []string{"a", "拼错了"}, MaxIterations: 2}}}},
		{"resultFrom 不是组成员", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "rev", Members: []string{"a"}, MaxIterations: 2, ResultFrom: "b"}}}},
		{"组 ID 与阶段撞名", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "a", Members: []string{"b"}, MaxIterations: 2}}}},
		{"同一个阶段被两个组收编", &WorkflowGraphPolicy{Groups: []StageGroupOverride{
			{ID: "g1", Members: []string{"a"}, MaxIterations: 2},
			{ID: "g2", Members: []string{"a"}, MaxIterations: 2}}}},
		{"负的并发上限", &WorkflowGraphPolicy{MaxParallel: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			roles := NewRoleRegistry(t.TempDir())
			def := &WorkflowDef{Name: name, Mode: "graph", Graph: tc.gp, Stages: twoStages}
			t.Cleanup(func() { UnregisterWorkflow(name); UnregisterGraphOverride(name) })
			if err := RegisterWorkflow(def, roles); err == nil {
				t.Fatal("坏声明必须在**注册期**就被拒 —— 存下来就成了一枚只有真跑才炸的雷")
			} else if !strings.Contains(err.Error(), "graph_adapter") {
				t.Fatalf("报错应来自图能力校验 (与直译期同一条), got %v", err)
			}
			// 被拒的那一次不得留下任何痕迹 (与 P3 的守卫同口径)。
			if ov, ok := LookupGraphOverride(name); ok {
				t.Fatalf("注册被拒却留下了覆盖表条目: %+v", ov)
			}
		})
	}
	// 阳性对照: 同一份定义, 只把坏字段改对, 必须能注册 —— 否则上面那一批可能只是
	// 因为"注册这条路整个坏了"而全绿 (本仓记过的 [[empty-hard-gate-set-is-vacuous-truth]])。
	t.Run("阳性对照", func(t *testing.T) {
		roles := NewRoleRegistry(t.TempDir())
		def := &WorkflowDef{Name: name, Mode: "graph", Stages: twoStages,
			Graph: &WorkflowGraphPolicy{Groups: []StageGroupOverride{
				{ID: "rev", Members: []string{"a", "b"}, MaxIterations: 2}}}}
		t.Cleanup(func() { UnregisterWorkflow(name); UnregisterGraphOverride(name) })
		if err := RegisterWorkflow(def, roles); err != nil {
			t.Fatalf("修好之后必须能注册, got %v", err)
		}
	})
}

// TestWorkflowGraphPolicyRoundTripKeepsJSONShape 断言新字段的 JSON 标签与 WorkflowGraphOverride
// **逐条对齐** —— X3 的注释承诺"同一份覆盖表可以在 env 文件与工作流定义之间整段搬运"。
//
// 一条 `{"maxParallel":4}` 在两个位置上必须写同一个字面量。标签写飘了不会有任何编译
// 错误, 只会让运维把 env 里验证过的一段粘进定义时静默失效。
func TestWorkflowGraphPolicyRoundTripKeepsJSONShape(t *testing.T) {
	const raw = `{"maxParallel":4,"maxTotalNodes":9,"groups":[{"id":"g","members":["a"],"maxIterations":2}]}`
	var gp WorkflowGraphPolicy
	if err := json.Unmarshal([]byte(raw), &gp); err != nil {
		t.Fatal(err)
	}
	var ov WorkflowGraphOverride
	if err := json.Unmarshal([]byte(raw), &ov); err != nil {
		t.Fatal(err)
	}
	if gp.MaxParallel != ov.MaxParallel || gp.MaxTotalNodes != ov.MaxTotalNodes {
		t.Fatalf("两个类型的 JSON 标签没有对齐: policy=%+v override=%+v", gp, ov)
	}
	if len(gp.Groups) != len(ov.Groups) || gp.Groups[0].MaxIterations != ov.Groups[0].MaxIterations {
		t.Fatalf("groups 的 JSON 形状没有对齐: %+v vs %+v", gp.Groups, ov.Groups)
	}
	// toOverride 搬过去之后不能丢东西。
	to := gp.toOverride()
	if to.MaxParallel != 4 || to.MaxTotalNodes != 9 || len(to.Groups) != 1 {
		t.Fatalf("toOverride 丢字段: %+v", to)
	}
	// 阶段级必须留空 —— 那九个字段归 StageDef 声明 (P4/X1), 两处都写等于两条送达路径。
	if to.Stages != nil {
		t.Fatalf("工作流级策略不该产生阶段级声明 (会造成同一个语义两条送达路径): %+v", to.Stages)
	}
}
