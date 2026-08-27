package api

// 实测 mimo-v2.5 走 OpenAI 协议 (chat/completions) 的工具调用。
// 运行: MIMO_TEST_BASE_URL=<gateway|opencode> MIMO_TEST_API_KEY=sk-... go test ./pkg/api/ -run TestLiveMimoToolUse -v

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestLiveMimoToolUse(t *testing.T) {
	baseURL := os.Getenv("MIMO_TEST_BASE_URL")
	key := os.Getenv("MIMO_TEST_API_KEY")
	if baseURL == "" || key == "" {
		t.Skip("set MIMO_TEST_BASE_URL / MIMO_TEST_API_KEY")
	}
	c := NewClient(baseURL, key, "mimo-v2.5")
	c.Protocol = ProtocolOpenAI
	c.RetryCount = 0

	tools := []types.APITool{
		{Name: "calc", Description: "把两个数相加", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`)},
	}
	first := []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"What is 2+2? Use the calc tool and give the answer."}]`)},
	}

	// ── 非流式: 期望返回 tool_use ──
	resp, err := c.SendMessage(context.Background(), first, nil, tools, 1024)
	if err != nil {
		t.Fatalf("non-stream SendMessage: %v", err)
	}
	t.Logf("[non-stream] stop=%s blocks=%v", resp.StopReason, blockTypes(resp.Content))
	if resp.StopReason != "tool_use" {
		t.Fatalf("非流式期望 tool_use, 实际 stop_reason=%q content=%v", resp.StopReason, resp.Content)
	}
	var toolUse *types.ContentBlock
	for i := range resp.Content {
		if resp.Content[i].Type == types.ContentBlockToolUse {
			toolUse = &resp.Content[i]
		}
	}
	if toolUse == nil {
		t.Fatalf("非流式无 tool_use 块: %+v", resp.Content)
	}
	t.Logf("[non-stream] tool_use name=%s id=%s input=%s", toolUse.Name, toolUse.ID, toolUse.Input)

	// ── 工具结果回填 → 期望 end_turn 文本 (验证 request 侧 tool_result 转换) ──
	second := []types.APIMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"` + toolUse.ID + `","name":"calc","input":` + string(toolUse.Input) + `}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"` + toolUse.ID + `","content":"4"}]`)},
	}
	resp2, err := c.SendMessage(context.Background(), second, nil, tools, 1024)
	if err != nil {
		t.Fatalf("round-trip SendMessage: %v", err)
	}
	t.Logf("[round-trip] stop=%s blocks=%v", resp2.StopReason, blockTypes(resp2.Content))
	if resp2.StopReason == "" {
		t.Fatalf("round-trip 空 stop_reason")
	}

	// ── 流式: 期望文本 + 工具事件 ──
	evCh, errCh := c.StreamMessage(context.Background(), first, nil, tools, 1024)
	var text strings.Builder
	var toolNames []string
	var stop string
	for ev := range evCh {
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == types.ContentBlockToolUse {
				toolNames = append(toolNames, ev.ContentBlock.Name)
			}
		case "content_block_delta":
			if ev.Delta != nil {
				if ev.Delta.Type == "text_delta" {
					text.WriteString(ev.Delta.Text)
				}
				if ev.Delta.StopReason != "" {
					stop = ev.Delta.StopReason
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stream err: %v", err)
	}
	t.Logf("[stream] stop=%s tools=%v text=%q", stop, toolNames, text.String())
	if stop != "tool_use" || len(toolNames) == 0 {
		t.Fatalf("流式期望 tool_use + 工具块, 实际 stop=%q tools=%v", stop, toolNames)
	}
}

func blockTypes(blocks []types.ContentBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, string(b.Type))
	}
	return out
}
