package wiki

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// startTestAPI 起一个真实监听的 APIServer（端口 0 = 由内核分配），
// 并把 cluster 路由挂到同一个 mux 上——这正是生产形态：:18080 上
// /wiki/*、/sync/*、/api/*、/cluster/* 全共享 APIServer.Mux()。
func startTestAPI(t *testing.T, secret string) string {
	t.Helper()
	api := NewAPIServer(NewEngine(t.TempDir(), "", "", ""), secret)
	api.SetBindHost("127.0.0.1")

	// cluster 侧不自带鉴权, 依赖共享 mux 上的统一中间件——正是历史上漏保护的那组
	ss := statestore.NewMemStore()
	cluster.Mount(api.Mux(), cluster.NewQueue(ss, time.Minute), cluster.NewRegistry(ss, time.Minute))

	// 端口 0 拿一个空闲端口, 再交给 Start
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := api.Start(port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(api.Stop)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// 等监听就绪
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond); err == nil {
			_ = c.Close()
			return base
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("服务未在 3s 内就绪")
	return base
}

func getStatus(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// 端到端验证"一次中间件覆盖共享 mux 上的全部路由组"这一核心主张。
// 历史上鉴权是逐路由 s.auth(...) 包装的, 结果 /api/* 与后加的 /cluster/*
// 都漏了; 单测只能证明中间件本身对, 证明不了 Start() 真的把它装上了。
func TestStart_统一鉴权覆盖共享mux(t *testing.T) {
	const secret = "integration-token"
	base := startTestAPI(t, secret)

	// /cluster/* 自身不带任何鉴权代码, 若它被拦住, 就说明拦点在 Handler 层
	// 而非逐路由——这是本测试最关键的一条。
	for _, path := range []string{"/wiki/status", "/cluster/tasks"} {
		if code := getStatus(t, base+path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s 无 token = %d, 期望 401", path, code)
		}
		if code := getStatus(t, base+path, "wrong"); code != http.StatusUnauthorized {
			t.Errorf("%s 错 token = %d, 期望 401", path, code)
		}
		if code := getStatus(t, base+path, secret); code == http.StatusUnauthorized {
			t.Errorf("%s 正确 token 仍被拒 (401)", path)
		}
	}
}

// fail-open：未配置 secret 时一切照旧放行。这是 8+ 下游平台的兼容底线,
// 也是本次改动可以零风险上线的依据。
func TestStart_未配置secret时放行(t *testing.T) {
	base := startTestAPI(t, "")
	for _, path := range []string{"/wiki/status", "/cluster/tasks"} {
		if code := getStatus(t, base+path, ""); code == http.StatusUnauthorized {
			t.Errorf("%s 在未配置 secret 时被拒 (401), 破坏了向后兼容", path)
		}
	}
}

func TestSetBindHost(t *testing.T) {
	// 只验证 host 真的进了监听地址：绑 127.0.0.1 后不应能从非回环地址连上。
	// 这里用 Addr 字段断言即可, 不做跨网卡连接测试（CI 环境网卡不确定）。
	api := NewAPIServer(NewEngine(t.TempDir(), "", "", ""), "")
	api.SetBindHost("127.0.0.1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if err := api.Start(port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer api.Stop()
	want := fmt.Sprintf("127.0.0.1:%d", port)
	if api.server.Addr != want {
		t.Errorf("server.Addr = %q, 期望 %q", api.server.Addr, want)
	}
	if api.server.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout 未设置（Slowloris 防护缺失）")
	}
}

// 确认 handler 真的被中间件包过一层, 而不是裸 mux。
func TestStart_handler非裸mux(t *testing.T) {
	api := NewAPIServer(NewEngine(t.TempDir(), "", "", ""), "tok")
	api.SetBindHost("127.0.0.1")
	rr := httptest.NewRecorder()
	// 直接打裸 mux：不经中间件, 故不会 401（此时走 s.auth 的逐路由旧闸）
	api.Mux().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/cluster/tasks", nil))
	bareCode := rr.Code

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if err := api.Start(port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer api.Stop()

	rr2 := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/cluster/tasks", nil))
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("经 server.Handler = %d, 期望 401（说明中间件未装上）", rr2.Code)
	}
	if bareCode == http.StatusUnauthorized {
		t.Log("注意: 裸 mux 也返回 401, 本测试对比性减弱")
	}
}
