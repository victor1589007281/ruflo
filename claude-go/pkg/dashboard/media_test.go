package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandleTeamRefineEndpoint 锁定直连端点 POST /api/teams/{name}/refine|fork 的路由与 payload 契约。
// (验证 handleTeamRefine 写入的键名与 bot.DashboardTeamAction 读取的键名一致: feedback/targetStage/newName)
func TestHandleTeamRefineEndpoint(t *testing.T) {
	type captured struct {
		action, name string
		payload      map[string]interface{}
	}
	var got *captured
	s := &Server{cfg: Config{
		StateDir: t.TempDir(),
		TeamAction: func(action, name string, payload map[string]interface{}) error {
			got = &captured{action: action, name: name, payload: payload}
			return nil
		},
	}, provider: NewProvider(t.TempDir(), 0)}

	// refine: 正常
	got = nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/teams/demo/refine",
		strings.NewReader(`{"feedback":"补充基准数据","targetStage":"article-writing"}`))
	s.handleTeamDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refine code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got == nil || got.action != "refine" || got.name != "demo" {
		t.Fatalf("TeamAction 未按预期调用: %+v", got)
	}
	if got.payload["feedback"] != "补充基准数据" || got.payload["targetStage"] != "article-writing" {
		t.Fatalf("payload 键名契约不符 (handleTeamRefine vs bot 读取): %+v", got.payload)
	}

	// refine: 空 feedback → 400, 不应触发执行
	got = nil
	rec = httptest.NewRecorder()
	s.handleTeamDetail(rec, httptest.NewRequest("POST", "/api/teams/demo/refine", strings.NewReader(`{"feedback":""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空 feedback 应 400, got %d", rec.Code)
	}
	if got != nil {
		t.Error("空 feedback 不应调用 TeamAction")
	}

	// fork: 正常
	got = nil
	rec = httptest.NewRecorder()
	s.handleTeamDetail(rec, httptest.NewRequest("POST", "/api/teams/demo/fork", strings.NewReader(`{"newName":"demo-v2"}`)))
	if rec.Code != http.StatusOK || got == nil || got.action != "fork" || got.payload["newName"] != "demo-v2" {
		t.Fatalf("fork 契约不符: code=%d %+v", rec.Code, got)
	}

	// 无 TeamAction → 503
	s2 := &Server{cfg: Config{StateDir: t.TempDir()}, provider: NewProvider(t.TempDir(), 0)}
	rec = httptest.NewRecorder()
	s2.handleTeamDetail(rec, httptest.NewRequest("POST", "/api/teams/demo/refine", strings.NewReader(`{"feedback":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无 TeamAction 应 503, got %d", rec.Code)
	}
}

func TestHandleTeamMedia(t *testing.T) {
	dir := t.TempDir()
	mediaDir := filepath.Join(dir, "teams", "demo", "media")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatal(err)
	}
	pngMagic := "\x89PNG\r\n\x1a\n"
	if err := os.WriteFile(filepath.Join(mediaDir, "poster.png"), []byte(pngMagic+"fakedata"), 0644); err != nil {
		t.Fatal(err)
	}
	// 放一个 media 目录之外的敏感文件, 用于验证穿越被拦
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("TOPSECRET"), 0644); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: Config{StateDir: dir}, provider: NewProvider(dir, 0)}

	// 1) 列清单
	rec := httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/api/teams/demo/media", nil), "demo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列清单 code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "poster.png") || !strings.Contains(rec.Body.String(), "/api/teams/demo/media/poster.png") {
		t.Fatalf("清单缺少文件或 URL: %s", rec.Body.String())
	}

	// 2) 服务真实文件
	rec = httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/api/teams/demo/media/poster.png", nil), "demo", []string{"poster.png"})
	if rec.Code != http.StatusOK {
		t.Fatalf("服务文件 code=%d", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), pngMagic) {
		t.Fatalf("服务文件内容不对: %q", rec.Body.String()[:8])
	}

	// 3) path-traversal 必须被拒, 且不泄露目录外内容
	rec = httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/x", nil), "demo", []string{"..", "..", "..", "secret.txt"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("穿越应被拒(403), got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "TOPSECRET") {
		t.Fatal("path traversal 泄露了 media 目录外的内容!")
	}

	// 4) 模型生成的 HTML 必须带 CSP sandbox 头
	if err := os.WriteFile(filepath.Join(mediaDir, "page.html"), []byte("<h1>hi</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/api/teams/demo/media/page.html", nil), "demo", []string{"page.html"})
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("HTML 产出缺少 CSP sandbox 头: %q", csp)
	}
}
