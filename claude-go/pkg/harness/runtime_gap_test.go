// runtime_gap_test.go — 本轮补齐的 harness 能力的生产路径验收。
//
// 每个用例都走 Runner.RunTurn 或 QueueWorker 的真实装配，而不是直接调辅助函数：
// 这一轮修的全是"实现存在、判据存在，但生产路径没接上"，只验单元函数会再次
// 放过同一类问题。
package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/types"
)

// ---------------------------------------------------------------------------
// R-H3: 业务安全判定
// ---------------------------------------------------------------------------

// TestRunTurn_SafetyBlockStopsBeforeModel 钉住"block 时一个字节都不发"。
func TestRunTurn_SafetyBlockStopsBeforeModel(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	sink := &recordingSink{}
	tr := &traceRecorder{}
	r := newTestRunner(t, up, 4, sink, func(o *RunnerOptions) { o.Trace = tr })

	req := textTurn("s1", "t1", "我不想活了")
	req.Safety = SafetyVerdict{Decision: "block", Reasons: []string{"self_harm"}}

	res, err := r.RunTurn(context.Background(), req, nil)
	if err == nil {
		t.Fatal("safety=block 应返回错误")
	}
	if !errors.Is(err, ErrSafetyBlocked) {
		t.Fatalf("应为 ErrSafetyBlocked, 实际 %v", err)
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindSafety {
		t.Fatalf("错误分类应为 safety, 实际 %v", err)
	}
	if up.count() != 0 {
		t.Fatalf("safety=block 后仍调了 %d 次模型", up.count())
	}
	if res.Terminal.Reason != "safety_blocked" {
		t.Fatalf("终态理由应为 safety_blocked, 实际 %q", res.Terminal.Reason)
	}
	if res.Safety.Decision != "block" || len(res.Safety.Reasons) != 1 {
		t.Fatalf("TurnResult.Safety 必须原样携带业务判定, 实际 %+v", res.Safety)
	}
	// 安全拦截先于落库：否则一条被拒绝的内容已经写进了宿主历史。
	if len(sink.users) != 0 {
		t.Fatalf("safety=block 时不应写用户消息, 实际 %d 条", len(sink.users))
	}
	last, ok := tr.last()
	if !ok || last.Safety.Decision != "block" {
		t.Fatalf("trace 必须携带安全判定, 实际 %+v (ok=%v)", last.Safety, ok)
	}
}

// TestRunTurn_SafetyUnknownDecisionFailsClosed 钉住"拼错的 decision 不能放行"。
func TestRunTurn_SafetyUnknownDecisionFailsClosed(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)
	req := textTurn("s1", "t1", "hi")
	req.Safety = SafetyVerdict{Decision: "deny"}

	if _, err := r.RunTurn(context.Background(), req, nil); !errors.Is(err, ErrSafetyBlocked) {
		t.Fatalf("未知 decision 应失败关闭, 实际 %v", err)
	}
	if up.count() != 0 {
		t.Fatal("失败关闭时不应发请求")
	}
}

// ---------------------------------------------------------------------------
// R-H3: 引用
// ---------------------------------------------------------------------------

// TestRunTurn_CitationsComeFromServedBlocks 钉住"只有真正进入请求的块的引用
// 才出现在结果里"，并验证引用事件与结果同源。
func TestRunTurn_CitationsComeFromServedBlocks(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.ContextTokenBudget = 10
		o.ContextProvider = ctxProviderFunc(func(context.Context, TurnContext) (ContextPatch, error) {
			return ContextPatch{Blocks: []ContextBlock{
				{
					ID: "keep", Text: "关键证据", Priority: 10, Tokens: 5,
					Refs: []Citation{
						{ID: "c1", Source: "kb", Title: "材料一"},
						{ID: "c1", Source: "kb", Title: "重复引用"},
					},
				},
				{
					ID: "drop", Text: "次要证据", Priority: 1, Tokens: 100,
					Refs: []Citation{{ID: "c2", Source: "kb", Title: "被裁掉的材料"}},
				},
			}}, nil
		})
	})

	var events []TurnEvent
	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "问"), func(ev TurnEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 1 || res.Citations[0].ID != "c1" {
		t.Fatalf("引用应只含被保留块的引用且按 ID 去重, 实际 %+v", res.Citations)
	}
	var cited []string
	for _, ev := range events {
		if ev.Kind == EventCitation && ev.Citation != nil {
			cited = append(cited, ev.Citation.ID)
		}
	}
	if len(cited) != 1 || cited[0] != "c1" {
		t.Fatalf("引用事件应与结果同源, 实际 %v", cited)
	}
	if !strings.Contains(up.lastBody(), "关键证据") {
		t.Fatal("被保留的证据没有进入请求体")
	}
}

// ---------------------------------------------------------------------------
// R-H11: 模态判定必须用"实际会用的模型"
// ---------------------------------------------------------------------------

