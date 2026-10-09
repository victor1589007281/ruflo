package agentbind

// 组装器的验收 (21.8.12 P2)。
//
// 本文件的每一条都在钉同一件事: **读出来的东西必须能分辨"接上了"与"没接上"**。
// 组装器最容易写成的那种样子是"解析成功就返回成功" —— 而一份解析成功却没人读的
// 声明, 与一行都没写在下游完全一样。所以这里的断言不看"有没有解析出东西", 只看
// Status 是哪一档、Consumer 指不指得出地方。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// fakePool 一个只有三个技能的假池 —— 不必起真的技能注册表 (那个要扫盘,
// 于是"组装器对池的说法对不对"会被磁盘状态绑架)。
type fakePool map[string]bool

func (p fakePool) HasSkill(name string) bool { return p[name] }

// install 写一份 agent 资产进 state 目录并装载索引。
func install(t *testing.T, name, fm, body string) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), ".claude-go")
	dir := filepath.Join(stateDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + fm + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := agent.AgentCatalog()
	agent.SetAgentCatalog(asset.LoadStandard(asset.LoadRoots{State: stateDir}))
	t.Cleanup(func() { agent.SetAgentCatalog(prev) })
}

func bindingOf(t *testing.T, b *AssetBinding, facet string) Binding {
	t.Helper()
	for _, bd := range b.Bindings {
		if bd.Facet == facet {
			return bd
		}
	}
	t.Fatalf("读数里没有 %q 这一面 (共 %d 面) —— 一条没被报出来的绑定等于没有验证过它", facet, len(b.Bindings))
	return Binding{}
}

// Test档位拼错不作废成chat 是本文件最要紧的一条。
//
// builtin.NormalizeToolProfile 对不认识的串**恒返回 chat** 且不留痕, 于是
// `tool-profile: codign` 的表现是"这个子代理拿到了最窄的档位" —— 一次没有任何
// 报错的权限收窄, 从外面看与"这个角色本来就不需要文件工具"完全一样。
// 组装器必须比它更早地把这件事拦住, 且**不能**把非法值当成一个合法档位。
func Test档位拼错不作废成chat(t *testing.T) {
	install(t, "typo", "name: typo\ntool-profile: codign", "正文")
	b := BindAgentAsset("typo", nil)

	if b.HasToolProfile {
		t.Fatalf("非法档位不得被当作有效声明, got %q", b.ToolProfile)
	}
	if b.ToolProfile == builtin.ToolProfileChat {
		t.Fatal("非法档位被静默掰成了 chat —— 这正是要防的那次权限变更")
	}
	bd := bindingOf(t, b, "tool-profile")
	if bd.Status != BindingInvalidValue {
		t.Fatalf("非法档位必须报 invalid_value, got %q", bd.Status)
	}
	if len(b.Warnings()) == 0 {
		t.Fatal("非法档位必须留痕 (降级可见), Warnings 却是空的")
	}

	// 阳性对照: 同一个拼写位置写对了, 必须走 wired 那一档。
	// 缺了它, 上面"不是 chat"无法区分"校验生效"与"这一面根本没被处理"。
	install(t, "ok", "name: ok\ntool-profile: analysis", "正文")
	b2 := BindAgentAsset("ok", nil)
	if !b2.HasToolProfile || b2.ToolProfile != builtin.ToolProfileAnalysis {
		t.Fatalf("合法档位应原样生效, got (%q,%v)", b2.ToolProfile, b2.HasToolProfile)
	}
	if bindingOf(t, b2, "tool-profile").Status != BindingWired {
		t.Fatal("合法档位必须报 wired")
	}
}

