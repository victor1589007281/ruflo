package engine

import "testing"

// TestReportShape 钉住与评测脚本一致的口径: 正文只数汉字, 表格行含表头,
// 域名只从证据表取。
func TestReportShape(t *testing.T) {
	const report = `# 标题：固态电池调研

## 结论

产业化在 2026 年进入中试阶段，头部厂商良率仍是瓶颈。

| # | 来源 | 说明 |
|---|------|------|
| 1 | https://a.example.com/x | 数据 |
| 2 | https://a.example.com/y | 数据 |
| 3 | https://b.example.org/z | 数据 |

正文里这条内联链接 https://c.example.net/inline 不应计入证据表域名。
`
	body, rows, hosts := reportShape(report)
	// "标题" 在 H1 里不计; 正文汉字: 固态电池调研(H1 排除) 结论 产业化在... 等。
	if body == 0 {
		t.Fatal("bodyCJK 不应为 0")
	}
	if rows != 4 {
		t.Fatalf("rows = %d, want 4 (表头 + 3 行)", rows)
	}
	if hosts != 2 {
		t.Fatalf("hosts = %d, want 2 (a.example.com + b.example.org)", hosts)
	}
}

// TestReportShapeEmpty 覆盖空文本与无表格报告: 不能因为除零/空串误报。
func TestReportShapeEmpty(t *testing.T) {
	if b, r, h := reportShape(""); b != 0 || r != 0 || h != 0 {
		t.Fatalf("empty: got %d/%d/%d", b, r, h)
	}
	if b, r, h := reportShape("只有一段中文说明。"); b == 0 || r != 0 || h != 0 {
		t.Fatalf("no table: got %d/%d/%d", b, r, h)
	}
}
