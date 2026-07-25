package feishu

// constraints_wiring_test.go —— design/01 §4.6 接线的**向后兼容**断言。
// 这是本次改动风险最高的一面: 6 个下游平台的现行行为依赖角色名子串推断, 任何
// "更严的默认"都可能打断生产。所以下面几组测试全部在证同一件事 ——
// 没有显式声明时, 新路径的输出与改造前**逐项相同**。

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestFeishuSessionConstraints_与改造前的DisabledTools字面量逐项等价
// 改造前 createSession 里是:
//
//	DisabledTools: map[string]bool{"TeamCreate": true, "TeamDelete": true, "TeamMailbox": true}
//
// 现在这份名单声明在 ConstraintSet 里再编译下发, 结果必须一字不差。
func TestFeishuSessionConstraints_与改造前的DisabledTools字面量逐项等价(t *testing.T) {
	want := map[string]bool{"TeamCreate": true, "TeamDelete": true, "TeamMailbox": true}

	cfg := &engine.Config{}
	applyConstraints(cfg, feishuSessionConstraints())

	if len(cfg.DisabledTools) != len(want) {
		t.Fatalf("黑名单项数变了: 期望 %d, 实际 %v", len(want), cfg.DisabledTools)
	}
	for name := range want {
		if !cfg.DisabledTools[name] {
			t.Errorf("黑名单缺 %s: %v", name, cfg.DisabledTools)
		}
	}
	// 白名单必须仍是 nil —— 非 nil 会让**除白名单外的所有工具**被拒, 是最容易
	// 悄悄打断生产的那种"更严"。
	if cfg.AllowedTools != nil {
		t.Errorf("绝不能凭空引入白名单, 实际 %v", cfg.AllowedTools)
	}
	// 权限档不该被约束改动 (改造前也没动过)。
	if cfg.PermissionMode != "" {
		t.Errorf("权限档被凭空收窄成 %q", cfg.PermissionMode)
	}
	if cfg.ConstraintOrigin != "feishu-session" {
		t.Errorf("来源链应可审计, 实际 %q", cfg.ConstraintOrigin)
	}
}

// TestResolveToolProfile_无显式声明时与profileForTeamRole逐项等价
// 覆盖 profileForTeamRole 的每个分支 (含 development 工作流的特例) + 那个真实误判角色。
func TestResolveToolProfile_无显式声明时与profileForTeamRole逐项等价(t *testing.T) {
	cases := []struct{ role, workflow string }{
		{"coder", "novel-v3"}, {"tester", ""}, {"architect", ""}, {"implementer", ""},
		{"world-builder", "novel-v3"}, // 老坑角色: 含 "build" → coding, 现状必须原样保留
		{"researcher", "development"}, {"reviewer", "development"},
		{"planner", "development"}, {"architect", "development"},
		{"tech-investigator", ""}, {"source-analyst", ""}, {"critic", ""},
		{"fact-checker", ""}, {"researcher", ""}, {"reviewer", ""}, {"planner", ""},
		{"writer", ""}, {"", ""}, {"whatever-else", "some-workflow"},
	}
	for _, c := range cases {
		want := profileForTeamRole(c.role, c.workflow)
		cs := agent.NewConstraintSet("team-role:" + c.role).WithToolProfile(string(mapExplicitToolProfile("")))
		got := resolveToolProfile(cs, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
			return profileForTeamRole(c.role, c.workflow)
		})
		if got != want {
			t.Errorf("role=%q workflow=%q: 新路径 %q ≠ 改造前 %q", c.role, c.workflow, got, want)
		}
		if cs.ToolProfileSource() != agent.ProfileSourceRoleNameFallback {
			t.Errorf("role=%q: 用了角色名推断却没留痕 (来源=%q)", c.role, cs.ToolProfileSource())
		}
	}
}

