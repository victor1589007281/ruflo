// evidence_test.go —— evidence.md「同上」展开的验收。
//
// 失败形态是**改错列或改错行**: 把结论列里的"同上"当成 URL 指代, 或把
// 两条不同来源之间的"同上"接到更早的 URL 上。两种都会让证据表看起来更完整,
// 实际却把引用指到了错误页面。
package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEvidence(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "evidence.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("写 evidence.md: %v", err)
	}
	return p
}

func TestNormalizeEvidenceExpandsSameAsAbove(t *testing.T) {
	p := writeEvidence(t, `# 证据台账

| # | 结论 | 来源 URL | 原文摘录 | 时间 | 置信度 |
|---|---|---|---|---|---|
| E1 | 营收 | https://a.example/1 | "x" | 2026 | 高 |
| E2 | 毛利率 | 同上 E1 URL | "y" | 2026 | 高 |
| E3 | 指引 | 同上 | "z" | 2026 | 高 |
| E4 | 第三方 | https://b.example/2 | "w" | 2026 | 中 |
| E5 | 评论 | 同上 | "v" | 2026 | 中 |
`)
	if n := normalizeEvidenceFile(p); n != 3 {
		t.Fatalf("展开行数 = %d, want 3", n)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "同上") {
		t.Fatalf("仍残留「同上」:\n%s", text)
	}
	for _, want := range []string{
		"| E2 | 毛利率 | https://a.example/1 |",
		"| E3 | 指引 | https://a.example/1 |",
		"| E5 | 评论 | https://b.example/2 |",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("缺少展开结果 %q:\n%s", want, text)
		}
	}
}

// TestNormalizeEvidenceOnlyTouchesURLColumn 钉住"只改 URL 列": 结论列里的
// 同上属于正文, 展开成 URL 会把一条结论变成一串链接。
func TestNormalizeEvidenceOnlyTouchesURLColumn(t *testing.T) {
	body := `| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| E1 | 同上 | https://a.example/1 | "x" | 2026 |
`
	p := writeEvidence(t, body)
	if n := normalizeEvidenceFile(p); n != 0 {
		t.Fatalf("结论列的「同上」被误改, rows=%d", n)
	}
	got, _ := os.ReadFile(p)
	if string(got) != body {
		t.Fatalf("文件被改写:\n%s", got)
	}
}

// TestNormalizeEvidenceLeavesCleanFileAlone 钉住幂等与无副作用: 没有同上时
// 一个字节都不该动 (包括 mtime 之外的空白)。
func TestNormalizeEvidenceLeavesCleanFileAlone(t *testing.T) {
	body := `| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| E1 | 营收 | https://a.example/1 | "x" | 2026 |
`
	p := writeEvidence(t, body)
	if n := normalizeEvidenceFile(p); n != 0 {
		t.Fatalf("干净文件被改动, rows=%d", n)
	}
	got, _ := os.ReadFile(p)
	if string(got) != body {
		t.Fatalf("干净文件内容变了:\n%s", got)
	}
}

// TestNormalizeEvidenceWithoutPriorURLKeepsCell 钉住"没有可展开目标时不猜":
// 第一行就写同上, 保留原样比接一个空 URL 更诚实。
func TestNormalizeEvidenceWithoutPriorURLKeepsCell(t *testing.T) {
	body := `| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| E1 | 营收 | 同上 | "x" | 2026 |
`
	p := writeEvidence(t, body)
	if n := normalizeEvidenceFile(p); n != 0 {
		t.Fatalf("无前序 URL 时不应展开, rows=%d", n)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "| 同上 |") {
		t.Fatalf("原样保留失败:\n%s", got)
	}
}

// TestExpandSameAsAboveOnlyURLColumnAndPerTable 钉住两件事:
//   - "时间"列的同上合法, 不能改;
//   - 换表后不能沿用上一张表最后一条 URL (那会凭空造引用)。
func TestExpandSameAsAboveOnlyURLColumnAndPerTable(t *testing.T) {
	in := `| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| 1 | 营收 | https://a.example/1 | "x" | 2026 |
| 2 | 增速 | 同上 | "y" | 同上 |

后记段落。

| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| 1 | 另一张表 | 同上 | "z" | 2026 |
`
	out, n := expandSameAsAbove(in)
	if n != 1 {
		t.Fatalf("展开行数 = %d, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `| 2 | 增速 | https://a.example/1 | "y" | 同上 |`) {
		t.Fatalf("URL 列未展开或时间列被改写:\n%s", out)
	}
	if !strings.Contains(out, `| 1 | 另一张表 | 同上 | "z" | 2026 |`) {
		t.Fatalf("跨表沿用了上一条 URL:\n%s", out)
	}
}

const backfillReport = `# 某公司调研

## 结论

营收同比增长。

## 对比

| 项目 | 2025 | 2026 |
|---|---|---|
| 营收 | 100 | 120 |

## 证据

| # | 结论 | 来源 URL | 原文摘录 | 时间 |
|---|---|---|---|---|
| 1 | 营收 | https://a.example/1 | "100" | 2026 |
| 2 | 增速 | https://a.example/1 | "20%" | 2026 |
| 3 | 指引 | https://b.example/2 | "120" | 2026 |
`

// TestBackfillEvidenceFromReportTable 钉住回填口径: 只抄证据表, 不抄对比表;
// 0 字节文件被替换, 非空文件一个字节不动。
func TestBackfillEvidenceFromReportTable(t *testing.T) {
	empty := writeEvidence(t, "")
	if !backfillEvidenceFile(empty, backfillReport) {
		t.Fatal("空 evidence.md 没有被回填")
	}
	got, err := os.ReadFile(empty)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "https://a.example/1") || !strings.Contains(text, "https://b.example/2") {
		t.Fatalf("证据表 URL 缺失:\n%s", text)
	}
	if strings.Contains(text, "| 营收 | 100 | 120 |") {
		t.Fatalf("误把对比表回填成证据表:\n%s", text)
	}
	if !strings.Contains(text, "未新增任何事实") {
		t.Fatalf("缺少回填来源说明:\n%s", text)
	}

	keep := writeEvidence(t, "模型自己写的台账\n")
	if backfillEvidenceFile(keep, backfillReport) {
		t.Fatal("非空 evidence.md 被覆盖")
	}
	if b, _ := os.ReadFile(keep); string(b) != "模型自己写的台账\n" {
		t.Fatalf("非空文件内容变了: %s", b)
	}
}

// TestBackfillEvidenceNoTable 钉住"报告里没有证据表时不臆造": 文件保持缺失。
func TestBackfillEvidenceNoTable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evidence.md")
	if backfillEvidenceFile(p, "# 报告\n\n只有正文，没有表格。\n") {
		t.Fatal("无表格报告不应回填")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("不应创建 evidence.md: %v", err)
	}
}
