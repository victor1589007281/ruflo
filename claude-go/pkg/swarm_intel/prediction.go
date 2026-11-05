package swarm_intel

import (
	"math"
	"time"
)

// Fuser 预测融合器。
// 实现对数意见池 (Logarithmic Opinion Pool) + Brier Score 校准。
type Fuser struct {
	boids *BoidsCoordinator
}

// NewFuser 创建预测融合器。
func NewFuser(boids *BoidsCoordinator) *Fuser {
	return &Fuser{boids: boids}
}

// Fuse 将多个 Agent 的预测融合为最终预测 (权重 = 纯 confidence, 无信任输入)。
//
// 算法: 对数意见池 (Log Opinion Pool)
//
//	log P*(outcome) ∝ Σ w_i × log P_i(outcome)
//
// 其中 w_i 由 Agent 的 Confidence 归一化得到。
func (f *Fuser) Fuse(domain PredictionDomain, agentPreds []AgentPrediction) *FusedPrediction {
	return f.fuse(domain, agentPreds, nil)
}

// FuseWithTrust 同 Fuse, 但权重 = confidence × 信任分 (E3 在线权重)。
// trust 来自 ByzantineFuser.SnapshotTrust: 无信任记录的 agent 取 1.0,
// 与 v2 Fuse 输出一致; 信任分不同的 agent 会拿到不同的融合权重。
func (f *Fuser) FuseWithTrust(domain PredictionDomain, agentPreds []AgentPrediction, trust map[string]float64) *FusedPrediction {
	return f.fuse(domain, agentPreds, trust)
}

// fuse 融合本体: trust 为 nil/空时权重即纯 confidence (v2 行为)。
func (f *Fuser) fuse(domain PredictionDomain, agentPreds []AgentPrediction, trust map[string]float64) *FusedPrediction {
	if len(agentPreds) == 0 {
		return &FusedPrediction{
			Question:  domain.Question,
			Method:    "none",
			CreatedAt: time.Now(),
		}
	}

	weights := f.computeWeights(agentPreds, trust)

	logProbs := make(map[string]float64)
	for _, o := range domain.Outcomes {
		for i, p := range agentPreds {
			prob := clampProb(p.Predictions[o])
			logProbs[o] += weights[i] * math.Log(prob)
		}
	}

	rawProbs := make(map[string]float64)
	var totalProb float64
	for o, lp := range logProbs {
		rawProbs[o] = math.Exp(lp)
		totalProb += rawProbs[o]
	}

	outcomes := make([]OutcomePrediction, 0, len(domain.Outcomes))
	for _, o := range domain.Outcomes {
		prob := rawProbs[o] / totalProb
		lower, upper := wilsonInterval(prob, len(agentPreds))
		outcomes = append(outcomes, OutcomePrediction{
			Outcome:     o,
			Probability: prob,
			Lower95:     lower,
			Upper95:     upper,
		})
	}

	maxRound := 0
	for _, p := range agentPreds {
		if p.Round > maxRound {
			maxRound = p.Round
		}
	}

	return &FusedPrediction{
		Question:  domain.Question,
		Outcomes:  outcomes,
		Consensus: f.boids.MeasureConsensus(agentPreds),
		Rounds:    maxRound,
		Method:    "log_opinion_pool",
		Agents:    agentPreds,
		CreatedAt: time.Now(),
	}
}

// computeWeights 基于 Confidence × 信任分计算归一化权重 (E3 在线权重)。
//
// 算法解读：
//
//	原理：w_i = max(0.1, confidence_i × trust_i) 后归一化 —— 信任分是 ByzantineFuser
//	      从历次 RecordOutcome/RegisterResolution 学来的命中率画像, 高信任的 agent
//	      在融合里音量更大; 下限 0.1 保证任何 agent 永不失语 (防止单 agent 垄断,
//	      与 v2 的 max(0.1, confidence) 同一道护栏)。
//	关键不变量：trust 无该 agent 记录时必须取 1.0 而不是默认信任 0.5 —— 共享域
//	      (aiops) 从不产生信任分, 若取 0.5 会把 v2 输出整体拉偏, 零回归承诺就破了。
//	代价与边界：信任分是 EWMA 类小步长更新的慢变量, 单次误判最多把权重拉移 (0.2×0.1)
//	      量级; 恶意刷信任需要连续多次押中, 成本随次数线性增长。
func (f *Fuser) computeWeights(preds []AgentPrediction, trust map[string]float64) []float64 {
	weights := make([]float64, len(preds))
	var sum float64
	for i, p := range preds {
		t, ok := trust[p.AgentID]
		if !ok {
			t = 1.0 // 无信任记录 = 中性, 与 v2 confidence 权重逐字节一致
		}
		w := math.Max(0.1, p.Confidence*t)
		weights[i] = w
		sum += w
	}
	for i := range weights {
		weights[i] /= sum
	}
	return weights
}

// extremizeProb E4: 按 judge 给的 extremization 强度把融合概率推离 0.5 —
// p' = clip(0.5 + (p-0.5)×(1+2×ext), 0.01, 0.99)。
// 依据 extremization 文献 (Larrick & Soll 等): 独立判断的群体误差项近似抵消,
// 保留群体均值之上的"独立信息分量"应把概率往群体一致方向再推一步。
// 注意多结果分布经此逐维变换后不再严格归一 —— 这是公式的有意取舍 (二元场景精确成立)。
func extremizeProb(p, extremization float64) float64 {
	p2 := 0.5 + (p-0.5)*(1+2*extremization)
	return math.Max(0.01, math.Min(0.99, p2))
}

// agentEntropy 单个 agent 预测分布的归一化 Shannon 熵 (0=笃定, 1=均匀分布)。
// E5 AUQ 分流阈值用它识别"分布犹疑"的 agent; 与 DebateGate 的聚合熵口径一致
// (同一归一化), 只是作用域从群体收窄到单个 agent。
func agentEntropy(p AgentPrediction, outcomes []string) float64 {
	h := 0.0
	for _, o := range outcomes {
		prob := p.Predictions[o]
		if prob > 0 {
			h -= prob * math.Log2(prob)
		}
	}
	maxH := math.Log2(float64(len(outcomes)))
	if maxH <= 0 {
		return 0
	}
	return h / maxH
}

// wilsonInterval 计算 Wilson Score 95% 置信区间。
func wilsonInterval(p float64, n int) (float64, float64) {
	if n == 0 {
		return 0, 1
	}
	z := 1.96 // 95% CI
	nf := float64(n)
	denominator := 1 + z*z/nf
	centre := p + z*z/(2*nf)
	spread := z * math.Sqrt((p*(1-p)+z*z/(4*nf))/nf)

	lower := (centre - spread) / denominator
	upper := (centre + spread) / denominator

	return math.Max(0, lower), math.Min(1, upper)
}

// ComputeBrierScore 计算 Brier Score (事后校准用)。
// actual: 实际发生的结果 ID
func ComputeBrierScore(pred *FusedPrediction, actual string) float64 {
	var brier float64
	for _, o := range pred.Outcomes {
		expected := 0.0
		if o.Outcome == actual {
			expected = 1.0
		}
		diff := o.Probability - expected
		brier += diff * diff
	}
	return brier
}
