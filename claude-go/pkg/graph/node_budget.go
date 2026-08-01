package graph

// node_budget.go —— NodeSpec.TimeoutSec 在**挂起-唤醒**之间的语义 (design/01 §六
// 那条 ❌: "角色差异化超时的节点级天花板已有但 Suspended 态下的语义未定")。
//
// ---------------------------------------------------------------------------
// 一、先核实"挂起期间超时钟表还走不走" (答案是不走, 且这一条是对的)
// ---------------------------------------------------------------------------
//
// 节点级天花板的实现只有一处: execNode 里 `TimeoutSec>0` 时给节点 ctx 叠一层
// `context.WithTimeout` (engine.go 语义 3)。挂起有两种形态, 逐条核实:
//
//   - **跨运行挂起** (终局挂起 ⇒ run 以 suspended 收尾, 答复到位后 resume): 等待期
//     根本没有 ctx —— 引擎已经返回, 进程可能都退出了。所以"等人审批的节点被判超时"
//     这种缺陷在本仓**不存在**。
//   - **本次运行内 revive** (runner 回报 ReviveAfter>0): 等待发生在
//     `engine.go` 派发 goroutine 的 `e.sleep(ctx, wait)` 里, 用的是**运行 ctx**,
//     在 `execNode` 之外 ⇒ 也不烧节点预算。
//
// 两条都对: 等待不是执行, 不该吃执行预算。
//
// ---------------------------------------------------------------------------
// 二、真正未定的是另一件事: 唤醒后剩余预算怎么算
// ---------------------------------------------------------------------------
//
// 改造前, 每次 revive 都会**重新进一次 execNode**, 于是每次都拿到一个**全新的完整**
// `TimeoutSec`。而 `TimeoutSec` 的契约到处都写着"节点**总预算**":
//
//	spec.go        NodeSpec.TimeoutSec  "节点**总预算**(秒), 0=不限"
//	engine.go 文件头 语义 3               "TimeoutSec>0 时叠加 deadline"
//	pkg/agent/graph_adapter.go:100       "引擎的 NodeSpec.TimeoutSec 包住**整个节点**
//	                                      (retry 环 + loop 环), 属于'总预算'"
//
// 于是一个反复挂起的节点能吃到 `(MaxRevives+1) × TimeoutSec`(缺省 MaxRevives=3 ⇒
// **4 倍**), 加上最多 `MaxRevives × MaxWaitSec` 的等待。天花板名存实亡, 而且是静默的
// —— journal 里每次尝试都是一条独立的 node.started, 谁也看不出预算被翻了 4 倍。
//
// **定死的语义**(本文件实现):
//
//	TimeoutSec = 节点在**一次运行内**的累计**执行**预算。
//	  · 挂起的等待时长不计入 (等待不是执行, 见上);
//	  · 本次运行内的每次 revive 从剩余预算里扣, 不重置;
//	  · 预算耗尽 ⇒ 节点 failed (不是继续挂着), 见下文三;
//	  · **跨运行 (resume) 预算重置**, 见下文四。
//
// ---------------------------------------------------------------------------
// 三、耗尽为什么判 failed (而 revive 额度耗尽判 suspended)
// ---------------------------------------------------------------------------
//
// suspend.go 里"唤醒额度用尽"刻意**不**转失败: 那时节点没出错, 只是这次没等到,
// 转 failed 会让 `fail` 条件边误触发、让平台把一次等待当失败去补救。
//
// 预算耗尽是**另一件事**: 节点已经把它被授权的全部执行墙钟烧完了。继续挂着等 resume
// 意味着下一轮又给它一份完整预算 —— 那就回到了改造前那个"天花板不封顶"的形态, 只是
// 换成了跨运行发生。所以这里 fail-closed: 判 failed, 并把"已用/上限/尝试次数"写进
// journal 与错误文案, 让"为什么这个节点没跑"可查。
//
// ---------------------------------------------------------------------------
// 四、跨运行为什么重置 (journal 里有时间戳也不该续算)
// ---------------------------------------------------------------------------
//
// 技术上能续算: journal 有 node.started / node.suspended 的时间戳。但不该:
//
//   - 挂起等待期**天然跨进程**(人工审批的等待正是最容易跨越进程生命周期的, 见
//     suspend.go 文件头二)。一个等了一夜的审批节点, 若把"上一轮已用掉的执行时间"带过来,
//     resume 时可能第一秒就超时 —— 而它一秒钟的活都还没干。
//   - resume 是**新一轮墙钟授权**。这与 BudgetManager"重放时不重建台账"是同一口径
//     (journal.go EvBudgetConsumed 的注释: 否则一个跑过六轮的图永远无法 resume)。
//     两处口径若不一致, "预算"这个词在同一个引擎里就有两种含义。
//
// ---------------------------------------------------------------------------
// 五、为什么不需要环境开关
// ---------------------------------------------------------------------------
//
// 记账只对**能被本次运行内再叫一次**的节点发生, 判据是 `node.Suspend != nil`
// (只有它能让 reviveWait 返回 >0: human 节点被 reviveWait 显式排除, 未声明 Suspend
// 的节点被 authorizeSuspend 归一成 failed 根本到不了挂起)。
//
// 而全仓**没有任何生产图声明 Suspend / human / subgraph 节点**:
// `pkg/agent/graph_adapter.go` 的 `StageGraphOverride` 连 Suspend 字段都没有, Kind 只
// 认 ""|agent|gate|map|reduce。`grep -rn "Suspend:" --include=*.go pkg/agent cmd` 零命中
// (只有 pkg/graph 的两个测试)。于是对现存的每一个生产节点, 本文件的两个函数都在
// `budgetTracked()==false` 的那一支直接返回, 与改造前**逐字节相同** ——
// 不存在需要用开关保护的生产可感知行为。开关反而有害: 一个默认关的天花板等于天花板
// 不成立, 而声明它的人以为成立 (本仓修过的一类缺陷)。

