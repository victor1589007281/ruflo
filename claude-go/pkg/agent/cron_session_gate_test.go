package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cron_session_gate_test.go — "任务的 chatID 空不得静默跑进匿名会话"。
//
// # 为什么这条值得单独一组
//
// chatID 就是被唤醒的那个会话的身份 (会话级轨迹按它命桶, 见
// pkg/feishu/session_trace.go)。它为空时, 一次运行会:
//   - 写进 `trace-run--<n>` 这种归不了任何团队的桶 → 轨迹页上这次运行等于没发生;
//   - 那个本该被唤醒的员工**没被唤醒**, 而任务报"成功"。
//
// 这是"看着跑了"最典型的一种 —— 所以闸门必须落在**拒绝执行**上, 而不是记一条日志
// 然后照跑 (记日志的版本与现在的区别只有一行输出, 在 dashboard 上完全看不出差别)。

// gateFakeExecutor 记录每一个真实到达执行器的调用。
type gateFakeExecutor struct {
	queryCalls   int32
	commandCalls int32
	syncCalls    int32
	notifies     int32
	lastNotify   string
}

func (f *gateFakeExecutor) RunWorkflow(context.Context, string, string, string, string) error {
	return nil
}
func (f *gateFakeExecutor) SendQuery(context.Context, string, string) (string, error) {
	atomic.AddInt32(&f.queryCalls, 1)
	return "ok", nil
}
func (f *gateFakeExecutor) RunCommand(context.Context, string, string) error {
	atomic.AddInt32(&f.commandCalls, 1)
	return nil
}
func (f *gateFakeExecutor) Notify(_ string, msg string) {
	atomic.AddInt32(&f.notifies, 1)
	f.lastNotify = msg
}
func (f *gateFakeExecutor) WikiOrganize(context.Context, string) (string, error) { return "", nil }
func (f *gateFakeExecutor) WikiHealthCheck(context.Context) (string, error)      { return "", nil }
func (f *gateFakeExecutor) WikiLint(context.Context) (string, error)             { return "", nil }
func (f *gateFakeExecutor) TriggerSync(context.Context, string) (string, error) {
	atomic.AddInt32(&f.syncCalls, 1)
	return "ok", nil
}

