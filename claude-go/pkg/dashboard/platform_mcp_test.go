package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/httpauth"
	"github.com/anthropic/claude-go/pkg/platformmcp"
)

// mcpFixture 造一个有一个团队的 stateDir, 并把 dashboard 装配到一个真实 mux 上
// (与 :18080 的生产装配路径 MountOn 完全一致)。
func mcpFixture(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	stateDir := t.TempDir()
	teamDir := filepath.Join(stateDir, "teams", "demo")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	teamJSON := map[string]any{
		"name": "demo", "workflow": "novel-v2", "status": "completed",
		"objective": "写一章", "created_at": time.Now().Format(time.RFC3339),
	}
	b, _ := json.Marshal(teamJSON)
	if err := os.WriteFile(filepath.Join(teamDir, "team.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	MountOn(Config{StateDir: stateDir}, mux)
	return mux, stateDir
}

// rpc 打一次 /api/mcp/rpc, 返回 tools/call 的文本内容与 isError。
func rpc(t *testing.T, mux *http.ServeMux, body string) (string, bool, *json.RawMessage) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, PlatformMCPPath, strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, body=%s", PlatformMCPPath, rr.Code, rr.Body.String())
	}
	var resp struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rr.Body.String())
	}
	if resp.Error != nil {
		return "", true, resp.Error
	}
	if resp.Result == nil || len(resp.Result.Content) == 0 {
		return "", resp.Result != nil && resp.Result.IsError, nil
	}
	return resp.Result.Content[0].Text, resp.Result.IsError, nil
}

// 通电证据: 平台 MCP 端点挂在生产装配路径 (MountOn) 上, 打过去能读到真团队数据。
func TestPlatformMCP_经生产mux读到真团队(t *testing.T) {
	mux, _ := mcpFixture(t)

	text, isErr, rpcErr := rpc(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"platform_teams_list"}}`)
	if rpcErr != nil || isErr {
		t.Fatalf("teams_list 失败: isErr=%v err=%v text=%s", isErr, rpcErr, text)
	}
	if !strings.Contains(text, "demo") {
		t.Errorf("团队清单里应有 demo: %s", text)
	}

	text, isErr, _ = rpc(t, mux, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"platform_team_get","arguments":{"name":"demo"}}}`)
	if isErr {
		t.Fatalf("team_get 失败: %s", text)
	}
	if !strings.Contains(text, "novel-v2") {
		t.Errorf("团队详情应含 workflow: %s", text)
	}
}

// 工作流/技能/工具三个只读工具在任何形态下都可用 (不依赖注入)。
func TestPlatformMCP_只读能力无需注入(t *testing.T) {
	mux, _ := mcpFixture(t)
	for _, tool := range []string{
		platformmcp.ToolWorkflowsList,
		platformmcp.ToolSkillsList,
		platformmcp.ToolToolsList,
	} {
		text, isErr, rpcErr := rpc(t, mux,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`"}}`)
		if rpcErr != nil || isErr {
			t.Errorf("%s 失败: isErr=%v err=%v text=%s", tool, isErr, rpcErr, text)
			continue
		}
		if !json.Valid([]byte(text)) {
			t.Errorf("%s 的结果不是合法 JSON: %s", tool, text)
		}
	}
}

// 路径穿越守卫: 团队名走的是 safeName, MCP 侧不能成为绕过 HTTP 侧校验的后门。
func TestPlatformMCP_团队名非法直接拒(t *testing.T) {
	mux, _ := mcpFixture(t)
	text, isErr, _ := rpc(t, mux,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"platform_team_get","arguments":{"name":"../../etc"}}}`)
	if !isErr {
		t.Errorf("非法团队名应被拒: %s", text)
	}
}

// 没有 TaskService 时, task_submit 必须如实报错。
// 这是本仓的老病: "已排队, 等待主进程消费"而全仓没有消费方。
func TestPlatformMCP_无TaskService时提交如实报错(t *testing.T) {
	SetActionSink(nil)
	t.Cleanup(func() { SetActionSink(nil) })
	mux, _ := mcpFixture(t)

	text, isErr, _ := rpc(t, mux,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"platform_task_submit","arguments":{"team":"demo","objective":"跑一遍"}}}`)
	if !isErr {
		t.Fatalf("无 TaskService 时不应报成功: %s", text)
	}
	if !strings.Contains(text, platformmcp.ErrUnavailable.Error()) {
		t.Errorf("错误里应说明能力不可用: %s", text)
	}
}

