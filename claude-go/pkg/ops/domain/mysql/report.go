package mysql

import (
	"github.com/anthropic/claude-go/pkg/ops/evidence"
	"github.com/anthropic/claude-go/pkg/ops/report"
)

// RenderMarkdown 把证据包渲染成可复核的报告。
//
// 硬约束：结论段落里的每一句都带 evidence_id；没有证据的判断只能出现在
// “缺口/未决”里。这样报告本身可以当作验收对象（无引用结论数 = 0）。
func RenderMarkdown(b *evidence.Bundle) string {
	return report.RenderMarkdown("MySQL RCA", b, []string{"threads_connected", "threads_running", "queries"})
}
