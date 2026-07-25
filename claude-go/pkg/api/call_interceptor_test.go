package api

// call_interceptor_test —— LLM 调用切面的链语义验收 (design/01 §4.10)。
//
// 这些用例钉的是"链本身不会骗人": 吞掉调用、重复发请求、panic 三种第三方误用都必须
// 变成**点名到拦截器**的错误, 而不是静默的成功或整进程崩溃。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// ciExec 一个记账用的终点: 记录被调次数, 回一份带 token 的记录。
func ciExec(hits *int, tokens int) CallExec {
	return func(_ context.Context, _ LLMCall) (LLMResult, error) {
		*hits++
		return LLMResult{
			Response: &types.APIResponse{ID: "resp"},
			Record:   LLMCallRecord{TotalTokens: tokens, Status: "success"},
		}, nil
	}
}

func TestCall链顺序按注册序最外层在前(t *testing.T) {
	var order []string
	mk := func(name string) CallInterceptor {
		return FuncCallInterceptor{N: name, Fn: func(ctx context.Context, c LLMCall, next CallExec) (LLMResult, error) {
			order = append(order, name+"-in")
			r, err := next(ctx, c)
			order = append(order, name+"-out")
			return r, err
		}}
	}
	hits := 0
	_, err := chainCall([]CallInterceptor{mk("a"), mk("b")}, ciExec(&hits, 0))(context.Background(), LLMCall{})
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	want := []string{"a-in", "b-in", "b-out", "a-out"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("链顺序错: got=%v want=%v", order, want)
	}
	if hits != 1 {
		t.Errorf("终点应恰好被调 1 次, got=%d", hits)
	}
}

func TestCall不调next且不报错被点名拒绝(t *testing.T) {
	hits := 0
	swallow := FuncCallInterceptor{N: "swallow", Fn: func(_ context.Context, _ LLMCall, _ CallExec) (LLMResult, error) {
		return LLMResult{}, nil // 忘了调 next 却报成功
	}}
	_, err := chainCall([]CallInterceptor{swallow}, ciExec(&hits, 0))(context.Background(), LLMCall{})
	if !errors.Is(err, errCallNextNotCalled) {
		t.Fatalf("应报「未调用 next」: %v", err)
	}
	if !strings.Contains(err.Error(), "swallow") {
		t.Errorf("错误必须点名拦截器: %v", err)
	}
	if hits != 0 {
		t.Errorf("终点不该被调: %d", hits)
	}
}

func TestCall明示拒绝是正当用法(t *testing.T) {
	hits := 0
	deny := FuncCallInterceptor{N: "deny", Fn: func(_ context.Context, _ LLMCall, _ CallExec) (LLMResult, error) {
		return LLMResult{}, errors.New("预算不足, 拒绝调用")
	}}
	_, err := chainCall([]CallInterceptor{deny}, ciExec(&hits, 0))(context.Background(), LLMCall{})
	if err == nil || errors.Is(err, errCallNextNotCalled) {
		t.Fatalf("明示拒绝应原样返回自己的错误, 不该被当成「忘了调 next」: %v", err)
	}
	if err.Error() != "预算不足, 拒绝调用" {
		t.Errorf("错误原文被改写: %v", err)
	}
	if hits != 0 {
		t.Errorf("终点不该被调: %d", hits)
	}
}

func TestCall重复调next被点名拒绝(t *testing.T) {
	hits := 0
	double := FuncCallInterceptor{N: "double", Fn: func(ctx context.Context, c LLMCall, next CallExec) (LLMResult, error) {
		_, _ = next(ctx, c)
		return next(ctx, c) // 第二次 = 同一次逻辑调用发两次 HTTP
	}}
	_, err := chainCall([]CallInterceptor{double}, ciExec(&hits, 0))(context.Background(), LLMCall{})
	if !errors.Is(err, errCallNextTwice) {
		t.Fatalf("应报「重复调用 next」: %v", err)
	}
	if !strings.Contains(err.Error(), "double") {
		t.Errorf("错误必须点名拦截器: %v", err)
	}
	if hits != 1 {
		t.Errorf("真实请求只该发出 1 次 (第二次被链挡住), got=%d", hits)
	}
}