// 注入 TaskService 后 (生产里 cmd/claude-go/main.go 调 SetActionSink 注入
// *agent.FileQueueTaskService) 提交真落成任务档案, 且 task_status 查得到。
func TestPlatformMCP_注入TaskService后真提交(t *testing.T) {
	stateDir := t.TempDir()
	ts := agent.NewFileQueueTaskService(agent.TaskServiceOptions{
		ActionsDir: filepath.Join(stateDir, ".dashboard", "actions"),
		// 不给 Runner: 任务建档后停在 pending, 正是"诚实"的语义, 也让测试不跑真团队。
	})
	SetActionSink(ts)
	t.Cleanup(func() { SetActionSink(nil) })

	mux := http.NewServeMux()
	MountOn(Config{StateDir: stateDir}, mux)

	text, isErr, _ := rpc(t, mux,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"platform_task_submit","arguments":{"team":"demo","workflow":"novel-v2","objective":"写一章"}}}`)
	if isErr {
		t.Fatalf("提交失败: %s", text)
	}
	var rec agent.TaskRecord
	if err := json.Unmarshal([]byte(text), &rec); err != nil {
		t.Fatalf("提交结果不是任务档案: %v (%s)", err, text)
	}
	if rec.ID == "" {
		t.Fatalf("任务档案缺 ID: %s", text)
	}
	if rec.Spec.Team != "demo" || rec.Spec.Objective != "写一章" {
		t.Errorf("档案里的 spec 失真: %+v", rec.Spec)
	}
	if rec.Source != "platform-mcp" {
		t.Errorf("Source = %q, 期望 platform-mcp (来源可追溯)", rec.Source)
	}

	// task_status 能按 ID 查回同一份档案。
	text, isErr, _ = rpc(t, mux,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"platform_task_status","arguments":{"id":"`+rec.ID+`"}}}`)
	if isErr {
		t.Fatalf("task_status 失败: %s", text)
	}
	if !strings.Contains(text, rec.ID) {
		t.Errorf("task_status 未返回同一档案: %s", text)
	}

	// tasks_list 走 TaskService 而不是 tasks.json 只读视图。
	text, isErr, _ = rpc(t, mux,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"platform_tasks_list"}}`)
	if isErr {
		t.Fatalf("tasks_list 失败: %s", text)
	}
	if !strings.Contains(text, `"source": "taskservice"`) {
		t.Errorf("有 TaskService 时 source 应为 taskservice: %s", text)
	}

	// 幂等: 同 team|workflow|objective 再提交一次拿回同一个任务 ID。
	text, _, _ = rpc(t, mux,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"platform_task_submit","arguments":{"team":"demo","workflow":"novel-v2","objective":"写一章"}}}`)
	var rec2 agent.TaskRecord
	if err := json.Unmarshal([]byte(text), &rec2); err != nil {
		t.Fatalf("第二次提交结果解析失败: %v", err)
	}
	if rec2.ID != rec.ID {
		t.Errorf("重复提交应归并到同一任务: %s vs %s", rec2.ID, rec.ID)
	}
}

// 无 TaskService 时 tasks_list 退回 tasks.json 只读视图, 且必须自报来源 ——
// 否则调用方会拿这些 id 去 task_status 查, 一律查不到。
func TestPlatformMCP_无TaskService时任务清单自报来源(t *testing.T) {
	SetActionSink(nil)
	t.Cleanup(func() { SetActionSink(nil) })
	mux, _ := mcpFixture(t)
	text, isErr, _ := rpc(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"platform_tasks_list"}}`)
	if isErr {
		t.Fatalf("tasks_list 失败: %s", text)
	}
	if !strings.Contains(text, "未接入 TaskService") {
		t.Errorf("应说明这是只读视图: %s", text)
	}
}

// 端点必须真被 :18080 的统一鉴权中间件拦住。
// 这是把它挂在 /api/ 前缀下 (而不是 /mcp) 的全部理由 —— 一个能提交任务的端点
// 落在鉴权白名单外就是提权口。
func TestPlatformMCP_受统一鉴权保护(t *testing.T) {
	mux, _ := mcpFixture(t)
	// 与 wiki.APIServer.Start / dashboard.ListenAndServe 里的装配一致。
	srv := httptest.NewServer(httpauth.Middleware(httpauth.Config{Secret: "tok"})(mux))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	resp, err := http.Post(srv.URL+PlatformMCPPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 token = %d, 期望 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+PlatformMCPPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("带正确 token = %d, 期望 200", resp2.StatusCode)
	}
	var out struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if len(out.Result.Tools) != 8 {
		t.Errorf("工具数 = %d, 期望 8", len(out.Result.Tools))
	}
}

// Backend 与 stdio 形态共用同一实现: PlatformMCPBackend 必须可导出取用,
// 否则 cmd/claude-go 的 platform-mcp-server 子命令就得再写一份。
func TestPlatformMCPBackend可被stdio复用(t *testing.T) {
	srv := NewServer(Config{StateDir: t.TempDir(), Addr: "127.0.0.1:0"})
	be := srv.PlatformMCPBackend()
	if be == nil {
		t.Fatal("PlatformMCPBackend 返回 nil")
	}
	if _, err := be.Workflows(); err != nil {
		t.Errorf("Workflows: %v", err)
	}
	// 确认它真是 platformmcp.Backend (编译期已保证, 这里再跑一次调用链)
	var _ platformmcp.Backend = be
	_ = context.Background()
}
