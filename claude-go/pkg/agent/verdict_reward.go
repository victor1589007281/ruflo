package agent

// verdict_reward.go —— 奖励源 verdict.heuristic (design/03 §4.2 奖励源清单第 7 行)。
//
// ---------------------------------------------------------------------------
// 先说清"卡点"到底是什么, 因为设计里记的账已经不准了
// ---------------------------------------------------------------------------
//
// 设计当时记的原因是「RunIsolated 不走 HookChain, 团队路径根本不产 turn verdict」。
// 逐条核实后, 这句话对了一半:
//
//   - CLI 团队路径 (`claude-go run` → RunIsolated) 确实**不产** turn verdict:
//     它有自己的 turn 循环, 不经 HookChain 的 PhasePostTurn, TurnMetricsHook 不触发。
//   - 飞书会话路径 (团队 agent 走完整 queryLoop) **产** turn verdict, 但那个
//     `internal_hook.Trajectory` 只带 SessionID, **没有 RunID/NodeID** ——
//     而 AggregateRewards 强制要求 RunID。也就是说有 verdict 的那条路径, verdict
//     归不到 run 上; 归得到 run 的那条路径, 没有 verdict。两边各缺一半。
//
// 所以"直接把 TurnMetricsHook 接到 RewardBus"是走不通的 (拿不到 run 归因), 而
// "让 pkg/engine 反向依赖 pkg/agent 去发奖励"会成环。
//
// ---------------------------------------------------------------------------
// 走通的那条路: 从**已经带 run 归因的轨迹**里把 verdict 算回来
// ---------------------------------------------------------------------------
//
// TraceStore 的 Span 天生就是按 (RunID, NodeID, TurnID) 组织的 —— 那正是 §4.1 建它
// 的理由。turn Span 里有 stop_reason, tool_call Span 里有 is_error, llm_call Span 里
// 有 stop_reason + status。verdict 的全部输入都在里面, 且都是**真实执行结果**:
// 模型为什么停下、工具报没报错, 不是常量也不是随机数。
//
// 于是这条源实现为"run 收尾时从本 run 的轨迹派生", 挂在运行级拦截器链的 evolution
// 环里 —— 那一环本来就是 episode 奖励与 latency shaping 的发出处, 同一批 run 级信号
// 待在一起。两条路径由此都被覆盖: 飞书路径用 turn Span, CLI 路径回落 llm_call Span。
//
// ---------------------------------------------------------------------------
// 三条刻意的取舍
// ---------------------------------------------------------------------------
//
//  1. **裁决口径与 internal_hook.InferVerdict 逐条对齐, 但不 import 它**。
//     pkg/agent 现在完全不依赖 pkg/engine, 为一个枚举建这条边不划算 (learners 的
//     fallbackWeights 是同一套处理: 复制 + 用测试焊住)。一致性由
//     verdict_reward_consistency_test.go 保证 —— 那是外部测试包, 只在测试二进制里
//     反向 import, 生产依赖图不变。
//  2. **unknown 不计入**。stop_reason 认不出、或 tool_use 轮拿不到工具成败时,
//     InferVerdict 给的是 unknown ——它是"我不知道"而不是"中等"。把它折成 0 会让
//     没有信息的轮次去稀释有信息的轮次, 那就是在造假信号。
//  3. **权重 0.15, 全表最低**。它只回答"这轮跑完了没、工具报错没", 一个把工具全调
//     成功而内容全错的 run 在它眼里是满分。比 episode (0.3) 还低是必须的:
//     episode 至少反映了交付判定。
//
// 已知局限 (记账, 不假装做完了): CLI 路径没有 tool_call Span (RunIsolated 不经
// HookChain 的 PhasePostToolUse), 所以那条路上的 tool_use 轮一律 unknown 被跳过,
// 实际只有"最后一轮怎么停的"与"有没有调用报错"两种信号进得来。

