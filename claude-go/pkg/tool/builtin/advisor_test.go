package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// newAdvisorTestServer 返回一个模拟 Anthropic Messages API 的 httptest server。
// handler 返回 (advice, httpStatus)。
func newAdvisorTestServer(t *testing.T, calls *atomic.Int64, handler func(body []byte) (string, int)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		body := make([]byte, 0)
		if r.Body != nil {
			buf := new(strings.Builder)
			b := make([]byte, 4096)
			for {
				n, err := r.Body.Read(b)
				buf.Write(b[:n])
				if err != nil {
					break
				}
			}
			body = []byte(buf.String())
		}
		advice, status := handler(body)
		if status != 200 {
			w.WriteHeader(status)
			fmt.Fprint(w, advice)
			return
		}
		resp := types.APIResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []types.ContentBlock{{
				Type: types.ContentBlockText,
				Text: advice,
			}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 100, OutputTokens: 50},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func testMessages(assistantTurns int) []types.Message {
	msgs := []types.Message{{
		Type:    types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "任务: 修复 pkg/foo 的空指针 bug"}},
	}}
	for i := 0; i < assistantTurns; i++ {
		msgs = append(msgs, types.Message{
			Type: types.MessageTypeAssistant,
			Content: []types.ContentBlock{
				{Type: types.ContentBlockText, Text: fmt.Sprintf("第 %d 轮思考", i)},
				{Type: types.ContentBlockToolUse, Name: "Read", Input: json.RawMessage(`{"path":"/tmp/foo.go"}`)},
			},
		})
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeUser,
			Content: []types.ContentBlock{{Type: types.ContentBlockToolResult, Content: fmt.Sprintf("结果 %d", i)}},
		})
	}
	return msgs
}

func TestAdvisorToolBasics(t *testing.T) {
	tl := NewAdvisorTool(nil, AdvisorOptions{})
	if tl.Name() != "advisor" {
		t.Fatalf("Name = %q", tl.Name())
	}
	if !tl.IsReadOnly(nil) {
		t.Fatal("advisor 应为只读")
	}
	if tl.IsConcurrencySafe(nil) {
		t.Fatal("advisor 不应并发执行")
	}
	if pr := tl.CheckPermissions(nil, nil); pr != nil {
		t.Fatal("advisor 无需权限确认")
	}
	var schema map[string]any
	if err := json.Unmarshal(tl.InputSchema(), &schema); err != nil {
		t.Fatalf("schema 非法 JSON: %v", err)
	}
	if props, ok := schema["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Fatalf("advisor 应为无参数工具, got %v", schema)
	}
}

