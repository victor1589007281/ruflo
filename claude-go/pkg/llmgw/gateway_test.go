package llmgw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// newFakeServer 造一个最小 Anthropic Messages 兼容假端点:
//   - 非流式 POST /v1/messages → 固定 APIResponse JSON (文本 "pong");
//   - 流式 (请求体 stream=true) → 最小 SSE 序列 (增量文本 "po"+"ng")。
func newFakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Stream    bool `json:"stream"`
			MaxTokens int  `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, line := range []string{
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","usage":{"input_tokens":3,"output_tokens":0}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}`,
				`data: {"type":"message_stop"}`,
			} {
				fmt.Fprintf(w, "%s\n\n", line)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "msg_1",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "text", "text": "pong"}],
			"model": "fake-model",
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 3, "output_tokens": 5}
		}`)
	})
	return httptest.NewServer(mux)
}

// newGateway 指向假端点的本地网关 (client POST 到 BaseURL+"/messages")。
func newGateway(srv *httptest.Server) *Local {
	return NewLocal(api.NewClient(srv.URL+"/v1", "test-key", "fake-model"))
}

// userMsg 构造单条 user 文本消息。
func userMsg(text string) []types.APIMessage {
	return []types.APIMessage{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":` + mustJSON(text) + `}]`),
	}}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestLocalComplete(t *testing.T) {
	srv := newFakeServer(t)
	defer srv.Close()
	gw := newGateway(srv)

	resp, err := gw.Complete(context.Background(), ChatRequest{
		Messages:  userMsg("ping"),
		System:    []string{"你是回声服务"},
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != types.ContentBlockText {
		t.Fatalf("响应内容块异常: %+v", resp.Content)
	}
	if resp.Content[0].Text != "pong" {
		t.Fatalf("文本应为 pong, got=%q", resp.Content[0].Text)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason 异常: %q", resp.StopReason)
	}
	if resp.Usage == nil || resp.Usage.OutputTokens != 5 {
		t.Fatalf("usage 异常: %+v", resp.Usage)
	}
}

func TestLocalSimple(t *testing.T) {
	srv := newFakeServer(t)
	defer srv.Close()
	gw := newGateway(srv)

	text, err := gw.Simple(context.Background(), "你是回声服务", "ping")
	if err != nil {
		t.Fatalf("Simple 失败: %v", err)
	}
	if text != "pong" {
		t.Fatalf("Simple 应返回 pong, got=%q", text)
	}
}

func TestLocalStream(t *testing.T) {
	srv := newFakeServer(t)
	defer srv.Close()
	gw := newGateway(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eventCh, errCh := gw.Stream(ctx, ChatRequest{Messages: userMsg("ping"), MaxTokens: 128})

	var sb strings.Builder
	var kinds []string
	for ev := range eventCh {
		kinds = append(kinds, ev.Type)
		if ev.Delta != nil {
			sb.WriteString(ev.Delta.Text)
		}
	}
	if err, ok := <-errCh; ok && err != nil {
		t.Fatalf("Stream 不应报错: %v", err)
	}
	if sb.String() != "pong" {
		t.Fatalf("流式拼接文本应为 pong, got=%q", sb.String())
	}
	if len(kinds) != 4 || kinds[0] != "message_start" || kinds[3] != "message_stop" {
		t.Fatalf("事件序列异常: %v", kinds)
	}
}

// TestLocalStreamError 错误传播: 假端点返回 400 (非可重试), errCh 应收到错误。
func TestLocalStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad tools"}}`)
	}))
	defer srv.Close()
	gw := newGateway(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eventCh, errCh := gw.Stream(ctx, ChatRequest{Messages: userMsg("ping"), MaxTokens: 8})

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Fatalf("应收到含 400 的错误, got=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("等待错误超时")
	}
	// 事件通道随后关闭, 无事件
	if ev, ok := <-eventCh; ok {
		t.Fatalf("错误路径不应有事件: %+v", ev)
	}

	// Complete 同样传播错误
	if _, err := gw.Complete(ctx, ChatRequest{Messages: userMsg("ping"), MaxTokens: 8}); err == nil {
		t.Fatalf("Complete 对 400 应报错")
	}
}
