package graph

// runstatus.go —— RunStatus 取值表 + 与设计 8 态的逐条对账 + 从 journal 推生命周期态
// (design/01 §4.3 GraphRun.Status)。
//
// ---------------------------------------------------------------------------
// 一、设计写了 8 态, 图层只该产生 6 个 —— 逐条对账 (不是打折)
// ---------------------------------------------------------------------------
//
// design/01 §4.3 的 `GraphRun.Status` 列了 8 个字面量:
//
//	created | running | paused | completed | delivered_with_remediation | failed | stopped | refining
//
// 把它们与团队层状态机 (pkg/agent/teams.go 的 TeamStatus 7 态) 逐条比对后, 只有 4 个
// 是图层能诚实产生的, 另 4 个是**团队层专有**。逐条依据:
//
//	created                    ✅ 图层产生 —— 但只作为**生命周期态**由 LifecycleStatus
//	                              从 journal 推出 (journal 里还没有 run.created),
//	                              永远不会是 Engine.Run 的返回值 (Run 是同步的)。
//	running                    ✅ 同上 (有 run.created 无 run.finished ⇒ 崩在半路)。
//	paused                     ❌ **两层都没有产生方**。团队层 7 态里没有 paused;
//	                              图层"暂停"在本仓的真实形态是 suspended (等人工答复 /
//	                              等限流额度, 见 suspend.go)。加一个零产生方的常量正是
//	                              §六 记账里"🟡 建成未通电"那一类, 故**不加**。
//	completed                  ✅ 已有。
//	delivered_with_remediation ❌ 团队层专有: 它是**门禁裁决**的结果
//	                              (pkg/agent/teams.go:944-947 computeRunDelivery ——
//	                              中间阶段失败过、但最终确定性门禁过了)。门禁在团队层,
//	                              图层看不到门禁元数据, 猜它等于伪造交付结论。
//	failed                     ✅ 已有。
//	stopped                    ❌ 团队层专有: 用户 StopTeam (teams.go:1478) 走
//	                              team.cancel → ctx 取消 → 图层只看到"ctx 没了",
//	                              **区分不出**"用户停"与"上游超时/父 ctx 取消"。
//	                              图层照语义 8 一律 failed, 由团队层按自己知道的成因
//	                              置 stopped。
//	refining                   ❌ 团队层专有且是**跨多次 run** 的生命周期态
//	                              (completed → refining → running, teams.go:1584)。
//	                              图层一次 Run 只覆盖一轮; 它的等价物是
//	                              node.invalidated 事件 + 下一次 resume。
//
// 图层另有 2 个设计没列但**必须有**的取值:
//
//	partial   ⚠️ 生产在用 (engine.go 汇总终态)。设计列表里没有等价物 ——
//	             delivered_with_remediation 是门禁裁决, 而 partial 说的是"节点成功比例
//	             介于全成功与全失败之间", 两者的判据、层次、可信来源都不同。
//	             **保留**: 删它是行为变更 (见下文"为什么保留 partial")。
//	suspended ✅ 设计成文后新增 (suspend.go): 有节点挂起 ⇒ 可 resume 续跑,
//	             与 failed 的区别是可恢复性。
//
// 于是: **图层 6 态** = created | running | completed | partial | failed | suspended。
// 团队层 7 态一字未动。两层的对应关系是一张**显式映射表**, 落在
// pkg/agent/graph_run_status.go —— 不靠"字符串刚好相等"。映射表放团队层而不是这里,
// 因为映射要用到 TeamStatus 与门禁语义, 而 pkg/graph 不该认识团队。
//
// ---------------------------------------------------------------------------
// 二、为什么保留 partial (删它的代价是具体的)
// ---------------------------------------------------------------------------
//
// partial 是 `Engine.Run` 三个真实终态之一 (engine.go 语义 6: 全 completed ⇒
// completed; 无 completed ⇒ failed; 其余 ⇒ partial)。删掉它只有两条路, 都更糟:
//
//   - 并进 completed ⇒ 一次"5 个阶段跑挂了 2 个"的运行会被报成完全成功。这是
//     fail-open 的**治理**方向, 本仓明令禁止。
//   - 并进 failed ⇒ 团队层现在的处置会翻转: graph_adapter 只对 failed 返回 error
//     (即"一个节点都没成"才判整轮失败), 部分成功照常走交付 → 门禁 → 可能
//     delivered_with_remediation。并进 failed 会让所有"部分成功"的运行直接失败,
//     而本仓 30+ 工作流里"某个可选阶段失败但主干交付了"是常态。
//
// 所以偏离设计的是**设计的枚举漏了这一格**, 不是实现打了折。已在 §4.3 记账。
//
// ---------------------------------------------------------------------------
// 三、生命周期态为什么从 journal 推而不是存一个字段
// ---------------------------------------------------------------------------
//
// design/01 §4.3 的原话是"journal 是唯一进度真源"。若再存一个 status 字段, 进程被
// kill 时它必然停在"running"而 journal 已经记到了最后一个 node.completed —— 那就是
// 第二个真源, 也正是 team.json 曾经的形态 (见 pkg/agent/team_projection.go)。
// LifecycleStatus 只读事件, 于是"崩在半路"这件事从 journal 本身可判, 不依赖任何人
// 在崩溃前记得写字段。

