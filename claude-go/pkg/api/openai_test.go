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
