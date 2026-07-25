package dashboard

// llm_gateway_test.go — design/02 §3.1 L1「最后一跳」的断言。
//
// 挡两个具体缺陷:
//  ① dashboard 的 env 直通路径 (DASHBOARD_LLM_BASE_URL / ANTHROPIC_BASE_URL)
//     不经 ConfigResolver, 于是设了 CLAUDE_GO_LLM_GATEWAY 也照旧直连 provider
//     —— 网关"接上了"但这条链路悄悄绕开;
//  ② LLMComplete 直接调 api.Client.SimpleComplete, LLMGateway 接口无生产调用方。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/llmgw"
)

// resetLLM 把全局共享客户端清干净 (本包的 LLM 客户端是包级单例)。
func resetLLM(t *testing.T) {
	t.Helper()
	ResetSharedLLMClient()
	t.Cleanup(ResetSharedLLMClient)
}

// ① env 直通路径也必须改指网关。
func TestL1_env直通路径也走网关(t *testing.T) {
	resetLLM(t)
	t.Setenv("DASHBOARD_LLM_API_KEY", "sk-test")
	t.Setenv("DASHBOARD_LLM_BASE_URL", "https://provider.example.com/anthropic")
	t.Setenv("DASHBOARD_LLM_MODEL", "m1")
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "http://127.0.0.1:18081/")

	client, profile, err := GetSharedLLMClient()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if client.BaseURL != "http://127.0.0.1:18081" {
		t.Errorf("BaseURL = %q, 期望改指网关 http://127.0.0.1:18081 (env 路径绕过了 ApplyGatewayOverride)", client.BaseURL)
	}
	if profile.Gateway != "http://127.0.0.1:18081" {
		t.Errorf("profile.Gateway = %q, 期望回报经网关 (/api/llm/status 需要能看出来)", profile.Gateway)
	}
}

// 未设网关时行为一字不变 —— 这是 8+ 下游平台的兼容底线。
// 顺带钉住 DashScope 端点的尾部修正 (/anthropic → /anthropic/v1) 仍在,
// 且修正发生在网关覆盖**之前** (顺序反了会把 /v1 补到网关地址上)。
func TestL1_未设网关时端点与尾部修正不变(t *testing.T) {
	resetLLM(t)
	t.Setenv("DASHBOARD_LLM_API_KEY", "sk-test")
	t.Setenv("DASHBOARD_LLM_BASE_URL", "https://dashscope.aliyuncs.com/api/v2/apps/anthropic")
	t.Setenv("DASHBOARD_LLM_MODEL", "m1")
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "")

	client, profile, err := GetSharedLLMClient()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if client.BaseURL != "https://dashscope.aliyuncs.com/api/v2/apps/anthropic/v1" {
		t.Errorf("BaseURL = %q, 期望保留 /anthropic → /anthropic/v1 的尾部修正", client.BaseURL)
	}
	if profile.Gateway != "" {
		t.Errorf("未设网关却回报 Gateway=%q", profile.Gateway)
	}
}

// ② LLMGateway 接口有生产调用方: LLMComplete 经它发请求。
// 用真 HTTP 假上游, 断言请求确实打出去了 —— 只断言"返回了字符串"证明不了链路。
func TestL1_LLMComplete经网关接口发出(t *testing.T) {
	resetLLM(t)
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"诊断结论"}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	// 注入形态 = 生产里飞书 bot 调 SetSharedLLMClient 的那条路。
	SetSharedLLMClient(api.NewClient(upstream.URL, "sk", "m"))

	gw, _, err := SharedGateway()
	if err != nil {
		t.Fatalf("SharedGateway: %v", err)
	}
	if _, ok := gw.(llmgw.LLMGateway); !ok {
		t.Fatal("SharedGateway 返回的不是 llmgw.LLMGateway")
	}

	out, profile, err := LLMComplete(context.Background(), "sys", "user", 5*time.Second)
	if err != nil {
		t.Fatalf("LLMComplete: %v", err)
	}
	if !strings.Contains(out, "诊断结论") {
		t.Errorf("输出 = %q", out)
	}
	if hits == 0 {
		t.Error("上游未收到请求 —— 链路没真走通")
	}
	if profile.Source != "injected:bot" {
		t.Errorf("profile.Source = %q, 期望 injected:bot", profile.Source)
	}
}

// 网关视图必须跟着客户端换: 重新注入后不能还包着老 client
// (否则热更换模型配置之后, 诊断还打老端点)。
func TestL1_重新注入后网关视图跟着换(t *testing.T) {
	resetLLM(t)
	a := api.NewClient("http://a.example", "k", "m")
	b := api.NewClient("http://b.example", "k", "m")

	SetSharedLLMClient(a)
	gwA, _, err := SharedGateway()
	if err != nil {
		t.Fatal(err)
	}
	SetSharedLLMClient(b)
	gwB, _, err := SharedGateway()
	if err != nil {
		t.Fatal(err)
	}
	if gwA == gwB {
		t.Error("换 client 后网关视图未重建")
	}
	local, ok := gwB.(*llmgw.Local)
	if !ok {
		t.Fatalf("期望 *llmgw.Local, got %T", gwB)
	}
	if local.Client != b {
		t.Error("网关视图包着的不是最新 client")
	}
}
