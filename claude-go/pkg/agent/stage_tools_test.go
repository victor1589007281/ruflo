package agent

// stage_tools_test.go —— 21.8.12 P3 / W5: "把 profileForTeamRole 的名字猜测改成
// StageDef.Tools 显式声明"。
//
// ---------------------------------------------------------------------------
// 这条改的是什么 (以及为什么不能只改一半)
// ---------------------------------------------------------------------------
//
// 改造前一个 stage 拿到什么工具, 由宿主按**角色名的子串**猜。两处误判是实的:
//
//	world-builder     含 "build"      ⇒ coding 档 ⇒ 拿到 Shell
//	tech-investigator 含 "investigat" ⇒ team 档 (缺联网), 而它是只读调研角色
//
// 改造后 stage 可以显式声明; 声明一出现, 猜测就不再被问。
//
// 但"声明能被读到一个"是不够的 —— 这一层有三个各自独立会失效的接点, 每一个都要
// 单独钉住, 因为它们失效时的表现各不相同且都是静默的:
//
//	1. 声明进 ConstraintSet      (ApplyStageTools)  ⇒ 失败时档位悄悄还是猜的那个
//	2. 声明跨图直译器            (stageDeclaredOverride → AgentSpec → 还原)
//	                                              ⇒ 失败时 pipeline 生效而 graph 不生效
//	3. 没声明时行为一个字节不变   (回归面)          ⇒ 失败时 6 个下游平台被静默改行为
//
// 第 3 条与前两条同等重要: 本仓 40 条在野工作流 JSON 与 6 个下游平台**都不声明**,
// 如果"没声明"这条路出了偏差, 症状是"所有既有工作流的能力面变了", 而这次改动的
// 全部卖点是它不该变。

