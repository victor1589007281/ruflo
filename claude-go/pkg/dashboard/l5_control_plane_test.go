package dashboard

// l5_control_plane_test.go — design/02 §3.5「BotAPIURL 硬编码回环」的断言。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestL5_控制面地址默认沿用配置(t *testing.T) {
	s := &Server{cfg: Config{BotAPIURL: "http://127.0.0.1:18080/", BotAPIToken: "cfg-tok"}}
	if got := s.botAPIBase(); got != "http://127.0.0.1:18080" {
		t.Errorf("botAPIBase = %q, 期望去掉尾斜杠后的配置值", got)
	}
	if got := s.botAPIToken(); got != "cfg-tok" {
		t.Errorf("botAPIToken = %q", got)
	}
}

// 环境变量优先: T2/T3 形态下控制面不在本机, 必须能指到别处。
func TestL5_环境变量可覆盖控制面地址(t *testing.T) {
	t.Setenv(EnvBotAPIURL, "http://control-plane.svc:18080/")
	t.Setenv(EnvBotAPIToken, "env-tok")
	s := &Server{cfg: Config{BotAPIURL: "http://127.0.0.1:18080", BotAPIToken: "cfg-tok"}}
	if got := s.botAPIBase(); got != "http://control-plane.svc:18080" {
		t.Errorf("botAPIBase = %q, 期望环境变量优先", got)
	}
	if got := s.botAPIToken(); got != "env-tok" {
		t.Errorf("botAPIToken = %q, 期望环境变量优先", got)
	}
}

// 两者都空 = 没有控制面, 调用方据此走"无主进程消费"的诚实分支。
func TestL5_均未配置时为空(t *testing.T) {
	s := &Server{cfg: Config{}}
	if got := s.botAPIBase(); got != "" {
		t.Errorf("botAPIBase = %q, 期望空", got)
	}
}

// 端到端: 未注入 TeamAction 且配置里没有 BotAPIURL, 只靠环境变量也能把
// /api/actions/team/stop/<t> 转发出去。这条同时证明 botAPIBase 真接在转发路径上,
// 而不是只是个没人调的 helper。
func TestL5_动作转发使用环境变量控制面(t *testing.T) {
	var gotPath, gotAuth string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message":"控制面已执行","consumed":true,"status":"done"}`))
	}))
	defer control.Close()

	t.Setenv(EnvBotAPIURL, control.URL)
	t.Setenv(EnvBotAPIToken, "tok-42")
	SetActionSink(nil)
	t.Cleanup(func() { SetActionSink(nil) })

	stateDir := t.TempDir()
	s := &Server{cfg: Config{StateDir: stateDir}, provider: NewProvider(stateDir, 0)}
	rr := httptest.NewRecorder()
	s.handleAction(rr, httptest.NewRequest(http.MethodPost, "/api/actions/team/stop/demo", strings.NewReader(`{}`)))

	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rr.Code, rr.Body.String())
	}
	if gotPath != "/api/actions/team/stop/demo" {
		t.Errorf("控制面收到的路径 = %q", gotPath)
	}
	if gotAuth != "Bearer tok-42" {
		t.Errorf("控制面收到的鉴权头 = %q, 期望 Bearer tok-42", gotAuth)
	}
	var resp actionResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rr.Body.String())
	}
	if !strings.Contains(resp.Message, "控制面已执行") {
		t.Errorf("应回传控制面的结论, got %+v", resp)
	}
}
