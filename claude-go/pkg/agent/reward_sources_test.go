package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// --- 脚手架 ---

// newRewardPTM 造一个只装了 Evolution (+可选 TraceStore) 的团队管理器与一个团队。
func newRewardPTM(t *testing.T, withTrace bool) (*ProductionTeamManager, *ProductionTeam, string, *tracestore.Store) {
	t.Helper()
	state := t.TempDir()
	evoDir := filepath.Join(state, "evolution")
	ee := NewEvolutionEngine(evoDir, nil)
	ptm := &ProductionTeamManager{teams: map[string]*ProductionTeam{}, evolution: ee}
	var ts *tracestore.Store
	if withTrace {
		ts = tracestore.New(statestore.NewFileStore(filepath.Join(state, "statestore")))
		ptm.traceStore = ts
	}
	team := &ProductionTeam{Name: "tm", Cwd: state, LastRunID: "run-1"}
	ptm.teams["tm"] = team
	return ptm, team, state, ts
}

// readRewards 读回 rewards.jsonl。
func readRewards(t *testing.T, state string) []RewardEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "evolution", "rewards.jsonl"))
	if err != nil {
		return nil
	}
	var out []RewardEvent
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		var ev RewardEvent
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("rewards.jsonl 有非法行: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// --- user.explicit (/team rate) ---

func TestRateTeam_星级映射与落盘(t *testing.T) {
	cases := []struct {
		stars int
		want  float64
	}{{1, -1}, {2, -0.5}, {3, 0}, {4, 0.5}, {5, 1}}
	for _, c := range cases {
		ptm, _, state, _ := newRewardPTM(t, false)
		if err := ptm.RateTeam("tm", c.stars, "评语"); err != nil {
			t.Fatalf("%d 星评分失败: %v", c.stars, err)
		}
		evs := readRewards(t, state)
		if len(evs) != 1 {
			t.Fatalf("%d 星应写出 1 条奖励, got %d", c.stars, len(evs))
		}
		ev := evs[0]
		if ev.Source != RewardSourceUserExplicit || ev.Value != c.want {
			t.Errorf("%d 星: source=%q value=%.2f, 期望 value=%.2f", c.stars, ev.Source, ev.Value, c.want)
		}
		if ev.RunID != "run-1" {
			t.Errorf("必须带上一轮的 RunID 才聚合得到, got %q", ev.RunID)
		}
		if ev.Weight != rewardWeightUser {
			t.Errorf("user.explicit 权重应为 %.1f, got %.1f", rewardWeightUser, ev.Weight)
		}
	}
}

// 人主动给的反馈拿不到落点时必须报错, 绝不静默丢弃。
func TestRateTeam_错误路径都返回错误(t *testing.T) {
	ptm, team, _, _ := newRewardPTM(t, false)
	if err := ptm.RateTeam("tm", 0, ""); err == nil {
		t.Error("0 星应报错")
	}
	if err := ptm.RateTeam("tm", 6, ""); err == nil {
		t.Error("6 星应报错")
	}
	if err := ptm.RateTeam("不存在", 5, ""); err == nil {
		t.Error("团队不存在应报错")
	}
	team.LastRunID = ""
	if err := ptm.RateTeam("tm", 5, ""); err == nil {
		t.Error("没有已完成运行时应报错 (而不是写一条 RunID 为空的死数据)")
	}
	noEvo := &ProductionTeamManager{teams: map[string]*ProductionTeam{"tm": {Name: "tm", LastRunID: "r"}}}
	if err := noEvo.RateTeam("tm", 5, ""); err == nil {
		t.Error("未装进化引擎时应报错")
	}
}

// --- user.steer (精修负信号) ---

func TestRecordSteerReward_归因到上一轮与指定阶段(t *testing.T) {
	ptm, team, state, _ := newRewardPTM(t, false)
	ptm.recordSteerReward(team, "run-prev", "这段写得太笼统", "implement")
	evs := readRewards(t, state)
	if len(evs) != 1 {
		t.Fatalf("应写出 1 条, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Source != RewardSourceUserSteer || ev.Value != steerRewardValue {
		t.Errorf("source/value 不对: %+v", ev)
	}
	if ev.RunID != "run-prev" {
		t.Errorf("必须归因到**上一轮** run, got %q", ev.RunID)
	}
	if ev.NodeID != "implement" {
		t.Errorf("指定了阶段就该挂到该阶段, got %q", ev.NodeID)
	}
	if ev.Value >= 0 {
		t.Error("精修是负信号")
	}
}

// 无 RunID 的奖励落盘即死数据 (AggregateRewards 强制要求 RunID), 不许写。
func TestRecordSteerReward_无RunID不写(t *testing.T) {
	ptm, team, state, _ := newRewardPTM(t, false)
	ptm.recordSteerReward(team, "", "反馈", "")
	if evs := readRewards(t, state); len(evs) != 0 {
		t.Fatalf("无 RunID 不该写盘, got %+v", evs)
	}
}

// 整体重跑 (未指定阶段) 挂 run 级, 不猜某个阶段。
func TestRecordSteerReward_未指定阶段挂run级(t *testing.T) {
	ptm, team, state, _ := newRewardPTM(t, false)
	ptm.recordSteerReward(team, "run-prev", "整体重来", "")
	evs := readRewards(t, state)
	if len(evs) != 1 || evs[0].NodeID != "" {
		t.Fatalf("未指定阶段应挂 run 级 (NodeID 空), got %+v", evs)
	}
}

// --- review.panel ---

func TestRecordReviewPanelReward_映射与零分守卫(t *testing.T) {
	ptm, team, state, _ := newRewardPTM(t, false)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-x"})

	// overall<=0 是"没评上"而不是"评了 0 分", 记进去等于捏造一条满负分。
	recordReviewPanelReward(ctx, ptm.evolution, team, 0, 0.5)
	if evs := readRewards(t, state); len(evs) != 0 {
		t.Fatalf("overall=0 不该写盘, got %+v", evs)
	}
	recordReviewPanelReward(ctx, ptm.evolution, team, 86, 0.8)
	evs := readRewards(t, state)
	if len(evs) != 1 {
		t.Fatalf("应写出 1 条, got %d", len(evs))
	}
	ev := evs[0]
	if want := 86.0/50.0 - 1.0; ev.Value != want {
		t.Errorf("0-100 → [-1,1] 映射应与 gate.content 一致 (%.3f), got %.3f", want, ev.Value)
	}
	if ev.Source != RewardSourceReviewPanel || ev.Weight != rewardWeightLLMJudge {
		t.Errorf("source/权重不对: %+v", ev)
	}
	if ev.RunID != "run-x" {
		t.Errorf("应从 ctx 取 RunID, got %q", ev.RunID)
	}
}

// nil 引擎 / nil 团队都不能 panic (奖励是增强项)。
func TestRecordReviewPanelReward_空安全(t *testing.T) {
	recordReviewPanelReward(context.Background(), nil, nil, 90, 1)
}

// 离线反解: 从历史 stage 产出里取回 overall。
func TestParsePanelOverall(t *testing.T) {
	out := "合议评审(3 位)结果:\n\n```json\n{\n  \"overall\": 86,\n  \"consensus\": 0.82\n}\n```\n"
	o, c, ok := parsePanelOverall(out)
	if !ok || o != 86 || c != 0.82 {
		t.Fatalf("反解失败: overall=%.1f consensus=%.2f ok=%v", o, c, ok)
	}
	if _, _, ok := parsePanelOverall("没有 JSON 的散文"); ok {
		t.Error("无 JSON 时应返回 ok=false")
	}
	if _, _, ok := parsePanelOverall(`{"overall": 0}`); ok {
		t.Error("overall<=0 应返回 ok=false")
	}
}

// --- latency shaping ---

func TestLatencyShapingValue_阶梯且只罚不奖(t *testing.T) {
	cases := []struct {
		sec  float64
		want float64
	}{
		{0, 0}, {60, 0}, {latencyBudgetSec, 0},
		{latencyBudgetSec + 1, -0.2}, {latencySoftSec, -0.2},
		{latencySoftSec + 1, -0.5}, {latencyHardSec, -0.5},
		{latencyHardSec + 1, -1}, {99999, -1},
	}
	for _, c := range cases {
		if got := latencyShapingValue(c.sec); got != c.want {
			t.Errorf("%.0fs → %.2f, 期望 %.2f", c.sec, got, c.want)
		}
	}
	// 只罚不奖: 任何时长都不该产生正奖励 (最快的路径是什么都不做)。
	for _, sec := range []float64{0, 1, 10, 1000, 100000} {
		if latencyShapingValue(sec) > 0 {
			t.Errorf("%.0fs 产生了正奖励, 那会直接激励偷工", sec)
		}
	}
}

// 预算内不写盘: 一条 0 值会挤掉 AggregateRewards 固定字节窗口里真正有信息的旧事件。
func TestRecordLatencyReward_预算内不写盘(t *testing.T) {
	ptm, team, state, _ := newRewardPTM(t, false)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "r"})
	ptm.recordLatencyReward(ctx, team, 60)
	if evs := readRewards(t, state); len(evs) != 0 {
		t.Fatalf("预算内不该写盘, got %+v", evs)
	}
	ptm.recordLatencyReward(ctx, team, latencyHardSec+10)
	evs := readRewards(t, state)
	if len(evs) != 1 || evs[0].Source != RewardSourceLatency || evs[0].Value != -1 {
		t.Fatalf("超重罚档应写一条 -1: %+v", evs)
	}
	if evs[0].Weight != rewardWeightShaping {
		t.Errorf("shaping 权重应为 %.1f (不与质量判断同权竞争), got %.1f", rewardWeightShaping, evs[0].Weight)
	}
}

