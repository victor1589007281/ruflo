package engine

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// TestSanitizeToolPairingOnRestoredHistory 会话历史快照读回后的健壮性 (design/02 R2)。
//
// 快照虽然存的是"每轮末的完整链条", 但按 token 上限裁剪时可能出现两种断口:
//   - 尾部窗口的起点切在 tool_use 与 tool_result 之间 → 孤儿 tool_result 打头;
//   - "compact 边界 + 尾部窗口"路径丢掉中段 → 孤儿 tool_use 悬空。
//
// 这两种历史直接发给严格网关 (Kimi/OpenAI 翻译层) 会 400, 必须被 sanitizeToolPairing
// 兜住: 断口两侧的孤儿块清干净, 而普通文本与配对完整的工具往返一个都不能少。
func TestSanitizeToolPairingOnRestoredHistory(t *testing.T) {
	restored := []types.Message{
		// 断口 1: 中段被裁掉, 只剩 tool_result (对应的 tool_use 已不在历史里)
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "gone", Content: "旧结果"},
		}},
		// compact 摘要边界: 必须原样保留
		{Type: types.MessageTypeAssistant, IsCompactBoundary: true, Content: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "【历史摘要】用户在调 pkg/feishu"},
		}},
		// 断口 2: tool_use 没有对应 result (中段被裁掉)
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockThinking, Thinking: "先看文件"},
			{Type: types.ContentBlockToolUse, ID: "dangling", Name: "Read"},
		}},
		// 完整往返: 必须整对留下
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "ok1", Name: "Grep"},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "ok1", Content: "命中 3 处"},
		}},
		// 重启后用户的新消息
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "继续"},
		}},
	}

	out := sanitizeToolPairing(restored)

	var useIDs, resIDs, texts []string
	thinking := 0
	boundary := 0
	for _, m := range out {
		if m.IsCompactBoundary {
			boundary++
		}
		if len(m.Content) == 0 {
			t.Fatal("产出里出现空 content 消息 (严格网关会 400)")
		}
		for _, b := range m.Content {
			switch b.Type {
			case types.ContentBlockToolUse:
				useIDs = append(useIDs, b.ID)
			case types.ContentBlockToolResult:
				resIDs = append(resIDs, b.ToolUseID)
			case types.ContentBlockText:
				texts = append(texts, b.Text)
			case types.ContentBlockThinking:
				thinking++
			}
		}
	}
	if len(useIDs) != 1 || useIDs[0] != "ok1" {
		t.Fatalf("孤儿 tool_use 未清除干净, 剩下 %v (期望只有 ok1)", useIDs)
	}
	if len(resIDs) != 1 || resIDs[0] != "ok1" {
		t.Fatalf("孤儿 tool_result 未清除干净, 剩下 %v (期望只有 ok1)", resIDs)
	}
	if boundary != 1 {
		t.Fatalf("compact 摘要边界被吞掉了 (boundary=%d)", boundary)
	}
	if len(texts) != 2 || texts[0] != "【历史摘要】用户在调 pkg/feishu" || texts[1] != "继续" {
		t.Fatalf("普通文本被误删/乱序: %v", texts)
	}
	if thinking != 1 {
		t.Fatalf("孤儿 tool_use 所在消息的 thinking 块应保留, thinking=%d", thinking)
	}
}

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
