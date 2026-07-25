package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// artifactFixture 造一个 dashboard 侧的团队现场: stateDir/teams/<name>/ (落点 A) +
// 独立的 cwd (落点 B) + team.json (dashboard 只能从它拿 cwd / 时间窗)。
func artifactFixture(t *testing.T, name string) (s *Server, stateDir, dataDir, cwd string, started time.Time) {
	t.Helper()
	stateDir = t.TempDir()
	cwd = t.TempDir()
	dataDir = filepath.Join(stateDir, "teams", name)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	started = time.Now().Add(-time.Hour)
	teamJSON := map[string]interface{}{
		"name": name, "workflow": "creative-v2", "status": "completed",
		"objective": "产出媒体", "cwd": cwd,
		"createdAt": started, "startedAt": started,
	}
	data, _ := json.Marshal(teamJSON)
	if err := os.WriteFile(filepath.Join(dataDir, "team.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	s = &Server{cfg: Config{StateDir: stateDir}, provider: NewProvider(stateDir, 0)}
	return s, stateDir, dataDir, cwd, started
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestHandleTeamMediaRecursesSubdirs 真实 bug 回归: 原实现用非递归 os.ReadDir 且直接
// skip 子目录, media/sub/deep.png 永远采不到 (前端显示"暂无媒体产出")。
func TestHandleTeamMediaRecursesSubdirs(t *testing.T) {
	s, _, dataDir, _, _ := artifactFixture(t, "demo")
	pngMagic := "\x89PNG\r\n\x1a\n"
	writeAt(t, filepath.Join(dataDir, "media", "poster.png"), pngMagic+"top")
	writeAt(t, filepath.Join(dataDir, "media", "sub", "deep.png"), pngMagic+"deep")
	writeAt(t, filepath.Join(dataDir, "media", "sub", "deeper", "x.mp4"), "mp4data")

	rec := httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/api/teams/demo/media", nil), "demo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列清单 code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Files []struct {
			Name, Ext, URL string
			Size           int64
		} `json:"files"`
		Scanned bool `json:"scanned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if !resp.Scanned {
		t.Errorf("扫描成功时应标记 scanned=true (供下游区分'真空'与'未扫')")
	}
	byName := map[string]string{} // name → url
	for _, f := range resp.Files {
		byName[f.Name] = f.URL
	}
	for _, want := range []string{"poster.png", "sub/deep.png", "sub/deeper/x.mp4"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("清单缺少嵌套产物 %q: %+v", want, resp.Files)
		}
	}
	if got := byName["sub/deep.png"]; got != "/api/teams/demo/media/sub/deep.png" {
		t.Fatalf("嵌套产物 URL 形态不对 (前端按 url 取文件): %q", got)
	}
	// 现有 URL 形态不变: 顶层文件仍是 /api/teams/{n}/media/{file}
	if got := byName["poster.png"]; got != "/api/teams/demo/media/poster.png" {
		t.Fatalf("顶层 URL 形态被破坏: %q", got)
	}

	// 该 URL 必须真的能取到内容 (走既有单文件分支, 守卫不变)
	rec = httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/api/teams/demo/media/sub/deep.png", nil),
		"demo", []string{"sub", "deep.png"})
	if rec.Code != http.StatusOK || !strings.HasSuffix(rec.Body.String(), "deep") {
		t.Fatalf("嵌套产物取不到: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 穿越守卫仍然生效 (改递归不能顺手放开安全)
	writeAt(t, filepath.Join(filepath.Dir(dataDir), "secret.txt"), "TOPSECRET")
	rec = httptest.NewRecorder()
	s.handleTeamMedia(rec, httptest.NewRequest("GET", "/x", nil), "demo",
		[]string{"..", "..", "secret.txt"})
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "TOPSECRET") {
		t.Fatalf("穿越应 403 且不泄露: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleTeamArtifactsEndpoint 新端点必须同时列出两个落点的产物 (/media 只看 dataDir)。
func TestHandleTeamArtifactsEndpoint(t *testing.T) {
	s, _, dataDir, cwd, _ := artifactFixture(t, "demo")
	writeAt(t, filepath.Join(dataDir, "media", "cover.png"), "PNG")
	writeAt(t, filepath.Join(cwd, "out", "song.wav"), "RIFF")

	rec := httptest.NewRecorder()
	s.handleTeamDetail(rec, httptest.NewRequest("GET", "/api/teams/demo/artifacts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var live struct {
		Source  string `json:"source"`
		Scanned bool   `json:"scanned"`
		Empty   bool   `json:"empty"`
		Roots   []struct {
			Kind    string `json:"kind"`
			Path    string `json:"path"`
			Scanned bool   `json:"scanned"`
			Files   int    `json:"files"`
		} `json:"roots"`
		Files []struct {
			Root, Rel, Source string
			Size              int64
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if live.Source != "live" {
		t.Errorf("无落盘清单时应现扫, source=%q", live.Source)
	}
	if !live.Scanned || live.Empty {
		t.Errorf("应为扫过且非空: scanned=%v empty=%v", live.Scanned, live.Empty)
	}
	if len(live.Roots) != 2 {
		t.Fatalf("应有 data+work 两个根: %+v", live.Roots)
	}
	found := map[string]bool{}
	for _, f := range live.Files {
		found[f.Root+":"+f.Rel] = true
	}
	if !found["data:media/cover.png"] {
		t.Errorf("dataDir 侧产物缺失: %+v", live.Files)
	}
	if !found["work:out/song.wav"] {
		t.Errorf("cwd 侧产物缺失 (只扫一处就是媒锻误判的根因): %+v", live.Files)
	}

	// 落盘清单优先 (它含物化记账的硬归属证据, 事后重扫拿不到); ?refresh=1 强制现扫
	manifest := map[string]interface{}{
		"team": "demo", "scanned": true, "empty": false,
		"roots": []map[string]interface{}{{"kind": "work", "path": cwd, "scanned": true, "files": 1}},
		"files": []map[string]interface{}{
			{"root": "work", "rel": "archived/only-in-manifest.txt", "source": "materialize"},
		},
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dataDir, "ARTIFACTS.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleTeamDetail(rec, httptest.NewRequest("GET", "/api/teams/demo/artifacts", nil))
	if !strings.Contains(rec.Body.String(), "only-in-manifest.txt") ||
		!strings.Contains(rec.Body.String(), `"source":"manifest"`) {
		t.Fatalf("有落盘清单时应优先返回它: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleTeamDetail(rec, httptest.NewRequest("GET", "/api/teams/demo/artifacts?refresh=1", nil))
	if strings.Contains(rec.Body.String(), "only-in-manifest.txt") ||
		!strings.Contains(rec.Body.String(), `"source":"live"`) {
		t.Fatalf("refresh=1 应现扫: %s", rec.Body.String())
	}

	// 未知子路径不静默 200
	rec = httptest.NewRecorder()
	s.handleTeamDetail(rec, httptest.NewRequest("GET", "/api/teams/demo/artifacts/bogus", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知子路径应 404, got %d", rec.Code)
	}
}

// TestHandleTeamArtifactsFileGuards 取文件子路由的三重守卫: root 白名单 / 穿越拦截 /
// cwd 侧归属校验。
func TestHandleTeamArtifactsFileGuards(t *testing.T) {
	s, stateDir, dataDir, cwd, started := artifactFixture(t, "demo")
	writeAt(t, filepath.Join(dataDir, "media", "cover.png"), "\x89PNG\r\n\x1a\nok")
	writeAt(t, filepath.Join(dataDir, "page.html"), "<h1>模型生成</h1>")
	writeAt(t, filepath.Join(cwd, "out", "song.wav"), "RIFFdata")
	// cwd 里团队开始前就有的私有文件: 在 work 根内, 但不属于本团队产物
	writeAt(t, filepath.Join(cwd, ".env"), "SECRET_KEY=leak-me")
	old := started.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(cwd, ".env"), old, old); err != nil {
		t.Fatal(err)
	}
	// stateDir 之外的敏感文件, 用于验证穿越
	writeAt(t, filepath.Join(stateDir, "secret.txt"), "TOPSECRET")

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleTeamDetail(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	// 1) data 根正常取文件
	rec := get("/api/teams/demo/artifacts/file/media/cover.png?root=data")
	if rec.Code != http.StatusOK || !strings.HasSuffix(rec.Body.String(), "ok") {
		t.Fatalf("data 根取文件失败: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 2) work 根取归属内的文件
	rec = get("/api/teams/demo/artifacts/file/out/song.wav?root=work")
	if rec.Code != http.StatusOK || rec.Body.String() != "RIFFdata" {
		t.Fatalf("work 根取产物失败: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 3) root 非白名单 → 403 (不接受调用方自带路径)
	for _, bad := range []string{"tool", "etc", "../../"} {
		rec = get("/api/teams/demo/artifacts/file/out/song.wav?root=" + bad)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("root=%q 应 403 (白名单外), got %d body=%s", bad, rec.Code, rec.Body.String())
		}
	}

	// 4) ../ 穿越 → 403 且不泄露内容
	rec = get("/api/teams/demo/artifacts/file/../../secret.txt?root=data")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("穿越应 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "TOPSECRET") {
		t.Fatal("path traversal 泄露了根目录外的内容!")
	}

	// 5) cwd 里不属于本团队时间窗的文件 → 403 (cwd 是用户真实仓库, 有私钥/配置)
	rec = get("/api/teams/demo/artifacts/file/.env?root=work")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非本团队产物应 403, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "leak-me") {
		t.Fatal("泄露了团队开始前就存在的 cwd 私有文件!")
	}

	// 6) 模型生成的 HTML 必须带 CSP sandbox (与 media 端点同一硬化)
	rec = get("/api/teams/demo/artifacts/file/page.html?root=data")
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("HTML 产出缺少 CSP sandbox 头: %q", csp)
	}

	// 7) 不存在的文件 → 404 (不是 200 空体)
	rec = get("/api/teams/demo/artifacts/file/nope.txt?root=data")
	if rec.Code != http.StatusNotFound {
		t.Errorf("缺失文件应 404, got %d", rec.Code)
	}
}