func TestAdvisorToolNotConfigured(t *testing.T) {
	tl := NewAdvisorTool(nil, AdvisorOptions{})
	res, err := tl.Call(context.Background(), nil, &tool.ToolContext{})
	if err != nil {
		t.Fatalf("Call 不应返回 go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "not configured") {
		t.Fatalf("未配置时应返回 IsError 结果, got %+v", res)
	}
}

func TestAdvisorToolCallSuccessAndCooldown(t *testing.T) {
	var serverCalls atomic.Int64
	var gotTranscript string
	srv := newAdvisorTestServer(t, &serverCalls, func(body []byte) (string, int) {
		gotTranscript = string(body)
		return "建议: 先写单测再改代码", 200
	})
	defer srv.Close()

	client := api.NewClient(srv.URL, "test-key", "test-model")
	tl := NewAdvisorTool(client, AdvisorOptions{CooldownTurns: 2})

	tctx := &tool.ToolContext{Messages: testMessages(3)}
	res, err := tl.Call(context.Background(), nil, tctx)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError || !strings.Contains(res.Content, "先写单测") {
		t.Fatalf("应返回建议文本, got %+v", res)
	}
	if serverCalls.Load() != 1 {
		t.Fatalf("应发起 1 次 LLM 调用, got %d", serverCalls.Load())
	}
	// 请求体应包含序列化的轨迹 (含任务定义与工具调用)
	if !strings.Contains(gotTranscript, "pkg/foo") || !strings.Contains(gotTranscript, "tool_use") {
		t.Fatalf("transcript 未包含轨迹内容: %.300s", gotTranscript)
	}

	// 冷却: assistant 轮数未推进, 第二次调用应被拒 (不打 LLM)
	res2, err := tl.Call(context.Background(), nil, tctx)
	if err != nil {
		t.Fatalf("Call2: %v", err)
	}
	if res2.IsError || !strings.Contains(res2.Content, "cooldown") {
		t.Fatalf("冷却期应返回提示, got %+v", res2)
	}
	if serverCalls.Load() != 1 {
		t.Fatalf("冷却期不应发起 LLM 调用, got %d", serverCalls.Load())
	}

	// 轮数推进 >= CooldownTurns 后恢复可用
	tctx2 := &tool.ToolContext{Messages: testMessages(5)}
	res3, err := tl.Call(context.Background(), nil, tctx2)
	if err != nil {
		t.Fatalf("Call3: %v", err)
	}
	if res3.IsError || !strings.Contains(res3.Content, "先写单测") {
		t.Fatalf("冷却结束应恢复咨询, got %+v", res3)
	}
}

func TestAdvisorToolBudgetExhausted(t *testing.T) {
	var serverCalls atomic.Int64
	srv := newAdvisorTestServer(t, &serverCalls, func([]byte) (string, int) { return "ok", 200 })
	defer srv.Close()

	client := api.NewClient(srv.URL, "test-key", "test-model")
	tl := NewAdvisorTool(client, AdvisorOptions{MaxCallsPerSession: 1, CooldownTurns: 1})

	if _, err := tl.Consult(context.Background(), testMessages(1)); err != nil {
		t.Fatalf("Consult1: %v", err)
	}
	if _, err := tl.Consult(context.Background(), testMessages(2)); err != ErrAdvisorBudgetExhausted {
		t.Fatalf("应返回 ErrAdvisorBudgetExhausted, got %v", err)
	}
	// Call 路径把预算耗尽映射为非 error 提示
	res, err := tl.Call(context.Background(), nil, &tool.ToolContext{Messages: testMessages(9)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError || !strings.Contains(res.Content, "budget exhausted") {
		t.Fatalf("预算耗尽应返回非 error 提示, got %+v", res)
	}
	if serverCalls.Load() != 1 {
		t.Fatalf("预算耗尽后不应再打 LLM, got %d", serverCalls.Load())
	}
}

func TestAdvisorToolServerErrorDegradesGracefully(t *testing.T) {
	srv := newAdvisorTestServer(t, nil, func([]byte) (string, int) {
		return `{"error":{"type":"invalid_request_error","message":"bad request"}}`, 400
	})
	defer srv.Close()

	client := api.NewClient(srv.URL, "test-key", "test-model")
	tl := NewAdvisorTool(client, AdvisorOptions{})
	res, err := tl.Call(context.Background(), nil, &tool.ToolContext{Messages: testMessages(1)})
	if err != nil {
		t.Fatalf("Call 不应抛 go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "Advisor unavailable") {
		t.Fatalf("LLM 失败应降级为 IsError 结果, got %+v", res)
	}
}

func TestAdvisorToolPromptTooLongRetriesWithHalfBudget(t *testing.T) {
	var serverCalls atomic.Int64
	srv := newAdvisorTestServer(t, &serverCalls, func([]byte) (string, int) {
		if serverCalls.Load() == 1 {
			return `{"error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens"}}`, 400
		}
		return "折减后成功", 200
	})
	defer srv.Close()

	client := api.NewClient(srv.URL, "test-key", "test-model")
	tl := NewAdvisorTool(client, AdvisorOptions{})
	advice, err := tl.Consult(context.Background(), testMessages(3))
	if err != nil {
		t.Fatalf("Consult: %v", err)
	}
	if advice != "折减后成功" {
		t.Fatalf("prompt too long 应对半折减重试, got %q", advice)
	}
	if serverCalls.Load() != 2 {
		t.Fatalf("应恰好 2 次调用, got %d", serverCalls.Load())
	}
}

func TestBuildAdvisorTranscriptRendering(t *testing.T) {
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "任务描述"}}},
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockThinking, Thinking: "内部思考不应出现"},
			{Type: types.ContentBlockText, Text: "我来处理"},
			{Type: types.ContentBlockToolUse, Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, Content: "file1\nfile2", IsError: false},
			{Type: types.ContentBlockImage, Source: &types.MediaSource{Type: "base64", Data: "xxxx"}},
		}},
	}
	out := BuildAdvisorTranscript(msgs, 10000)
	for _, want := range []string{"任务描述", "我来处理", "[tool_use] Bash", "[tool_result] file1", "[image]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript 缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "内部思考") {
		t.Fatalf("thinking 块不应进入 transcript:\n%s", out)
	}
}

