package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// TestAnthropicToResponses 出站翻译: tool_use → function_call, tool_result →
// function_call_output, tool_choice any → required, 文本按 role 归位。
func TestAnthropicToResponses(t *testing.T) {
	req := types.APIRequest{
		Model:     "muse-spark-1.2-contributor",
		MaxTokens: 1024,
		Stream:    true,
		System:    "你是助手",
		Messages: []types.APIMessage{
			// assistant: 文本 + tool_use
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"calc","input":{"a":2,"b":2}}]`)},
			// user: tool_result + 追问
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"4"},{"type":"text","text":"and now?"}]`)},
		},
		Tools: []types.APITool{
			{Name: "calc", Description: "add two numbers", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`)},
		},
		ToolChoice: &types.APIToolChoice{Type: "any"},
	}
	rreq, err := anthropicToResponses(req, true)
	if err != nil {
		t.Fatalf("anthropicToResponses: %v", err)
	}
	if rreq.Model != "muse-spark-1.2-contributor" {
		t.Fatalf("model wrong: %s", rreq.Model)
	}
	if rreq.Instructions != "你是助手" {
		t.Fatalf("instructions wrong: %q", rreq.Instructions)
	}
	// 期望 input items: assistant-text(message) → assistant-function_call → user-function_call_output → user-text
	if len(rreq.Input) != 4 {
		t.Fatalf("want 4 input items, got %d: %s", len(rreq.Input), rawItems(t, rreq))
	}
	if string(rreq.Input[0].Raw) == "" {
		t.Fatalf("item0 raw empty")
	}
	if !strings.Contains(string(rreq.Input[1].Raw), `"function_call"`) || !strings.Contains(string(rreq.Input[1].Raw), `"toolu_1"`) || !strings.Contains(string(rreq.Input[1].Raw), `"calc"`) {
		t.Fatalf("assistant tool_use→function_call wrong: %s", rreq.Input[1].Raw)
	}
	if !strings.Contains(string(rreq.Input[2].Raw), `"function_call_output"`) || !strings.Contains(string(rreq.Input[2].Raw), `"toolu_1"`) || !strings.Contains(string(rreq.Input[2].Raw), `"4"`) {
		t.Fatalf("tool_result→function_call_output wrong: %s", rreq.Input[2].Raw)
	}
	if rreq.ToolChoice != "auto" {
		t.Fatalf("tool_choice any -> auto (上游仅支持 auto), got %v", rreq.ToolChoice)
	}
	if len(rreq.Tools) != 1 || rreq.Tools[0].Name != "calc" || rreq.Tools[0].Type != "function" {
		t.Fatalf("tools wrong: %+v", rreq.Tools)
	}
	if !rreq.Stream {
		t.Fatalf("stream not propagated")
	}
	// 序列化后确认 wire 形态无嵌套 function 对象
	b, _ := json.Marshal(rreq)
	if strings.Contains(string(b), `{"type":"function","function":`) {
		t.Fatalf("tool 用了 chat/completions 的嵌套形态: %s", b)
	}
}

// TestAnthropicToResponsesToolChoiceNone none 语义: 上游不支持 "none", 用不传
// tools 实现禁用工具 (直接发 none 上游会 400)。
func TestAnthropicToResponsesToolChoiceNone(t *testing.T) {
	req := types.APIRequest{
		Model: "muse-spark-1.2-contributor",
		Messages: []types.APIMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
		},
		Tools: []types.APITool{
			{Name: "calc", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: &types.APIToolChoice{Type: "none"},
	}
	rreq, err := anthropicToResponses(req, false)
	if err != nil {
		t.Fatalf("anthropicToResponses: %v", err)
	}
	if len(rreq.Tools) != 0 {
		t.Fatalf("tool_choice none 应清空 tools, got %+v", rreq.Tools)
	}
	b, _ := json.Marshal(rreq)
	if strings.Contains(string(b), `"tools"`) {
		t.Fatalf("none 语义不应发送 tools 字段: %s", b)
	}
}

func rawItems(t *testing.T, rreq *responsesRequest) string {
	t.Helper()
	var parts []string
	for _, it := range rreq.Input {
		parts = append(parts, string(it.Raw))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestParseOpenAIResponsesResponse 非流式解析: message + function_call + usage。
func TestParseOpenAIResponsesResponse(t *testing.T) {
	raw := `{
		"id":"resp_abc",
		"object":"response",
		"model":"muse-spark-1.2-contributor",
		"status":"completed",
		"output":[
			{"type":"reasoning","id":"rs_1"},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"答案是 4。"}]},
			{"type":"function_call","id":"fc_1","call_id":"call_9","name":"search","arguments":"{\"q\":\"tokyo\"}"}
		],
		"usage":{"input_tokens":1200,"output_tokens":80,"input_tokens_details":{"cached_tokens":900}}
	}`
	resp, err := parseOpenAIResponsesResponse("muse-spark-1.2-contributor", []byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// output 含 function_call → 必须报 tool_use (引擎据此执行工具)
	if resp.StopReason != string(types.StopReasonToolUse) {
		t.Fatalf("stop_reason want tool_use, got %q", resp.StopReason)
	}
	// reasoning 块丢弃; 期望 文本 + tool_use
	if len(resp.Content) != 2 {
		t.Fatalf("want 2 blocks (text+tool_use), got %d: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != types.ContentBlockText || resp.Content[0].Text != "答案是 4。" {
		t.Fatalf("text block wrong: %+v", resp.Content[0])
	}
	if resp.Content[1].Type != types.ContentBlockToolUse {
		t.Fatalf("block1 want tool_use, got %+v", resp.Content[1])
	}
	if resp.Content[1].ID != "call_9" || resp.Content[1].Name != "search" {
		t.Fatalf("tool_use id/name wrong: %+v", resp.Content[1])
	}
	if string(resp.Content[1].Input) != `{"q":"tokyo"}` {
		t.Fatalf("tool_use input wrong: %s", resp.Content[1].Input)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 1200 || resp.Usage.OutputTokens != 80 || resp.Usage.CacheReadInputTokens != 900 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

// TestResponsesStreamTranslator 流式翻译: 完整事件序列 → Anthropic 事件序列
// (文本块打开/关闭, tool 块打开/关闭, message_delta 收尾)。
func TestResponsesStreamTranslator(t *testing.T) {
	tr := newOpenAIResponsesStreamTranslator("muse-spark-1.2-contributor")
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.in_progress","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content":[{"type":"output_text","text":""}]}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"你好"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"世界"}`,
		`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"你好世界"}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content":[{"type":"output_text","text":"你好世界"}]}`,
		`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"你好世界"}]}}`,
		// 工具调用
		`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_9"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"q\":\"tok"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"yo\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"q\":\"tokyo\"}"}`,
		`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_9","arguments":"{\"q\":\"tokyo\"}"}}`,
		// 完成
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":50}}}}`,
		`{"type":"ping","timestamp":0}`,
	}
	var seq []string
	for _, ev := range events {
		deltas, err := tr.translate([]byte(ev))
		if err != nil {
			t.Fatalf("translate %q: %v", ev, err)
		}
		for _, d := range deltas {
			seq = append(seq, d.Type)
		}
	}
	// 期望序列 (严格串行: 文本块在 tool 块之前关闭)
	want := []string{
		"message_start",
		"content_block_start", // 文本
		"content_block_delta",
		"content_block_delta",
		"content_block_stop", // 文本关闭 (function_call item 出现前)
		"content_block_start", // 工具
		"content_block_delta",
		"content_block_delta",
		"content_block_stop", // 工具关闭
		"message_delta",
	}
	if len(seq) != len(want) {
		t.Fatalf("事件数 %d != %d\nseq=%v", len(seq), len(want), seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("seq[%d]=%q want %q (全序=%v)", i, seq[i], want[i], seq)
		}
	}
	// 校验 message_delta 携带 usage + stop_reason
	if tr.usage == nil || tr.usage.InputTokens != 100 || tr.usage.CacheReadInputTokens != 50 {
		t.Fatalf("usage not captured: %+v", tr.usage)
	}
	if tr.status != "completed" {
		t.Fatalf("status not captured: %q", tr.status)
	}
	// 翻译完成后 ping 被忽略
	deltas, err := tr.translate([]byte(`{"type":"ping"}`))
	if err != nil || len(deltas) != 0 {
		t.Fatalf("post-complete ping should be dropped, got %v err=%v", deltas, err)
	}
}
