package agent

// team_projection.go —— team.json 的**运行进度投影** (design/01 §4.3 "Journal 是
// 唯一进度真源" + §六 那条 ❌ "team.json 投影化")。
//
// ---------------------------------------------------------------------------
// 一、投影化到底要做什么 (不是删 team.json)
// ---------------------------------------------------------------------------
//
// team.json 是**下游契约**: pkg/dashboard 直接读盘 (v14_handlers.go:651 handleTeamDAG
// 合成实时 DAG、run_feedback.go:216 靠 lastRunId 认领 run 属于哪个团队), `team status`
// / 飞书卡片 / 8+ 下游平台都按它现在的形状分支。**格式一个字节都不动**。
//
// 要做的是让**运行进度那部分字段**由 journal 派生, 而不是两处各写一遍。改造前是两份
// 各自独立的记账:
//
//	journal   : 引擎每个状态迁移 Append 一条 (node.started/completed/failed/...)
//	team.json : teamGraphHooks.flush() 用**本进程 hook 收到的节点**整体覆盖 Stages
//
// 两份必然漂移, 而且有一处是**永久性**的:
//
//	resume 时 Replay 命中缓存的节点**不会被派发** (engine.go: dr.state 已有 ⇒ 跳过),
//	于是它们一条 hook 都不发 ⇒ 不进 h.byID ⇒ flush() 的快照里没有它们 ⇒
//	team.json 的 Stages 只剩"这一轮真跑了的那几个"。
//	正常收尾时 run_interceptors.go:331 (`team.Stages = rc.results`) 会用引擎返回的
//	全量节点 (含缓存命中) 覆盖回来, 于是只是"跑的时候少几个阶段";
//	但**这一轮又失败**时走 failTeam (teams.go:1415), 它**不碰 Stages** ⇒
//	截断后的 Stages 永久落盘。一个反复失败-恢复的团队, 每恢复一次 dashboard 上
//	就少一批已完成阶段, 而 journal 里一直都在。
//
// ---------------------------------------------------------------------------
// 二、逐字段核实: 哪些能从 journal 重建, 哪些不能 (也不该)
// ---------------------------------------------------------------------------
//
// ProductionTeam 的字段 (teams.go:478-525) 逐条过一遍:
//
//	能重建 (= 运行进度, journal 是真源):
//	  Stages[].Name       ← node.* 事件的 node_id
//	  Stages[].Status     ← node.completed / node.failed / node.suspended
//	  Stages[].Output     ← node.completed 的 data.output
//	  Stages[].Error      ← node.failed 的 data.error / node.suspended 的 data.reason
//	  Stages[].StartedAt  ← node.started 的 ts
//	  Stages[].Duration   ← node.completed/failed 的 ts − node.started 的 ts
//	  StartedAt           ← run.created 的 ts
//	  FinishedAt          ← run.finished 的 ts
//	  LastRunID           ← 最后一个 run.created 的 run_id
//	  (生命周期态)         ← graph.LifecycleStatus(events)
//
//	不能重建, 且**不该**重建 (= 声明, 不是运行进度):
//	  Name / Workflow / ChatID / Cwd / Language / CreatedAt / TaskIDs
//	      —— 团队创建时的声明。journal 里 run.created 虽然带 graph(=工作流名) 与
//	      objective, 但那是"这一轮跑的是什么", 不是"这个团队是什么"。拿它反推团队声明
//	      会在换工作流重跑后把历史写进现在。
//	  Objective
//	      —— 同上。run.created 里有 objective, 但团队的 Objective 是**下一轮要跑什么**
//	      (RunTeam 会先赋值再跑), 与"上一轮跑过什么"不是同一个量。
//	  Stages[].Role
//	      —— **journal 不记 role**: node.completed 的载荷只有 output/score,
//	      node.started 只有 scope.extra (engine.go execNode)。role 是**声明**
//	      (NodeSpec.Agent.Role), 从 spec / WorkflowDef 取才是真源; 给 journal 加一个
//	      role 字段等于把声明抄进事件流, 抄本会漂移。故投影把 Role 留空, 由调用方按
//	      声明侧填 (见 seedFromJournal 用 h.roles)。
//	  Stages[].Input / V2TaskID
//	      —— 前者图路径本来就不填 (提示词组装归 runner); 后者是 V2 Task 系统的外部 ID。
//	  Agents / Progress
//	      —— 心跳快照 (Coordinator 的执行相位 "LLM生成"/"编译", 粒度是 turn),
//	      journal 的粒度是**节点**, 重建出来是另一个量纲。硬凑会让 dashboard 上
//	      "此刻在做什么"从执行相位退化成阶段名 —— 那是 §4.11 黑板订阅那条记账里
//	      "会改 Progress.Phase 词表"点名过的行为变更。
//	  Mailbox / RefineHistory / PendingFeedback / FeedbackTarget / Error
//	      —— 团队层自己的账 (邮件、精修留痕、待处理反馈、团队级错误文案),
//	      引擎完全看不到。
//	  Status
//	      —— 团队层 7 态里有 4 个图层根本产生不出来
//	      (delivered_with_remediation 是门禁裁决 / stopped 是用户意图 / refining 跨多轮
//	      / created 是团队生命周期)。详见 graph_run_status.go 的映射表。**投影不改
//	      team.Status**: 状态机一字不动是这一轮的硬边界。
//
// ---------------------------------------------------------------------------
// 三、只覆盖 graph 路径, 且默认关
// ---------------------------------------------------------------------------
//
// journal 只在图引擎路径下产生 (`<team.dataDir>/graph-journal/journal.jsonl`,
// graph_adapter.go:1425)。pipeline 路径的进度真源仍是 checkpoints.json —— 那条路上
// team.json 依然是**独立真源**, 本文件对它无能为力。这是这一轮**没做完**的部分,
// 已在 design/01 §六 写清边界与理由。
//
// 默认关 (`CLAUDE_GO_TEAM_PROJECTION`), 因为它改的是 team.json 的**内容**:
// 恢复路径会让一个崩溃过的团队突然多出一批 Stages, 而有下游按 `len(stages)` 或
// stage 名做判断 (例如 mediaforge 的 harvest 按阶段名取产出)。"团队恢复后阶段变多"
// 虽然是修正, 但确实是可感知的变化, 该由部署方显式打开。
// 关的时候**连 journal 文件都不读** ⇒ 零 I/O、零解析, 与改造前逐字节相同。

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
)

