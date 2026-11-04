package harness

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// TestEmitterSeqMonotonicAndBeginTurn 钉住：Seq 每轮从 1 开始、连续、唯一，
// BeginTurn 后重置。
func TestEmitterSeqMonotonicAndBeginTurn(t *testing.T) {
	em := NewEmitter(EmitterOptions{BufferSize: 8})
	em.BeginTurn("t1")
	for i := 0; i < 5; i++ {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "x"}); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	res := em.Replay(0)
	if res.Truncated {
		t.Fatal("容量 8 装 5 条不应截断")
	}
	if len(res.Events) != 5 {
		t.Fatalf("事件数=%d，want 5", len(res.Events))
	}
	for i, ev := range res.Events {
		if want := int64(i + 1); ev.Seq != want {
			t.Fatalf("第 %d 条 Seq=%d，want %d", i, ev.Seq, want)
		}
		if ev.TurnID != "t1" {
			t.Fatalf("第 %d 条 TurnID=%q，want t1", i, ev.TurnID)
		}
	}

	em.BeginTurn("t2")
	if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "y"}); err != nil {
		t.Fatal(err)
	}
	res = em.Replay(0)
	if len(res.Events) != 1 || res.Events[0].Seq != 1 || res.Events[0].TurnID != "t2" {
		t.Fatalf("新一轮未重置: %+v", res)
	}
	if res.TurnID != "t2" {
		t.Fatalf("ReplayResult.TurnID=%q，want t2（跨轮续放必须靠它区分）", res.TurnID)
	}
}

// TestEmitterFillsSeqAndTurnID 钉住：调用方留空的 Seq/TurnID 由 Emitter 填；
// 调用方手填的 Seq 被覆盖（Seq 只有唯一分配点）。
func TestEmitterFillsSeqAndTurnID(t *testing.T) {
	em := NewEmitter(EmitterOptions{})
	em.BeginTurn("turn-x")
	if err := em.Emit(TurnEvent{Kind: EventDelta, Seq: 999, Delta: "a"}); err != nil {
		t.Fatal(err)
	}
	res := em.Replay(0)
	if len(res.Events) != 1 {
		t.Fatalf("len=%d", len(res.Events))
	}
	ev := res.Events[0]
	if ev.Seq != 1 {
		t.Fatalf("手填 Seq 未被覆盖: %d", ev.Seq)
	}
	if ev.TurnID != "turn-x" {
		t.Fatalf("TurnID 未填充: %q", ev.TurnID)
	}
}

// TestEmitterTerminalOnce 钉住：每轮终态只接受第一条，第二条返回哨兵且不入缓冲。
func TestEmitterTerminalOnce(t *testing.T) {
	em := NewEmitter(EmitterOptions{})
	em.BeginTurn("t")
	term := TerminalStub("completed")
	if err := em.Emit(TurnEvent{Kind: EventTerminal, Terminal: term}); err != nil {
		t.Fatalf("首条终态失败: %v", err)
	}
	err := em.Emit(TurnEvent{Kind: EventTerminal, Terminal: TerminalStub("failed")})
	if !errors.Is(err, ErrTerminalAlreadyEmitted) {
		t.Fatalf("第二条终态 err=%v，want ErrTerminalAlreadyEmitted", err)
	}
	res := em.Replay(0)
	if len(res.Events) != 1 {
		t.Fatalf("被拒绝的终态进了缓冲: len=%d", len(res.Events))
	}
	if res.Events[0].Terminal == nil || res.Events[0].Terminal.Reason != "completed" {
		// 仅确认保留的是首条终态（completed），第二条 failed 未被写入。
		t.Fatalf("保留的终态不是首条: %+v", res.Events[0].Terminal)
	}

	// 新一轮解除封锁。
	em.BeginTurn("t2")
	if err := em.Emit(TurnEvent{Kind: EventTerminal, Terminal: term}); err != nil {
		t.Fatalf("BeginTurn 后仍被封锁: %v", err)
	}
}

// TestEmitterTurnMismatch 钉住：事件自带 TurnID 与本轮不一致时拒收，
// 避免污染回放作用域。
func TestEmitterTurnMismatch(t *testing.T) {
	em := NewEmitter(EmitterOptions{})
	em.BeginTurn("a")
	err := em.Emit(TurnEvent{Kind: EventDelta, TurnID: "b", Delta: "x"})
	if !errors.Is(err, ErrTurnMismatch) {
		t.Fatalf("err=%v，want ErrTurnMismatch", err)
	}
	if res := em.Replay(0); len(res.Events) != 0 {
		t.Fatalf("被拒事件进了缓冲: %d", len(res.Events))
	}
}

