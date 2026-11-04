// context_timeout_test.go — R-H1 的超时与 critical 语义验收。
//
// 两条判据都落在"宿主看到了什么"上，而不是"provider 被调用过"：
//   - 超时：本轮必须**照常完成**，请求照发，TurnResult.Degraded/trace 里
//     有 context_degraded，且总耗时受 ContextTimeout 约束；
//   - critical：本轮必须在模型调用**之前**终止，错误分类是 ErrKindContext，
//     上游请求数为 0（否则用户会先收到一段"没有证据的回答"再看到报错）。
package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type ctxProviderFunc func(context.Context, TurnContext) (ContextPatch, error)

func (f ctxProviderFunc) ProvideTurnContext(ctx context.Context, tc TurnContext) (ContextPatch, error) {
	return f(ctx, tc)
}

type traceRecorder struct {
	mu     sync.Mutex
	traces []TurnTrace
}

func (r *traceRecorder) RecordTurnTrace(_ context.Context, tr TurnTrace) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.traces = append(r.traces, tr)
	return nil
}

func (r *traceRecorder) last() (TurnTrace, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.traces) == 0 {
		return TurnTrace{}, false
	}
	return r.traces[len(r.traces)-1], true
}

// TestRunTurn_ContextProviderTimeoutDegrades 是验收第 4 条的硬超时版本：
// provider 不主动看 ctx 时，本轮也不能被它拖住。
func TestRunTurn_ContextProviderTimeoutDegrades(t *testing.T) {
	up := &runnerUpstream{text: "先回答", calls: 0}
	trace := &traceRecorder{}
	r := newTestRunner(t, up, 2, nil, func(o *RunnerOptions) {
		o.ContextTimeout = 50 * time.Millisecond
		o.Trace = trace
		o.ContextProvider = ctxProviderFunc(func(_ context.Context, _ TurnContext) (ContextPatch, error) {
			// 刻意忽略 ctx：验证 harness 的硬超时不依赖 provider 配合。
			time.Sleep(500 * time.Millisecond)
			return ContextPatch{}, errors.New("late")
		})
	})

	start := time.Now()
	res, err := r.RunTurn(context.Background(), textTurn("s-timeout", "t-timeout", "在吗"), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("provider 超时应降级而不是失败: %v", err)
	}
	if res == nil || res.Terminal.Reason != "completed" {
		t.Fatalf("超时后本轮应正常完成, 实际 %+v", res)
	}
	if !containsString(res.Degraded, "context_degraded") {
		t.Fatalf("TurnResult.Degraded 缺 context_degraded: %v", res.Degraded)
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("硬超时未生效: 耗时 %s（provider 睡 500ms，上限 50ms）", elapsed)
	}
	if up.count() != 1 {
		t.Fatalf("降级后模型必须照常调用一次, 实际 %d 次", up.count())
	}
	tr, ok := trace.last()
	if !ok {
		t.Fatal("没有收到 trace")
	}
	if !containsString(tr.Degraded, "context_degraded") {
		t.Fatalf("trace 缺 context_degraded: %v", tr.Degraded)
	}
}

// TestRunTurn_ContextProviderCriticalFailsBeforeModel 钉住 critical 的语义：
// 失败即拒绝，且拒绝发生在模型调用之前。
func TestRunTurn_ContextProviderCriticalFailsBeforeModel(t *testing.T) {
	up := &runnerUpstream{text: "不应出现"}
	sink := &recordingSink{}
	r := newTestRunner(t, up, 2, sink, func(o *RunnerOptions) {
		o.ContextProvider = ctxProviderFunc(func(_ context.Context, _ TurnContext) (ContextPatch, error) {
			return ContextPatch{Critical: true}, errors.New("index unavailable")
		})
	})

	res, err := r.RunTurn(context.Background(), textTurn("s-crit", "t-crit", "在吗"), nil)
	if err == nil {
		t.Fatalf("critical 失败必须返回 error, 实际 res=%+v", res)
	}
	var herr *Error
	if !errors.As(err, &herr) {
		t.Fatalf("应为 *harness.Error, 实际 %T: %v", err, err)
	}
	if herr.Kind != ErrKindContext {
		t.Fatalf("错误分类应为 %q, 实际 %q", ErrKindContext, herr.Kind)
	}
	if up.count() != 0 {
		t.Fatalf("critical 失败时不得调用模型, 实际 %d 次", up.count())
	}
	if res == nil || res.Terminal.Reason != "context_critical" {
		t.Fatalf("终态应为 context_critical, 实际 %+v", res)
	}
	// 用户消息仍应在模型之前落库（R-H4 的时序不受影响）。
	if len(sink.users) != 1 {
		t.Fatalf("EnsureUser 应恰好 1 次, 实际 %d", len(sink.users))
	}
}

// TestRunTurn_ContextProviderCriticalWithoutErrorIsNotFailure 钉住"只有失败才
// 能 critical"：正常返回的 patch 即使标了 Critical，也不该把可选增强变成硬依赖。
func TestRunTurn_ContextProviderCriticalWithoutErrorIsNotFailure(t *testing.T) {
	up := &runnerUpstream{text: "有证据"}
	r := newTestRunner(t, up, 2, nil, func(o *RunnerOptions) {
		o.ContextProvider = ctxProviderFunc(func(_ context.Context, _ TurnContext) (ContextPatch, error) {
			return ContextPatch{
				Critical: true,
				Blocks:   []ContextBlock{{ID: "ev-1", Text: "证据正文"}},
			}, nil
		})
	})

	res, err := r.RunTurn(context.Background(), textTurn("s-ok", "t-ok", "在吗"), nil)
	if err != nil {
		t.Fatalf("正常返回的 patch 不应导致失败: %v", err)
	}
	if res.Terminal.Reason != "completed" {
		t.Fatalf("终态应 completed, 实际 %+v", res.Terminal)
	}
	if !strings.Contains(up.lastBody(), "证据正文") {
		t.Fatalf("证据块未进入请求: %s", up.lastBody())
	}
}
