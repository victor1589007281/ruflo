// Package swarm_intel 群体智能引擎。
// 基于 Boids 协调 + 信素记忆 + 多 Agent 辩论 + 贝叶斯融合的预测与模拟系统。
//
// 学术参考:
//   - Boids (Reynolds 1987): 对齐/分离/聚合 → 信念空间多样性控制
//   - ACO (Dorigo 1997): 信素记忆 + 路径强化
//   - Multi-Agent Debate (Du et al. 2023): 多轮辩论提升推理准确性
//   - Bayesian Belief Fusion: 对数意见池融合多源概率估计
//   - Delphi Method: 多轮匿名迭代收敛
//   - Society of Mind (Minsky 1986): 角色化子 Agent 分工协作
package swarm_intel

import (
	"context"
	"time"
)

// PredictionDomain 将任意目标结构化为可预测问题。
type PredictionDomain struct {
	Question    string   `json:"question"`
	Horizon     string   `json:"horizon"`      // short / medium / long
	OutcomeType string   `json:"outcome_type"`  // binary / categorical / numeric / scenario
	Outcomes    []string `json:"outcomes"`
	Context     string   `json:"context"`
	Constraints []string `json:"constraints,omitempty"`
}

// AgentPrediction 单个 Agent 的预测结果。
type AgentPrediction struct {
	AgentID     string             `json:"agent_id"`
	AgentRole   string             `json:"agent_role"`
	Predictions map[string]float64 `json:"predictions"` // outcome → probability
	Confidence  float64            `json:"confidence"`
	Rationale   string             `json:"rationale"`
	Evidence    []string           `json:"evidence,omitempty"`
	Round       int                `json:"round"`
}

// FusedPrediction 融合后的最终预测。
type FusedPrediction struct {
	Question    string                `json:"question"`
	Outcomes    []OutcomePrediction   `json:"outcomes"`
	Consensus   float64               `json:"consensus"`    // 0-1 共识度
	BrierScore  float64               `json:"brier_score"`  // 校准分数
	Rounds      int                   `json:"rounds"`
	Method      string                `json:"method"`
	Summary     string                `json:"summary"`
	Agents      []AgentPrediction     `json:"agents"`
	PheromoneState map[string]float64 `json:"pheromone_state,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
}

// OutcomePrediction 单个结果的融合概率。
type OutcomePrediction struct {
	Outcome     string  `json:"outcome"`
	Probability float64 `json:"probability"`
	Lower95     float64 `json:"lower_95"` // 95% 置信区间下界
	Upper95     float64 `json:"upper_95"` // 95% 置信区间上界
}

// BoidsConfig Boids 协调参数。
type BoidsConfig struct {
	AlignmentWeight  float64 `json:"alignment_weight"`  // 证据对齐 (默认 0.3)
	SeparationWeight float64 `json:"separation_weight"` // 假设分离 (默认 0.4)
	CohesionWeight   float64 `json:"cohesion_weight"`   // 共识聚合 (默认 0.3)
	MaxDivergence    float64 `json:"max_divergence"`    // 触发额外辩论的分歧阈值
}

// DefaultBoidsConfig 返回默认 Boids 参数。
func DefaultBoidsConfig() BoidsConfig {
	return BoidsConfig{
		AlignmentWeight:  0.3,
		SeparationWeight: 0.4,
		CohesionWeight:   0.3,
		MaxDivergence:    0.6,
	}
}

// PheromoneTrail 信素路径 (ACO 启发)。
type PheromoneTrail struct {
	HypothesisID string    `json:"hypothesis_id"`
	Hypothesis   string    `json:"hypothesis"`
	Strength     float64   `json:"strength"`
	Supporters   int       `json:"supporters"`
	Evidence     []string  `json:"evidence,omitempty"`
	LastUpdate   time.Time `json:"last_update"`
	DecayRate    float64   `json:"decay_rate"`
}

// SimulationConfig 场景模拟配置。
type SimulationConfig struct {
	Mode      string   `json:"mode"`      // social / game / montecarlo
	Agents    int      `json:"agents"`
	Rounds    int      `json:"rounds"`
	Scenarios []string `json:"scenarios"`
	StopCond  string   `json:"stop_condition,omitempty"`
}

// SimulationResult 模拟结果。
type SimulationResult struct {
	Mode       string                `json:"mode"`
	Rounds     int                   `json:"rounds"`
	Scenarios  []ScenarioOutcome     `json:"scenarios"`
	Emergent   []string              `json:"emergent_behaviors,omitempty"`
	Summary    string                `json:"summary"`
	CreatedAt  time.Time             `json:"created_at"`
}

// ScenarioOutcome 单个场景分支的结果。
type ScenarioOutcome struct {
	Name        string  `json:"name"`
	Probability float64 `json:"probability"`
	Description string  `json:"description"`
	KeyEvents   []string `json:"key_events,omitempty"`
}

// LLMClient LLM 调用接口 (与 agent 包保持一致)。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// NotifyFunc 进度通知回调。
type NotifyFunc func(chatID, message string)
