// idempotent_sink_test.go — 钉住 TurnSink 幂等包装的语义（Moqi R-H4 验收第 3 条）。
//
// fake sink 记录**每一次真实转发**，因为本文件要证明的核心就是"某些调用没有
// 转发、某些调用必须每次都转发"。只看包装层返回什么是不够的：一个把两次调用
// 都转发出去、却对第二次撒谎说"已存在"的实现，同样能满足返回值断言，但库里
// 已经有两条消息。计数是唯一能区分这两者的证据。
package harness

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// idemFakeSink 是可观测的 TurnSink 替身。
type idemFakeSink struct {
	mu sync.Mutex

	ensureCalls []UserRecord
	appendCalls []AssistantRecord
	markCalls   []TurnState

	// ensureAlready 是 EnsureUser 的返回值（模拟业务 API 已先写过）。
	ensureAlready bool
	// failEnsure / failAppend 是接下来 N 次调用的失败次数；用递减而不是
	// 布尔位，才能表达"第一次失败、第二次成功"。
	failEnsure int
	failAppend int

	// appendGate 非 nil 时，AppendAssistant 记录调用后阻塞到它关闭，
	// 用来在并发测试里把"其他 goroutine 正在等待在途转发"变成确定性时序。
	appendGate <-chan struct{}
	// appendEntered 在 AppendAssistant 真正进入时自增。
	appendEntered atomic.Int32
}

func (f *idemFakeSink) EnsureUser(_ context.Context, rec UserRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, rec)
	if f.failEnsure > 0 {
		f.failEnsure--
		return false, errors.New("ensure failed")
	}
	return f.ensureAlready, nil
}

func (f *idemFakeSink) AppendAssistant(_ context.Context, rec AssistantRecord) error {
	f.mu.Lock()
	f.appendCalls = append(f.appendCalls, rec)
	fail := f.failAppend > 0
	if fail {
		f.failAppend--
	}
	f.mu.Unlock()

	f.appendEntered.Add(1)
	if f.appendGate != nil {
		<-f.appendGate
	}
	if fail {
		return errors.New("append failed")
	}
	return nil
}

func (f *idemFakeSink) MarkTurn(_ context.Context, state TurnState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markCalls = append(f.markCalls, state)
	return nil
}

func (f *idemFakeSink) counts() (ensure, appendN, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ensureCalls), len(f.appendCalls), len(f.markCalls)
}

func (f *idemFakeSink) appends() []AssistantRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AssistantRecord(nil), f.appendCalls...)
}

// TestIdempotentSinkEnsureUserOnce 钉住：同一键第二次 EnsureUser 不转发，
// 且 already=true；首次的 already 必须原样透传（这里是 false）。
func TestIdempotentSinkEnsureUserOnce(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{}
	sink := NewIdempotentSink(inner)
	rec := UserRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1"}

	already, err := sink.EnsureUser(ctx, rec)
	if err != nil || already {
		t.Fatalf("首次 EnsureUser=(%v,%v)，want (false,nil)", already, err)
	}
	already, err = sink.EnsureUser(ctx, rec)
	if err != nil || !already {
		t.Fatalf("第二次 EnsureUser=(%v,%v)，want (true,nil)", already, err)
	}

	if ensure, _, _ := inner.counts(); ensure != 1 {
		t.Fatalf("inner.EnsureUser 被调用 %d 次，want 1（第二次不得转发）", ensure)
	}
}

// TestIdempotentSinkEnsureUserAlreadyPassthrough 钉住：宿主说"已经写过"时，
// 包装层也要把它原样返回，不能因为自己没见过就改口成 false。
func TestIdempotentSinkEnsureUserAlreadyPassthrough(t *testing.T) {
	inner := &idemFakeSink{ensureAlready: true}
	sink := NewIdempotentSink(inner)
	rec := UserRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1"}

	already, err := sink.EnsureUser(context.Background(), rec)
	if err != nil || !already {
		t.Fatalf("首次 EnsureUser=(%v,%v)，want (true,nil) 原样透传", already, err)
	}
}

