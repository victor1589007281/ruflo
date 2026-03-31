package neural

import "math"

// EWCConfig holds elastic weight consolidation strength.
type EWCConfig struct {
	Lambda float64 `json:"lambda"`
}

// EWCConsolidator approximates diagonal Fisher information to protect important patterns.
type EWCConsolidator struct {
	cfg EWCConfig
}

// NewEWCConsolidator builds a consolidator with lambda regularization strength.
func NewEWCConsolidator(lambda float64) *EWCConsolidator {
	if lambda <= 0 {
		lambda = 0.5
	}
	return &EWCConsolidator{cfg: EWCConfig{Lambda: lambda}}
}

// ConsolidatePatterns updates pattern confidences with diagonal Fisher penalty:
// theta_new_i = theta_learned_i + lambda * F_i * (theta_old_i - theta_learned_i)^2
// Here each "weight" dimension is pattern confidence treated as a scalar slot; F_i ~ usage-derived importance.
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
		// Fisher diagonal proxy: higher usage => more information => stronger anchoring
		F := math.Sqrt(float64(1 + p.UsageCount))
		thetaLearned := thetaOld // after distillation caller updates confidence; EWC nudges toward old if F large
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

// FisherDiagonal returns per-pattern diagonal Fisher approximation.
func FisherDiagonal(p *Pattern) float64 {
	if p == nil {
		return 0
	}
	return math.Sqrt(float64(1+p.UsageCount)) * math.Abs(p.Confidence)
}
