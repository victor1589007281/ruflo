package assembly

import (
	"encoding/json"
	"strings"
	"testing"
)

// baseReport 造一份"一切正常"的装配自报, 供各用例在其上制造单点差异。
func baseReport() *Report {
	return &Report{
		SchemaVersion: SchemaVersion,
		RunID:         "run-t1",
		ConfigSource:  &ConfigSource{Path: "/etc/claude-go/config.json", Origin: "container /etc/claude-go/config.json", Found: true},
		Model:         &ModelInfo{Primary: "dashscope:qwen3.6-plus"},
		Budget:        &BudgetInfo{MaxTurns: 40, MaxTokens: 16384},
		Tools: &ToolsInfo{
			Registered:   10,
			Visible:      []string{"bash", "read", "sql_exec"},
			Hidden:       []string{"write"},
			Whitelist:    []string{"bash", "read", "sql_exec"},
			WhitelistSet: true,
		},
		InternalHooks: map[string][]string{"PreTurn": {"MemoryInject"}, "PostCompact": {}},
		ExternalHooks: []ExternalHook{{Event: "PreToolUse", Type: "command", Index: 0}},
		Policy:        &PolicyInfo{PermissionMode: "bypass"},
		Components: map[string]bool{
			"error_classifier": true, "json_repair": true,
			"loop_detector": true, "prompt_cache": true,
		},
	}
}

