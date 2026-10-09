package dashboard

// workflow_revision_handlers_test.go —— P3 版本守卫在 **HTTP 这一层**的契约。
//
// 与 pkg/agent/workflow_revision_test.go 的分工: 那边查的是注册表语义, 这边查的是
// **调用方看得到的那个字节**。两边都要有, 因为守卫的价值全在"调用方能分辨"上:
//   - 版本冲突必须是 **409**, 不是 400。前端据此提示"刷新后重试"而不是"改你的表单" ——
//     冲突时用户的定义一个字都没写错, 让他去改会改坏一份本来对的东西。
//   - 新建时 GET 出来的 revision 必须能被**原样回传**并成功改写。这条是端到端闭环:
//     只测服务端语义的话, "GET 不给 revision" 与 "PUT 不收 revision" 各自都测不到。
//
// 判据全程走 HTTP 往返 (与 TestDynamicWorkflowOverHTTP 同形), 不直接调 handler ——
// 直接调 handler 会绕过 DTO 序列化, 而"字段名对不上"正是这条链上最容易出的错。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

// revSrv 起一个只挂了工作流三个端点的测试服务。
func revSrv(t *testing.T) *httptest.Server {
	t.Helper()
	s := &Server{cfg: Config{StateDir: t.TempDir()}, provider: NewProvider(t.TempDir(), 0)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workflows", s.handleWorkflows)
	mux.HandleFunc("/api/workflows/", s.handleWorkflow)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// revPost 发一个注册请求, 返回状态码与响应体。
func revPost(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/workflows", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func revBody(prompt string) string {
	return `{"name":"http-rev","mode":"pipeline","stages":[{"name":"a","role":"coder","prompt":"` + prompt + `"}]}`
}

// TestWorkflowPutRequiresRevisionOverHTTP 走真实 HTTP: PUT 不带 revision 必须被拒。
//
// 这条锁的是**路由与字段名**: 请求体里的 `revision` 若被 JSON 解成 0 (字段名写错、
// 或嵌在外层对象里没解出来), 守卫同样会拒 —— 那份"拒"看起来是对的, 只是原因不对,
// 而真正带了版本的请求会**永远**失败。所以下面还有一条"带了就必须成功"。
func TestWorkflowPutRequiresRevisionOverHTTP(t *testing.T) {
	ts := revSrv(t)
	agent.UnregisterWorkflow("http-rev")
	t.Cleanup(func() { agent.UnregisterWorkflow("http-rev") })

	if code, body := revPost(t, ts, revBody("v1 {objective}")); code != http.StatusOK {
		t.Fatalf("首次注册应 200, got %d %s", code, body)
	}

	// 不带 revision 的 PUT → 409
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/workflows/http-rev",
		strings.NewReader(`{"mode":"pipeline","stages":[{"name":"a","role":"coder","prompt":"覆盖 {objective}"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("缺 revision 的改写应 409 (前端据此提示刷新重试, 而不是让用户改表单), got %d", resp.StatusCode)
	}

	// 同名的 POST (走创建口) → 也必须是 409, 不是 400 也不是 200。
	if code, body := revPost(t, ts, revBody("同名再来一次 {objective}")); code != http.StatusConflict {
		t.Fatalf("同名 re-POST 应 409, got %d %s", code, body)
	}
}

// TestWorkflowGetRevisionRoundTripsOverHTTP 端到端闭环: GET 出的 revision 原样回传必须成功。
//
// 这是**唯一**能发现"GET 不给 revision"或"PUT 读的不是同一个字段"的用例 ——
// 两条各自单独测都测不出来, 因为它们各自的实现看起来都对, 断的是中间那根线。
func TestWorkflowGetRevisionRoundTripsOverHTTP(t *testing.T) {
	ts := revSrv(t)
	agent.UnregisterWorkflow("http-rev")
	t.Cleanup(func() { agent.UnregisterWorkflow("http-rev") })

	if code, body := revPost(t, ts, revBody("v1 {objective}")); code != http.StatusOK {
		t.Fatalf("首次注册应 200, got %d %s", code, body)
	}

	// 按 webapp 的真实动作: GET /api/workflows 列表, 取出这一条。
	resp, err := http.Get(ts.URL + "/api/workflows")
	if err != nil {
		t.Fatal(err)
	}
	var list []workflowDefDTO
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var got *workflowDefDTO
	for i := range list {
		if list[i].Name == "http-rev" {
			got = &list[i]
		}
	}
	if got == nil {
		t.Fatal("列表里应含刚注册的动态工作流")
	}
	if got.Revision != 1 {
		t.Fatalf("列表里的 revision 应为 1 (客户端要回传的就是它), got %d", got.Revision)
	}

	// 把 GET 到的值原样回传。
	put := `{"revision":` + itoa(got.Revision) + `,"mode":"pipeline","stages":[{"name":"a","role":"coder","prompt":"v2 {objective}"}]}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/workflows/http-rev", strings.NewReader(put))
	req.Header.Set("Content-Type", "application/json")
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("带 GET 到的 revision 改写应成功, got %d", pr.StatusCode)
	}
	var out map[string]json.RawMessage
	if err := json.NewDecoder(pr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	var wf workflowDefDTO
	if err := json.Unmarshal(out["workflow"], &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Revision != 2 {
		t.Fatalf("响应里应回新的 revision=2 (前端紧接着还要用它), got %d", wf.Revision)
	}
	// 落盘的那一份也必须已经是 r2 —— 否则重启后版本倒退。
	if a := agent.RevisionOf("http-rev"); a != 2 {
		t.Fatalf("注册表里应为 r2, got %d", a)
	}
}

// TestWorkflowDeleteThenRecreateStartsAtOne 删除后重建是一个**新**资产, 版本从头开始。
//
// 这条锁的是"删除有没有把注册表清干净": 若 UnregisterWorkflow 只摘图不删定义,
// 重建时会撞上守卫 (名字还在) —— 而用户的动作是合法的"删掉重来"。
func TestWorkflowDeleteThenRecreateStartsAtOne(t *testing.T) {
	ts := revSrv(t)
	agent.UnregisterWorkflow("http-rev")
	t.Cleanup(func() { agent.UnregisterWorkflow("http-rev") })

	if code, body := revPost(t, ts, revBody("v1 {objective}")); code != http.StatusOK {
		t.Fatalf("首次注册应 200, got %d %s", code, body)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/workflows/http-rev", nil)
	dr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dr.Body.Close()
	if dr.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200, got %d", dr.StatusCode)
	}
	if code, body := revPost(t, ts, revBody("重建 {objective}")); code != http.StatusOK {
		t.Fatalf("删除后重建应 200 (守卫不该拦住一个合法的删掉重来), got %d %s", code, body)
	}
	if a := agent.RevisionOf("http-rev"); a != 1 {
		t.Fatalf("重建后应是新资产 r1, got %d", a)
	}
}

// itoa 避免为一个用例引入 strconv (本文件只用到这一处)。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