// Test每条接上的绑定都点名消费方 是"声明了但没人读"的结构性防线。
//
// 一份读数值如果只说"我解析出来了", 看的人无从判断它是否通电。接线被删掉之后
// 这条读数会**一字不变** —— 那正是本仓最贵的缺陷形态。
func Test每条接上的绑定都点名消费方(t *testing.T) {
	install(t, "full", `name: full
tool-profile: analysis
permission-mode: plan
max-turns: 7
model: claude-sonnet-5
skills: [alpha, beta]
tools: [Read, Grep]
hooks:
  - SessionStart:command:./boot.sh
  - PreToolUse:command:./guard.sh:Bash*`, "正文")

	b := BindAgentAsset("full", fakePool{"alpha": true, "beta": true})
	if b.Asset == nil || !b.Active {
		t.Fatalf("资产应装载成功, got %+v", b)
	}
	for _, facet := range []string{"tool-profile", "permission-mode", "max-turns", "model", "skills", "tools", "hooks"} {
		bd := bindingOf(t, b, facet)
		if bd.Status != BindingWired {
			t.Errorf("%s 应报 wired, got %q (%s)", facet, bd.Status, bd.Note)
			continue
		}
		if bd.Consumer == "" {
			t.Errorf("%s 报了 wired 却没点名消费方 —— 看的人无从判断它是否通电", facet)
		}
	}
	// 声明值必须原样带出来, 不能只报一个"成功"。
	if b.MaxTurns != 7 || b.Model != "claude-sonnet-5" || b.PermissionMode != types.PermissionModePlan {
		t.Errorf("声明值没有原样带出: turns=%d model=%q mode=%q", b.MaxTurns, b.Model, b.PermissionMode)
	}
	if len(b.Hooks) != 2 || b.Hooks[0].Event != types.HookEventSessionStart {
		t.Errorf("hook 没解析出来: %+v", b.Hooks)
	}
	// if 匹配器是第 4 段, 不能吃掉命令路径。
	if b.Hooks[1].Command != "./guard.sh" || b.Hooks[1].If != "Bash*" {
		t.Errorf("hook 的 if 匹配器/命令切分错了: %+v", b.Hooks[1])
	}
	if len(b.Warnings()) != 0 {
		t.Errorf("这一份装配应当干净, 却报了: %v", b.Warnings())
	}
}

// Test技能查不到要点名 钉住"部分命中"这个最难发现的形态。
//
// 三条技能里两条在池里、一条不在 → 装配看起来成功了 (清单非空、模型看得到东西),
// 缺的那一条要靠逐条比对才看得出来。所以状态必须是 unknown_name **且** Resolved
// 里如实给出命中的那几条, 两者缺一都会让人误判。
func Test技能查不到要点名(t *testing.T) {
	install(t, "sk", "name: sk\nskills: [alpha, nope]", "正文")
	b := BindAgentAsset("sk", fakePool{"alpha": true})

	bd := bindingOf(t, b, "skills")
	if bd.Status != BindingUnknownName {
		t.Fatalf("部分命中必须报 unknown_name, got %q", bd.Status)
	}
	if len(bd.Resolved) != 1 || bd.Resolved[0] != "alpha" {
		t.Errorf("命中的那几条要如实给出, got %v", bd.Resolved)
	}
	if len(b.Skills) != 1 || b.Skills[0] != "alpha" {
		t.Errorf("只有池里有的才该进 Skills, got %v", b.Skills)
	}
	if len(b.Warnings()) == 0 {
		t.Fatal("缺的技能必须留痕")
	}

	// 没有池可查**不等于**查了没有值 —— 但两者在"这条技能会不会生效"上是同一个
	// 答案 (不会), 所以状态同样是 unknown_name。区别写在 Note 里。
	b2 := BindAgentAsset("sk", nil)
	if bindingOf(t, b2, "skills").Status != BindingUnknownName {
		t.Fatal("没有池时不得报 wired —— 那会让没传池的调用方以为自己接上了")
	}
}

// Test权限档不认识要作废 与档位那条同源: 拼错的档会一路落到 modeDecision 的
// default 分支 (写入类工具走 Ask), 而在非交互的子代理里那与"全部拒绝"几乎一样。
func Test权限档不认识要作废(t *testing.T) {
	install(t, "pm", "name: pm\npermission-mode: readonly", "正文")
	b := BindAgentAsset("pm", nil)
	if b.HasPermissionMode {
		t.Fatalf("非法权限档不得生效, got %q", b.PermissionMode)
	}
	if bindingOf(t, b, "permission-mode").Status != BindingInvalidValue {
		t.Fatal("非法权限档必须报 invalid_value")
	}
	if len(b.Warnings()) == 0 {
		t.Fatal("非法权限档必须留痕")
	}
}

// Test没有消费方的声明如实报出来 —— 这一条是本仓诚实性的判据。
//
// plugin 在方案里与 hook/skill/tool 并列为四类绑定之一, 而本仓**没有** plugin
// 运行时。接一个空操作上去能让"plugin 已支持"这句话在代码里成立, 却让运行期多
// 一条永远静默失败的路径。所以它必须报 no_consumer 并指名替代路径。
func Test没有消费方的声明如实报出来(t *testing.T) {
	install(t, "pl", "name: pl\nruntime: plugin", "正文")
	b := BindAgentAsset("pl", nil)

	bd := bindingOf(t, b, "plugin")
	if bd.Status != BindingNoConsumer {
		t.Fatalf("本仓没有 plugin 运行时, 必须报 no_consumer, got %q", bd.Status)
	}
	if bd.Consumer != "" {
		t.Errorf("没有消费方就不该填一个消费方, got %q", bd.Consumer)
	}
	if bd.Note == "" {
		t.Error("no_consumer 必须说明该走哪条替代路径, 否则看的人只知道「不行」")
	}
	// 反向: 没写 runtime 的资产不该凭空多出这一面 —— 否则每份资产都报一次假问题。
	install(t, "plain", "name: plain", "正文")
	if len(BindAgentAsset("plain", nil).Warnings()) != 0 {
		t.Errorf("没声明 plugin 的资产不该报 plugin 的问题: %v", BindAgentAsset("plain", nil).Warnings())
	}
}