func TestBuildAdvisorTranscriptSkipsAdvisorToolUse(t *testing.T) {
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "任务"}}},
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, Name: "advisor", Input: json.RawMessage(`{}`)},
		}},
	}
	out := BuildAdvisorTranscript(msgs, 10000)
	if strings.Contains(out, "advisor") {
		t.Fatalf("in-flight advisor tool_use 不应进入 transcript:\n%s", out)
	}
}

func TestBuildAdvisorTranscriptTruncation(t *testing.T) {
	msgs := []types.Message{{
		Type:    types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "首条任务定义 HEAD_MARKER"}},
	}}
	for i := 0; i < 200; i++ {
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeAssistant,
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: fmt.Sprintf("中段消息 %03d %s", i, strings.Repeat("x", 400))}},
		})
	}
	msgs = append(msgs, types.Message{
		Type:    types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "最新消息 TAIL_MARKER"}},
	})

	budgetTokens := 2000 // ≈8000 chars, 远小于 200*400
	out := BuildAdvisorTranscript(msgs, budgetTokens)

	if !strings.Contains(out, "HEAD_MARKER") {
		t.Fatal("截断应保留首条任务定义")
	}
	if !strings.Contains(out, "TAIL_MARKER") {
		t.Fatal("截断应保留最新消息")
	}
	if !strings.Contains(out, "messages omitted") {
		t.Fatal("截断应插入折叠标记")
	}
	if len(out) > budgetTokens*4+1000 {
		t.Fatalf("输出超预算: len=%d budget=%d", len(out), budgetTokens*4)
	}
}

func TestBuildAdvisorTranscriptEmpty(t *testing.T) {
	if out := BuildAdvisorTranscript(nil, 1000); !strings.Contains(out, "empty conversation") {
		t.Fatalf("空历史应有占位输出, got %q", out)
	}
}

func TestAdvisorToolThinkingOnlyFallback(t *testing.T) {
	// 模拟推理模型把输出预算全花在 thinking 块上 (无 text 块)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := types.APIResponse{
			ID: "msg_t", Type: "message", Role: "assistant",
			Content: []types.ContentBlock{{
				Type:     types.ContentBlockThinking,
				Thinking: "分析中……结论是应该先做基准测试再决定是否用 goroutine 池",
			}},
			StopReason: "max_tokens",
			Usage:      &types.Usage{InputTokens: 100, OutputTokens: 2000},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := api.NewClient(srv.URL, "test-key", "test-model")
	tl := NewAdvisorTool(client, AdvisorOptions{})
	advice, err := tl.Consult(context.Background(), testMessages(1))
	if err != nil {
		t.Fatalf("Consult: %v", err)
	}
	if !strings.Contains(advice, "基准测试") || !strings.Contains(advice, "分析过程节选") {
		t.Fatalf("应回退到 thinking 内容, got %q", advice)
	}
}

func TestTailOfRuneSafe(t *testing.T) {
	s := strings.Repeat("中", 100)
	out := tailOf(s, 10)
	if !strings.HasPrefix(out, "...") || len([]rune(out)) != 13 {
		t.Fatalf("tailOf rune 安全截断失败: %q", out)
	}
	for _, r := range out {
		if r != '.' && r != '中' {
			t.Fatalf("出现乱码 rune: %q", out)
		}
	}
}

func TestAdvisorToolEmptyAdviceUsesPlaceholder(t *testing.T) {
	srv := newAdvisorTestServer(t, nil, func([]byte) (string, int) { return "   ", 200 })
	defer srv.Close()
	client := api.NewClient(srv.URL, "test-key", "test-model")
	tl := NewAdvisorTool(client, AdvisorOptions{})
	advice, err := tl.Consult(context.Background(), testMessages(1))
	if err != nil {
		t.Fatalf("Consult: %v", err)
	}
	if advice != "[Advisor response]" {
		t.Fatalf("空建议应替换为官方占位符, got %q", advice)
	}
}
