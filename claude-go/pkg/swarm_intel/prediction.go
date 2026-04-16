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

// Fuse 将多个 Agent 的预测融合为最终预测。
//
// 算法: 对数意见池 (Log Opinion Pool)
//   log P*(outcome) ∝ Σ w_i × log P_i(outcome)
// 其中 w_i 由 Agent 的 Confidence 归一化得到。
func (f *Fuser) Fuse(domain PredictionDomain, agentPreds []AgentPrediction) *FusedPrediction {
	if len(agentPreds) == 0 {
		return &FusedPrediction{
			Question:  domain.Question,
			Method:    "none",
			CreatedAt: time.Now(),
		}
	}

	weights := f.computeWeights(agentPreds)

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

// computeWeights 基于 Confidence 计算归一化权重。
func (f *Fuser) computeWeights(preds []AgentPrediction) []float64 {
	weights := make([]float64, len(preds))
	var sum float64
	for i, p := range preds {
		w := math.Max(0.1, p.Confidence)
		weights[i] = w
		sum += w
	}
	for i := range weights {
		weights[i] /= sum
	}
	return weights
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
