package platformmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend 记录被调到的方法, 并可指定错误。
type fakeBackend struct {
	calls     []string
	submitted TaskSubmit
	failWith  error
}

func (f *fakeBackend) note(name string) { f.calls = append(f.calls, name) }

func (f *fakeBackend) Teams() (any, error) {
	f.note("Teams")
	return []map[string]string{{"name": "demo"}}, f.failWith
}
func (f *fakeBackend) Team(name string) (any, error) {
	f.note("Team:" + name)
	return map[string]string{"name": name}, f.failWith
}
func (f *fakeBackend) Workflows() (any, error) {
	f.note("Workflows")
	return []string{"novel-v2"}, f.failWith
}
func (f *fakeBackend) Skills() (any, error) {
	f.note("Skills")
	return []string{"sk"}, f.failWith
}
func (f *fakeBackend) Tools() (any, error) {
	f.note("Tools")
	return []string{"Read"}, f.failWith
}
func (f *fakeBackend) Tasks() (any, error) {
	f.note("Tasks")
	return []string{"t1"}, f.failWith
}
func (f *fakeBackend) SubmitTask(spec TaskSubmit) (any, error) {
	f.note("SubmitTask")
	f.submitted = spec
	if f.failWith != nil {
		return nil, f.failWith
	}
	return map[string]string{"id": "task-1", "state": "pending"}, nil
}
func (f *fakeBackend) TaskStatus(id string) (any, error) {
	f.note("TaskStatus:" + id)
	return map[string]string{"id": id}, f.failWith
}

func call(t *testing.T, s *Server, body string) rpcResponse {
	t.Helper()
	raw, ok := s.HandleRPC([]byte(body))
	if !ok {
		t.Fatalf("请求 %s 未产生响应", body)
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, raw)
	}
	return resp
}

// initialize 必须回协议版本与 serverInfo, 否则任何 MCP 客户端都握不上手。
func TestInitialize握手(t *testing.T) {
	resp := call(t, New(&fakeBackend{}), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if resp.Error != nil {
		t.Fatalf("initialize 报错: %+v", resp.Error)
	}
	m, _ := resp.Result.(map[string]any)
	if m["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, 期望 %s", m["protocolVersion"], ProtocolVersion)
	}
}

// 工具名是对外契约 (下游 agent 会写进配置)。这条钉住清单, 与 :18080 路径同等对待。
func Test工具清单是冻结契约(t *testing.T) {
	want := []string{
		ToolTeamsList, ToolTeamGet, ToolWorkflowsList, ToolSkillsList,
		ToolToolsList, ToolTasksList, ToolTaskSubmit, ToolTaskStatus,
	}
	got := New(&fakeBackend{}).Tools()
	if len(got) != len(want) {
		t.Fatalf("工具数 = %d, 期望 %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("第 %d 个工具 = %q, 期望 %q", i, got[i].Name, w)
		}
		if got[i].Description == "" {
			t.Errorf("%s 缺 description (agent 靠它选工具)", w)
		}
		if !json.Valid(got[i].InputSchema) {
			t.Errorf("%s 的 inputSchema 不是合法 JSON", w)
		}
	}
	// tools/list 走 RPC 也要能拿到同一份。
	resp := call(t, New(&fakeBackend{}), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	b, _ := json.Marshal(resp.Result)
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Errorf("tools/list 结果里缺 %s", w)
		}
	}
}

func Test工具调用透传到Backend(t *testing.T) {
	be := &fakeBackend{}
	s := New(be)

	resp := call(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"platform_team_get","arguments":{"name":"demo"}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call 报错: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "demo") {
		t.Errorf("结果里应含团队名: %s", b)
	}
	if len(be.calls) != 1 || be.calls[0] != "Team:demo" {
		t.Errorf("Backend 调用记录 = %v, 期望 [Team:demo]", be.calls)
	}
}

