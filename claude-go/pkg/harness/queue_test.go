package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// fakeClock 是注入式时钟：租约/退避测试靠推进它而不是 sleep。
// 多秒 sleep 既慢，又会把"租约过期"和"测试机器负载"绑在一起。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestQueue 返回一个基于 MemStore 的队列与可控时钟。
// 退避基数压到 1ms，让"Nack → 可再领取"只需推进几十毫秒。
func newTestQueue(t *testing.T) (*StoreQueue, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	q := NewStoreQueue(statestore.NewMemStore(), QueueOptions{
		Now:          clk.Now,
		DefaultLease: time.Minute,
		BackoffBase:  1 * time.Millisecond,
		BackoffMax:   8 * time.Millisecond,
	})
	if q == nil {
		t.Fatal("NewStoreQueue 返回 nil")
	}
	return q, clk
}

func mustEnqueue(t *testing.T, q *StoreQueue, task Task) TaskID {
	t.Helper()
	id, err := q.Enqueue(context.Background(), task)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("Enqueue 返回空 TaskID")
	}
	return id
}

// ---------------------------------------------------------------------------
// 1. 至少一次投递：Attempts 在派发时自增
// ---------------------------------------------------------------------------

func TestQueueAttemptsIncrementAtDispatch(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	payload := json.RawMessage(`{"n":1}`)
	id := mustEnqueue(t, q, Task{Kind: "noop", Payload: payload, MaxAttempts: 5})

	// 第一轮：claim 后立刻可见 Attempts=1 —— 还没有完成任何工作。
	c1, err := q.Claim(ctx, "worker-a", nil, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if c1 == nil {
		t.Fatal("Claim 应返回任务")
	}
	if c1.Attempts != 1 {
		t.Fatalf("派发后 Attempts 应为 1，实际 %d（完成时才自增会让崩溃 worker 永远停在 0）", c1.Attempts)
	}
	if c1.Worker != "worker-a" || c1.ID != string(id) {
		t.Fatalf("claimed 视图字段错误: %+v", c1)
	}
	if string(c1.Payload) != string(payload) {
		t.Fatalf("Payload 往返后不一致: %s", c1.Payload)
	}

	// 第二轮：Nack 回队再 claim，Attempts=2。
	if err := q.Nack(ctx, id, errors.New("boom")); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	clk.Advance(5 * time.Millisecond) // 越过 1ms 退避窗口
	c2, err := q.Claim(ctx, "worker-a", nil, time.Minute)
	if err != nil || c2 == nil {
		t.Fatalf("第二次 Claim: task=%v err=%v", c2, err)
	}
	if c2.Attempts != 2 {
		t.Fatalf("第二次派发 Attempts 应为 2，实际 %d", c2.Attempts)
	}
	if c2.ID != string(id) {
		t.Fatalf("应重新派发同一个任务，得到 %s", c2.ID)
	}
}

// ---------------------------------------------------------------------------
// 2. 租约过期重派：reaper 必须在 scan 之前
// ---------------------------------------------------------------------------

func TestQueueLeaseExpiryRedispatched(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 5})

	c1, err := q.Claim(ctx, "worker-crashed", nil, time.Minute)
	if err != nil || c1 == nil {
		t.Fatalf("首次 Claim: task=%v err=%v", c1, err)
	}
	if c1.Attempts != 1 {
		t.Fatalf("Attempts=%d", c1.Attempts)
	}

	// 租约未过期时，别的 worker 拿不到它。
	if got, err := q.Claim(ctx, "worker-b", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("租约内不应被他人领取: task=%v err=%v", got, err)
	}

	// worker-crashed 消失，时钟越过租约。若 scan 先于 reap，这条记录仍标
	// leased 会被跳过，队列从此永远显示"有任务但无人可领"。
	clk.Advance(2 * time.Minute)
	c2, err := q.Claim(ctx, "worker-b", nil, time.Minute)
	if err != nil {
		t.Fatalf("过期后 Claim: %v", err)
	}
	if c2 == nil {
		t.Fatal("租约过期的任务必须重新可领取（reap 必须先于 scan）")
	}
	if c2.ID != string(id) {
		t.Fatalf("应重派 %s，实际 %s", id, c2.ID)
	}
	if c2.Attempts != 2 {
		t.Fatalf("重派后 Attempts 应为 2，实际 %d", c2.Attempts)
	}
	if c2.Worker != "worker-b" {
		t.Fatalf("重派后持有者应为 worker-b，实际 %q", c2.Worker)
	}
	if !c2.LeaseUntil.After(clk.Now()) {
		t.Fatalf("重派后必须持有新租约: until=%v now=%v", c2.LeaseUntil, clk.Now())
	}
}

