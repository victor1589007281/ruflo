// bashguard_test.go —— Bash 目标合并闸的验收。
//
// 判据不是"代码里有一个 if", 而是三条行为:
//   - 下载后单独解析同一份文件 → 拒绝 (要求合并);
//   - 合并命令的重试 → 放行一次, 之后同一目标再出现 → 拒绝;
//   - 不同目标 / 与文档无关的命令 → 不受影响.
//
// 第三条是防止闸门退化成"Bash 次数上限"的那条臂: 误伤多目标并行与
// go test 这类命令, 比漏判更糟 (它会让 agent 真的干不了活)。
package tool

import (
	"strings"
	"testing"
)

func TestBashMergeGuardRejectsSplitExtraction(t *testing.T) {
	g := NewBashMergeGuard()
	cwd := t.TempDir()

	if reason := g.CheckAndRecord(cwd, "curl -sL https://example.com/report.pdf -o /tmp/report.pdf"); reason != "" {
		t.Fatalf("首次下载不应被拒: %s", reason)
	}
	// 单独一条解析命令: 目标已出现过, 且本身不是合并命令 → 必须拒.
	reason := g.CheckAndRecord(cwd, "pdftotext /tmp/report.pdf -")
	if reason == "" {
		t.Fatal("下载后单独解析同一份 PDF 应被拒绝")
	}
	if !strings.Contains(reason, "合并") || !strings.Contains(reason, "report.pdf") {
		t.Fatalf("拒绝文案要指明目标与合并要求, got: %s", reason)
	}
}

func TestBashMergeGuardAllowsOneMergedRetryThenRejects(t *testing.T) {
	g := NewBashMergeGuard()
	cwd := t.TempDir()

	first := "curl -sL https://example.com/report.pdf -o /tmp/report.pdf"
	if reason := g.CheckAndRecord(cwd, first); reason != "" {
		t.Fatalf("首次下载不应被拒: %s", reason)
	}
	// 合并重试: 下载 + 解析在同一条命令里 → 放行一次.
	merged := "curl -sL https://example.com/report.pdf -o /tmp/report.pdf && pdftotext -layout /tmp/report.pdf - | head -50"
	if reason := g.CheckAndRecord(cwd, merged); reason != "" {
		t.Fatalf("合并重试应放行一次: %s", reason)
	}
	// 第三次: 同一个目标已经有过合并重试 → 拒绝, 即使这次也是合并命令.
	if reason := g.CheckAndRecord(cwd, merged); reason == "" {
		t.Fatal("同一目标第三次出现应被拒绝")
	}
}

// TestBashMergeGuardLinksURLToBasename 钉住 URL 与本地同名文件是同一个目标。
//
// 没有这条, `curl https://x/a.pdf` 与后续 `pdftotext a.pdf` 会被当成两个不同
// 资源 —— 而它们恰恰是"拆成两条命令"最典型的形态。
func TestBashMergeGuardLinksURLToBasename(t *testing.T) {
	g := NewBashMergeGuard()
	cwd := t.TempDir()

	if reason := g.CheckAndRecord(cwd, "curl -sL https://example.com/papers/a.pdf"); reason != "" {
		t.Fatalf("首次下载不应被拒: %s", reason)
	}
	if reason := g.CheckAndRecord(cwd, "pdftotext a.pdf -"); reason == "" {
		t.Fatal("URL basename 与本地同名文件应被视为同一目标")
	}
}

func TestBashMergeGuardLeavesOtherCommandsAlone(t *testing.T) {
	g := NewBashMergeGuard()
	cwd := t.TempDir()

	commands := []string{
		"go test ./pkg/search/...",
		"git status --short",
		"ls -la",
		"grep -rn TODO pkg/",
	}
	for i := 0; i < 3; i++ {
		for _, cmd := range commands {
			if reason := g.CheckAndRecord(cwd, cmd); reason != "" {
				t.Fatalf("非文档命令不应被拒 (%q): %s", cmd, reason)
			}
		}
	}

	// 不同目标各自第一次访问都放行.
	for _, cmd := range []string{
		"pdftotext /tmp/a.pdf -",
		"pdftotext /tmp/b.pdf -",
		"pdftotext /tmp/c.pdf -",
	} {
		if reason := g.CheckAndRecord(cwd, cmd); reason != "" {
			t.Fatalf("不同目标的首次访问不应被拒 (%q): %s", cmd, reason)
		}
	}
}

// TestBashMergeGuardNilIsInert 钉住"未启用 = 逐字节旧行为"。
func TestBashMergeGuardNilIsInert(t *testing.T) {
	var g *BashMergeGuard
	for i := 0; i < 5; i++ {
		if reason := g.CheckAndRecord(t.TempDir(), "pdftotext /tmp/a.pdf -"); reason != "" {
			t.Fatalf("nil guard 必须完全惰性, got: %s", reason)
		}
	}
}