// teamProjectionEnabled 投影开关 (默认关, 理由见文件头三)。
//
// 与 CLAUDE_GO_GRAPH_* 一族同形: 只认 1|on|true, 其余 (含拼错) 一律关。
// 这里**不**学 graph_watchdog 的"未知取值退到 notify" —— 那边退档只是多观测,
// 这边退档会改 team.json 的内容, 方向不安全。
func teamProjectionEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_TEAM_PROJECTION")))
	return v == "1" || v == "on" || v == "true"
}

// teamProgressProjection team.json 里能由 journal 派生的那部分 (见文件头二)。
type teamProgressProjection struct {
	// RunID 最后一个 run.created 的 run_id (= team.LastRunID)。
	RunID string
	// Lifecycle graph.LifecycleStatus 的结果 (created|running|<终态>)。
	Lifecycle string
	// Stages 按 journal 里首次出现的顺序 (Role 恒空, 见文件头二)。
	Stages []StageResult
	// StartedAt / FinishedAt run.created / run.finished 的时间 (零值 = 无该事件)。
	StartedAt  time.Time
	FinishedAt time.Time
}

// projectTeamProgress 把 journal 事件投影成运行进度 (纯函数, 无 I/O —— 于是这一层
// 的语义可以脱离磁盘单测)。
//
// 只看**最后一个 run.created 起**的那一段, 与 graph.Replay / graph.LifecycleStatus
// 完全同一口径: journal 在生产是 per-team 而非 per-run 的, 看整个文件会把上一轮的
// 事件当成本轮 (Replay 修过的那个 P0 的同形错误)。
func projectTeamProgress(events []graph.Event) teamProgressProjection {
	out := teamProgressProjection{Lifecycle: graph.LifecycleStatus(events)}

	start, lastRun := -1, ""
	for i, ev := range events {
		if ev.Type == graph.EvRunCreated {
			start, lastRun = i, ev.RunID
		}
	}
	if start < 0 {
		return out
	}
	out.RunID = lastRun
	out.StartedAt = msToTime(events[start].TS)

	type acc struct {
		sr      StageResult
		startTS int64
	}
	// order 记首次出现序 —— journal 是 append-only, 首次出现序即调度序, 与 pipeline
	// 的阶段顺序观感一致。不能靠 map 迭代序: 那会让 team.json 每次写盘都不同形。
	var order []string
	byID := map[string]*acc{}
	get := func(id string) *acc {
		a, ok := byID[id]
		if !ok {
			a = &acc{sr: StageResult{Name: id}}
			byID[id] = a
			order = append(order, id)
		}
		return a
	}

	for _, ev := range events[start:] {
		if lastRun != "" && ev.RunID != "" && ev.RunID != lastRun {
			continue
		}
		if ev.Type == graph.EvRunFinished {
			out.FinishedAt = msToTime(ev.TS)
			continue
		}
		if ev.NodeID == "" {
			continue
		}
		switch ev.Type {
		case graph.EvNodeStarted:
			a := get(ev.NodeID)
			a.startTS = ev.TS
			a.sr.StartedAt = msToTime(ev.TS)
			a.sr.Status = TaskRunning
			// 重跑 (retry / revive) 会再来一条 node.started: 清掉上一次的终态残留,
			// 否则一个"失败后重试成功"的节点会带着旧的 Error 文案。
			a.sr.Error = ""
		case graph.EvNodeCompleted:
			a := get(ev.NodeID)
			a.sr.Status = TaskCompleted
			a.sr.Output = evStr(ev, "output")
			a.sr.Error = ""
			a.sr.Duration = durSince(a.startTS, ev.TS)
		case graph.EvNodeFailed:
			a := get(ev.NodeID)
			a.sr.Status = TaskFailed
			a.sr.Error = evStr(ev, "error")
			a.sr.Duration = durSince(a.startTS, ev.TS)
		case graph.EvNodeSuspended:
			// 挂起**不是终态**: 报 TaskFailed 会让下游把一次等待当失败去补救
			// (suspend.go 反复强调的那条), 报 TaskCompleted 是凭空交付。
			// 取 TaskRunning + 原因进 Error —— 这也正是 team.json 里"还在等"的最近似表达
			// (StageResult 没有第四种状态, 加一种就要改 8+ 下游的 status 分支)。
			a := get(ev.NodeID)
			a.sr.Status = TaskRunning
			a.sr.Error = evStr(ev, "reason")
		case graph.EvNodeSkipped:
			// 条件边把流程路由到别的分支 / hook deny / 上游 skipped 级联: 这个阶段
			// **没有执行**, 不是失败。与 graph_adapter.go:1480 逐条一致 —— 那里也把
			// skipped 排除在 results 之外 (一旦条件边真用起来, 被路由掉的分支会被
			// 统计成"阶段失败"并把整个团队判死)。
			delete(byID, ev.NodeID)
		case graph.EvNodeInvalidated:
			// refine 显式失效: 与 Replay 同一口径 (从已完成里摘掉), 于是投影里也不该
			// 再出现它 —— 否则 dashboard 上一个待重跑的阶段仍显示上一轮的产出。
			delete(byID, ev.NodeID)
		}
	}

	out.Stages = make([]StageResult, 0, len(byID))
	for _, id := range order {
		if a, ok := byID[id]; ok { // skipped/invalidated 已被 delete
			out.Stages = append(out.Stages, a.sr)
		}
	}
	return out
}