// TestCall重复调next即使被吞错也报 —— 拦截器把 guarded 的错误吃掉还报成功时,
// 链仍要响: 否则"发了两次 HTTP"就彻底不可见了。
func TestCall重复调next即使被吞错也报(t *testing.T) {
	hits := 0
	sneaky := FuncCallInterceptor{N: "sneaky", Fn: func(ctx context.Context, c LLMCall, next CallExec) (LLMResult, error) {
		r, _ := next(ctx, c)
		_, _ = next(ctx, c) // 错误被丢弃
		return r, nil       // 报成功
	}}
	_, err := chainCall([]CallInterceptor{sneaky}, ciExec(&hits, 0))(context.Background(), LLMCall{})
	if !errors.Is(err, errCallNextTwice) {
		t.Fatalf("链应自己发现重复调用: %v", err)
	}
}

func TestCall拦截器panic不穿透也不放行(t *testing.T) {
	hits := 0
	boom := FuncCallInterceptor{N: "boom", Fn: func(_ context.Context, _ LLMCall, _ CallExec) (LLMResult, error) {
		panic("第三方拦截器炸了")
	}}
	res, err := chainCall([]CallInterceptor{boom}, ciExec(&hits, 0))(context.Background(), LLMCall{})
	if err == nil {
		t.Fatal("panic 必须转成错误 (fail-closed), 不能当成放行")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "panic") {
		t.Errorf("错误应点名拦截器与 panic: %v", err)
	}
	if res.Response != nil {
		t.Error("panic 后不该带出半截响应")
	}
}

func TestCall校验拒绝nil空名重名(t *testing.T) {
	if err := ValidateCallInterceptors([]CallInterceptor{nil}); err == nil {
		t.Error("nil 应被拒")
	}
	if err := ValidateCallInterceptors([]CallInterceptor{FuncCallInterceptor{N: "  "}}); err == nil {
		t.Error("空名应被拒")
	}
	dup := FuncCallInterceptor{N: "same"}
	if err := ValidateCallInterceptors([]CallInterceptor{dup, dup}); err == nil {
		t.Error("重名应被拒")
	}
	if err := ValidateCallInterceptors([]CallInterceptor{FuncCallInterceptor{N: "a"}, FuncCallInterceptor{N: "b"}}); err != nil {
		t.Errorf("合法链不该被拒: %v", err)
	}
}

func TestCall进程级注册幂等且可摘(t *testing.T) {
	t.Cleanup(func() { UnregisterCallInterceptor("t-idem") })
	ic := FuncCallInterceptor{N: "t-idem"}
	for i := 0; i < 3; i++ {
		if err := RegisterCallInterceptor(ic); err != nil {
			t.Fatalf("注册失败: %v", err)
		}
	}
	n := 0
	for _, name := range GlobalCallInterceptorNames() {
		if name == "t-idem" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("幂等注册后应只有 1 份, got=%d", n)
	}
	UnregisterCallInterceptor("t-idem")
	for _, name := range GlobalCallInterceptorNames() {
		if name == "t-idem" {
			t.Error("摘除后仍在链上")
		}
	}
}