// --- 权重表 ---

func TestRewardSourceWeight_新源已登记(t *testing.T) {
	cases := map[string]float64{
		RewardSourceGateCompile:  rewardWeightDeterministic,
		RewardSourceGateTest:     rewardWeightDeterministic,
		RewardSourceUserExplicit: rewardWeightUser,
		RewardSourceUserSteer:    rewardWeightUser,
		RewardSourceGateContent:  rewardWeightLLMJudge,
		RewardSourceReviewPanel:  rewardWeightLLMJudge,
		RewardSourceEpisode:      rewardWeightEpisode,
		RewardSourceLatency:      rewardWeightShaping,
		"cost":                   rewardWeightShaping,
		"从未登记的源":                 rewardWeightUnknown,
	}
	for src, want := range cases {
		if got := RewardSourceWeight(src); got != want {
			t.Errorf("源 %q 权重 %.2f, 期望 %.2f", src, got, want)
		}
	}
	// 排序关系是设计的核心约束: 确定性门禁 > 人 > LLM 评分 > 终态 > shaping。
	if !(rewardWeightDeterministic > rewardWeightUser &&
		rewardWeightUser > rewardWeightLLMJudge &&
		rewardWeightLLMJudge > rewardWeightEpisode &&
		rewardWeightEpisode > rewardWeightShaping) {
		t.Error("权重阶梯被破坏 (design/03 §4.6 奖励源加权可信度)")
	}
}

