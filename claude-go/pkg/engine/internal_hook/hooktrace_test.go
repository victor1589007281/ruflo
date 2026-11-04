// hooktrace_test.go — Moqi R-H7 的 trace 验收。
//
// 与 hookpolicy_test.go 的分工：那边验**链的失败语义**（advisory 继续 / critical
// 终止），这里只验**证据面**：每个 hook（含被跳过的）是否都留下了一条可读记录，
// 以及 per-request 观测面在并发 run 下是否串台。
package internal_hook

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// traceRecorder 是并发安全的 HookTrace 收集器。
type traceRecorder struct {
	mu  sync.Mutex
	got []HookTrace
}

func (r *traceRecorder) ObserveHookTrace(_ context.Context, tr HookTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, tr)
}

func (r *traceRecorder) snapshot() []HookTrace {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]HookTrace(nil), r.got...)
}

// eventRecorder 收集总线 Event。
type eventRecorder struct {
	mu  sync.Mutex
	got []Event
}

func (r *eventRecorder) Observe(_ context.Context, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
}

func (r *eventRecorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.got...)
}

// traceHook 是可编程 hook。它**总是**实现 HookPolicy（Critical/无超时是零值，
// 与不实现时的默认行为相同），因此一个类型就能表达 plain / advisory。
type traceHook struct {
	BaseInternalHook
	exec    func(*HookContext) (*HookResult, error)
	crit    Criticality
	timeout time.Duration
}

func (h *traceHook) Execute(c *HookContext) (*HookResult, error) { return h.exec(c) }
func (h *traceHook) Criticality() Criticality                    { return h.crit }
func (h *traceHook) Timeout() time.Duration                      { return h.timeout }

func newTraceHook(name string, prio int, exec func(*HookContext) (*HookResult, error)) *traceHook {
	return &traceHook{
		BaseInternalHook: BaseInternalHook{
			Name_: name, Priority_: prio, Phases_: []InternalHookPhase{PhasePreRequest},
		},
		exec: exec,
	}
}

func traceByName(traces []HookTrace, name string) (HookTrace, bool) {
	for _, tr := range traces {
		if tr.Name == name {
			return tr, true
		}
	}
	return HookTrace{}, false
}

// TestHookTrace_每条hook都有耗时与结果 钉住基本证据面：执行过的 hook 一条不少，
// 字段齐全，Result 是从 HookResult 归纳出的标签（不含正文）。
func TestHookTrace_每条hook都有耗时与结果(t *testing.T) {
	rec := &traceRecorder{}
	ctx := WithHookTraceObserver(context.Background(), rec)

	c := NewHookChain()
	c.Register(newTraceHook("ok", 10, func(*HookContext) (*HookResult, error) { return nil, nil }))
	c.Register(newTraceHook("prompt", 20, func(*HookContext) (*HookResult, error) {
		return &HookResult{SystemPrompt: []string{"s"}}, nil
	}))
	bad := newTraceHook("bad", 30, func(*HookContext) (*HookResult, error) {
		return nil, errors.New("retrieval down")
	})
	bad.crit = Advisory
	c.Register(bad)

	if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx, TurnCount: 3}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	traces := rec.snapshot()
	if len(traces) != 3 {
		t.Fatalf("应有 3 条 trace, 实际 %d: %+v", len(traces), traces)
	}
	for i, tr := range traces {
		if tr.Name == "" {
			t.Errorf("trace[%d] 缺 Name", i)
		}
		if tr.Phase != PhasePreRequest {
			t.Errorf("trace[%d] Phase 不符: %v", i, tr.Phase)
		}
		if tr.Duration <= 0 {
			t.Errorf("trace[%d] (%s) Duration 应 > 0, 实际 %v", i, tr.Name, tr.Duration)
		}
	}

	if traces[0].Result != "no_op" {
		t.Errorf("未介入 hook 的 Result 应为 no_op, 实际 %q", traces[0].Result)
	}
	if traces[1].Result != "prompt_modified" {
		t.Errorf("改 prompt 的 hook Result 应为 prompt_modified, 实际 %q", traces[1].Result)
	}
	if traces[2].Err == "" {
		t.Error("失败 hook 的 trace 应带 Err")
	}
	if traces[2].Result != "" {
		t.Errorf("失败 hook 的 Result 应为空 (不能伪装成 no_op), 实际 %q", traces[2].Result)
	}
	if traces[2].Criticality != Advisory {
		t.Errorf("trace 应带该 hook 的 Criticality, 实际 %v", traces[2].Criticality)
	}
	if traces[0].Criticality != Critical {
		t.Errorf("未声明 advisory 的 hook Criticality 应为 critical, 实际 %v", traces[0].Criticality)
	}
}

