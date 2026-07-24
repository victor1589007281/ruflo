package cluster

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

func newQ(t *testing.T, lease time.Duration) *Queue {
	return NewQueue(statestore.NewMemStore(), lease)
}

func TestEnqueuePullComplete(t *testing.T) {
	q := newQ(t, time.Minute)
	id, err := q.Enqueue(Task{Kind: "stage", Payload: json.RawMessage(`{"role":"coder"}`)})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := q.Pull("w1", nil)
	if err != nil || !ok {
		t.Fatalf("应拉到任务: ok=%v err=%v", ok, err)
	}
	if task.ID != id || task.Status != "leased" || task.Worker != "w1" || task.Attempts != 1 {
		t.Fatalf("租约字段错误: %+v", task)
	}
	if err := q.Complete(id, "w1", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	done, _ := q.Get(id)
	if done.Status != "completed" || string(done.Result) != `{"ok":true}` {
		t.Fatalf("完成状态错误: %+v", done)
	}
	// 完成后无可拉任务
	if _, ok, _ := q.Pull("w1", nil); ok {
		t.Fatal("完成的任务不应再被拉取")
	}
}

func TestPullKindFilter(t *testing.T) {
	q := newQ(t, time.Minute)
	_, _ = q.Enqueue(Task{Kind: "stage"})
	_, _ = q.Enqueue(Task{Kind: "media"})
	// worker 只接受 media
	task, ok, _ := q.Pull("w1", []string{"media"})
	if !ok || task.Kind != "media" {
		t.Fatalf("kind 过滤失败: %+v ok=%v", task, ok)
	}
}

func TestFailRequeueUntilMaxAttempts(t *testing.T) {
	q := newQ(t, time.Minute)
	id, _ := q.Enqueue(Task{Kind: "stage", MaxAttempts: 2})

	// 第 1 次失败 → 回队 (attempts=1 < 2)
	t1, _, _ := q.Pull("w1", nil)
	if err := q.Fail(id, "w1", "boom"); err != nil {
		t.Fatal(err)
	}
	if t1.Attempts != 1 {
		t.Fatalf("首拉 attempts 应为 1: %d", t1.Attempts)
	}
	after1, _ := q.Get(id)
	if after1.Status != "pending" {
		t.Fatalf("未达上限应回 pending: %s", after1.Status)
	}

	// 第 2 次失败 → 终态 failed (attempts=2 >= 2)
	_, ok, _ := q.Pull("w2", nil)
	if !ok {
		t.Fatal("回队任务应可再拉")
	}
	_ = q.Fail(id, "w2", "boom again")
	after2, _ := q.Get(id)
	if after2.Status != "failed" {
		t.Fatalf("达上限应 failed: %s", after2.Status)
	}
}

func TestLeaseExpiryReap(t *testing.T) {
	q := newQ(t, 50*time.Millisecond) // 极短租约
	id, _ := q.Enqueue(Task{Kind: "stage", MaxAttempts: 3})
	task, _, _ := q.Pull("crashed-worker", nil)
	if task.Status != "leased" {
		t.Fatal("应被租出")
	}
	time.Sleep(80 * time.Millisecond) // 租约过期

	// 另一 worker Pull 时触发回收 → 拿到被回收的任务
	reaped, ok, _ := q.Pull("w2", nil)
	if !ok || reaped.ID != id || reaped.Worker != "w2" {
		t.Fatalf("过期租约应被回收重派: %+v ok=%v", reaped, ok)
	}
	if reaped.Attempts != 2 {
		t.Fatalf("重派应递增 attempts: %d", reaped.Attempts)
	}
}

func TestExtendKeepsLease(t *testing.T) {
	q := newQ(t, 60*time.Millisecond)
	id, _ := q.Enqueue(Task{Kind: "stage"})
	_, _, _ = q.Pull("w1", nil)
	// 持续续租
	for i := 0; i < 3; i++ {
		time.Sleep(30 * time.Millisecond)
		if err := q.Extend(id, "w1"); err != nil {
			t.Fatalf("续租失败: %v", err)
		}
	}
	// 续租期间不应被回收
	got, _ := q.Get(id)
	if got.Status != "leased" || got.Worker != "w1" {
		t.Fatalf("续租应保持租约: %+v", got)
	}
	// 非租约 worker 不能续租
	if err := q.Extend(id, "intruder"); err == nil {
		t.Fatal("非租约 worker 续租应报错")
	}
}

func TestWaitCompletes(t *testing.T) {
	q := newQ(t, time.Minute)
	id, _ := q.Enqueue(Task{Kind: "stage"})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _, _ = q.Pull("w1", nil)
		_ = q.Complete(id, "w1", nil)
	}()
	task, err := q.Wait(id, time.Second)
	if err != nil || task.Status != "completed" {
		t.Fatalf("Wait 应返回完成态: %+v err=%v", task, err)
	}
}
