package agent

// constraints_test.go —— design/01 §4.6 的两条硬要求各自对应一组断言:
//  1. 「约束只能收窄不能放宽」(单调性, 安全属性): Narrow 系列测试逐维度证明放宽被拒,
//     且被拒时**不返回子约束** —— 否则单调性就只是一句注释。
//  2. 「单一真源」: TestProfileCaps_与真实注册表双向一致 用 pkg/tool/builtin 的真实
//     注册表反查本包的特权位表, 任何人往某档位加工具而忘了同步这里, 测试就红。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// ---------------------------------------------------------------------------
// 单调收窄: 放宽必须被拒
// ---------------------------------------------------------------------------

func TestNarrow_工具档位放宽被拒(t *testing.T) {
	parent := NewConstraintSet("session").WithToolProfile(ProfileAnalysis)
	child := NewConstraintSet("node").WithToolProfile(ProfileCoding)

	got, err := parent.Narrow(child)
	if err == nil {
		t.Fatalf("analysis → coding 是放宽 (拿到写盘/Shell), 必须被拒, 却通过了: %+v", got.Tools)
	}
	if got != nil {
		t.Fatalf("放宽被拒时第一个返回值必须为 nil (调用方不得'退而用 child'), 实际 %+v", got)
	}
	if !strings.Contains(err.Error(), "单调性违规") || !strings.Contains(err.Error(), ProfileCoding) {
		t.Errorf("错误信息应指明违规的档位, 实际: %v", err)
	}
}

func TestNarrow_工具档位收窄放行(t *testing.T) {
	parent := NewConstraintSet("session").WithToolProfile(ProfileCoding)
	child := NewConstraintSet("node").WithToolProfile(ProfileTeam)

	got, err := parent.Narrow(child)
	if err != nil {
		t.Fatalf("coding → team 是收窄, 必须放行: %v", err)
	}
	if got.ToolProfile() != ProfileTeam {
		t.Errorf("生效档位应为子声明 team, 实际 %q", got.ToolProfile())
	}
	if got.ConstraintOrigin() != "session > node" {
		t.Errorf("继承链应表达'从哪继承而来', 实际 %q", got.ConstraintOrigin())
	}
}

func TestNarrow_档位不可比时拒绝(t *testing.T) {
	// coding 有写盘/Shell 但没有联网; analysis 有联网但没有写盘。两者互不包含,
	// 谁也不是谁的收窄 —— 这时必须拒绝而不是猜一个 (fail-closed)。
	if _, err := NewConstraintSet("p").WithToolProfile(ProfileCoding).
		Narrow(NewConstraintSet("c").WithToolProfile(ProfileAnalysis)); err == nil {
		t.Fatal("coding 与 analysis 不可比 (analysis 带 CapNet), 必须拒绝")
	}
	if _, err := NewConstraintSet("p").WithToolProfile(ProfileCoding).
		Narrow(NewConstraintSet("c").WithToolProfile(ProfileResearch)); err == nil {
		t.Fatal("coding 与 research 不可比 (research 带 CapNet), 必须拒绝")
	}
}

func TestNarrow_父未声明档位时子自由(t *testing.T) {
	got, err := NewConstraintSet("p").Narrow(NewConstraintSet("c").WithToolProfile(ProfileAdmin))
	if err != nil {
		t.Fatalf("父没有档位上界时子可自由声明: %v", err)
	}
	if got.ToolProfile() != ProfileAdmin {
		t.Errorf("期望 admin, 实际 %q", got.ToolProfile())
	}
}

func TestNarrow_白名单越界被拒(t *testing.T) {
	parent := NewConstraintSet("cli").WithToolAllowList([]string{"Read", "Grep"})
	child := NewConstraintSet("node").WithToolAllowList([]string{"Read", "Shell"})

	got, err := parent.Narrow(child)
	if err == nil {
		t.Fatal("子白名单出现父名单外的 Shell = 放宽, 必须被拒")
	}
	if got != nil {
		t.Fatalf("被拒时不得返回合成结果, 实际 %+v", got)
	}
	if !strings.Contains(err.Error(), "Shell") {
		t.Errorf("错误应点名越界工具 Shell, 实际: %v", err)
	}
}

