package swarm_intel

import (
	"math"
	"testing"
)

func mkPreds() []AgentPrediction {
	return []AgentPrediction{
		{AgentID: "a", Predictions: map[string]float64{"x": 0.7, "y": 0.3}, Confidence: 0.8},
		{AgentID: "b", Predictions: map[string]float64{"x": 0.6, "y": 0.4}, Confidence: 0.7},
		{AgentID: "c", Predictions: map[string]float64{"x": 0.1, "y": 0.9}, Confidence: 0.6}, // 离群
	}
}

// aiops 零回归的关键不变式: trustScores 为空时, TrustWeightedTrimmedFuse ≡ TrimmedFuse。
func TestTrustWeightedFuse_EmptyTrust_IdenticalToTrimmed(t *testing.T) {
	bf := NewByzantineFuser(0.2)
	preds := mkPreds()
	plain := bf.TrimmedFuse(preds)
	weighted := bf.TrustWeightedTrimmedFuse(preds) // trustScores 仍为空
	if len(plain) != len(weighted) {
		t.Fatalf("键数不同: %d vs %d", len(plain), len(weighted))
	}
	for k, v := range plain {
		if math.Abs(v-weighted[k]) > 1e-9 {
			t.Fatalf("空信任下结果应逐字节一致: %s %v vs %v", k, v, weighted[k])
		}
	}
}

// 有学习信任时, 低信任 (<0.15) 的 agent 应被剔除后再融合。
func TestTrustWeightedFuse_ExcludesLowTrust(t *testing.T) {
	bf := NewByzantineFuser(0.2)
	preds := mkPreds()
	// 把 c (离群) 打成低信任, a/b 保持高信任
	bf.RestoreTrust(map[string]float64{"a": 0.8, "b": 0.8, "c": 0.05})
	got := bf.TrustWeightedTrimmedFuse(preds)
	// 剔除 c 后只剩 a,b (均押 x 高), x 应显著高于含 c 时
	if got["x"] <= got["y"] {
		t.Fatalf("剔除低信任离群 agent 后 x 应占优: %v", got)
	}
	if got["x"] < 0.6 {
		t.Fatalf("剔除离群后 x 期望 ≥0.6, 实际 %v", got["x"])
	}
}

// 不过度剪枝: 即使多数低信任, 剩余不足 3 个则保留原集。
func TestTrustWeightedFuse_NoOverPrune(t *testing.T) {
	bf := NewByzantineFuser(0.2)
	preds := mkPreds()
	bf.RestoreTrust(map[string]float64{"a": 0.05, "b": 0.05, "c": 0.05}) // 全低
	got := bf.TrustWeightedTrimmedFuse(preds)
	plain := NewByzantineFuser(0.2).TrimmedFuse(preds)
	for k, v := range plain {
		if math.Abs(v-got[k]) > 1e-9 {
			t.Fatalf("剩余不足 3 应退回原集: %s %v vs %v", k, v, got[k])
		}
	}
}

func TestConformalSnapshotRestore(t *testing.T) {
	c := NewConformalCalibrator(0.05)
	for _, s := range []float64{0.1, 0.2, 0.3} {
		c.AddScore(s)
	}
	snap := c.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("快照应有 3 条, 实际 %d", len(snap))
	}
	c2 := NewConformalCalibrator(0.05)
	c2.Restore(snap)
	if len(c2.Snapshot()) != 3 {
		t.Fatalf("恢复后应有 3 条")
	}
	// 恢复后 quantile 不再是空历史的保守默认 0.15
	c2.mu.Lock()
	q := c2.computeQuantile()
	c2.mu.Unlock()
	if q == 0.15 {
		t.Fatalf("恢复历史后 quantile 不应恒为默认 0.15")
	}
}

func TestRecordOutcome_FeedsConformalAndTrust(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir() // 隔离: 绝不读写共享的 ~/.claude-go/swarm_intel
	e := NewEngine(nil, cfg)
	pred := &FusedPrediction{
		Question: "哪条时间线?",
		Outcomes: []OutcomePrediction{
			{Outcome: "时间线 #1", Probability: 0.6, Lower95: 0.4, Upper95: 0.8},
			{Outcome: "时间线 #2", Probability: 0.4, Lower95: 0.2, Upper95: 0.6},
		},
		Agents: []AgentPrediction{
			{AgentID: "analyst-optimist", Predictions: map[string]float64{"时间线 #1": 0.9, "时间线 #2": 0.1}},
			{AgentID: "analyst-pessimist", Predictions: map[string]float64{"时间线 #1": 0.1, "时间线 #2": 0.9}},
		},
	}
	before := len(e.conformal.Snapshot())
	brier := e.RecordOutcome(pred, "时间线 #1")
	if brier <= 0 {
		t.Fatalf("Brier 应 > 0, 实际 %v", brier)
	}
	if len(e.conformal.Snapshot()) != before+1 {
		t.Fatalf("RecordOutcome 应向保形历史追加 1 条 nonconformity")
	}
	// 押中 #1 的 optimist 信任应升, 押错的 pessimist 应降
	if e.byzantineFuser.GetTrust("analyst-optimist") <= 0.5 {
		t.Fatalf("押中者信任应 > 0.5, 实际 %v", e.byzantineFuser.GetTrust("analyst-optimist"))
	}
	if e.byzantineFuser.GetTrust("analyst-pessimist") >= 0.5 {
		t.Fatalf("押错者信任应 < 0.5, 实际 %v", e.byzantineFuser.GetTrust("analyst-pessimist"))
	}
}
