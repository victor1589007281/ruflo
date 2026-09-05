package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestAnthropicToOpenAI(t *testing.T) {
	req := types.APIRequest{
		Model:     "mimo-v2.5",
		MaxTokens: 1024,
		Stream:    true,
		System:    "你是助手",
		Messages: []types.APIMessage{
			// assistant: tool_use
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"calc","input":{"a":2,"b":2}}]`)},
			// user: tool_result + text
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"4"},{"type":"text","text":"and now?"}]`)},
		},
		Tools: []types.APITool{
			{Name: "calc", Description: "add two numbers", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`)},
		},
		ToolChoice: &types.APIToolChoice{Type: "any"},
	}
	oreq, err := anthropicToOpenAI(req, true)
	if err != nil {
		t.Fatalf("anthropicToOpenAI: %v", err)
	}
	if len(oreq.Messages) != 4 {
		t.Fatalf("want 4 messages (system/user-tool/user-tool/assistant), got %d: %+v", len(oreq.Messages), oreq.Messages)
	}
	if oreq.Messages[0].Role != "system" || oreq.Messages[0].Content != "你是助手" {
		t.Fatalf("system message wrong: %+v", oreq.Messages[0])
	}
	if oreq.Messages[1].Role != "assistant" {
		t.Fatalf("msg1 want assistant, got %+v", oreq.Messages[1])
	}
	if len(oreq.Messages[1].ToolCalls) != 1 || oreq.Messages[1].ToolCalls[0].ID != "toolu_1" || oreq.Messages[1].ToolCalls[0].Function.Name != "calc" {
		t.Fatalf("assistant tool_calls wrong: %+v", oreq.Messages[1].ToolCalls)
	}
	if oreq.Messages[2].Role != "tool" || oreq.Messages[2].ToolCallID != "toolu_1" {
		t.Fatalf("tool message wrong: %+v", oreq.Messages[2])
	}
	if oreq.Messages[3].Role != "user" {
		t.Fatalf("msg3 want user, got %+v", oreq.Messages[3])
	}
	if oreq.Tools[0].Type != "function" || oreq.Tools[0].Function.Name != "calc" {
		t.Fatalf("tools wrong: %+v", oreq.Tools)
	}
	if oreq.ToolChoice != "required" {
		t.Fatalf("tool_choice any -> required, got %v", oreq.ToolChoice)
	}
	if oreq.StreamOptions == nil || !oreq.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options.include_usage not set")
	}
	b, _ := json.Marshal(oreq)
	if !strings.Contains(string(b), "chat") && !strings.Contains(string(b), "stream_options") {
		t.Fatalf("marshaled body missing fields: %s", b)
	}
	t.Logf("BODY: %s", b)
}

func TestParseOpenAIResponse(t *testing.T) {
	raw := `{"id":"chatcmpl-x","model":"mimo-v2.5","choices":[{"index":0,"message":{"role":"assistant","content":"Let me calculate","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{\"a\":2,\"b\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`
	resp, err := parseOpenAIResponse("mimo-v2.5", []byte(raw))
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason want tool_use, got %s", resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("want 2 blocks (text + tool_use), got %d", len(resp.Content))
	}
	if resp.Content[0].Type != types.ContentBlockText || resp.Content[0].Text != "Let me calculate" {
		t.Fatalf("text block wrong: %+v", resp.Content[0])
	}
	if resp.Content[1].Type != types.ContentBlockToolUse || resp.Content[1].Name != "calc" || resp.Content[1].ID != "call_1" {
		t.Fatalf("tool_use block wrong: %+v", resp.Content[1])
	}
	if string(resp.Content[1].Input) != `{"a":2,"b":2}` {
		t.Fatalf("input wrong: %s", resp.Content[1].Input)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

func TestOpenAIStreamTranslator(t *testing.T) {
	tr := newOpenAIStreamTranslator("mimo-v2.5")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"Think"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calc","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":2,\"b\":2}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":99,"completion_tokens":30}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	// 事件类型序列
	var seq []string
	for _, e := range all {
		seq = append(seq, e.Type)
		if e.Type == "content_block_start" && e.ContentBlock != nil {
			seq = append(seq, "["+string(e.ContentBlock.Type)+"]")
		}
	}
	want := "message_start, content_block_start, [text], content_block_delta, content_block_stop, content_block_start, [tool_use], content_block_delta, content_block_stop, message_delta"
	if got := strings.Join(seq, ", "); got != want {
		t.Fatalf("event sequence wrong:\n got: %s\nwant: %s", got, want)
	}
	last := all[len(all)-1]
	if last.Delta == nil || last.Delta.StopReason != "tool_use" {
		t.Fatalf("last event stop_reason wrong: %+v", last)
	}
	if last.Usage == nil || last.Usage.InputTokens != 99 {
		t.Fatalf("usage not propagated: %+v", last.Usage)
	}
	// 工具块 Input 累积校验: 通过模拟引擎消费重放
	var curInput strings.Builder
	for _, e := range all {
		if e.Delta != nil && e.Delta.Type == "input_json_delta" {
			curInput.WriteString(e.Delta.PartialJSON)
		}
	}
	if curInput.String() != `{"a":2,"b":2}` {
		t.Fatalf("accumulated tool input wrong: %q", curInput.String())
	}
}

// TestOpenAIStreamTranslatorUsageTailFrame 覆盖 opencode/omen/hy3 的上游行为:
// usage 不在 finish_reason 帧里, 而在其后的 {choices:[],usage:{...}} 独立尾帧。
// message_delta 必须挂起到尾帧吸收 usage 后补发, 且 stop_reason/usage 都正确。
func TestOpenAIStreamTranslatorUsageTailFrame(t *testing.T) {
	tr := newOpenAIStreamTranslator("omen-alpha")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,       // finish 帧无 usage
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":500,"completion_tokens":88}}`, // 独立 usage 尾帧
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	last := all[len(all)-1]
	if last.Type != "message_delta" {
		t.Fatalf("last event want message_delta, got %s", last.Type)
	}
	if last.Delta == nil || last.Delta.StopReason != "max_tokens" {
		t.Fatalf("stop_reason want max_tokens (finish length), got %+v", last.Delta)
	}
	if last.Usage == nil || last.Usage.InputTokens != 500 || last.Usage.OutputTokens != 88 {
		t.Fatalf("usage not propagated from tail frame: %+v", last.Usage)
	}
	// message_delta 发完即 finished: 后续帧被忽略
	if evs, _ := tr.translate([]byte(`{"id":"c1","usage":{"prompt_tokens":9}}`)); len(evs) != 0 {
		t.Fatalf("expected no events after finished, got %d", len(evs))
	}
	// Flush 此时应为 no-op (未挂起)
	if evs := tr.Flush(); len(evs) != 0 {
		t.Fatalf("Flush after tail-frame finish should be empty, got %d", len(evs))
	}
}

// TestOpenAIStreamTranslatorFlushNoUsageTail 覆盖上游忽略 include_usage、始终不送
// usage 尾帧的退化情形: Flush() 兜底补发 message_delta, 保证引擎收到 stop_reason。
func TestOpenAIStreamTranslatorFlushNoUsageTail(t *testing.T) {
	tr := newOpenAIStreamTranslator("omen-alpha")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		// 没有 usage-only 尾帧, 直接 EOF
	}
	for _, c := range chunks {
		if _, err := tr.translate([]byte(c)); err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
	}
	evs := tr.Flush()
	if len(evs) != 1 {
		t.Fatalf("Flush want 1 deferred message_delta, got %d", len(evs))
	}
	if evs[0].Type != "message_delta" || evs[0].Delta == nil || evs[0].Delta.StopReason != "end_turn" {
		t.Fatalf("flush delta wrong: %+v", evs[0])
	}
	// 第二次 Flush 应为 no-op
	if evs := tr.Flush(); len(evs) != 0 {
		t.Fatalf("second Flush should be empty, got %d", len(evs))
	}
}
