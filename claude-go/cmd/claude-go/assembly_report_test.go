package main

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/assembly"
	"github.com/anthropic/claude-go/pkg/feishu"
)

// 没有描述符 ≠ 描述了空。前者是"平台根本没签发版本", 对账要如实报"未对账";
// 后者是一份什么都没有要求的声明, 对账照跑并以零差异收场。
// 两者压成同一个 nil, 面板上就分不出"没对账"与"对过了, 一致"。
func TestDeclarationFromProfileAbsentIsNil(t *testing.T) {
	if d := declarationFromProfile(nil); d != nil {
		t.Fatalf("没有配置时不应有声明, got %+v", d)
	}
	// 配置在, 但没带描述符 —— 这是 CLI 手工运行与旧平台的常态。
	cfg := &feishu.JSONConfig{}
	if d := declarationFromProfile(cfg); d != nil {
		t.Fatalf("配置里没有 assemblyProfile 时不应凭空造声明, got %+v", d)
	}
}

// 描述符里**没写的段**必须原样留成 nil (不参与对账), 不许被"顺理成章"地补上:
// 补出来的是一份**期望**不是一份**声明**, 拿它去对账等于自己判自己。
func TestDeclarationFromProfileDoesNotInventUndeclaredFacets(t *testing.T) {
	// 配置里有一份白名单, 但描述符没声明工具面 —— 违和, 正是要钉住的场景。
	cfg := &feishu.JSONConfig{AllowedTools: feishu.StringList{"Bash", "Read"}}
	cfg.AssemblyProfile = &feishu.AssemblyProfile{Version: "v1"}

	d := declarationFromProfile(cfg)
	if d == nil {
		t.Fatal("有描述符就应当有声明")
	}
	if d.Tools != nil {
		t.Errorf("描述符未声明工具面, 不得拿 cfg.AllowedTools 顶上, got %v", d.Tools)
	}
	if d.Components != nil || d.Model != "" || d.PermissionMode != "" {
		t.Errorf("未声明的段必须留空, got %+v", d)
	}
}

// nil 与空表是两回事: nil = 不要求工具面 (不参与对账),
// 空表 = 要求拒绝一切 (fail-closed)。压平会让"要求拒绝一切"那次对账整个消失。
func TestDeclarationFromProfileKeepsNilVsEmpty(t *testing.T) {
	cfg := &feishu.JSONConfig{}
	cfg.AssemblyProfile = &feishu.AssemblyProfile{Version: "v1", Tools: feishu.StringList{}}
	d := declarationFromProfile(cfg)
	if d.Tools == nil {
		t.Fatal("描述符里显式的空工具表必须活下来 —— 它是 fail-closed 的声明")
	}
	if len(d.Tools) != 0 {
		t.Fatalf("空表不该被填出内容, got %v", d.Tools)
	}

	// 空表不含具体工具名, 故不产生"逐项"差异 (逐项差异只对具名项成立)。
	// 这条钉住的是: fail-closed 的实现方式失效时, 对账不会用逐项差异掩盖它。
	actual := &assembly.Report{
		SchemaVersion: assembly.SchemaVersion,
		Tools:         &assembly.ToolsInfo{Registered: 57, Visible: nil, Hidden: []string{"Bash"}, WhitelistSet: true},
	}
	if diffs := assembly.Reconcile(d, actual); len(diffs) != 0 {
		t.Fatalf("空表不含具体工具名, 不产生逐项差异, got %+v", diffs)
	}
}

// hook 声明的编号口径必须与运行期自报同源 (都是配置数组下标)。
// 一旦这里改成"手填 index", 两边口径分家, 对账会整片报"未装载" —— 那是幻觉不是发现。
func TestDeclarationFromProfileHookIndexIsArrayPosition(t *testing.T) {
	cfg := &feishu.JSONConfig{}
	cfg.AssemblyProfile = &feishu.AssemblyProfile{
		Version: "v1",
		Hooks: []feishu.AssemblyProfileHook{
			{Event: "PreToolUse", Type: "command"},
			{Event: "PostCompact", Type: "http"},
		},
	}
	d := declarationFromProfile(cfg)
	if len(d.ExternalHooks) != 2 {
		t.Fatalf("应翻出 2 条 hook 声明, got %+v", d.ExternalHooks)
	}
	if d.ExternalHooks[0].Index != 0 || d.ExternalHooks[1].Index != 1 {
		t.Fatalf("序号必须取数组下标, got %+v", d.ExternalHooks)
	}
	if d.ExternalHooks[1].Event != "PostCompact" || d.ExternalHooks[1].Type != "http" {
		t.Fatalf("事件与类型要原样带过, got %+v", d.ExternalHooks[1])
	}
}

// 描述符版本号的取值点是**唯一**的, 且在入口这一侧。
//
// 采集侧 (pkg/engine) 刻意不碰它: "我照哪张图纸装的"与"装成了什么"是两件事,
// 让采集端去读配置会让它重新变成"报告配置里写了什么"。这条钉住那个分界 ——
// 描述符在就带版本, 不在就留空 (不是编一个版本号), 消费侧据此判"这次没照谁的图纸装"。
func TestDeclarationVersionOfReadsOnlyTheDescriptor(t *testing.T) {
	cfg := &feishu.JSONConfig{}
	cfg.AssemblyProfile = &feishu.AssemblyProfile{Version: "abc123def456"}
	if v := declarationVersionOf(cfg); v != "abc123def456" {
		t.Fatalf("自报要带上实际读到的描述符版本, got %q", v)
	}
	if v := declarationVersionOf(&feishu.JSONConfig{}); v != "" {
		t.Fatalf("没有描述符时不得编出版本号, got %q", v)
	}
	if v := declarationVersionOf(nil); v != "" {
		t.Fatalf("没有配置时不得编出版本号, got %q", v)
	}
}

// 描述符版本**不参与**指纹: 换一版图纸而装配结果一字未变时, 说"装配变了"是错的 ——
// 指纹回答的是"装成了什么", 不是"照谁的图纸装的"。
func TestFingerprintIgnoresDeclarationVersion(t *testing.T) {
	base := &assembly.Report{
		SchemaVersion: assembly.SchemaVersion,
		Tools:         &assembly.ToolsInfo{Registered: 3, Visible: []string{"a"}, WhitelistSet: true},
	}
	other := *base
	other.DeclarationVersion = "v-next"
	if base.Fingerprint() != other.Fingerprint() {
		t.Fatal("描述符版本不该改变装配指纹")
	}
	// 配对验证: 真正改了装配时必须变, 否则上一条会退化成"恒等"。
	changed := *base
	changed.Tools = &assembly.ToolsInfo{Registered: 3, Visible: []string{"a", "b"}, WhitelistSet: true}
	if base.Fingerprint() == changed.Fingerprint() {
		t.Fatal("装配实质变化时指纹必须变")
	}
}
