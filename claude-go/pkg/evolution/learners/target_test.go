package learners

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/asset"
)

func TestParseTargetTagged(t *testing.T) {
	cases := []struct {
		in   string
		kind TargetKind
		name string
	}{
		{"skill:commit", TargetKind(asset.KindSkill), "commit"},
		{"agent:coder", TargetKind(asset.KindAgent), "coder"},
		{"hook:audit", TargetKind(asset.KindHook), "audit"},
		{"stage:development/implement", TargetKindStage, "development/implement"},
		{"SKILL:commit", TargetKind(asset.KindSkill), "commit"}, // 前缀大小写不敏感
	}
	for _, c := range cases {
		k, n, err := ParseTarget(c.in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", c.in, err)
		}
		if k != c.kind || n != c.name {
			t.Fatalf("ParseTarget(%q) = (%q,%q), want (%q,%q)", c.in, k, n, c.kind, c.name)
		}
	}
}

// TestParseTargetLegacyIsPreserved 是**兼容性回归测试**: 磁盘上真实的 9 份草案里
// target 是 `<workflow>/<stage>` 这种无标签形态。若把它们的解释改掉, 全部既有灰度
// 会当场失效 —— 而表现为"没有实验", 即静默不生效, 不会有任何报错。
func TestParseTargetLegacyIsPreserved(t *testing.T) {
	legacy := []string{
		"if-question-expand/增量出题", // 取自 /mnt/data/claude-go/evolution/proposals 的真实取值
		"development/implement",
		// 工作流名恰好叫某个 kind、或含冒号 —— 都必须仍然判成 legacy
		"skill/x",
		"agent/x",
		"a:b/c",
		"workflow/Development",
	}
	for _, s := range legacy {
		k, n, err := ParseTarget(s)
		if err != nil {
			t.Fatalf("旧格式 %q 必须仍可解析: %v", s, err)
		}
		if k != TargetKindLegacy {
			t.Fatalf("旧格式 %q 被判成 %q (会让既有草案全部 match 不上)", s, k)
		}
		if n != s {
			t.Fatalf("旧格式的名字应逐字节保留: got %q want %q", n, s)
		}
	}
}

func TestParseTargetRejectsEmptyAndMalformed(t *testing.T) {
	for _, bad := range []string{"", "   ", "skill:", "  skill:  "} {
		if _, _, err := ParseTarget(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestFormatTarget(t *testing.T) {
	got, err := FormatTarget(TargetKind(asset.KindSkill), " commit ")
	if err != nil || got != "skill:commit" {
		t.Fatalf("FormatTarget = %q %v", got, err)
	}
	// legacy 不可构造: 它是解析的产物, 允许构造会让 "legacy:x" 有个假合法解释
	if _, err := FormatTarget(TargetKindLegacy, "x"); err == nil {
		t.Fatal("legacy 不该可构造")
	}
	if _, err := FormatTarget("nope", "x"); err == nil {
		t.Fatal("未知 kind 应报错")
	}
	if _, err := FormatTarget(TargetKind(asset.KindSkill), "a\nb"); err == nil {
		t.Fatal("含换行的 name 应被拒 (会把 jsonl 一条记录掰成两行)")
	}
	if _, err := FormatTarget(TargetKind(asset.KindSkill), "  "); err == nil {
		t.Fatal("空 name 应被拒")
	}
}

// TestStageTargetIsByteIdenticalToLegacy 锁死旧格式的**逐字节**形状。
// 这条断言的价值在于: 一旦有人"顺手"把 StageTarget 改成带 stage: 前缀,
// 既有灰度会静默失效 —— 这条测试会当场红, 而生产上不会有任何别的信号。
func TestStageTargetIsByteIdenticalToLegacy(t *testing.T) {
	got := StageTarget("development", "implement")
	want := "development" + "/" + "implement"
	if got != want {
		t.Fatalf("StageTarget 的字节形态变了: %q != %q (既有草案会全部 match 不上)", got, want)
	}
	k, _, err := ParseTarget(got)
	if err != nil || k != TargetKindLegacy {
		t.Fatalf("StageTarget 的产物必须是 legacy 形态: %q %v", k, err)
	}
	if got := StageTarget(" if-question-expand ", " 增量出题 "); got != "if-question-expand/增量出题" {
		t.Fatalf("空白未归一: %q", got)
	}
}

func TestAssetTargetRoundTrip(t *testing.T) {
	for _, k := range asset.Kinds() {
		s, err := AssetTarget(k, "x")
		if err != nil {
			t.Fatalf("AssetTarget(%s): %v", k, err)
		}
		gotK, gotN, err := ParseTarget(s)
		if err != nil || string(gotK) != string(k) || gotN != "x" {
			t.Fatalf("往返失败: %q → (%q,%q) %v", s, gotK, gotN, err)
		}
	}
	if _, err := AssetTarget(asset.Kind("bogus"), "x"); err == nil {
		t.Fatal("非法资产 kind 应报错")
	}
}

func TestValidateTarget(t *testing.T) {
	ok := []string{
		"skill:commit",
		"stage:development/implement",
		"if-question-expand/增量出题",
	}
	for _, s := range ok {
		if err := ValidateTarget(s); err != nil {
			t.Fatalf("ValidateTarget(%q) 应通过: %v", s, err)
		}
	}
	bad := []string{
		"",
		"skill:",
		"一个既不像标签也不带斜杠的名字", // 形状不明 ⇒ 永远 match 不上, 必须在写盘前拦
		"skill:a\tb",
	}
	for _, s := range bad {
		if err := ValidateTarget(s); err == nil {
			t.Fatalf("ValidateTarget(%q) 应被拒", s)
		}
	}
	// 报错信息里要带上出问题的那个 target, 否则排查时不知道是哪一条
	err := ValidateTarget("skill:")
	if err == nil || !strings.Contains(err.Error(), "skill:") {
		t.Fatalf("报错应指明 target: %v", err)
	}
}
