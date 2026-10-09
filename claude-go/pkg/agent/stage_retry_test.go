// stage_retry_test.go — H0-4 重试诊断的验收测试。
//
// 背景: run-mr-trading-v2-A00215179P 的三个阶段各自撞同一个 16 分钟硬超时、
// 各重试 3 次, 约 48 分钟墙钟花在重跑同一件注定失败的事上。原实现的重试循环
// 有退避但**没有诊断**: 不判断该不该重试, 也不在重试前改变任何状态。
//
// 本文件把诊断抽成了纯函数 classifyStageFailure, 因此每个分支都能被直接断言,
// 不需要起真实 run。这是"逐项落地 + 单元验收"边界的正确用法: 能证明机制通电,
// 不能证明长程成功率变好 (后者要 H4 场景台)。
package agent

import (
	"testing"
	"time"
)

// 判据 1: 同一失败原样重现在重试时**必须改变状态** (提升超时), 而不是原样重跑。
func TestH04_RepeatedSameFailureEscalates(t *testing.T) {
	// 实测里两次重试的错误文本 (时长不同, 但显然是同一个失败)。
	first := "agent execution timeout after 15m59.758822982s: context deadline exceeded"
	second := "agent execution timeout after 15m58.123456789s: context deadline exceeded"

	// 第一次失败: 无前序指纹 ⇒ 原样重试是合理的 (还不知道会不会复现)。
	action, sig1, _ := classifyStageFailure(first, "", false)
	if action != stageRetryAsIs {
		t.Fatalf("首次失败不应直接升级/放弃, got action=%v", action)
	}

	// 第二次: 同一指纹 ⇒ 必须升级 (改变状态), 不能原样重跑。
	action, sig2, why := classifyStageFailure(second, sig1, false)
	if sig1 != sig2 {
		t.Fatalf("时长不同的同一失败被当成两个不同失败: %q vs %q\n"+
			"这正是 48 分钟空转的成因 —— 原样重跑同一个注定失败的尝试", sig1, sig2)
	}
	if action != stageRetryEscalate {
		t.Fatalf("同一失败复现却未升级重试 (action=%v, why=%q)", action, why)
	}
}

// 判据 2: 升级过之后仍是同一失败 ⇒ 放弃, 不再重试。
//
// 不这么做的话, "提升超时"本身就是个无底洞: 每次翻倍、每次再撞。
func TestH04_EscalatedThenSameFailureAborts(t *testing.T) {
	errMsg := "agent execution timeout after 16m00.000000000s: context deadline exceeded"
	sig := stageFailureSignature(errMsg)

	action, _, why := classifyStageFailure(errMsg, sig, true)
	if action != stageRetryAbort {
		t.Fatalf("已升级过仍同一失败, 应当放弃 (action=%v, why=%q)", action, why)
	}
}

// 判据 3: 失败原因**变了** ⇒ 原样重试仍有意义 (不该被误判成"同一失败")。
func TestH04_DifferentFailureStillRetries(t *testing.T) {
	prev := stageFailureSignature("agent execution timeout after 15m59.0s: context deadline exceeded")
	cur := "connection reset by peer"

	action, _, _ := classifyStageFailure(cur, prev, true)
	if action != stageRetryAsIs {
		t.Fatalf("失败原因已变, 原样重试是合理的, got action=%v", action)
	}
}

// 判据 4: 不可重试的错误族立即放弃 —— 与引擎侧 ErrorClassifier 的
// BadRequest/Auth MaxAttempts=0 保持一致。
func TestH04_NonRetryableFamiliesAbortImmediately(t *testing.T) {
	cases := []struct {
		name string
		err  string
	}{
		{"400", "API error 400: invalid request body"},
		{"401", "401 unauthorized: bad api key"},
		{"403", "403 forbidden"},
		{"422", "422 unprocessable entity"},
		{"cancelled", "cancelled during retry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, _, why := classifyStageFailure(tc.err, "", false)
			if action != stageRetryAbort {
				t.Fatalf("%q 属于不可重试族, 应当立即放弃 (action=%v, why=%q)", tc.err, action, why)
			}
		})
	}
}

// 判据 5 (R2 守卫): 限流/过载等**可重试**族不得被误判成不可重试。
//
// 这条守的是"终止判据误杀"风险: 429 与 503 是暂时性的, 重试正是正确处置。
func TestH04_RetryableFamiliesAreNotAborted(t *testing.T) {
	for _, errMsg := range []string{
		"429 too many requests",
		"503 service unavailable",
		"LLM 熔断器开启: 连续多次失败, 30s 后重试",
		"rate limit exceeded",
	} {
		action, _, why := classifyStageFailure(errMsg, "", false)
		if action == stageRetryAbort {
			t.Fatalf("%q 是可重试族, 却被判放弃 (why=%q) —— 会误杀正常的长任务", errMsg, why)
		}
	}
}

// 判据 6: 指纹归一化必须吃掉数字与时长, 否则判据 1 不成立。
func TestH04_SignatureNormalizesNumbers(t *testing.T) {
	a := stageFailureSignature("agent execution timeout after 15m59.758822982s")
	b := stageFailureSignature("agent execution timeout after 16m01.234567890s")
	if a != b {
		t.Fatalf("不同数值的同一句式应归一到同一指纹:\n  %q\n  %q", a, b)
	}

	// 反面: 真正不同的错误不该被归一化到一起 (否则会误判成"同一失败"而放弃)。
	c := stageFailureSignature("connection reset by peer")
	if a == c {
		t.Fatal("语义不同的错误被归一化成同一指纹 —— 会导致误放弃")
	}
}

// 判据 7: 超时提升是单调的且有上界 (翻倍, 封顶 2×stageTimeout)。
func TestH04_EscalationDoublesAndCaps(t *testing.T) {
	got := stageRetryEscalation(stageTimeout)
	if got != stageTimeout*2 {
		t.Fatalf("提升后应为 %v, got %v", stageTimeout*2, got)
	}
	// 再提升一次不应超过封顶。
	capped := stageRetryEscalation(got)
	if capped > stageTimeout*2 {
		t.Fatalf("提升超过封顶 %v, got %v", stageTimeout*2, capped)
	}
	if capped != got {
		t.Fatalf("已达封顶时不应继续增长: %v -> %v", got, capped)
	}
	if got <= 0 {
		t.Fatalf("提升后超时必须为正, got %v", got)
	}
}

// 判据 8: 不同阶段的失败互不干扰 —— 指纹按阶段独立维护 (由调用方持有)。
//
// 这里断言的是函数的纯性: 同样输入永远同样输出, 不依赖任何包级可变状态。
// 若哪天有人在 classifyStageFailure 里塞了全局 map, 这条会因并行执行而失效。
func TestH04_ClassifyIsPure(t *testing.T) {
	errMsg := "agent execution timeout after 15m59.0s"
	a1, s1, w1 := classifyStageFailure(errMsg, "", false)
	b1, s2, w2 := classifyStageFailure(errMsg, "", false)
	if a1 != b1 || s1 != s2 || w1 != w2 {
		t.Fatal("classifyStageFailure 不是纯函数: 同输入给出不同输出")
	}

	// 两组独立状态 (模拟两个并发阶段) 不应互相污染。
	_, sigA, _ := classifyStageFailure("failure A", "", false)
	_, sigB, _ := classifyStageFailure("failure B", "", false)
	if sigA == sigB {
		t.Fatal("不同失败给出相同指纹")
	}
	if time.Duration(0) == stageTimeout {
		t.Skip("stageTimeout 为 0, 跳过")
	}
}
