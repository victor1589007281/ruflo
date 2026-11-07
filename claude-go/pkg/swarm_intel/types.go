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
	OutcomeType string   `json:"outcome_type"` // binary / categorical / numeric / scenario
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
	ID             string              `json:"id,omitempty"` // persistResults 落库时盖 pred-<unixms>, 供外部按 ID 核销 (RegisterResolution)
	Question       string              `json:"question"`
	Outcomes       []OutcomePrediction `json:"outcomes"`
	Consensus      float64             `json:"consensus"`   // 0-1 共识度
	BrierScore     float64             `json:"brier_score"` // 校准分数
	Rounds         int                 `json:"rounds"`
	Method         string              `json:"method"`
	Summary        string              `json:"summary"`
	Agents         []AgentPrediction   `json:"agents"`
	Extremization  float64             `json:"extremization,omitempty"` // E2/E4: judge 给出的极端化强度 0~0.3 (实际生效值, 校准不足时为 0)
	PheromoneState map[string]float64  `json:"pheromone_state,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
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
	Mode      string   `json:"mode"` // social / game / montecarlo
	Agents    int      `json:"agents"`
	Rounds    int      `json:"rounds"`
	Scenarios []string `json:"scenarios"`
	StopCond  string   `json:"stop_condition,omitempty"`
	// Detail 引擎档位: "kernel"(默认) 走结构化轮次内核
	// (social/game/crisis/org/creative/market/policy/tech 八种模式);
	// "fast" 各模式旧的单 prompt 叙事回退。montecarlo 忽略此字段。
	Detail string `json:"detail,omitempty"`
	// 结构化内核扩展参数 (Detail=kernel 时生效, 零值走内核保守默认):
	// Topology=ring/smallworld 用环格/小世界影响图替代完全图;
	// Rewire 每轮后按立场同质性重连; MemoryRounds 把邻居立场史带入轮内 prompt;
	// Epsilon/EntropyDelta/MinRounds 是自适应终止判据。
	Topology     string  `json:"topology,omitempty"`
	Rewire       bool    `json:"rewire,omitempty"`
	MemoryRounds int     `json:"memory_rounds,omitempty"`
	Epsilon      float64 `json:"epsilon,omitempty"`
}

// SimulationResult 模拟结果。
type SimulationResult struct {
	Mode      string            `json:"mode"`
	Rounds    int               `json:"rounds"`
	Scenarios []ScenarioOutcome `json:"scenarios"`
	Emergent  []string          `json:"emergent_behaviors,omitempty"`
	Summary   string            `json:"summary"`
	CreatedAt time.Time         `json:"created_at"`

	// Phase 2 (social kernel): 可选的立场轨迹与指标曲线,
	// 仅 kernel 模式产出; 其他模式留空, 旧消费者不受影响。
	Trajectory   map[string][]float64 `json:"trajectory,omitempty"`    // agentID → 每轮立场
	MetricCurves map[string][]float64 `json:"metric_curves,omitempty"` // variance/entropy/clustering → 每轮值

	// KernelDetail 结构化内核运行过程明细 (仅 detail=kernel 产出):
	// agent 设计 (spawn 画像+初末立场)、逐轮事件 (action/stance/emergence)、
	// prompt 设计与内核参数。前端"过程明细"页的数据源。
	KernelDetail *KernelDetail `json:"kernel_detail,omitempty"`
}

// KernelDetail 结构化轮次内核的运行过程明细。
type KernelDetail struct {
	Cfg     KernelCfgView       `json:"cfg"`     // 内核参数 (收敛判据相关的可调项)
	Prompts KernelPrompts       `json:"prompts"` // prompt 设计 (实际发出的 spawn + 轮内模板)
	Agents  []KernelAgentDesign `json:"agents"`  // spawn 生成的 agent 画像 + 初末立场
	Events  []KernelEvent       `json:"events"`  // 逐轮事件: action → FJ更新(stance) → 涌现检测
	// 自适应终止: 实际跑的轮数与停止原因 (budget=跑满预算帽才停, 其余=提前收敛)。
	StopReason string `json:"stop_reason,omitempty"`
	RoundsRun  int    `json:"rounds_run,omitempty"`
	// 网络拓扑: 影响图的边集 (full=完全图; ring/smallworld=环格/小世界; Rewire 后有重连事件)。
	Topology *KernelTopology `json:"topology,omitempty"`
}

// KernelTopology 一次模拟的影响图快照。边权重=基础拓扑权重×提及加成,
// rewire=true 时边集随轮次演化 (rewire 事件见 Events)。
type KernelTopology struct {
	Type   string       `json:"type"`        // full | ring | smallworld
	K      int          `json:"k,omitempty"` // 环格邻接度数
	Rewire bool         `json:"rewire,omitempty"`
	Edges  []KernelEdge `json:"edges"` // 终态 (含初态+全部重连后的) 边集
}

// KernelEdge 单条影响边。
type KernelEdge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Weight float64 `json:"weight"`
}

// KernelCfgView 内核参数快照。
type KernelCfgView struct {
	NumAgents   int     `json:"num_agents"`
	Rounds      int     `json:"rounds"`
	HistoryK    int     `json:"history_k"`    // 每轮携带的最近环境消息条数
	MaxConcur   int     `json:"max_concur"`   // 轮内 LLM 并发
	DefaultStub float64 `json:"default_stub"` // spawn 未给 stubbornness 时的默认固执度
	// 自适应终止判据 (①): 全员 |Δstance| < Epsilon ⇒ converged;
	// 熵变化 < EntropyDelta 连续 2 轮 ⇒ entropy_plateau; 都不满足跑满 Rounds ⇒ budget。
	Epsilon      float64 `json:"epsilon,omitempty"`
	EntropyDelta float64 `json:"entropy_delta,omitempty"`
	MinRounds    int     `json:"min_rounds,omitempty"`
	// 拓扑与记忆 (③)
	Topology     string `json:"topology,omitempty"`      // full | ring | smallworld
	TopoK        int    `json:"topo_k,omitempty"`        // 环格度数
	Rewire       bool   `json:"rewire,omitempty"`        // 每轮后同质性重连
	MemoryRounds int    `json:"memory_rounds,omitempty"` // 邻居立场历史带入轮内 prompt 的轮数
}

// KernelPrompts prompt 设计快照: spawn 是实际发出的完整 prompt (含议题),
// round_template 是轮内决策 prompt 的静态模板段 (动态部分=身份/立场/邻居/环境消息)。
type KernelPrompts struct {
	Spawn         string `json:"spawn"`
	RoundTemplate string `json:"round_template"`
}

// KernelAgentDesign 单个 agent 的设计档案: spawn 时 LLM 生成的画像 + 数值属性,
// 以及模拟结束后的最终立场 (轨迹图的单 agent 文字版)。
type KernelAgentDesign struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Profile       string  `json:"profile"`        // 身份+性格一句话
	InitialStance float64 `json:"initial_stance"` // spawn 时的立场
	FinalStance   float64 `json:"final_stance"`   // 全部轮次后的立场
	Stubbornness  float64 `json:"stubbornness"`   // 固执度 0..1 (FJ 模型 stub 参数)
}

// KernelEvent 内核单条事件。Type=action (agent 决策) / stance (FJ 更新后立场) /
// emergence (涌现检测触发)。字段按 Type 取用:
//   - action:    AgentID/Name/Action/Content/StanceDelta
//   - stance:    AgentID/Stance (更新后的新立场)
//   - emergence: Kind/Evidence (带前后指标的判定证据)
type KernelEvent struct {
	Round       int     `json:"round"`
	Type        string  `json:"type"`
	AgentID     string  `json:"agent_id,omitempty"`
	Name        string  `json:"name,omitempty"`
	Action      string  `json:"action,omitempty"`
	Content     string  `json:"content,omitempty"`
	StanceDelta float64 `json:"stance_delta,omitempty"`
	Stance      float64 `json:"stance,omitempty"`
	Kind        string  `json:"kind,omitempty"`
	Evidence    string  `json:"evidence,omitempty"`
}

// ScenarioOutcome 单个场景分支的结果。
type ScenarioOutcome struct {
	Name        string   `json:"name"`
	Probability float64  `json:"probability"`
	Description string   `json:"description"`
	KeyEvents   []string `json:"key_events,omitempty"`
}

// LLMClient LLM 调用接口 (与 agent 包保持一致)。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// NotifyFunc 进度通知回调。
type NotifyFunc func(chatID, message string)