func TestNarrow_白名单取子集放行(t *testing.T) {
	parent := NewConstraintSet("cli").WithToolAllowList([]string{"Read", "Grep", "Glob"})
	child := NewConstraintSet("node").WithToolAllowList([]string{"Read"})

	got, err := parent.Narrow(child)
	if err != nil {
		t.Fatalf("子集必须放行: %v", err)
	}
	allow := got.CompiledToolAllow()
	if len(allow) != 1 || !allow["Read"] {
		t.Errorf("期望白名单收窄到 {Read}, 实际 %v", allow)
	}
}

func TestNarrow_父无白名单时子可新设(t *testing.T) {
	// 从"不设白名单"到"只放行两个工具"本身就是收窄, 必须允许。
	got, err := NewConstraintSet("p").Narrow(NewConstraintSet("c").WithToolAllowList([]string{"Read", "Grep"}))
	if err != nil {
		t.Fatalf("新设白名单是收窄: %v", err)
	}
	if len(got.CompiledToolAllow()) != 2 {
		t.Errorf("期望 2 个白名单项, 实际 %v", got.CompiledToolAllow())
	}
}

func TestNarrow_黑名单只增不减(t *testing.T) {
	parent := NewConstraintSet("p").WithDeniedTools("TeamCreate", "TeamDelete")
	child := NewConstraintSet("c").WithDeniedTools("Shell")

	got, err := parent.Narrow(child)
	if err != nil {
		t.Fatalf("黑名单并集不构成放宽: %v", err)
	}
	deny := got.CompiledToolDeny()
	for _, want := range []string{"TeamCreate", "TeamDelete", "Shell"} {
		if !deny[want] {
			t.Errorf("黑名单应为并集, 缺 %s: %v", want, deny)
		}
	}
	// 子约束没有"解除禁用"的语法, 这里顺带钉住这个事实: 父禁的永远还在。
	if len(deny) != 3 {
		t.Errorf("期望 3 项黑名单, 实际 %v", deny)
	}
}

func TestNarrow_权限提权被拒(t *testing.T) {
	cases := []struct {
		parent, child string
		wantErr       bool
	}{
		{string(types.PermissionModePlan), string(types.PermissionModeBypass), true},    // 只读 → 全放行
		{string(types.PermissionModeDefault), string(types.PermissionModeBypass), true}, // 每次问 → 全放行
		{"", string(types.PermissionModeBypass), true},                                  // 未设置按 default 计, 仍不许提权
		{string(types.PermissionModeDefault), string(types.PermissionModePlan), false},  // 降权放行
		{string(types.PermissionModeBypass), string(types.PermissionModeAcceptEdits), false},
		{string(types.PermissionModePlan), string(types.PermissionModePlan), false}, // 相等
	}
	for _, c := range cases {
		_, err := NewConstraintSet("p").WithPermission(c.parent).
			Narrow(NewConstraintSet("c").WithPermission(c.child))
		if c.wantErr && err == nil {
			t.Errorf("父=%q 子=%q 是提权, 必须被拒", c.parent, c.child)
		}
		if !c.wantErr && err != nil {
			t.Errorf("父=%q 子=%q 是收窄或相等, 应放行, 实际: %v", c.parent, c.child, err)
		}
	}
}

func TestNarrow_Blocking不可从fail_closed降级为fail_open(t *testing.T) {
	// "fail-closed 的治理, fail-open 的交付": 已经声明失败阻塞下游的门禁,
	// 子约束不能把它降级成失败也放过。
	if _, err := NewConstraintSet("p").WithBlocking(BlockingFailBlocksDependents).
		Narrow(NewConstraintSet("c").WithBlocking(BlockingFailOpen)); err == nil {
		t.Fatal("fail_blocks_dependents → fail_open 是放宽, 必须被拒")
	}
	got, err := NewConstraintSet("p").WithBlocking(BlockingFailOpen).
		Narrow(NewConstraintSet("c").WithBlocking(BlockingFailBlocksDependents))
	if err != nil {
		t.Fatalf("fail_open → fail_blocks_dependents 是收紧, 应放行: %v", err)
	}
	if got.Blocking != BlockingFailBlocksDependents {
		t.Errorf("期望 %s, 实际 %s", BlockingFailBlocksDependents, got.Blocking)
	}
	// fail_closed 是设计文字里的写法, 归一到 WBS 的线上值。
	if NewConstraintSet("x").WithBlocking("fail_closed").Blocking != BlockingFailBlocksDependents {
		t.Error("fail_closed 应归一为 fail_blocks_dependents")
	}
}

