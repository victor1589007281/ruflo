package api

// R-H17 回退观测的验收。
//
// 假上游按请求体里的 model 决定行为：主模型一律 529（overloaded），备用模型一律
// 200。这样"是否发生了回退"由服务端实际收到的模型名证明，而不是只看回调 ——
// 只断言回调被调一次的话，一个"回调了但根本没切换模型"的实现依然全绿。
//
// 两路都要覆盖：引擎生产走流式（StreamMessage），但非流式路径也真实存在于
// 诊断/内部调用里，两边各有一套独立的回退循环，改一边漏一边是这类代码的典型缺陷。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

const (
	fbPrimary = "primary-model"
	fbBackup  = "backup-model"
)

// fallbackUpstream 是"先 529 再 200"的假上游。
type fallbackUpstream struct {
	stream bool

	mu     sync.Mutex
	models []string // 按到达顺序记录请求体里的 model, 供"确实切过"的断言使用
}

func (u *fallbackUpstream) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	u.mu.Lock()
	u.models = append(u.models, body.Model)
	u.mu.Unlock()

	if body.Model != fbBackup {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
		return
	}

	if !u.stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"`+fbBackup+`",`+
			`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":11,"output_tokens":7}}`)
		return
	}

	// 最小合法 SSE。message_delta(stop_reason) 是流完整性判据的一部分，缺了它
	// 这条流会被判成断流，测试就会因为与回退无关的原因变红。
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":%q,\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n", fbBackup)
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n")
}

func (u *fallbackUpstream) seenModels() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

// newFallbackTestClient 用极小的重试退避，避免 529 的重试把测试拖到分钟级。
//
// RetryCount=1 是有意的：maxRetry<=0 会被内部改回 4，所以"0 次重试"实际不可达；
// 1 表示主模型先试 2 次（首次 + 1 次重试）再进入 FallbackModels，正好走完
// "重试耗尽 → 回退"这条真实分支。
func newFallbackTestClient(srv *httptest.Server) *Client {
	return &Client{
		BaseURL:        srv.URL,
		APIKey:         "k",
		Model:          fbPrimary,
		FallbackModels: []string{fbBackup},
		RetryCount:     1,
		RetryBase:      time.Millisecond,
		RetryMax:       time.Millisecond,
		Client:         &http.Client{Timeout: 5 * time.Second},
	}
}

// runFallbackRequest 驱动一路请求并返回最终错误。流式必须排空 eventCh。
func runFallbackRequest(t *testing.T, c *Client, stream bool) error {
	t.Helper()
	msgs := []types.APIMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}}
	if !stream {
		_, err := c.SendMessage(context.Background(), msgs, nil, nil, 64)
		return err
	}
	evCh, errCh := c.StreamMessage(context.Background(), msgs, nil, nil, 64)
	for range evCh {
	}
	return <-errCh
}

func TestFallbackObserver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{"NonStream", false},
		{"Stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fallbackUpstream{stream: tc.stream}
			srv := httptest.NewServer(http.HandlerFunc(up.handle))
			defer srv.Close()

			var mu sync.Mutex
			var got []FallbackInfo
			c := newFallbackTestClient(srv).WithFallbackObserver(func(info FallbackInfo) {
				mu.Lock()
				got = append(got, info)
				mu.Unlock()
			})

			if err := runFallbackRequest(t, c, tc.stream); err != nil {
				t.Fatalf("回退后请求应成功, 实际失败: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("回退观测应恰好触发 1 次, 实际 %d 次: %+v", len(got), got)
			}
			info := got[0]
			if info.FromModel != fbPrimary {
				t.Errorf("FromModel = %q, 期望 %q", info.FromModel, fbPrimary)
			}
			if info.ToModel != fbBackup {
				t.Errorf("ToModel = %q, 期望 %q", info.ToModel, fbBackup)
			}
			if info.StatusCode != 529 {
				t.Errorf("StatusCode = %d, 期望 529", info.StatusCode)
			}
			if info.Reason == "" {
				t.Error("Reason 不应为空: 回退原因必须进 trace/账本")
			}

			// 服务端侧证据: 最后一个到达的请求确实用了备用模型。没有这条,
			// "回调了但没切换"仍可能通过上面的断言。
			models := up.seenModels()
			if len(models) == 0 || models[len(models)-1] != fbBackup {
				t.Fatalf("假上游收到的模型序列 = %v, 期望最后一次是 %q", models, fbBackup)
			}
		})
	}
}

// TestFallbackObserverNilUnchanged 锁住"nil 回调 = 完全旧行为"：返回原实例,
// 请求照常成功（含回退路径本身）。
func TestFallbackObserverNilUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{"NonStream", false},
		{"Stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fallbackUpstream{stream: tc.stream}
			srv := httptest.NewServer(http.HandlerFunc(up.handle))
			defer srv.Close()

			base := newFallbackTestClient(srv)
			c := base.WithFallbackObserver(nil)
			if c != base {
				t.Fatal("nil 观察者应原样返回原 client, 而不是制造一个副本")
			}
			if err := runFallbackRequest(t, c, tc.stream); err != nil {
				t.Fatalf("nil 观察者下请求应成功, 实际失败: %v", err)
			}
		})
	}
}

// TestFallbackObserverPanicDoesNotBreakRequest 观测面不能影响主路径: 回调 panic
// 时请求仍须成功, 且回退本身仍然发生。
func TestFallbackObserverPanicDoesNotBreakRequest(t *testing.T) {
	up := &fallbackUpstream{stream: false}
	srv := httptest.NewServer(http.HandlerFunc(up.handle))
	defer srv.Close()

	c := newFallbackTestClient(srv).WithFallbackObserver(func(FallbackInfo) {
		panic("observer boom")
	})
	if err := runFallbackRequest(t, c, false); err != nil {
		t.Fatalf("观察者 panic 不应拖垮请求, 实际失败: %v", err)
	}
}