// Test影子资产一律不装配 与 P5 的清单口径对齐。
//
// P5 的清单把 shadow 排除在外 (否则影子资产会被模型直接调用, 等于绕过整个灰度机制)。
// 查表这一侧若放行, "清单里没有它"与"它能在运行时被激活"就成了两句互相矛盾的话,
// 而这套灰度机制就只剩一句声明。
func Test影子资产一律不装配(t *testing.T) {
	install(t, "shadowed", "name: shadowed\nstatus: shadow\ntool-profile: admin\npermission-mode: bypass", "正文")
	b := BindAgentAsset("shadowed", nil)

	if b.Asset == nil {
		t.Fatal("名字查得到 —— 只是不该被激活, 两者要分开 (留痕时要说清是哪种)")
	}
	if b.Active {
		t.Fatal("shadow 资产不得被判为可装配")
	}
	if b.HasToolProfile || b.HasPermissionMode || len(b.Bindings) != 0 {
		t.Errorf("非 active 资产不得装出任何一面声明, got %+v", b.Bindings)
	}
	if b.AssemblyDeclaration() != nil {
		t.Error("非 active 资产不得产出声明 —— 否则对账会拿一份根本不该生效的声明去比")
	}
	if len(b.Warnings()) == 0 {
		t.Fatal("被 shadow 挡下必须留痕, 否则「我写了它怎么没生效」无法归因")
	}
}

// Test声明的对账面只翻能翻的。
//
// assembly.Declaration 的每一段都必须能对着**实际**比。这里钉住三条:
//
//  1. disallowed-tools **原样**翻进它自己那一段, 而**绝不**被换算成白名单 ——
//     换算要拿实际注册表去减声明, 那是自比自 (见 AssemblyDeclaration 的注释)。
//     断言的是 d.Tools == nil 而**不是** d == nil: 后者会把"黑名单根本没进对账"
//     这个缺陷一起盖过去, 而那正是 P2′ 要修的那个洞。
//  2. 什么都没声明时返回 nil (一份什么都不要的声明与"没有声明可对"是两回事)。
//  3. 显式白名单照翻。
func Test声明的对账面只翻能翻的(t *testing.T) {
	install(t, "only-deny", "name: only-deny\ndisallowed-tools: [Bash]", "正文")
	d0 := BindAgentAsset("only-deny", nil).AssemblyDeclaration()
	if d0 == nil {
		t.Fatal("只声明黑名单也必须产出声明 —— 否则这份黑名单不参与对账, " +
			"「声明要挡却实际挡不住」就没有任何地方答得出来 (P2′ 修的正是这个)")
	}
	if d0.Tools != nil {
		t.Errorf("黑名单不得被换算成白名单: 换算要拿实际注册表去减声明, 是自比自 —— got Tools=%v", d0.Tools)
	}
	if len(d0.DisallowedTools) != 1 || d0.DisallowedTools[0] != "Bash" {
		t.Errorf("黑名单要原样进 DisallowedTools, got %v", d0.DisallowedTools)
	}

	install(t, "plain2", "name: plain2", "正文")
	if d := BindAgentAsset("plain2", nil).AssemblyDeclaration(); d != nil {
		t.Errorf("什么都没声明必须返回 nil (不参与对账), got %+v", d)
	}

	install(t, "has-tools", "name: has-tools\ntools: [Read, Grep]", "正文")
	d := BindAgentAsset("has-tools", nil).AssemblyDeclaration()
	if d == nil || len(d.Tools) != 2 {
		t.Fatalf("显式白名单要翻进声明侧, got %+v", d)
	}

	install(t, "hooked", "name: hooked\nhooks:\n  - SessionStart:command:./b.sh", "正文")
	d2 := BindAgentAsset("hooked", nil).AssemblyDeclaration()
	if d2 == nil || len(d2.ExternalHooks) != 1 || d2.ExternalHooks[0].Index != 0 {
		t.Fatalf("hook 声明要翻进声明侧且序号取数组下标, got %+v", d2)
	}
}