// TestIdempotentSinkAppendOnce 钉住：同一键第二次 AppendAssistant 不转发，
// 且 CompletedAssistant 能取回首次缓存的记录。
func TestIdempotentSinkAppendOnce(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{}
	sink := NewIdempotentSink(inner)
	rec := AssistantRecord{
		OwnerID: "alice", SessionID: "s1", TurnID: "t1",
		MessageID: "m1", Model: "claude-x",
	}

	if err := sink.AppendAssistant(ctx, rec); err != nil {
		t.Fatalf("首次 AppendAssistant: %v", err)
	}
	if err := sink.AppendAssistant(ctx, rec); err != nil {
		t.Fatalf("第二次 AppendAssistant: %v", err)
	}
	if _, appendN, _ := inner.counts(); appendN != 1 {
		t.Fatalf("inner.AppendAssistant 被调用 %d 次，want 1", appendN)
	}

	got, ok, err := sink.CompletedAssistant(ctx, "alice", "s1", "t1")
	if err != nil || !ok {
		t.Fatalf("CompletedAssistant=(%v,%v)，want ok=true", ok, err)
	}
	if got.MessageID != "m1" || got.Model != "claude-x" {
		t.Fatalf("CompletedAssistant 返回 %+v，want 缓存的首次记录", got)
	}
}

