package llmgw

// trace_e2e_test.go — 验证 design/02 §3.1 的完整一跳:
//
//	上层 → LLMGateway 接口 → api.Client → (CLAUDE_GO_LLM_GATEWAY) → llmgw.Server → provider
//	                                                                    ↓
//	                                                            access.jsonl 带 trace 四元组
//
// 之前的缺口有两处, 这里各挡一处:
//   - AccessRecord.RunID 字段早就有, 但全仓没有任何客户端发过 X-CG-Run-ID ⇒ 恒空;
//   - Local 只是转调 api.Client, ChatRequest 里没有 Trace 可带。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestTrace四元组_经网关接口一路落到accessjsonl 是这一跳的端到端证据。
func TestTrace四元组_经网关接口一路落到accessjsonl(t *testing.T) {
	var sawAuth, sawModel string
	upstream := fakeUpstream(t, &sawAuth, &sawModel)
	defer upstream.Close()

	stateDir := t.TempDir()
	gwSrv := newTestServer(t, upstream.URL, stateDir)
	gwHTTP := httptest.NewServer(gwSrv.Handler())
	defer gwHTTP.Close()

	// 上层只持有 LLMGateway 接口; 客户端的 BaseURL 指向网关 (等价于生产里
	// modelconfig.ApplyGatewayOverride 干的事)。
	var gw LLMGateway = NewLocal(api.NewClient(gwHTTP.URL, "sk-client", "k3"))

	resp, err := gw.Complete(context.Background(), ChatRequest{
		Messages:  []types.APIMessage{{Role: "user", Content: json.RawMessage(`"ping"`)}},
		System:    []string{"be brief"},
		MaxTokens: 32,
		Trace:     Trace{RunID: "run-42", NodeID: "node-a", TurnID: "turn-3", CallID: "call-9"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp == nil {
		t.Fatal("响应为 nil")
	}

	rec := readFirstRecord(t, filepath.Join(stateDir, "access.jsonl"))
	if rec.RunID != "run-42" || rec.NodeID != "node-a" || rec.TurnID != "turn-3" || rec.CallID != "call-9" {
		t.Errorf("网关未收到完整 trace 四元组: run=%q node=%q turn=%q call=%q",
			rec.RunID, rec.NodeID, rec.TurnID, rec.CallID)
	}
	if rec.Provider != "kimi" {
		t.Errorf("provider = %q, 期望 kimi (路由未生效)", rec.Provider)
	}
}

// 没设 trace 时不应该多发头 —— 直连 provider 的默认形态必须一字节不变。
func TestTrace未设置时不发头(t *testing.T) {
	var sawHeaders []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			sawHeaders = append(sawHeaders, strings.ToLower(k))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	gw := NewLocal(api.NewClient(upstream.URL, "sk", "m"))
	if _, err := gw.Complete(context.Background(), ChatRequest{
		Messages:  []types.APIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		MaxTokens: 8,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, h := range sawHeaders {
		if strings.HasPrefix(h, "x-cg-") {
			t.Errorf("未设 trace 却发了 %s 头", h)
		}
	}
}

// Simple 没有请求结构体, trace 只能经 ctx。这条确认 WithTrace 对它同样生效。
func TestTrace经WithTrace对Simple生效(t *testing.T) {
	var sawRun string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRun = r.Header.Get(api.TraceHeaderRunID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	gw := NewLocal(api.NewClient(upstream.URL, "sk", "m"))
	ctx := WithTrace(context.Background(), Trace{RunID: "job-7"})
	if _, err := gw.Simple(ctx, "sys", "user"); err != nil {
		t.Fatalf("Simple: %v", err)
	}
	if sawRun != "job-7" {
		t.Errorf("X-CG-Run-ID = %q, 期望 job-7", sawRun)
	}
}

// SimpleClient 必须同时满足"文本 + 多模态"两件套, 否则 pkg/wiki / pkg/vision
// 这类端口注入不进去 (编译期就断)。这里用编译期断言 + 一次真实调用双保险。
func TestSimpleClient满足最小LLM端口(t *testing.T) {
	var port interface {
		SimpleComplete(ctx context.Context, system, user string) (string, error)
		RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	port = SimpleClient{GW: NewLocal(api.NewClient(upstream.URL, "sk", "m"))}
	out, err := port.SimpleComplete(context.Background(), "s", "u")
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("SimpleComplete = %q, err=%v", out, err)
	}
	if _, err := port.RawComplete(context.Background(),
		json.RawMessage(`[{"type":"text","text":"hi"}]`), 16); err != nil {
		t.Fatalf("RawComplete: %v", err)
	}
}