// readTeamJournalEvents 读一个团队的 graph journal 事件 (无 journal ⇒ nil, nil)。
//
// 直接读文件而不是 graph.NewFileJournal(...).ReadAll(): 后者会**创建**目录与文件并
// 持有一个写句柄。恢复路径 (loadPersistedTeams) 会遍历 baseDir 下每一个目录, 用那条
// 路会给每个从未跑过图的团队凭空造出一个空 journal, 还每个泄一个 fd。
func readTeamJournalEvents(dataDir string) []graph.Event {
	if dataDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dataDir, graphJournalDirName, "journal.jsonl"))
	if err != nil || len(data) == 0 {
		return nil
	}
	return graph.DecodeJournal(data)
}

// projectTeamProgressFromJournal 读盘 + 投影。ok=false = 没有 journal 或还没跑过。
func projectTeamProgressFromJournal(dataDir string) (teamProgressProjection, bool) {
	evs := readTeamJournalEvents(dataDir)
	if len(evs) == 0 {
		return teamProgressProjection{}, false
	}
	p := projectTeamProgress(evs)
	if p.RunID == "" && len(p.Stages) == 0 {
		return p, false
	}
	return p, true
}

// backfillTeamProgressFromJournal 用 journal 投影补齐一个从磁盘恢复的团队的运行进度
// (design/01 §六 team.json 投影化的**恢复路径**接线点)。
//
// 只在 team.json 的 Stages **比 journal 少**时才动它, 并且只**补**不删:
//   - 补: journal 有而 team.json 没有的阶段 (= 上一轮 resume 命中缓存、hook 没发、
//     被 flush() 整体覆盖掉的那些);
//   - 不删: team.json 里有而 journal 没有的阶段照原样留着 —— pipeline 路径的阶段
//     就在这一类里 (它们从不进 journal), 按 journal 删就会把整条 pipeline 的历史抹掉。
//
// **不改 team.Status**: 状态机一字不动 (见文件头二)。恢复路径的
// `running → failed + "进程重启"` 保持原样, 于是 RunTeam 的 resume 判据
// (failed + 同 objective) 照旧成立。
//
// 返回补进去的阶段数 (0 = 无变化), 供日志与测试断言。
func backfillTeamProgressFromJournal(team *ProductionTeam) int {
	if team == nil || !teamProjectionEnabled() {
		return 0
	}
	p, ok := projectTeamProgressFromJournal(team.dataDir)
	if !ok {
		return 0
	}
	have := make(map[string]bool, len(team.Stages))
	for _, s := range team.Stages {
		have[s.Name] = true
	}
	added := 0
	for _, s := range p.Stages {
		if have[s.Name] {
			continue
		}
		team.Stages = append(team.Stages, s)
		added++
	}
	// LastRunID 也补: 它是 design/03 奖励聚合的 episode 键, 空值落盘即死数据
	// (teams.go:496 的注释)。崩在 run 中间时 team.json 可能还没记上它。
	if team.LastRunID == "" && p.RunID != "" {
		team.LastRunID = p.RunID
		added++
	}
	return added
}

// baseNodeID 取一个限定 NodeID 的顶层声明节点名。
//
// 运行期合成的 ID 有四种形态, 第一个分隔符之前都是顶层声明节点:
//
//	map 分片        <阶段>#<序号>
//	loop-group 成员 <组>#it<轮次>/<成员>
//	subgraph 成员   <节点>~sg/<成员>
//	动态展开产物    <父>/<子>
func baseNodeID(id string) string {
	if i := strings.IndexAny(id, "#/~"); i > 0 {
		return id[:i]
	}
	return id
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func durSince(startTS, endTS int64) string {
	if startTS <= 0 || endTS < startTS {
		return ""
	}
	return (time.Duration(endTS-startTS) * time.Millisecond).String()
}

func evStr(ev graph.Event, key string) string {
	if ev.Data == nil {
		return ""
	}
	s, _ := ev.Data[key].(string)
	return s
}