// TestHook事件拼错要点名 —— 外部 hook 最常见的静默失效形态。
//
// 拼错的事件名会让这条 hook **永远不触发**, 而装载、注册、日志一路都是成功的。
// 合法的两条不该陪葬 (整份作废会让作者以为"格式不对", 实际是"有一条没生效")。
func TestHook事件拼错要点名(t *testing.T) {
	install(t, "badhook", `name: badhook
hooks:
  - SessionStart:command:./ok.sh
  - PostTooluse:command:./never.sh
  - PreToolUse:command:./ok2.sh`, "正文")
	b := BindAgentAsset("badhook", nil)

	if len(b.Hooks) != 2 {
		t.Fatalf("合法的两条 hook 应当照常生效, got %+v", b.Hooks)
	}
	var sawInvalid bool
	for _, bd := range b.Bindings {
		if bd.Facet == "hooks" && bd.Status == BindingInvalidValue {
			sawInvalid = true
		}
	}
	if !sawInvalid {
		t.Fatal("拼错的事件名必须报 invalid_value —— 它永远不会触发, 却没有第二个地方看得出来")
	}
	if len(b.Warnings()) == 0 {
		t.Fatal("拼错的事件名必须留痕")
	}
}

// Test嵌套Hooks写法读不出来要说话 —— 本仓的 frontmatter 是扁平的。
//
// Claude Code 的 hooks 是嵌套的 (事件 → 匹配器 → hooks 列表)。pkg/asset 的解析器
// 为了与 pkg/skills 逐字节一致**只认扁平写法**, 嵌套喂进来会塌成一堆认不出的键。
// 静默读成空是最坏的结果 (作者以为配上了), 所以至少要报出来。
func Test嵌套Hooks写法读不出来要说话(t *testing.T) {
	install(t, "nested", `name: nested
hooks:
  PreToolUse:
    - matcher: Bash
      hooks: [{type: command, command: ./x.sh}]`, "正文")
	b := BindAgentAsset("nested", nil)

	if len(b.Hooks) > 0 {
		t.Skipf("本仓解析器已能读嵌套写法 (%+v) —— 这条断言的前提变了, 请重新评估", b.Hooks)
	}
	var msg string
	for _, bd := range b.Bindings {
		if bd.Facet == "hooks" && bd.Status != BindingUnset {
			msg = bd.Note
		}
	}
	if msg == "" {
		t.Fatal("读不出来的 hooks 声明必须报出来 —— 静默读空会让作者以为配上了")
	}
}

// ============================================================================
// P2′ 工具名单 (21.8.12 P2′) —— 黑名单真检查
// ============================================================================

// Test黑名单原样接线进读数 钉住"声明被解析"与"声明被送进运行时"之间的那道缝。
//
// 改造前 bindTools 把两份名单**合并**进一个 Declared 就完事了, 黑白不分 —— 于是
// 调用方拿到的读数里没有任何东西告诉它"这是一份黑名单, 该走 WithDeniedTools"。
// 没有那个字段的后果不是报错, 是**没有接线**: 装配读数一路绿, `disallowed-tools:
// [Bash]` 一个工具都没挡住。
//
// 所以这里断言的是**字段分家** (Tools 与 DisallowedTools 各归各), 而不是"解析成功"。
func Test黑名单原样接线进读数(t *testing.T) {
	install(t, "deny-bash", "name: deny-bash\ndisallowed-tools: [Bash]", "正文")
	b := BindAgentAsset("deny-bash", nil)

	if len(b.Tools) != 0 {
		t.Errorf("只声明黑名单时白名单必须为空 —— 否则调用方 WithToolAllowList 会 fail-closed 地拒绝一切: %v", b.Tools)
	}
	if len(b.DisallowedTools) != 1 || b.DisallowedTools[0] != "Bash" {
		t.Fatalf("黑名单必须原样落进 DisallowedTools (它是调用方唯一能照着接线的字段), got %v", b.DisallowedTools)
	}
	bd := bindingOf(t, b, "tools")
	if bd.Status != BindingWired {
		t.Errorf("Bash 是真工具名, 不得报 invalid_value: %+v", bd)
	}
	if !strings.Contains(bd.Consumer, "WithDeniedTools") {
		t.Errorf("Consumer 必须点出黑名单的消费方 —— 只说 AllowList 会把人指向错的那条线: %q", bd.Consumer)
	}
}

