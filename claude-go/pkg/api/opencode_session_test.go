// opencode_session_test.go — 直连 opencode 时必须带 x-opencode-session。
//
// # 为什么这条测试必须走 SendMessage 而不是只调 applyExtraHeaders
//
// 这个仓库最反复出现的缺陷形态是"函数实现好了、单测全绿、生产路径从不调用"
// （枢纽/闸门/账本都踩过）。会话头的注入函数之前确实存在于两个网关里，但
// **直连**路径（claude-go run --model opencode:...）的三个请求构造点一个都没调 ——
// 于是 CLI 直连 opencode 每次都 400 MissingSessionID，而网关在中间时完全看不出来。
//
// 所以这里从公开入口 SendMessage 发一次请求，断言**假上游真正收到的头**。
// 只断言私有 helper 的行为，无法区分"helper 正确"与"helper 被接线"。
package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// headerRT 直接充当 Transport：不联网，记录请求头并回一个最小合法响应。
type headerRT struct {
	mu      sync.Mutex
	headers []http.Header
}

func (r *headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.headers = append(r.headers, req.Header.Clone())
	r.mu.Unlock()
	body := `{"id":"m","type":"message","role":"assistant","model":"deepseek-v4.1-flash",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func sendOnce(t *testing.T, c *Client) {
	t.Helper()
	msgs := []types.APIMessage{{Role: "user", Content: []byte(`"hi"`)}}
	if _, err := c.SendMessage(context.Background(), msgs, nil, nil, 16); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

func TestSendMessage_OpencodeDirectGetsSessionHeader(t *testing.T) {
	rt := &headerRT{}
	c := &Client{
		BaseURL:    "https://opencode.ai/zen/go/v1",
		APIKey:     "k",
		Model:      "deepseek-v4.1-flash",
		Client:     &http.Client{Transport: rt},
		RetryCount: 1,
	}

	sendOnce(t, c)
	sendOnce(t, c)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.headers) != 2 {
		t.Fatalf("请求数=%d，want 2", len(rt.headers))
	}
	first := rt.headers[0].Get(OpencodeSessionHeader)
	second := rt.headers[1].Get(OpencodeSessionHeader)
	if first == "" {
		t.Fatal("直连 opencode 的请求没有 x-opencode-session —— 上游会 400 MissingSessionID")
	}
	// 会话 ID 同时是提示缓存的分片键：同客户端两次请求必须同会话，否则缓存命中率恒 0。
	if first != second {
		t.Fatalf("同一客户端的会话 ID 不稳定: %q vs %q", first, second)
	}
}

func TestSendMessage_NonOpencodeGetsNoSessionHeader(t *testing.T) {
	rt := &headerRT{}
	c := &Client{
		BaseURL:    "https://api.example.com/v1",
		APIKey:     "k",
		Model:      "m",
		Client:     &http.Client{Transport: rt},
		RetryCount: 1,
	}
	sendOnce(t, c)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if got := rt.headers[0].Get(OpencodeSessionHeader); got != "" {
		t.Fatalf("非 opencode 端点被塞了会话头 %q —— 未启用的能力不得改变出站字节", got)
	}
}

// TestSendMessage_ExplicitSessionHeaderWins 钉住优先级：调用方（网关）显式给的
// 会话 ID 优先于默认值。网关按入站会话透传，默认值会把这些会话合并成一个。
func TestSendMessage_ExplicitSessionHeaderWins(t *testing.T) {
	rt := &headerRT{}
	c := &Client{
		BaseURL:      "https://opencode.ai/zen/go/v1",
		APIKey:       "k",
		Model:        "deepseek-v4.1-flash",
		Client:       &http.Client{Transport: rt},
		RetryCount:   1,
		ExtraHeaders: http.Header{OpencodeSessionHeader: {"from-gateway"}},
	}
	sendOnce(t, c)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if got := rt.headers[0].Get(OpencodeSessionHeader); got != "from-gateway" {
		t.Fatalf("会话头=%q，want from-gateway", got)
	}
}
