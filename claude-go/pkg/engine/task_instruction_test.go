package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestTaskInstructionSentOnce 锁住"任务指令只发一份"这条契约。
//
// 背景: TaskInstruction 有两个消费点 —— queryLoop 把它追加到 system prompt 末尾,
// SubmitMessage 又曾在 userContent 为空时把它当成首条 user 消息的正文。两者叠加,
// 每次 llm_call 会把整份阶段提示词原样发两遍 (实测 TraceStore 里团队 stage 的 user
// 段与最后一个 system 段逐字节相同: if-question-expand 单请求 76KB 中 29KB 是重复,
// 全量 5645 次 llm_call 100% 命中)。
//
// 断言方式刻意选"数出现次数"而不是比对某一段: 重复是**位置无关**的浪费, 只要同一份
// 正文在请求体里出现两次就是 bug, 出现在 system 还是 messages 里都一样。
func TestTaskInstructionSentOnce(t *testing.T) {
	const marker = "TASK-INSTRUCTION-MARKER-9f3a"

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		resp := types.APIResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "test-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "ok"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model", Client: srv.Client()}
	e := NewQueryEngine(&Config{MaxTurns: 1}, client, tool.NewRegistry(), nil, nil, nil, prompt.NewManager(t.TempDir()))
	e.TaskInstruction = marker

	// 团队 stage 的真实调用形态: 任务指令已经由调用方放进 TaskInstruction,
	// 提交时 userContent 为空。
	for range e.SubmitMessage(context.Background(), "") {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("没有捕获到任何请求体")
	}
	body := bodies[0]
	if n := strings.Count(body, marker); n != 1 {
		t.Fatalf("任务指令在请求体中出现 %d 次, 期望 1 次 (system 一份, user 侧不得复制)\nbody=%s",
			n, truncateForTest(body, 600))
	}
	// 首条 user 消息必须仍然存在: 部分 provider 要求消息序列以 user 轮开始。
	var payload struct {
		// system 的实际形态随 provider 而异 (字符串或 content block 数组),
		// 这里只关心"非空", 形态交给 client 自己的契约测试。
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("请求体不是预期 JSON: %v", err)
	}
	if len(payload.Messages) == 0 || payload.Messages[0].Role != "user" {
		t.Fatalf("首条消息应为 user, got %+v", payload.Messages)
	}
	if !contentHasText(payload.Messages[0].Content) {
		t.Fatalf("首条 user 消息内容为空, 应保留一句指向 system 的短提示: %#v", payload.Messages[0].Content)
	}
	if len(payload.System) == 0 || string(payload.System) == "null" {
		t.Fatal("system 段为空, 任务指令无处安放")
	}
}

// contentHasText 兼容裸字符串与 content block 数组两种合法形态。
func contentHasText(content any) bool {
	switch v := content.(type) {
	case string:
		return v != ""
	case []any:
		for _, b := range v {
			if m, ok := b.(map[string]any); ok {
				if s, _ := m["text"].(string); s != "" {
					return true
				}
			}
		}
	}
	return false
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(截断)"
}