// --- gate Span (design/03 §4.1 第 5 种 Kind) ---

// readSpans 读回某 run 的全部 span。
func readSpans(t *testing.T, ts *tracestore.Store, runID string) []tracestore.Span {
	t.Helper()
	spans, err := ts.ReadRun(runID)
	if err != nil {
		t.Fatalf("读 span 失败: %v", err)
	}
	return spans
}

func TestWriteGateSpan_门禁判定入轨迹(t *testing.T) {
	ptm, team, _, ts := newRewardPTM(t, true)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-g", NodeID: "impl", TurnID: "t1"})
	ptm.recordGateReward(ctx, team, RewardSourceGateTest, "go test failed: 断言不通过")

	spans := readSpans(t, ts, "run-g")
	var gate *tracestore.Span
	for i := range spans {
		if spans[i].Kind == tracestore.KindGate {
			gate = &spans[i]
		}
	}
	if gate == nil {
		t.Fatalf("应写出一条 Kind=gate 的 Span, got %+v", spans)
	}
	if gate.Name != RewardSourceGateTest || gate.Attrs["pass"] != false {
		t.Errorf("门禁 Span 字段不对: name=%q attrs=%v", gate.Name, gate.Attrs)
	}
	if gate.Attrs["status"] != "fail" {
		t.Errorf("失败门禁的 status 应为 fail, got %v", gate.Attrs["status"])
	}
	detail, err := ts.Resolve(gate.OutputRef)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "断言不通过") {
		t.Errorf("报错全文应可从 Span 还原 (蒸馏要读它), got %q", detail)
	}
}