func TestNarrow_路径不得跳出父沙箱(t *testing.T) {
	parent := &ConstraintSet{Origin: "p", Paths: &PathConstraint{WriteRoots: []string{"/srv/app"}}}

	// 前缀相同但不是子目录 —— 这是路径沙箱最经典的绕过手法。
	if _, err := parent.Narrow(&ConstraintSet{Origin: "c", Paths: &PathConstraint{WriteRoots: []string{"/srv/appx"}}}); err == nil {
		t.Fatal("/srv/appx 不在 /srv/app 之下 (只是字符串前缀), 必须被拒")
	}
	if _, err := parent.Narrow(&ConstraintSet{Origin: "c", Paths: &PathConstraint{WriteRoots: []string{"/etc"}}}); err == nil {
		t.Fatal("/etc 跳出父沙箱, 必须被拒")
	}
	got, err := parent.Narrow(&ConstraintSet{Origin: "c", Paths: &PathConstraint{WriteRoots: []string{"/srv/app/sub"}}})
	if err != nil {
		t.Fatalf("子目录是收窄, 应放行: %v", err)
	}
	if len(got.Paths.WriteRoots) != 1 || got.Paths.WriteRoots[0] != "/srv/app/sub" {
		t.Errorf("期望写根收窄到 /srv/app/sub, 实际 %v", got.Paths.WriteRoots)
	}
}

func TestNarrow_模型档位上限不可抬高(t *testing.T) {
	parent := &ConstraintSet{Origin: "p", ModelTier: &ModelBound{MaxTier: ModelTierStandard}}
	if _, err := parent.Narrow(&ConstraintSet{Origin: "c", ModelTier: &ModelBound{MaxTier: ModelTierLarge}}); err == nil {
		t.Fatal("上限 standard → large 是放宽, 必须被拒")
	}
	if _, err := parent.Narrow(&ConstraintSet{Origin: "c", ModelTier: &ModelBound{MaxTier: ""}}); err == nil {
		t.Fatal("子不声明上限 = 去掉父的上限, 同样是放宽, 必须被拒")
	}
	if _, err := parent.Narrow(&ConstraintSet{Origin: "c", ModelTier: &ModelBound{MaxTier: ModelTierSmall}}); err != nil {
		t.Fatalf("上限 standard → small 是收窄: %v", err)
	}
}

func TestNarrow_不修改入参(t *testing.T) {
	parent := NewConstraintSet("p").WithToolProfile(ProfileCoding).WithDeniedTools("A")
	child := NewConstraintSet("c").WithToolProfile(ProfileTeam).WithDeniedTools("B")

	if _, err := parent.Narrow(child); err != nil {
		t.Fatalf("意外失败: %v", err)
	}
	if parent.ToolProfile() != ProfileCoding || len(parent.Tools.Deny) != 1 {
		t.Errorf("父约束被修改了: %+v", parent.Tools)
	}
	if child.ToolProfile() != ProfileTeam || len(child.Tools.Deny) != 1 {
		t.Errorf("子约束被修改了: %+v", child.Tools)
	}
	if len(parent.Lineage) != 0 {
		t.Errorf("父的继承链被写脏: %v", parent.Lineage)
	}
}

func TestNarrow_多维度违规一次报全(t *testing.T) {
	parent := NewConstraintSet("p").WithToolProfile(ProfileTeam).
		WithPermission(string(types.PermissionModePlan)).
		WithToolAllowList([]string{"Read"})
	child := NewConstraintSet("c").WithToolProfile(ProfileCoding).
		WithPermission(string(types.PermissionModeBypass)).
		WithToolAllowList([]string{"Read", "Shell"})

	_, err := parent.Narrow(child)
	if err == nil {
		t.Fatal("三个维度同时放宽, 必须被拒")
	}
	for _, frag := range []string{"档位", "权限档", "白名单"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("错误信息应涵盖 %q 维度, 实际: %v", frag, err)
		}
	}
}

