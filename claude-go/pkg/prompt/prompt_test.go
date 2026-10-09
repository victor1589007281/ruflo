package prompt

// prompt_test.go —— 只覆盖"日期开关"这一条, 因为它是**唯一一个会让整段系统提示
// 每天作废一次**的字段 (见 Manager.ShowEnvironmentDate 的注释)。
//
// 这里刻意不断言"日期字符串等于今天"——那种断言在跨零点时会偶发失败, 而且它
// 证明不了我们要的东西。真正要保的性质是**关闭时输出与日期无关**, 所以断言的是
// "Date: 这一行不存在", 也就是"同一份配置在任何一天跑出来的字节都一样"。

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
)

func buildEnv(t *testing.T, showDate bool) string {
	t.Helper()
	m := NewManager("/tmp")
	m.ShowEnvironmentDate = showDate
	// 空注册表而非 nil: buildDefaultSystemPrompt 会对它调 tools.All()。
	return m.buildDefaultSystemPrompt(tool.NewRegistry())
}

// 关闭时 (默认): 提示词里不出现 Date 行 —— 这是"跨天前缀不变"的充分条件。
func Test系统提示_默认不写日期(t *testing.T) {
	got := buildEnv(t, false)
	if strings.Contains(got, "Date: ") {
		t.Fatalf("ShowEnvironmentDate=false 时不应出现 Date 行, 实际出现了:\n%s", got)
	}
	// 环境块本身必须还在 —— 不能靠"整段没拼"来通过上面那条。
	if !strings.Contains(got, "<environment>") || !strings.Contains(got, "OS: ") {
		t.Fatalf("environment 块缺失, 开关关掉的是日期行而不是整个块:\n%s", got)
	}
}

// 打开时: 日期行出现, 且格式为 YYYY-MM-DD。
func Test系统提示_打开时写日期(t *testing.T) {
	got := buildEnv(t, true)
	want := "Date: " + time.Now().Format("2006-01-02") + "\n"
	// 跨零点的极端情况: 允许命中的是"昨天"或"明天"。
	alts := []string{
		"Date: " + time.Now().Add(-24*time.Hour).Format("2006-01-02") + "\n",
		"Date: " + time.Now().Add(24*time.Hour).Format("2006-01-02") + "\n",
	}
	ok := strings.Contains(got, want)
	for _, alt := range alts {
		ok = ok || strings.Contains(got, alt)
	}
	if !ok {
		t.Fatalf("ShowEnvironmentDate=true 时应出现形如 %q 的日期行:\n%s", want, got)
	}
}

// 关掉日期后, 环境块里排在日期**之后**的行 (cwd/shell) 必须原地不动 ——
// 否则"少了一行"会顺带改变后续内容的相对位置, 前缀照样变。
func Test系统提示_关日期不改其余行(t *testing.T) {
	off := buildEnv(t, false)
	on := buildEnv(t, true)

	// 把 on 里的日期行删掉, 应当与 off 逐字节相同。
	lines := strings.Split(on, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.HasPrefix(ln, "Date: ") {
			continue
		}
		kept = append(kept, ln)
	}
	if stripped := strings.Join(kept, "\n"); stripped != off {
		t.Fatalf("开关只应增删 Date 行, 其余内容不得变化\n--- off ---\n%s\n--- on(去日期) ---\n%s", off, stripped)
	}
}

// 开关默认值是 false —— 落在 Manager 的零值上, 保证"忘了配"等于"不写日期"
// 而不是"写日期"。这条是给未来改 NewManager 的人兜底的。
func Test系统提示_开关默认关(t *testing.T) {
	m := NewManager("/tmp")
	if m.ShowEnvironmentDate {
		t.Fatal("NewManager 的默认值必须是 false: 漏配的场景应当退化成时间无关")
	}
}