// 图运行状态 (design/01 §4.3 RunStatus; 6 个取值的对账见本文件头)。
//
// 前两个是**生命周期态**: 只由 LifecycleStatus 从 journal 推出, `Engine.Run` 的
// 返回值永远不会是它们 (Run 同步返回, 返回时运行必然已结束)。后四个是**终态**:
// 既是 Run 的返回值, 也是 run.finished 事件的 status。
const (
	// RunStatusCreated journal 里还没有本图的 run.created —— 从未跑过。
	RunStatusCreated = "created"
	// RunStatusRunning 有 run.created 但没有 run.finished ——
	// 要么正在跑, 要么**崩在半路**(进程被 kill/OOM)。两者从 journal 分辨不出,
	// 也不必分辨: 两种情况的处置都是"按 journal 已完成的节点 resume 续跑"。
	RunStatusRunning = "running"

	RunStatusCompleted = "completed"
	RunStatusPartial   = "partial"
	RunStatusFailed    = "failed"
	// RunStatusSuspended 本次运行有节点挂起 (等人工答复 / 等限流退避), 可 resume 续跑。
	// **只有声明了 suspend 的节点或 human 节点才能产生它** —— 未声明时一切照旧。
	RunStatusSuspended = "suspended"
)

// IsTerminalRunStatus 该取值是否为**终态** (可作为 Engine.Run 的返回值 /
// run.finished 的 status)。created/running 是生命周期态, 返回 false。
//
// 未知取值返回 false: 判据是"在不在已知终态集合里", 不是"不是那两个生命周期态"。
// 后者会让一个拼错的字面量被当成终态一路传下去 (fail-open)。
func IsTerminalRunStatus(s string) bool {
	switch s {
	case RunStatusCompleted, RunStatusPartial, RunStatusFailed, RunStatusSuspended:
		return true
	}
	return false
}

// LifecycleStatus 从 journal 事件推出 GraphRun 的生命周期状态 (design/01 §4.3)。
//
// 判据只有三条, 顺序即语义:
//  1. 没有任何 run.created ⇒ created (这张图在这个 journal 上从未跑过)。
//  2. 最后一个 run.created 之后出现了同 RunID 的 run.finished ⇒ 取它带的 status。
//  3. 否则 ⇒ running (正在跑, 或崩在半路)。
//
// 只看**最后一个** run.created 起的那一段, 与 Replay 完全同一口径 (journal 在生产是
// per-team 而非 per-run 的, 同一文件里有多轮事件; 看整个文件会把上一轮的 run.finished
// 当成本轮的 —— 那正是 Replay 修过的那个 P0 的同形错误)。
//
// run.finished 带了未知 status 时**原样返回**而不是归一成某个已知值: 调用方
// (pkg/agent/graph_run_status.go 的映射表) 对未知取值是 fail-closed 的, 在这里悄悄
// 归一等于把那道闸绕掉。
func LifecycleStatus(events []Event) string {
	start, lastRun := -1, ""
	for i, ev := range events {
		if ev.Type == EvRunCreated {
			start, lastRun = i, ev.RunID
		}
	}
	if start < 0 {
		return RunStatusCreated
	}
	for _, ev := range events[start:] {
		if ev.Type != EvRunFinished {
			continue
		}
		// RunID 为空的事件不过滤 (老 journal 可能没写 RunID), 与 Replay 一致。
		if lastRun != "" && ev.RunID != "" && ev.RunID != lastRun {
			continue
		}
		if s, ok := ev.Data["status"].(string); ok && s != "" {
			return s
		}
		// 有 run.finished 但载荷里没有 status: 运行确实结束了, 但结论不可知。
		// 报 running 会让调用方去 resume 一个已经结束的 run; 报 completed 是凭空
		// 编造交付结论。取 partial —— "跑完了但结论不完整"正是它的语义。
		return RunStatusPartial
	}
	return RunStatusRunning
}
