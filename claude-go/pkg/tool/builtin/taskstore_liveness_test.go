package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSweepTestStore 建一个空台账 (路径在 t.TempDir 下, 不会读到真实数据)。
func newSweepTestStore(t *testing.T) *TaskStore {
	t.Helper()
	return NewTaskStore(filepath.Join(t.TempDir(), "tasks.json"))
}

// countBlockedWithFailedUpstream 数"永远解不开"的条目: 状态是 blocked, 但依赖里
// 至少有一条已经 failed。这正是 21.4.6 记的 245 条僵尸的形态。
func countBlockedWithFailedUpstream(t *testing.T, s *TaskStore) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rec := range s.byID {
		if rec.Status != "blocked" {
			continue
		}
		for _, dep := range rec.DependsOn {
			if d, ok := s.byID[dep]; ok && d.Status == "failed" {
				n++
				break
			}
		}
	}
	return n
}

func countByStatus(t *testing.T, s *TaskStore, status string) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rec := range s.byID {
		if rec.Status == status {
			n++
		}
	}
	return n
}

// ageTask 把一条记录的时间戳往前拨, 模拟"已经躺了很久"。
func ageTask(t *testing.T, s *TaskStore, id string, d time.Duration) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		t.Fatalf("ageTask: 任务 %s 不存在", id)
	}
	old := time.Now().UTC().Add(-d).Format(time.RFC3339Nano)
	rec.UpdatedAt = old
	rec.CreatedAt = old
	s.byID[id] = rec
}

// TestH12_SetTaskStatusUnblocksDependents 钉住**生产真实走的那条路**。
//
// pkg/agent/workflow.go:922 / swarm.go:762 / orchestrator.go:7321 全部用
// SetTaskStatus(id,"failed") 记失败, 而它此前不做任何下游联动 —— 这就是 245 条
// 僵尸的成因。改造前这条断言 1 (不是 0)。
func TestH12_SetTaskStatusUnblocksDependents(t *testing.T) {
	s := newSweepTestStore(t)

	up, err := s.AddTask("上游", "", "")
	if err != nil {
		t.Fatal(err)
	}
	down, err := s.AddTaskWithDeps("下游", "", "", []string{up}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := countByStatus(t, s, "blocked"); got != 1 {
		t.Fatalf("前置条件不成立: 建带依赖的任务后应有 1 条 blocked, 实际 %d", got)
	}

	if err := s.SetTaskStatus(up, "failed"); err != nil {
		t.Fatal(err)
	}

	if n := countBlockedWithFailedUpstream(t, s); n != 0 {
		t.Fatalf("上游 failed 后仍有 %d 条 blocked 卡在上游失败上 —— 这条永远不会再被解锁", n)
	}

	// 对照臂: 下游必须**还在**且变成 pending。少了这一臂, "0 条卡死"会因为在
	// 上面被误删/误判而假绿。
	s.mu.Lock()
	rec, ok := s.byID[down]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("下游任务 %s 从台账里消失了 —— 判据不能靠删数据达成", down)
	}
	if rec.Status != "pending" {
		t.Fatalf("下游任务状态 = %q, 期望 pending (解锁, 不是级联失败)", rec.Status)
	}
}

// writeLegacyLedger 直接落一份**改造前那版代码会写出**的台账: 上游是 failed,
// 下游还停在 blocked。改造后已经无法通过 API 造出这个形态 (SetTaskStatus 会顺带
// 解锁), 所以必须从盘上灌进来 —— 真实台账里那 245 条正是老代码写下的存量。
func writeLegacyLedger(t *testing.T, path string, pairs int) {
	t.Helper()
	old := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	var b strings.Builder
	b.WriteString(`{"tasks":{`)
	for i := 0; i < pairs; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		up := fmt.Sprintf("up-%d", i)
		down := fmt.Sprintf("down-%d", i)
		fmt.Fprintf(&b, `%q:{"id":%q,"subject":"up","status":"failed","createdAt":%q,"updatedAt":%q}`, up, up, old, old)
		b.WriteString(",")
		fmt.Fprintf(&b, `%q:{"id":%q,"subject":"down","status":"blocked","dependsOn":[%q],"createdAt":%q,"updatedAt":%q}`,
			down, down, up, old, old)
	}
	b.WriteString(`}}`)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestH12_SweepClearsBacklog 钉住 H1-2 的验收判据本身:
// "台账里 blocked 且上游 failed 的条目数 = 0"。
//
// 判据的输入是**盘上的存量**, 不是当场造出来的条目 —— 这正是台账里 245 条的来源。
func TestH12_SweepClearsBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	const pairs = 12
	writeLegacyLedger(t, path, pairs)

	s := NewTaskStore(path)
	before := countBlockedWithFailedUpstream(t, s)
	if before != pairs {
		t.Fatalf("前置条件不成立: 台账里应有 %d 条卡死条目, 实际读到 %d", pairs, before)
	}

	res := s.SweepStale(time.Now().UTC(), 0, 0) // TTL=0: 只解锁, 不判死
	after := countBlockedWithFailedUpstream(t, s)
	if after != 0 {
		t.Fatalf("对账后仍有 %d 条 blocked 卡在上游失败上 (对账前 %d)", after, before)
	}

	// 对照臂 1: 解锁而不是丢弃 —— 24 条记录必须都还在。
	s.mu.Lock()
	total := len(s.byID)
	s.mu.Unlock()
	if total != pairs*2 {
		t.Fatalf("对账后台账剩 %d 条, 期望 %d 条 (解锁不等于删除)", total, pairs*2)
	}
	// 对照臂 2: 解锁条数必须报数, 否则"0 条卡死"可能来自"什么都没做"以外的原因。
	if res.Unblocked != pairs {
		t.Fatalf("对账报告解锁 %d 条, 期望 %d 条", res.Unblocked, pairs)
	}
}