// TestResolveToolProfile_显式声明压过角色名推断 是那个真实误判的回归测试:
// world-builder 因角色名含 "build" 被 profileForTeamRole 判成 coding 档 (拿到 Shell),
// 显式声明 analysis 之后必须变成 analysis, 且不再走推断。
func TestResolveToolProfile_显式声明压过角色名推断(t *testing.T) {
	if inferred := profileForTeamRole("world-builder", "novel-v3"); inferred != builtin.ToolProfileCoding {
		t.Fatalf("前提变了: world-builder 的推断结果已不是 coding 而是 %q, 请更新本测试", inferred)
	}
	cs := agent.NewConstraintSet("team-role:world-builder").
		WithToolProfile(string(mapExplicitToolProfile("analysis")))
	fallbackCalled := false
	got := resolveToolProfile(cs, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
		fallbackCalled = true
		return profileForTeamRole("world-builder", "novel-v3")
	})
	if got != builtin.ToolProfileAnalysis {
		t.Errorf("显式声明应胜出, 实际 %q", got)
	}
	if fallbackCalled {
		t.Error("有显式声明时不应再跑角色名推断")
	}
	if cs.ToolProfileSource() != agent.ProfileSourceExplicit {
		t.Errorf("来源应为 explicit, 实际 %q", cs.ToolProfileSource())
	}
}

// TestResolveToolProfile_拼错的显式声明回落而不降权:
// mapExplicitToolProfile 对未知值返回 "", 于是走回退 —— 一个拼错的声明不应静默
// 剥掉 agent 的工具 (那种"更严"最难排查)。
func TestResolveToolProfile_拼错的显式声明回落到推断(t *testing.T) {
	for _, bad := range []string{"codingg", "CODEING", "readonly", "  "} {
		cs := agent.NewConstraintSet("team-role:coder").WithToolProfile(string(mapExplicitToolProfile(bad)))
		got := resolveToolProfile(cs, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
			return profileForTeamRole("coder", "")
		})
		if want := profileForTeamRole("coder", ""); got != want {
			t.Errorf("声明 %q 拼错时应回落到 %q, 实际 %q", bad, want, got)
		}
	}
}

func TestResolveSessionToolProfile_与inferFeishuToolProfile逐项等价(t *testing.T) {
	texts := []string{
		"/team list", "帮我改一下 claude-go 的代码", "分析一下最新新闻", "今天天气怎么样",
		"重构 pkg/engine", "查一下资料", "", "写个 todo 应用",
	}
	for _, txt := range texts {
		want := inferFeishuToolProfile(txt)
		if got := resolveSessionToolProfile(txt); got != want {
			t.Errorf("text=%q: 新路径 %q ≠ 改造前 %q", txt, got, want)
		}
	}
}

// TestNestedExplicitProfile_声明只能收窄不能放宽 是嵌套 agent 那条线的单调性断言。
// 最关键的一行是 (declared=coding, fallback=chat) → "": 图节点声明了 coding, 但这次
// 派生本该只有 chat 档, 直接采用就等于给一个受限子代理发了 Shell。
func TestNestedExplicitProfile_声明只能收窄不能放宽(t *testing.T) {
	cases := []struct {
		declared, fallback, want builtin.ToolProfile
		why                      string
	}{
		{"", builtin.ToolProfileCoding, "", "无声明 → 走回退 (现状)"},
		{builtin.ToolProfileChat, builtin.ToolProfileCoding, builtin.ToolProfileChat, "chat ⊆ coding, 收窄采纳"},
		{builtin.ToolProfileTeam, builtin.ToolProfileCoding, builtin.ToolProfileTeam, "team ⊆ coding, 收窄采纳"},
		{builtin.ToolProfileCoding, builtin.ToolProfileChat, "", "coding ⊋ chat 是放宽, 拒绝"},
		{builtin.ToolProfileAdmin, builtin.ToolProfileTeam, "", "admin 是最宽档, 一律拒绝"},
		{builtin.ToolProfileCoding, builtin.ToolProfileResearch, "", "coding 与 research 不可比, 拒绝"},
		{builtin.ToolProfileAnalysis, builtin.ToolProfileCoding, "", "analysis 带联网, 与 coding 不可比, 拒绝"},
		{builtin.ToolProfileAnalysis, builtin.ToolProfileResearch, builtin.ToolProfileAnalysis, "analysis ⊆ research"},
		{builtin.ToolProfileCoding, builtin.ToolProfileCoding, builtin.ToolProfileCoding, "相等即收窄"},
	}
	for _, c := range cases {
		if got := nestedExplicitProfile(c.declared, c.fallback); got != c.want {
			t.Errorf("declared=%q fallback=%q: 期望 %q, 实际 %q (%s)", c.declared, c.fallback, c.want, got, c.why)
		}
	}
}

