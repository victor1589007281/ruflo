package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// gateRunner 可控的假运行器: 一直阻塞到 release 被关闭, 记录被调次数。
type gateRunner struct {
	release chan struct{}
	calls   atomic.Int64
	output  string
	err     error
}

func newGateRunner(out string) *gateRunner {
	return &gateRunner{release: make(chan struct{}), output: out}
}

func (g *gateRunner) Run(ctx context.Context, _ TaskRecord) (string, error) {
	g.calls.Add(1)
	select {
	case <-g.release:
		return g.output, g.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// instantRunner 立刻成功的假运行器。
type instantRunner struct {
	calls atomic.Int64
	specs chan TaskSpec
}

func newInstantRunner() *instantRunner {
	return &instantRunner{specs: make(chan TaskSpec, 16)}
}

func (r *instantRunner) Run(_ context.Context, rec TaskRecord) (string, error) {
	r.calls.Add(1)
	select {
	case r.specs <- rec.Spec:
	default:
	}
	return "done:" + rec.Spec.Objective, nil
}

func newTestSvc(t *testing.T, runner TaskRunner) *FileQueueTaskService {
	t.Helper()
	return NewFileQueueTaskService(TaskServiceOptions{
		Store:        statestore.NewMemStore(),
		Runner:       runner,
		PollInterval: 10 * time.Millisecond,
	})
}

// TestSubmitIdempotentSameKeyReturnsSameTask 断言: 同幂等键的两次提交返回**同一个任务**
// (而不是新建第二个), 且运行器只被调用一次。这是取代 tryStartTeam 状态判断去重的核心语义。
func TestSubmitIdempotentSameKeyReturnsSameTask(t *testing.T) {
	runner := newGateRunner("ok")
	svc := newTestSvc(t, runner)
	spec := TaskSpec{Team: "alpha", Workflow: "techblog", Objective: "写一篇文章"}

	a, err := svc.Submit(spec, SubmitOpts{})
	if err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}
	b, err := svc.Submit(spec, SubmitOpts{})
	if err != nil {
		t.Fatalf("重复 Submit: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("同 team 同 objective 重复提交应返回同一任务, got %s vs %s", a.ID, b.ID)
	}
	if n := len(svc.List(TaskFilter{})); n != 1 {
		t.Fatalf("应只有 1 份任务档案, got %d", n)
	}

	close(runner.release)
	if _, err := svc.Wait(context.Background(), a.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("运行器应只被调用 1 次 (幂等), got %d", got)
	}
}

// TestSubmitIdempotentScopeSemantics 断言两种归并范围的差异:
// Active(默认) 在任务到终态后允许重跑(新任务), Forever 则永远归并到同一份。
func TestSubmitIdempotentScopeSemantics(t *testing.T) {
	svc := newTestSvc(t, newInstantRunner())
	spec := TaskSpec{Team: "beta", Workflow: "techblog", Objective: "目标X"}

	first, err := svc.Submit(spec, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), first.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Active: 前一个已终态 → 允许新建 (否则"重跑一遍"永远做不到)
	second, err := svc.Submit(spec, SubmitOpts{})
	if err != nil {
		t.Fatalf("重跑 Submit: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("终态后的重复提交应是新任务, 却复用了 %s", first.ID)
	}
	if _, err := svc.Wait(context.Background(), second.ID); err != nil {
		t.Fatalf("Wait 第二个: %v", err)
	}

	// Forever: 即使终态也归并
	k := "action:fixed-1"
	x, err := svc.Submit(spec, SubmitOpts{IdempotencyKey: k, IdemScope: IdemScopeForever})
	if err != nil {
		t.Fatalf("Forever Submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), x.ID); err != nil {
		t.Fatalf("Wait Forever: %v", err)
	}
	y, err := svc.Submit(spec, SubmitOpts{IdempotencyKey: k, IdemScope: IdemScopeForever})
	if err != nil {
		t.Fatalf("Forever 重复 Submit: %v", err)
	}
	if y.ID != x.ID {
		t.Fatalf("IdemScopeForever 应始终归并到同一任务, got %s vs %s", y.ID, x.ID)
	}
}

// TestSubmitActiveIndexBlocksConcurrentSameTeam 断言: 即使调用方给了**不同的显式幂等键**,
// 同 team 同 objective 只要还活着就归并到同一任务 —— 这是 tryStartTeam 竞态的真正修复。
func TestSubmitActiveIndexBlocksConcurrentSameTeam(t *testing.T) {
	runner := newGateRunner("ok")
	svc := newTestSvc(t, runner)
	spec := TaskSpec{Team: "gamma", Workflow: "techblog", Objective: "同一个目标"}

	a, err := svc.Submit(spec, SubmitOpts{IdempotencyKey: "key-A"})
	if err != nil {
		t.Fatalf("Submit A: %v", err)
	}
	b, err := svc.Submit(spec, SubmitOpts{IdempotencyKey: "key-B"})
	if err != nil {
		t.Fatalf("Submit B: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("同团队同目标的活跃任务应被归并, got %s vs %s", a.ID, b.ID)
	}
	close(runner.release)
	if _, err := svc.Wait(context.Background(), a.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("同团队不应被并发跑两遍, 运行器调用 %d 次", got)
	}
}

// TestSubmitConcurrentIdempotent 并发提交 20 次, 断言只建了 1 份档案、只跑了 1 次。
func TestSubmitConcurrentIdempotent(t *testing.T) {
	runner := newGateRunner("ok")
	svc := newTestSvc(t, runner)
	spec := TaskSpec{Team: "delta", Workflow: "techblog", Objective: "并发提交"}

	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := svc.Submit(spec, SubmitOpts{})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			ids[i] = rec.ID
		}(i)
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("第 %d 次提交拿到了不同任务: %s vs %s", i, id, ids[0])
		}
	}
	if n := len(svc.List(TaskFilter{})); n != 1 {
		t.Fatalf("并发提交应只建 1 份档案, got %d", n)
	}
	close(runner.release)
	if _, err := svc.Wait(context.Background(), ids[0]); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("运行器应只跑 1 次, got %d", got)
	}
}

// TestWaitBlocksUntilTerminal 断言 Wait 真的等到终态: 运行器未释放前 Wait 不返回,
// 释放后返回 completed 且带运行器的输出。
func TestWaitBlocksUntilTerminal(t *testing.T) {
	runner := newGateRunner("产物摘要")
	svc := newTestSvc(t, runner)
	rec, err := svc.Submit(TaskSpec{Team: "eps", Workflow: "techblog", Objective: "等终态"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	done := make(chan TaskResult, 1)
	go func() {
		res, err := svc.Wait(context.Background(), rec.ID)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		t.Fatalf("运行器未完成时 Wait 就返回了: %+v", res)
	case <-time.After(150 * time.Millisecond):
		// 期望: 仍在等
	}

	close(runner.release)
	select {
	case res := <-done:
		if res.State != TaskStateCompleted {
			t.Fatalf("终态应为 completed, got %s (err=%s)", res.State, res.Error)
		}
		if res.Output != "产物摘要" {
			t.Fatalf("应带回运行器输出, got %q", res.Output)
		}
		if res.FinishedAt.IsZero() {
			t.Fatal("终态应有 FinishedAt")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("释放后 Wait 未在 3s 内返回")
	}
}

// TestWaitRespectsContext 断言 Wait 在 ctx 取消时返回错误而不是永久挂住。
func TestWaitRespectsContext(t *testing.T) {
	runner := newGateRunner("ok")
	svc := newTestSvc(t, runner)
	defer close(runner.release)
	rec, err := svc.Submit(TaskSpec{Team: "zeta", Objective: "ctx"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := svc.Wait(ctx, rec.ID); err == nil {
		t.Fatal("ctx 超时后 Wait 应返回错误")
	}
}

// TestPauseThenResume 断言: Pause 让任务落到 paused (不是 failed), Resume 后能跑到 completed,
// 且 Attempts 记录了两次启动。
func TestPauseThenResume(t *testing.T) {
	var release atomic.Pointer[chan struct{}]
	first := make(chan struct{})
	release.Store(&first)

	runner := TaskRunnerFunc(func(ctx context.Context, rec TaskRecord) (string, error) {
		ch := *release.Load()
		select {
		case <-ch:
			return "第二轮完成", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	svc := newTestSvc(t, runner)
	rec, err := svc.Submit(TaskSpec{Team: "eta", Objective: "暂停续跑"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitState(t, svc, rec.ID, TaskStateRunning)

	if err := svc.Pause(rec.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, svc, rec.ID, TaskStatePaused)

	second := make(chan struct{})
	close(second)
	release.Store(&second)
	if err := svc.Resume(rec.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	res, err := svc.Wait(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.State != TaskStateCompleted || res.Output != "第二轮完成" {
		t.Fatalf("续跑后应 completed 并带新输出, got state=%s out=%q", res.State, res.Output)
	}
	got, _ := svc.Get(rec.ID)
	if got.Attempts != 2 {
		t.Fatalf("Attempts 应为 2 (首跑+续跑), got %d", got.Attempts)
	}
}

// TestStopIsTerminal 断言 Stop 落到 stopped 这个终态 (Wait 会返回)。
func TestStopIsTerminal(t *testing.T) {
	runner := newGateRunner("ok")
	svc := newTestSvc(t, runner)
	defer close(runner.release)
	rec, err := svc.Submit(TaskSpec{Team: "theta", Objective: "停止"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitState(t, svc, rec.ID, TaskStateRunning)
	if err := svc.Stop(rec.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res, err := svc.Wait(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.State != TaskStateStopped {
		t.Fatalf("应为 stopped, got %s", res.State)
	}
}

// TestRefineRecordsAndReruns 断言精修留痕 + 把反馈交给运行器 + 重新跑到终态。
func TestRefineRecordsAndReruns(t *testing.T) {
	runner := newInstantRunner()
	svc := newTestSvc(t, runner)
	rec, err := svc.Submit(TaskSpec{Team: "iota", Workflow: "techblog", Objective: "初稿"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), rec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := svc.Refine(rec.ID, "补充基准数据", "article-writing"); err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if _, err := svc.Wait(context.Background(), rec.ID); err != nil {
		t.Fatalf("Wait 精修: %v", err)
	}
	got, _ := svc.Get(rec.ID)
	if len(got.Refines) != 1 || got.Refines[0].Feedback != "补充基准数据" || got.Refines[0].FromNode != "article-writing" {
		t.Fatalf("精修留痕不正确: %+v", got.Refines)
	}
	if got.Spec.Params["feedback"] != "补充基准数据" {
		t.Fatalf("反馈应写入 Spec.Params 供运行器消费, got %+v", got.Spec.Params)
	}
	if got.Attempts != 2 {
		t.Fatalf("精修后 Attempts 应为 2, got %d", got.Attempts)
	}
}

// TestNoRunnerStaysPending 断言未注入运行器时任务停在 pending (不假装 running/completed)。
func TestNoRunnerStaysPending(t *testing.T) {
	svc := NewFileQueueTaskService(TaskServiceOptions{Store: statestore.NewMemStore()})
	rec, err := svc.Submit(TaskSpec{Team: "kappa", Objective: "无运行器"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	got, _ := svc.Get(rec.ID)
	if got.State != TaskStatePending {
		t.Fatalf("无运行器应停在 pending, got %s", got.State)
	}
	if err := svc.Resume(rec.ID); err == nil {
		t.Fatal("无运行器时 Resume 应报错而不是静默成功")
	}
}

// TestRecordsSurviveNewInstance 断言档案经 statestore 持久化: 新建一个服务实例仍能 List 到。
func TestRecordsSurviveNewInstance(t *testing.T) {
	dir := t.TempDir()
	store := statestore.NewFileStore(dir)
	runner := newInstantRunner()
	svc := NewFileQueueTaskService(TaskServiceOptions{Store: store, Runner: runner, PollInterval: 10 * time.Millisecond})
	rec, err := svc.Submit(TaskSpec{Team: "lambda", Workflow: "techblog", Objective: "持久化"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), rec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	svc2 := NewFileQueueTaskService(TaskServiceOptions{Store: statestore.NewFileStore(dir), PollInterval: 10 * time.Millisecond})
	got, ok := svc2.Get(rec.ID)
	if !ok {
		t.Fatal("新实例应能读到已持久化的任务档案")
	}
	if got.State != TaskStateCompleted || got.Spec.Team != "lambda" {
		t.Fatalf("档案内容不对: %+v", got)
	}
	if n := len(svc2.List(TaskFilter{Team: "lambda"})); n != 1 {
		t.Fatalf("List 应看到 1 条, got %d", n)
	}
	if svc.WriteErrors() != 0 {
		t.Fatalf("不应有落盘失败, got %d", svc.WriteErrors())
	}
}

// TestFlattenBucketNoCollisionAndValid 断言 bucket 扁平化: ①消毒后仍是 statestore 合法名
// (读写真能往返, 而不是退化成 badBucketKV); ②"a/b" 与 "a-b" 不会被消毒成同一个桶。
func TestFlattenBucketNoCollisionAndValid(t *testing.T) {
	store := statestore.NewMemStore()
	b1 := flattenBucket("tasks", "team/a")
	b2 := flattenBucket("tasks", "team-a")
	if b1 == b2 {
		t.Fatalf("不同命名空间被消毒成同一个 bucket: %s", b1)
	}
	if err := store.KV(b1).Put("k", "v1"); err != nil {
		t.Fatalf("bucket %q 不合法: %v", b1, err)
	}
	if err := store.KV(b2).Put("k", "v2"); err != nil {
		t.Fatalf("bucket %q 不合法: %v", b2, err)
	}
	var got string
	if _, err := store.KV(b1).Get("k", &got); err != nil || got != "v1" {
		t.Fatalf("bucket 隔离失败: got=%q err=%v", got, err)
	}
	// 含中文 + 超长命名空间也必须落到合法桶名
	long := flattenBucket("tasks", "中文团队"+string(make([]byte, 0))+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := store.KV(long).Put("k", 1); err != nil {
		t.Fatalf("超长/中文命名空间 bucket %q 不合法: %v", long, err)
	}
}

// ---------------------------------------------------------------------------
// :7777 动作队列消费
// ---------------------------------------------------------------------------

// writePendingAction 按 dashboard 的写入格式造一条 pending 动作 (逐字对齐
// extra_handlers.go 的 rec 结构), 用于证明消费方读的是真实格式。
func writePendingAction(t *testing.T, dir, id, kind, action, target string, payload map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{
		"id": id, "kind": kind, "action": action, "target": target,
		"status": "pending", "requested": time.Now().Format(time.RFC3339),
		"source": "dashboard",
	}
	if payload != nil {
		rec["payload"] = payload
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	p := filepath.Join(dir, id+".json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readActionFile(t *testing.T, path string) ActionRecord {
	t.Helper()
	act, err := readAction(path)
	if err != nil {
		t.Fatalf("读动作文件 %s: %v", path, err)
	}
	return act
}

// TestConsumeActionTurnsIntoTask 这是本次最要紧的断言: 队列里的一条动作**真被消费** ——
// 状态从 pending 变成 accepted、回填了 taskId/consumedAt, 且对应任务真的跑到了终态。
// 历史上这个目录只写不读, 记录烂在盘上。
func TestConsumeActionTurnsIntoTask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".dashboard", "actions")
	runner := newInstantRunner()
	svc := NewFileQueueTaskService(TaskServiceOptions{
		Store: statestore.NewMemStore(), Runner: runner,
		ActionsDir: dir, PollInterval: 10 * time.Millisecond,
	})
	path := writePendingAction(t, dir, "team-run-demo-1", "team", "run", "demo", map[string]any{
		"workflow": "techblog", "objective": "写一篇关于向量库的文章", "lang": "go",
	})

	stats, err := svc.ConsumeActions(context.Background())
	if err != nil {
		t.Fatalf("ConsumeActions: %v", err)
	}
	if stats.Scanned != 1 || stats.Accepted != 1 {
		t.Fatalf("应扫描并接收 1 条动作, got %+v", stats)
	}

	act := readActionFile(t, path)
	if act.Status != ActionStatusAccepted {
		t.Fatalf("动作状态应为 accepted, got %q (error=%q)", act.Status, act.Error)
	}
	if act.TaskID == "" || act.ConsumedAt == "" {
		t.Fatalf("动作应回填 taskId/consumedAt (被消费的硬证据), got %+v", act)
	}

	res, err := svc.Wait(context.Background(), act.TaskID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.State != TaskStateCompleted {
		t.Fatalf("动作派生的任务应跑到 completed, got %s", res.State)
	}
	// 载荷被正确翻译成任务规格
	select {
	case spec := <-runner.specs:
		if spec.Team != "demo" || spec.Workflow != "techblog" || spec.Language != "go" ||
			spec.Objective != "写一篇关于向量库的文章" {
			t.Fatalf("动作载荷翻译成任务规格不正确: %+v", spec)
		}
	default:
		t.Fatal("运行器没有收到任务规格")
	}

	// 第二轮消费不应重复处理 (状态已非 pending)
	stats2, err := svc.ConsumeActions(context.Background())
	if err != nil {
		t.Fatalf("第二轮 ConsumeActions: %v", err)
	}
	if stats2.Scanned != 0 {
		t.Fatalf("已消费的动作不应被再次认领, got %+v", stats2)
	}
	if runner.calls.Load() != 1 {
		t.Fatalf("运行器应只被调 1 次, got %d", runner.calls.Load())
	}
	if _, err := os.Stat(path + inflightSuffix); !os.IsNotExist(err) {
		t.Fatal("认领标记应在回写后被清理")
	}
}

// TestConsumeActionUnsupportedWhenNoExecutor 断言没有注入执行器时, 非任务型动作被标为
// unsupported 并写明原因 —— 绝不静默标 done (那正是本次要消灭的假承诺)。
func TestConsumeActionUnsupportedWhenNoExecutor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	svc := NewFileQueueTaskService(TaskServiceOptions{Store: statestore.NewMemStore(), ActionsDir: dir})
	path := writePendingAction(t, dir, "cron-trigger-x-1", "cron", "trigger", "x", nil)

	stats, err := svc.ConsumeActions(context.Background())
	if err != nil {
		t.Fatalf("ConsumeActions: %v", err)
	}
	if stats.Unsupported != 1 {
		t.Fatalf("应记 1 条 unsupported, got %+v", stats)
	}
	act := readActionFile(t, path)
	if act.Status != ActionStatusUnsupported || act.Error == "" {
		t.Fatalf("应标 unsupported 并写明原因, got status=%q error=%q", act.Status, act.Error)
	}
}

// TestConsumeActionDelegatesToExecutor 断言注入执行器后, 非任务型动作真被执行并回写 done。
func TestConsumeActionDelegatesToExecutor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	var seen []string
	svc := NewFileQueueTaskService(TaskServiceOptions{
		Store: statestore.NewMemStore(), ActionsDir: dir,
		ActionExecutor: ActionExecutorFunc(func(_ context.Context, act ActionRecord) (string, error) {
			seen = append(seen, act.Kind+"."+act.Action+":"+act.Target)
			return "已触发", nil
		}),
	})
	path := writePendingAction(t, dir, "cron-trigger-y-1", "cron", "trigger", "y", nil)
	if _, err := svc.ConsumeActions(context.Background()); err != nil {
		t.Fatalf("ConsumeActions: %v", err)
	}
	act := readActionFile(t, path)
	if act.Status != ActionStatusDone || act.Result != "已触发" {
		t.Fatalf("应由执行器完成并回写 done, got %+v", act)
	}
	if len(seen) != 1 || seen[0] != "cron.trigger:y" {
		t.Fatalf("执行器应收到原始动作, got %v", seen)
	}
}

// TestConsumeActionStopInterruptsTask 断言 team.stop 动作真的作用到活跃任务上。
func TestConsumeActionStopInterruptsTask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	runner := newGateRunner("ok")
	svc := NewFileQueueTaskService(TaskServiceOptions{
		Store: statestore.NewMemStore(), Runner: runner,
		ActionsDir: dir, PollInterval: 10 * time.Millisecond,
	})
	defer close(runner.release)
	rec, err := svc.Submit(TaskSpec{Team: "mu", Workflow: "techblog", Objective: "长跑"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitState(t, svc, rec.ID, TaskStateRunning)

	path := writePendingAction(t, dir, "team-stop-mu-1", "team", "stop", "mu", nil)
	if _, err := svc.ConsumeActions(context.Background()); err != nil {
		t.Fatalf("ConsumeActions: %v", err)
	}
	act := readActionFile(t, path)
	if act.Status != ActionStatusDone || act.TaskID != rec.ID {
		t.Fatalf("team.stop 应停到活跃任务上, got %+v", act)
	}
	res, err := svc.Wait(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.State != TaskStateStopped {
		t.Fatalf("任务应为 stopped, got %s", res.State)
	}
}

// TestReclaimStaleClaim 断言认领后崩溃的动作会被回收重投, 不会永久卡死。
func TestReclaimStaleClaim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	svc := NewFileQueueTaskService(TaskServiceOptions{
		Store: statestore.NewMemStore(), Runner: newInstantRunner(),
		ActionsDir: dir, StaleClaim: time.Nanosecond, PollInterval: 10 * time.Millisecond,
	})
	// 模拟"已认领但未回写"的残骸
	path := writePendingAction(t, dir, "team-run-stale-1", "team", "run", "stale",
		map[string]any{"workflow": "techblog", "objective": "回收测试"})
	if err := os.Rename(path, path+inflightSuffix); err != nil {
		t.Fatal(err)
	}
	stats, err := svc.ConsumeActions(context.Background())
	if err != nil {
		t.Fatalf("ConsumeActions: %v", err)
	}
	if stats.Reclaimed != 1 {
		t.Fatalf("应回收 1 条过期认领, got %+v", stats)
	}
	if stats.Accepted != 1 {
		t.Fatalf("回收后应被正常消费, got %+v", stats)
	}
}

// TestListActionsReadsQueue 断言 ListActions 能把裸目录读成结构化列表 (原先无任何读侧)。
func TestListActionsReadsQueue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	svc := NewFileQueueTaskService(TaskServiceOptions{Store: statestore.NewMemStore(), ActionsDir: dir})
	writePendingAction(t, dir, "a-1", "team", "run", "t1", nil)
	writePendingAction(t, dir, "a-2", "cron", "trigger", "t2", nil)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListActions()
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("坏记录应被跳过而不影响其余, got %d 条: %+v", len(list), list)
	}
}

// ---------------------------------------------------------------------------
// TeamRunner: 真接到 RunTeam 上 (stub agent, 无 LLM)
// ---------------------------------------------------------------------------

// TestTeamRunnerRunsRealTeam 端到端: 动作队列 → TaskService → TeamRunner → RunTeam →
// 团队真跑完 → 任务 completed。用 stub agent 运行时, 不打 LLM。
func TestTeamRunnerRunsRealTeam(t *testing.T) {
	tmp := t.TempDir()
	wfName := "tsq-echo-" + fmt.Sprint(time.Now().UnixNano())
	if err := RegisterWorkflow(&WorkflowDef{
		Name: wfName, Mode: "pipeline", QualityGate: "none",
		Stages: []StageDef{{
			Name: "draft", Role: "tsq-writer",
			Prompt: "你是一个撰稿人, 根据目标写一段内容。目标: {objective}",
		}},
	}, nil); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	t.Cleanup(func() { _ = UnregisterWorkflow(wfName) })

	rec := &stubRec{}
	mgr := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(tmp, "teams"),
		Factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &stubRunner{role: role, rec: rec}, nil
		},
		Notify: func(_, _ string) {},
	})

	dir := filepath.Join(tmp, ".dashboard", "actions")
	svc := NewFileQueueTaskService(TaskServiceOptions{
		Store:  statestore.NewFileStore(filepath.Join(tmp, "state")),
		Runner: NewTeamRunner(mgr), ActionsDir: dir,
		PollInterval: 20 * time.Millisecond,
	})
	path := writePendingAction(t, dir, "team-run-real-1", "team", "run", "realteam",
		map[string]any{"workflow": wfName, "objective": "介绍一下向量检索"})

	if _, err := svc.ConsumeActions(context.Background()); err != nil {
		t.Fatalf("ConsumeActions: %v", err)
	}
	act := readActionFile(t, path)
	if act.TaskID == "" {
		t.Fatalf("动作未被转成任务: %+v", act)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := svc.Wait(ctx, act.TaskID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.State != TaskStateCompleted {
		t.Fatalf("真团队应跑到 completed, got %s (err=%s output=%s)", res.State, res.Error, res.Output)
	}
	team := mgr.GetTeam("realteam")
	if team == nil {
		t.Fatal("TeamRunner 应已创建团队")
	}
	if st := teamStatusOf(team); !isSuccessfulTeamStatus(st) {
		t.Fatalf("团队终态应成功, got %s", st)
	}
	roles, _ := rec.snap()
	if len(roles) == 0 {
		t.Fatal("stub agent 未被调用, 说明团队没真跑")
	}
}

// TestTeamRunnerRejectsMissingWorkflow 断言团队不存在且没给 workflow 时报错而不是空跑。
func TestTeamRunnerRejectsMissingWorkflow(t *testing.T) {
	tmp := t.TempDir()
	mgr := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(tmp, "teams"),
		Factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &stubRunner{role: role, rec: &stubRec{}}, nil
		},
		Notify: func(_, _ string) {},
	})
	if _, err := NewTeamRunner(mgr).Run(context.Background(), TaskRecord{
		Spec: TaskSpec{Team: "nope", Objective: "x"},
	}); err == nil {
		t.Fatal("团队不存在且无 workflow 时应报错")
	}
}

// waitState 轮询等任务进入指定状态。
func waitState(t *testing.T, svc *FileQueueTaskService, id string, want TaskState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := svc.Get(id); ok && rec.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec, _ := svc.Get(id)
	t.Fatalf("任务 %s 未在 3s 内进入 %s, 当前 %s", id, want, rec.State)
}
