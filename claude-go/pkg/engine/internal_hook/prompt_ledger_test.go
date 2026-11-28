// 提示词账本单测: 组件的"量得出"是观测的前提。
//
// 这条用例守的是一个真实事故 (2026-11-28): 精简技能清单的开标签带属性
// (`<available_skills summary="short">`), 而 countXMLBlocks 只认字面量
// `<available_skills>`, 于是 CLI 路径上 skill_listing_chars 恒为 0。
// 排查技能路由时它把人引向"清单根本没下发"的错误结论 —— 账本报错比不报更糟。
package internal_hook

import (
	"strings"
	"testing"
)

func TestCountXMLBlocksToleratesAttributes(t *testing.T) {
	// 精简清单的真实形状 (skills.renderShortListing)。计到闭标签为止, 不含其后换行。
	short := "<available_skills summary=\"short\">\n- weread-skills: 微信读书\n</available_skills>"
	if got, want := countXMLBlocks(short+"\n", "available_skills"), len(short); got != want {
		t.Fatalf("带属性的开标签应被计入, want %d, got %d", want, got)
	}

	// 全量清单 (无属性) 与带属性两种形状并存时都要算。
	full := "<available_skills>\n- a\n</available_skills>"
	both := short + "\n" + full
	if got, want := countXMLBlocks(both, "available_skills"), len(short)+len(full); got != want {
		t.Fatalf("两种形状应都计入, want %d, got %d", want, got)
	}
}

func TestCountXMLBlocksDoesNotMatchLongerTagNames(t *testing.T) {
	// <available_skills_extra> 不是 available_skills, 一个字符都不该计。
	s := "<available_skills_extra>x</available_skills_extra>\n"
	if got := countXMLBlocks(s, "available_skills"); got != 0 {
		t.Fatalf("更长的标签名不应命中, got %d", got)
	}
}

func TestCountXMLBlocksUnclosedCountsRest(t *testing.T) {
	// 截断的 prompt 把剩余量记满 —— 记 0 会把它伪装成"这段不存在"。
	s := "head<available_skills summary=\"short\">\n- a\n"
	if got, want := countXMLBlocks(s, "available_skills"), len(s)-len("head"); got != want {
		t.Fatalf("闭标签缺失时应记剩余全部, want %d, got %d", want, got)
	}
}

// 端到端: 精简清单真的进了 system prompt 时, 账本的 skill 组件必须非 0。
func TestBuildPromptComponentMetricsCountsShortSkillListing(t *testing.T) {
	listing := "<available_skills summary=\"short\">\n- weread-skills: 微信读书\n</available_skills>"
	sys := []string{"you are a helpful agent\n", listing + "\n"}
	m := BuildPromptComponentMetrics(sys, nil, nil)
	if m.SkillListingChars != len(listing) {
		t.Fatalf("skill_listing_chars 应等于清单长度 %d, got %d (system=%q)",
			len(listing), m.SkillListingChars, strings.Join(sys, "\n"))
	}
}
