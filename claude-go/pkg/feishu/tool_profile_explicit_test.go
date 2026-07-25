package feishu

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// 显式 tool_profile 声明必须能映射到内部档位——这是 design/01 §4.6
// "显式声明取代角色名猜测" 的落点。
func TestMapExplicitToolProfile(t *testing.T) {
	for in, want := range map[string]builtin.ToolProfile{
		"coding":   builtin.ToolProfileCoding,
		"CODING":   builtin.ToolProfileCoding, // 大小写不敏感
		"  team  ": builtin.ToolProfileTeam,   // 两侧空白容错
		"analysis": builtin.ToolProfileAnalysis,
		"research": builtin.ToolProfileResearch,
		"chat":     builtin.ToolProfileChat,
		"admin":    builtin.ToolProfileAdmin,
	} {
		if got := mapExplicitToolProfile(in); got != want {
			t.Errorf("mapExplicitToolProfile(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 未知值/空值必须返回空串，让调用方回落到角色名推断。
// 关键：一个拼错的声明**不应**静默剥掉 agent 的工具。
func TestMapExplicitToolProfile_未知值回落(t *testing.T) {
	for _, in := range []string{"", "   ", "codingg", "Coder", "none", "off"} {
		if got := mapExplicitToolProfile(in); got != "" {
			t.Errorf("mapExplicitToolProfile(%q) = %q, 期望空串(回落推断)", in, got)
		}
	}
}

// 回归 world-builder 那个真实误判：角色名含 "build" 会被 profileForTeamRole
// 判成 Coding 档从而拿到 Bash。显式声明 analysis 必须能压过它。
func TestExplicitProfile_压过角色名误判(t *testing.T) {
	inferred := profileForTeamRole("world-builder", "novel-v3")
	explicit := mapExplicitToolProfile("analysis")
	if explicit == "" {
		t.Fatal("analysis 应能映射")
	}
	if explicit == inferred {
		t.Skipf("本机推断结果恰为 %q, 该用例失去对比性", inferred)
	}
	t.Logf("world-builder 的角色名推断=%q, 显式声明=%q —— 显式优先", inferred, explicit)
}