func TestNarrow_继承链累加(t *testing.T) {
	l1 := NewConstraintSet("cli-flags").WithToolProfile(ProfileAdmin)
	l2, err := l1.Narrow(NewConstraintSet("feishu-session").WithToolProfile(ProfileCoding))
	if err != nil {
		t.Fatalf("l2: %v", err)
	}
	l3, err := l2.Narrow(NewConstraintSet("node:writer").WithToolProfile(ProfileTeam))
	if err != nil {
		t.Fatalf("l3: %v", err)
	}
	if want := "cli-flags > feishu-session > node:writer"; l3.ConstraintOrigin() != want {
		t.Errorf("继承链应为 %q, 实际 %q", want, l3.ConstraintOrigin())
	}
	if !strings.Contains(l3.AuditTrail(), "profile=team") {
		t.Errorf("审计串应包含生效档位, 实际 %q", l3.AuditTrail())
	}
}

func TestNarrow_子未声明档位则继承并标记来源(t *testing.T) {
	got, err := NewConstraintSet("p").WithToolProfile(ProfileTeam).Narrow(NewConstraintSet("c"))
	if err != nil {
		t.Fatalf("意外失败: %v", err)
	}
	if got.ToolProfile() != ProfileTeam {
		t.Errorf("应继承父档位, 实际 %q", got.ToolProfile())
	}
	if got.ToolProfileSource() != ProfileSourceInherited {
		t.Errorf("来源应标为 inherited (审计上区分自声明与继承), 实际 %q", got.ToolProfileSource())
	}
}

// ---------------------------------------------------------------------------
// fail-closed: nil 与空集的区别不能丢
// ---------------------------------------------------------------------------

func TestAllowList_空集是全拒而非不限制(t *testing.T) {
	unset := NewConstraintSet("a") // 从未设过白名单
	if unset.CompiledToolAllow() != nil {
		t.Error("未设白名单必须编译成 nil (= 不限制), 否则会把所有工具全拒")
	}

	denyAll := NewConstraintSet("b").WithToolAllowList([]string{})
	allow := denyAll.CompiledToolAllow()
	if allow == nil {
		t.Fatal("空白名单必须编译成非 nil 空 map (= 全部拒绝), nil 会退化成不限制 —— fail-open 回归")
	}
	if len(allow) != 0 {
		t.Errorf("期望空 map, 实际 %v", allow)
	}
}