// TestHookTrace_AdvisoryPanic本轮继续 是 R-H7 的核心验收：
// advisory panic 不终止本轮，且 trace 里有 hook panic 的证据。
func TestHookTrace_AdvisoryPanic本轮继续(t *testing.T) {
	rec := &traceRecorder{}
	ctx := WithHookTraceObserver(context.Background(), rec)

	c := NewHookChain()
	boom := newTraceHook("boom", 10, func(*HookContext) (*HookResult, error) { panic("boom") })
	boom.crit = Advisory
	c.Register(boom)
	var reached bool
	c.Register(newTraceHook("after", 20, func(*HookContext) (*HookResult, error) {
		reached = true
		return nil, nil
	}))

	if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx}); err != nil {
		t.Fatalf("advisory panic 不应让链返回 error: %v", err)
	}
	if !reached {
		t.Fatal("advisory panic 后后续 hook 没有执行")
	}

	tr, ok := traceByName(rec.snapshot(), "boom")
	if !ok {
		t.Fatal("panic hook 没有 trace 记录")
	}
	if tr.Panic == "" {
		t.Error("panic hook 的 trace 应记录 Panic")
	}
	if tr.Err == "" {
		t.Error("panic hook 的 trace 也应带 Err (panic 已转为错误)")
	}
	if tr.Skipped {
		t.Error("执行过的 hook 不应标记 Skipped")
	}
}

// TestHookTrace_AdvisoryTimeout 验超时形态可辨认。
func TestHookTrace_AdvisoryTimeout(t *testing.T) {
	rec := &traceRecorder{}
	ctx := WithHookTraceObserver(context.Background(), rec)

	c := NewHookChain()
	slow := newTraceHook("slow", 10, func(hc *HookContext) (*HookResult, error) {
		<-hc.Ctx.Done()
		return nil, hc.Ctx.Err()
	})
	slow.crit = Advisory
	slow.timeout = 20 * time.Millisecond
	c.Register(slow)

	if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx}); err != nil {
		t.Fatalf("advisory timeout 不应返回 error: %v", err)
	}
	tr, ok := traceByName(rec.snapshot(), "slow")
	if !ok {
		t.Fatal("超时 hook 没有 trace 记录")
	}
	if !tr.TimedOut {
		t.Error("超时 hook 的 trace 应标记 TimedOut")
	}
	if tr.Err == "" {
		t.Error("超时 hook 的 trace 应带 Err")
	}
}

// TestHookTrace_Critical失败证据先于错误 钉住 R-H7 的硬约束：
// critical 失败时 trace 必须在 Execute 返回 error 之前就已投递。
// 这里在 Execute 返回后立刻读 recorder —— 若投递发生在 return 之后（例如另起
// goroutine），这条断言就会间歇性失败，因此能真正约束时序。
func TestHookTrace_Critical失败证据先于错误(t *testing.T) {
	rec := &traceRecorder{}
	ctx := WithHookTraceObserver(context.Background(), rec)

	c := NewHookChain()
	c.Register(newTraceHook("gate", 10, func(*HookContext) (*HookResult, error) {
		return nil, errors.New("gate failed")
	}))
	c.Register(newTraceHook("after", 20, func(*HookContext) (*HookResult, error) { return nil, nil }))

	if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx}); err == nil {
		t.Fatal("critical 失败应返回 error")
	}

	traces := rec.snapshot()
	tr, ok := traceByName(traces, "gate")
	if !ok {
		t.Fatal("critical 失败 hook 没有 trace 记录 —— 证据在 return 之后丢了")
	}
	if tr.Err == "" {
		t.Error("critical 失败 hook 的 trace 应带 Err")
	}
	after, ok := traceByName(traces, "after")
	if !ok {
		t.Fatal("未执行到的 hook 也应留下 trace")
	}
	if !after.Skipped {
		t.Error("critical 失败后未执行的 hook 应标记 Skipped")
	}
}

