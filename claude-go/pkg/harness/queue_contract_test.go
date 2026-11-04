package harness

// R-H13 持久后台任务：验收契约测试。
//
// 与 queue_test.go 的分工：那边钉的是队列内部算法（派发计数、退避、能力过滤等），
// 这里钉的是设计文档 07-claude-go-assessment.md "R-H13" 的逐条验收，而且全部用
// **磁盘后端 + 重建实例**来模拟 kill -9 / 进程重启 —— 只靠同一实例的内存态无法
// 证明"持久"，这两类断言不能互相替代。
//
// 时间全部走 fakeClock 注入（queue_test.go 的 newFakeClock），不 sleep 到秒级。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// newFileQueue 返回一个基于磁盘 FileStore 的队列。
//
// 同一 dir 上再次调用等价于进程重启：新实例没有任何内存态，只能从磁盘读回任务；
// 用 t.TempDir() 让每个测试拥有独立目录，测试结束由 testing 清理，无需手工删文件。
// 退避基数压到 1ms（封顶 8ms），让"Nack → 可再领取"只需推进几毫秒假时钟。
func newFileQueue(t *testing.T, dir string, clk *fakeClock) *StoreQueue {
	t.Helper()
	q := NewStoreQueue(statestore.NewFileStore(dir), QueueOptions{
		Now:          clk.Now,
		DefaultLease: time.Minute,
		BackoffBase:  1 * time.Millisecond,
		BackoffMax:   8 * time.Millisecond,
	})
	if q == nil {
		t.Fatal("NewStoreQueue 返回 nil")
	}
	return q
}

// decodePayload 把任务 Payload 解析成 map，用于磁盘后端（会重新缩进 JSON）下的
// 语义比较；解析失败直接判测试失败，避免空 map 让断言恒真。
func decodePayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Payload 不是合法 JSON 对象: %s (%v)", raw, err)
	}
	return m
}

// ---------------------------------------------------------------------------
// 验收 1：enqueue 后 kill -9 -> 重启可 claim
// ---------------------------------------------------------------------------

// TestQueueContractPendingSurvivesRestart 钉"持久"的核心：任务入队后进程被强杀
// （不调用任何关闭/刷盘 API），新进程从同一数据目录重建队列，必须仍能 claim 到。
//
// 失败即意味着 Enqueue 只写了内存，或者重启路径读不到已有记录 —— 后者正是
// kill -9 场景下"任务凭空消失"的表现。
func TestQueueContractPendingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	ctx := context.Background()

	q1 := newFileQueue(t, dir, clk)
	id := mustEnqueue(t, q1, Task{
		ID:             "persist-after-kill9",
		Kind:           "extract",
		IdempotencyKey: "turn-7-extract",
		Payload:        json.RawMessage(`{"turn":7}`),
		MaxAttempts:    3,
	})

	// 模拟 kill -9：q1 不再被使用，也不会有任何优雅停机回调。直接在同一目录上
	// 构造新队列；若数据只存在 q1 的内存里，这里就什么都看不到。
	q2 := newFileQueue(t, dir, clk)
	c, err := q2.Claim(ctx, "worker-after-restart", nil, time.Minute)
	if err != nil {
		t.Fatalf("重启后 Claim: %v", err)
	}
	if c == nil {
		t.Fatalf("任务 %s 入队后强杀重启必须仍可 claim（持久化的核心验收）", id)
	}
	if c.ID != string(id) {
		t.Fatalf("重启后领到的任务不对: got=%s want=%s", c.ID, id)
	}
	if c.Kind != "extract" {
		t.Fatalf("任务 Kind 未完整落盘: %+v", c)
	}
	// Payload 走磁盘 JSON 往返会被重新缩进，按语义比较而不是逐字节。
	if got := decodePayload(t, c.Payload); got["turn"] != float64(7) {
		t.Fatalf("任务 Payload 未完整落盘: %s", c.Payload)
	}
	if c.IdempotencyKey != "turn-7-extract" {
		t.Fatalf("IdempotencyKey 未跨重启保留: %q", c.IdempotencyKey)
	}
	if c.Attempts != 1 {
		t.Fatalf("重启后首次派发 Attempts 应为 1，实际 %d", c.Attempts)
	}
}