// TestRunTurn_ModalityUsesEngineDefaultModel 钉住改造前的静默跳过：
// 没有配 ModelRoles 时 model 为空，旧实现直接放行整段能力协商。
func TestRunTurn_ModalityUsesEngineDefaultModel(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil) // engine.Config.Model = "m"，不在能力表里

	req := textTurn("s1", "t1", "看图")
	req.Input = append(req.Input, types.ContentBlock{
		Type:   types.ContentBlockImage,
		Source: &types.MediaSource{Type: "base64", MediaType: "image/png", Data: "AAAA"},
	})

	_, err := r.RunTurn(context.Background(), req, nil)
	if !errors.Is(err, ErrUnsupportedModality) {
		t.Fatalf("未配置角色映射时也必须按 engine 默认模型判定模态, 实际 %v", err)
	}
	if up.count() != 0 {
		t.Fatal("模态不支持时不应发请求")
	}
}

// ---------------------------------------------------------------------------
// R-H12: 终态恰好一条（包括早退路径）
// ---------------------------------------------------------------------------

// TestRunTurn_TerminalEmittedOnceOnEarlyExit 钉住早退也要有终态。
//
// 用 safety=block 制造一条"在任何模型调用之前"退出的路径：改造前这条路径
// 直接 return，消费方等不到终态事件。
func TestRunTurn_TerminalEmittedOnceOnEarlyExit(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)

	req := textTurn("s1", "t1", "hi")
	req.Safety = SafetyVerdict{Decision: "block"}

	var events []TurnEvent
	if _, err := r.RunTurn(context.Background(), req, func(ev TurnEvent) {
		events = append(events, ev)
	}); err == nil {
		t.Fatal("应返回错误")
	}

	var terminals int
	var lastSeq int64
	for _, ev := range events {
		if ev.Seq <= lastSeq {
			t.Fatalf("Seq 必须严格单调: prev=%d cur=%d", lastSeq, ev.Seq)
		}
		lastSeq = ev.Seq
		if ev.Kind == EventTerminal {
			terminals++
			if ev.Terminal == nil || ev.Terminal.Reason != "safety_blocked" {
				t.Fatalf("终态事件内容不对: %+v", ev.Terminal)
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("早退路径也必须恰好一条终态事件, 实际 %d", terminals)
	}
}

// ---------------------------------------------------------------------------
// R-H10: partial 标记
// ---------------------------------------------------------------------------

// TestRunTurn_PartialOnBudgetCancel 钉住"预算中断时已有文本被标成半截"。
func TestRunTurn_PartialOnBudgetCancel(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "已经说了一半"}
	r := newTestRunner(t, up, 4, nil)

	req := textTurn("s1", "t1", "讲个长故事")
	// 角色级 token 上限（R-H17）同时被验：1 token 必然在第一次 usage 后触发。
	req.Policy.RoleLimits = map[string]RoleLimit{
		RoleDialogue: {MaxTokens: 1},
	}

	res, err := r.RunTurn(context.Background(), req, nil)
	if err == nil {
		t.Fatal("预算耗尽应返回错误")
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindBudget {
		t.Fatalf("错误分类应为 budget, 实际 %v", err)
	}
	if res.Terminal.Reason != "budget_exhausted" {
		t.Fatalf("终态应为 budget_exhausted, 实际 %q", res.Terminal.Reason)
	}
	if res.Text == "" {
		t.Fatal("已经收到的文本不应丢失")
	}
	if !res.Partial {
		t.Fatal("非正常完成且有内容时必须标 partial")
	}
}

// ---------------------------------------------------------------------------
// R-H2: 布局观测必须上报
// ---------------------------------------------------------------------------

type gapLayoutSink struct {
	mu      sync.Mutex
	layouts []PromptLayout
	usages  []types.Usage
}

func (s *gapLayoutSink) RecordPromptLayout(_ context.Context, l PromptLayout, u types.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.layouts = append(s.layouts, l)
	s.usages = append(s.usages, u)
	return nil
}

func (s *gapLayoutSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.layouts)
}

// TestRunTurn_RecordsPromptLayout 钉住 RecordPromptLayout 有生产调用方。
func TestRunTurn_RecordsPromptLayout(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "好的"}
	sink := &gapLayoutSink{}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.Layout = NewLayoutBuilder(LayoutOptions{})
		o.PromptLayout = sink
	})

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 {
		t.Fatalf("每轮应上报一次布局观测, 实际 %d", sink.count())
	}
	if sink.layouts[0].PrefixHash == "" {
		t.Fatal("上报的布局必须带 PrefixHash")
	}
}

// ---------------------------------------------------------------------------
// R-H13: 队列运行时
// ---------------------------------------------------------------------------

