// hookpolicy.go — hook 的 criticality / timeout / panic 语义（Moqi R-H7）。
//
// 改造前 HookChain.Execute 的形状是：顺序执行、无 per-hook timeout、无 panic
// recovery、任一 hook 返回 error 就中断整条链。对内部功能组件这是对的（Compact
// 失败继续跑没有意义）；对**业务增强**（检索、审计、指标）则不对 —— 用户会因为
// 一个可选 hook 挂了而这一轮无法回复。
//
// 这里的做法是**不改变既有默认**：没有实现 HookPolicy 的 hook 一律视为 Critical
// 且无 timeout，与改造前逐字节相同。只有显式声明 Advisory 的 hook 才获得"失败
// 降级继续"的语义。这样"安全 gate 失败即拒绝"与"检索增强失败即降级"能共存，
// 而存量 hook 的行为不受影响。
package internal_hook

import (
	"context"
	"fmt"
	"time"
)

// Criticality 决定 hook 失败时的后果。
type Criticality int

const (
	// Critical 失败即终止本轮（改造前的行为，也是所有存量 hook 的默认）。
	Critical Criticality = iota
	// Advisory 失败只记录、继续执行后续 hook。
	Advisory
)

// String 返回可读名（进 trace 用）。
func (c Criticality) String() string {
	if c == Advisory {
		return "advisory"
	}
	return "critical"
}

// HookPolicy 是 hook 可选实现的失败语义声明。
//
// 不实现它的 hook 得到 Criticality=Critical、Timeout=0（不限时），即改造前行为。
type HookPolicy interface {
	// Criticality 返回该 hook 失败时的后果。零值 Critical。
	Criticality() Criticality
	// Timeout 返回单次执行的超时；0 = 不限时（改造前行为）。
	Timeout() time.Duration
}

// HookOutcome 是单个 hook 一次执行的观测结果，供 trace/metrics 使用。
//
// 这是本次改造新增的可观测面：改造前 hook 的耗时与失败原因不出链。
type HookOutcome struct {
	Name        string
	Phase       InternalHookPhase
	Criticality Criticality
	// Duration 是该 hook 自身的执行耗时（不含被跳过的时间）。
	Duration time.Duration
	// Err 非 nil 表示该 hook 失败；Advisory 失败时链继续。
	Err error
	// Panic 非 nil 表示该 hook panic 已被 recover。
	Panic any
	// TimedOut 表示该 hook 因 Timeout 被中止。
	TimedOut bool
	// Skipped 表示因前序 hook 的 ReturnTerminal/SkipRemaining 而没有执行。
	Skipped bool
}

// Degraded 报告该 hook 是否属于"失败但链继续"（advisory 失败/超时/panic）。
func (o HookOutcome) Degraded() bool {
	return o.Criticality == Advisory && (o.Err != nil || o.Panic != nil || o.TimedOut)
}

// HookObserver 接收每次 hook 执行的观测结果。
//
// 用接口而不是回调字段：装配方可以同时挂多个观察者（trace + metrics），
// 且 nil 观察者退化为零开销。
type HookObserver interface {
	OnHookOutcome(o HookOutcome)
}

// HookTrace 是单个 hook 一次执行/跳过的完整 trace 记录（R-H7）。
//
// 与 HookOutcome 的分工：HookOutcome 挂在 HookChain 字段上（构造期装配、全局），
// 而 HookTrace 走 ctx（per-request）—— 多个 run 并发共用同一个 HookChain 时，
// 全局观察者无法区分"这条记录属于哪一轮"。ctx 天然是 per-request 的，因此
// 并发 run 不会串台。
//
// Err/Panic 是字符串而不是 error/any：trace 最终要落库/进日志，序列化时
// error 的形态不稳定；且不应把可能含用户正文的 error 链带出去（只取 message）。
type HookTrace struct {
	Name        string
	Phase       InternalHookPhase
	Criticality Criticality
	// Duration 是该 hook 自身的执行耗时；Skipped 时为 0。
	Duration time.Duration
	// Err 失败 hook 的错误文本；成功为空。
	Err string
	// Panic 是 recover 到的 panic 内容文本；未 panic 为空。
	Panic string
	// TimedOut 表示该 hook 因 Timeout 被中止。
	TimedOut bool
	// Skipped 表示因前序 hook 的 ReturnTerminal/SkipRemaining/失败而未执行。
	Skipped bool
	// Result 是 HookResult 的**归纳标签**（见 hookResultSummary），不含用户正文。
	// 失败 hook 为空；被跳过的 hook 为 "skipped"。
	Result string
}

