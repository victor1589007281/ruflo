package linux

import (
	"github.com/anthropic/claude-go/pkg/ops/evidence"
	"github.com/anthropic/claude-go/pkg/ops/report"
)

// RenderMarkdown 渲染主机证据报告。时间线按 key=value 输出，避免把
// 某个环境的列名硬编码进通用层。
func RenderMarkdown(b *evidence.Bundle) string {
	return report.RenderMarkdown("Linux RCA", b, nil)
}
