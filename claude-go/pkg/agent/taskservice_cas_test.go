package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// F12 goal 事件溯源 CAS: 目标状态机以事件溯源存储, 更新带乐观并发控制 (冲突即拒绝)。

// newCASTestSvc 无 runner 的服务实例 (状态机测试不需要真执行)。
func newCASTestSvc(t *testing.T) *FileQueueTaskService {
	t.Helper()
	return NewFileQueueTaskService(TaskServiceOptions{
		Store: statestore.NewMemStore(), PollInterval: 10 * time.Millisecond,
	})
}

// readJournalRaw 直接读流 (不过滤任务), 供完整性断言。
func readJournalRaw(t *testing.T, svc *FileQueueTaskService) []TaskJournalEvent {
	t.Helper()
	evs, err := svc.ReadJournal("")
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	return evs
}

// TestTransitionCASRejectsStaleRevision 断言: 期望修订号与档案不符时更新被拒绝
// (errors.Is ErrStateConflict), 档案不被覆盖, 且冲突留痕进事件流。
func TestTransitionCASRejectsStaleRevision(t *testing.T) {
	svc := newCASTestSvc(t)
	rec, err := svc.Submit(TaskSpec{Team: "cas-1", Objective: "目标 A"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	base, _ := svc.Get(rec.ID)
	if base.Revision <= 0 {
		t.Fatalf("建档后 Revision 应已 ≥1 (KV 投影初值), got %d", base.Revision)
	}

	// 并发方先改了一步: 走 CAS 正常更新, Revision +1
	if _, err := svc.TransitionCAS(rec.ID, base.Revision, func(r *TaskRecord) error {
		r.Meta = map[string]string{"note": "第一次更新"}
		return nil
	}); err != nil {
		t.Fatalf("第一次 CAS: %v", err)
	}

	// 持旧 Revision 的更新者被拒绝
	cur, _ := svc.Get(rec.ID)
	_, err = svc.TransitionCAS(rec.ID, base.Revision, func(r *TaskRecord) error {
		r.Meta = map[string]string{"note": "过期写"}
		return nil
	})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("过期 Revision 应返回 ErrStateConflict, got %v", err)
	}
	after, _ := svc.Get(rec.ID)
	if after.Revision != cur.Revision || after.Meta["note"] != "第一次更新" {
		t.Fatalf("被拒的更新不得覆盖档案: rev=%d meta=%+v", after.Revision, after.Meta)
	}

	// 冲突留痕: 事件流里应有 conflict 事件
	evs := readJournalRaw(t, svc)
	found := false
	for _, ev := range evs {
		if ev.Conflict && ev.Task == rec.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("冲突应留 conflict 事件: %+v", evs)
	}
}

// TestTransitionCASConcurrentExactlyOneWins 断言: 两个 goroutine 携同一期望修订号
// 并发 CAS, 恰好一个成功一个被拒 (冲突即拒绝, 而非最后写者静默覆盖)。
func TestTransitionCASConcurrentExactlyOneWins(t *testing.T) {
	svc := newCASTestSvc(t)
	rec, err := svc.Submit(TaskSpec{Team: "cas-2", Objective: "并发目标"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	base, _ := svc.Get(rec.ID)

	var wins, losses atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.TransitionCAS(rec.ID, base.Revision, func(r *TaskRecord) error {
				r.Meta = map[string]string{"winner": string(rune('A' + i))}
				return nil
			})
			if err == nil {
				wins.Add(1)
			} else if errors.Is(err, ErrStateConflict) {
				losses.Add(1)
			} else {
				t.Errorf("意外的错误类型: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 || losses.Load() != 1 {
		t.Fatalf("并发 CAS 应恰好 1 胜 1 负, got wins=%d losses=%d", wins.Load(), losses.Load())
	}
	after, _ := svc.Get(rec.ID)
	if after.Revision != base.Revision+1 {
		t.Fatalf("Revision 只应前进 1 步, got %d → %d", base.Revision, after.Revision)
	}
}

// TestTransitionCASStateTransitionSemantics 断言: mutate 里改 State 即一次真实
// 状态迁移 —— 字段不变式 (FinishedAt/Attempts/Error) 与内部 transition 同构,
// 终态迁移释放活跃索引, Wait 能被唤醒。
func TestTransitionCASStateTransitionSemantics(t *testing.T) {
	svc := newCASTestSvc(t)
	rec, err := svc.Submit(TaskSpec{Team: "cas-3", Objective: "状态迁移"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	base, _ := svc.Get(rec.ID)

	out, err := svc.TransitionCAS(rec.ID, base.Revision, func(r *TaskRecord) error {
		r.State = TaskStateStopped
		r.Error = "外部终止"
		return nil
	})
	if err != nil {
		t.Fatalf("CAS 迁移: %v", err)
	}
	if out.State != TaskStateStopped || out.Error != "外部终止" {
		t.Fatalf("迁移语义失真: %+v", out)
	}
	if out.FinishedAt.IsZero() {
		t.Fatal("终态迁移应盖 FinishedAt")
	}
	if out.Revision != base.Revision+1 {
		t.Fatalf("迁移后 Revision 应 +1, got %d", out.Revision)
	}
	// 终态 → 活跃索引释放: 再提交同目标应是新任务
	again, err := svc.Submit(rec.Spec, SubmitOpts{})
	if err != nil {
		t.Fatalf("终态后再提交: %v", err)
	}
	if again.ID == rec.ID {
		t.Fatal("终态后活跃索引未释放, 同目标提交仍归并旧任务")
	}
	// Wait 立即返回 (被 CAS 迁移唤醒)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := svc.Wait(ctx, rec.ID)
	if err != nil || res.State != TaskStateStopped {
		t.Fatalf("Wait 应立即看到 stopped, got %v/%v", res, err)
	}
}

// TestJournalChainContinuousAndReplayable 断言事件流真源性质: 全部事件 Seq 连续
// (1..N 重放), 成功迁移事件带 Revision, 链条 from/to 首尾相接, 冲突事件不破坏链条。
func TestJournalChainContinuousAndReplayable(t *testing.T) {
	svc := newCASTestSvc(t)
	rec, err := svc.Submit(TaskSpec{Team: "cas-4", Objective: "事件溯源"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	cur, _ := svc.Get(rec.ID)

	// 一串 CAS: 成功 → 冲突 → 成功
	if _, err := svc.TransitionCAS(rec.ID, cur.Revision, func(r *TaskRecord) error {
		r.Meta = map[string]string{"n": "1"}
		return nil
	}); err != nil {
		t.Fatalf("CAS1: %v", err)
	}
	_, _ = svc.TransitionCAS(rec.ID, cur.Revision, func(r *TaskRecord) error { return nil }) // 过期 → conflict
	cur, _ = svc.Get(rec.ID)
	if _, err := svc.TransitionCAS(rec.ID, cur.Revision, func(r *TaskRecord) error {
		r.State = TaskStateFailed
		r.Error = "终态"
		return nil
	}); err != nil {
		t.Fatalf("CAS2: %v", err)
	}

	evs, err := svc.ReadJournal(rec.ID)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(evs) < 4 {
		t.Fatalf("事件流应 ≥4 条 (submit/cas/conflict/cas), got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("Seq 应连续 1..N, 第 %d 条 seq=%d", i, ev.Seq)
		}
		if ev.Task != rec.ID {
			t.Fatalf("事件 %d 属于别的任务: %+v", i, ev)
		}
	}
	// 链条首尾相接: 非首事件的 From == 前一事件的 To (submit 的 From 为空)
	for i := 1; i < len(evs); i++ {
		prev, e := evs[i-1], evs[i]
		if e.From != prev.To {
			t.Fatalf("事件链断裂: #%d to=%q, #%d from=%q", i-1, prev.To, i, e.From)
		}
	}
	// 成功迁移事件带 Revision (与档案演进一致), 冲突事件不落 Revision 之外的字段变更
	var conflicts, nonConflicts int
	for _, ev := range evs {
		if ev.Conflict {
			conflicts++
		} else {
			nonConflicts++
			if ev.Revision == 0 {
				t.Fatalf("非冲突事件应带 Revision: %+v", ev)
			}
		}
	}
	if conflicts != 1 || nonConflicts < 3 {
		t.Fatalf("应恰 1 条冲突事件且 ≥3 条成功事件, got %d/%d", conflicts, nonConflicts)
	}
}

// TestCASPerRecordLockAllowsParallelDifferentTasks 断言 per-record 锁不串行化
// 不同任务的 CAS (锁粒度是档案, 不是整个服务)。
func TestCASPerRecordLockAllowsParallelDifferentTasks(t *testing.T) {
	svc := newCASTestSvc(t)
	const n = 6
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		rec, err := svc.Submit(TaskSpec{Team: "cas-5", Objective: "任务" + string(rune('a'+i))}, SubmitOpts{})
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		ids[i] = rec.ID
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			rec, ok := svc.Get(id)
			if !ok {
				t.Errorf("任务 %s 消失", id)
				return
			}
			if _, err := svc.TransitionCAS(id, rec.Revision, func(r *TaskRecord) error {
				r.Meta = map[string]string{"done": "y"}
				return nil
			}); err != nil {
				t.Errorf("CAS %s: %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	for _, id := range ids {
		got, _ := svc.Get(id)
		if got.Meta["done"] != "y" {
			t.Fatalf("任务 %s 的 CAS 未生效: %+v", id, got)
		}
	}
}

// TestRefineIsCASGuarded 断言 Refine 的留痕走乐观并发: 并发 Refine 恰好一个落上,
// 另一个被拒 (反馈不是静默丢失, 而是显式报错)。
func TestRefineIsCASGuarded(t *testing.T) {
	runner := newInstantRunner()
	svc := newTestSvc(t, runner)
	rec, err := svc.Submit(TaskSpec{Team: "cas-6", Workflow: "techblog", Objective: "初稿"}, SubmitOpts{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), rec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = svc.Refine(rec.ID, "反馈"+string(rune('A'+i)), "")
		}(i)
	}
	close(start)
	wg.Wait()

	var okCount int
	for i, err := range errs {
		if err == nil {
			okCount++
			continue
		}
		if !errors.Is(err, ErrStateConflict) {
			t.Errorf("Refine %d 的失败应是冲突类: %v", i, err)
		}
	}
	if okCount != 1 {
		t.Fatalf("并发精修应恰好 1 个成功, got %d (errs=%v)", okCount, errs)
	}
	got, _ := svc.Get(rec.ID)
	if len(got.Refines) != 1 {
		t.Fatalf("留痕应恰好 1 条, got %+v", got.Refines)
	}
}