import "time"

// budgetTracked 该节点是否需要跨"挂起-唤醒"累计执行预算。
//
// 两个条件都必要: 没有 TimeoutSec 就没有天花板可言 (不限); 没有 Suspend 就不可能在
// 本次运行内被再叫一次 (见文件头五), 记账只会白占一个 map 槽位。
func budgetTracked(node NodeSpec) bool {
	return node.TimeoutSec > 0 && node.Suspend != nil
}

// remainingBudget 本次执行尝试可用的执行预算, 以及是否还有余额。
//
// 返回 (剩余时长, true) 表示可以跑; (已用总量, false) 表示预算耗尽 —— 第一个返回值
// 在耗尽时改载"已用总量", 供调用方直接写进错误文案 (省一次查表)。
//
// 未跟踪的节点 (budgetTracked==false) 恒返回完整 TimeoutSec: 于是首次执行与改造前
// 逐字节相同。
func (rc *runCtx) remainingBudget(evID string, node NodeSpec) (time.Duration, bool) {
	total := time.Duration(node.TimeoutSec) * time.Second
	if !budgetTracked(node) {
		return total, true
	}
	rc.mu.Lock()
	spent := rc.spent[evID]
	rc.mu.Unlock()
	if spent >= total {
		return spent, false
	}
	return total - spent, true
}

// chargeBudget 把一次执行的实际耗时记进本节点的累计执行预算。
//
// 按**限定 ID** (scope.evID) 记账而不是按 NodeSpec.ID: loop-group 的每一轮
// (`<组>#it<轮次>/<成员>`)、map 的每个分片 (`<阶段>#<序号>`)、subgraph 的成员
// (`<节点>~sg/<成员>`) 都是各自独立的一次执行, 各有自己的预算。按 NodeSpec.ID 记
// 会让第 2 轮组循环继承第 1 轮的消耗 —— 那是把"每轮一份预算"悄悄改成"整组一份"。
func (rc *runCtx) chargeBudget(evID string, node NodeSpec, d time.Duration) {
	if !budgetTracked(node) || d <= 0 {
		return
	}
	rc.mu.Lock()
	if rc.spent == nil {
		rc.spent = map[string]time.Duration{}
	}
	rc.spent[evID] += d
	rc.mu.Unlock()
}
