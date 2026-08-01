package llmgw

// remote_test.go —— remote 那条腿的验收 (design/02 §3.1 实现两态 / §四 T1)。
//
// 这里刻意让 Remote 打的是**真的 llmgw.Server**(它前面再挂一个假 provider),
// 而不是一个"长得像网关"的 httptest handler:
// 要验的正是"上层 → Remote → 独立网关进程 → provider"这一跳能对上账,
// 用手写 handler 替掉网关就等于把被测对象换成了测试自己。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

func newRemoteToGateway(t *testing.T, gwURL string) *Remote {
	t.Helper()
	r, err := NewRemote(RemoteConfig{BaseURL: gwURL, Model: "kimi:k3", APIKey: "sk-client"})
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	return r
}

// Remote 必须与 Local 完全可替换: 编译期断言 + 一次真调用。
func TestRemote满足LLMGateway接口且可替换Local(t *testing.T) {
	var sawAuth, sawModel string
	up := fakeUpstream(t, &sawAuth, &sawModel)
	defer up.Close()
	stateDir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, stateDir).Handler())
	defer gw.Close()

	// 上层只持有接口, 换实现不改一行调用代码。
	var g LLMGateway = newRemoteToGateway(t, gw.URL)
	resp, err := g.Complete(context.Background(), ChatRequest{
		Messages:  []types.APIMessage{{Role: "user", Content: json.RawMessage(`"ping"`)}},
		System:    []string{"be brief"},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if txt := joinText(resp); !strings.Contains(txt, "pong") {
		t.Errorf("响应文本 = %q, 期望含 pong", txt)
	}
	// 出站到 provider 的模型名应被网关剥掉 "provider:" 前缀
	if sawModel != "k3" {
		t.Errorf("provider 侧看到模型名 %q, 期望 k3 (网关未剥前缀)", sawModel)
	}
	// 关键: 打到 provider 的凭据必须是**网关那份**, 不是客户端那份 ——
	// remote 档的收益之一就是"本进程不必持有 provider 凭据"。
	if sawAuth != "sk-kimi" {
		t.Errorf("provider 侧鉴权头 = %q, 期望网关注入的 sk-kimi", sawAuth)
	}
	rec := readFirstRecord(t, filepath.Join(stateDir, "access.jsonl"))
	if rec.Provider != "kimi" {
		t.Errorf("access.jsonl provider = %q, 期望 kimi", rec.Provider)
	}
}

// trace 四元组必须随请求头透传: server.go 那四个 Header.Get 不能又读回空值。
func TestRemote的trace四元组落到accessjsonl(t *testing.T) {
	var sawAuth, sawModel string
	up := fakeUpstream(t, &sawAuth, &sawModel)
	defer up.Close()
	stateDir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, stateDir).Handler())
	defer gw.Close()

	r := newRemoteToGateway(t, gw.URL)
	if _, err := r.Complete(context.Background(), ChatRequest{
		Messages:  []types.APIMessage{{Role: "user", Content: json.RawMessage(`"ping"`)}},
		MaxTokens: 16,
		Trace:     Trace{RunID: "run-r1", NodeID: "node-r", TurnID: "turn-2", CallID: "call-8"},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec := readFirstRecord(t, filepath.Join(stateDir, "access.jsonl"))
	if rec.RunID != "run-r1" || rec.NodeID != "node-r" || rec.TurnID != "turn-2" || rec.CallID != "call-8" {
		t.Errorf("网关未收到完整 trace 四元组: run=%q node=%q turn=%q call=%q",
			rec.RunID, rec.NodeID, rec.TurnID, rec.CallID)
	}
}

// Simple 没有请求结构体, trace 只能经 ctx —— 与 Local 同一优先级规则。
func TestRemote的Simple经ctx带trace(t *testing.T) {
	var sawAuth, sawModel string
	up := fakeUpstream(t, &sawAuth, &sawModel)
	defer up.Close()
	stateDir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, stateDir).Handler())
	defer gw.Close()

	r := newRemoteToGateway(t, gw.URL)
	ctx := WithTrace(context.Background(), Trace{RunID: "job-r7"})
	out, err := r.Simple(ctx, "sys", "user")
	if err != nil {
		t.Fatalf("Simple: %v", err)
	}
	if !strings.Contains(out, "pong") {
		t.Errorf("Simple = %q", out)
	}
	if rec := readFirstRecord(t, filepath.Join(stateDir, "access.jsonl")); rec.RunID != "job-r7" {
		t.Errorf("access.jsonl run_id = %q, 期望 job-r7", rec.RunID)
	}
}

// 未设 trace 时一个 x-cg-* 头都不该多发 (直连形态逐字节不变)。
func TestRemote未设trace时不发头(t *testing.T) {
	var sawHeaders []string
	fakeGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for k := range req.Header {
			sawHeaders = append(sawHeaders, strings.ToLower(k))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","content":[{"type":"text","text":"ok"}]}`)
	}))
	defer fakeGW.Close()

	r := newRemoteToGateway(t, fakeGW.URL)
	if _, err := r.Simple(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Simple: %v", err)
	}
	for _, h := range sawHeaders {
		if strings.HasPrefix(h, "x-cg-") {
			t.Errorf("未设 trace 却发了 %s 头", h)
		}
	}
}

