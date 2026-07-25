package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 转发必须带上载荷。原实现复用 r.Body, 而 handleAction 更早处已把它 io.ReadAll 读空
// —— 转发出去的请求体是空的。team.stop 那组载荷通常为空所以没暴露; 换成 team.create
// 就是"工作流和目标全丢"。
func TestForwardActionToControl_载荷不丢(t *testing.T) {
	var gotBody string
	var gotAuth string
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(actionResp{OK: true, Message: "团队已创建"})
	}))
	defer ctl.Close()

	t.Setenv("CLAUDE_GO_BOT_API_URL", ctl.URL)
	t.Setenv("CLAUDE_GO_BOT_API_TOKEN", "tok")
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/actions/team/create/x", nil)
	payload := map[string]interface{}{"workflow": "research", "objective": "目标"}

	im, _, ok := s.forwardActionToControl(req, "team", "create", "x", payload)
	if !ok {
		t.Fatal("配了基址却没转发")
	}
	if !strings.Contains(gotBody, "research") || !strings.Contains(gotBody, "目标") {
		t.Errorf("转发请求体丢了载荷: %q", gotBody)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("未带 token: %q —— 控制面启用 apiSecret 后会 401", gotAuth)
	}
	if im != "团队已创建" {
		t.Errorf("未回传控制面的结果: %q", im)
	}
}

// 本进程自己能执行时不转发 —— 否则同一个动作在两个进程里各跑一次。
func TestForwardActionToControl_有本地能力不转发(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOT_API_URL", "http://never-called")
	s := &Server{cfg: Config{TeamAction: func(string, string, map[string]interface{}) error { return nil }}}
	if _, _, ok := s.forwardActionToControl(
		httptest.NewRequest(http.MethodPost, "/api/actions/team/run/x", nil), "team", "run", "x", nil); ok {
		t.Error("本进程有 TeamAction 时不该转发(那是回环, 且会双跑)")
	}
	// 未配基址也不转发
	t.Setenv("CLAUDE_GO_BOT_API_URL", "")
	if _, _, ok := (&Server{}).forwardActionToControl(
		httptest.NewRequest(http.MethodPost, "/api/actions/team/run/x", nil), "team", "run", "x", nil); ok {
		t.Error("未配基址不该转发")
	}
}

// 控制面拒了要如实说, 不许谎报成功 —— 否则前端以为已生效。
func TestForwardActionToControl_不谎报(t *testing.T) {
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ctl.Close()
	t.Setenv("CLAUDE_GO_BOT_API_URL", ctl.URL)
	im, hint, ok := (&Server{}).forwardActionToControl(
		httptest.NewRequest(http.MethodPost, "/api/actions/team/run/x", nil), "team", "run", "x", nil)
	if !ok {
		t.Fatal("应视为已尝试转发")
	}
	if im != "" {
		t.Errorf("被拒却给了成功消息: %q", im)
	}
	if !strings.Contains(hint, "403") {
		t.Errorf("未如实回报状态码: %q", hint)
	}
}
