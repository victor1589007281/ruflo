package api

// 实测 muse-spark-1.2-contributor 走 OpenAI Responses API (/v1/responses) 经东京
// tailnet 代理 (tinyproxy 100.96.50.63:8888) 的完整链路: 非流式工具调用 + 工具结果
// 回填 + 流式文本。
//
// 运行:
//   MUSE_TEST_BASE_URL=https://opencode.ai/zen/go/v1 \
//   MUSE_TEST_API_KEY=sk-... \
//   MUSE_TEST_PROXY=http://100.96.50.63:8888 \
//   go test ./pkg/api/ -run TestLiveMuseSparkResponses -v

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestLiveMuseSparkResponses(t *testing.T) {
	baseURL := os.Getenv("MUSE_TEST_BASE_URL")
	key := os.Getenv("MUSE_TEST_API_KEY")
	proxy := os.Getenv("MUSE_TEST_PROXY")
	if baseURL == "" || key == "" {
		t.Skip("set MUSE_TEST_BASE_URL / MUSE_TEST_API_KEY")
	}

	newClient := func() *Client {
		c := NewClient(baseURL, key, "muse-spark-1.2-contributor")
		c.Protocol = ProtocolOpenAIResponses
		c.SetProxy(proxy)
		c.RetryCount = 0
		return c
	}

	tools := []types.APITool{
		{Name: "calc", Description: "把两个数相加", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`)},
	}

	// ── 0. tool_choice=any (ForceToolChoiceAny): 上游只支持 auto, 翻译层应降级而非 400 ──
	anyMsg := []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"What is 2+2? Use the calc tool."}]`)},
	}
	c0 := newClient().WithToolChoiceAny()
	resp0, err := c0.SendMessage(context.Background(), anyMsg, nil, tools, 1024)
	if err != nil {
		t.Fatalf("[tool_choice=any] SendMessage: %v", err)
	}
	t.Logf("[tool_choice=any] stop=%s blocks=%v", resp0.StopReason, blockTypes(resp0.Content))
	if resp0.StopReason != "tool_use" {
		t.Fatalf("[tool_choice=any] 期望 tool_use, 实际 stop_reason=%q content=%v", resp0.StopReason, resp0.Content)
	}

	// ── 1. 非流式: 期望返回 tool_use ──
	c := newClient()
	first := []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"What is 2+2? Use the calc tool and give the answer."}]`)},
	}
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

	// ── 2. 工具结果回填 → 期望 end_turn 文本 (验证 request 侧 function_call_output 转换) ──
	second := []types.APIMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"` + toolUse.ID + `","name":"calc","input":` + string(toolUse.Input) + `}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"` + toolUse.ID + `","content":"4"}]`)},
	}
	resp2, err := c.SendMessage(context.Background(), second, nil, tools, 1024)
	if err != nil {
		t.Fatalf("round-trip SendMessage: %v", err)
	}
	t.Logf("[round-trip] stop=%s blocks=%v", resp2.StopReason, blockTypes(resp2.Content))
	if resp2.StopReason != "end_turn" {
		t.Fatalf("回填后期望 end_turn, 实际 stop_reason=%q content=%v", resp2.StopReason, resp2.Content)
	}
	var sb strings.Builder
	for _, b := range resp2.Content {
		if b.Type == types.ContentBlockText {
			sb.WriteString(b.Text)
		}
	}
	if !strings.Contains(sb.String(), "4") {
		t.Fatalf("回填后文本应含答案 4, 实际: %q", sb.String())
	}
	t.Logf("[round-trip] text=%q", sb.String())

	// ── 3. 流式: 期望 text deltas + end_turn ──
	c2 := newClient()
	third := []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Count from 1 to 3, one number per line."}]`)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	evCh, errCh := c2.StreamMessage(ctx, third, nil, nil, 1024)
	var gotText strings.Builder
	gotStop := ""
	for ev := range evCh {
		if ev.Delta != nil {
			if ev.Delta.Type == "text_delta" {
				gotText.WriteString(ev.Delta.Text)
			}
			if ev.Delta.StopReason != "" {
				gotStop = ev.Delta.StopReason
			}
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if gotText.Len() == 0 {
		t.Fatalf("流式无文本输出")
	}
	t.Logf("[stream] stop=%q text=%q", gotStop, gotText.String())
	if gotStop != "end_turn" {
		t.Fatalf("流式期望 end_turn, 实际 %q", gotStop)
	}
	if !strings.Contains(gotText.String(), "1") || !strings.Contains(gotText.String(), "3") {
		t.Fatalf("流式文本缺数字: %q", gotText.String())
	}
}