// TestHookError_Critical失败是typed_error 钉住 R-H7 的 typed error 契约：
// 终止原因可被 errors.As 读出，且底层错误链不被吞掉。
func TestHookError_Critical失败是typed_error(t *testing.T) {
	sentinel := errors.New("gate failed")
	c := NewHookChain()
	c.Register(newTraceHook("gate", 10, func(*HookContext) (*HookResult, error) {
		return nil, sentinel
	}))

	_, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()})
	var he *HookError
	if !errors.As(err, &he) {
		t.Fatalf("critical 失败应返回 *HookError, 实际 %T: %v", err, err)
	}
	if he.Hook != "gate" || he.Phase != PhasePreRequest || he.Criticality != Critical {
		t.Errorf("HookError 缺少定位字段: %+v", he)
	}
	if he.Panic != nil || he.TimedOut {
		t.Errorf("普通错误不应带 Panic/TimedOut: %+v", he)
	}
	if !errors.Is(err, sentinel) {
		t.Error("Unwrap 应保留底层错误链")
	}

	// panic 路径同样可区分。
	c2 := NewHookChain()
	c2.Register(newTraceHook("boom", 10, func(*HookContext) (*HookResult, error) { panic("boom") }))
	_, err2 := c2.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()})
	var he2 *HookError
	if !errors.As(err2, &he2) {
		t.Fatalf("critical panic 应返回 *HookError, 实际 %T: %v", err2, err2)
	}
	if he2.Panic != "boom" {
		t.Errorf("HookError.Panic 应为 boom, 实际 %#v", he2.Panic)
	}
	if !strings.Contains(err2.Error(), "panic") {
		t.Errorf("错误文本应可辨认为 panic, 实际 %q", err2.Error())
	}
}

// TestHookTrace_跳过也有记录 ReturnTerminal / SkipRemaining 两条终止路径都要
// 给后续 hook 留 Skipped 记录。
func TestHookTrace_跳过也有记录(t *testing.T) {
	cases := []struct {
		name string
		res  *HookResult
	}{
		{"return_terminal", &HookResult{ReturnTerminal: &types.Terminal{Reason: "stop"}}},
		{"skip_remaining", &HookResult{SkipRemaining: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &traceRecorder{}
			ctx := WithHookTraceObserver(context.Background(), rec)

			c := NewHookChain()
			c.Register(newTraceHook("stopper", 10, func(*HookContext) (*HookResult, error) {
				return tc.res, nil
			}))
			c.Register(newTraceHook("skipped1", 20, func(*HookContext) (*HookResult, error) {
				t.Error("被跳过的 hook 不应执行")
				return nil, nil
			}))
			c.Register(newTraceHook("skipped2", 30, func(*HookContext) (*HookResult, error) {
				t.Error("被跳过的 hook 不应执行")
				return nil, nil
			}))

			if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			traces := rec.snapshot()
			if len(traces) != 3 {
				t.Fatalf("应有 3 条 trace (1 执行 + 2 跳过), 实际 %d: %+v", len(traces), traces)
			}
			for _, name := range []string{"skipped1", "skipped2"} {
				tr, ok := traceByName(traces, name)
				if !ok {
					t.Fatalf("被跳过的 hook %s 没有 trace 记录", name)
				}
				if !tr.Skipped {
					t.Errorf("%s 的 trace 应标记 Skipped", name)
				}
				if tr.Result != "skipped" {
					t.Errorf("%s 的 Result 应为 skipped, 实际 %q", name, tr.Result)
				}
				if tr.Duration != 0 {
					t.Errorf("%s 未执行, Duration 应为 0, 实际 %v", name, tr.Duration)
				}
			}
			// 主动终止的 hook 自身不是 skipped。
			stopper, _ := traceByName(traces, "stopper")
			if stopper.Skipped {
				t.Error("主动终止的 hook 不应标记 Skipped")
			}
			if stopper.Result != tc.name {
				t.Errorf("终止 hook 的 Result 应为 %s, 实际 %q", tc.name, stopper.Result)
			}
		})
	}
}

