package skills

import (
	"strings"
	"testing"
)

func mkSkill(name, desc string) *Skill {
	return &Skill{Name: name, Description: desc, Body: "正文", LoadedFrom: "builtin"}
}

// 按名字裁剪: 只出现点名的技能, 其余一个都不许漏。
//
// 这是"把跨项目技能库从每次请求里摘掉"的机制 —— 内置库 28 个技能里绝大多数
// 与当前任务无关, 全量清单实测 3133 字节 / 35 条而 Skill 工具 0 次被用。
func TestFormatShortListingForNamesFilters(t *testing.T) {
	r := NewRegistry()
	r.Register(mkSkill("golang-patterns", "Go 模式"))
	r.Register(mkSkill("django-tdd", "Django TDD"))
	r.Register(mkSkill("concurrency-review", "并发安全审查专家"))
	r.Register(mkSkill("mf-brand-guide", "媒锻品牌规范"))

	out := r.FormatShortListingForNames([]string{"golang-patterns", "concurrency-review"})

	if !strings.Contains(out, "golang-patterns") || !strings.Contains(out, "concurrency-review") {
		t.Errorf("点名的技能应出现:\n%s", out)
	}
	for _, unwanted := range []string{"django-tdd", "mf-brand-guide"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("未点名的技能 %s 不应出现:\n%s", unwanted, out)
		}
	}
	// 名称序确定性, 与传入顺序无关
	rev := r.FormatShortListingForNames([]string{"concurrency-review", "golang-patterns"})
	if out != rev {
		t.Errorf("清单应与传入顺序无关:\n%q\n%q", out, rev)
	}
}

func TestFormatShortListingForNamesEmpty(t *testing.T) {
	r := NewRegistry()
	r.Register(mkSkill("golang-patterns", "Go 模式"))

	// 角色没声明技能 → 空串 (宁可什么都不列, 也不要把整个技能库倒进去)
	if got := r.FormatShortListingForNames(nil); got != "" {
		t.Errorf("无名字应返回空串, 得 %q", got)
	}
	// 名字都对不上 (技能已卸载/改名) → 同样空串, 不留半个空壳块
	if got := r.FormatShortListingForNames([]string{"不存在的技能"}); got != "" {
		t.Errorf("名字全不匹配应返回空串, 得 %q", got)
	}
}

// 全量清单本身不能因为这次改动发生变化 (主会话/其他入口仍走它)。
func TestFormatShortListingStillListsAll(t *testing.T) {
	r := NewRegistry()
	r.Register(mkSkill("golang-patterns", "Go 模式"))
	r.Register(mkSkill("django-tdd", "Django TDD"))

	out := r.FormatShortListing(0)
	if !strings.Contains(out, "golang-patterns") || !strings.Contains(out, "django-tdd") {
		t.Errorf("全量清单应两个都在:\n%s", out)
	}
}
