package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandleWorkflowGenerate 验证 LLM 生成工作流编排端点 (Mode 2)。
func TestHandleWorkflowGenerate(t *testing.T) {
	s := &Server{cfg: Config{StateDir: t.TempDir(), LLMComplete: func(_ context.Context, _, _ string) (string, error) {
		return `{"name":"ai-gen-flow","mode":"pipeline","stages":[{"name":"a","prompt":"do {objective}"},{"name":"b","prompt":"then","dependsOn":["a"]}]}`, nil
	}}, provider: NewProvider(t.TempDir(), 0)}
	rec := httptest.NewRecorder()
	s.handleWorkflowGenerate(rec, httptest.NewRequest("POST", "/api/workflows/generate", strings.NewReader(`{"objective":"写一篇技术文章"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("生成应 200, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ai-gen-flow") || !strings.Contains(rec.Body.String(), `"workflow"`) {
		t.Errorf("应返回生成的工作流(含prompt供编辑): %s", rec.Body.String())
	}

	// 无 LLM → 503
	s2 := &Server{cfg: Config{StateDir: t.TempDir()}, provider: NewProvider(t.TempDir(), 0)}
	rec = httptest.NewRecorder()
	s2.handleWorkflowGenerate(rec, httptest.NewRequest("POST", "/api/workflows/generate", strings.NewReader(`{"objective":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无 LLM 应 503, got %d", rec.Code)
	}
}

// TestHandleTeamSwarmPlan 验证蜂群动态编排可视化端点 (Mode 3): 从黑板取 swarm-plan 转 stage 形状。
func TestHandleTeamSwarmPlan(t *testing.T) {
	dir := t.TempDir()
	teamDir := filepath.Join(dir, "teams", "swt")
	if err := os.MkdirAll(teamDir, 0755); err != nil {
		t.Fatal(err)
	}
	planJSON := `{"subTasks":[{"id":"t1","description":"调研","role":"researcher"},{"id":"t2","description":"撰写","role":"writer","dependsOn":["t1"]}],"strategy":"pipeline","rationale":"先调研后写"}`
	entries := []map[string]string{{"key": "swarm-plan", "value": planJSON}}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(teamDir, "blackboard.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{StateDir: dir}, provider: NewProvider(dir, 0)}
	rec := httptest.NewRecorder()
	s.handleTeamSwarmPlan(rec, httptest.NewRequest("GET", "/x", nil), "swt")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"available":true`) || !strings.Contains(body, "t1") || !strings.Contains(body, "t2") {
		t.Fatalf("蜂群编排未正确返回: %s", body)
	}
	if !strings.Contains(body, `"strategy":"pipeline"`) {
		t.Errorf("应含策略: %s", body)
	}

	// 无 swarm-plan 的团队 → available:false, 不报错
	rec = httptest.NewRecorder()
	s.handleTeamSwarmPlan(rec, httptest.NewRequest("GET", "/x", nil), "nonexistent")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Errorf("无 plan 应返回 available:false, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestDynamicWorkflowOverHTTP 通过真实 HTTP 往返(httptest.Server)触发动态工作流注册端点,
// 模拟 webapp → claude-go 的实际调用路径 (真实 TCP/路由/JSON 编解码)。
func TestDynamicWorkflowOverHTTP(t *testing.T) {
	s := &Server{cfg: Config{StateDir: t.TempDir()}, provider: NewProvider(t.TempDir(), 0)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workflows", s.handleWorkflows)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 创建动态工作流 (webapp 走的就是这个 POST)
	body := `{"name":"http-dyn-flow","mode":"pipeline","qualityGate":"content","stages":[
		{"name":"a","role":"x","prompt":"do {objective}"},
		{"name":"b","role":"y","prompt":"then {prev_result}","dependsOn":["a"]}]}`
	resp, err := http.Post(ts.URL+"/api/workflows", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建动态工作流 HTTP %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 列表能拿到它
	r2, err := http.Get(ts.URL + "/api/workflows")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	listedBytes, _ := io.ReadAll(r2.Body)
	listed := string(listedBytes)
	if !strings.Contains(listed, "http-dyn-flow") || !strings.Contains(listed, `"custom":true`) {
		t.Fatalf("列表未含动态工作流(注册未生效): %s", listed)
	}
}

// TestHandleWorkflowCreate 验证 POST /api/workflows 动态工作流注册端点 (webapp 创建自定义工作流走它)。
func TestHandleWorkflowCreate(t *testing.T) {
	s := &Server{cfg: Config{StateDir: t.TempDir()}, provider: NewProvider(t.TempDir(), 0)}

	// 合法 pipeline (内联 prompt) → 注册成功, 落盘
	body := `{"name":"webapp-dyn-1","mode":"pipeline","qualityGate":"content","stages":[
		{"name":"a","role":"x","prompt":"do {objective}"},
		{"name":"b","role":"y","prompt":"then {prev_result}","dependsOn":["a"]}]}`
	rec := httptest.NewRecorder()
	s.handleWorkflows(rec, httptest.NewRequest("POST", "/api/workflows", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("注册合法工作流应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"persisted":true`) {
		t.Errorf("应落盘 persisted:true, body=%s", rec.Body.String())
	}
	// 落盘文件存在
	if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "workflows", "webapp-dyn-1.json")); err != nil {
		t.Errorf("工作流应落盘: %v", err)
	}
	// GET 列表应包含它, 且内容门禁声明可见
	rec = httptest.NewRecorder()
	s.handleWorkflows(rec, httptest.NewRequest("GET", "/api/workflows", nil))
	if !strings.Contains(rec.Body.String(), "webapp-dyn-1") || !strings.Contains(rec.Body.String(), `"custom":true`) {
		t.Errorf("列表应含自定义工作流且标记 custom: %s", rec.Body.String())
	}

	// 非法模式 (creative_media 有伴生硬编码) → 400, 不注册
	rec = httptest.NewRecorder()
	s.handleWorkflows(rec, httptest.NewRequest("POST", "/api/workflows",
		strings.NewReader(`{"name":"webapp-dyn-bad","mode":"creative_media","stages":[{"name":"a","prompt":"x"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非纯数据模式应被拒 400, got %d", rec.Code)
	}
}

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
