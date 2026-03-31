package neural

// SONAMode selects preset SONA tuning profiles.
type SONAMode string

const (
	SONAModeRealTime SONAMode = "realtime"
	SONAModeBalanced SONAMode = "balanced"
	SONAModeResearch SONAMode = "research"
	SONAModeEdge     SONAMode = "edge"
	SONAModeBatch    SONAMode = "batch"
)

// SONAConfigForMode returns parameters for the named mode; unknown modes fall back to balanced.
func SONAConfigForMode(mode SONAMode) SONAConfig {
	switch mode {
	case SONAModeRealTime:
		return SONAConfig{
			LearningRate:      0.12,
			LoraRank:          4,
			EWCLambda:         0.2,
			MaxTrajectorySize: 64,
			MaxSignals:        256,
			MaxPatterns:       2048,
		}
	case SONAModeResearch:
		return SONAConfig{
			LearningRate:      0.03,
			LoraRank:          32,
			EWCLambda:         0.55,
			MaxTrajectorySize: 512,
			MaxSignals:        2048,
			MaxPatterns:       8192,
		}
	case SONAModeEdge:
		return SONAConfig{
			LearningRate:      0.08,
			LoraRank:          4,
			EWCLambda:         0.35,
			MaxTrajectorySize: 128,
			MaxSignals:        128,
			MaxPatterns:       512,
		}
	case SONAModeBatch:
		return SONAConfig{
			LearningRate:      0.04,
			LoraRank:          12,
			EWCLambda:         0.45,
			MaxTrajectorySize: 384,
			MaxSignals:        4096,
			MaxPatterns:       16384,
		}
	default:
		return DefaultSONAConfig()
	}
}