// runTrigger 触发一条 job 并等它跑完 (executeJob 是 goroutine)。
// 用 RunCount 落地当"跑完了"的判据 —— 它在两条路径上都会 +1 (含被闸门拦下的那条),
// 所以等待它不会把"被拦下"误当成"还没跑完"。
//
// ---------------------------------------------------------------------------
// 返回的是**锁内取的快照**, 不是 GetJob 那个共享指针
// ---------------------------------------------------------------------------
//
// GetJob 是在锁内查表、出锁之后把 map 里那**同一个指针**交给调用方, 而 executeJob
// (goroutine) 紧接着仍在写它的 FailCount/LastResult/RunCount。调用方出锁后读 got.X
// 就是一次无同步的读 —— `-race` 下本文件四条用例全红, 不加 -race 则一路绿。
//
// 判据本身要的就是"跑完之后这几个字段是多少", 所以在锁内把值拷一份带走即可:
// CronJob 是纯值类型 (字符串/整型/time.Time), 拷贝不共享任何可变状态, 调用方读
// 自己的副本怎么读都不会与执行 goroutine 相撞。
func runTrigger(t *testing.T, cs *CronScheduler, job *CronJob) *CronJob {
	t.Helper()
	if err := cs.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := cs.TriggerJob(job.ID); err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, done := jobSnapshot(cs, job.ID); done {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("任务一直没执行完")
	return nil
}

// jobSnapshot 在锁内读 job 并返回一份拷贝; done 表示 RunCount 已落地。
func jobSnapshot(cs *CronScheduler, id string) (*CronJob, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	j := cs.jobs[id]
	if j == nil {
		return nil, false
	}
	snap := *j
	return &snap, snap.RunCount > 0
}

func TestCronQueryWithoutChatIDIsRefused(t *testing.T) {
	exec := &gateFakeExecutor{}
	cs := NewCronScheduler("", exec)

	got := runTrigger(t, cs, &CronJob{
		Name: "唤醒巡检员", Schedule: "0 6 * * *", JobType: "query", Payload: "巡一遍",
		// ChatID 空 —— 这就是故障注入点
	})

	if n := atomic.LoadInt32(&exec.queryCalls); n != 0 {
		t.Fatalf("会话名为空时**不该**真的去跑 (执行器被调了 %d 次) —— "+
			"它会跑进一个匿名会话: 该唤醒的员工没醒, 任务却报成功", n)
	}
	if got.FailCount != 1 {
		t.Errorf("FailCount 应为 1 (让它在 dashboard 上看得见), 得到 %d", got.FailCount)
	}
	if !strings.Contains(got.LastResult, "失败") || !strings.Contains(got.LastResult, "会话") {
		t.Errorf("LastResult 应说明失败原因是缺会话, 得到 %q", got.LastResult)
	}
	// 通知照样要发。★ 但别把这条读成"有人会收到飞书消息": 这里 chatID 是空的,
	// 通知本身就没有收件人 (headless 下退化成一行日志, 在线下飞书 API 会拒)。
	// 真正让人**必须**看见的是上面那两个字段 (dashboard 的 /api/cron 会显示
	// failCount 与 lastResult) —— 通知只是尽力而为的第二条腿, 不是判据。
	if atomic.LoadInt32(&exec.notifies) == 0 || !strings.Contains(exec.lastNotify, "未执行") {
		t.Errorf("被拦下时也应尝试通知 (第二条腿可以不达, 但不能不试), 得到 %q", exec.lastNotify)
	}
}

// TestCronQueryWithChatIDStillRuns 是上一条的**反向对照**。
//
// 没有它, 把闸门写成 "query 一律拒绝" 也能让上面那条通过 —— 而那会让整个
// cron query 功能消失。
func TestCronQueryWithChatIDStillRuns(t *testing.T) {
	exec := &gateFakeExecutor{}
	cs := NewCronScheduler("", exec)

	got := runTrigger(t, cs, &CronJob{
		Name: "唤醒巡检员", Schedule: "0 6 * * *", JobType: "query",
		Payload: "巡一遍", ChatID: "dba-oncall",
	})

	if n := atomic.LoadInt32(&exec.queryCalls); n != 1 {
		t.Fatalf("具名会话的 query 应真实执行 1 次, 得到 %d 次", n)
	}
	if got.FailCount != 0 {
		t.Errorf("正常执行不该算失败, LastResult=%q", got.LastResult)
	}
}

// TestCronCommandWithoutChatIDIsRefused — command 与 query 同一条纪律。
func TestCronCommandWithoutChatIDIsRefused(t *testing.T) {
	exec := &gateFakeExecutor{}
	cs := NewCronScheduler("", exec)

	got := runTrigger(t, cs, &CronJob{
		Name: "敲个命令", Schedule: "0 6 * * *", JobType: "command", Payload: "ls",
	})

	if n := atomic.LoadInt32(&exec.commandCalls); n != 0 {
		t.Errorf("会话名为空的 command 不该执行, 被调了 %d 次", n)
	}
	if got.FailCount != 1 {
		t.Error("应记为失败")
	}
}

// TestCronSyncWithoutChatIDStillRuns 是"闸门别开太大"的对照。
//
// sync / wiki-* 这几类**不寻址会话** —— chatID 只用于发通知。给它们也套上会话闸门,
// 等于把一批本来正常工作的定时任务凭空停掉, 而且报的是"缺少会话"这种说不通的理由。
func TestCronSyncWithoutChatIDStillRuns(t *testing.T) {
	exec := &gateFakeExecutor{}
	cs := NewCronScheduler("", exec)

	got := runTrigger(t, cs, &CronJob{
		Name: "同步", Schedule: "0 6 * * *", JobType: "sync", Payload: "ima",
	})

	if n := atomic.LoadInt32(&exec.syncCalls); n != 1 {
		t.Errorf("sync 不寻址会话, 空 chatID 也应执行, 得到 %d 次", n)
	}
	if got.FailCount != 0 {
		t.Errorf("sync 不该被判失败, LastResult=%q", got.LastResult)
	}
}

// TestNeedsSessionTargetPin 把"哪些任务类型要会话"钉成一张显式的表。
//
// 加新任务类型时必须有人**回答**这个问题 —— 默认 false (不需要) 的后果正是这次
// 要消灭的那种静默。这条测试让"漏答"在 diff 里看得见。
func TestNeedsSessionTargetPin(t *testing.T) {
	mustHave := []string{"query", "command", "workflow"}
	mustNot := []string{"sync", "wiki-organize", "wiki-health-check", "wiki-lint"}
	for _, jt := range mustHave {
		if !needsSessionTarget(jt) {
			t.Errorf("%q 寻址会话, 必须要求 chatID", jt)
		}
	}
	for _, jt := range mustNot {
		if needsSessionTarget(jt) {
			t.Errorf("%q 只用 chatID 发通知, 不该要求它", jt)
		}
	}
}
