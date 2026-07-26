package agent

// team_metrics.go —— 团队级指标的**唯一**发射口 (design/03 §1.3 跨源对账)。
//
// ---------------------------------------------------------------------------
// 它修的是什么
// ---------------------------------------------------------------------------
//
// 改造前全仓 **59 个** `Collector.RecordRun` 调用点**无一例外**把 `team.Name` 传进
// 那个叫 `runID` 的参数:
//
//	we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name, labels)
//	                                                           ^^^^^^^^^ 这是团队名
//
// 于是 `team.jsonl` 的 `run_id` 字段里装的是团队名, 而 `llm.jsonl` 的 `run_id` 是真
// RunID (`run-<团队>-<traceID>`) —— **两个 sink 按 run_id 根本 join 不上, 而字段名让人
// 以为能**。design/03 §1.3 把"三种 run_id 格式并存"列为断点, 这 59 处是其中最大的一处。
//
// ⚠️ 我此前把这件事记成"代价大于收益、留作单独一轮", 理由是"team.Name 是团队身份的
// 唯一载体, 换成 RunID 就得同时加 `team` 标签, 而加标签会改 Prometheus 序列身份"。
// **那个理由错了** —— 它默认了"团队身份只能靠 labels 携带"。`MetricEvent` 里 `RunID`
// 从一开始就与 `Labels` 分离且不进 Prometheus; 给它加一个同样不进 Prometheus 的
// `Team` 字段, 三件事同时成立:
//
//	① run_id 恢复本义 (真 RunID) ⇒ 与 llm.jsonl / trace spans / rewards 可 join;
//	② 团队身份不丢 (进 Team 字段);
//	③ **一个 Prometheus 序列都没动** (labels 一字未改) ⇒ 既有 Grafana 查询不受影响。
//
// 这与 dashboard 侧奖励聚合早就在用的 `(run_id, team)` 二元组口径也一致
// (`run_feedback.go`: "AggregateRewards 按 (run_id, team) 过滤")。
//
// ---------------------------------------------------------------------------
// RunID 从哪来 (为什么不用穿 ctx)
// ---------------------------------------------------------------------------
//
// 59 个调用点里只有约一半的外层函数带 ctx —— 另一半 (`recordWBSPlanMetrics` /
// `flushStagesLive` / `recordGraphStageMetrics` / `runBuildHardGate` ...) 只拿到
// `*ProductionTeam`。为此给 8 个函数加 ctx 参数是可行但过度的改动: **`team.LastRunID`
// 里已经是同一个值**(`teams.go` 在 executeWorkflow 开头写入 `trace.From(ctx).RunID`,
// 注释写明"运行后才发生的奖励只能靠它归因"), 而每个调用点都握着 team。
//
// 于是本文件只做一件事: 把 (team → RunID, 团队名) 这一步收到**一处**。

import "github.com/anthropic/claude-go/pkg/metrics"

// teamRunID 取团队本轮运行的 trace RunID; 没有则返回空。
//
// **拿不到时返回空而不是退回 team.Name** —— 退回去正是本文件要修的那个缺陷 (一个叫
// run_id 的字段里装团队名)。团队身份已由 Team 字段独立携带, 空 run_id 如实表示
// "这条指标不属于任何一次已开始的运行"(团队创建、重启后的补记等), 那是真话。
//
// 加锁读: `LastRunID` 由 executeWorkflow 在 team.mu 下写入, 而指标可能从别的 goroutine
// 发出 (心跳 / 看门狗)。仓内另有一处裸读 (graph_adapter.go), 那是预存在的竞争, 不在本
// 改动范围 —— 但新代码不该再添一处。
func teamRunID(team *ProductionTeam) string {
	if team == nil {
		return ""
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	return team.LastRunID
}

// recordTeamRun 发一条团队级指标 (module 通常是 "team")。
//
// 取代散在 7 个文件里的 `X.RecordRun(module, name, v, team.Name, labels)`:
// 收到一处之后, "run_id 到底装的是什么"只有一个答案, 而不是 59 个各自为政的答案。
//
// m/team 为 nil 时是空操作 —— 各调用点原本就有 `if we.metrics != nil` 之类的守卫,
// 这里再兜一层是因为守卫漏一处的表现是**panic 在指标路径上**, 而指标绝不该能弄死交付。
//
// ⚠️ 第一版把 m 写成 `interface{ RecordRunTeam(...) }` 以便测试注入, **那是个 bug**:
// 调用点传的是 `*metrics.Collector`, 它为 nil 时装进接口后是**带类型的 nil**,
// `m == nil` 为 false ⇒ 照样调进去 ⇒ `c.mu.Lock()` 在 nil 上 panic。也就是说那道
// "兜一层"的守卫在最需要它的场景下恰好失效。所以这里收具体类型: `m == nil` 才是真的判空。
// 可测性靠真 Collector 写临时目录再读回 JSONL —— 顺带把序列化那一段也一起验了。
func recordTeamRun(m *metrics.Collector, module, name string, value float64, team *ProductionTeam, labels map[string]string) {
	if m == nil || team == nil {
		return
	}
	m.RecordRunTeam(module, name, value, teamRunID(team), team.Name, labels)
}