// 租约过期且已达上限：任务进死信，Claim 不再返回它。
func TestQueueLeaseExpiryAtMaxAttemptsGoesDead(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 1})
	if _, err := q.Claim(ctx, "w", nil, time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	clk.Advance(2 * time.Minute)

	got, err := q.Claim(ctx, "w2", nil, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got != nil {
		t.Fatalf("已达上限的过期任务不应再被派发: %+v", got)
	}
	dead, err := q.DeadLetters(ctx)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(dead) != 1 || dead[0] != id {
		t.Fatalf("应进死信 %s，实际 %v", id, dead)
	}
}

// ---------------------------------------------------------------------------
// 3. Renew 必须能失败
// ---------------------------------------------------------------------------

func TestQueueRenewFailsWhenLeaseExpired(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 5})
	if _, err := q.Claim(ctx, "worker-a", nil, time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// 租约有效期内续租成功，且截止时间前移。
	if err := q.Renew(ctx, id, time.Minute); err != nil {
		t.Fatalf("有效租约内 Renew 应成功: %v", err)
	}

	clk.Advance(2 * time.Minute)
	if err := q.Renew(ctx, id, time.Minute); err == nil {
		t.Fatal("租约过期后 Renew 必须报错（否则与 reaper 赛跑，结果取决于调度顺序）")
	}
}

func TestQueueRenewForDetectsReclaimByAnotherWorker(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 5})
	if _, err := q.ClaimFor(ctx, "worker-a", nil, []string{"bash"}, time.Minute); err != nil {
		t.Fatalf("ClaimFor A: %v", err)
	}

	// A 挂起，租约过期，B 回收并重新领取。此时任务仍是 leased 且租约新鲜，
	// 不带归属校验的 Renew 无法分辨 —— RenewFor 用 worker 身份拒绝旧持有者。
	clk.Advance(2 * time.Minute)
	cB, err := q.ClaimFor(ctx, "worker-b", nil, []string{"bash"}, time.Minute)
	if err != nil || cB == nil {
		t.Fatalf("B 应领取成功: task=%v err=%v", cB, err)
	}

	if err := q.RenewFor(ctx, id, "worker-a", time.Minute); err == nil {
		t.Fatal("旧持有者 RenewFor 必须失败：两个 worker 同时自认持有 = 静默重复执行")
	}
	if err := q.RenewFor(ctx, id, "worker-b", time.Minute); err != nil {
		t.Fatalf("当前持有者 RenewFor 应成功: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. Nack：指数退避 + 死信
// ---------------------------------------------------------------------------

func TestQueueNackBackoffAndDeadLetter(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 3})
	var delays []time.Duration

	for attempt := 1; attempt <= 3; attempt++ {
		c, err := q.Claim(ctx, "w", nil, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次 Claim: %v", attempt, err)
		}
		if c == nil {
			t.Fatalf("第 %d 次 Claim 应拿到任务", attempt)
		}
		if c.Attempts != attempt {
			t.Fatalf("第 %d 次派发的 Attempts=%d", attempt, c.Attempts)
		}
		before := clk.Now()
		if err := q.Nack(ctx, id, fmt.Errorf("fail-%d", attempt)); err != nil {
			t.Fatalf("第 %d 次 Nack: %v", attempt, err)
		}
		if attempt < 3 {
			// Nack 后立刻不可领取（退避窗口内）。
			if got, err := q.Claim(ctx, "w", nil, time.Minute); err != nil || got != nil {
				t.Fatalf("退避窗口内不应可领取: task=%v err=%v", got, err)
			}
			// 找到它的可见时间需要读内部状态；用 KV 直接读。
			var rec storeTask
			ok, err := q.kv.Get(liveKey(string(id)), &rec)
			if err != nil || !ok {
				t.Fatalf("读任务记录: ok=%v err=%v", ok, err)
			}
			delays = append(delays, rec.VisibleAt.Sub(before))
			clk.Advance(rec.VisibleAt.Sub(clk.Now()) + time.Millisecond)
		} else {
			// 第三次失败：达上限 → 死信。
			dead, err := q.DeadLetters(ctx)
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(dead) != 1 || dead[0] != id {
				t.Fatalf("应进死信 %s，实际 %v", id, dead)
			}
		}
	}

	if len(delays) != 2 {
		t.Fatalf("应采集到两次退避，实际 %v", delays)
	}
	if delays[1] <= delays[0] {
		t.Fatalf("退避必须递增: %v", delays)
	}

	// 死信任务永不回到 Claim。
	clk.Advance(time.Hour)
	if got, err := q.Claim(ctx, "w", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("死信任务不应再被 Claim 返回: task=%v err=%v", got, err)
	}
}