// TestNestedExplicitProfile_只读派生不会因节点声明而拿到Shell 把上面那条最危险的
// 组合放到真实输入上跑一遍: opts.ReadOnly=true 的派生 + 节点声明 coding。
func TestNestedExplicitProfile_只读派生不会因节点声明而拿到Shell(t *testing.T) {
	fallback := profileForRunOptions(agent.RunOptions{ReadOnly: true}, agent.RunMetadata{})
	if fallback != builtin.ToolProfileResearch {
		t.Fatalf("前提变了: ReadOnly 派生的档位已不是 research 而是 %q", fallback)
	}
	if got := nestedExplicitProfile(builtin.ToolProfileCoding, fallback); got != "" {
		t.Fatalf("只读派生绝不能因为外层节点声明 coding 就拿到写盘/Shell, 实际采纳了 %q", got)
	}
}

// TestWarnRoleNameInference_按来源去重 证明 deprecation 痕迹不会刷爆日志:
// 高频阶段反复建 agent, 同一 (来源, 档位) 只提醒一次。
func TestWarnRoleNameInference_按来源去重(t *testing.T) {
	origin := "team-role:test-dedup-" + t.Name()
	warnRoleNameInference(origin, builtin.ToolProfileCoding)
	if _, ok := roleInferWarned.Load(origin + "|" + string(builtin.ToolProfileCoding)); !ok {
		t.Fatal("首次提醒后应记入去重表")
	}
	warnRoleNameInference(origin, builtin.ToolProfileCoding) // 不应 panic, 也不该重复记
	// 不同档位算不同事件 (角色档位漂移是值得再提醒一次的信号)。
	warnRoleNameInference(origin, builtin.ToolProfileTeam)
	if _, ok := roleInferWarned.Load(origin + "|" + string(builtin.ToolProfileTeam)); !ok {
		t.Error("不同档位应各自提醒一次")
	}
}

// TestWarnRoleNameInference_去重表有上限 钉住那条内存边界: 嵌套 agent 的来源含
// 模型可控的 SubagentType, 去重表必须有顶, 否则模型胡编名字就能让它无界增长。
func TestWarnRoleNameInference_去重表有上限(t *testing.T) {
	before := roleInferWarnedCount.Load()
	roleInferWarnedCount.Store(roleInferWarnCap) // 模拟已到顶
	defer roleInferWarnedCount.Store(before)

	key := "team-role:overflow-" + t.Name()
	warnRoleNameInference(key, builtin.ToolProfileCoding)
	if _, ok := roleInferWarned.Load(key + "|" + string(builtin.ToolProfileCoding)); ok {
		t.Error("到顶后不应再往去重表里塞新条目")
	}
}

// TestApplyConstraints_档位声明不会带来名单副作用:
// 团队/嵌套那两条线的 ConstraintSet 只带档位, 装配到引擎必须什么名单都不产生 ——
// 否则就是凭空变严, 会打断现有工作流。
func TestApplyConstraints_档位声明不会带来名单副作用(t *testing.T) {
	cfg := &engine.Config{PermissionMode: types.PermissionModeBypass}
	cs := agent.NewConstraintSet("team-role:coder").WithToolProfile(string(builtin.ToolProfileCoding))
	applyConstraints(cfg, cs)

	if cfg.DisabledTools != nil || cfg.AllowedTools != nil {
		t.Errorf("只声明档位却产生了名单: disabled=%v allowed=%v", cfg.DisabledTools, cfg.AllowedTools)
	}
	if cfg.PermissionMode != types.PermissionModeBypass {
		t.Errorf("权限档被凭空改成 %q", cfg.PermissionMode)
	}
	if !strings.Contains(cfg.ConstraintOrigin, "team-role:coder") {
		t.Errorf("来源链应记录角色, 实际 %q", cfg.ConstraintOrigin)
	}
}