// 流式是生产主路径 (QueryEngine 走 StreamMessage), 不支持就等于 remote 档不可用。
func TestRemote流式经网关拿到增量与usage(t *testing.T) {
	var sawAuth, sawModel string
	up := fakeUpstream(t, &sawAuth, &sawModel)
	defer up.Close()
	stateDir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, stateDir).Handler())
	defer gw.Close()

	r := newRemoteToGateway(t, gw.URL)
	evCh, errCh := r.Stream(context.Background(), ChatRequest{
		Messages:  []types.APIMessage{{Role: "user", Content: json.RawMessage(`"ping"`)}},
		MaxTokens: 32,
		Trace:     Trace{RunID: "run-stream"},
	})
	var text strings.Builder
	var sawStart, sawStop bool
	for ev := range evCh {
		switch ev.Type {
		case "message_start":
			sawStart = true
		case "content_block_delta":
			if ev.Delta != nil {
				text.WriteString(ev.Delta.Text)
			}
		case "message_stop":
			sawStop = true
		}
	}
	for err := range errCh {
		if err != nil {
			t.Fatalf("流式出错: %v", err)
		}
	}
	if !sawStart || !sawStop {
		t.Errorf("SSE 事件不全: start=%v stop=%v", sawStart, sawStop)
	}
	if text.String() != "hi" {
		t.Errorf("增量文本 = %q, 期望 hi", text.String())
	}
	// 流式也要能在网关侧对上账 (双边 token + trace)
	rec := readFirstRecord(t, filepath.Join(stateDir, "access.jsonl"))
	if !rec.Stream {
		t.Error("access.jsonl 未标记 stream=true")
	}
	if rec.RunID != "run-stream" {
		t.Errorf("流式 run_id = %q", rec.RunID)
	}
	if rec.InputTokens != 11 || rec.OutputTokens != 7 {
		t.Errorf("流式双边 token 记账 = in %d / out %d, 期望 11/7", rec.InputTokens, rec.OutputTokens)
	}
}

// ---------------------------------------------------------------------------
// fail-closed: 网关不可达 / 非 2xx 一律报错, 绝不静默直连 provider
// ---------------------------------------------------------------------------

// 网关不可达: 必须报错。这条测试的存在意义是防"加个 fallback 到 Local 让它更健壮"
// 这种听起来很对的改动 —— 那会让"流量经网关"变成谎话而运维毫无察觉。
func TestRemote网关不可达时报错且不回落(t *testing.T) {
	// 端口 1 上不会有服务 (即使有, 也不是我们的网关)
	r, err := NewRemote(RemoteConfig{BaseURL: "http://127.0.0.1:1", Model: "k3", APIKey: "sk"})
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	// 直连 provider 的痕迹只有一种可能来源: Remote 结构体里握着一个 api.Client。
	// 它没有, 所以这里只需断言"错误必须冒出来"。
	out, err := r.Simple(context.Background(), "s", "u")
	if err == nil {
		t.Fatalf("网关不可达却返回成功, out=%q —— 静默回落直连即为 fail-open", out)
	}
	if !strings.Contains(err.Error(), "不可达") {
		t.Errorf("错误信息未说明网关不可达: %v", err)
	}

	// 流式同样不能静默变成"空产出 + nil 错误"
	evCh, errCh := r.Stream(context.Background(), ChatRequest{
		Messages: []types.APIMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
	})
	n := 0
	for range evCh {
		n++
	}
	var streamErr error
	for e := range errCh {
		streamErr = e
	}
	if streamErr == nil {
		t.Errorf("流式在网关不可达时收到 %d 个事件且错误为 nil —— 静默成功", n)
	}
}

// 网关返回非 2xx (401 未配 token / 502 上游挂了): 必须报错并带上状态码。
func TestRemote非2xx时报错并带状态码(t *testing.T) {
	for _, status := range []int{401, 429, 500, 502} {
		badGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
		}))
		r := newRemoteToGateway(t, badGW.URL)
		_, err := r.Complete(context.Background(), ChatRequest{
			Messages: []types.APIMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
		})
		if err == nil {
			t.Errorf("网关 %d 却返回成功", status)
		} else if !strings.Contains(err.Error(), http.StatusText(status)) &&
			!strings.Contains(err.Error(), itoa(status)) {
			t.Errorf("状态 %d 的错误信息未带状态码: %v", status, err)
		}
		badGW.Close()
	}
}