// Test黑名单大小写写错要点名 是上一条的**分辨力**对照, 也是本项的唯一实质检查。
//
// `disallowed-tools: [bash]` 与 `[Bash]` 在运行期之外的一切读数上**完全相同**:
// 都接上、都进生效黑名单、都不报错。区别只在一个挡得住、一个挡不住。而黑名单
// 写错的后果与白名单**相反** —— 白名单写错是太窄 (少给), 黑名单写错是**太宽**
// (该挡的没挡), 而"太宽"在功能测试里永远看不出来。
func Test黑名单大小写写错要点名(t *testing.T) {
	install(t, "deny-lower", "name: deny-lower\ndisallowed-tools: [bash]", "正文")
	b := BindAgentAsset("deny-lower", nil)

	bd := bindingOf(t, b, "tools")
	if bd.Status != BindingInvalidValue {
		t.Errorf("认不出的名字必须报 invalid_value, 否则写错的黑名单静默地什么都不挡: %+v", bd)
	}
	if !strings.Contains(bd.Note, "bash") {
		t.Errorf("要点名是哪一个名字写错了, 否则看的人得回去逐字比: %q", bd.Note)
	}
	// 认不出的名字**仍然接线** —— 见 bindTools 的注释: 本文件是纯装配不改行为,
	// 而目录不全知 (MCP/调用方注册的工具不在里面), 拿它删东西是用"我没编目"
	// 冒充"它不存在"。
	if len(b.DisallowedTools) != 1 || b.DisallowedTools[0] != "bash" {
		t.Errorf("认不出的名字必须原样保留 (剔除会改变生效行为, 而那是调用方的事): %v", b.DisallowedTools)
	}

	// 阴性对照: 写对的名字不得被报成错的 —— 否则这条判据会把每一份正确的
	// 黑名单都染红, 人就不看了。
	install(t, "deny-ok", "name: deny-ok\ndisallowed-tools: [Bash, Read]", "正文")
	if bd := bindingOf(t, BindAgentAsset("deny-ok", nil), "tools"); bd.Status != BindingWired {
		t.Errorf("正确的黑名单不得被报成问题: %+v", bd)
	}
}

// Test黑名单在analysis档下不误报 钉住"为什么判拼写要用静态目录而不是实际注册表"。
//
// analysis 档下 Bash **从未被注册**。若拿"实际注册表里有没有"当判据, 这个**正确
// 且冗余**的声明 (作者在防御性地关掉一个本就不该出现的工具) 会被报成"名字不存在",
// 而看的人会去删掉一行本来就对的配置。
func Test黑名单在analysis档下不误报(t *testing.T) {
	install(t, "analysis-deny-bash", "name: analysis-deny-bash\ntool-profile: analysis\ndisallowed-tools: [Bash]", "正文")
	bd := bindingOf(t, BindAgentAsset("analysis-deny-bash", nil), "tools")

	if bd.Status != BindingWired {
		t.Errorf("Bash 是个真工具名 (只是 analysis 档不注册它), 不得报成拼写错误: %+v", bd)
	}
	if len(bd.Declared) != 1 || bd.Declared[0] != "Bash" {
		t.Errorf("黑名单要原样进声明侧, 才能在对账里报出「声明要挡但本档位根本没有它」: %v", bd.Declared)
	}
}

// Test白名单认不出的名字不剔空 是本项最要紧的一条**安全**断言。
//
// 白名单里有一个非内置工具 (MCP 动态工具很常见) 时, 若把认不出的名字删掉, 结果
// 可能是**空的非 nil 白名单** —— 而那是 fail-closed 的"拒绝一切工具"。于是一份
// 本来只是窄的配置变成了一份恒失败的配置, 且没有任何人要求过这个变化。
//
// 目录不是全知, 所以它只用来判是非, 不用来下判决。
func Test白名单认不出的名字不剔空(t *testing.T) {
	install(t, "mcp-tool", "name: mcp-tool\ntools: [Read, mcp__foo__bar]", "正文")
	b := BindAgentAsset("mcp-tool", nil)

	if len(b.Tools) != 2 {
		t.Fatalf("认不出的名字不得被剔除 (剔除会把白名单削成空集 = 拒绝一切工具), got %v", b.Tools)
	}
	// 但仍要报出来 —— 不剔不等于不说。
	bd := bindingOf(t, b, "tools")
	if bd.Status != BindingInvalidValue {
		t.Errorf("认不出的名字必须留痕, 否则它只表现为「那个工具没被调用」: %+v", bd)
	}
	if !strings.Contains(bd.Note, "mcp__foo__bar") {
		t.Errorf("要点名是哪一个: %q", bd.Note)
	}
}
