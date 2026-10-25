// runner_thinking_test.go —— thinking 块不得混进最终交付文本。
//
// 真实故障 (2026-10-25 solid): 最后一轮同时返回 thinking 与 text, thinking 是
// 一份带 H1 的写作提纲。旧实现把两者直接拼接, 交付文本于是有两个一级标题;
// CLI 在引擎外再清洗一次掩盖了大部分问题, 但引擎内的引用审计 / evidence.md
// 回填 / trace 都已经看到污染文本, 审计字数与交付报告对不上。
package engine

import (
	"context"
	"strings"
	"testing"
)

func TestThinkingExcludedWhenTextPresent(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "# 正式标题\n\n正文结论。")
	u.thinking = "先规划：\n# 提纲标题\n\n1. 结论\n2. 证据"
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写报告", IsolatedRunOptions{
		MaxTurns: 2,
		Stage:    id.Stage,
	})
	if err != nil {
		t.Fatalf("RunIsolated: %v", err)
	}
	if strings.Contains(got, "先规划") || strings.Contains(got, "提纲标题") {
		t.Fatalf("thinking 混进了交付文本:\n%s", got)
	}
	if got != "# 正式标题\n\n正文结论。" {
		t.Fatalf("交付文本被改写: %q", got)
	}
}

// TestThinkingUsedOnlyAsFallback 钉住兜底: 模型只回 thinking (text 为空) 时,
// 仍然要拿到内容 —— 否则这类模型会以 "assistant returned empty content" 失败。
func TestThinkingUsedOnlyAsFallback(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "")
	u.thinking = "只有推理块里的回答"
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写报告", IsolatedRunOptions{
		MaxTurns: 2,
		Stage:    id.Stage,
	})
	if err != nil {
		t.Fatalf("RunIsolated: %v", err)
	}
	if got != "只有推理块里的回答" {
		t.Fatalf("fallback 失败: %q", got)
	}
}