// HookTraceObserver 接收每次 hook 执行的 trace。
//
// 由 ctx 携带（WithHookTraceObserver），因此是 per-request 的。实现方必须
// 自行保证并发安全：同一个 observer 可能被同一轮的多个 phase 调用，也可能被
// 并行工具路径调用。ObserveHookTrace 的 panic 会被调用方 recover，不影响本轮。
type HookTraceObserver interface {
	ObserveHookTrace(context.Context, HookTrace)
}

// hookResultSummary 把 HookResult 归纳成一个稳定的短标签。
//
// 只输出**类别**，不输出任何用户正文：trace 会进运维日志，正文属于用户数据。
// 优先级固定（终止 > 截断 > 消息 > 提示词 > 其余），同一份结果每次得到同一个
// 标签，便于聚合与 diff。
func hookResultSummary(r *HookResult) string {
	if r == nil {
		return "no_op"
	}
	switch {
	case r.ReturnTerminal != nil:
		return "return_terminal"
	case r.SkipRemaining:
		return "skip_remaining"
	case r.Messages != nil || len(r.AppendMsgs) > 0:
		return "messages_injected"
	case r.SystemPrompt != nil:
		return "prompt_modified"
	case r.ToolUseBlocks != nil || r.AssistantBlocks != nil:
		return "blocks_modified"
	case r.SkipStreamDelta:
		return "stream_delta_skipped"
	case r.InjectContinue || r.ContinueMsg != nil:
		return "continue_injected"
	case len(r.StreamEvents) > 0:
		return "stream_events"
	case r.Backoff > 0:
		return "backoff"
	default:
		return "no_op"
	}
}

// hookTraceResult 决定一条 trace 的 Result 标签。
//
// 失败 hook 的 r 恒为 nil（executeIsolated 在错误/panic 时不返回结果），
// 这里不能把它记成 "no_op" —— 那会让"失败了"和"什么都没做"在 trace 里同形。
func hookTraceResult(o HookOutcome, r *HookResult) string {
	if o.Skipped {
		return "skipped"
	}
	if o.Err != nil || o.Panic != nil {
		return ""
	}
	return hookResultSummary(r)
}

// trace 把一次 HookOutcome 转成 trace 记录。
func (o HookOutcome) trace(result string) HookTrace {
	tr := HookTrace{
		Name:        o.Name,
		Phase:       o.Phase,
		Criticality: o.Criticality,
		Duration:    o.Duration,
		TimedOut:    o.TimedOut,
		Skipped:     o.Skipped,
		Result:      result,
	}
	if o.Err != nil {
		tr.Err = o.Err.Error()
	}
	if o.Panic != nil {
		tr.Panic = fmt.Sprintf("%v", o.Panic)
	}
	return tr
}

// traceSkippedHooks 为没有执行到的 hook 补 trace 记录。
//
// 链在中途终止（critical 失败 / ReturnTerminal / SkipRemaining）时，后面的 hook
// 不是"不存在"，而是"被跳过了"。两者在 trace 里必须可区分：只有后者带
// Skipped=true。Duration 为 0 是事实（没执行），不是缺失。
func traceSkippedHooks(ctx context.Context, obs HookTraceObserver, hooks []InternalHook, phase InternalHookPhase) {
	if obs == nil || len(hooks) == 0 {
		return
	}
	for _, h := range hooks {
		crit, _ := hookPolicyOf(h)
		out := HookOutcome{Name: h.Name(), Phase: phase, Criticality: crit, Skipped: true}
		deliverHookTrace(ctx, obs, out.trace("skipped"))
	}
}

// HookError 是 Critical hook 终止本轮时返回的 typed error（R-H7）。
//
// 为什么必须是类型而不是 fmt.Errorf 字符串：调用方要能回答"这一轮为什么停"——
// 是哪个 hook、是失败还是 panic、有没有超时、底层错误能不能 errors.Is。解析错误
// 字符串做不到这些（而字符串是给人看的，不能当契约）。
//
// Error() 的文本与改造前逐字一致，Unwrap 保留底层错误链：
// 既有按字符串/errors.Is 判断的调用方行为不变。
type HookError struct {
	Hook        string
	Phase       InternalHookPhase
	Criticality Criticality
	// TimedOut 表示失败由 per-hook timeout 触发。
	TimedOut bool
	// Panic 是 recover 到的 panic 内容；nil 表示普通错误。
	Panic any
	// Err 是该 hook 返回的错误（panic 时是合成的 "panic: ..."）。
	Err error
}

