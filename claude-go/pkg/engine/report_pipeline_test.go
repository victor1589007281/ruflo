// report_pipeline_test.go —— 报告清理 / 标题补齐 / evidence 回填的端到端顺序。
//
// 三个 defer 的**执行顺序**是这条链的隐式契约: 审计与 trace 必须看到清理后的
// 文本, evidence.md 回填必须拿到最终报告里的证据表。只用单元测试验各自的
// 纯函数, 顺序反了也全绿 —— 2026-10-25 的 solid 任务里审计字数比交付报告多
// 154 字, 正是这种"看起来都测了"的盲区。
package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunIsolatedExpandsSameAsAbove 端到端钉住交付报告里的"同上"展开:
// 纯函数测试只证明函数对; 这里证明 runner 的 defer 链真的调用了它, 且只改
// URL 列 (时间列的"同上"保留)。
func TestRunIsolatedExpandsSameAsAbove(t *testing.T) {
	id, cfg := stateIdentity()
	cwd := t.TempDir()
	cfg.Cwd = cwd
	raw := `# 测试报告

## 证据

| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| 1 | 甲 | https://a.example/1 | "x" | 2026 |
| 2 | 乙 | 同上 | "y" | 同上 |
`
	u := newStateUpstream(t, 0, raw)
	e := newStateEngine(t, u, cfg, cwd)

	got, err := e.RunIsolated(context.Background(), "主题：测试报告", IsolatedRunOptions{
		MaxTurns:          3,
		NormalizeEvidence: true,
		Stage:             id.Stage,
	})
	if err != nil {
		t.Fatalf("RunIsolated: %v", err)
	}
	if !strings.Contains(got, `| 2 | 乙 | https://a.example/1 | "y" | 同上 |`) {
		t.Fatalf("URL 列未展开或时间列被误改:\n%s", got)
	}
}

func TestRunIsolatedCleansReportAndBackfillsEvidence(t *testing.T) {
	id, cfg := stateIdentity()
	cwd := t.TempDir()
	cfg.Cwd = cwd
	raw := `Hmm, let me draft the report first.

# 标题：测试主题报告

## 结论

这是一段正文，用来验证清理、标题补齐与证据回填的执行顺序。

| # | 结论 | 来源 URL | 摘录 | 时间 |
|---|---|---|---|---|
| 1 | 结论一 | https://a.example/1 | "x" | 2026 |
| 2 | 结论二 | https://b.example/2 | "y" | 2026 |
`
	u := newStateUpstream(t, 0, raw)
	e := newStateEngine(t, u, cfg, cwd)

	got, err := e.RunIsolated(context.Background(), "主题：测试主题", IsolatedRunOptions{
		MaxTurns:          3,
		NormalizeEvidence: true,
		Stage:             id.Stage,
	})
	if err != nil {
		t.Fatalf("RunIsolated: %v", err)
	}
	if strings.Contains(got, "Hmm") || strings.Contains(got, "标题：") {
		t.Fatalf("交付文本仍带草稿噪声:\n%s", got)
	}
	if !strings.HasPrefix(got, "# 测试主题报告") {
		// 占位标题应被任务里的「主题：」替换。
		t.Fatalf("一级标题未补齐:\n%s", got)
	}
	ev, err := os.ReadFile(filepath.Join(cwd, "evidence.md"))
	if err != nil {
		t.Fatalf("evidence.md 未回填: %v", err)
	}
	if !strings.Contains(string(ev), "https://a.example/1") || !strings.Contains(string(ev), "https://b.example/2") {
		t.Fatalf("回填内容缺证据 URL:\n%s", ev)
	}
}
