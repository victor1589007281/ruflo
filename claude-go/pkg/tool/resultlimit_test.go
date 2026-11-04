// resultlimit_test.go — R-H9 的"超大结果被截断并带引用"。
//
// 三条独立判据，缺一条这条验收就是名义上的：
//  1. 结果**确实**被降到上限内（否则契约只是被记录，没被执行）；
//  2. 完整内容可从引用取回（否则"截断"等于数据丢失）；
//  3. 切点不切断 UTF-8 字符（否则同一份结果在不同工具间表现不一致）。
package tool

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/types"
)

// limitedTool 实现 ResultLimiter，用来验证上限真的从工具声明流到结果组装。
type limitedTool struct {
	limit   int
	content string
}

func (t *limitedTool) Name() string                           { return "limited_tool" }
func (t *limitedTool) Description() string                    { return "declares MaxResultBytes" }
func (t *limitedTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (t *limitedTool) IsReadOnly(json.RawMessage) bool        { return true }
func (t *limitedTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (t *limitedTool) MaxResultBytes() int                    { return t.limit }
func (t *limitedTool) CheckPermissions(json.RawMessage, *ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}
func (t *limitedTool) Call(context.Context, json.RawMessage, *ToolContext) (*ToolResult, error) {
	return &ToolResult{Content: t.content}, nil
}

func runLimited(t *testing.T, limit int, content string) (string, *ToolContext) {
	t.Helper()
	tl := &limitedTool{limit: limit, content: content}
	reg := NewRegistry()
	reg.Register(tl)
	tctx := &ToolContext{Cwd: t.TempDir()} // Cwd 非空 ⇒ artifact 可落盘
	msg := RunToolUse(context.Background(), types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "limited_tool",
		Input: json.RawMessage(`{}`),
	}, reg, tctx, nil)
	if len(msg.Content) != 1 {
		t.Fatalf("期望单条 tool_result, 实得 %+v", msg.Content)
	}
	return msg.Content[0].Content, tctx
}

// TestResultLimit_未声明上限时不截断 是零值语义。
//
// 没实现 ResultLimiter 的工具必须逐字节拿到原始结果 —— 这是"接入元数据是纯增量"
// 的结构保证。若这条挂了，说明默认值不是 0，而所有存量工具都开始被静默截断。
func TestResultLimit_未声明上限时不截断(t *testing.T) {
	plain := &countingTool{} // 不实现 ResultLimiter
	if got := ResultLimitOf(plain); got != 0 {
		t.Fatalf("未声明工具的上限应为 0, 实得 %d", got)
	}
	if got := ResultLimitOf(nil); got != 0 {
		t.Fatalf("nil 工具的上限应为 0, 实得 %d", got)
	}
}

// TestResultLimit_超限被截断且可回取全文 是主验收。
func TestResultLimit_超限被截断且可回取全文(t *testing.T) {
	const limit = 400
	content := bigContent(limit * 8)
	out, _ := runLimited(t, limit, content)

	if len(out) > limit {
		t.Fatalf("结果 %d 字节, 超过声明的上限 %d —— 上限没有被执行", len(out), limit)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("截断必须显式标注, 实得:\n%s", out)
	}
	// 引用必须可回取：这是"带引用"与"直接丢掉"的分界。
	var artifactPath string
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "full_artifact: "); ok {
			artifactPath = strings.TrimSpace(v)
		}
	}
	if artifactPath == "" {
		t.Fatalf("截断结果必须给出 full_artifact 引用:\n%s", out)
	}
	got, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("artifact 读不到: %v", err)
	}
	if string(got) != content {
		t.Fatalf("artifact 内容与原文不一致 (len %d vs %d)", len(got), len(content))
	}
}

// TestResultLimit_未超限时逐字节不变 钉住"改造对正常结果零影响"。
func TestResultLimit_未超限时逐字节不变(t *testing.T) {
	content := "短结果"
	out, _ := runLimited(t, 4000, content)
	if out != content {
		t.Fatalf("未超限的结果被改动了: %q -> %q", content, out)
	}
}

// TestResultLimit_落盘失败时明说没有全文 守住"不要指向不存在的文件"。
//
// Cwd 为空 ⇒ 没有 artifact 目录。此时必须明说拿不到全文，而不是给一个空路径
// 或一个编出来的路径 —— 模型会去 Read 它并得到一条与真正问题无关的错误。
func TestResultLimit_落盘失败时明说没有全文(t *testing.T) {
	content := bigContent(4000)
	out := applyResultByteLimit("limited_tool", nil, content, &ToolContext{}, 400)

	if !strings.Contains(out, "落盘失败") {
		t.Fatalf("无法落盘时必须明说没有全文:\n%s", out)
	}
	if strings.Contains(out, "full_artifact: /") {
		t.Fatalf("落盘失败时不应给出路径:\n%s", out)
	}
	if len(out) > 400 {
		t.Fatalf("结果 %d 字节超过上限 400", len(out))
	}
}

// TestResultLimit_不切断多字节字符 守住 UTF-8 合法性。
//
// 上限刻意设成奇数，落在汉字中间 —— 一个朴素的 s[:maxBytes] 实现会在这里
// 切出非法字节序列。判据用 utf8.ValidString 而不是"看着没乱码"。
func TestResultLimit_不切断多字节字符(t *testing.T) {
	content := strings.Repeat("汉", 300) // 900 字节
	out := applyResultByteLimit("limited_tool", nil, content, &ToolContext{}, 401)

	if !utf8.ValidString(out) {
		t.Fatalf("截断结果不是合法 UTF-8 (切断了一个多字节字符)")
	}
	if len(out) > 401 {
		t.Fatalf("结果 %d 字节超过上限 401", len(out))
	}
}

// TestResultLimit_上限小于标记时退化 是边界。
//
// 上限比诊断标记本身还小时，只能返回纯前缀。此时仍然**不能**超限 ——
// 若为了塞进标记而让结果比声明的上限更长，调用方会以为上限没生效。
func TestResultLimit_上限小于标记时退化(t *testing.T) {
	content := bigContent(1000)
	out := applyResultByteLimit("limited_tool", nil, content, &ToolContext{}, 8)
	if len(out) > 8 {
		t.Fatalf("极小上限下结果仍为 %d 字节, 超过上限 8", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatalf("极小上限下切出了非法 UTF-8")
	}
}
