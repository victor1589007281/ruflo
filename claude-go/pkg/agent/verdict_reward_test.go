package agent

import (
	"context"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// span 造一条轨迹 Span 的最小构造器。
func vspan(kind, node, turn string, attrs map[string]any) tracestore.Span {
	return tracestore.Span{TraceID: "run-1", SpanID: kind + "-" + turn, Kind: kind,
		NodeID: node, TurnID: turn, Attrs: attrs}
}

// TestDeriveVerdictStats_按节点分组且unknown不计入 —— 核心口径: unknown 是"我不知道",
// 不能折成 0 去稀释有信息的轮次。
func TestDeriveVerdictStats_按节点分组且unknown不计入(t *testing.T) {
	spans := []tracestore.Span{
		// impl 节点: 一轮 end_turn 无工具 (success) + 一轮 stop_reason 认不出 (unknown)
		vspan(tracestore.KindTurn, "impl", "t1", map[string]any{"stop_reason": "end_turn"}),
		vspan(tracestore.KindTurn, "impl", "t2", map[string]any{"stop_reason": "pause_turn"}),
		// review 节点: 一轮 max_tokens (partial)
		vspan(tracestore.KindTurn, "review", "t3", map[string]any{"stop_reason": "max_tokens"}),
	}
	got := map[string]nodeVerdictStat{}
	for _, s := range deriveVerdictStats(spans) {
		got[s.Node] = s
	}
	if len(got) != 2 {
		t.Fatalf("应产出 2 个节点的统计, got %d (%+v)", len(got), got)
	}
	impl := got["impl"]
	if impl.Counted != 1 || impl.Skipped != 1 {
		t.Errorf("impl 应只计入 1 轮、跳过 1 轮 unknown, got counted=%d skipped=%d", impl.Counted, impl.Skipped)
	}
	if impl.Score() != 1 {
		t.Errorf("impl 唯一计入的一轮是 success ⇒ 分数应为 1 (unknown 若被折成 0 会变成 0.5), got %v", impl.Score())
	}
	if got["review"].Score() != 0 || got["review"].Counted != 1 {
		t.Errorf("review 的 max_tokens 应记 partial=0 且计入, got %+v", got["review"])
	}
}

// 工具成败按 turn 配对: 全成功=success, 半数=partial, 全失败=fail。
func TestDeriveVerdictStats_工具成败按turn配对(t *testing.T) {
	spans := []tracestore.Span{
		vspan(tracestore.KindTurn, "impl", "t1", map[string]any{"stop_reason": "tool_use"}),
		vspan(tracestore.KindToolCall, "impl", "t1", map[string]any{"is_error": false}),
		vspan(tracestore.KindToolCall, "impl", "t1", map[string]any{"is_error": false}),
		vspan(tracestore.KindTurn, "fix", "t2", map[string]any{"stop_reason": "tool_use"}),
		vspan(tracestore.KindToolCall, "fix", "t2", map[string]any{"is_error": true}),
		vspan(tracestore.KindToolCall, "fix", "t2", map[string]any{"is_error": true}),
	}
	got := map[string]float64{}
	for _, s := range deriveVerdictStats(spans) {
		got[s.Node] = s.Score()
	}
	if got["impl"] != 1 {
		t.Errorf("两个工具全成功 ⇒ success(1), got %v", got["impl"])
	}
	if got["fix"] != -1 {
		t.Errorf("两个工具全失败 ⇒ fail(-1), got %v", got["fix"])
	}
}

// is_error 缺失 (工具没回结果) 必须按失败算 —— 没拿到结果的调用不是成功。
func TestDeriveVerdictStats_无结果的工具按失败算(t *testing.T) {
	spans := []tracestore.Span{
		vspan(tracestore.KindTurn, "impl", "t1", map[string]any{"stop_reason": "tool_use"}),
		vspan(tracestore.KindToolCall, "impl", "t1", map[string]any{"has_result": false}),
	}
	st := deriveVerdictStats(spans)
	if len(st) != 1 || st[0].Score() != -1 {
		t.Errorf("无 is_error 位的工具调用应判失败, got %+v", st)
	}
}

// CLI (RunIsolated) 路径没有 turn Span, 必须回落 llm_call; 且**不得**与 turn 混算。
func TestDeriveVerdictStats_无turn时回落llm_call(t *testing.T) {
	spans := []tracestore.Span{
		// hasTurn 的节点: 即使也有 llm_call 也只算 turn (否则同一轮被数两遍)
		vspan(tracestore.KindTurn, "impl", "t1", map[string]any{"stop_reason": "end_turn"}),
		vspan(tracestore.KindLLMCall, "impl", "t1", map[string]any{"stop_reason": "end_turn", "status": "success"}),
		// 无 turn 的节点: 回落 llm_call, 其中一条调用直接失败
		vspan(tracestore.KindLLMCall, "iso", "t2", map[string]any{"stop_reason": "end_turn", "status": "success"}),
		vspan(tracestore.KindLLMCall, "iso", "t2", map[string]any{"stop_reason": "", "status": "error"}),
	}
	got := map[string]nodeVerdictStat{}
	for _, s := range deriveVerdictStats(spans) {
		got[s.Node] = s
	}
	if got["impl"].Counted != 1 {
		t.Errorf("有 turn 证据的节点不得再吃 llm_call (会把同一轮数两遍), got counted=%d", got["impl"].Counted)
	}
	if got["impl"].FromLLMCall {
		t.Error("impl 有 turn 证据, 不该被标成 llm_call 回落")
	}
	iso := got["iso"]
	if !iso.FromLLMCall {
		t.Error("iso 没有 turn 证据, 应标记为 llm_call 回落 (供复盘时知道证据成色)")
	}
	if iso.Counted != 2 || iso.Score() != 0 {
		t.Errorf("一成功一失败 ⇒ (1-1)/2 = 0, got counted=%d score=%v", iso.Counted, iso.Score())
	}
}

// --- 落盘链路 ---

func TestRecordVerdictRewards_落盘并可被聚合(t *testing.T) {
	ptm, team, state, ts := newRewardPTM(t, true)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-v"})
	// TraceID 必须是本 run 的 id: 轨迹按它分桶, 写错桶就等于没写。
	write := func(turn, stop string) {
		ts.Write(tracestore.Span{TraceID: "run-v", SpanID: "s-" + turn, Kind: tracestore.KindTurn,
			NodeID: "impl", TurnID: turn, Attrs: map[string]any{"stop_reason": stop}})
	}
	write("t1", "end_turn") // success
	write("t2", "error")    // fail  ⇒ 均值 0

	ptm.recordVerdictRewards(ctx, team)

	evs := readRewards(t, state)
	if len(evs) == 0 {
		t.Fatal("应写出 verdict.heuristic 奖励")
	}
	ev := evs[0]
	if ev.Source != RewardSourceVerdictHeuristic {
		t.Errorf("source = %q, want %q", ev.Source, RewardSourceVerdictHeuristic)
	}
	if ev.NodeID != "impl" {
		t.Errorf("必须带节点归因 (GEPA 的 FindWeakNodes 靠它定位), got %q", ev.NodeID)
	}
	if ev.RunID != "run-v" || ev.Team != "tm" {
		t.Errorf("run/team 归因缺失, 落盘即死数据: %+v", ev)
	}
	if ev.Weight != rewardWeightHeuristic {
		t.Errorf("权重应为全表最低的 %.2f, got %.2f", rewardWeightHeuristic, ev.Weight)
	}
	// 真的能被读侧聚合到 (这才叫接进了总线, 不是只写了一行日志)。
	if _, ok := ptm.evolution.StageRewardScore("run-v", "tm", "impl"); !ok {
		t.Error("AggregateRewards 必须能聚合到这条证据")
	}
}

// 无 RunID / 无底座 / 空轨迹三种情形都必须静默不写 —— 采集是观测不是治理。
func TestRecordVerdictRewards_缺前提时不写死数据(t *testing.T) {
	ptm, team, state, _ := newRewardPTM(t, true)
	ptm.recordVerdictRewards(context.Background(), team) // 无 RunID
	if evs := readRewards(t, state); len(evs) != 0 {
		t.Fatalf("无 RunID 时不得写出无归因奖励, got %+v", evs)
	}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-empty"})
	ptm.recordVerdictRewards(ctx, team) // 轨迹为空
	if evs := readRewards(t, state); len(evs) != 0 {
		t.Fatalf("空轨迹时不得凭空造奖励, got %+v", evs)
	}
	noTrace, teamB, stateB, _ := newRewardPTM(t, false)
	noTrace.recordVerdictRewards(ctx, teamB) // 无底座
	if evs := readRewards(t, stateB); len(evs) != 0 {
		t.Fatalf("未装 TraceStore 时应 no-op, got %+v", evs)
	}
}