// TestHookTrace_并发observer互不串记录 是 per-request 观测面的核心验收：
// 两个 goroutine 各用自己的 ctx observer、**共用同一条 HookChain** 各跑 100 次。
//
// 为什么必须共用链：如果观测者被装成 HookChain 的字段（或包级变量），两个 run
// 会互相覆盖/串记录；只有 ctx 方案才能让同一实例服务两个请求。每次 Execute 跑
// 两个 hook，因此每个 observer 应恰好收到 2×runs 条，且各 hook 各 runs 条 ——
// 字段实现的典型症状是一个 observer 收到 4×runs、另一个 0 条。
func TestHookTrace_并发observer互不串记录(t *testing.T) {
	const runs = 100
	recA, recB := &traceRecorder{}, &traceRecorder{}

	shared := NewHookChain()
	shared.Register(newTraceHook("A", 10, func(*HookContext) (*HookResult, error) { return nil, nil }))
	shared.Register(newTraceHook("B", 20, func(*HookContext) (*HookResult, error) { return nil, nil }))

	run := func(rec *traceRecorder) {
		ctx := WithHookTraceObserver(context.Background(), rec)
		for i := 0; i < runs; i++ {
			if _, err := shared.Execute(PhasePreRequest, &HookContext{Ctx: ctx}); err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run(recA) }()
	go func() { defer wg.Done(); run(recB) }()
	wg.Wait()

	for label, rec := range map[string]*traceRecorder{"A": recA, "B": recB} {
		traces := rec.snapshot()
		if len(traces) != 2*runs {
			t.Errorf("observer %s 应收到 %d 条, 实际 %d (观测者串台或被覆盖)",
				label, 2*runs, len(traces))
			continue
		}
		counts := map[string]int{}
		for _, tr := range traces {
			counts[tr.Name]++
		}
		if counts["A"] != runs || counts["B"] != runs {
			t.Errorf("observer %s 记录分布不符: %+v", label, counts)
		}
	}
}

// TestHookTrace_AdvisoryTimeout不泄漏goroutine：per-hook 超时不得靠"每 hook 一个
// 后台 goroutine"实现。跑 100 次超时后，goroutine 数应回落到基线附近。
func TestHookTrace_AdvisoryTimeout不泄漏goroutine(t *testing.T) {
	baseline := runtime.NumGoroutine()

	c := NewHookChain()
	slow := newTraceHook("slow", 10, func(hc *HookContext) (*HookResult, error) {
		<-hc.Ctx.Done()
		return nil, hc.Ctx.Err()
	})
	slow.crit = Advisory
	slow.timeout = time.Millisecond
	c.Register(slow)

	for i := 0; i < 100; i++ {
		if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()}); err != nil {
			t.Fatalf("第 %d 次 Execute: %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= baseline+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("100 次 advisory timeout 后 goroutine 未回落: got %d, baseline %d", got, baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHookTrace_同优先级按注册顺序且重复一致 钉住可复现性。
func TestHookTrace_同优先级按注册顺序且重复一致(t *testing.T) {
	want := []string{"first", "r1", "r2", "r3"}
	for iter := 0; iter < 20; iter++ {
		rec := &traceRecorder{}
		ctx := WithHookTraceObserver(context.Background(), rec)

		c := NewHookChain()
		// 先注册同优先级三个, 再注册一个更高优先级的 —— 高优先级应排到最前,
		// 同优先级的三个必须保持注册序。
		c.Register(newTraceHook("r1", 5, func(*HookContext) (*HookResult, error) { return nil, nil }))
		c.Register(newTraceHook("r2", 5, func(*HookContext) (*HookResult, error) { return nil, nil }))
		c.Register(newTraceHook("r3", 5, func(*HookContext) (*HookResult, error) { return nil, nil }))
		c.Register(newTraceHook("first", 1, func(*HookContext) (*HookResult, error) { return nil, nil }))

		if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx}); err != nil {
			t.Fatalf("iter %d Execute: %v", iter, err)
		}
		traces := rec.snapshot()
		if len(traces) != len(want) {
			t.Fatalf("iter %d trace 数不符: got %d want %d", iter, len(traces), len(want))
		}
		for i, name := range want {
			if traces[i].Name != name {
				t.Fatalf("iter %d 执行顺序不符: got %v want %v", iter,
					traceNames(traces), want)
			}
		}
	}
}

func traceNames(traces []HookTrace) []string {
	out := make([]string, 0, len(traces))
	for _, tr := range traces {
		out = append(out, tr.Name)
	}
	return out
}

// TestBusEvent带耗时与结果 验 Event 新增字段在 emit 处真的填上了 (只增不改)。
func TestBusEvent带耗时与结果(t *testing.T) {
	er := &eventRecorder{}
	ctx := WithObserver(context.Background(), er)

	c := NewHookChain()
	c.Register(newTraceHook("act", 10, func(*HookContext) (*HookResult, error) {
		return &HookResult{SkipStreamDelta: true}, nil
	}))

	if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: ctx, TurnCount: 4}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	evs := er.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应有 1 条事件, 实际 %d", len(evs))
	}
	ev := evs[0]
	if ev.Duration <= 0 {
		t.Errorf("Event.Duration 应 > 0, 实际 %v", ev.Duration)
	}
	if ev.Result != "stream_delta_skipped" {
		t.Errorf("Event.Result 应为 stream_delta_skipped, 实际 %q", ev.Result)
	}
	if ev.Panic || ev.TimedOut {
		t.Errorf("成功 hook 不应带 Panic/TimedOut: %+v", ev)
	}
	if ev.Err != "" {
		t.Errorf("成功 hook 的 Event.Err 应为空, 实际 %q", ev.Err)
	}
	if ev.TurnCount != 4 {
		t.Errorf("既有字段 TurnCount 语义被改动: %d", ev.TurnCount)
	}
}