// D0.7 的验收原文: "人为制造一处'声明了但未生效', 对账必须报出并指到具体项"。
// 这里制造的正是方案 20.6.2 记录的那个真实缺陷 —— 声明要 prompt_cache, 实际没装上。
func TestReconcileReportsDeclaredNotActual(t *testing.T) {
	actual := baseReport()
	actual.Components["prompt_cache"] = false

	decl := &Declaration{Components: []string{"error_classifier", "json_repair", "loop_detector", "prompt_cache"}}
	diffs := Reconcile(decl, actual)

	if len(diffs) != 1 {
		t.Fatalf("应恰好报出 1 处差异, got %d: %+v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Kind != DiffDeclaredNotActual {
		t.Errorf("差异种类应为 declared_not_actual, got %s", d.Kind)
	}
	if d.Facet != "components" || d.Item != "prompt_cache" {
		t.Errorf("必须指到具体项 components/prompt_cache, got %s/%s", d.Facet, d.Item)
	}
}

// 只报"有一个组件没生效"而说不出是哪一个, 面板上仍然要人去翻 —— 这条用例钉住
// "能指到具体项"这个要求本身。
func TestReconcileNamesTheSpecificTool(t *testing.T) {
	actual := baseReport()
	// sql_exec 被黑名单拉黑 → 不在可见集里。
	actual.Tools.Visible = []string{"bash", "read"}
	actual.Tools.Hidden = []string{"sql_exec", "write"}
	actual.Tools.Blacklist = []string{"sql_exec"}

	decl := &Declaration{Tools: []string{"bash", "read", "sql_exec"}}
	diffs := Reconcile(decl, actual)

	if len(diffs) != 1 {
		t.Fatalf("应恰好 1 处差异 (write 被白名单挡住不算差异, 因为没声明它), got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].Item != "sql_exec" {
		t.Fatalf("应指到 sql_exec, got %s", diffs[0].Item)
	}
	// 成因必须分得开: 被拉黑 ≠ 不在白名单。混成一句"不可见", 读的人得回去翻两份名单。
	if !strings.Contains(diffs[0].Actual, "黑名单") {
		t.Errorf("成因应说明是被黑名单拉黑, got %q", diffs[0].Actual)
	}
}

func TestReconcileHiddenReasonDistinguishesCauses(t *testing.T) {
	whitelisted := &ToolsInfo{WhitelistSet: true, Whitelist: []string{"bash"}}
	if got := hiddenReason(whitelisted, "sql_exec"); !strings.Contains(got, "白名单") {
		t.Errorf("白名单外应报'不在生效白名单内', got %q", got)
	}
	blacklisted := &ToolsInfo{Blacklist: []string{"sql_exec"}}
	if got := hiddenReason(blacklisted, "sql_exec"); !strings.Contains(got, "黑名单") {
		t.Errorf("黑名单内应报'被黑名单拉黑', got %q", got)
	}
	unregistered := &ToolsInfo{}
	if got := hiddenReason(unregistered, "ghost"); !strings.Contains(got, "未注册") {
		t.Errorf("两者都不沾应报'未注册', got %q", got)
	}
}

func TestReconcileReportsActualNotDeclared(t *testing.T) {
	actual := baseReport()
	actual.Tools.Visible = []string{"bash", "read", "sql_exec", "extra_tool"}

	decl := &Declaration{Tools: []string{"bash", "read"}}
	diffs := Reconcile(decl, actual)

	var found *Diff
	for i := range diffs {
		if diffs[i].Item == "extra_tool" {
			found = &diffs[i]
		}
	}
	if found == nil {
		t.Fatalf("生效了却没声明也要报 (意味着装配路径上有声明之外的来源), got %+v", diffs)
	}
	if found.Kind != DiffActualNotDeclared {
		t.Errorf("种类应为 actual_not_declared, got %s", found.Kind)
	}
}

func TestReconcileReportsMismatch(t *testing.T) {
	actual := baseReport()
	actual.Model.Primary = "dashscope:qwen3.6-plus"

	decl := &Declaration{Model: "dashscope:qwen3.5-plus"}
	diffs := Reconcile(decl, actual)

	if len(diffs) != 1 || diffs[0].Kind != DiffMismatch {
		t.Fatalf("模型不一致应报一处 mismatch, got %+v", diffs)
	}
	if diffs[0].Declared != "dashscope:qwen3.5-plus" || diffs[0].Actual != "dashscope:qwen3.6-plus" {
		t.Errorf("两侧取值都要报出来, got %q vs %q", diffs[0].Declared, diffs[0].Actual)
	}
}

// 未声明 ≠ 声明了空。nil 白名单是不参与对账, 空切片是"应当拒绝一切" ——
// 把这两者混为一谈, 一次"没配白名单"会被报成"白名单写错了"。
func TestReconcileNilDeclarationIsNotAnEmptyDeclaration(t *testing.T) {
	actual := baseReport()
	actual.Tools.Visible = nil
	actual.Tools.Hidden = []string{"bash", "read", "sql_exec", "write"}

	if diffs := Reconcile(&Declaration{}, actual); len(diffs) != 0 {
		t.Fatalf("未声明白名单时不应产生任何工具面差异, got %+v", diffs)
	}
	// 显式声明白名单而实际一个都不可见 —— 这正是 fail-closed 的事故现场, 必须全报。
	diffs := Reconcile(&Declaration{Tools: []string{}}, actual)
	if len(diffs) != 0 {
		t.Fatalf("空白名单声明本身不含具体工具名, 不产生逐项差异, got %+v", diffs)
	}
	diffs = Reconcile(&Declaration{Tools: []string{"bash", "read"}}, actual)
	if len(diffs) != 2 {
		t.Fatalf("显式声明的两项都不可见, 应各报一处, got %d: %+v", len(diffs), diffs)
	}
}

// 对账结果必须排序: map 遍历顺序会让同一份装配每次报出不同顺序, 人看两次会以为变了。
func TestReconcileIsDeterministic(t *testing.T) {
	actual := baseReport()
	actual.Tools.Visible = nil
	actual.Tools.Hidden = []string{"a", "b", "c", "d", "e", "f"}
	decl := &Declaration{Tools: []string{"a", "b", "c", "d", "e", "f"}}

	first := Reconcile(decl, actual)
	firstJSON, _ := json.Marshal(first)
	for i := 0; i < 50; i++ {
		again, _ := json.Marshal(Reconcile(decl, actual))
		if string(again) != string(firstJSON) {
			t.Fatalf("第 %d 次对账结果与首次不同:\n%s\n%s", i, firstJSON, again)
		}
	}
}

// fingerprint 必须只认装配实质: 把每次都变的字段 (run/ticket/实例) 算进去的话,
// 它会恒变, 面板上看起来"每次装配都不一样"而实际相同 —— 这种噪声会让指标失效。
func TestFingerprintIgnoresPerRunFields(t *testing.T) {
	r := baseReport()
	base := r.Fingerprint()
	if base == "" {
		t.Fatal("fingerprint 不应为空")
	}

	r.RunID = "run-other"
	r.SessionID = "sess-xyz"
	r.TicketID = "T-2026-0919"
	r.InstanceID = "db-prod-7"
	r.ConfigSource = &ConfigSource{Path: "/another/path.json", Origin: "home ~/.claude-go/config.json", Found: true}
	r.Workspace = &WorkspaceInfo{Cwd: "/tmp/x"}
	r.Prompt = &PromptInfo{Total: 12345}
	r.Warnings = []string{"某条提醒"}

	if got := r.Fingerprint(); got != base {
		t.Fatalf("每次运行都变的字段不该影响 fingerprint: %s -> %s", base, got)
	}
}

func TestFingerprintChangesOnAssemblySubstance(t *testing.T) {
	r := baseReport()
	base := r.Fingerprint()

	r.Components["prompt_cache"] = false
	afterComponent := r.Fingerprint()
	if afterComponent == base {
		t.Fatal("组件装配变化必须改变 fingerprint")
	}

	r2 := baseReport()
	r2.Tools.Visible = []string{"bash"}
	if r2.Fingerprint() == base {
		t.Fatal("工具面变化必须改变 fingerprint")
	}

	r3 := baseReport()
	r3.InternalHooks["PreTurn"] = []string{"MemoryInject", "PromptCache"}
	if r3.Fingerprint() == base {
		t.Fatal("内置 hook 链变化必须改变 fingerprint")
	}
}

func TestSummaryAlwaysSaysSomething(t *testing.T) {
	if got := Summary(nil); got == "" {
		t.Fatal("零差异也要给一句话 —— 空串在面板上与'还没对账'长得一样")
	} else if !strings.Contains(got, "一致") {
		t.Errorf("零差异应报'一致', got %q", got)
	}

	decl := &Declaration{Tools: []string{"ghost"}, Model: "m1"}
	actual := baseReport()
	got := Summary(Reconcile(decl, actual))
	if !strings.Contains(got, "声明未生效") {
		t.Errorf("有差异时结论要点出种类, got %q", got)
	}
}

// 对账没有"实际"一侧时不能编结论: 采集失败 (actual 为 nil) 与"装配为空"是两回事。
func TestReconcileWithNilActualReportsNothing(t *testing.T) {
	if diffs := Reconcile(&Declaration{Tools: []string{"bash"}}, nil); len(diffs) != 0 {
		t.Fatalf("没有实际侧就没有可比之物, 不应凭空报差异, got %+v", diffs)
	}
	// 但反之, "实际侧在、工具面没采到"必须说出来 —— 那才是采集失败, 不能静默当一致。
	actual := baseReport()
	actual.Tools = nil
	diffs := Reconcile(&Declaration{Tools: []string{"bash"}}, actual)
	if len(diffs) != 1 || !strings.Contains(diffs[0].Actual, "未采集") {
		t.Fatalf("工具面缺失应显式报'未采集', got %+v", diffs)
	}
}

// 对账结论打包后必须能被"确定地"读出来: diffs 为空数组而不是 null ——
// null 在消费侧与"这个字段没有这个语义"难以区分, 而"对过账且一致"是个确定结论。
func TestReconcileResultEmptyDiffsIsArrayNotNull(t *testing.T) {
	res := NewReconcileResult(&Declaration{}, baseReport())
	if res.Diffs == nil {
		t.Fatal("零差异必须给空数组而非 nil")
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"diffs":[]`) {
		t.Fatalf("序列化后应为 diffs:[], got %s", b)
	}
	if res.Summary == "" {
		t.Fatal("结论不能为空 —— 空串在面板上与'还没对账'长得一样")
	}
	// 声明必须同行: 同一份实际装配对着不同声明会得出不同差异, 只给差异读不出相对性。
	if res.Declared == nil {
		t.Fatal("声明必须随结论一起给, 否则读的人分不清'零差异'是因为对上了还是因为没声明")
	}
}

func TestReconcileExternalHooks(t *testing.T) {
	actual := baseReport()
	decl := &Declaration{ExternalHooks: []ExternalHook{
		{Event: "PreToolUse", Type: "command", Index: 0},
		{Event: "PostCompact", Type: "http", Index: 0},
	}}
	diffs := Reconcile(decl, actual)

	// 声明了 2 条: 一条在 (类型一致), 一条没装载。
	if len(diffs) != 1 {
		t.Fatalf("应报一处 (PostCompact 未装载), got %+v", diffs)
	}
	if diffs[0].Item != "PostCompact#0" || !strings.Contains(diffs[0].Actual, "未装载") {
		t.Errorf("应指到 PostCompact#0 且说明未装载, got %+v", diffs[0])
	}
}

// 白名单里的名字放不进任何工具时, 报错必须指向**真正要改的地方**。
//
// 两种成因的修法完全不同:
//   - 在名单里却不可见 ⇒ 名字写错了 / 工具没注册到这条路径 → 改名字或注册
//   - 不在名单里       ⇒ 这个岗位本就不该有它               → 改名单
// 把两者都说成"不在生效白名单内", 看的人会去改一个本来就对的地方, 然后
// "改完了还是红的" —— 那会让整个对账面板失去可信度。
func TestHiddenReasonDistinguishesGhostFromUnlisted(t *testing.T) {
	tools := &ToolsInfo{
		Registered:   3,
		Visible:      []string{"Bash"},
		Hidden:       []string{"Read"},
		WhitelistSet: true,
		Whitelist:    []string{"Bash", "Read", "ghost"},
	}

	// 在名单里, 但注册表里没有 —— 要说清"名字本身有问题"。
	if got := hiddenReason(tools, "ghost"); !strings.Contains(got, "注册表") {
		t.Errorf("白名单里的幽灵工具应指向注册表, got %q", got)
	}
	// 不在名单里 —— 这才是"不在生效白名单内"。
	if got := hiddenReason(tools, "Write"); !strings.Contains(got, "不在生效白名单内") {
		t.Errorf("未列入名单的工具应报不在白名单内, got %q", got)
	}
	// 黑名单优先: 一个工具既可能不在白名单又被拉黑, 拉黑是更具体的原因。
	bl := &ToolsInfo{Blacklist: []string{"danger"}, WhitelistSet: true, Whitelist: []string{"danger"}}
	if got := hiddenReason(bl, "danger"); !strings.Contains(got, "黑名单") {
		t.Errorf("被拉黑应报黑名单, got %q", got)
	}
}

// ============================================================================
// 黑名单对账 (21.8.12 P2′)
// ============================================================================
//
// 这一面与白名单那一面 (reconcileTools) 的**问题形态不同**, 所以判据也不同:
//
//	白名单查的是"少给了" —— 声明要给却没给。实际侧有逐工具的可见集, 一比就出来。
//	黑名单查的是"给了但你以为没给" —— 一份没接上的黑名单, 在装配读数上与"接上了
//	且恰好没有那个工具"完全一样, 而作者以为自己关掉了它。这就是下面两条要劈开的。

// TestReconcileDisallowedReportsVisibleTool 第一关: 声明要挡的工具**仍然可见**。
//
// 这是最直接的一种失败, 也是唯一一种"用户能自己看出来"的 —— 模型手上就有那个工具。
func TestReconcileDisallowedReportsVisibleTool(t *testing.T) {
	actual := baseReport()
	// 没有白名单: 除了被拉黑的 write, 其余全部可见。
	actual.Tools.WhitelistSet = false
	actual.Tools.Whitelist = nil
	actual.Tools.Visible = []string{"bash", "read", "sql_exec"}
	actual.Tools.Blacklist = []string{"write"}

	decl := &Declaration{DisallowedTools: []string{"sql_exec"}}
	diffs := Reconcile(decl, actual)

	if len(diffs) != 1 {
		t.Fatalf("应恰好报出 1 处差异, got %d: %+v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Kind != DiffDeclaredNotActual || d.Facet != "disallowed_tools" || d.Item != "sql_exec" {
		t.Errorf("必须报成 disallowed_tools/sql_exec 的 declared_not_actual, got %s %s/%s", d.Kind, d.Facet, d.Item)
	}
	if !strings.Contains(d.Actual, "可见") {
		t.Errorf("实际侧要说明它仍然可见 (否则看的人不知道是哪一种失败): %q", d.Actual)
	}
}

// TestReconcileDisallowedReportsUnwiredDeclaration 第二关, 也是本项存在的**全部理由**。
//
// 构造的是那个静默形态: 声明要挡 Bash, 而 Bash **恰好本来就不在可见集里** (本档位
// 没注册它 / 被白名单挡了)。于是"生效了没有"从可见性上**完全看不出来** ——
// 第一关过, 但生效黑名单里根本没有 Bash ⇒ 这条声明没有任何东西在读它。
//
// 缺了这条判据, 一个把 WithDeniedTools 那行删掉的改动不会有任何测试变红 ——
// 而线上表现是"黑名单看起来配上了、报告一路绿、实际一个都没挡"。
func TestReconcileDisallowedReportsUnwiredDeclaration(t *testing.T) {
	actual := baseReport()
	actual.Tools.WhitelistSet = false
	actual.Tools.Whitelist = nil
	// Bash 不可见 —— 但**不是因为**这条声明: 生效黑名单里没有它。
	actual.Tools.Visible = []string{"read", "sql_exec"}
	actual.Tools.Hidden = []string{"bash"}
	actual.Tools.Blacklist = nil

	decl := &Declaration{DisallowedTools: []string{"bash"}}
	diffs := Reconcile(decl, actual)

	if len(diffs) != 1 {
		t.Fatalf("声明没走到生效黑名单必须报出来, got %d: %+v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Kind != DiffDeclaredNotActual || d.Item != "bash" {
		t.Errorf("应报 bash 的 declared_not_actual, got %s %s", d.Kind, d.Item)
	}
	if !strings.Contains(d.Actual, "未进入生效黑名单") {
		t.Errorf("要指出真因是**接线断了**, 而不是笼统的「不可见」—— "+
			"后者会让人去查白名单/注册表, 那两处本来就是对的: %q", d.Actual)
	}
}

// TestReconcileDisallowedSilentOnCorrectAndRedundant 是上面那条的阴性对照。
//
// `disallowed-tools: [Bash]` + `tool-profile: analysis` 这个组合里, Bash 本就不该
// 出现。若把它报成问题, 看的人会去删掉一行**本来就对**的配置 —— 而"改完了还是红的"
// 会让整个对账面板失去可信度。
//
// 它进了生效黑名单 (接线照走, 与注册与否无关), 所以两关都过、零差异。
func TestReconcileDisallowedSilentOnCorrectAndRedundant(t *testing.T) {
	actual := baseReport()
	actual.Tools.WhitelistSet = false
	actual.Tools.Whitelist = nil
	actual.Tools.Visible = []string{"read", "sql_exec"}
	actual.Tools.Hidden = []string{"bash"}
	actual.Tools.Blacklist = []string{"bash"} // ← 声明走到了生效黑名单

	decl := &Declaration{DisallowedTools: []string{"bash"}}
	if diffs := Reconcile(decl, actual); len(diffs) != 0 {
		t.Errorf("正确且冗余的黑名单声明不得报差异 (否则会让人去改一行本来就对的配置): %+v", diffs)
	}
}

// TestReconcileDisallowedDoesNotReportActualNotDeclared 钉住一个**刻意的反方向缺席**。
//
// 生效黑名单是**汇总值** —— 团队约束、岗位约束、CLI 旗标都会往里汇, 而 Declaration
// 只带得动"资产自己声明"的那一份。照汇总值报差异, 得到的是"调用方还有别的约束"
// 这句话, 每个嵌套子代理都会报一遍, 而它不是缺陷。
//
// 这条断言的是"不要报", 所以它天然是个容易在重构中被"顺手补全"的地方 —— 写下来。
func TestReconcileDisallowedDoesNotReportActualNotDeclared(t *testing.T) {
	actual := baseReport()
	actual.Tools.WhitelistSet = false
	actual.Tools.Whitelist = nil
	actual.Tools.Visible = []string{"read"}
	actual.Tools.Hidden = []string{"bash", "write"}
	// 生效黑名单里有 write, 而本声明只说了 bash —— write 来自调用方的约束链。
	actual.Tools.Blacklist = []string{"bash", "write"}

	decl := &Declaration{DisallowedTools: []string{"bash"}}
	if diffs := Reconcile(decl, actual); len(diffs) != 0 {
		t.Errorf("生效黑名单里的**额外**项来自调用方约束链, 不是本声明的缺陷, 不得报: %+v", diffs)
	}
}

// TestReconcileDisallowedNilIsNotAnEmptyBlacklist 与白名单那条同源。
//
// nil = 没声明这一面 ⇒ 不参与对账; 非 nil 空切片 = 声明了一份空黑名单 (要求什么都不禁)
// ⇒ 参与对账 (且必然零差异)。把两者混起来会让每一份没写 disallowed-tools 的资产
// 都背上一次无意义的比对。
func TestReconcileDisallowedNilIsNotAnEmptyBlacklist(t *testing.T) {
	actual := baseReport()
	actual.Tools.Blacklist = []string{"write"}

	if diffs := Reconcile(&Declaration{}, actual); len(diffs) != 0 {
		t.Errorf("没声明黑名单时不得参与对账, got %+v", diffs)
	}
	if diffs := Reconcile(&Declaration{DisallowedTools: []string{}}, actual); len(diffs) != 0 {
		t.Errorf("空白名单声明是合法的零差异, got %+v", diffs)
	}
}

// TestReconcileDisallowedWithoutToolsInfoIsReported 与白名单那条对称:
// 采集不到工具面时, 沉默会被读成"没问题"。
func TestReconcileDisallowedWithoutToolsInfoIsReported(t *testing.T) {
	actual := baseReport()
	actual.Tools = nil

	diffs := Reconcile(&Declaration{DisallowedTools: []string{"bash"}}, actual)
	if len(diffs) != 1 || diffs[0].Item != "*" {
		t.Fatalf("采集不到工具面必须报出来, got %+v", diffs)
	}
}
