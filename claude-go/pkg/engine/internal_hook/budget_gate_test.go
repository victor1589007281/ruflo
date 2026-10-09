// budget_gate_test.go — P0-1 / H0-5 run 级预算闸门的验收测试。
//
// 起因: QueryEngine.CumulativeUsage 只累加、从不与任何上限比较 —— 全仓检索确认它
// 是"只写不读"的结构体 (唯一的读者 GetContextUsage 算的是单次请求的窗口占用, 不是
// 累计成本)。于是当前唯一的预算是 maxTurns, 而那是**步数**上限不是成本上限:
// 一次 WebFetch 与一次 Read 都算 1 轮, 成本可能差三个数量级。
//
// 验收口径按 21.5.1 的否定式写: 断言"在 Y 条件下 X 不可能发生", 且判据落在运行态
// (Spend 的返回值 / Exhausted 读数), 不落在装配态 (对象存在、字段非空)。
//
// 这一条钉的是**终止判据本身**, 不是它被接在哪儿: 接线的验收在 engine 侧
// (budget_gate_terminal_test.go), 那条才证得了"run 的终态原因是 budget_exhausted"。
// 两条分开是刻意的 —— 判据错了和判据没通电是两种缺陷, 混在一条测试里无法区分。
package internal_hook

import "testing"

// 判据 1: 未设上限 (<=0) 时永不触发 —— 默认行为不变, 不影响现有运行。
//
// 同时钉住"不设上限"与"没在记账"的可区分性: Spent 必须照常累加并报出来。
func TestH05_ZeroLimitNeverTrips(t *testing.T) {
	g := NewRunBudgetGate(0)
	for i := 0; i < 100; i++ {
		if spent, trip := g.Spend(1_000_000); trip {
			t.Fatalf("未设上限却在第 %d 次调用触发 (spent=%d)", i, spent)
		}
	}
	if g.Exhausted() {
		t.Fatal("未设上限不应进入耗尽态")
	}
	if got := g.Spent(); got != 100_000_000 {
		t.Fatalf("不设上限时 Spent() = %d, 期望 100000000 —— '没设上限'不等于'没在记账'", got)
	}
}

// 判据 2 (主判据): 累计消耗超过上限时**必须**触发, 且给出实际消耗量。
func TestH05_ExceedingLimitTrips(t *testing.T) {
	g := NewRunBudgetGate(1000)

	if _, trip := g.Spend(400); trip {
		t.Fatal("400/1000 不应触发")
	}
	if _, trip := g.Spend(400); trip {
		t.Fatal("800/1000 不应触发")
	}
	spent, trip := g.Spend(400)
	if !trip {
		t.Fatal("1200/1000 必须触发 —— 这正是 CumulativeUsage 只累加不比较时缺的行为")
	}
	if spent != 1200 {
		t.Fatalf("spent = %d, 期望 1200 (要能报出超了多少)", spent)
	}
	if !g.Exhausted() {
		t.Fatal("触发后必须进入耗尽态, 供后续调用快速短路")
	}
}

// 判据 3: 恰好等于上限**不**触发 (边界不含), 超过才触发。
func TestH05_ExactLimitDoesNotTrip(t *testing.T) {
	g := NewRunBudgetGate(1000)
	if _, trip := g.Spend(1000); trip {
		t.Fatal("恰好用满上限不应触发 —— 上限是包含的")
	}
	if _, trip := g.Spend(1); !trip {
		t.Fatal("超出 1 个 token 就该触发")
	}
}

// 判据 4: 一旦耗尽, 后续调用一律短路为耗尽 —— 不再需要重复比较。
//
// 这保证闸门在长跑里是 O(1) 的, 且"触发"是单调的 (不会因为某次调用没消耗就恢复)。
func TestH05_ExhaustedIsSticky(t *testing.T) {
	g := NewRunBudgetGate(100)
	g.Spend(200)

	// 即使后续调用"零消耗", 也不该把耗尽态撤销 —— 预算是 run 级、单调的。
	for i := 0; i < 5; i++ {
		if _, trip := g.Spend(0); !trip {
			t.Fatalf("耗尽态在第 %d 次零消耗调用后被撤销了", i)
		}
	}
	if !g.Exhausted() {
		t.Fatal("耗尽态必须是单调的")
	}
}

// 判据 5: 负消耗 (畸形输入) 不得把累计值拉回上限之下。
//
// 上游返回异常用量 (负数) 时, 朴素累加会让 spent 回退, 闸门就永远触发不了 ——
// 这是"预算闸门被一个坏数据点架空"的经典形态。
func TestH05_NegativeSpendCannotUnwind(t *testing.T) {
	g := NewRunBudgetGate(1000)
	g.Spend(600)
	g.Spend(-10_000) // 畸形: 负用量
	if _, trip := g.Spend(401); !trip {
		// 负值若被累加, spent = 600-10000+401 = -8999 < 1000 ⇒ 不触发。
		t.Fatal("负消耗把累计值拉回了上限之下 —— 闸门被畸形数据架空")
	}
}

// 判据 6: Spent 反映累计值, 且与是否耗尽无关 (供指标与日志读数)。
func TestH05_SpentReportsCumulative(t *testing.T) {
	g := NewRunBudgetGate(0) // 不设上限也要能报数 (只观测不拦截)
	g.Spend(10)
	g.Spend(32)
	if got := g.Spent(); got != 42 {
		t.Fatalf("Spent() = %d, 期望 42", got)
	}
	if g.Exhausted() {
		t.Fatal("不设上限时不应耗尽")
	}
}

// 判据 7: nil 接收者安全 —— 与 ErrorClassifier 等同类组件保持一致的防御口径。
//
// 装配方可能不给闸门 (limit 未配), 此时所有方法都该是零值语义而不是 panic。
// 这条防的是"配置没开 ⇒ 整个 run 崩掉", 而配置没开本该是"不拦截"。
func TestH05_NilGateIsSafe(t *testing.T) {
	var g *RunBudgetGate
	if _, trip := g.Spend(1); trip {
		t.Fatal("nil 闸门不应触发")
	}
	if g.Exhausted() {
		t.Fatal("nil 闸门不应处于耗尽态")
	}
	if g.Spent() != 0 || g.Limit() != 0 {
		t.Fatal("nil 闸门读数应为 0")
	}
	g.Reset() // 不应 panic
}

// 判据 8: Reset 是 run 边界复位 —— 清掉累计与耗尽态, 但**保留上限**。
//
// 引擎是会话级复用的, 而闸门要的是 run 级语义。若 Reset 把上限也清掉, 第二个 run
// 就变成"不设限"—— 那是把止损线在一次复位里悄悄拆掉, 比不复位更危险。
func TestH05_ResetClearsSpendKeepsLimit(t *testing.T) {
	g := NewRunBudgetGate(1000)
	g.Spend(5000)
	if !g.Exhausted() {
		t.Fatal("前置条件失败: 应已耗尽")
	}

	g.Reset()

	if g.Exhausted() {
		t.Fatal("Reset 之后不该还是耗尽态 —— 闸门按 run 记账, 上一个 run 的账不该呛死下一个")
	}
	if got := g.Spent(); got != 0 {
		t.Fatalf("Reset 之后 Spent() = %d, 期望 0", got)
	}
	if got := g.Limit(); got != 1000 {
		t.Fatalf("Reset 把上限也清掉了 (Limit() = %d) —— 等于悄悄拆掉止损线", got)
	}
	// 上限仍在: 再花 1001 必须还是能拦。
	if _, trip := g.Spend(1001); !trip {
		t.Fatal("Reset 之后闸门不再拦截 —— 上限丢了")
	}
}