// ---------------------------------------------------------------------------
// 验收 2：重复执行幂等
// ---------------------------------------------------------------------------

// TestQueueContractDuplicateDeliveryIsIdempotentAtQueueLayer 钉"重复执行幂等"里
// **队列层能提供的那部分**，边界必须说清楚：
//
//   - 队列保证同一 Task.ID 只存在一条记录：重复 Enqueue 直接报错，不会生成第二个
//     work unit；handler 因此不会因为"同一次入队被提交两遍"而执行两次。
//   - 至少一次投递导致同一任务被重复派发时，两次拿到的是同一个 ID 和同一个
//     IdempotencyKey，handler 可以用它做业务侧去重（结果写入幂等）。
//   - Ack 幂等：worker 写完结果后崩溃、重启再上报一次成功，返回 nil 而不是错误。
//   - **边界**：队列不按 IdempotencyKey 去重 —— 不同 Task.ID 携带相同 key 是允许的，
//     真正的"副作用只发生一次"由 handler 用该 key 落幂等记录来保证。本测试最后一
//     段显式验证这个边界，避免把队列误当成业务幂等服务。
func TestQueueContractDuplicateDeliveryIsIdempotentAtQueueLayer(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	ctx := context.Background()

	q1 := newFileQueue(t, dir, clk)
	id := mustEnqueue(t, q1, Task{
		ID:             "idem-task",
		Kind:           "noop",
		IdempotencyKey: "turn-42-extract",
		MaxAttempts:    5,
	})

	c1, err := q1.Claim(ctx, "worker-a", nil, time.Minute)
	if err != nil || c1 == nil {
		t.Fatalf("首次 Claim: task=%v err=%v", c1, err)
	}

	// 第一次投递的 worker 崩溃（未 Ack、未 Nack），租约到期，重启后另一个 worker
	// 再次拿到同一个任务 —— 这就是"至少一次"下的重复执行。
	clk.Advance(2 * time.Minute)
	q2 := newFileQueue(t, dir, clk)
	c2, err := q2.Claim(ctx, "worker-b", nil, time.Minute)
	if err != nil || c2 == nil {
		t.Fatalf("重投 Claim: task=%v err=%v", c2, err)
	}
	if c2.ID != string(id) || c2.Attempts != 2 {
		t.Fatalf("重复投递应仍是同一个任务、Attempts=2，实际 id=%s attempts=%d", c2.ID, c2.Attempts)
	}
	if c1.IdempotencyKey != c2.IdempotencyKey || c2.IdempotencyKey != "turn-42-extract" {
		t.Fatalf("重复投递必须携带同一个 IdempotencyKey 供 handler 去重: first=%q second=%q",
			c1.IdempotencyKey, c2.IdempotencyKey)
	}

	// 同 ID 重复入队必须被拒绝，否则同一次业务动作会多出一个可执行副本。
	if _, err := q2.Enqueue(ctx, Task{ID: string(id), Kind: "noop", IdempotencyKey: "turn-42-extract"}); err == nil {
		t.Fatal("重复 Task.ID 必须报错：允许覆盖/新增会绕过 handler 的幂等键")
	}

	// Ack 幂等：成功上报可以重复到达。
	if err := q2.Ack(ctx, id); err != nil {
		t.Fatalf("首次 Ack: %v", err)
	}
	if err := q2.Ack(ctx, id); err != nil {
		t.Fatalf("重复 Ack 必须幂等返回 nil（至少一次投递下的正常现象），实际 %v", err)
	}

	// 边界：相同 IdempotencyKey、不同 Task.ID 不会被队列去重。这不是缺陷，
	// 而是契约把"副作用恰好一次"交给 handler（设计文档原文：幂等由 handler 的
	// IdempotencyKey 保证）。显式断言该行为，防止实现日后悄悄改变职责划分。
	if _, err := q2.Enqueue(ctx, Task{ID: "idem-task-second", Kind: "noop", IdempotencyKey: "turn-42-extract"}); err != nil {
		t.Fatalf("队列不应按 IdempotencyKey 去重（去重责任在 handler），实际报错: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 验收 3：毒任务进入死信
// ---------------------------------------------------------------------------

// TestQueueContractPoisonTaskReachesDeadLetters 钉"毒任务进入死信"：处理函数反复
// 失败（Nack），达到 MaxAttempts 后必须出现在 DeadLetters 且永不回到 Claim。
//
// 每次 Nack 后、下一次 claim 前都重建 StoreQueue：Attempts 计数必须随任务一起
// 落盘，否则进程重启会把尝试次数清零，毒任务永远到不了上限 —— 这正是"死信"
// 在崩溃保守场景下最容易失效的地方。
func TestQueueContractPoisonTaskReachesDeadLetters(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	ctx := context.Background()

	q := newFileQueue(t, dir, clk)
	id := mustEnqueue(t, q, Task{ID: "poison", Kind: "noop", MaxAttempts: 3})

	for attempt := 1; attempt <= 3; attempt++ {
		c, err := q.Claim(ctx, "worker-a", nil, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次 Claim: %v", attempt, err)
		}
		if c == nil {
			t.Fatalf("第 %d 次 Claim 应拿到毒任务", attempt)
		}
		if c.Attempts != attempt {
			t.Fatalf("第 %d 次派发 Attempts=%d（尝试次数必须跨重启累计）", attempt, c.Attempts)
		}
		if err := q.Nack(ctx, id, fmt.Errorf("poison-%d", attempt)); err != nil {
			t.Fatalf("第 %d 次 Nack: %v", attempt, err)
		}
		if attempt < 3 {
			// 模拟执行该次尝试的进程退出；退避窗口内任务不可领取。
			q = newFileQueue(t, dir, clk)
			if got, err := q.Claim(ctx, "worker-a", nil, time.Minute); err != nil || got != nil {
				t.Fatalf("退避窗口内不应可领取: task=%v err=%v", got, err)
			}
			clk.Advance(10 * time.Millisecond) // 越过 1ms/2ms 的指数退避
		}
	}

	dead, err := q.DeadLetters(ctx)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(dead) != 1 || dead[0] != id {
		t.Fatalf("达到最大尝试次数后应进死信 %s，实际 %v", id, dead)
	}

	// 死信状态本身也必须持久：重启后既不再被 claim，也仍然在死信集合里。
	q = newFileQueue(t, dir, clk)
	clk.Advance(time.Hour)
	if got, err := q.Claim(ctx, "worker-b", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("死信任务不得再被 Claim: task=%v err=%v", got, err)
	}
	dead, err = q.DeadLetters(ctx)
	if err != nil {
		t.Fatalf("重启后 DeadLetters: %v", err)
	}
	if len(dead) != 1 || dead[0] != id {
		t.Fatalf("死信记录必须跨重启保留，实际 %v", dead)
	}
}

// ---------------------------------------------------------------------------
// 验收 4：lease 过期后重投
// ---------------------------------------------------------------------------

// TestQueueContractLeaseExpiryRedeliversToAnotherWorker 钉"claim 后不 renew，等
// lease 过期，任务可被另一个 worker 再 claim"。
//
// 与 queue_test.go 的租约测试互补之处：这里加入了**续租控制组** —— 续租把截止
// 时间推过原租约后，旧 worker 仍然独占；一旦停止续租并越过新截止时间，任务才
// 交棒。没有这个控制组，"Claim 根本不看租约"的实现也能通过一个只用过期时间的
// 测试（它在任何时刻都会把任务发出去）。
func TestQueueContractLeaseExpiryRedeliversToAnotherWorker(t *testing.T) {
	q, clk := newTestQueue(t)
	ctx := context.Background()

	id := mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 5})
	if _, err := q.Claim(ctx, "worker-a", nil, time.Minute); err != nil {
		t.Fatalf("worker-a Claim: %v", err)
	}

	// 续租前的租约窗口内：别人拿不到。
	clk.Advance(30 * time.Second)
	if got, err := q.Claim(ctx, "worker-b", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("租约窗口内不应被他人领取: task=%v err=%v", got, err)
	}

	// worker-a 续租：截止时间从 t+60s 推到 t+90s。此刻已越过原租约，但未越过
	// 新租约 —— 若续租有效，任务仍归 worker-a。
	if err := q.Renew(ctx, id, time.Minute); err != nil {
		t.Fatalf("续租应成功: %v", err)
	}
	clk.Advance(45 * time.Second) // t+75s
	if got, err := q.Claim(ctx, "worker-b", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("续租后未到新截止时间，任务不应被重投（否则续租形同虚设）: task=%v err=%v", got, err)
	}

	// 停止续租，越过新截止时间（t+95s > t+90s）：worker-b 必须能接手。
	clk.Advance(20 * time.Second)
	c, err := q.Claim(ctx, "worker-b", nil, time.Minute)
	if err != nil {
		t.Fatalf("租约过期后 Claim: %v", err)
	}
	if c == nil {
		t.Fatal("lease 过期后任务必须可被另一个 worker 再 claim")
	}
	if c.ID != string(id) || c.Worker != "worker-b" || c.Attempts != 2 {
		t.Fatalf("重投视图错误: id=%s worker=%s attempts=%d", c.ID, c.Worker, c.Attempts)
	}
	// 接管后旧持有者必须失败，否则两个 worker 同时自认持有 = 静默重复执行。
	if err := q.RenewFor(ctx, id, "worker-a", time.Minute); err == nil {
		t.Fatal("租约被 worker-b 接手后，worker-a 的 RenewFor 必须失败")
	}
}

// ---------------------------------------------------------------------------
// 验收 5：停机不丢在途任务
// ---------------------------------------------------------------------------

// TestQueueContractInflightTaskSurvivesShutdown 钉"停机不丢在途任务"：worker 已
// claim 但尚未 Ack / Nack，进程消失后重启，任务必须仍在。
//
// 断言分两步，避免把"任务还在"和"任务立刻被重复执行"混为一谈：
//  1. 重启后租约仍在有效期内 —— 任务不可被别的 worker 领取（防重复执行）；
//  2. 假时钟越过租约后 —— 同一任务可被重新领取，Attempts 从 2 继续。
//
// 若 Claim 没把 leased 状态/租约落盘（只改了内存），重启后会看到 pending，
// 第一步就会立刻领到任务而失败；若 reaper 把过期租约当成删除，第二步会拿不到。
func TestQueueContractInflightTaskSurvivesShutdown(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	ctx := context.Background()

	q1 := newFileQueue(t, dir, clk)
	id := mustEnqueue(t, q1, Task{ID: "inflight", Kind: "long-job", Payload: json.RawMessage(`{"job":1}`), MaxAttempts: 5})
	c, err := q1.Claim(ctx, "worker-a", nil, time.Minute)
	if err != nil || c == nil {
		t.Fatalf("worker-a Claim: task=%v err=%v", c, err)
	}
	if c.Attempts != 1 {
		t.Fatalf("首次派发 Attempts=%d", c.Attempts)
	}

	// 停机（含 kill -9）：不 Ack、不 Nack，直接丢弃实例。
	q2 := newFileQueue(t, dir, clk)

	// 第一步：在途租约仍被尊重，任务不会立刻被第二个 worker 重复执行。
	if got, err := q2.Claim(ctx, "worker-b", nil, time.Minute); err != nil || got != nil {
		t.Fatalf("重启后租约有效期内不应被重复领取: task=%v err=%v", got, err)
	}

	// 第二步：任务没有消失，租约到期后可被接手，且尝试次数接着累加。
	clk.Advance(2 * time.Minute)
	c2, err := q2.Claim(ctx, "worker-b", nil, time.Minute)
	if err != nil {
		t.Fatalf("重启后租约过期 Claim: %v", err)
	}
	if c2 == nil {
		t.Fatalf("停机时在途的任务 %s 不得丢失", id)
	}
	if c2.ID != string(id) {
		t.Fatalf("重启后重新领到的不是原任务: got=%s want=%s", c2.ID, id)
	}
	if c2.Attempts != 2 {
		t.Fatalf("接手后在途尝试次数应继续累加为 2，实际 %d（计数被重启清零说明状态未持久）", c2.Attempts)
	}
	if c2.Kind != "long-job" || decodePayload(t, c2.Payload)["job"] != float64(1) {
		t.Fatalf("在途任务内容未完整保留: %+v", c2)
	}
}