// ciTestServer 一个最小的 Messages API 服务端 (回一条固定文本 + usage)。
func ciTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":11,"output_tokens":7}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ciTestClient(srv *httptest.Server) *Client {
	return &Client{
		BaseURL: srv.URL, APIKey: "k", Model: "m", RetryCount: 0,
		// Client.Client 必须给: sendMessageDirect 直接用它发请求, nil 会 panic
		// (这是 api.Client 的既有前提, 生产由 NewClient 填)。
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

// TestCall空链时SendMessage原路直通 —— 空链不该改变任何行为, 也不该付构造成本。
func TestCall空链时SendMessage原路直通(t *testing.T) {
	if n := len(GlobalCallInterceptorNames()); n != 0 {
		t.Skipf("进程级链非空 (%d 个), 本例只在空链下有意义", n)
	}
	c := ciTestClient(ciTestServer(t))
	if c.callChain() != nil {
		t.Fatal("未装配时 callChain 应为 nil (空链零开销的前提)")
	}
	resp, err := c.SendMessage(context.Background(), []types.APIMessage{{Role: "user"}}, nil, nil, 16)
	if err != nil {
		t.Fatalf("空链直通应成功: %v", err)
	}
	if resp == nil || len(resp.Content) == 0 || resp.Content[0].Text != "pong" {
		t.Errorf("响应不对: %+v", resp)
	}
}

// TestCall链上能拿到真实LLMCallRecord —— 这是 §4.10 说的"统一观测": 限流等待 /
// 熔断状态 / 三类 token 全在 Record 里, 第三方不必去读 Client 的私有字段。
//
// 记录是经 ctx 记录槽从 emit 单点回传的; 若那条线断了, 这里的 TotalTokens 会是 0,
// 而 0 与"这次没花 token"不可区分 —— 所以这一条必须真断言到非零。
func TestCall链上能拿到真实LLMCallRecord(t *testing.T) {
	c := ciTestClient(ciTestServer(t))
	var got LLMCallRecord
	var sawModel string
	c.CallInterceptors = []CallInterceptor{FuncCallInterceptor{N: "probe",
		Fn: func(ctx context.Context, call LLMCall, next CallExec) (LLMResult, error) {
			sawModel = call.Model
			res, err := next(ctx, call)
			got = res.Record
			return res, err
		}}}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-9", NodeID: "s7"})
	if _, err := c.SendMessage(ctx, []types.APIMessage{{Role: "user"}}, []string{"sys"}, nil, 16); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if sawModel != "m" {
		t.Errorf("LLMCall.Model = %q, 期望 m", sawModel)
	}
	if got.TotalTokens != 18 {
		t.Errorf("Record.TotalTokens = %d, 期望 18 (11 in + 7 out)", got.TotalTokens)
	}
	if got.Status != "success" {
		t.Errorf("Record.Status = %q", got.Status)
	}
	// trace 四元组必须已经盖在记录上 (design/03 §4.1: llm.jsonl ↔ 团队轨迹的关联键)。
	if got.RunID != "run-9" || got.NodeID != "s7" {
		t.Errorf("Record trace 归因丢了: run=%q node=%q", got.RunID, got.NodeID)
	}
}

// TestCall链非空时TokenLedger端到端入账 —— 台账 + 真实 Client, 一路走通。
func TestCall链非空时TokenLedger端到端入账(t *testing.T) {
	c := ciTestClient(ciTestServer(t))
	l := NewTokenLedger()
	c.CallInterceptors = []CallInterceptor{l}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-e2e", NodeID: "n1"})
	for i := 0; i < 2; i++ {
		if _, err := c.SendMessage(ctx, []types.APIMessage{{Role: "user"}}, nil, nil, 16); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
	}
	if got := l.Tokens("run-e2e", "n1"); got != 36 {
		t.Errorf("两次调用应累计 36 token, got=%d", got)
	}
}

func TestTokenLedger按trace四元组归因(t *testing.T) {
	l := NewTokenLedger()
	hits := 0
	chain := chainCall([]CallInterceptor{l}, ciExec(&hits, 120))
	ctxCall := LLMCall{Trace: trace.IDs{RunID: "run-1", NodeID: "s0"}}
	if _, err := chain(context.Background(), ctxCall); err != nil {
		t.Fatal(err)
	}
	if _, err := chain(context.Background(), ctxCall); err != nil {
		t.Fatal(err)
	}
	other := LLMCall{Trace: trace.IDs{RunID: "run-1", NodeID: "s1"}}
	if _, err := chain(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if got := l.Tokens("run-1", "s0"); got != 240 {
		t.Errorf("(run-1,s0) 累计应为 240, got=%d", got)
	}
	if got := l.RunTokens("run-1"); got != 360 {
		t.Errorf("run-1 全量应为 360, got=%d", got)
	}
	// 无 RunID 的调用单独记 —— 丢弃会让"总量对不上"无从判断是漏记还是真没花。
	if _, err := chain(context.Background(), LLMCall{}); err != nil {
		t.Fatal(err)
	}
	if got := l.Unattributed(); got != 120 {
		t.Errorf("无归因合计应为 120, got=%d", got)
	}
	l.Forget("run-1")
	if got := l.RunTokens("run-1"); got != 0 {
		t.Errorf("Forget 后应清零, got=%d", got)
	}
	if got := l.Unattributed(); got != 120 {
		t.Errorf("Forget 不该动无归因账: got=%d", got)
	}
}

// TestTokenLedger零token不入账 —— "报 0" 与 "没回报" 必须可区分, 这与
// pkg/graph BudgetManager 里同一条取舍一致 (报 0 会让未实现的用量回报看起来像免费)。
func TestTokenLedger零token不入账(t *testing.T) {
	l := NewTokenLedger()
	hits := 0
	if _, err := chainCall([]CallInterceptor{l}, ciExec(&hits, 0))(
		context.Background(), LLMCall{Trace: trace.IDs{RunID: "r", NodeID: "n"}}); err != nil {
		t.Fatal(err)
	}
	if got := l.Tokens("r", "n"); got != 0 {
		t.Errorf("零 token 不该入账: %d", got)
	}
}
