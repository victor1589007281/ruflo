package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

// stubCronExec 是 CronExecutor 的空实现 (测试不实际执行任务)。
type stubCronExec struct{}

func (stubCronExec) RunWorkflow(context.Context, string, string, string, string) error { return nil }
func (stubCronExec) SendQuery(context.Context, string, string) (string, error)         { return "", nil }
func (stubCronExec) RunCommand(context.Context, string, string) error                 { return nil }
func (stubCronExec) Notify(string, string)                                            {}
func (stubCronExec) WikiOrganize(context.Context, string) (string, error)             { return "", nil }
func (stubCronExec) WikiHealthCheck(context.Context) (string, error)                  { return "", nil }
func (stubCronExec) WikiLint(context.Context) (string, error)                         { return "", nil }
func (stubCronExec) TriggerSync(context.Context, string) (string, error)              { return "", nil }

// TestCronWriteHandlers 验证 /api/cron 写接口经解析器打到活动调度器的完整链路
// (create/disable/update/delete), 以及未注入解析器时返回 501。
// 这是线上唯一无法用独立 dashboard 冒烟覆盖的"已注入"路径。
func TestCronWriteHandlers(t *testing.T) {
	dir := t.TempDir()
	sched := agent.NewCronScheduler(filepath.Join(dir, "cron"), stubCronExec{})
	s := &Server{cfg: Config{StateDir: dir}, provider: NewProvider(dir, 0)}
	s.SetCronController(func() CronController { return sched })

	// 未注入解析器 → POST 应 501 (独立 dashboard :7777 的行为)
	sNil := &Server{cfg: Config{StateDir: dir}, provider: NewProvider(dir, 0)}
	rec := httptest.NewRecorder()
	sNil.handleCron(rec, httptest.NewRequest("POST", "/api/cron", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil resolver POST: want 501, got %d", rec.Code)
	}

	// 创建
	rec = httptest.NewRecorder()
	s.handleCron(rec, httptest.NewRequest("POST", "/api/cron",
		strings.NewReader(`{"name":"t1","schedule":"0 3 * * *","jobType":"command","payload":"echo hi"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	jobs := sched.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("after create: want 1 job, got %d", len(jobs))
	}
	id := jobs[0].ID
	if jobs[0].Schedule != "0 3 * * *" || jobs[0].JobType != "command" {
		t.Fatalf("job stored wrong: schedule=%q jobType=%q", jobs[0].Schedule, jobs[0].JobType)
	}

	// GET 经活动调度器(:18080 路径)应列出刚建的任务
	rec = httptest.NewRecorder()
	s.handleCron(rec, httptest.NewRequest("GET", "/api/cron", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("GET should list created job %s; got %s", id, rec.Body.String())
	}

	// 非法 cron → 400
	rec = httptest.NewRecorder()
	s.handleCron(rec, httptest.NewRequest("POST", "/api/cron",
		strings.NewReader(`{"schedule":"not-a-cron","jobType":"command","payload":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cron: want 400, got %d", rec.Code)
	}

	// 停用
	rec = httptest.NewRecorder()
	s.handleCronItem(rec, httptest.NewRequest("PATCH", "/api/cron/"+id, strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: want 200, got %d", rec.Code)
	}
	if sched.GetJob(id).Enabled {
		t.Fatal("job should be disabled after PATCH enabled=false")
	}

	// 改 schedule
	rec = httptest.NewRecorder()
	s.handleCronItem(rec, httptest.NewRequest("PATCH", "/api/cron/"+id, strings.NewReader(`{"schedule":"0 9 * * *"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d", rec.Code)
	}
	if got := sched.GetJob(id).Schedule; got != "0 9 * * *" {
		t.Fatalf("schedule not updated: %q", got)
	}

	// 删除
	rec = httptest.NewRecorder()
	s.handleCronItem(rec, httptest.NewRequest("DELETE", "/api/cron/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d", rec.Code)
	}
	if sched.GetJob(id) != nil {
		t.Fatal("job should be gone after DELETE")
	}
}