// newRealtimeQueue 用真实时钟：worker 的轮询/退避都是墙钟驱动，
// 假时钟不推进会让 Nack 后的任务永远不可见。
func newRealtimeQueue(t *testing.T) *StoreQueue {
	t.Helper()
	q := NewStoreQueue(statestore.NewMemStore(), QueueOptions{
		DefaultLease: time.Minute,
		BackoffBase:  2 * time.Millisecond,
		BackoffMax:   20 * time.Millisecond,
	})
	if q == nil {
		t.Fatal("NewStoreQueue 返回 nil")
	}
	return q
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func TestQueueWorker_ProcessesAndAcks(t *testing.T) {
	q := newRealtimeQueue(t)
	mustEnqueue(t, q, Task{Kind: "noop", MaxAttempts: 3})

	got := make(chan ClaimedTask, 1)
	w, err := NewQueueWorker(QueueWorkerOptions{
		Queue: q, WorkerID: "w1", PollInterval: 5 * time.Millisecond, Lease: time.Minute,
		Handler: TaskHandlerFunc(func(_ context.Context, task ClaimedTask) error {
			got <- task
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case task := <-got:
		if task.Kind != "noop" || task.Attempts != 1 {
			t.Fatalf("handler 收到的任务不对: %+v", task)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker 没有领取任务")
	}
	waitFor(t, 2*time.Second, "Ack", func() bool { return w.Stats().Completed == 1 })

	// 已 Ack 的任务不应再被领走。
	if task, err := q.Claim(context.Background(), "w2", nil, time.Minute); err != nil || task != nil {
		t.Fatalf("已 Ack 的任务仍可领取: task=%+v err=%v", task, err)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestQueueWorker_GracefulStopWaitsForInFlight(t *testing.T) {
	q := newRealtimeQueue(t)
	mustEnqueue(t, q, Task{Kind: "slow", MaxAttempts: 3})

	started := make(chan struct{})
	release := make(chan struct{})
	w, err := NewQueueWorker(QueueWorkerOptions{
		Queue: q, WorkerID: "w1", PollInterval: 5 * time.Millisecond, Lease: time.Minute,
		Handler: TaskHandlerFunc(func(ctx context.Context, _ ClaimedTask) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker 没有开始执行任务")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- w.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop 不应在在途 handler 结束前返回: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("宽限内结束应返回 nil: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 没有在 handler 结束后返回")
	}
	if w.Stats().Completed != 1 {
		t.Fatalf("优雅停止应让在途任务完成并 Ack, stats=%+v", w.Stats())
	}
}

// losingLeaseQueue 模拟"租约被别的 worker 抢走"。
type losingLeaseQueue struct {
	*StoreQueue
}

func (q *losingLeaseQueue) RenewFor(context.Context, TaskID, string, time.Duration) error {
	return errors.New("lease stolen")
}

func TestQueueWorker_LeaseLossCancelsHandler(t *testing.T) {
	base := newRealtimeQueue(t)
	mustEnqueue(t, base, Task{Kind: "noop", MaxAttempts: 3})
	q := &losingLeaseQueue{StoreQueue: base}

	var canceled int32
	w, err := NewQueueWorker(QueueWorkerOptions{
		Queue: q, WorkerID: "w1", PollInterval: 5 * time.Millisecond,
		// lease/3 = 30ms，续租会很快失败。
		Lease: 90 * time.Millisecond,
		Handler: TaskHandlerFunc(func(ctx context.Context, _ ClaimedTask) error {
			<-ctx.Done()
			atomic.StoreInt32(&canceled, 1)
			return ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "续租失败后取消 handler", func() bool {
		return atomic.LoadInt32(&canceled) == 1
	})
	waitFor(t, 2*time.Second, "LeaseLost 计数", func() bool {
		return w.Stats().LeaseLost == 1
	})
	// 失去租约后不能 Ack/Nack：任务仍保持 leased，等新持有者处理。
	if got := w.Stats().Completed + w.Stats().Failed; got != 0 {
		t.Fatalf("失去租约后不应写回结果, stats=%+v", w.Stats())
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestQueueWorker_PanicIsNackedAndRetried(t *testing.T) {
	q := newRealtimeQueue(t)
	mustEnqueue(t, q, Task{Kind: "flaky", MaxAttempts: 3})

	var attempts int32
	w, err := NewQueueWorker(QueueWorkerOptions{
		Queue: q, WorkerID: "w1", PollInterval: 5 * time.Millisecond, Lease: time.Minute,
		Handler: TaskHandlerFunc(func(context.Context, ClaimedTask) error {
			if atomic.AddInt32(&attempts, 1) == 1 {
				panic("boom")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "panic 后重试成功", func() bool {
		return w.Stats().Completed == 1
	})
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("panic 应导致 Nack 重试, attempts=%d", got)
	}
	if w.Stats().Failed == 0 {
		t.Fatal("panic 必须计入失败（否则任务会停在 leased 直到租约过期）")
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
