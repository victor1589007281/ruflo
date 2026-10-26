package k8s

import (
	"github.com/anthropic/claude-go/pkg/ops/evidence"
	"github.com/anthropic/claude-go/pkg/ops/report"
)

// RenderMarkdown 渲染集群证据报告。
func RenderMarkdown(b *evidence.Bundle) string {
	return report.RenderMarkdown("Kubernetes RCA", b, nil)
}
