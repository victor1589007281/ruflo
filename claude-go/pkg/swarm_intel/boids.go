package swarm_intel

import (
	"math"
)

// BoidsCoordinator 实现 Boids 风格的信念空间协调。
//
// 三条规则映射到预测场景:
//   - Alignment (对齐): Agent 共享并同步关键证据，确保在相同信息基础上推理
//   - Separation (分离): 预测过于一致时强制多样性，避免群体盲区
//   - Cohesion (聚合): 预测不应偏离合理区间，保持在证据支撑范围内
type BoidsCoordinator struct {
	config BoidsConfig
}

// NewBoidsCoordinator 创建 Boids 协调器。
func NewBoidsCoordinator(cfg BoidsConfig) *BoidsCoordinator {
	return &BoidsCoordinator{config: cfg}
}

// MeasureDivergence 计算多个 Agent 预测之间的分歧度 (Jensen-Shannon Divergence 均值)。
// 返回 0 表示完全一致, 1 表示极度分歧。
//
// 算法解读：
//
//	原理：JSD = 各分布与"平均分布"之间 KL 散度的均值。相比直接两两算 KL, 它对平均分布取散度,
//	      于是**对称**且天然有界, 适合当作"群体是否分歧"的单一标量。
//	关键不变量：概率必须先 clampProb 再取 log —— 分布里出现 0 时 log2(0) = -Inf 会把整个
//	      分歧度污染成 NaN; 夹到 1e-10 是这里唯一挡得住它的地方。
//	代价与边界：O(预测数 × 结果标签数)。预测数 <2 或没有任何结果标签时直接返回 0 (无从谈
//	      分歧); 返回值再夹到 [0,1] —— 用 log2 算出的 JSD 上界本就是 1。
//	设计权衡：用 JSD 而不是投票熵: 后者只看最终选项分布, 看不见"两边都给同一选项 0.5 概率"
//	      这种一致, 会把高共识误判成高分歧。
func (bc *BoidsCoordinator) MeasureDivergence(predictions []AgentPrediction) float64 {
	if len(predictions) < 2 {
		return 0
	}

	outcomes := collectOutcomes(predictions)
	if len(outcomes) == 0 {
		return 0
	}

	avgDist := make(map[string]float64, len(outcomes))
	for _, o := range outcomes {
		for _, p := range predictions {
			avgDist[o] += p.Predictions[o]
		}
		avgDist[o] /= float64(len(predictions))
	}

	var totalJSD float64
	for _, p := range predictions {
		var kl float64
		for _, o := range outcomes {
			// 先夹再取 log: 分布里的 0 会让 log2 变成 -Inf, 整个分歧度就废了。
			pi := clampProb(p.Predictions[o])
			mi := clampProb(avgDist[o])
			if pi > 0 && mi > 0 {
				kl += pi * math.Log2(pi/mi)
			}
		}
		totalJSD += kl
	}

	jsd := totalJSD / float64(len(predictions))
	return math.Min(1.0, jsd)
}

// NeedMoreDebate 检查分歧度是否超过阈值,需要额外辩论轮。
func (bc *BoidsCoordinator) NeedMoreDebate(predictions []AgentPrediction) bool {
	return bc.MeasureDivergence(predictions) > bc.config.MaxDivergence
}

// MeasureConsensus 计算共识度 (1 - divergence)。
func (bc *BoidsCoordinator) MeasureConsensus(predictions []AgentPrediction) float64 {
	return 1.0 - bc.MeasureDivergence(predictions)
}

// DiversityCheck 检查预测多样性是否足够 (Separation 规则)。
// 返回需要强制差异化的 Agent 对。
func (bc *BoidsCoordinator) DiversityCheck(predictions []AgentPrediction) []string {
	var tooSimilar []string
	threshold := 0.05

	for i := 0; i < len(predictions); i++ {
		for j := i + 1; j < len(predictions); j++ {
			sim := predictionSimilarity(predictions[i], predictions[j])
			if sim > (1.0 - threshold) {
				tooSimilar = append(tooSimilar,
					predictions[i].AgentID+" ↔ "+predictions[j].AgentID)
			}
		}
	}
	return tooSimilar
}

// predictionSimilarity 计算两个预测的概率分布相似度 (余弦相似度)。
func predictionSimilarity(a, b AgentPrediction) float64 {
	outcomes := make(map[string]bool)
	for k := range a.Predictions {
		outcomes[k] = true
	}
	for k := range b.Predictions {
		outcomes[k] = true
	}

	var dotProduct, normA, normB float64
	for o := range outcomes {
		va := a.Predictions[o]
		vb := b.Predictions[o]
		dotProduct += va * vb
		normA += va * va
		normB += vb * vb
	}

	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// collectOutcomes 汇总所有预测出现过的结果标签 (并集)。
func collectOutcomes(predictions []AgentPrediction) []string {
	set := make(map[string]bool)
	for _, p := range predictions {
		for o := range p.Predictions {
			set[o] = true
		}
	}
	out := make([]string, 0, len(set))
	for o := range set {
		out = append(out, o)
	}
	return out
}

// clampProb 把概率夹到 (1e-10, 1-1e-10), 避免 JSD 里出现 log(0)。
func clampProb(p float64) float64 {
	if p < 1e-10 {
		return 1e-10
	}
	if p > 1 - 1e-10 {
		return 1 - 1e-10
	}
	return p
}
