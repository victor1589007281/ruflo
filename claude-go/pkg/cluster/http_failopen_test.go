package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 非 2xx 必须变成可见的 error。改造前 post 拿到状态码却只是返回它, 而
// Heartbeat/Complete/Fail 三个调用方都写成 `_, err :=` —— 401(没配 token) 与
// 400("任务不在你的租约内") 在 worker 侧全是静默成功。
func TestClient_非2xx必须报错(t *testing.T) {
	for _, code := range []int{400, 401, 403, 409, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, code, map[string]string{"error": "任务不在你的租约内"})
		}))
		c := NewClient(srv.URL, "w1")

		if err := c.Heartbeat(nil, []string{"stage"}); err == nil {
			t.Errorf("Heartbeat 遇到 %d 未报错 —— 静默成功", code)
		}
		if err := c.Complete("t1", json.RawMessage(`{}`)); err == nil {
			t.Errorf("Complete 遇到 %d 未报错 —— 上报终态被拒却当成功, 任务会停在 leased 直到租约过期", code)
		}
		if err := c.Fail("t1", "boom"); err == nil {
			t.Errorf("Fail 遇到 %d 未报错", code)
		}
		_, err := c.PullWithCaps([]string{"stage"}, nil)
		if err == nil {
			t.Errorf("Pull 遇到 %d 未报错", code)
		} else if !strings.Contains(err.Error(), "租约") {
			t.Errorf("错误应带服务端原文以便归因: %v", err)
		}
		srv.Close()
	}
}

// 204 是 /cluster/pull 的合法"无任务"应答, 不能被当成错误。
func TestClient_204无任务不算错(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 204, map[string]any{"task": nil})
	}))
	defer srv.Close()
	task, err := NewClient(srv.URL, "w1").PullWithCaps([]string{"stage"}, nil)
	if err != nil {
		t.Fatalf("204 被当成错误: %v", err)
	}
	if task != nil {
		t.Errorf("204 应返回 nil task, 实得 %+v", task)
	}
}

// 配了 token 就要发出去 —— httpauth 的保护前缀含 /cluster/, 不发就是每次 401。
func TestClient_Bearer头(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "w1").WithToken("s3cr3t").Heartbeat(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, 期望 Bearer s3cr3t", got)
	}
	// 未配 token 时不发头 (默认形态不变)
	if err := NewClient(srv.URL, "w1").Heartbeat(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("未配 token 却发了 %q", got)
	}
}
