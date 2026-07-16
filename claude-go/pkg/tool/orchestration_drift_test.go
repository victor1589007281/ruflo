package tool

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSummarizeLongToolResultInvalidUTF8Drift 复现并守住曾导致引擎 panic 的越界切片:
//
// strings.ToLower 底层 strings.Map 会把每个非法 UTF-8 字节替换成 U+FFFD(1 字节→3 字节),
// 于是当 raw 含二进制/被截断的多字节数据时, lower 会比 raw 长。旧代码用 lower 的偏移 idx
// 去切 raw(raw[idx:end]), idx 可越过 len(raw) → "slice bounds out of range [103740:100043]"。
// 修复后改切 lower(同坐标系恒合法且对齐 marker)。本测试正是那条失败路径:大段非法字节在前、
// 错误 marker 在后, marker 在 lower 中的偏移远超 len(raw)。
func TestSummarizeLongToolResultInvalidUTF8Drift(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ { // 前 20 行为短行(会被原样纳入摘要), 不含 marker
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString(strings.Repeat("\xff", 40000)) // 非法字节: lower 在此处增长约 3×
	b.WriteString("\nfatal ERROR at the very end\n")
	raw := b.String()

	if utf8.ValidString(raw) {
		t.Fatal("测试前提失效: raw 应含非法 UTF-8 字节")
	}

	const maxChars = 8000
	// 旧代码在此必 panic; 修复后正常返回。testing 会把 panic 记为测试失败。
	out := summarizeLongToolResult(raw, maxChars)

	if out == "" {
		t.Fatal("摘要不应为空")
	}
	if limit := maxChars + len("\n...(summary truncated)"); len(out) > limit {
		t.Fatalf("摘要超出 maxChars 上限: len=%d > %d", len(out), limit)
	}
	// 诊断片段应对齐到 marker: 小写化后含 "error"
	if !strings.Contains(out, "[diagnostic excerpt]") {
		t.Fatalf("摘要应含诊断片段标记")
	}
	if !strings.Contains(out, "error") {
		t.Fatalf("诊断片段应对齐到 marker(小写 error), 实际前 200 字节: %q", out[:minInt(200, len(out))])
	}
}

// TestSummarizeLongToolResultValidUTF8 常规路径(合法 UTF-8, 无漂移)仍工作:
// 不 panic、非空、有界、诊断片段对齐到 marker。
// 注: 不断言整体 UTF-8 合法——tail(raw[len-1200:]) 与末尾 out[:maxChars] 截断可能切在多字节
// rune 中间, 这是本函数固有(且早于本次修复)的行为, JSON 编码会把游离字节替换为 U+FFFD, 不崩溃。
func TestSummarizeLongToolResultValidUTF8(t *testing.T) {
	raw := strings.Repeat("正常日志行 normal log line\n", 2000) + "FAILED: something broke\n"
	if len(raw) <= 8000 {
		t.Fatalf("测试前提: raw 应超过 maxChars")
	}
	out := summarizeLongToolResult(raw, 8000)
	if out == "" {
		t.Fatal("摘要不应为空")
	}
	if limit := 8000 + len("\n...(summary truncated)"); len(out) > limit {
		t.Fatalf("摘要超出上限: len=%d > %d", len(out), limit)
	}
	if !strings.Contains(out, "[diagnostic excerpt]") || !strings.Contains(out, "failed") {
		t.Fatalf("诊断片段应对齐到 marker(小写 failed)")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
