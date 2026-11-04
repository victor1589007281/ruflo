// Compact 系统单元测试
// 测试: 微压缩、compact boundary
package unit

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestMicroCompact 测试微压缩 (折叠大输出)
//
// 第二个参数是 protectFrom (本轮新增消息的起始下标), 不是阈值 —— 阈值是包级常量
// compact.MicroCompactThreshold。本测试曾按旧签名 (第二参 = maxResultChars) 调用,
// 传 50000 后 from=50000 > len(messages)=1, 整段被判定为"保护区"而原样返回,
// 于是断言必然失败。签名在 f04acc12f 改成 protectFrom 时漏改了这里。
func TestMicroCompact(t *testing.T) {
	// 构造一个包含超大 tool_result 的消息
	largeContent := make([]byte, 60000)
	for i := range largeContent {
		largeContent[i] = 'A'
	}

	messages := []types.Message{
		{
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{
				{
					Type:      types.ContentBlockToolResult,
					ToolUseID: "test-1",
					Content:   string(largeContent),
				},
			},
		},
	}

	result := compact.MicroCompact(messages, 0)

	if len(result) != 1 {
		t.Fatalf("期望 1 条消息, 实际 %d", len(result))
	}

	content := result[0].Content[0].Content
	if len(content) > compact.MicroCompactKeepBytes+1024 {
		t.Errorf("微压缩后内容仍过大: %d 字符", len(content))
	}
	if !containsSubstr(content, "[tool_result 已折叠]") {
		t.Errorf("折叠后应包含折叠标记, 实际: %.200s", content)
	}
	if !containsSubstr(content, "call_id=test-1") {
		t.Error("折叠后必须保留 call_id (上游按 id 配对 tool_use/tool_result)")
	}
}

// TestMicroCompactProtectsEmittedHistory 钉住 protectFrom 语义:
// 下标之前的消息一字节都不能动 (前缀缓存按字节比对), 只有 [from, len) 会被折叠。
func TestMicroCompactProtectsEmittedHistory(t *testing.T) {
	big := make([]byte, 60000)
	for i := range big {
		big[i] = 'B'
	}
	messages := []types.Message{
		{
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{
				{Type: types.ContentBlockToolResult, ToolUseID: "old-1", Content: string(big)},
			},
		},
		{
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{
				{Type: types.ContentBlockToolResult, ToolUseID: "new-1", Content: string(big)},
			},
		},
	}

	result := compact.MicroCompact(messages, 1)

	if result[0].Content[0].Content != string(big) {
		t.Error("protectFrom 之前的历史被改写了 (会让前缀缓存从改写点起全废)")
	}
	if result[1].Content[0].Content == string(big) {
		t.Error("protectFrom 之后的新消息未被折叠")
	}
}

// TestGetMessagesAfterCompactBoundary 测试获取 compact boundary 之后的消息
func TestGetMessagesAfterCompactBoundary(t *testing.T) {
	messages := []types.Message{
		{Type: types.MessageTypeUser, UUID: "1"},
		{Type: types.MessageTypeAssistant, UUID: "2"},
		{Type: types.MessageTypeSystem, UUID: "3", IsCompactBoundary: true},
		{Type: types.MessageTypeUser, UUID: "4"},
		{Type: types.MessageTypeAssistant, UUID: "5"},
	}

	result := compact.GetMessagesAfterCompactBoundary(messages)
	if len(result) != 3 { // boundary + 2 after
		t.Errorf("期望 3 条消息, 实际 %d", len(result))
	}
	if result[0].UUID != "3" {
		t.Errorf("第一条应是 boundary, 实际 UUID=%s", result[0].UUID)
	}
}

// TestNoCompactBoundary 没有 boundary 时返回所有消息
func TestNoCompactBoundary(t *testing.T) {
	messages := []types.Message{
		{Type: types.MessageTypeUser, UUID: "1"},
		{Type: types.MessageTypeAssistant, UUID: "2"},
	}

	result := compact.GetMessagesAfterCompactBoundary(messages)
	if len(result) != 2 {
		t.Errorf("无 boundary 时应返回所有消息, 实际 %d", len(result))
	}
}