// 流式在返回 200 之后中途断流: 必须报错, 不能读成"模型什么都没说"。
func TestRemote流式中途断流必须报错(t *testing.T) {
	brokenGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 发半个事件后直接劫持连接并关掉: 客户端会在读取中拿到非 EOF 错误。
		_, _ = io.WriteString(w, `data: {"type":"message_start"}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer brokenGW.Close()

	r := newRemoteToGateway(t, brokenGW.URL)
	evCh, errCh := r.Stream(context.Background(), ChatRequest{
		Messages: []types.APIMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
	})
	for range evCh {
	}
	var got error
	for e := range errCh {
		got = e
	}
	if got == nil {
		t.Error("断流后错误通道为空 —— 网络中断被伪装成'空产出'")
	}
}

// ---------------------------------------------------------------------------
// 档位开关: 默认 local (改默认会影响 8+ 个在用 :18080 的下游平台)
// ---------------------------------------------------------------------------

func TestNewFromEnv默认local(t *testing.T) {
	t.Setenv(GatewayModeEnv, "")
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "http://127.0.0.1:18081")
	gw, err := NewFromEnv(api.NewClient("http://example.invalid", "sk", "m"))
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if _, ok := gw.(*Local); !ok {
		t.Fatalf("默认档位不是 local, 而是 %T —— 默认行为被改变", gw)
	}
}

func TestNewFromEnv显式remote(t *testing.T) {
	t.Setenv(GatewayModeEnv, ModeRemote)
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "http://127.0.0.1:18081/")
	gw, err := NewFromEnv(api.NewClient("http://example.invalid", "sk", "kimi:k3"))
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	r, ok := gw.(*Remote)
	if !ok {
		t.Fatalf("档位 remote 却拿到 %T", gw)
	}
	// 地址只有一个来源: CLAUDE_GO_LLM_GATEWAY (尾斜杠已规范化)
	if r.baseURL != "http://127.0.0.1:18081" {
		t.Errorf("remote baseURL = %q", r.baseURL)
	}
	if r.model != "kimi:k3" {
		t.Errorf("remote model = %q, 期望沿用已解析的模型别名", r.model)
	}
}

// remote 档但没给网关地址: 必须报错, 不能"退回 local 先跑起来"。
func TestNewFromEnv_remote缺地址时fail_closed(t *testing.T) {
	t.Setenv(GatewayModeEnv, ModeRemote)
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "")
	gw, err := NewFromEnv(api.NewClient("http://example.invalid", "sk", "m"))
	if err == nil {
		t.Fatalf("remote 档缺地址却成功, 拿到 %T —— fail-open", gw)
	}
	if gw != nil {
		t.Errorf("报错时还返回了实现 %T", gw)
	}
}

// 档位名拼错: 报错而不是当 local 放过去 ("设了以为生效其实没生效")。
func TestNewFromEnv未知档位报错(t *testing.T) {
	t.Setenv(GatewayModeEnv, "Remote-ish")
	if _, err := NewFromEnv(api.NewClient("http://x", "sk", "m")); err == nil {
		t.Error("未知档位名被静默当成 local")
	}
}

// NewRemote 的两个必填项缺失时报错 (地址靠猜/模型靠猜都会让事后归因不可能)。
func TestNewRemote必填项(t *testing.T) {
	if _, err := NewRemote(RemoteConfig{Model: "m"}); err == nil {
		t.Error("缺 BaseURL 未报错")
	}
	if _, err := NewRemote(RemoteConfig{BaseURL: "http://x"}); err == nil {
		t.Error("缺 Model 未报错")
	}
}

// Simple/Diag 的输出上限必须与 api.Client 同一个常量 (漂移的表现是"换档就被截断")。
func TestRemote的Simple上限与api一致(t *testing.T) {
	var gotMax int
	fakeGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		gotMax = body.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":2}}`)
	}))
	defer fakeGW.Close()

	r := newRemoteToGateway(t, fakeGW.URL)
	if _, err := r.Simple(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Simple: %v", err)
	}
	if gotMax != api.SimpleCompleteMaxTokens {
		t.Errorf("max_tokens = %d, 期望 api.SimpleCompleteMaxTokens=%d", gotMax, api.SimpleCompleteMaxTokens)
	}

	// Diag 的元数据格式必须与 api.Client.CompleteDiag 逐字一致
	_, diag, err := r.Diag(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Diag: %v", err)
	}
	for _, want := range []string{"stop=end_turn", "outTok=2", "blocks=1", "textLen=2", "tail="} {
		if !strings.Contains(diag, want) {
			t.Errorf("diag 缺 %q: %s", want, diag)
		}
	}
}

// 非流式超时落在 ctx 上而不是 http.Client.Timeout: 后者会掐断正常的长流式回答。
func TestRemote超时不影响流式(t *testing.T) {
	slowGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`+"\n\n")
			if f != nil {
				f.Flush()
			}
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer slowGW.Close()

	r, err := NewRemote(RemoteConfig{BaseURL: slowGW.URL, Model: "m", Timeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	evCh, errCh := r.Stream(context.Background(), ChatRequest{
		Messages: []types.APIMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
	})
	n := 0
	for range evCh {
		n++
	}
	for e := range errCh {
		if e != nil {
			t.Fatalf("流式被非流式超时掐断: %v", e)
		}
	}
	if n != 3 {
		t.Errorf("收到 %d 个增量, 期望 3 (总时长 180ms > Timeout 80ms 却不应超时)", n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
