// maxturns_test.go —— H4-3 落在 **CLI 路径**上的一半验收。
//
// 为什么 CLI 路径要单独一组: 它的"改造前行为"与团队路径**不同** ——
// cmd/claude-go/main.go 里写死的是 150 (团队侧是 clampTurns 的 40), 所以
// "未配置 = 改造前" 这条纪律在两条路径上要各自对各自的旧值负责。把那两个数
// 混成一份共享默认值, 会让其中一条路径的止损线悄悄搬家。
//
// 另外这里盖住一处**既有的**缺陷: 改造前 `maxTurns := 150` 无条件赋值, 之后的
// 代码再也没有读过 r.modelCfg.MaxTurns —— 模型配置里配的 maxTurns 在这条路径上
// 完全不起作用 (kimi 系配的 40 也被一起丢掉)。现在它原样优先。
package main

import "testing"

// TestH43_CLI上限优先级 判定顺序的回归臂。
func TestH43_CLI上限优先级(t *testing.T) {
	t.Run("思考型恒为 1", func(t *testing.T) {
		// 即使显式配了 500, 思考型也是 1 —— 它同时关掉了工具循环,
		// 多给轮数不会让它多做事, 只会让一个卡住的 run 多空转。
		if got := cliStageMaxTurns("architect", 500, 150); got != 1 {
			t.Fatalf("思考型上限 = %d, 应为 1", got)
		}
		if got := cliStageMaxTurns("Architect", 0, 150); got != 1 {
			t.Fatalf("大小写不该改变判定, 实得 %d", got)
		}
	})

	t.Run("模型级声明原样优先", func(t *testing.T) {
		// 这条盖的是改造前的缺陷: 当时模型级 maxTurns 被无条件忽略。
		if got := cliStageMaxTurns("coder", 40, 150); got != 40 {
			t.Fatalf("模型级 40 被改成 %d —— 改造前这里是恒 150, "+
				"模型配置里写的 maxTurns 在这条路径上从来不起作用", got)
		}
	})

	t.Run("未声明时用长程档", func(t *testing.T) {
		if got := cliStageMaxTurns("coder", 0, 150); got != 150 {
			t.Fatalf("未声明模型级时上限 = %d, 应为长程档 150", got)
		}
		if got := cliStageMaxTurns("coder", 0, 300); got != 300 {
			t.Fatalf("长程档配了 300, 实得 %d", got)
		}
	})

	t.Run("做事型角色不被当成思考型", func(t *testing.T) {
		for _, role := range []string{"coder", "tester", "reviewer", "coordinator", "world-builder"} {
			if got := cliStageMaxTurns(role, 0, 150); got != 150 {
				t.Fatalf("role=%q 上限 = %d, 应为 150 (它要开工具循环跑长程)", role, got)
			}
		}
	})
}

// TestH43_CLI未配置时逐字回落到改造前的150 最重要的回归臂。
//
// 改造前这条路径的硬编码值就是 150。ai 段没写 longHorizonMaxTurns 时,
// 有效值必须仍是 150 —— 否则这次改动会在没人配置的情况下改掉 CLI 上每一个
// 做事型 stage 的止损线。
func TestH43_CLI未配置时逐字回落到改造前的150(t *testing.T) {
	if got := longHorizonMaxTurnsOr(0); got != 150 {
		t.Fatalf("未配置时长程档 = %d, 应为改造前的硬编码值 150", got)
	}
	// 反面: 配了就听配置的。
	if got := longHorizonMaxTurnsOr(300); got != 300 {
		t.Fatalf("配置的 300 被改成了 %d", got)
	}
	// 负值 (解析事故) 也走兜底, 不产生一个"比 1 还小"的上限。
	if got := longHorizonMaxTurnsOr(-1); got != 150 {
		t.Fatalf("负值应走兜底 150, 实得 %d", got)
	}
}

// TestH43_CLI的长程档确实过了100 验收前提 (方案 §21.5.6)。
//
// 团队侧那条判据由 pkg/feishu 的用例盖; 这里盖 CLI 侧 —— 两条路径各有一个
// 默认值, 只判其中一条会让另一条的回归无人看着。
func TestH43_CLI的长程档确实过了100(t *testing.T) {
	if got := longHorizonMaxTurnsOr(0); got <= 100 {
		t.Fatalf("CLI 未配置时的长程档 = %d, 必须 > 100", got)
	}
}
