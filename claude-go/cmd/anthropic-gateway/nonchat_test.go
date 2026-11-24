package main

// nonchat_test.go — 钉住非对话 wire (embeddings / systemone) 的网关行为。
//
// 为什么这些断言必须走**真实路由表 + 真实配置解析路径**:
//   - 上一轮"网关不支持这两个类型"的根因不在 handler 逻辑, 而在
//     (a) 模型的出站 base 与对话模型不同 (provider 只有一个 baseUrl);
//     (b) 路由表里根本没有这两个入口。
//     只测 handler 函数的话, 两个根因都测不出来 —— handler 可以完全正确而
//     路由没注册、base 仍然拼到对话端点上。
//   所以这里用 feishu.JSONConfig → modelconfig 的 production 加载路径,
//   并通过 newGatewayHandler 的完整路由表发请求。
//
// 钉的四条不变量:
//   1. 端点拼接用模型级 upstreamBaseUrl, 不是 provider.baseUrl;
//   2. 出站 model 字段剥成裸名 (provider 前缀只在本网关有意义);
//   3. opencode 上游必须带 x-opencode-session (缺了上游 400);
//   4. 协议不符/模型未声明一律失败, **不按会话回退** (向量维度与决策语义
//      都会因此静默变错)。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/feishu"
)

// capturedRequest 是假上游收到的一次请求。
type capturedRequest struct {
	Path    string
	Headers http.Header
	Body    map[string]any
}