// TestEmitterReplayAfter 钉住：Replay 返回 Seq > after 的事件，顺序正确。
func TestEmitterReplayAfter(t *testing.T) {
	em := NewEmitter(EmitterOptions{BufferSize: 16})
	em.BeginTurn("t")
	for i := 1; i <= 6; i++ {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	res := em.Replay(3)
	if res.Truncated {
		t.Fatal("未溢出不应截断")
	}
	if len(res.Events) != 3 {
		t.Fatalf("len=%d，want 3", len(res.Events))
	}
	for i, ev := range res.Events {
		if want := int64(i + 4); ev.Seq != want {
			t.Fatalf("[%d] Seq=%d want %d", i, ev.Seq, want)
		}
	}
	// 负数 after 当作从头。
	if got := em.Replay(-1); len(got.Events) != 6 {
		t.Fatalf("Replay(-1) len=%d，want 6", len(got.Events))
	}
	// after 越界：空且不叫截断（那只是"没有更新的事件"）。
	if got := em.Replay(100); len(got.Events) != 0 || got.Truncated {
		t.Fatalf("Replay(100)=%+v，want 空且非截断", got)
	}
}

// TestEmitterRingOverflowAndTruncation 钉住：满了淘汰最旧，Seq 不回绕；
// 读已淘汰区间必须报告 Truncated，而恰好接上最旧保留事件时不报。
func TestEmitterRingOverflowAndTruncation(t *testing.T) {
	const size = 4
	em := NewEmitter(EmitterOptions{BufferSize: size})
	em.BeginTurn("t")
	for i := 1; i <= 7; i++ {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}

	res := em.Replay(0)
	if !res.Truncated {
		t.Fatal("已淘汰 seq1-3，Replay(0) 必须 Truncated")
	}
	if len(res.Events) != size {
		t.Fatalf("保留 %d 条，want %d", len(res.Events), size)
	}
	if res.Events[0].Seq != 4 || res.Events[size-1].Seq != 7 {
		t.Fatalf("保留区间=[%d,%d]，want [4,7]", res.Events[0].Seq, res.Events[size-1].Seq)
	}
	// 淘汰后 Seq 仍严格递增。
	for i := 1; i < len(res.Events); i++ {
		if res.Events[i].Seq <= res.Events[i-1].Seq {
			t.Fatalf("Seq 非递增: %d 后接 %d", res.Events[i-1].Seq, res.Events[i].Seq)
		}
	}

	// afterSeq=3：下一条恰好是最旧保留的 4，无缺口。
	if got := em.Replay(3); got.Truncated {
		t.Fatalf("after=3 恰好接上 seq4，不应 Truncated")
	} else if len(got.Events) != size || got.Events[0].Seq != 4 {
		t.Fatalf("after=3: %+v", got)
	}
	// afterSeq=2：seq3 已丢，必须截断。
	if got := em.Replay(2); !got.Truncated {
		t.Fatal("after=2 丢了 seq3，必须 Truncated")
	}
	// afterSeq=7（最新）：空，非截断。
	if got := em.Replay(7); len(got.Events) != 0 || got.Truncated {
		t.Fatalf("after=7: %+v", got)
	}

	// 极小的缓冲也保持同一语义。
	tiny := NewEmitter(EmitterOptions{BufferSize: 1})
	tiny.BeginTurn("t")
	for i := 0; i < 3; i++ {
		if err := tiny.Emit(TurnEvent{Kind: EventDelta, Delta: "z"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := tiny.Replay(0); !got.Truncated || len(got.Events) != 1 || got.Events[0].Seq != 3 {
		t.Fatalf("size=1: %+v", got)
	}
}

// TestEmitterAssembleTextLossless 钉住：AssembleText(全部 delta) == 最终文本，
// 且把相邻 delta 合并成一条后结果不变（线上合并/再分片都无损）。
func TestEmitterAssembleTextLossless(t *testing.T) {
	parts := []string{"你", "好", "，", "世", "界"}
	em := NewEmitter(EmitterOptions{BufferSize: 64})
	em.BeginTurn("t")
	for _, p := range parts {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: p, BlockIndex: 0}); err != nil {
			t.Fatal(err)
		}
	}
	// 中间插一个非文本事件，确认它不贡献文本、也不打断拼接。
	if err := em.Emit(TurnEvent{Kind: EventToolStart, Tool: &ToolEvent{Name: "search"}}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "！"}); err != nil {
		t.Fatal(err)
	}

	all := em.Replay(0).Events
	final := AssembleText(all)
	if want := "你好，世界！"; final != want {
		t.Fatalf("AssembleText=%q，want %q", final, want)
	}

	// 模拟线上合并：把所有 delta 并成一条（Kind 保持 delta）。
	merged := []TurnEvent{
		{Seq: 1, TurnID: "t", Kind: EventDelta, Delta: "你好，世界！"},
		{Seq: 2, TurnID: "t", Kind: EventToolStart, Tool: &ToolEvent{Name: "search"}},
	}
	if got := AssembleText(merged); got != final {
		t.Fatalf("合并后拼接=%q，与原序列=%q 不等（合并有损）", got, final)
	}

	// 不同的分片方式也必须一致。
	resharded := []TurnEvent{
		{Kind: EventDelta, Delta: "你"},
		{Kind: EventDelta, Delta: "好，世界！"},
	}
	if got := AssembleText(resharded); got != final {
		t.Fatalf("重分片后拼接=%q，want %q", got, final)
	}
}

// TestEmitterSinkOutsideLock 钉住：Sink 在锁外调用 —— 回调里 Replay 不会死锁，
// 且能看到刚发射的事件（append 先于解锁）。
func TestEmitterSinkOutsideLock(t *testing.T) {
	var em *Emitter
	var seen []int64
	var sinkErr error
	em = NewEmitter(EmitterOptions{
		BufferSize: 8,
		Sink: func(ev TurnEvent) {
			// 若 Emit 仍持锁，这里会永久阻塞。
			res := em.Replay(0)
			seen = append(seen, ev.Seq)
			if len(res.Events) == 0 || res.Events[len(res.Events)-1].Seq != ev.Seq {
				sinkErr = fmt.Errorf("sink 内 Replay 看不到刚发射的事件 %d: %+v", ev.Seq, res.Events)
			}
		},
	})
	em.BeginTurn("t")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "d"}); err != nil {
				sinkErr = err
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Sink 回调 Replay 死锁（Emit 持锁调用了 Sink）")
	}
	if sinkErr != nil {
		t.Fatal(sinkErr)
	}
	if len(seen) != 5 {
		t.Fatalf("Sink 收到 %d 次，want 5", len(seen))
	}
}