import (
	"context"
	"strings"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// verdict 裁决枚举 (取值与 internal_hook.TrajVerdict 逐字一致)。
type verdict string

const (
	verdictSuccess verdict = "success"
	verdictPartial verdict = "partial"
	verdictFail    verdict = "fail"
	verdictUnknown verdict = "unknown"
)

// inferTurnVerdict 从 stop_reason 与工具成败推断一轮的裁决。
//
// **这是 internal_hook.InferVerdict 的镜像**: 分支、阈值 (0.9 / 0.5)、空工具时的
// stop_reason 表全部照抄。改这里必须同步改那里, 否则同一条轨迹在引擎侧与学习侧会得到
// 两个不同的结论 —— consistency 测试会把漂移抓出来。
//
// 与原版的唯一差异: 原版有 aborted 参数走 VerdictAborted, 这里没有 —— 轨迹里没有
// "被用户中止"这个位, 中止会表现为 stop_reason 为 aborted*/error, 落进 fail。
func inferTurnVerdict(stopReason string, okTools, totalTools int) verdict {
	if totalTools == 0 {
		switch stopReason {
		case "end_turn", "":
			return verdictSuccess
		case "max_tokens":
			return verdictPartial
		case "error":
			return verdictFail
		default:
			return verdictUnknown
		}
	}
	ratio := float64(okTools) / float64(totalTools)
	switch {
	case ratio >= 0.9:
		return verdictSuccess
	case ratio >= 0.5:
		return verdictPartial
	default:
		return verdictFail
	}
}

// verdictValue 裁决 → [-1,1]。第二返回值 false = 不计入 (unknown)。
func verdictValue(v verdict) (float64, bool) {
	switch v {
	case verdictSuccess:
		return 1, true
	case verdictPartial:
		return 0, true
	case verdictFail:
		return -1, true
	default:
		return 0, false
	}
}

// nodeVerdictStat 一个节点的裁决统计。
type nodeVerdictStat struct {
	Node    string
	Sum     float64
	Counted int            // 计入的轮数 (不含 unknown)
	Skipped int            // 被跳过的 unknown 轮数 (进 Raw, 说明证据覆盖率)
	Kinds   map[string]int // 各裁决出现次数 (进 Raw, 供人复盘)
	// FromLLMCall = true 表示该节点没有 turn Span, 用 llm_call 回落 (CLI 路径)。
	FromLLMCall bool
}

// Score 平均裁决分; Counted=0 时无意义 (调用方须先判)。
func (s nodeVerdictStat) Score() float64 {
	if s.Counted == 0 {
		return 0
	}
	return s.Sum / float64(s.Counted)
}

// deriveVerdictStats 从一个 run 的全部 Span 派生逐节点裁决统计。
//
// 分组键是 NodeID (阶段名)。NodeID 为空的 Span 归到 "" 组 —— 那是 run 级轮次
// (没进 stage 的调用), 记成 NodeID 为空的奖励事件仍能被 RunRewardScore 聚合到。
func deriveVerdictStats(spans []tracestore.Span) []nodeVerdictStat {
	// ① tool_call 按 turn 汇总成败。
	type toolTally struct{ ok, total int }
	toolsByTurn := map[string]*toolTally{}
	for _, sp := range spans {
		if sp.Kind != tracestore.KindToolCall {
			continue
		}
		t := toolsByTurn[sp.TurnID]
		if t == nil {
			t = &toolTally{}
			toolsByTurn[sp.TurnID] = t
		}
		t.total++
		// is_error 缺失 (工具没回结果) 按失败算: 没拿到结果的工具调用不是成功。
		if !attrBool(sp.Attrs, "is_error", true) {
			t.ok++
		}
	}

	// ② turn Span 优先; 记录哪些节点有 turn 证据。
	stats := map[string]*nodeVerdictStat{}
	get := func(node string) *nodeVerdictStat {
		s := stats[node]
		if s == nil {
			s = &nodeVerdictStat{Node: node, Kinds: map[string]int{}}
			stats[node] = s
		}
		return s
	}
	hasTurn := map[string]bool{}
	for _, sp := range spans {
		if sp.Kind != tracestore.KindTurn {
			continue
		}
		hasTurn[sp.NodeID] = true
		ok, total := 0, 0
		if t := toolsByTurn[sp.TurnID]; t != nil {
			ok, total = t.ok, t.total
		}
		accumulate(get(sp.NodeID), inferTurnVerdict(attrString(sp.Attrs, "stop_reason"), ok, total))
	}

	// ③ 没有 turn Span 的节点回落 llm_call (CLI RunIsolated 路径)。
	// **只对没有 turn 证据的节点回落** —— 两者混算会把同一轮数两遍。
	for _, sp := range spans {
		if sp.Kind != tracestore.KindLLMCall || hasTurn[sp.NodeID] {
			continue
		}
		s := get(sp.NodeID)
		s.FromLLMCall = true
		// status=="error" 是这条路上唯一确凿的失败位 (HTTP 调用失败/熔断),
		// 它比 stop_reason 更可信, 所以先判。
		if attrString(sp.Attrs, "status") == "error" {
			accumulate(s, verdictFail)
			continue
		}
		ok, total := 0, 0
		if t := toolsByTurn[sp.TurnID]; t != nil {
			ok, total = t.ok, t.total
		}
		accumulate(s, inferTurnVerdict(attrString(sp.Attrs, "stop_reason"), ok, total))
	}

	out := make([]nodeVerdictStat, 0, len(stats))
	for _, s := range stats {
		if s.Counted == 0 {
			continue // 全是 unknown: 没有证据就不发事件
		}
		out = append(out, *s)
	}
	return out
}

func accumulate(s *nodeVerdictStat, v verdict) {
	s.Kinds[string(v)]++
	val, ok := verdictValue(v)
	if !ok {
		s.Skipped++
		return
	}
	s.Sum += val
	s.Counted++
}

// recordVerdictRewards 派生并落盘本 run 的 verdict.heuristic 奖励。
//
// 全程 fail-open: 轨迹底座没装、读不出、这个 run 一条 Span 都没有, 都只是"没有这条
// 弱信号", 绝不影响交付 —— 与 writeGateSpan 同一条原则 (采集是观测不是治理)。
func (ptm *ProductionTeamManager) recordVerdictRewards(ctx context.Context, team *ProductionTeam) {
	if ptm == nil || ptm.evolution == nil || ptm.traceStore == nil || team == nil {
		return
	}
	runID := strings.TrimSpace(trace.From(ctx).RunID)
	if runID == "" {
		return // 无 run 归因的奖励落盘即死数据
	}
	spans, err := ptm.traceStore.ReadRun(runID)
	if err != nil || len(spans) == 0 {
		return
	}
	for _, st := range deriveVerdictStats(spans) {
		raw := map[string]any{
			"turns":    st.Counted,
			"verdicts": st.Kinds,
		}
		if st.Skipped > 0 {
			raw["unknown_skipped"] = st.Skipped
		}
		if st.FromLLMCall {
			// 让复盘的人一眼看出这条证据来自哪条路径 (CLI 路径没有工具成败位)。
			raw["source_span"] = tracestore.KindLLMCall
		}
		ptm.evolution.RecordReward(RewardEvent{
			RunID:  runID,
			NodeID: st.Node,
			Source: RewardSourceVerdictHeuristic,
			Value:  st.Score(),
			Raw:    raw,
			Team:   team.Name,
		})
	}
}

// attrString 从 Attrs 取字符串 (JSON 回读后类型可能已变, 只认 string)。
func attrString(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	if v, ok := attrs[key].(string); ok {
		return v
	}
	return ""
}

// attrBool 从 Attrs 取布尔; 缺失或类型不符时返回 def。
func attrBool(attrs map[string]any, key string, def bool) bool {
	if attrs == nil {
		return def
	}
	if v, ok := attrs[key].(bool); ok {
		return v
	}
	return def
}