// TestH12_FreshPendingSurvives 对照臂: 对账**不能**把刚提交的 pending 判死,
// 否则"pending 超过 T 的条目数 = 0"是靠杀光 pending 达成的。
func TestH12_FreshPendingSurvives(t *testing.T) {
	s := newSweepTestStore(t)
	id, err := s.AddTask("刚提交的任务", "", "")
	if err != nil {
		t.Fatal(err)
	}

	res := s.SweepStale(time.Now().UTC(), DefaultPendingTTL, DefaultBlockedTTL)

	s.mu.Lock()
	rec, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		t.Fatal("刚提交的任务从台账里消失了")
	}
	if rec.Status != "pending" {
		t.Fatalf("刚提交 %s 的任务被判成 %q —— 对账在杀健康条目", DefaultPendingTTL, rec.Status)
	}
	if res.ExpiredPending != 0 {
		t.Fatalf("对账误杀了 %d 条新鲜 pending", res.ExpiredPending)
	}
}

// TestH12_StalePendingExpires 被测臂: 躺过 TTL 的 pending 必须进终态。
func TestH12_StalePendingExpires(t *testing.T) {
	s := newSweepTestStore(t)
	id, err := s.AddTask("没人接手的任务", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ageTask(t, s, id, 2*DefaultPendingTTL)

	res := s.SweepStale(time.Now().UTC(), DefaultPendingTTL, DefaultBlockedTTL)

	s.mu.Lock()
	rec, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		t.Fatal("超时任务从台账里消失了 —— 终态该是 failed 而不是删除")
	}
	if !isTerminalTaskStatus(rec.Status) {
		t.Fatalf("躺了 %s 的 pending 状态仍是 %q, 期望终态", 2*DefaultPendingTTL, rec.Status)
	}
	if res.ExpiredPending != 1 {
		t.Fatalf("过期 pending 计数 = %d, 期望 1", res.ExpiredPending)
	}
}

// TestH12_DanglingDepExpires 依赖 ID 在台账里根本不存在 —— 没有任何事件能解锁它。
func TestH12_DanglingDepExpires(t *testing.T) {
	s := newSweepTestStore(t)
	id, err := s.AddTaskWithDeps("依赖了幽灵任务", "", "", []string{"does-not-exist"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	res := s.SweepStale(time.Now().UTC(), DefaultPendingTTL, DefaultBlockedTTL)

	s.mu.Lock()
	rec, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		t.Fatal("任务消失了")
	}
	if rec.Status != "failed" {
		t.Fatalf("依赖悬空的任务状态 = %q, 期望 failed", rec.Status)
	}
	if res.DanglingDeps != 1 {
		t.Fatalf("悬空依赖计数 = %d, 期望 1", res.DanglingDeps)
	}
}

// TestH12_UnblockedIsNotImmediatelyExpired 钉住对账内部的**顺序**性质:
// 被解锁的条目在同一轮里不能被当成超时条目再判死。否则"解锁"实际等于"判死",
// 而上游失败的 245 条会从 blocked 直接变 failed —— 那不是 21.5.3 要的语义。
func TestH12_UnblockedIsNotImmediatelyExpired(t *testing.T) {
	s := newSweepTestStore(t)
	up, err := s.AddTask("上游", "", "")
	if err != nil {
		t.Fatal(err)
	}
	down, err := s.AddTaskWithDeps("下游", "", "", []string{up}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 两条都躺了很久 (远超 blocked TTL), 上游随即失败。
	ageTask(t, s, up, 10*DefaultBlockedTTL)
	ageTask(t, s, down, 10*DefaultBlockedTTL)
	if err := s.SetTaskStatus(up, "failed"); err != nil {
		t.Fatal(err)
	}

	s.SweepStale(time.Now().UTC(), DefaultPendingTTL, DefaultBlockedTTL)

	s.mu.Lock()
	rec := s.byID[down]
	s.mu.Unlock()
	if rec.Status != "pending" {
		t.Fatalf("被解锁的下游在同一次对账里又被判成 %q —— 解锁被超时判定覆盖了", rec.Status)
	}
}

// TestH12_NoDepsBlockedIsAbsorbing 无依赖却停在 blocked: 没有任何东西能解锁它。
// AddTaskFull 的循环检测会把造成环的依赖摘掉, 于是可能留下 blocked 且 DependsOn 为空。
func TestH12_NoDepsBlockedIsAbsorbing(t *testing.T) {
	s := newSweepTestStore(t)
	id, err := s.AddTaskWithDeps("无依赖的 blocked", "", "", []string{"x"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 手工把依赖摘空, 模拟 filterCycleCreatingDeps 摘掉环边之后的样子。
	s.mu.Lock()
	rec := s.byID[id]
	rec.DependsOn = nil
	s.byID[id] = rec
	s.mu.Unlock()

	s.SweepStale(time.Now().UTC(), DefaultPendingTTL, DefaultBlockedTTL)

	s.mu.Lock()
	rec = s.byID[id]
	s.mu.Unlock()
	if rec.Status == "blocked" {
		t.Fatal("无依赖的 blocked 条目没有被处理 —— 它是个吸收态")
	}
}

func isTerminalTaskStatus(s string) bool {
	return s == "completed" || s == "failed"
}
