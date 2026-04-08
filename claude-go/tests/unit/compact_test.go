// Compact 系统单元测试
// 测试: 微压缩、compact boundary
package unit

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestMicroCompact 测试微压缩 (截断大输出)
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

	result := compact.MicroCompact(messages, 50000)

	if len(result) != 1 {
		t.Fatalf("期望 1 条消息, 实际 %d", len(result))
	}

	content := result[0].Content[0].Content
	if len(content) > 51000 { // 50000 + truncation message
		t.Errorf("微压缩后内容仍过大: %d 字符", len(content))
	}
	if !containsSubstr(content, "truncated") {
		t.Error("截断后应包含 truncated 标记")
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