func Test任务提交参数校验(t *testing.T) {
	be := &fakeBackend{}
	s := New(be)

	// 缺 objective → isError 的文本结果, 而不是协议错误。
	resp := call(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"platform_task_submit","arguments":{"team":"t"}}}`)
	if resp.Error != nil {
		t.Fatalf("参数不全应走 isError 而不是协议错误, got %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), `"isError":true`) {
		t.Errorf("缺 objective 应 isError=true: %s", b)
	}
	if len(be.calls) != 0 {
		t.Errorf("参数不合法时不应触达 Backend, got %v", be.calls)
	}

	// 齐全 → 落到 Backend, 参数不丢。
	resp = call(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"platform_task_submit","arguments":{"team":"t1","workflow":"novel-v2","objective":"写一章","language":"go"}}}`)
	if resp.Error != nil {
		t.Fatalf("提交报错: %+v", resp.Error)
	}
	if be.submitted.Team != "t1" || be.submitted.Workflow != "novel-v2" ||
		be.submitted.Objective != "写一章" || be.submitted.Language != "go" {
		t.Errorf("参数透传失真: %+v", be.submitted)
	}
}

// 未知工具走 JSON-RPC -32601; 工具自身失败走 isError。两者不能混, 否则客户端
// 分不清"我调错了"和"平台没这个能力"。
func Test未知工具与工具失败的区分(t *testing.T) {
	resp := call(t, New(&fakeBackend{}), `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope"}}`)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("未知工具应回 -32601, got %+v", resp.Error)
	}

	be := &fakeBackend{failWith: errors.New("boom")}
	resp = call(t, New(be), `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"platform_teams_list"}}`)
	if resp.Error != nil {
		t.Fatalf("工具失败不应变成协议错误: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "boom") || !strings.Contains(string(b), `"isError":true`) {
		t.Errorf("工具失败应以 isError 文本回传原因: %s", b)
	}
}

// Backend 为 nil (例如尚未装配) 时不能 panic, 要如实说不可用。
func Test无Backend时如实报不可用(t *testing.T) {
	resp := call(t, New(nil), `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"platform_teams_list"}}`)
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), ErrUnavailable.Error()) {
		t.Errorf("应说明能力不可用: %s", b)
	}
}

// 通知 (无 id) 按 JSON-RPC 规范不回响应。
func Test通知不回响应(t *testing.T) {
	if _, ok := New(&fakeBackend{}).HandleRPC([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); ok {
		t.Error("notifications/initialized 不应产生响应")
	}
}

func TestHTTP传输(t *testing.T) {
	s := New(&fakeBackend{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// GET → 405 (只接受 POST 的 JSON-RPC)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, 期望 405", resp.StatusCode)
	}

	// POST 单条
	resp, err = http.Post(srv.URL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var one rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&one); err != nil {
		t.Fatalf("单条响应解析失败: %v", err)
	}
	if one.Error != nil {
		t.Errorf("tools/list 报错: %+v", one.Error)
	}

	// POST 批量: 两条请求 + 一条通知 → 只回两条
	resp2, err := http.Post(srv.URL, "application/json", strings.NewReader(
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var batch []json.RawMessage
	if err := json.NewDecoder(resp2.Body).Decode(&batch); err != nil {
		t.Fatalf("批量响应解析失败: %v", err)
	}
	if len(batch) != 2 {
		t.Errorf("批量响应条数 = %d, 期望 2 (通知不回)", len(batch))
	}
}

func TestStdio传输(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"platform_workflows_list"}}` + "\n")
	var out bytes.Buffer
	if err := New(&fakeBackend{}).ServeStdio(in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout 行数 = %d, 期望 2 (通知不回), 内容:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], "novel-v2") {
		t.Errorf("第二条应含工作流结果: %s", lines[1])
	}
}

// 报文不合法 → -32700, 且必须是一条合法 JSON (不能返回空字节让客户端挂住)。
func Test解析错误(t *testing.T) {
	raw, ok := New(&fakeBackend{}).HandleRPC([]byte(`{not json`))
	if !ok {
		t.Fatal("解析错误也应回响应")
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("错误响应本身不是合法 JSON: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("期望 -32700, got %+v", resp.Error)
	}
}
