// hook_trace_test.go — R-H7 在 Harness 侧的接线验收。
//
// 引擎侧已经证明了"每个 hook 都会产生 HookTrace"；这里只验 Harness 真的把观测者
// 装进了**本轮 ctx** 并把记录写进 TurnTrace。判据不看日志，只看 TurnTrace：
//   - 正常 hook 有 Name/Phase/Duration/Result；
//   - advisory panic → Panic 字段有值 + Degraded 含 hook_panic，且本轮照常完成；
//   - advisory timeout → TimedOut + Degraded 含 hook_timeout，且本轮照常完成。
//
// 后两条同时是 Q-H3 的对照：同样是失败，advisory 不能把一轮变成错误。
package harness

import (
	"context"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
)

// probeHook 是一个最小可注入 hook：可配置耗时/panic/超时。
type probeHook struct {
	*internal_hook.BaseInternalHook
	sleep     time.Duration
	panicWith string
	timeout   time.Duration
}

func (h *probeHook) Execute(_ *internal_hook.HookContext) (*internal_hook.HookResult, error) {
	if h.sleep > 0 {
		time.Sleep(h.sleep)
	}
	if h.panicWith != "" {
		panic(h.panicWith)
	}
	return nil, nil
}

func (h *probeHook) Criticality() internal_hook.Criticality { return internal_hook.Advisory }
func (h *probeHook) Timeout() time.Duration                 { return h.timeout }

func newProbe(name string, sleep time.Duration, panicWith string, timeout time.Duration) *probeHook {
	return &probeHook{
		BaseInternalHook: &internal_hook.BaseInternalHook{
			Name_:     name,
			Priority_: 9999, // 排在内置 hook 之后，避免改变既有合并顺序
			Phases_:   []internal_hook.InternalHookPhase{internal_hook.PhasePreRequest},
		},
		sleep:     sleep,
		panicWith: panicWith,
		timeout:   timeout,
	}
}

// runWithProbe 起一轮带注入 hook 的真实 RunTurn，返回 trace 与结果。
func runWithProbe(t *testing.T, h *probeHook) TurnTrace {
	t.Helper()
	up := &runnerUpstream{text: "照常回答"}
	trace := &traceRecorder{}
	var eng *engine.QueryEngine
	r := newTestRunner(t, up, 3, nil, func(o *RunnerOptions) {
		o.Trace = trace
		eng = o.Engine
	})
	if eng == nil || eng.HookChain == nil {
		t.Fatal("测试引擎没有 HookChain，无法注入探针")
	}
	eng.HookChain.Register(h)

	if _, err := r.RunTurn(context.Background(), textTurn("s-hook", "t-"+h.Name(), "在吗"), nil); err != nil {
		t.Fatalf("advisory hook 不应让本轮失败: %v", err)
	}
	tr, ok := trace.last()
	if !ok {
		t.Fatal("没有收到 trace")
	}
	return tr
}

func findHook(tr TurnTrace, name string) (HookRecord, bool) {
	for _, h := range tr.Hooks {
		if h.Name == name {
			return h, true
		}
	}
	return HookRecord{}, false
}

// TestRunTurn_HookTraceRecordsDurationAndResult 钉住正常 hook 的记录：
// 名字、阶段、耗时、结果标签四样都要在 trace 里。
func TestRunTurn_HookTraceRecordsDurationAndResult(t *testing.T) {
	tr := runWithProbe(t, newProbe("ProbeSlow", 4*time.Millisecond, "", 0))
	rec, ok := findHook(tr, "ProbeSlow")
	if !ok {
		t.Fatalf("trace 缺 ProbeSlow 记录: %+v", tr.Hooks)
	}
	if rec.Phase != internal_hook.PhasePreRequest.String() {
		t.Fatalf("阶段错误: %q", rec.Phase)
	}
	if rec.Criticality != "advisory" {
		t.Fatalf("失败语义错误: %q", rec.Criticality)
	}
	if rec.DurationMS < 2.0 {
		t.Fatalf("耗时未被测量: %.3fms（hook 实际睡 4ms）", rec.DurationMS)
	}
	if rec.Result != "no_op" {
		t.Fatalf("结果标签错误: %q", rec.Result)
	}
	// 内置 hook 也必须在：只记注入探针等于观测面没接上。
	if len(tr.Hooks) < 2 {
		t.Fatalf("trace 只有 %d 条 hook 记录，内置 hook 未上报", len(tr.Hooks))
	}
}

// TestRunTurn_HookPanicIsRecordedAndDegradedButTurnCompletes 钉住 advisory panic：
// panic 文本进 trace，Degraded 有 hook_panic，本轮 completed。
func TestRunTurn_HookPanicIsRecordedAndDegradedButTurnCompletes(t *testing.T) {
	tr := runWithProbe(t, newProbe("ProbePanic", 0, "boom-canary", 0))
	rec, ok := findHook(tr, "ProbePanic")
	if !ok {
		t.Fatalf("trace 缺 ProbePanic 记录: %+v", tr.Hooks)
	}
	if rec.Panic != "boom-canary" {
		t.Fatalf("panic 未记录: %+v", rec)
	}
	if rec.Result != "" {
		t.Fatalf("失败 hook 不应有结果标签（会与 no_op 混淆）: %q", rec.Result)
	}
	if !containsString(tr.Degraded, "hook_panic") {
		t.Fatalf("Degraded 缺 hook_panic: %v", tr.Degraded)
	}
	if tr.Terminal.Reason != "completed" {
		t.Fatalf("advisory panic 不应终止本轮: %q", tr.Terminal.Reason)
	}
}

// TestRunTurn_HookTimeoutIsRecordedAndDegradedButTurnCompletes 钉住 advisory 超时：
// TimedOut 进 trace，Degraded 有 hook_timeout，本轮 completed，且总耗时受超时约束。
func TestRunTurn_HookTimeoutIsRecordedAndDegradedButTurnCompletes(t *testing.T) {
	start := time.Now()
	tr := runWithProbe(t, newProbe("ProbeTimeout", 200*time.Millisecond, "", 5*time.Millisecond))
	elapsed := time.Since(start)

	rec, ok := findHook(tr, "ProbeTimeout")
	if !ok {
		t.Fatalf("trace 缺 ProbeTimeout 记录: %+v", tr.Hooks)
	}
	if !rec.TimedOut {
		t.Fatalf("超时未标记: %+v", rec)
	}
	if rec.DurationMS < 4.0 || rec.DurationMS > 100.0 {
		t.Fatalf("超时耗时不对: %.3fms（期望约 5ms）", rec.DurationMS)
	}
	if !containsString(tr.Degraded, "hook_timeout") {
		t.Fatalf("Degraded 缺 hook_timeout: %v", tr.Degraded)
	}
	if tr.Terminal.Reason != "completed" {
		t.Fatalf("advisory 超时不应终止本轮: %q", tr.Terminal.Reason)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("本轮被超时 hook 拖住: %s（hook 睡 200ms，超时 5ms）", elapsed)
	}
}
