// hook_error_termination_test.go — H0-2 终止判据通电的验收测试。
//
// 背景: 运行态实测 (210 trace / 209 run) 显示 54 个 failed run 中
// **由系统判据主动终止的 = 0** —— 18 个人工叫停, 36 个重试耗尽。
// 同时 trace 里有 8738 条 "LLM 熔断器开启" 事件: 系统识别到了连续失败,
// 但只做退避, 从不终止。
//
// 三条判据在源码里都存在, 却因不同原因全部失效:
//   1. CircuitBreakerHook  被 hook_error.go 的短路架空 (分类器在场即 return nil)
//   2. ErrorClassifier     族计数在每次成功响应后被 ResetAll 清空 ⇒ 只抓得住
//                          "不中断的连续失败", 抓不住交错失败
//   3. StopSignal/LoopDetector 从不产生 ReturnTerminal, 只注入软提示
//
// 本文件的判据全部是否定式的: 断言的是"在某种输入下**不可能**继续跑",
// 而不是"某个字段被赋值了"。
package internal_hook

import (
	"errors"
	"testing"
)

// newRunErrChain 构造与 engine 相同的 OnError 链: 分类器(110) → 熔断器(120)。
func newRunErrChain(clf *ErrorClassifier) *HookChain {
	chain := NewHookChain()
	chain.Register(NewErrorClassifierHook(clf, nil, nil))
	chain.Register(NewCircuitBreakerHook(clf, nil))
	return chain
}

// 基线判据: 不中断的连续同因失败, 分类器会耗尽族预算并终止。
//
// 这条**当前就是通过的** —— 它存在是为了把"分类器有效"的那一半钉住,
// 防止修 H0-2 时把这里改坏 (改坏的表现是它开始 Fatal)。
func TestBaseline_UninterruptedStreakTerminates(t *testing.T) {
	clf := NewErrorClassifier()
	chain := newRunErrChain(clf)
	err := errors.New("connection reset by peer")

	for i := 1; i <= 30; i++ {
		res, _ := chain.Execute(PhaseOnError, &HookContext{
			Phase:             PhaseOnError,
			Error:             err,
			ConsecutiveErrors: i,
		})
		if res != nil && res.ReturnTerminal != nil {
			if res.ReturnTerminal.Reason == "" {
				t.Fatal("终止理由为空: 无法区分是新判据终止的还是别的原因")
			}
			t.Logf("基线: 第 %d 次同因失败后终止, reason=%s", i, res.ReturnTerminal.Reason)
			return
		}
	}
	t.Fatal("连续 30 次同因失败仍未终止 —— 分类器基线能力已损坏")
}

// 判据 1 (H0-2a): 分类器在场时, CircuitBreakerHook 不得被短路。
//
// 现状: Execute 开头 `if h.classifier != nil { return nil, nil }` ——
// 只要分类器被注册 (生产默认), 熔断器就是一段死代码, 达到阈值也不终止。
func TestH02_CircuitBreakerNotDisabledByClassifier(t *testing.T) {
	clf := NewErrorClassifier()
	cb := NewCircuitBreakerHook(clf, nil)

	res, err := cb.Execute(&HookContext{
		Phase:             PhaseOnError,
		Error:             errors.New("boom"),
		ConsecutiveErrors: MaxConsecutiveErrors,
	})
	if err != nil {
		t.Fatalf("hook 返回错误: %v", err)
	}
	if res == nil || res.ReturnTerminal == nil {
		t.Fatalf("连续错误达到阈值 %d 但熔断器未返回 Terminal "+
			"(分类器在场时被短路, 该 hook 实为死代码)", MaxConsecutiveErrors)
	}
	if res.ReturnTerminal.Reason != "circuit_breaker" {
		t.Fatalf("终止理由 = %q, 期望 circuit_breaker", res.ReturnTerminal.Reason)
	}
}

// 判据 2 (H0-2b): 交错失败 —— 即"失败之间夹着成功响应"的 run —— 也必须终止。
//
// 现状: engine.go 每次成功响应调用 ResetAll(), 把族计数清零。于是
// fail→success→fail→success 的 run 永远到不了族预算, 无限重试直到 maxTurns。
// 这正是 trace 里 8738 条熔断事件的形态。
//
// 修法要点: 需要一个**不随单次成功清零**的 run 级错误账本, 只在真正有进展
// (NoteProgress) 时才推进基线。本测试不调用 NoteProgress, 代表"零进展"。
func TestH02_InterleavedErrorsMustTerminate(t *testing.T) {
	clf := NewErrorClassifier()
	chain := newRunErrChain(clf)
	// 熔断器开启时上游回的就是这条; 分类器把它归到 unknown (预算 2)。
	err := errors.New("LLM 熔断器开启: 连续多次失败, 30s 后重试")

	for i := 1; i <= 60; i++ {
		res, _ := chain.Execute(PhaseOnError, &HookContext{
			Phase:             PhaseOnError,
			Error:             err,
			ConsecutiveErrors: 1,
		})
		if res != nil && res.ReturnTerminal != nil {
			t.Logf("交错失败第 %d 次后终止, reason=%s", i, res.ReturnTerminal.Reason)
			return
		}
		// 精确模拟 engine.go 成功分支: 一次成功响应把族计数清零。
		clf.ResetAll()
	}
	t.Fatal("交错失败 60 轮仍未终止 —— 该 run 会一直空转到 maxTurns " +
		"(实测 209 个 run 中系统判据终止数为 0)")
}

// 判据 3 (H0-2b 反例): 有进展时 run 级账本必须推进, 不能把正常长任务误杀。
//
// 这是 R2 风险 (终止判据误杀) 的守卫: 一个每隔几次失败就有真实进展的 run,
// 必须能一直跑下去。若实现成"只按累计错误数终止", 这条会 Fatal。
func TestH02_ProgressResetsRunErrorLedger(t *testing.T) {
	clf := NewErrorClassifier()
	chain := newRunErrChain(clf)
	err := errors.New("LLM 熔断器开启: 连续多次失败, 30s 后重试")

	const rounds = 200
	for i := 1; i <= rounds; i++ {
		res, _ := chain.Execute(PhaseOnError, &HookContext{
			Phase:             PhaseOnError,
			Error:             err,
			ConsecutiveErrors: 1,
		})
		if res != nil && res.ReturnTerminal != nil {
			t.Fatalf("有持续进展的 run 在第 %d 轮被误杀, reason=%s "+
				"(R2: 终止判据不得误杀正常长任务)", i, res.ReturnTerminal.Reason)
		}
		clf.ResetAll()
		// 每轮都以"有真实进展"收尾 (例如工具调用成功产出)。
		clf.NoteProgress()
	}
	t.Logf("有进展的 run 跑满 %d 轮未被误杀", rounds)
}
