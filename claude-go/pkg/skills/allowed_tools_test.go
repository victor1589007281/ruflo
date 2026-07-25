package skills

import (
	"reflect"
	"testing"
)

// Skill.AllowedTools 此前是**死字段** (结构体里有、全仓无赋值点), 于是 SKILL.md 里
// 写了 allowed-tools 在代码里恒为空。不越权闸 (pkg/evolution/govern) 依赖它才有对象,
// 所以这里把三种在野写法都焊住。
func TestParseFrontmatter_解析allowedTools三种写法(t *testing.T) {
	cases := []struct {
		name string
		fm   string
		want []string
	}{
		{"逗号分隔", "name: a\nallowed-tools: Read, Bash", []string{"Read", "Bash"}},
		{"下划线键名", "name: a\nallowed_tools: Read, Grep", []string{"Read", "Grep"}},
		{"内联列表", "name: a\nallowed-tools: [Read, WebFetch]", []string{"Read", "WebFetch"}},
		{"空白分隔", "name: a\nallowed-tools: Read Glob", []string{"Read", "Glob"}},
		{"单个", "name: a\nallowed-tools: Bash", []string{"Bash"}},
		{"空值", "name: a\nallowed-tools:", nil},
		{"未声明", "name: a", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var sk Skill
			parseFrontmatter(c.fm, &sk)
			if !reflect.DeepEqual(sk.AllowedTools, c.want) {
				t.Errorf("got %#v, want %#v", sk.AllowedTools, c.want)
			}
		})
	}
}

// 逗号写法绝不能按空白切: "Read, Bash" 若切成 ["Read," "Bash"] 会让治理层看到一个
// 带标点的"不认识的工具名", 而不认识就拒绝 ⇒ 一个格式问题变成一次误拒。
func TestParseToolList_逗号优先于空白(t *testing.T) {
	got := parseToolList("Read, Bash, WebFetch")
	want := []string{"Read", "Bash", "WebFetch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, n := range got {
		if n[len(n)-1] == ',' {
			t.Fatalf("工具名残留逗号: %q", n)
		}
	}
}

// 解析 allowed-tools 不得影响运行期可用性判定 (IsActive 只看 status)。
func TestAllowedTools不影响IsActive(t *testing.T) {
	var sk Skill
	parseFrontmatter("name: a\nallowed-tools: Bash", &sk)
	if !sk.IsActive() {
		t.Error("声明工具不该让技能变成停用状态 —— 运行期可见性只由 toolExposed 裁决")
	}
}
