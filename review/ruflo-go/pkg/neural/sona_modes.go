// 本文件定义 SONA 运行模式枚举及每种模式对应的 SONAConfig 数值预设（学习率、LoRA 秩、EWC λ、缓冲与模式上限等）。
// 与 sona.go 中 SetMode 字符串分支互补：SONAConfigForMode 供需要显式枚举 API 的调用方使用。
package neural

// SONAMode 为 SONA 预设调参配置的逻辑名称。
type SONAMode string

const (
	SONAModeRealTime SONAMode = "realtime"
	SONAModeBalanced SONAMode = "balanced"
	SONAModeResearch SONAMode = "research"
	SONAModeEdge     SONAMode = "edge"
	SONAModeBatch    SONAMode = "batch"
)

// SONAConfigForMode 返回模式对应超参；未知模式回退 DefaultSONAConfig()（balanced）。
// real-time：快反应、较小轨迹、中等信号缓冲；research：低学习率、强 EWC、大缓冲；edge：强资源约束；batch：大批次友好。
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