// TestIdempotentSinkOwnerScoped 钉住：不同 owner 复用相同 (session,turn) 是
// 两个独立键 —— 各自都要真实转发，且各自取回自己的记录。
func TestIdempotentSinkOwnerScoped(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{}
	sink := NewIdempotentSink(inner)

	alice := AssistantRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1", MessageID: "alice-msg"}
	bob := AssistantRecord{OwnerID: "bob", SessionID: "s1", TurnID: "t1", MessageID: "bob-msg"}

	if err := sink.AppendAssistant(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := sink.AppendAssistant(ctx, bob); err != nil {
		t.Fatal(err)
	}
	if _, appendN, _ := inner.counts(); appendN != 2 {
		t.Fatalf("inner.AppendAssistant 被调用 %d 次，want 2（owner 是不同的键）", appendN)
	}

	gotA, okA, _ := sink.CompletedAssistant(ctx, "alice", "s1", "t1")
	gotB, okB, _ := sink.CompletedAssistant(ctx, "bob", "s1", "t1")
	if !okA || !okB {
		t.Fatalf("按 owner 取回失败：alice ok=%v bob ok=%v", okA, okB)
	}
	if gotA.MessageID != "alice-msg" || gotB.MessageID != "bob-msg" {
		t.Fatalf("owner 串号：alice=%q bob=%q", gotA.MessageID, gotB.MessageID)
	}
}

// TestIdempotentSinkOwnerNormalized 钉住：空 owner 与 "local" 是同一个键。
//
// 若按原始字符串建键，同一用户在"旧数据 owner 为空"与"新调用 owner=local"
// 两个入口下会有两套键，重放照样重复落库 —— 归一化就是为了堵住这个口子。
func TestIdempotentSinkOwnerNormalized(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{}
	sink := NewIdempotentSink(inner)

	rec := AssistantRecord{OwnerID: "", SessionID: "s1", TurnID: "t1", MessageID: "m1"}
	if err := sink.AppendAssistant(ctx, rec); err != nil {
		t.Fatal(err)
	}

	got, ok, err := sink.CompletedAssistant(ctx, "local", "s1", "t1")
	if err != nil || !ok || got.MessageID != "m1" {
		t.Fatalf("空 owner 与 local 未合并：ok=%v got=%+v err=%v", ok, got, err)
	}

	// 反向：以 local 落键，空 owner 也能读到。
	rec2 := AssistantRecord{OwnerID: "local", SessionID: "s1", TurnID: "t2", MessageID: "m2"}
	if err := sink.AppendAssistant(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := sink.CompletedAssistant(ctx, "", "s1", "t2"); !ok {
		t.Fatal("local 落键后空 owner 读不到")
	}
	if _, appendN, _ := inner.counts(); appendN != 2 {
		t.Fatalf("inner.AppendAssistant 被调用 %d 次，want 2（两个不同 turn）", appendN)
	}
}

// TestIdempotentSinkAppendRetryAfterFailure 钉住：失败不缓存，重试必须真的
// 再次转发；成功后 CompletedAssistant 才能看见。
func TestIdempotentSinkAppendRetryAfterFailure(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{failAppend: 1}
	sink := NewIdempotentSink(inner)
	rec := AssistantRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1", MessageID: "m1"}

	if err := sink.AppendAssistant(ctx, rec); err == nil {
		t.Fatal("第一次 AppendAssistant 应当失败")
	}
	if _, ok, _ := sink.CompletedAssistant(ctx, "alice", "s1", "t1"); ok {
		t.Fatal("失败的 AppendAssistant 不得被当成已完成")
	}

	if err := sink.AppendAssistant(ctx, rec); err != nil {
		t.Fatalf("第二次 AppendAssistant 应当成功，得 %v", err)
	}
	if _, appendN, _ := inner.counts(); appendN != 2 {
		t.Fatalf("inner.AppendAssistant 被调用 %d 次，want 2（失败必须允许重试）", appendN)
	}
	if _, ok, _ := sink.CompletedAssistant(ctx, "alice", "s1", "t1"); !ok {
		t.Fatal("重试成功后 CompletedAssistant 应当可见")
	}
}

// TestIdempotentSinkEnsureUserFailureNotCached 钉住：EnsureUser 失败同样不落
// 标记，第二次必须重新转发。
func TestIdempotentSinkEnsureUserFailureNotCached(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{failEnsure: 1}
	sink := NewIdempotentSink(inner)
	rec := UserRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1"}

	if _, err := sink.EnsureUser(ctx, rec); err == nil {
		t.Fatal("第一次 EnsureUser 应当失败")
	}
	already, err := sink.EnsureUser(ctx, rec)
	if err != nil {
		t.Fatalf("第二次 EnsureUser 应当成功，得 %v", err)
	}
	if already {
		t.Fatal("第二次 EnsureUser 不应声称 already")
	}
	if ensure, _, _ := inner.counts(); ensure != 2 {
		t.Fatalf("inner.EnsureUser 被调用 %d 次，want 2（失败必须允许重试）", ensure)
	}
}

// TestIdempotentSinkUserWithoutAssistant 钉住：只有用户消息、没有助手消息时
// 不能假装完成 —— 否则 runner 会跳过模型调用却什么都不返回。
func TestIdempotentSinkUserWithoutAssistant(t *testing.T) {
	ctx := context.Background()
	sink := NewIdempotentSink(&idemFakeSink{})

	if _, err := sink.EnsureUser(ctx, UserRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if rec, ok, err := sink.CompletedAssistant(ctx, "alice", "s1", "t1"); ok || err != nil {
		t.Fatalf("只有用户消息时 CompletedAssistant=(%+v,%v,%v)，want ok=false", rec, ok, err)
	}
}

// TestIdempotentSinkMarkTurnAlwaysForwarded 钉住：MarkTurn 是覆盖不是追加，
// 每次都转发；终态 failed 后 CompletedAssistant 必须 ok=false。
func TestIdempotentSinkMarkTurnAlwaysForwarded(t *testing.T) {
	ctx := context.Background()
	inner := &idemFakeSink{}
	sink := NewIdempotentSink(inner)
	base := AssistantRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1", MessageID: "m1"}
	if err := sink.AppendAssistant(ctx, base); err != nil {
		t.Fatal(err)
	}

	if err := sink.MarkTurn(ctx, TurnState{OwnerID: "alice", SessionID: "s1", TurnID: "t1", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.MarkTurn(ctx, TurnState{OwnerID: "alice", SessionID: "s1", TurnID: "t1", State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, _, mark := inner.counts(); mark != 2 {
		t.Fatalf("inner.MarkTurn 被调用 %d 次，want 2（状态覆盖必须每次转发）", mark)
	}

	if rec, ok, err := sink.CompletedAssistant(ctx, "alice", "s1", "t1"); ok || err != nil {
		t.Fatalf("MarkTurn(failed) 后 CompletedAssistant=(%+v,%v,%v)，want ok=false", rec, ok, err)
	}
}

// TestIdempotentSinkInterruptedNotComplete 钉住：interrupted 与 failed 同属
// "不可复用"终态。
func TestIdempotentSinkInterruptedNotComplete(t *testing.T) {
	ctx := context.Background()
	sink := NewIdempotentSink(&idemFakeSink{})
	if err := sink.AppendAssistant(ctx, AssistantRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.MarkTurn(ctx, TurnState{OwnerID: "alice", SessionID: "s1", TurnID: "t1", State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := sink.CompletedAssistant(ctx, "alice", "s1", "t1"); ok {
		t.Fatal("interrupted 的 turn 不得被当成完成")
	}
}

// TestIdempotentSinkConcurrentAppendOnce 钉住：同一键 8 个并发 AppendAssistant
// 恰好只真正转发 1 次（配合 -race 运行）。
func TestIdempotentSinkConcurrentAppendOnce(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	inner := &idemFakeSink{appendGate: gate}
	sink := NewIdempotentSink(inner)
	rec := AssistantRecord{OwnerID: "alice", SessionID: "s1", TurnID: "t1", MessageID: "m1"}

	const goroutines = 8
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- sink.AppendAssistant(ctx, rec)
		}()
	}
	close(start)

	// 等到首次转发真的进入 inner 再放行。此时其余 goroutine 已经在 inflight
	// 占位上等待，测试稳定覆盖"后来者等它结束"这条路径，而不是靠 sleep 猜。
	for inner.appendEntered.Load() == 0 {
		// 自旋很短：inner 的第一次调用由上面的 goroutine 立即触发。
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发 AppendAssistant 返回错误: %v", err)
		}
	}

	appends := inner.appends()
	if len(appends) != 1 {
		t.Fatalf("inner.AppendAssistant 被调用 %d 次，want 1", len(appends))
	}
	if _, ok, _ := sink.CompletedAssistant(ctx, "alice", "s1", "t1"); !ok {
		t.Fatal("并发成功写入后 CompletedAssistant 应当可见")
	}
}

// TestIdempotentSinkNil 钉住：NewIdempotentSink(nil) 返回 nil，且各方法对
// nil receiver 安全 —— Runner 里 nil sink 是合法语义"不落库"。
func TestIdempotentSinkNil(t *testing.T) {
	sink := NewIdempotentSink(nil)
	if sink != nil {
		t.Fatal("NewIdempotentSink(nil) 应当返回 nil")
	}

	// 显式 *IdempotentSink 的 nil 值也要安全，因为接口断言后可能得到 nil 指针。
	var nilSink *IdempotentSink
	var _ TurnSink = nilSink
	var _ TurnReplay = nilSink

	ctx := context.Background()
	if already, err := nilSink.EnsureUser(ctx, UserRecord{}); already || err != nil {
		t.Fatalf("nil EnsureUser=(%v,%v)，want (false,nil)", already, err)
	}
	if err := nilSink.AppendAssistant(ctx, AssistantRecord{}); err != nil {
		t.Fatalf("nil AppendAssistant: %v", err)
	}
	if err := nilSink.MarkTurn(ctx, TurnState{}); err != nil {
		t.Fatalf("nil MarkTurn: %v", err)
	}
	if _, ok, err := nilSink.CompletedAssistant(ctx, "", "", ""); ok || err != nil {
		t.Fatalf("nil CompletedAssistant ok=%v err=%v，want (false,nil)", ok, err)
	}
}