// TestEmitterReplayConcurrent 钉住：并发 Emit 与 Replay 无数据竞争（-race 下运行），
// 且最终 Seq 集合恰好是 1..N。
func TestEmitterReplayConcurrent(t *testing.T) {
	em := NewEmitter(EmitterOptions{BufferSize: 4096})
	em.BeginTurn("t")

	const producers = 8
	const perProducer = 100
	var wg sync.WaitGroup
	wg.Add(producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				_ = em.Emit(TurnEvent{Kind: EventDelta, Delta: fmt.Sprint(p)})
			}
		}(p)
	}
	stop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = em.Replay(0)
			}
		}
	}()
	wg.Wait()
	close(stop)
	reader.Wait()

	res := em.Replay(0)
	if res.Truncated {
		t.Fatal("容量 4096 装 800 条不应截断")
	}
	if len(res.Events) != producers*perProducer {
		t.Fatalf("len=%d，want %d", len(res.Events), producers*perProducer)
	}
	want := make(map[int64]bool, len(res.Events))
	for _, ev := range res.Events {
		want[ev.Seq] = true
	}
	for i := int64(1); i <= producers*perProducer; i++ {
		if !want[i] {
			t.Fatalf("缺 Seq %d（唯一性被破坏）", i)
		}
	}
}

// TestEmitterLastEmitAt 钉住：Clock 注入生效，且 LastEmitAt 随事件推进。
func TestEmitterLastEmitAt(t *testing.T) {
	base := time.Unix(1700000000, 0)
	calls := 0
	em := NewEmitter(EmitterOptions{Clock: func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Second)
	}})
	if !em.LastEmitAt().IsZero() {
		t.Fatal("未发射事件时 LastEmitAt 应为零值")
	}
	em.BeginTurn("t")
	if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "a"}); err != nil {
		t.Fatal(err)
	}
	if got, want := em.LastEmitAt(), base.Add(time.Second); !got.Equal(want) {
		t.Fatalf("LastEmitAt=%v，want %v", got, want)
	}
}

// TerminalStub 构造一个带可辨认 Reason 的终态，便于在测试里区分首条/第二条。
// 放在 events_test.go 里是因为它只在测试中使用，不进契约 API。
func TerminalStub(reason string) *types.Terminal {
	return &types.Terminal{Reason: reason}
}
