package swarm_intel

import (
	"math"
	"testing"
)

func rawPreds() []AgentPrediction {
	return []AgentPrediction{
		{AgentID: "a", Predictions: map[string]float64{"plot": 80, "character": 70, "prose": 60}},
		{AgentID: "b", Predictions: map[string]float64{"plot": 82, "character": 71, "prose": 61}},
		{AgentID: "c", Predictions: map[string]float64{"plot": 78, "character": 69, "prose": 59}},
	}
}

// TrimmedFuseRaw 必须保住量纲。改造前 review-panel 走的是归一化那条路:
// 同一份数据 N=2 得 plot=81, N=3 得 plot=38.1 —— dimensions 与同一份 JSON 里的
// overall 不同量纲, 且在 N=2/N=3 之间跳变。
func TestTrimmedFuseRaw_不归一保住量纲(t *testing.T) {
	bf := NewByzantineFuser(0.34)
	raw := bf.TrimmedFuseRaw(rawPreds())
	// 三个样本剔掉最高最低各 1 个 ⇒ 每维剩中位数
	for dim, want := range map[string]float64{"plot": 80, "character": 70, "prose": 60} {
		if math.Abs(raw[dim]-want) > 0.01 {
			t.Errorf("%s = %.2f, 期望 %.2f —— 若得到 ~1/3 那么归一化还在", dim, raw[dim], want)
		}
	}
	sum := 0.0
	for _, v := range raw {
		sum += v
	}
	if math.Abs(sum-1.0) < 0.01 {
		t.Error("各维之和恰为 1 —— 说明仍被当成概率分布归一化了")
	}
}

// N=2 与 N=3 必须同量纲 (改造前正是在这里跳变)。
func TestTrimmedFuseRaw_跨样本数量纲一致(t *testing.T) {
	bf := NewByzantineFuser(0.34)
	p := rawPreds()
	two := bf.TrimmedFuseRaw(p[:2])
	three := bf.TrimmedFuseRaw(p)
	// 两者都该在 0-100 量纲, 差距不该是一个数量级
	if two["plot"] < 50 || three["plot"] < 50 {
		t.Errorf("量纲不一致: N=2 plot=%.2f, N=3 plot=%.2f", two["plot"], three["plot"])
	}
}

// TrimmedFuse 的既有行为一字不改 —— 它另有依赖归一语义的调用方(概率分布场景),
// 一刀切删 normalize 会把这个 bug 换成另一个方向的 bug。
func TestTrimmedFuse_归一行为不变(t *testing.T) {
	bf := NewByzantineFuser(0.34)
	got := bf.TrimmedFuse(rawPreds())
	sum := 0.0
	for _, v := range got {
		sum += v
	}
	if math.Abs(sum-1.0) > 0.01 {
		t.Errorf("N>=3 时 TrimmedFuse 应仍归一(各维之和=1), 实得 %.4f", sum)
	}
	// N<3 走 simpleMerge, 本就不归一 —— 这条既有行为也不能变
	two := bf.TrimmedFuse(rawPreds()[:2])
	if math.Abs(two["plot"]-81) > 0.01 {
		t.Errorf("N<2 时 plot = %.2f, 期望 81 (simpleMerge 不归一)", two["plot"])
	}
}
