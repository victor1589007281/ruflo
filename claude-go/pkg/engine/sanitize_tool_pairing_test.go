package engine

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestSanitizeToolPairing(t *testing.T) {
	msgs := []types.Message{
		// 合法对: tool_use t1 + tool_result t1
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "ok"},
			{Type: types.ContentBlockToolUse, ID: "t1", Name: "Read"},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "t1", Content: "file"},
		}},
		// 孤儿 tool_use t2 (无 result) + 同消息保留 text
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "keep me"},
			{Type: types.ContentBlockToolUse, ID: "t2", Name: "Grep"},
		}},
		// 孤儿 tool_result t9 (无 use) -> 整条丢弃
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "t9", Content: "orphan"},
		}},
	}

	out := sanitizeToolPairing(msgs)

	// 统计剩余工具块
	var useIDs, resIDs []string
	textCount := 0
	for _, m := range out {
		for _, b := range m.Content {
			switch b.Type {
			case types.ContentBlockToolUse:
				useIDs = append(useIDs, b.ID)
			case types.ContentBlockToolResult:
				resIDs = append(resIDs, b.ToolUseID)
			case types.ContentBlockText:
				textCount++
			}
		}
	}
	if len(useIDs) != 1 || useIDs[0] != "t1" {
		t.Fatalf("expected only tool_use t1 kept, got %v", useIDs)
	}
	if len(resIDs) != 1 || resIDs[0] != "t1" {
		t.Fatalf("expected only tool_result t1 kept, got %v", resIDs)
	}
	if textCount != 2 { // "ok" + "keep me" 都应保留
		t.Fatalf("expected 2 text blocks preserved, got %d", textCount)
	}
	// 孤儿 tool_result 整条消息应被丢弃
	for _, m := range out {
		if len(m.Content) == 0 {
			t.Fatalf("empty message should have been dropped")
		}
	}
}
