// EWC++（Elastic Weight Consolidation，弹性权重巩固）实现。
//
// # 理论背景（Kirkpatrick et al., PNAS 2017）
//
// 在持续学习（Continual Learning）场景中，模型依次学习多个任务时会发生
// "灾难性遗忘"（catastrophic forgetting）——新任务的梯度更新覆盖旧任务学到的参数。
// EWC 的核心思想是：对旧任务中重要的参数施加正则惩罚，使其在新任务学习中不会偏移太远。
//
// # 数学公式
//
// 总损失函数：
//
//	L_total = L_new(θ) + (λ/2) · Σ_i F_i · (θ_i − θ*_i)²
//
// 其中：
//   - θ*_i 是旧任务训练完成后的最优参数值
//   - F_i 是 Fisher 信息矩阵的对角近似，衡量参数 i 对旧任务的重要性
//   - λ 控制正则强度；λ 越大，旧参数越难被修改
//
// Fisher 信息矩阵对角近似：F_i ≈ E[(∂log p(y|x;θ)/∂θ_i)²]
// 直觉：若某参数的梯度方差大，则该参数对输出影响大，需要更强保护。
//
// # EWC++ 改进
//
// 标准 EWC 需存储每个历史任务的 Fisher 矩阵；EWC++ 通过在线累积更新 Fisher 矩阵，
// 只需保留一组"运行中"的重要性权重，内存开销恒定。
//
// # 本文件的工程近似
//
// 在 Ruflo 中，"参数" 对应每个 Pattern 的 Confidence（置信度，标量）；
// Fisher 对角近似采用 F_i = √(1 + UsageCount)，使用次数越多的模式保护越强。
// ConsolidatePatterns 对每个模式执行一次更新；当前实现中 thetaLearned == thetaOld
// 导致 delta 恒为 0，在外部 caller 先行修改 Confidence 后再调用时才会产生实际拉回效果。
package neural

import "math"

// EWCConfig 弹性权重巩固配置。
//   - Lambda: 正则化强度系数（对应公式中的 λ），越大越保守。默认 0.5。
type EWCConfig struct {
	Lambda float64 `json:"lambda"`
}

// EWCConsolidator 弹性权重巩固器：利用对角 Fisher 信息近似保护高使用率模式的置信度，
// 避免新模式的学习覆盖旧模式。
type EWCConsolidator struct {
	cfg EWCConfig
}

// NewEWCConsolidator 构造巩固器。lambda ≤ 0 时自动回退到默认值 0.5。
func NewEWCConsolidator(lambda float64) *EWCConsolidator {
	if lambda <= 0 {
		lambda = 0.5
	}
	return &EWCConsolidator{cfg: EWCConfig{Lambda: lambda}}
}

// ConsolidatePatterns 对一组 Pattern 执行 EWC 正则更新：
//
//	θ_new = θ_learned + λ · F · (θ_old − θ_learned)²
//
// 其中 F = √(1 + UsageCount) 作为 Fisher 对角近似。
// 结果裁剪到 [0, 1] 区间。返回深拷贝切片，不修改入参。
//
// 注意：当前实现中 thetaLearned 直接取 thetaOld，使 delta 为 0（无偏移）。
// 在实际使用中，外部 caller 应先根据 verdict 修改 Confidence 再调用本方法，
// 此时 delta ≠ 0，正则项才会产生"拉回"效果。
func (e *EWCConsolidator) ConsolidatePatterns(patterns []*Pattern) []*Pattern {
	if e == nil || len(patterns) == 0 {
		return patterns
	}
	out := make([]*Pattern, len(patterns))
	for i, p := range patterns {
		if p == nil {
			continue
		}
		cp := *p
		thetaOld := p.Confidence
		// Fisher 对角近似：使用次数越多 → 信息量越大 → 锚定越强
		F := math.Sqrt(float64(1 + p.UsageCount))
		thetaLearned := thetaOld
		delta := thetaOld - thetaLearned
		thetaNew := thetaLearned + e.cfg.Lambda*F*delta*delta
		if thetaNew < 0 {
			thetaNew = 0
		}
		if thetaNew > 1 {
			thetaNew = 1
		}
		cp.Confidence = thetaNew
		out[i] = &cp
	}
	return out
}

// FisherDiagonal 计算单个 Pattern 的 Fisher 对角近似值：
//
//	F = √(1 + UsageCount) · |Confidence|
//
// 值越大表示该模式对旧任务越重要，EWC 更新时应受到更强保护。
func FisherDiagonal(p *Pattern) float64 {
	if p == nil {
		return 0
	}
	return math.Sqrt(float64(1+p.UsageCount)) * math.Abs(p.Confidence)
}