// MaxAttempts=0 时使用队列默认 3。
func TestQueueDefaultMaxAttempts(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()
	id := mustEnqueue(t, q, Task{Kind: "noop"}) // 不设 MaxAttempts

	for i := 1; i <= 3; i++ {
		if _, err := q.Claim(ctx, "w", nil, time.Minute); err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if err := q.Nack(ctx, id, nil); err != nil {
			t.Fatalf("Nack %d: %v", i, err)
		}
		clk.Advance(time.Second)
	}
	dead, err := q.DeadLetters(ctx)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("默认上限 3 应在第三次失败后进死信，实际死信=%v", dead)
	}
}

// ---------------------------------------------------------------------------
// 5. Ack 幂等
// ---------------------------------------------------------------------------

func TestQueueAckIdempotent(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop"})
	if _, err := q.Claim(ctx, "w", nil, time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.Ack(ctx, id); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := q.Ack(ctx, id); err != nil {
		t.Fatalf("重复 Ack 必须幂等返回 nil，实际 %v", err)
	}
	if got, err := q.Claim(ctx, "w", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("已完成任务不应再被领取: task=%v err=%v", got, err)
	}
	// 已完成任务不在死信。
	if dead, err := q.DeadLetters(ctx); err != nil || len(dead) != 0 {
		t.Fatalf("DeadLetters=%v err=%v", dead, err)
	}
}

// ---------------------------------------------------------------------------
// 6. DeadLetters 排序
// ---------------------------------------------------------------------------

func TestQueueDeadLettersSorted(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	for _, id := range []string{"z-task", "a-task", "m-task"} {
		mustEnqueue(t, q, Task{ID: id, Kind: "noop", MaxAttempts: 1})
	}
	// 依次领取并 Nack（MaxAttempts=1 → 直接死信）。领取顺序按 CreatedAt，
	// 显式 ID 不保证等于创建顺序；循环直到队列空。
	for {
		c, err := q.Claim(ctx, "w", nil, time.Minute)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if c == nil {
			break
		}
		if err := q.Nack(ctx, TaskID(c.ID), errors.New("x")); err != nil {
			t.Fatalf("Nack: %v", err)
		}
		clk.Advance(time.Millisecond)
	}

	dead, err := q.DeadLetters(ctx)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	want := []TaskID{"a-task", "m-task", "z-task"}
	if len(dead) != len(want) {
		t.Fatalf("死信集合=%v", dead)
	}
	for i := range want {
		if dead[i] != want[i] {
			t.Fatalf("死信必须按字典序: got=%v want=%v", dead, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. RequireCaps 过滤
// ---------------------------------------------------------------------------

func TestQueueRequireCapsFiltering(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	mustEnqueue(t, q, Task{ID: "needs-bash", Kind: "noop", RequireCaps: []string{"bash"}})
	mustEnqueue(t, q, Task{ID: "needs-none", Kind: "noop"})
	mustEnqueue(t, q, Task{ID: "needs-bash-k8s", Kind: "noop", RequireCaps: []string{"bash", "k8s"}})

	// Claim（nil caps）不施加能力过滤：旧调用方行为不变。
	c, err := q.Claim(ctx, "legacy", nil, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("Claim: task=%v err=%v", c, err)
	}
	if c.ID != "needs-bash" {
		t.Fatalf("nil caps 应忽略 RequireCaps，按 FIFO 领取，实际 %s", c.ID)
	}

	// 只有 bash 的 worker：拿不到 needs-bash-k8s。
	c, err = q.ClaimFor(ctx, "bash-only", nil, []string{"bash"}, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("ClaimFor bash-only: task=%v err=%v", c, err)
	}
	if c.ID != "needs-none" {
		t.Fatalf("bash-only 应跳过 needs-bash-k8s，实际领到 %s", c.ID)
	}

	// 再拿一次：只剩 k8s 要求，bash-only 领不到。
	if got, err := q.ClaimFor(ctx, "bash-only", nil, []string{"bash"}, time.Minute); err != nil || got != nil {
		t.Fatalf("缺 k8s 的 worker 不应领到任务: task=%v err=%v", got, err)
	}

	// 有 bash+k8s 的 worker 才能领。
	c, err = q.ClaimFor(ctx, "full", nil, []string{"k8s", "bash"}, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("ClaimFor full: task=%v err=%v", c, err)
	}
	if c.ID != "needs-bash-k8s" {
		t.Fatalf("应领到 needs-bash-k8s，实际 %s", c.ID)
	}

	// 显式空切片 = 无任何能力：只能接无要求的任务。
	mustEnqueue(t, q, Task{ID: "needs-any", Kind: "noop", RequireCaps: []string{"bash"}})
	if got, err := q.ClaimFor(ctx, "nocaps", nil, []string{}, time.Minute); err != nil || got != nil {
		t.Fatalf("空 caps 不应领到有要求的任务: task=%v err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// 8. 空队列不是错误
// ---------------------------------------------------------------------------

func TestQueueEmptyClaimNotError(t *testing.T) {
	q, _ := newTestQueue(t)
	c, err := q.Claim(context.Background(), "w", nil, time.Minute)
	if err != nil {
		t.Fatalf("空队列 Claim 返回错误: %v", err)
	}
	if c != nil {
		t.Fatalf("空队列应返回 (nil, nil)，实际 %+v", c)
	}
	// DeadLetters 空集也是空切片而非错误。
	dead, err := q.DeadLetters(context.Background())
	if err != nil || len(dead) != 0 {
		t.Fatalf("空死信: dead=%v err=%v", dead, err)
	}
}

// ---------------------------------------------------------------------------
// 9. Kind 过滤：空 kinds 接受全部
// ---------------------------------------------------------------------------

func TestQueueKindFiltering(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	mustEnqueue(t, q, Task{ID: "k1", Kind: "extract"})
	mustEnqueue(t, q, Task{ID: "k2", Kind: "reflect"})

	// kinds=[reflect] 跳过 extract。
	c, err := q.Claim(ctx, "w", []string{"reflect"}, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("Claim reflect: task=%v err=%v", c, err)
	}
	if c.Kind != "reflect" {
		t.Fatalf("应只领 reflect，实际 %+v", c)
	}

	// kinds=nil 接受全部。
	c, err = q.Claim(ctx, "w", nil, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("Claim 全部: task=%v err=%v", c, err)
	}
	if c.Kind != "extract" {
		t.Fatalf("空 kinds 应接受所有 Kind，实际 %+v", c)
	}
}

// ---------------------------------------------------------------------------
// 10. IdempotencyKey 原样透传
// ---------------------------------------------------------------------------

func TestQueueIdempotencyKeyPreserved(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	mustEnqueue(t, q, Task{Kind: "noop", IdempotencyKey: "turn-42-extract"})
	c, err := q.Claim(ctx, "w", nil, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("Claim: task=%v err=%v", c, err)
	}
	// 队列不去重、不改写：同一 key 可以入队多次，去重责任在 handler。
	if c.IdempotencyKey != "turn-42-extract" {
		t.Fatalf("IdempotencyKey 必须原样携带，实际 %q", c.IdempotencyKey)
	}
}

// ---------------------------------------------------------------------------
// 11. 并发：同一任务不会被两个 worker 同时领到
// ---------------------------------------------------------------------------

func TestQueueConcurrentClaimNoDuplicate(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	const n = 50
	for i := 0; i < n; i++ {
		mustEnqueue(t, q, Task{Kind: "noop"})
	}

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			for {
				c, err := q.Claim(ctx, worker, nil, time.Minute)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				seen[c.ID]++
				mu.Unlock()
			}
		}(fmt.Sprintf("w%d", w))
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("应恰好领取 %d 个任务，实际 %d", n, len(seen))
	}
	for id, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("任务 %s 被重复领取 %d 次（并发下 read-modify-write 未串行化）", id, cnt)
		}
	}
}

// ---------------------------------------------------------------------------
// 附加：入队校验与延迟任务
// ---------------------------------------------------------------------------

func TestQueueEnqueueValidation(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, Task{Kind: ""}); err == nil {
		t.Fatal("空 Kind 必须报错")
	}
	if _, err := q.Enqueue(ctx, Task{ID: "dup", Kind: "noop"}); err != nil {
		t.Fatalf("首次入队: %v", err)
	}
	if _, err := q.Enqueue(ctx, Task{ID: "dup", Kind: "noop"}); err == nil {
		t.Fatal("重复 ID 必须报错而不是覆盖（覆盖会抹掉在途租约与 attempts）")
	}
}

func TestQueueVisibleAtDelayedTask(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	mustEnqueue(t, q, Task{Kind: "noop", VisibleAt: clk.Now().Add(10 * time.Minute)})
	if got, err := q.Claim(ctx, "w", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("未到可见时间的任务不应可领取: task=%v err=%v", got, err)
	}
	clk.Advance(11 * time.Minute)
	if got, err := q.Claim(ctx, "w", nil, time.Minute); err != nil || got == nil {
		t.Fatalf("到可见时间后应可领取: task=%v err=%v", got, err)
	}
}