// newFakeUpstream 起一个记录请求并回固定 JSON 的假上游。
func newFakeUpstream(t *testing.T, status int, respBody string) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var got []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got = append(got, capturedRequest{Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// newNonChatGateway 按生产路径装配网关: config JSON → feishu → modelconfig → registry/resolver.
func newNonChatGateway(t *testing.T, upstreamURL string) *gateway {
	t.Helper()
	cfgJSON := fmt.Sprintf(`{
  "providers": {
    "opencode": {
      "name": "opencode",
      "baseUrl": "%s/zen/go/v1",
      "apiKey": "sk-test",
      "models": {
        "opencode:chat-model": {"maxTokens": 1024},
        "opencode:emb-small": {"protocol": "embeddings", "upstreamBaseUrl": "%s/inference/openai/v1"},
        "opencode:dec-1": {"protocol": "systemone", "upstreamBaseUrl": "%s/inference/systemone/v1"}
      }
    }
  }
}`, upstreamURL, upstreamURL, upstreamURL)
	var jc feishu.JSONConfig
	if err := json.Unmarshal([]byte(cfgJSON), &jc); err != nil {
		t.Fatalf("解析测试配置失败: %v", err)
	}
	registry, resolver, err := modelconfig.LoadFromConfig(jc.ToModelConfigJSON())
	if err != nil {
		t.Fatalf("构建 resolver 失败: %v", err)
	}
	return &gateway{registry: registry, resolver: resolver, tryPrefix: "opencode"}
}

func doJSON(t *testing.T, h http.Handler, path string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

// TestEmbeddings_ForwardsToModelBase 钉住端点拼接与出站请求形状。
func TestEmbeddings_ForwardsToModelBase(t *testing.T) {
	up, got := newFakeUpstream(t, 200, `{"object":"list","model":"emb-small","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	h := newGatewayHandler(newNonChatGateway(t, up.URL))

	rr, out := doJSON(t, h, "/v1/embeddings", `{"model":"opencode:emb-small","input":"hello"}`)
	if rr.Code != 200 {
		t.Fatalf("期望 200, 实际 %d: %s", rr.Code, rr.Body.String())
	}
	if out["model"] != "emb-small" {
		t.Errorf("响应体应原样回传, got %v", out["model"])
	}
	if len(*got) != 1 {
		t.Fatalf("假上游应收到 1 次请求, 实际 %d", len(*got))
	}
	req := (*got)[0]
	// 关键: 用的是模型级 upstreamBaseUrl, 不是 provider.baseUrl (/zen/go/v1)。
	if req.Path != "/inference/openai/v1/embeddings" {
		t.Errorf("上游路径应由 upstreamBaseUrl 拼出, got %q", req.Path)
	}
	if got := req.Headers.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization 头错误: %q", got)
	}
	if req.Headers.Get("x-opencode-session") == "" {
		t.Error("opencode 上游必须带 x-opencode-session (缺失上游 400)")
	}
	if req.Body["model"] != "emb-small" {
		t.Errorf("出站 model 应剥成裸名, got %v", req.Body["model"])
	}
	if req.Body["input"] != "hello" {
		t.Errorf("请求体其余字段必须原样透传, got %v", req.Body["input"])
	}
}

// TestSystemOne_DecideAlias 钉住 /v1/decide 与 /v1/systemone 是同一条上游路径。
func TestSystemOne_DecideAlias(t *testing.T) {
	up, got := newFakeUpstream(t, 200, `{"model":"dec-1","answers":{"q1":{"type":"noul","noul":0.93}}}`)
	h := newGatewayHandler(newNonChatGateway(t, up.URL))

	for _, path := range []string{"/v1/decide", "/v1/systemone", "/systemone"} {
		*got = nil
		rr, _ := doJSON(t, h, path, `{"model":"opencode:dec-1","state":"hi","questions":{"q1":{"type":"noul"}}}`)
		if rr.Code != 200 {
			t.Fatalf("%s 期望 200, 实际 %d: %s", path, rr.Code, rr.Body.String())
		}
		req := (*got)[0]
		if req.Path != "/inference/systemone/v1/systemone" {
			t.Errorf("%s 上游路径错误: %q", path, req.Path)
		}
		if req.Body["model"] != "dec-1" {
			t.Errorf("出站 model 应剥成裸名, got %v", req.Body["model"])
		}
		// 决策的价值全在 questions/state 上, 一个字段都不能丢。
		if _, ok := req.Body["questions"]; !ok {
			t.Errorf("%s 请求体丢了 questions: %v", path, req.Body)
		}
		if req.Body["state"] != "hi" {
			t.Errorf("%s 请求体丢了 state: %v", path, req.Body)
		}
	}
}

// TestNonChat_ProtocolGuard 协议不符必须 400 —— 绝不能拿对话模型去答向量请求。
func TestNonChat_ProtocolGuard(t *testing.T) {
	up, got := newFakeUpstream(t, 200, `{}`)
	h := newGatewayHandler(newNonChatGateway(t, up.URL))

	rr, out := doJSON(t, h, "/v1/embeddings", `{"model":"opencode:chat-model","input":"hello"}`)
	if rr.Code != 400 {
		t.Fatalf("对话模型用于 /v1/embeddings 应 400, 实际 %d", rr.Code)
	}
	msg := fmt.Sprint(out["error"])
	if !strings.Contains(msg, "protocol") && !strings.Contains(msg, "协议") {
		t.Errorf("错误信息应说明协议不符, got %q", msg)
	}
	if len(*got) != 0 {
		t.Error("协议校验失败时不该打到上游")
	}

	// 反向同理: 向量模型不能拿来 /v1/decide。
	rr2, _ := doJSON(t, h, "/v1/decide", `{"model":"opencode:emb-small","questions":{}}`)
	if rr2.Code != 400 {
		t.Fatalf("向量模型用于 /v1/decide 应 400, 实际 %d", rr2.Code)
	}
}

// TestNonChat_UnknownModelNoFallback 未声明模型必须 404, 不回退到会话上次的模型。
func TestNonChat_UnknownModelNoFallback(t *testing.T) {
	up, got := newFakeUpstream(t, 200, `{}`)
	gw := newNonChatGateway(t, up.URL)
	// 先让会话记住一个"好模型": 对话路径的成功解析会写 lastGood。
	gw.rememberGood("sess-1", "opencode:chat-model")
	h := newGatewayHandler(gw)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings",
		strings.NewReader(`{"model":"opencode:no-such-embedding","input":"x"}`))
	req.Header.Set(claudeSessionHeader, "sess-1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 404 {
		t.Fatalf("未声明模型应 404 (不回退), 实际 %d: %s", rr.Code, rr.Body.String())
	}
	if len(*got) != 0 {
		t.Error("未知模型不该打到上游")
	}
}

// TestNonChat_UpstreamErrorPassesThrough 上游 4xx/5xx 必须带原状态与原错误体回传。
func TestNonChat_UpstreamErrorPassesThrough(t *testing.T) {
	const upstreamErr = `{"error":{"type":"INVALID_REQUEST","message":"state too long"}}`
	up, _ := newFakeUpstream(t, 422, upstreamErr)
	h := newGatewayHandler(newNonChatGateway(t, up.URL))

	rr, _ := doJSON(t, h, "/v1/decide", `{"model":"opencode:dec-1","state":"x","questions":{"q1":{"type":"noul"}}}`)
	if rr.Code != 422 {
		t.Fatalf("上游 422 应原样回传, 实际 %d", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != upstreamErr {
		t.Errorf("错误体应逐字节回传, got %q", rr.Body.String())
	}
}

// TestChatEntryRejectsNonChatModel 反方向: 向量/决策模型从对话入口进来必须 400,
// 且不落到上游 —— 否则会被拼上 /messages 或 /chat/completions, 回一个与原因无关的错误。
func TestChatEntryRejectsNonChatModel(t *testing.T) {
	up, got := newFakeUpstream(t, 200, `{}`)
	h := newGatewayHandler(newNonChatGateway(t, up.URL))

	rr, out := doJSON(t, h, "/v1/messages",
		`{"model":"opencode:emb-small","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 400 {
		t.Fatalf("/v1/messages + 向量模型应 400, 实际 %d: %s", rr.Code, rr.Body.String())
	}
	if msg := fmt.Sprint(out["error"]); !strings.Contains(msg, "/v1/embeddings") {
		t.Errorf("拒绝文案应指路 /v1/embeddings, got %q", msg)
	}

	rr2, _ := doJSON(t, h, "/v1/chat/completions", `{"model":"opencode:dec-1","messages":[]}`)
	if rr2.Code != 400 {
		t.Fatalf("/v1/chat/completions + 决策模型应 400, 实际 %d", rr2.Code)
	}
	if len(*got) != 0 {
		t.Error("被拒的请求不该打到上游")
	}
}

// TestNonChat_RoutesRegistered 路由注册本身是回归点: 这些路径曾经不存在。
func TestNonChat_RoutesRegistered(t *testing.T) {
	up, _ := newFakeUpstream(t, 200, `{}`)
	h := newGatewayHandler(newNonChatGateway(t, up.URL))
	for _, p := range []string{
		"/v1/embeddings", "/embeddings",
		"/v1/systemone", "/systemone",
		"/v1/decide", "/decide",
	} {
		rr, _ := doJSON(t, h, p, `{"model":"opencode:emb-small"}`)
		// 404 说明路由没注册; 400/200 都说明请求进了 handler。
		if rr.Code == 404 {
			t.Errorf("路径 %s 未注册 (裸 404)", p)
		}
	}
}

// TestNonChat_UpstreamBaseFallback 未配 upstreamBaseUrl 时回落 provider.baseUrl ——
// 单 base 的自建上游无需改配置。
func TestNonChat_UpstreamBaseFallback(t *testing.T) {
	rc := modelconfig.ResolvedConfig{BaseURL: "https://example.com/v1/"}
	if got := nonChatUpstreamBase(rc); got != "https://example.com/v1" {
		t.Errorf("应回落 provider.baseUrl 并去尾斜杠, got %q", got)
	}
	rc.UpstreamBaseURL = "https://example.com/emb/v1/"
	if got := nonChatUpstreamBase(rc); got != "https://example.com/emb/v1" {
		t.Errorf("应优先模型级 upstreamBaseUrl, got %q", got)
	}
}
