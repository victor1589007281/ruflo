package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// --- 纯函数: 请求正文渲染 ---

// llm_call Span 的 Input 必须能还原"真正发出去的那份 prompt" ——
// design/03 §1.3 的"action 完整文本缺失"就是指它。
func TestRenderLLMRequest_还原system与消息与工具清单(t *testing.T) {
	msgs := []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`"请实现登录"`)},
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"tu1","name":"Read","input":{"path":"a.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"tu1","content":"文件内容","is_error":false}]`)},
	}
	tools := []types.APITool{{Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	got := renderLLMRequest([]string{"你是工程师", ""}, msgs, tools)

	for _, want := range []string{
		"=== system[0] ===", "你是工程师",
		"=== tools ===", "- Read (schema",
		"=== user ===", "请实现登录",
		"=== assistant ===", "[tool_use Read]",
		"[tool_result tu1] 文件内容",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染结果缺少 %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "system[1]") {
		t.Error("空 system 段不该渲染出来 (只增字节不增信息)")
	}
}

// 解析不了的 content 必须原样保留 JSON, 绝不返回空 —— 轨迹的用途是还原真实请求。
func TestRenderAPIContent_未知形态保留原文(t *testing.T) {
	if got := renderAPIContent(json.RawMessage(`{"未知":"结构"}`)); !strings.Contains(got, "未知") {
		t.Errorf("未知结构应原样保留而不是丢掉: %q", got)
	}
	if got := renderAPIContent(json.RawMessage(`null`)); got != "" {
		t.Errorf("null 应渲染为空串, got %q", got)
	}
	if got := renderAPIContent(nil); got != "" {
		t.Errorf("nil 应渲染为空串, got %q", got)
	}
	// 未知块类型至少要留下类型名, 而不是静默消失。
	if got := renderAPIContent(json.RawMessage(`[{"type":"未来的新块"}]`)); !strings.Contains(got, "未来的新块") {
		t.Errorf("未知块类型应留下类型名: %q", got)
	}
}

func TestItoa(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{{0, "0"}, {7, "7"}, {123, "123"}, {-45, "-45"}} {
		if got := itoa(c.in); got != c.want {
			t.Errorf("itoa(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

// --- writeLLMCallSpan 本体 ---

func newTraceEngine(t *testing.T) (*QueryEngine, *tracestore.Store) {
	t.Helper()
	ts := tracestore.New(statestore.NewFileStore(filepath.Join(t.TempDir(), "statestore")))
	e := &QueryEngine{}
	e.TraceStore = ts
	return e, ts
}

func TestWriteLLMCallSpan_写出llm_call种类与token计数(t *testing.T) {
	e, ts := newTraceEngine(t)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-1", NodeID: "impl", TurnID: "t3", CallID: "c9"})
	e.writeLLMCallSpan(ctx, llmCallSpan{
		Model:      "kimi-k3",
		System:     []string{"你是工程师"},
		Messages:   []types.APIMessage{{Role: "user", Content: json.RawMessage(`"干活"`)}},
		Output:     "好的, 开始",
		Usage:      &types.Usage{InputTokens: 120, OutputTokens: 30, CacheReadInputTokens: 5},
		StopReason: "end_turn", Stream: true, Attempt: 2,
	})

	spans, err := ts.ReadRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("应写出 1 条 Span, got %d", len(spans))
	}
	s := spans[0]
	if s.Kind != tracestore.KindLLMCall {
		t.Fatalf("Kind 必须是 llm_call (§4.1 五种 Kind 里缺的那一种), got %q", s.Kind)
	}
	if s.NodeID != "impl" || s.TurnID != "t3" || s.ParentID != "t3" {
		t.Errorf("trace 四元组应挂全: %+v", s)
	}
	if s.Attrs["status"] != "success" || s.Attrs["model"] != "kimi-k3" {
		t.Errorf("attrs 不对: %v", s.Attrs)
	}
	if s.Attrs["input_tokens"] != float64(120) || s.Attrs["output_tokens"] != float64(30) {
		t.Errorf("token 计数应入 attrs: %v", s.Attrs)
	}
	if s.Attrs["attempt"] != float64(2) {
		t.Error("attempt 必须记: 学习管线要能看出这是第几次重试才拿到的产出")
	}
	// prompt 全文可还原
	in, err := ts.Resolve(s.InputRef)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in, "你是工程师") || !strings.Contains(in, "干活") {
		t.Errorf("prompt 应可完整还原, got %q", in)
	}
	out, _ := ts.Resolve(s.OutputRef)
	if out != "好的, 开始" {
		t.Errorf("response 应可还原, got %q", out)
	}
}

// 失败的调用同样要入轨迹: 只在成功路径写会让学习管线看到"一次就成"的假象。
func TestWriteLLMCallSpan_失败调用也入轨迹(t *testing.T) {
	e, ts := newTraceEngine(t)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-e"})
	e.writeLLMCallSpan(ctx, llmCallSpan{Model: "m", Err: context.DeadlineExceeded})

	spans, _ := ts.ReadRun("run-e")
	if len(spans) != 1 {
		t.Fatalf("失败调用也该写 Span, got %d", len(spans))
	}
	if spans[0].Attrs["status"] != "error" {
		t.Errorf("status 应为 error, got %v", spans[0].Attrs["status"])
	}
	if s, _ := spans[0].Attrs["error"].(string); s == "" {
		t.Error("错误信息应入 attrs")
	}
}

// 未装 TraceStore 时零成本 no-op。
func TestWriteLLMCallSpan_未装轨迹底座空安全(t *testing.T) {
	var e *QueryEngine
	e.writeLLMCallSpan(context.Background(), llmCallSpan{})
	e2 := &QueryEngine{}
	e2.writeLLMCallSpan(context.Background(), llmCallSpan{})
}

func TestTruncateForAttr_超长截断(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncateForAttr(long)
	if len([]rune(got)) > 301 {
		t.Errorf("应截断到 300 + 省略号, got %d", len([]rune(got)))
	}
	if truncateForAttr("短") != "短" {
		t.Error("短串不该被改动")
	}
}

// --- 端到端: RunIsolated 真跑一次 LLM 调用后必须留下 llm_call Span ---
//
// 这是"通电"的判据: 不是测函数能写 Span, 而是测**生产路径上真的写了**。
// 团队/stage 的绝大多数 LLM 流量走 RunIsolated, 所以这一处比 queryLoop 那处更要紧。
func TestRunIsolated_产生llm_call与node两种Span(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := types.APIResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "test-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "这是模型产出"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 42, OutputTokens: 7},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ts := tracestore.New(statestore.NewFileStore(filepath.Join(t.TempDir(), "statestore")))
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model", Client: srv.Client()}
	e := NewQueryEngine(&Config{}, client, tool.NewRegistry(), nil, nil, nil, nil)
	e.TraceStore = ts

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-iso", NodeID: "implement"})
	out, err := e.RunIsolated(ctx, "请实现登录接口", IsolatedRunOptions{MaxTurns: 1, DisableTools: true})
	if err != nil {
		t.Fatalf("RunIsolated 失败: %v", err)
	}
	if !strings.Contains(out, "这是模型产出") {
		t.Fatalf("产出不对: %q", out)
	}

	spans, err := ts.ReadRun("run-iso")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, s := range spans {
		kinds[s.Kind]++
	}
	if kinds[tracestore.KindLLMCall] != 1 {
		t.Fatalf("RunIsolated 应写出 1 条 llm_call Span (通电判据), got kinds=%v", kinds)
	}
	if kinds[tracestore.KindNode] != 1 {
		t.Errorf("原有的 node Span 不该丢, got kinds=%v", kinds)
	}
	// llm_call 的 prompt 与 token 都得在
	for _, s := range spans {
		if s.Kind != tracestore.KindLLMCall {
			continue
		}
		if s.Attrs["input_tokens"] != float64(42) {
			t.Errorf("token 计数应来自真实响应, got %v", s.Attrs["input_tokens"])
		}
		if s.Attrs["isolated"] != true {
			t.Error("RunIsolated 的 llm_call 应标 isolated")
		}
		in, _ := ts.Resolve(s.InputRef)
		if !strings.Contains(in, "请实现登录接口") {
			t.Errorf("请求正文应可还原, got %q", in)
		}
	}
}

// LLM 调用失败 (HTTP 400) 时, RunIsolated 仍要留下 llm_call Span。
func TestRunIsolated_调用失败仍留Span(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
	}))
	defer srv.Close()

	ts := tracestore.New(statestore.NewFileStore(filepath.Join(t.TempDir(), "statestore")))
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(), RetryCount: 1}
	e := NewQueryEngine(&Config{}, client, tool.NewRegistry(), nil, nil, nil, nil)
	e.TraceStore = ts

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-bad"})
	if _, err := e.RunIsolated(ctx, "x", IsolatedRunOptions{MaxTurns: 1, DisableTools: true}); err == nil {
		t.Fatal("400 应返回错误")
	}
	spans, _ := ts.ReadRun("run-bad")
	var llmSpans int
	for _, s := range spans {
		if s.Kind == tracestore.KindLLMCall {
			llmSpans++
			if s.Attrs["status"] != "error" {
				t.Errorf("失败调用的 status 应为 error, got %v", s.Attrs["status"])
			}
		}
	}
	if llmSpans != 1 {
		t.Fatalf("失败调用也应留 1 条 llm_call Span, got %d (spans=%d)", llmSpans, len(spans))
	}
}