import (
	"context"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// TestApplyStageTools档位取代猜测 声明了 profile 就必须被应用, 且来源是 explicit。
//
// 断言**来源**而不只是取值: "world-builder 拿到了 research 档" 这件事, 在
// "显式声明了 research" 与 "名字碰巧被判成 research" 两种情况下**取值完全相同** ——
// 只有 ProfileSource 能把它们分开, 而这条改动要的正是前者。
func TestApplyStageTools档位取代猜测(t *testing.T) {
	cs := NewConstraintSet("stage:world-builder")
	applied := ApplyStageTools(cs, &StageTools{Profile: ProfileAnalysis})
	if len(applied) == 0 {
		t.Fatal("声明了 profile 却没有被应用")
	}
	if got := cs.ToolProfile(); got != ProfileAnalysis {
		t.Fatalf("档位应为声明的 analysis, got %q", got)
	}
	if got := cs.ToolProfileSource(); got != ProfileSourceExplicit {
		t.Fatalf("档位来源必须是 explicit —— 否则它随时会被一条猜得更宽的规则顶掉, got %q", got)
	}
}

// TestApplyStageTools未声明时一个字节不动 是那个"6 个下游平台行为不变"的回归面。
func TestApplyStageTools未声明时一个字节不动(t *testing.T) {
	for name, st := range map[string]*StageTools{
		"nil":     nil,
		"零值":      {},
		"三项全空的指针": {Allow: nil},
	} {
		cs := NewConstraintSet("stage:x")
		if applied := ApplyStageTools(cs, st); len(applied) != 0 {
			t.Errorf("%s: 未声明时不该应用任何东西, got %v", name, applied)
		}
		if got := cs.ToolProfile(); got != "" {
			t.Errorf("%s: 未声明时档位必须保持未决定 (留空给 fallback), got %q", name, got)
		}
		if got := cs.ToolProfileSource(); got != ProfileSourceUnset {
			t.Errorf("%s: 未声明时来源必须仍是 unset, got %q", name, got)
		}
	}
}

// TestApplyStageTools显式空白名单拒绝一切 —— `tools: []` 与"没写 tools"必须分开。
//
// 这条是 P2′ 那个坑的同一形态在 stage 层的复现: 用 `len(Tools) > 0` 当门槛, 就把
// "要求拒绝一切工具"这条 fail-closed 声明整个吞掉, 结果是完全放行 —— 一份安全声明
// 被静默翻译成它的反面。所以判据必须是**键写没写**(指针非 nil), 不是长度。
func TestApplyStageTools显式空白名单拒绝一切(t *testing.T) {
	empty := []string{}
	cs := NewConstraintSet("stage:x")
	ApplyStageTools(cs, &StageTools{Allow: &empty})

	allow := cs.CompiledToolAllow()
	if allow == nil {
		t.Fatal("tools: [] 必须被翻译成**非 nil 的空白名单** —— nil 的语义是「不限制」, 正好相反")
	}
	if len(allow) != 0 {
		t.Fatalf("空名单不该含任何工具, got %v", allow)
	}
}

// TestApplyStageTools白名单按交集收窄 说明名单与档位是正交的两维。
func TestApplyStageTools白名单按交集收窄(t *testing.T) {
	allow := []string{"Read", "Bash"}
	cs := NewConstraintSet("stage:x")
	applied := ApplyStageTools(cs, &StageTools{Profile: ProfileAnalysis, Allow: &allow})
	if len(applied) != 2 {
		t.Fatalf("档位与名单应各应用一次, got %v", applied)
	}
	if got := cs.ToolProfile(); got != ProfileAnalysis {
		t.Fatalf("档位应被应用, got %q", got)
	}
	m := cs.CompiledToolAllow()
	if len(m) != 2 || !m["Read"] || !m["Bash"] {
		t.Fatalf("白名单应含 Read 与 Bash, got %v", m)
	}
}

// TestApplyStageTools黑名单是并集 声明只能收窄, 不能把已有的黑名单顶掉。
func TestApplyStageTools黑名单是并集(t *testing.T) {
	cs := NewConstraintSet("stage:x").WithDeniedTools("WebFetch")
	ApplyStageTools(cs, &StageTools{Deny: []string{"Bash", "WebFetch"}})
	m := cs.CompiledToolDeny()
	for _, n := range []string{"WebFetch", "Bash"} {
		if !m[n] {
			t.Fatalf("黑名单应为并集 (含外层已有的 + 本阶段声明的), 缺 %q: %v", n, m)
		}
	}
}

// TestApplyStageTools不改写声明本身 注册表里那份定义不能被执行期改动。
//
// ClaimStageTools 拿到的指针指向**注册表里那份 StageDef 的 StageTools**。如果就地改它
// (例如规范化时把 nil 写成空切片), 一次运行就会永久改掉资产定义 —— 而那个改动会被
// 下一次 GET 读出来并落盘, 于是"跑了一次"变成了"改了一版"。
func TestApplyStageTools不改写声明本身(t *testing.T) {
	allow := []string{"Read"}
	st := &StageTools{Profile: ProfileAnalysis, Allow: &allow, Deny: []string{"Bash"}}
	cs := NewConstraintSet("stage:x")
	ApplyStageTools(cs, st)

	if st.Allow != &allow {
		t.Error("Allow 指针被换掉了 (应原样保留)")
	}
	if len(allow) != 1 || allow[0] != "Read" {
		t.Fatalf("声明的 Allow 切片被就地改动了: %v", allow)
	}
	// 反向: 改约束集不该影响声明。
	cs.WithDeniedTools("StrReplace")
	if len(st.Deny) != 1 {
		t.Fatalf("约束集的改动串到了注册表那份声明上: %v", st.Deny)
	}
}

// TestStageToolsDeclares 三态判定的分辨力。
func TestStageToolsDeclares(t *testing.T) {
	empty := []string{}
	cases := map[string]struct {
		st   *StageTools
		want bool
	}{
		"nil":  {nil, false},
		"零值":   {&StageTools{}, false},
		"只有档位": {&StageTools{Profile: ProfileChat}, true},
		"显式空白名单(最容易漏的那个)": {&StageTools{Allow: &empty}, true},
		"只有黑名单": {&StageTools{Deny: []string{"Bash"}}, true},
	}
	for name, c := range cases {
		if got := c.st.Declares(); got != c.want {
			t.Errorf("%s: Declares() = %v, want %v", name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 跨图直译器的往返 (第 2 个接点)
// ---------------------------------------------------------------------------

// TestStageTools跨图直译往返 声明必须原样穿过 WorkflowDef → GraphSpec → StageDef。
//
// 这条挡的是"pipeline 生效、graph 静默不生效": 图路径上 stageNodeRunner **重建**一个
// StageDef, 字段没被搬过去就是丢。而两条路径的产出在运行读数上看起来一样
// (都是一组可见工具), 所以这个丢法没有任何报错。
func TestStageTools跨图直译往返(t *testing.T) {
	allow := []string{"Read", "Grep"}
	deny := []string{"Bash"}
	decl := &StageTools{Profile: ProfileAnalysis, Allow: &allow, Deny: deny}

	// 1. StageDef → 覆盖结构
	so := stageDeclaredOverride(StageDef{Name: "s", Agent: "r", Tools: decl})
	if so.ToolProfile != ProfileAnalysis {
		t.Fatalf("档位没被搬进覆盖结构: %q", so.ToolProfile)
	}
	if so.Tools == nil || len(*so.Tools) != 2 {
		t.Fatalf("白名单没被搬进覆盖结构: %v", so.Tools)
	}
	if len(so.DisallowedTools) != 1 || so.DisallowedTools[0] != "Bash" {
		t.Fatalf("黑名单没被搬进覆盖结构: %v", so.DisallowedTools)
	}

	// 2. 覆盖结构 → AgentSpec (直译器真正写进图的那一步) → StageDef (stageNodeRunner 重建)
	back := stageToolsFromAgentSpec(graph.AgentSpec{
		ToolProfile: so.ToolProfile, Tools: so.Tools, DisallowedTools: so.DisallowedTools,
	})
	if back == nil || !back.Declares() {
		t.Fatal("从 AgentSpec 还原失败 —— 图路径上的 agent 会拿不到这个声明")
	}
	if back.Profile != ProfileAnalysis {
		t.Errorf("档位往返丢失: %q", back.Profile)
	}
	if back.Allow == nil || len(*back.Allow) != 2 {
		t.Errorf("白名单往返丢失: %v", back.Allow)
	}
	if len(back.Deny) != 1 {
		t.Errorf("黑名单往返丢失: %v", back.Deny)
	}
}

// TestStageTools空声明不占ctx "没声明"这条路必须一个字节都不进 context。
//
// WithStageTools(nil) 是显式 no-op 而不是"放一个空值进去": 后者会让
// StageToolsFromContext 返回非 nil, 于是消费点那条 `if st != nil` 的分支恒真 ——
// 一份"看起来接了、实际什么都没带"的接线, 正是本仓在 agent_assets.go 里已经
// 踩过一次的坑 (两条送达路径使得拆掉任一条都不改变行为)。
func TestStageTools空声明不占ctx(t *testing.T) {
	base := context.Background()
	for name, st := range map[string]*StageTools{
		"nil":   nil,
		"零值":    {},
		"空切片指针": {Allow: &[]string{}},
	} {
		ctx := WithStageTools(base, st)
		got := StageToolsFromContext(ctx)
		if name == "空切片指针" {
			if got == nil {
				t.Errorf("%s: 显式空白名单是**有效声明**, 必须进 ctx", name)
			}
			continue
		}
		if got != nil {
			t.Errorf("%s: 未声明不该在 ctx 上留下非 nil 值", name)
		}
	}
}

// TestStageTools覆盖表压住阶段声明 是"覆盖表优先"这条既有规则在新增字段上的延续。
//
// mergeStageOverride 的注释里已经点名过这个坑: 逐字段手抄合并规则, **加一个字段就要
// 加一行**, 忘加的表现是"覆盖表里写了不生效"而不是任何报错。本轮给
// StageGraphOverride 加了 Tools/DisallowedTools 两个字段, 所以这条必须有。
func TestStageTools覆盖表压住阶段声明(t *testing.T) {
	declAllow := []string{"Read", "Grep", "Glob"}
	ovAllow := []string{"Read"}
	decl := &StageTools{Profile: ProfileAnalysis, Allow: &declAllow, Deny: []string{"Bash"}}

	so := stageDeclaredOverride(StageDef{Name: "s", Tools: decl})
	ov := StageGraphOverride{
		ToolProfile:     ProfileCoding,
		Tools:           &ovAllow,
		DisallowedTools: []string{"WebFetch"},
	}
	merged := mergeStageOverride(so, ov)

	if merged.ToolProfile != ProfileCoding {
		t.Errorf("覆盖表应压住定义的档位, got %q", merged.ToolProfile)
	}
	if merged.Tools == nil || len(*merged.Tools) != 1 || (*merged.Tools)[0] != "Read" {
		t.Fatalf("覆盖表应**整份替换**白名单 (半份继承会拼出谁都没声明过的名单), got %v", merged.Tools)
	}
	if len(merged.DisallowedTools) != 1 || merged.DisallowedTools[0] != "WebFetch" {
		t.Errorf("覆盖表的黑名单没生效: %v", merged.DisallowedTools)
	}
}

// ---------------------------------------------------------------------------
// 端到端: 从真实 executeGraph 出来 (第 1 个接点与第 2 个接点合起来验)
// ---------------------------------------------------------------------------

// TestStageTools真图执行时到达factory —— 这条是本文件里唯一**不可替代**的用例。
//
// 上面那些往返测试各自只测一段: ApplyStageTools 测函数, 往返测试手工喂 AgentSpec。
// 而"声明真的从 executeStage 的 ctx 走到了宿主的 factory"这件事, 只有跑一次真实图
// 才验得了 —— 中间隔着 stageNodeRunner 重建 StageDef、ExecuteSingleStage、
// executeStage 注入三个环节, 每一个都能把声明悄悄丢掉而不报错。
//
// 与 TestGraphOverrideToolProfileAndMaxTurns 的分工: 那条测的是**覆盖表**这条来源
// (NodeExecHints), 这条测的是**工作流定义自己声明**那条来源 (StageDef.Tools)。
// 两条来源必须各有一条用例, 因为它们的接线是分开写的 —— 覆盖表那条走
// NodeExecHints, 定义那条走 StageToolsFromContext。
func TestStageTools真图执行时到达factory(t *testing.T) {
	const name = "graph-stage-tools-test"
	allow := []string{"Read", "Grep"}
	deny := []string{"Bash"}

	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "dig", Agent: "world-builder", Prompt: "写设定 {objective}",
			Tools: &StageTools{Profile: "analysis", Allow: &allow, Deny: deny}},
		// 第二个阶段**不声明**: 它与声明的那一个跑在同一次执行里, 于是这条用例
		// 同时充当"没声明时 ctx 上什么都没有"的对照 —— 只测声明那一个的话,
		// "把声明无条件发给所有阶段"这种错法照样全绿。
		{Name: "plain", Agent: "world-builder", Prompt: "同上 {objective}"},
	}}

	// 按**节点名**分别捕获, 不能用 atomic.Value 存"最后一次":
	// 两个阶段跑在同一次执行里, 最后一次写入会盖掉前一次, 于是"声明到达了 dig"
	// 与"声明到达了 plain"这两件事被压成一条读数 —— 而它们恰恰是要分开看的。
	// (第一版就是这么写的, 结果 plain 的 nil 盖掉了 dig 的声明, 用例假红。)
	var seen sync.Map // node 名 → *StageTools
	we := &WorkflowExecutor{
		planCfgResolver: &PlanConfigResolver{},
		notify:          func(_, _ string) {},
		factory: func(ctx context.Context, role, _ string) (AgentRunner, error) {
			seen.Store(NodeExecHintsFromContext(ctx).Node, StageToolsFromContext(ctx))
			return &progStageRunner{role: role, fn: func(_ context.Context, r, _ string) (string, error) {
				return stubStageOutput(r), nil
			}}, nil
		},
	}
	if _, err := we.executeGraph(context.Background(), wf, "测试目标", newStubTeam(t, "stagetools")); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}

	load := func(node string) *StageTools {
		v, ok := seen.Load(node)
		if !ok {
			t.Fatalf("阶段 %q 从没走到 factory (对照失效)", node)
		}
		st, _ := v.(*StageTools)
		return st
	}

	got := load("dig")
	if got == nil || !got.Declares() {
		t.Fatal("工作流定义里声明的 StageDef.Tools 没能到达宿主 factory —— " +
			"这正是「pipeline 生效、graph 静默不生效」那类缺陷")
	}
	if got.Profile != "analysis" {
		t.Errorf("档位没到达: %q", got.Profile)
	}
	if got.Allow == nil || len(*got.Allow) != 2 {
		t.Errorf("白名单没到达: %v", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "Bash" {
		t.Errorf("黑名单没到达: %v", got.Deny)
	}

	// 对照: **没声明的那个阶段必须什么都不带**。
	//
	// 少了这一条, "声明被无条件广播给每一个阶段"这种错法照样全绿 —— 而那是把一个
	// 高风险阶段的能力面泄漏给它的邻居, 且看起来一切正常。
	if plain := load("plain"); plain != nil {
		t.Fatalf("未声明的阶段不该在 ctx 上拿到任何工具声明, got %+v", plain)
	}
}