// Error 返回与改造前一致的错误文本。
func (e *HookError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Panic != nil {
		return fmt.Sprintf("internal hook %s panic: %v", e.Hook, e.Panic)
	}
	return fmt.Sprintf("internal hook %s: %v", e.Hook, e.Err)
}

// Unwrap 保留底层错误链（panic 时指向合成的 panic 错误）。
func (e *HookError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// hookPolicyOf 取 hook 的策略，未实现 HookPolicy 时返回改造前的默认值。
func hookPolicyOf(h InternalHook) (Criticality, time.Duration) {
	if p, ok := h.(HookPolicy); ok {
		return p.Criticality(), p.Timeout()
	}
	return Critical, 0
}

// executeIsolated 执行单个 hook，带 panic recovery 与可选超时。
//
// 三个必须成立的性质：
//  1. panic 不穿出本函数 —— 一个业务 hook 的空指针不能杀掉整轮；
//  2. 超时用独立的 child context，不能取消调用方的 ctx（那会让本轮其他组件全部
//     收到取消，表现为"一个 advisory hook 超时导致整轮失败"，与目标相反）；
//  3. 返回的 HookOutcome 恒有值，即使 panic —— 否则 trace 里这一条会消失。
func executeIsolated(hook InternalHook, phase InternalHookPhase, hctx *HookContext) (r *HookResult, out HookOutcome) {
	crit, timeout := hookPolicyOf(hook)
	out = HookOutcome{Name: hook.Name(), Phase: phase, Criticality: crit}

	start := time.Now()
	defer func() { out.Duration = time.Since(start) }()

	execCtx := hctx
	if timeout > 0 {
		base := context.Background()
		if hctx != nil && hctx.Ctx != nil {
			base = hctx.Ctx
		}
		tctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		// 浅拷贝 HookContext 再换 Ctx：hook 之间共享的跨 phase 状态指针不变，
		// 但本 hook 看到的是带超时的 ctx。不能改原对象——那是链上共享的。
		cp := *hctx
		cp.Ctx = tctx
		execCtx = &cp
	}

	// panic recovery 必须和 Execute 在**同一个 goroutine**：超时路径下 Execute
	// 在子 goroutine 里跑，若 recover 留在这里（调用方 goroutine），一次 panic
	// 就会掀掉整个进程。
	run := func() (res *HookResult, err error, panicked any) {
		defer func() {
			if p := recover(); p != nil {
				panicked = p
				// panic 对 Critical hook 等价于 error：转成错误向上返回，
				// 由调用方按 criticality 决定终止还是继续。
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		res, err = hook.Execute(execCtx)
		return res, err, nil
	}

	if timeout <= 0 {
		res, err, panicked := run()
		out.Panic, out.Err = panicked, err
		if err != nil {
			return nil, out
		}
		return res, out
	}

	// 硬超时：hook 不看 ctx 时也必须按时返回。
	//
	// 只传 ctx 而不另起 goroutine 不是"独立 timeout"，只是"告诉 hook 该结束了"：
	// 一个 sleep/阻塞读的实现会照样把整轮拖住，而 trace 里连 TimedOut 都不会有
	// （R-H7 验收第 2 条的失败形态）。代价是超时后子 goroutine 会运行到 hook
	// 自己返回为止；hook 契约要求它在 ctx 取消后尽快退出，违反者的残留 goroutine
	// 记在 hook 自己的账上，不能变成整轮的无界等待。
	//
	// 子 goroutine 只写自己的局部变量，并通过**带缓冲**的 channel 交回结果：
	// 超时返回后即使 hook 稍后才结束，也不会阻塞、不会泄漏 channel。
	done := make(chan hookExecResult, 1)
	go func() {
		res, err, panicked := run()
		done <- hookExecResult{res: res, err: err, panicked: panicked}
	}()
	tctx := execCtx.Ctx
	select {
	case got := <-done:
		out.Panic, out.Err = got.panicked, got.err
		if got.err == nil {
			return got.res, out
		}
		if tctx.Err() == context.DeadlineExceeded {
			out.TimedOut = true
		}
		return nil, out
	case <-tctx.Done():
		out.TimedOut = true
		out.Err = fmt.Errorf("hook %s: %w", hook.Name(), tctx.Err())
		return nil, out
	}
}

// hookExecResult 是超时路径下子 goroutine 交回的结果。
type hookExecResult struct {
	res      *HookResult
	err      error
	panicked any
}

// allowsContinue 判断该 outcome 是否允许链继续。
func (o HookOutcome) allowsContinue() bool {
	return o.Criticality == Advisory
}
