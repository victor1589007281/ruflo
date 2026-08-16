package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCronJobModelPatch 任务级模型字段的 patch 语义:
// 空 = 不动 / "default" 或 "-" = 清除 / 其他 = 设置 (webapp 下拉"默认"靠清除语义)。
func TestCronJobModelPatch(t *testing.T) {
	cs := NewCronScheduler("", &cronModelFakeExecutor{})
	j := &CronJob{Name: "t", Schedule: "0 6 * * *", JobType: "query", Payload: "hi"}
	if err := cs.AddJob(j); err != nil {
		t.Fatal(err)
	}
	id := j.ID

	// 空 = 不动
	if err := cs.UpdateJob(id, &CronJob{Name: "t2"}); err != nil {
		t.Fatal(err)
	}
	if cs.GetJob(id).Model != "" {
		t.Error("空 patch 不应改 Model")
	}
	// 设置
	if err := cs.UpdateJob(id, &CronJob{Model: "ollama:lfm2.5:2.6b-q4_k_m"}); err != nil {
		t.Fatal(err)
	}
	if cs.GetJob(id).Model != "ollama:lfm2.5:2.6b-q4_k_m" {
		t.Errorf("Model 未设置: %q", cs.GetJob(id).Model)
	}
	// 名称不被模型 patch 误清 (非空才覆盖纪律)
	if cs.GetJob(id).Name != "t2" {
		t.Error("其他字段被误改")
	}
	// 清除哨兵
	for _, sentinel := range []string{"default", "-"} {
		if err := cs.UpdateJob(id, &CronJob{Model: sentinel}); err != nil {
			t.Fatal(err)
		}
		if cs.GetJob(id).Model != "" {
			t.Errorf("哨兵 %q 应清除 Model, 得 %q", sentinel, cs.GetJob(id).Model)
		}
		_ = cs.UpdateJob(id, &CronJob{Model: "ollama:x"})
	}
}

// TestCronTriggerJob 手动触发: 真实执行一次 (替代此前无消费方的 cron.trigger 假承诺),
// 不动调度计划。
func TestCronTriggerJob(t *testing.T) {
	exec := &cronModelFakeExecutor{}
	cs := NewCronScheduler("", exec)
	j := &CronJob{Name: "sync-t", Schedule: "0 6 * * *", JobType: "sync", Payload: "ima"}
	if err := cs.AddJob(j); err != nil {
		t.Fatal(err)
	}
	if err := cs.TriggerJob(j.ID); err != nil {
		t.Fatal(err)
	}
	// executeJob 是 goroutine, 等执行计数落地 (最多 2s)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&exec.syncCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&exec.syncCalls) != 1 {
		t.Fatal("TriggerJob 未真实执行 sync")
	}
	if got := cs.GetJob(j.ID); got.RunCount != 1 {
		t.Errorf("RunCount 应为 1, 得 %d", got.RunCount)
	}
	if err := cs.TriggerJob("cron-不存在"); err == nil {
		t.Error("不存在的任务应报错")
	}
}

// TestCronQueryModelDispatch job.Model 非空时走 CronModelExecutor;
// 执行器不实现该接口时回退 SendQuery (有日志, 不静默)。
func TestCronQueryModelDispatch(t *testing.T) {
	exec := &cronModelFakeExecutor{}
	cs := NewCronScheduler("", exec)
	j := &CronJob{Name: "q", Schedule: "0 6 * * *", JobType: "query", Payload: "你好",
		Model: "ollama:lfm2.5:2.6b-q4_k_m"}
	if err := cs.AddJob(j); err != nil {
		t.Fatal(err)
	}
	if err := cs.TriggerJob(j.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&exec.modelCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&exec.modelCalls) != 1 {
		t.Fatal("带模型的 query 未走 SendQueryWithModel")
	}
	if atomic.LoadInt32(&exec.plainCalls) != 0 {
		t.Error("不应回退到 SendQuery")
	}
	if exec.gotModel != "ollama:lfm2.5:2.6b-q4_k_m" {
		t.Errorf("执行器收到的模型不对: %q", exec.gotModel)
	}
}

// cronModelFakeExecutor 测试执行器: 同时实现 CronExecutor 与 CronModelExecutor。
type cronModelFakeExecutor struct {
	syncCalls  int32
	plainCalls int32
	modelCalls int32
	gotModel   string
}

func (f *cronModelFakeExecutor) RunWorkflow(context.Context, string, string, string, string) error {
	return nil
}
func (f *cronModelFakeExecutor) SendQuery(context.Context, string, string) (string, error) {
	atomic.AddInt32(&f.plainCalls, 1)
	return "ok", nil
}
func (f *cronModelFakeExecutor) SendQueryWithModel(_ context.Context, _, _, model string) (string, error) {
	atomic.AddInt32(&f.modelCalls, 1)
	f.gotModel = model
	return "ok:" + model, nil
}
func (f *cronModelFakeExecutor) RunCommand(context.Context, string, string) error { return nil }
func (f *cronModelFakeExecutor) Notify(string, string)                            {}
func (f *cronModelFakeExecutor) WikiOrganize(context.Context, string) (string, error) {
	return "", nil
}
func (f *cronModelFakeExecutor) WikiHealthCheck(context.Context) (string, error) { return "", nil }
func (f *cronModelFakeExecutor) WikiLint(context.Context) (string, error)        { return "", nil }
func (f *cronModelFakeExecutor) TriggerSync(context.Context, string) (string, error) {
	atomic.AddInt32(&f.syncCalls, 1)
	return "新增 0", nil
}

// 防止 FormatJobList 模型行回归: 设置了模型的任务应在列表里露出别名。
func TestCronFormatJobListShowsModel(t *testing.T) {
	cs := NewCronScheduler("", &cronModelFakeExecutor{})
	j := &CronJob{Name: "q", Schedule: "0 6 * * *", JobType: "query", Payload: "x",
		Model: "ollama:lfm2.5:2.6b-q4_k_m"}
	if err := cs.AddJob(j); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cs.FormatJobList(), "ollama:lfm2.5") {
		t.Error("任务列表未显示模型别名")
	}
}
