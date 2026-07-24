package llmgw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readFirstRecord 轮询读取 access.jsonl 首条记录。流式路径下, 客户端 ReadAll
// 返回与服务端 writeLog 之间在重负载下存在时序窗口, 轮询消除测试脆弱性。
func readFirstRecord(t *testing.T, path string) AccessRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
			if line != "" {
				var rec AccessRecord
				if json.Unmarshal([]byte(line), &rec) == nil && rec.TS != 0 {
					return rec
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("access.jsonl 未在超时内写入有效记录: %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 假上游: 记录收到的鉴权头与模型名, 按路径返回非流式/流式响应。
func fakeUpstream(t *testing.T, sawAuth *string, sawModel *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sawAuth = r.Header.Get("x-api-key")
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		*sawModel = probe.Model
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flushWrite := func(s string) { _, _ = io.WriteString(w, s) }
			flushWrite("event: message_start\n")
			flushWrite(`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}` + "\n\n")
			flushWrite("event: content_block_delta\n")
			flushWrite(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n")
			flushWrite("event: message_delta\n")
			flushWrite(`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n")
			flushWrite("event: message_stop\n")
			flushWrite(`data: {"type":"message_stop"}` + "\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":5,"output_tokens":3}}`)
	}))
}

func newTestServer(t *testing.T, upstreamURL, stateDir string) *Server {
	t.Helper()
	srv, err := NewServer([]Route{
		{Provider: "kimi", BaseURL: upstreamURL, APIKey: "sk-kimi", Models: []string{"k3"}},
		{Provider: "other", BaseURL: "http://127.0.0.1:1", APIKey: "sk-other", Models: []string{"m2"}},
	}, "kimi", stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestGatewayNonStreamRoutingAuthAndAccounting(t *testing.T) {
	var sawAuth, sawModel string
	up := fakeUpstream(t, &sawAuth, &sawModel)
	defer up.Close()
	dir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, dir).Handler())
	defer gw.Close()

	// 带 "provider:" 前缀的别名: 应路由到 kimi 且出站剥前缀
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"kimi:k3","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "pong") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if sawAuth != "sk-kimi" {
		t.Fatalf("鉴权头应为 kimi 的 key, got %q", sawAuth)
	}
	if sawModel != "k3" {
		t.Fatalf("出站模型名应剥掉 provider 前缀, got %q", sawModel)
	}

	// access.jsonl 记账: 双边 token 均非零
	rec := readFirstRecord(t, filepath.Join(dir, "access.jsonl"))
	if rec.InputTokens != 5 || rec.OutputTokens != 3 || rec.InputEstimated {
		t.Fatalf("token 记账错误: %+v", rec)
	}
	if rec.Provider != "kimi" || rec.Status != 200 {
		t.Fatalf("记录字段错误: %+v", rec)
	}
}

func TestGatewayStreamPassthroughAndUsageTee(t *testing.T) {
	var sawAuth, sawModel string
	up := fakeUpstream(t, &sawAuth, &sawModel)
	defer up.Close()
	dir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, dir).Handler())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/messages", "application/json",
		strings.NewReader(`{"model":"k3","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// SSE 原文透传
	if !strings.Contains(string(body), "message_start") || !strings.Contains(string(body), `"text":"hi"`) {
		t.Fatalf("SSE 未透传: %s", body)
	}
	// tee 记账: input=11 (message_start) output=7 (message_delta)
	rec := readFirstRecord(t, filepath.Join(dir, "access.jsonl"))
	if rec.InputTokens != 11 || rec.OutputTokens != 7 || !rec.Stream {
		t.Fatalf("流式 tee 记账错误: %+v", rec)
	}
}

func TestGatewayInputEstimateFallback(t *testing.T) {
	// 上游不回 usage → input 按请求字节估算并打标
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"m1","content":[]}`)
	}))
	defer up.Close()
	dir := t.TempDir()
	gw := httptest.NewServer(newTestServer(t, up.URL, dir).Handler())
	defer gw.Close()

	payload := `{"model":"k3","messages":[{"role":"user","content":"` + strings.Repeat("x", 400) + `"}]}`
	if _, err := http.Post(gw.URL+"/messages", "application/json", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	rec := readFirstRecord(t, filepath.Join(dir, "access.jsonl"))
	if !rec.InputEstimated || rec.InputTokens < 100 {
		t.Fatalf("input 估算兜底未生效: %+v", rec)
	}
}

func TestGatewayUpstreamDownReturns502(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer([]Route{{Provider: "dead", BaseURL: "http://127.0.0.1:1", APIKey: "k"}}, "dead", dir)
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()
	resp, err := http.Post(gw.URL+"/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游不可达应 502, got %d", resp.StatusCode)
	}
}

func TestGatewayHealthz(t *testing.T) {
	srv, _ := NewServer([]Route{{Provider: "p", BaseURL: "http://x", APIKey: "k"}}, "", "")
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()
	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz 失败: %v %v", err, resp)
	}
	resp.Body.Close()
}