func TestAllowList_JSON往返保住空集语义(t *testing.T) {
	// 这是 ToolConstraint.Allow 故意不加 omitempty 的原因: 空集若被省掉,
	// "全部拒绝"会静默变成"不限制"。约束经配置文件/HTTP 下发时必然要往返 JSON。
	for _, cs := range []*ConstraintSet{
		NewConstraintSet("deny-all").WithToolAllowList([]string{}),
		NewConstraintSet("unset"),
		NewConstraintSet("two").WithToolAllowList([]string{"Read", "Grep"}),
	} {
		blob, err := json.Marshal(cs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var back ConstraintSet
		if err := json.Unmarshal(blob, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		before, after := cs.CompiledToolAllow(), back.CompiledToolAllow()
		if (before == nil) != (after == nil) {
			t.Errorf("%s: JSON 往返丢了 nil/空集的区别 (%v → %v), json=%s", cs.Origin, before, after, blob)
		}
		if len(before) != len(after) {
			t.Errorf("%s: 白名单长度变了 %d → %d", cs.Origin, len(before), len(after))
		}
	}
}

func TestNarrowedPermissionMode_只在更严时下发(t *testing.T) {
	cases := []struct {
		declared, current, want string
	}{
		{string(types.PermissionModePlan), string(types.PermissionModeDefault), string(types.PermissionModePlan)},
		{string(types.PermissionModeBypass), string(types.PermissionModeDefault), ""}, // 更松 → 不下发
		{string(types.PermissionModePlan), string(types.PermissionModePlan), ""},      // 相等 → 无需下发
		{"", string(types.PermissionModeDefault), ""},                                 // 未声明
		{"nonsense", string(types.PermissionModeDefault), ""},                         // 不认识 → 不下发 (fail-closed)
		{string(types.PermissionModePlan), "nonsense", ""},                            // 当前档不认识 → 不动
	}
	for _, c := range cases {
		got := NewConstraintSet("x").WithPermission(c.declared).NarrowedPermissionMode(c.current)
		if got != c.want {
			t.Errorf("declared=%q current=%q: 期望 %q, 实际 %q", c.declared, c.current, c.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 档位决策: 显式压过回退
// ---------------------------------------------------------------------------

func TestResolveToolProfile_显式声明压过回退且不调用回退(t *testing.T) {
	cs := NewConstraintSet("team-role:world-builder").WithToolProfile(ProfileAnalysis)
	called := false
	got, src := cs.ResolveToolProfile(ProfileSourceRoleNameFallback, func() string {
		called = true
		return ProfileCoding
	})
	if got != ProfileAnalysis {
		t.Errorf("显式声明必须胜出, 实际 %q", got)
	}
	if src != ProfileSourceExplicit {
		t.Errorf("来源应为 explicit, 实际 %q", src)
	}
	if called {
		t.Error("有显式声明时不应再调用角色名推断 —— 那是白算一遍, 也会让来源标记失真")
	}
}

func TestResolveToolProfile_无声明时回退并留痕(t *testing.T) {
	cs := NewConstraintSet("team-role:coder")
	got, src := cs.ResolveToolProfile(ProfileSourceRoleNameFallback, func() string { return ProfileCoding })
	if got != ProfileCoding {
		t.Errorf("应采用回退结果, 实际 %q", got)
	}
	if src != ProfileSourceRoleNameFallback {
		t.Errorf("来源必须标成角色名推断 (这是将来退役它的依据), 实际 %q", src)
	}
	// 回退结果被写回约束集: 决策之后只有一个地方持有生效档位。
	if cs.ToolProfile() != ProfileCoding || cs.ToolProfileSource() != ProfileSourceRoleNameFallback {
		t.Errorf("回退结果未写回约束集: %+v", cs.Tools)
	}
}

func TestProfileNarrows与NarrowerProfile(t *testing.T) {
	if !ProfileNarrows(ProfileChat, ProfileCoding) {
		t.Error("chat 应是 coding 的收窄")
	}
	if ProfileNarrows(ProfileCoding, ProfileChat) {
		t.Error("coding 不是 chat 的收窄")
	}
	if !ProfileNarrows(ProfileCoding, ProfileCoding) {
		t.Error("相等应算收窄 (自反)")
	}
	if !ProfileNarrows(ProfileCoding, ProfileAdmin) {
		t.Error("admin 是顶元素, 任何档位都是它的收窄")
	}
	if ProfileNarrows("typo", ProfileAdmin) || ProfileNarrows(ProfileChat, "typo") {
		t.Error("不认识的档位名一律返回 false (fail-closed)")
	}
	if got, ok := NarrowerProfile(ProfileTeam, ProfileCoding); !ok || got != ProfileTeam {
		t.Errorf("NarrowerProfile(team, coding) 应为 team, 实际 %q ok=%v", got, ok)
	}
	if _, ok := NarrowerProfile(ProfileAnalysis, ProfileCoding); ok {
		t.Error("analysis 与 coding 不可比, 必须返回 false")
	}
}

// ---------------------------------------------------------------------------
// 校验 / nil 安全
// ---------------------------------------------------------------------------

func TestValidate_捕获自相矛盾与不可识别取值(t *testing.T) {
	cases := map[string]*ConstraintSet{
		"档位不可识别":        NewConstraintSet("a").WithToolProfile("codingg"),
		"权限不可识别":        NewConstraintSet("b").WithPermission("god-mode"),
		"Blocking 不可识别": {Origin: "c", Blocking: "maybe"},
		"白黑名单打架":        NewConstraintSet("d").WithToolAllowList([]string{"Read", "Shell"}).WithDeniedTools("Shell"),
		"模型区间反了":        {Origin: "e", ModelTier: &ModelBound{MinTier: ModelTierLarge, MaxTier: ModelTierSmall}},
	}
	for name, cs := range cases {
		if err := cs.Validate(); err == nil {
			t.Errorf("%s: 应校验失败", name)
		}
	}
	if err := NewConstraintSet("ok").WithToolProfile(ProfileTeam).
		WithPermission(string(types.PermissionModePlan)).WithBlocking(BlockingFailOpen).Validate(); err != nil {
		t.Errorf("合法约束不应报错: %v", err)
	}
}

func TestNarrow_不合法约束不参与合成(t *testing.T) {
	// 校验失败必须挡在合成之前, 否则一个写错档位名的声明会被当成"没声明"而静默继承。
	if _, err := NewConstraintSet("p").Narrow(NewConstraintSet("c").WithToolProfile("codin")); err == nil {
		t.Fatal("子约束档位名不合法, 必须拒绝合成")
	}
}

func TestConstraintSet_nil接收者安全(t *testing.T) {
	var cs *ConstraintSet
	if cs.CompiledToolAllow() != nil || cs.CompiledToolDeny() != nil {
		t.Error("nil 约束不应编译出任何名单")
	}
	if cs.NarrowedPermissionMode("plan") != "" || cs.ConstraintOrigin() != "" || cs.ToolProfile() != "" {
		t.Error("nil 约束的读方法应返回零值")
	}
	if err := cs.Validate(); err != nil {
		t.Errorf("nil 约束校验应通过: %v", err)
	}
	// 没有父约束是今天生产上的常态, 必须能直接 Narrow。
	got, err := cs.Narrow(NewConstraintSet("c").WithToolProfile(ProfileCoding))
	if err != nil || got.ToolProfile() != ProfileCoding {
		t.Errorf("nil 父约束下子约束应原样成立: %+v err=%v", got, err)
	}
	if p, src := cs.ResolveToolProfile(ProfileSourceRoleNameFallback, func() string { return ProfileChat }); p != ProfileChat || src != ProfileSourceRoleNameFallback {
		t.Errorf("nil 约束仍应能走回退: %q %q", p, src)
	}
}

// ---------------------------------------------------------------------------
// 单一真源: 特权位表必须与真实注册表一致
// ---------------------------------------------------------------------------

// toolNameCaps 真实工具名 → 特权位。这是本测试唯一的"人工事实", 粒度比档位表细
// 一级; 档位表 (profileCaps) 则必须能从它 + 真实注册内容推导出来。
var toolNameCaps = map[string]Capability{
	"Read": CapReadFS, "Glob": CapReadFS, "Grep": CapReadFS, "LSP": CapReadFS,
	"Write": CapWriteFS, "StrReplace": CapWriteFS,
	"EnterWorktree": CapWriteFS, "ExitWorktree": CapWriteFS,
	"Shell": CapExec, "TaskOutput": CapExec, "TaskStop": CapExec,
	"WebFetch": CapNet, "WebSearch": CapNet, "FetchKLine": CapNet, "FetchQuote": CapNet,
	"code_intel_query": CapReadIndex, "code_intel_status": CapReadIndex, "code_intel_branch": CapReadIndex,
	"code_intel_init": CapWriteIndex, "code_intel_update": CapWriteIndex,
	"EnterPlanMode": CapPlan, "ExitPlanMode": CapPlan,
	"TodoWrite": CapTask, "TaskCreate": CapTask, "TaskGet": CapTask, "TaskUpdate": CapTask, "TaskList": CapTask,
	"AskUserQuestion": CapInteract,
	"GenerateImage":   CapMedia, "GenerateGIF": CapMedia, "GenerateVideo": CapMedia,
	"GeneratePPTX": CapMedia, "GenerateChart": CapMedia, "GenerateSpeech": CapMedia,
	"TeamCreate": CapTeam, "TeamDelete": CapTeam, "TeamMailbox": CapTeam,
	"Config": CapAdminExtra, "Skill": CapAdminExtra, "ComputerUseStatus": CapAdminExtra,
	"CronCreate": CapAdminExtra, "CronDelete": CapAdminExtra, "CronList": CapAdminExtra,
	// 不扩大能力边界的输出/检索成型类工具, 不占特权位 (见 Capability 注释)。
	"StructuredOutput": 0, "ToolSearch": 0,
}

func realProfileTools(t *testing.T, profile builtin.ToolProfile) []string {
	t.Helper()
	reg := tool.NewRegistry()
	builtin.RegisterProfileToolsWithStore(reg, nil, nil, profile)
	return reg.Names()
}

func TestProfileCaps_每个工具都已分类(t *testing.T) {
	// 往任何档位加新工具而忘了分类, 这里就会红 —— 强制新工具必须明确它的特权维度,
	// 否则档位偏序 (Narrow 的判据) 会静默失真。
	for _, p := range []builtin.ToolProfile{
		builtin.ToolProfileChat, builtin.ToolProfileAnalysis, builtin.ToolProfileTeam,
		builtin.ToolProfileResearch, builtin.ToolProfileCoding, builtin.ToolProfileAdmin,
	} {
		for _, name := range realProfileTools(t, p) {
			if _, ok := toolNameCaps[name]; !ok {
				t.Errorf("档位 %s 里的工具 %q 未在 toolNameCaps 分类; "+
					"请给它定特权维度并同步 profileCaps", p, name)
			}
		}
	}
}

func TestProfileCaps_与真实注册表双向一致(t *testing.T) {
	profiles := []builtin.ToolProfile{
		builtin.ToolProfileChat, builtin.ToolProfileAnalysis, builtin.ToolProfileTeam,
		builtin.ToolProfileResearch, builtin.ToolProfileCoding, builtin.ToolProfileAdmin,
	}
	// 每个档位的"特权工具集" (剔除不占位的成型类工具)。
	privileged := map[builtin.ToolProfile]map[string]bool{}
	for _, p := range profiles {
		set := map[string]bool{}
		for _, name := range realProfileTools(t, p) {
			if toolNameCaps[name] != 0 {
				set[name] = true
			}
		}
		privileged[p] = set
	}

	subset := func(a, b map[string]bool) bool {
		for n := range a {
			if !b[n] {
				return false
			}
		}
		return true
	}

	// 双向一致: 「特权位表说 child 可收窄自 parent」⟺「真实特权工具集 child ⊆ parent」。
	// 少一个方向就只能证明表不冒进 / 表不保守其中一半。
	for _, child := range profiles {
		for _, parent := range profiles {
			byTable := ProfileNarrows(string(child), string(parent))
			byReal := subset(privileged[child], privileged[parent])
			if byTable != byReal {
				t.Errorf("%s ⊆ %s: 特权位表说 %v, 真实注册表说 %v —— profileCaps 与 "+
					"pkg/tool/builtin/profile.go 漂移了", child, parent, byTable, byReal)
			}
		}
	}
	// 顺带钉住 admin 是顶元素这一历史行为 (admin = 全部内置工具)。
	for _, p := range profiles {
		if !subset(privileged[p], privileged[builtin.ToolProfileAdmin]) {
			t.Errorf("admin 应是 %s 的超集", p)
		}
	}
}

func TestProfileCaps_档位名与builtin常量逐字一致(t *testing.T) {
	// 两处字符串必须相同, 否则宿主传进来的档位名会全部"不可识别"→ Narrow 全拒。
	pairs := map[string]builtin.ToolProfile{
		ProfileChat: builtin.ToolProfileChat, ProfileResearch: builtin.ToolProfileResearch,
		ProfileCoding: builtin.ToolProfileCoding, ProfileTeam: builtin.ToolProfileTeam,
		ProfileAnalysis: builtin.ToolProfileAnalysis, ProfileAdmin: builtin.ToolProfileAdmin,
	}
	if len(pairs) != len(profileCaps) {
		t.Fatalf("档位数量不一致: 本包 %d, 对照 %d", len(profileCaps), len(pairs))
	}
	for mine, theirs := range pairs {
		if mine != string(theirs) {
			t.Errorf("档位名不一致: %q vs %q", mine, theirs)
		}
	}
}
