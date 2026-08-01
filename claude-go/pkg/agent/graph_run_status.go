package agent

// graph_run_status.go —— 图层 RunStatus → 团队层处置的**显式映射表**
// (design/01 §4.3 "两层状态机对齐")。
//
// ---------------------------------------------------------------------------
// 一、为什么要一张表, 而不是继续靠"字符串刚好相等"
// ---------------------------------------------------------------------------
//
// 改造前团队层只认一个字面量: `graph_adapter.go` 里 `if rr.Status ==
// graph.RunStatusFailed { return error }`。其余取值一律落到"没报错 ⇒ 继续交付"这条
// 隐式的 else 上。它现在恰好对 completed/partial 是对的, 但有两个问题:
//
//  1. **suspended 会被当成交付成功**。挂起的语义是"等答复/额度, resume 才能往下走",
//     此时下游节点根本没跑。走进交付路径 ⇒ 团队被判 completed、REPORT.md 落盘、
//     记忆/技能提炼全部触发 —— 用一次"还在等"冒充一次交付。这是 fail-open 的**治理**,
//     本仓明令禁止的方向。
//  2. **新增取值默认放行**。图层日后再加一个终态, 团队层不改一行代码就会把它当成功。
//     "漏一个 case 就静默丢状态"正是 §4.3 watchdog 那条记账点名的形态。
//
// 所以这里把 6 个取值逐条列出, `default` 是 fail-closed 的拒绝而不是放行。
// 表放在团队层而不是 pkg/graph: 映射用到 TeamStatus 与门禁语义, 而 pkg/graph
// 不该认识团队 (它的 8 态对账见 pkg/graph/runstatus.go 文件头)。
//
// ---------------------------------------------------------------------------
// 二、suspended 为什么落到 failed, 而不是新增一个 TeamStatusPaused
// ---------------------------------------------------------------------------
//
// 设计的 8 态里有 `paused`, 看起来正是挂起该去的地方。但**不加**, 三条理由:
//
//   - 新增团队终态要让 8+ 下游平台的 status 分支各长一个 case, 漏一个就是静默丢状态
//     (与 §4.3 watchdog 拒绝新造 `stalled` 终态同一条红线)。team.json 的 status
//     字段是对外契约, dashboard (pkg/dashboard/v14_handlers.go:651 直接读盘) 与各平台
//     都按现有 7 个值分支。
//   - `failed` 在本仓的团队语义**本来就是"可恢复"**, 不是"报废":
//     `RunTeam` 的 resume 判据恰恰是 `team.Status == TeamStatusFailed &&
//     team.Objective == objective` (teams.go:722)。也就是说落 failed 之后, 下一次
//     RunTeam 会自动带 Resume 重跑 → 图层 Replay 读到 human.responded → 挂起节点完成
//     → 从断点续跑。这正是挂起想要的机械行为, 一行新代码都不需要。
//   - 区分度靠 `team.Error` 文案而不是新状态: 错误文本里写明"挂起等待"与节点 ID,
//     `team status` / dashboard 都能看到。
//
// ⚠️ 这一条是**行为变更**(suspended 从"当成功"变成"判失败"), 但**不是生产可感知的**:
// 全仓没有任何生产图能产生 suspended —— 只有声明了 `Suspend` 的节点、`human` 节点、
// 或成员挂起的 `subgraph` 节点能产生它 (engine.go 的 authorizeSuspend 把未声明的归一成
// failed), 而 `StageGraphOverride` (图能力的唯一声明入口) 连 Suspend 字段都没有,
// Kind 只认 ""|agent|gate|map|reduce。`grep -rn "Suspend:" pkg/agent cmd` 零命中。
// 所以这条边今天只在测试里可达, 不需要环境开关保护 (开关反而会让这道 fail-closed
// 的闸默认失效)。
//
// ---------------------------------------------------------------------------
// 三、created/running 为什么也在表里
// ---------------------------------------------------------------------------
//
// 它们**永远不会是 Engine.Run 的返回值**(Run 同步返回)。但同一张表也被
// team_projection.go 用来解释**从 journal 推出的生命周期态** —— 那里 running 的含义是
// "崩在半路"。两处共用一张表是刻意的: 否则"图层某个状态在团队层算什么"会有两份答案。

import "fmt"

// graphRunVerdict 一个图层运行状态在团队层的处置。
type graphRunVerdict struct {
	// Deliver 是否允许本次运行走交付路径 (computeRunDelivery → 门禁 → REPORT.md)。
	Deliver bool
	// Resumable 该状态下的团队可以靠"再跑一次同目标"从 journal 续跑。
	// 纯记账/文案用: 团队层的 resume 判据是 status==failed && 同 objective, 而落
	// failed 的路径 (failTeam) 是共用的, 这里不另开一条。
	Resumable bool
	// TeamStatus 该状态**若由团队层直接落状态**时对应的 TeamStatus。
	// 注意 Deliver=true 时它只是"下限": 真正的终态还要过 computeRunDelivery
	// (可能升级成 delivered_with_remediation, 也可能因阻塞性失败降成 failed)。
	TeamStatus TeamStatus
	// Reason 供错误文案与日志使用 (Deliver=false 时非空)。
	Reason string
}

// graphRunStatusVerdict 图层运行状态 → 团队层处置 (显式表, 见文件头)。
//
// 未知取值返回 error 而**不是**退到某个安全档: 退档意味着一个拼错的/新加的状态被
// 悄悄当成已知情形处理, 而调用方以为表覆盖全了。本仓的规矩是"静默退档等于让声明它的
// 人以为策略生效", 所以报错。
func graphRunStatusVerdict(status string) (graphRunVerdict, error) {
	switch status {
	case "completed":
		return graphRunVerdict{Deliver: true, TeamStatus: TeamStatusCompleted}, nil
	case "partial":
		// 部分节点成功。**照旧走交付**: 本仓 30+ 工作流里"某个可选阶段失败但主干交付了"
		// 是常态, 由 computeRunDelivery 按失败阶段 + 门禁裁决最终是
		// delivered_with_remediation 还是 failed。图层不越级下这个结论。
		return graphRunVerdict{Deliver: true, TeamStatus: TeamStatusCompleted}, nil
	case "failed":
		// 文案与改造前**逐字一致** (下游有按错误文本判定的用法, 改字面量就是改对外输出)。
		return graphRunVerdict{Resumable: true, TeamStatus: TeamStatusFailed,
			Reason: "graph_adapter: 图执行失败 (无节点完成)"}, nil
	case "suspended":
		return graphRunVerdict{Resumable: true, TeamStatus: TeamStatusFailed,
			Reason: "graph_adapter: 图执行挂起, 等待人工答复/限流额度 (答复到位后再跑同一目标即从 journal 续跑)"}, nil
	case "running":
		// 只能从 journal 推出 (有 run.created 无 run.finished): 正在跑, 或崩在半路。
		return graphRunVerdict{Resumable: true, TeamStatus: TeamStatusRunning,
			Reason: "graph_adapter: 图运行未收尾 (进程崩溃或仍在执行)"}, nil
	case "created":
		return graphRunVerdict{TeamStatus: TeamStatusCreated,
			Reason: "graph_adapter: 图从未运行"}, nil
	}
	return graphRunVerdict{}, fmt.Errorf("graph_adapter: 未知的图运行状态 %q (团队层映射表未覆盖; 见 graph_run_status.go)", status)
}