// 奖励与轨迹的守卫必须分开: 只装 TraceStore 没装 Evolution 时 gate Span 仍要写。
func TestWriteGateSpan_无进化引擎仍写轨迹(t *testing.T) {
	state := t.TempDir()
	ts := tracestore.New(statestore.NewFileStore(filepath.Join(state, "statestore")))
	ptm := &ProductionTeamManager{teams: map[string]*ProductionTeam{}, traceStore: ts}
	team := &ProductionTeam{Name: "tm", Cwd: state}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-h"})

	ptm.recordGateReward(ctx, team, RewardSourceGateCompile, "")
	spans := readSpans(t, ts, "run-h")
	if len(spans) != 1 || spans[0].Kind != tracestore.KindGate {
		t.Fatalf("未装 Evolution 也应写 gate Span, got %+v", spans)
	}
	if spans[0].Attrs["status"] != "pass" {
		t.Errorf("通过的门禁 status 应为 pass, got %v", spans[0].Attrs["status"])
	}
}

// 未装 TraceStore 时是零成本 no-op, 不 panic。
func TestWriteGateSpan_未装轨迹底座空安全(t *testing.T) {
	ptm, team, _, _ := newRewardPTM(t, false)
	ptm.writeGateSpan(nil, team, gateSpanInput{Gate: "gate.x"})
	ptm.writeGateSpan(context.Background(), nil, gateSpanInput{Gate: "gate.x"})
	var nilPTM *ProductionTeamManager
	nilPTM.writeGateSpan(context.Background(), team, gateSpanInput{Gate: "gate.x"})
}

// 跳过的门禁记 skipped 轨迹但不发奖励: 轨迹记事实, 奖励记证据。
func TestWriteGateSpan_跳过记轨迹不发奖励(t *testing.T) {
	ptm, team, state, ts := newRewardPTM(t, true)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-s"})
	ptm.writeGateSpan(ctx, team, gateSpanInput{
		Gate: RewardSourceGateCompile, Node: RewardSourceGateCompile,
		Input: team.Cwd, Detail: "无 go.mod", Skipped: true,
	})
	spans := readSpans(t, ts, "run-s")
	if len(spans) != 1 || spans[0].Attrs["status"] != "skipped" {
		t.Fatalf("应有一条 status=skipped 的 Span, got %+v", spans)
	}
	if evs := readRewards(t, state); len(evs) != 0 {
		t.Fatalf("跳过的门禁不该发奖励 (没跑过就不是证据), got %+v", evs)
	}
}

// gateStatusLabel 的三态。
func TestGateStatusLabel(t *testing.T) {
	if got := gateStatusLabel(gateSpanInput{Skipped: true, Pass: true}); got != "skipped" {
		t.Errorf("skipped 应优先于 pass, got %q", got)
	}
	if got := gateStatusLabel(gateSpanInput{Pass: true}); got != "pass" {
		t.Errorf("got %q", got)
	}
	if got := gateStatusLabel(gateSpanInput{}); got != "fail" {
		t.Errorf("got %q", got)
	}
}

// --- 端到端: 新奖励源确实能被读侧聚合到 ---

// 写了但聚合不到是本仓的经典失效模式, 这里正向验证一遍。
func TestNewRewardSources_可被AggregateRewards聚合(t *testing.T) {
	ptm, team, _, _ := newRewardPTM(t, false)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-agg"})

	ptm.recordSteerReward(team, "run-agg", "不够具体", "impl")
	recordReviewPanelReward(ctx, ptm.evolution, team, 90, 0.9)
	ptm.recordLatencyReward(ctx, team, latencyHardSec+1)

	agg := ptm.evolution.AggregateRewards(RewardQuery{RunID: "run-agg", Team: "tm"})
	if agg.Count != 3 {
		t.Fatalf("三条新源奖励都该被聚合到, got Count=%d sources=%v", agg.Count, agg.Sources)
	}
	for _, src := range []string{RewardSourceUserSteer, RewardSourceReviewPanel, RewardSourceLatency} {
		if agg.Sources[src] == 0 {
			t.Errorf("源 %q 没被聚合到: %v", src, agg.Sources)
		}
	}
	// 阶段级聚合只认同节点的证据。
	if s, ok := ptm.evolution.StageRewardScore("run-agg", "tm", "impl"); !ok || s >= 0 {
		t.Errorf("impl 阶段只有 steer 负信号, 应为负: score=%.3f ok=%v", s, ok)
	}
}
