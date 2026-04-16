package swarm_intel

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
)

// ConformalCalibrator 在线保形校准器。
// 源自 ORCA (arXiv:2604.01170) — 在测试时动态校准不确定性。
type ConformalCalibrator struct {
	alpha     float64   // 显著性水平 (默认 0.05)
	scoreHist []float64 // 保形分数历史
	maxHist   int       // 保留的最大历史长度
	mu        sync.Mutex
}

// ConfidenceSet 保形置信集合。
type ConfidenceSet struct {
	Outcomes []OutcomePrediction `json:"outcomes"`
	Width    float64             `json:"width"`   // 区间宽度
	Coverage float64             `json:"coverage"` // 期望覆盖率
}

// NewConformalCalibrator 创建保形校准器。
func NewConformalCalibrator(alpha float64) *ConformalCalibrator {
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.05
	}
	return &ConformalCalibrator{
		alpha:   alpha,
		maxHist: 500,
	}
}

// AddScore 添加历史保形分数 (用于在线更新)。
func (c *ConformalCalibrator) AddScore(score float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scoreHist = append(c.scoreHist, score)
	if len(c.scoreHist) > c.maxHist {
		c.scoreHist = c.scoreHist[len(c.scoreHist)-c.maxHist:]
	}
}

// CalibrateSet 对融合预测进行保形校准，返回调整后的置信区间。
func (c *ConformalCalibrator) CalibrateSet(pred *FusedPrediction) ConfidenceSet {
	c.mu.Lock()
	defer c.mu.Unlock()

	quantile := c.computeQuantile()
	adjusted := make([]OutcomePrediction, len(pred.Outcomes))
	for i, o := range pred.Outcomes {
		adjusted[i] = OutcomePrediction{
			Outcome:     o.Outcome,
			Probability: o.Probability,
			Lower95:     math.Max(0, o.Probability-quantile),
			Upper95:     math.Min(1, o.Probability+quantile),
		}
	}

	width := 2 * quantile
	return ConfidenceSet{
		Outcomes: adjusted,
		Width:    width,
		Coverage: 1 - c.alpha,
	}
}

func (c *ConformalCalibrator) computeQuantile() float64 {
	if len(c.scoreHist) == 0 {
		return 0.15 // 保守默认
	}
	sorted := make([]float64, len(c.scoreHist))
	copy(sorted, c.scoreHist)
	sort.Float64s(sorted)

	idx := int(math.Ceil(float64(len(sorted)+1) * (1 - c.alpha)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// DebateGate 认知不确定性门控。
// 源自 DOWN (arXiv:2504.05047) — 仅在不确定性高时启动辩论, ~6x 效率提升。
type DebateGate struct {
	EntropyThreshold float64 // Shannon 熵阈值
}

// NewDebateGate 创建辩论门控。
func NewDebateGate(threshold float64) *DebateGate {
	if threshold <= 0 {
		threshold = 0.7
	}
	return &DebateGate{EntropyThreshold: threshold}
}

// ShouldDebate 根据预测分布的 Shannon 熵判断是否需要辩论。
func (g *DebateGate) ShouldDebate(predictions []AgentPrediction) bool {
	if len(predictions) < 2 {
		return false
	}

	freqs := make(map[string]float64)
	total := 0.0
	for _, p := range predictions {
		for outcome, prob := range p.Predictions {
			freqs[outcome] += prob
			total += prob
		}
	}
	if total == 0 {
		return true
	}

	entropy := 0.0
	for _, count := range freqs {
		p := count / total
		if p > 0 {
			entropy -= p * math.Log2(p)
		}
	}

	maxEntropy := math.Log2(float64(len(freqs)))
	if maxEntropy == 0 {
		return true
	}
	normalizedEntropy := entropy / maxEntropy

	return normalizedEntropy > g.EntropyThreshold
}

// ByzantineFuser 拜占庭容错融合器。
// 源自 Sci Rep 16:11640 — 防止策略性 Agent 操纵群体预测。
type ByzantineFuser struct {
	trustScores map[string]float64 // Agent → 信任分数
	trimRatio   float64            // 修剪比例 (去掉最极端的)
	mu          sync.Mutex
}

// NewByzantineFuser 创建拜占庭容错融合器。
func NewByzantineFuser(trimRatio float64) *ByzantineFuser {
	if trimRatio <= 0 || trimRatio >= 0.5 {
		trimRatio = 0.2
	}
	return &ByzantineFuser{
		trustScores: make(map[string]float64),
		trimRatio:   trimRatio,
	}
}

// UpdateTrust 根据预测表现更新 Agent 信任分数。
func (bf *ByzantineFuser) UpdateTrust(agentID string, brierDelta float64) {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	current, ok := bf.trustScores[agentID]
	if !ok {
		current = 0.5
	}
	bf.trustScores[agentID] = math.Max(0, math.Min(1, current+brierDelta))
}

// TrimmedFuse 修剪后融合 — 去掉最极端的预测后再融合。
func (bf *ByzantineFuser) TrimmedFuse(predictions []AgentPrediction) map[string]float64 {
	bf.mu.Lock()
	defer bf.mu.Unlock()

	if len(predictions) < 3 {
		return simpleMerge(predictions)
	}

	trimCount := int(float64(len(predictions)) * bf.trimRatio)
	if trimCount < 1 {
		trimCount = 1
	}

	outcomes := collectOutcomes(predictions)
	result := make(map[string]float64)
	for _, outcome := range outcomes {
		vals := make([]float64, len(predictions))
		for i, p := range predictions {
			vals[i] = p.Predictions[outcome]
		}
		sort.Float64s(vals)
		trimmed := vals[trimCount : len(vals)-trimCount]
		if len(trimmed) == 0 {
			trimmed = vals
		}
		sum := 0.0
		for _, v := range trimmed {
			sum += v
		}
		result[outcome] = sum / float64(len(trimmed))
	}

	normalize(result)
	return result
}

// GetTrust 获取 Agent 的信任分数。
func (bf *ByzantineFuser) GetTrust(agentID string) float64 {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	score, ok := bf.trustScores[agentID]
	if !ok {
		return 0.5
	}
	return score
}

func simpleMerge(preds []AgentPrediction) map[string]float64 {
	result := make(map[string]float64)
	for _, p := range preds {
		for k, v := range p.Predictions {
			result[k] += v
		}
	}
	n := float64(len(preds))
	for k := range result {
		result[k] /= n
	}
	return result
}

func normalize(dist map[string]float64) {
	sum := 0.0
	for _, v := range dist {
		sum += v
	}
	if sum > 0 {
		for k := range dist {
			dist[k] /= sum
		}
	}
}

// BanditRouter Thompson Sampling 路由器。
// 源自 REDEREF (arXiv:2603.13256) — 动态路由到最合适的 Agent。
type BanditRouter struct {
	alphas map[string]float64 // Beta 分布 alpha (成功数)
	betas  map[string]float64 // Beta 分布 beta (失败数)
	mu     sync.Mutex
}

// NewBanditRouter 创建 Bandit 路由器。
func NewBanditRouter() *BanditRouter {
	return &BanditRouter{
		alphas: make(map[string]float64),
		betas:  make(map[string]float64),
	}
}

// Register 注册一个 Agent。
func (br *BanditRouter) Register(agentID string) {
	br.mu.Lock()
	defer br.mu.Unlock()
	if _, ok := br.alphas[agentID]; !ok {
		br.alphas[agentID] = 1.0
		br.betas[agentID] = 1.0
	}
}

// Update 更新 Agent 的成功/失败计数。
func (br *BanditRouter) Update(agentID string, success bool) {
	br.mu.Lock()
	defer br.mu.Unlock()
	if success {
		br.alphas[agentID] += 1.0
	} else {
		br.betas[agentID] += 1.0
	}
}

// SelectAgents 用 Thompson Sampling 选择 top-k 个 Agent。
func (br *BanditRouter) SelectAgents(k int) []string {
	br.mu.Lock()
	defer br.mu.Unlock()

	type agentScore struct {
		id    string
		score float64
	}
	var scores []agentScore
	for id, alpha := range br.alphas {
		beta := br.betas[id]
		sample := betaSample(alpha, beta)
		scores = append(scores, agentScore{id: id, score: sample})
	}
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	var result []string
	for i, s := range scores {
		if i >= k {
			break
		}
		result = append(result, s.id)
	}
	return result
}

func betaSample(alpha, beta float64) float64 {
	x := gammaVariate(alpha)
	y := gammaVariate(beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

func gammaVariate(shape float64) float64 {
	if shape < 1 {
		u := rand.Float64()
		return gammaVariate(shape+1) * math.Pow(u, 1.0/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		var x, v float64
		for {
			x = rand.NormFloat64()
			v = 1.0 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rand.Float64()
		if u < 1.0-0.0331*(x*x)*(x*x) {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1.0-v+math.Log(v)) {
			return d * v
		}
	}
}

// PredictionMarket 内部预测市场 (LMSR)。
type PredictionMarket struct {
	liquidity float64            // LMSR 流动性参数 b
	shares    map[string]float64 // outcome → 已购买份额
	budgets   map[string]float64 // agent → 剩余预算
	mu        sync.Mutex
}

// NewPredictionMarket 创建预测市场。
func NewPredictionMarket(liquidity float64, outcomes []string, agentBudget float64, agents []string) *PredictionMarket {
	if liquidity <= 0 {
		liquidity = 100.0
	}
	shares := make(map[string]float64)
	for _, o := range outcomes {
		shares[o] = 0
	}
	budgets := make(map[string]float64)
	for _, a := range agents {
		budgets[a] = agentBudget
	}
	return &PredictionMarket{
		liquidity: liquidity,
		shares:    shares,
		budgets:   budgets,
	}
}

// Price LMSR 定价函数: p(outcome) = exp(q/b) / Σ exp(q_j/b)。
func (pm *PredictionMarket) Price(outcome string) float64 {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.price(outcome)
}

func (pm *PredictionMarket) price(outcome string) float64 {
	logSumExp := 0.0
	maxQ := -math.MaxFloat64
	for _, q := range pm.shares {
		if q/pm.liquidity > maxQ {
			maxQ = q / pm.liquidity
		}
	}
	for _, q := range pm.shares {
		logSumExp += math.Exp(q/pm.liquidity - maxQ)
	}
	q := pm.shares[outcome]
	return math.Exp(q/pm.liquidity-maxQ) / logSumExp
}

// Buy Agent 购买 outcome 的份额。
func (pm *PredictionMarket) Buy(agentID, outcome string, amount float64) (float64, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	budget, ok := pm.budgets[agentID]
	if !ok {
		return 0, fmt.Errorf("unknown agent: %s", agentID)
	}

	cost := pm.cost(outcome, amount)
	if cost > budget {
		return 0, fmt.Errorf("insufficient budget: need %.2f, have %.2f", cost, budget)
	}

	pm.shares[outcome] += amount
	pm.budgets[agentID] -= cost
	return cost, nil
}

// Prices 返回所有 outcome 的当前价格 (概率)。
func (pm *PredictionMarket) Prices() map[string]float64 {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	result := make(map[string]float64)
	for outcome := range pm.shares {
		result[outcome] = pm.price(outcome)
	}
	return result
}

func (pm *PredictionMarket) cost(outcome string, deltaQ float64) float64 {
	before := pm.costFunction()
	pm.shares[outcome] += deltaQ
	after := pm.costFunction()
	pm.shares[outcome] -= deltaQ
	return after - before
}

func (pm *PredictionMarket) costFunction() float64 {
	maxQ := -math.MaxFloat64
	for _, q := range pm.shares {
		scaled := q / pm.liquidity
		if scaled > maxQ {
			maxQ = scaled
		}
	}
	sumExp := 0.0
	for _, q := range pm.shares {
		sumExp += math.Exp(q/pm.liquidity - maxQ)
	}
	return pm.liquidity * (maxQ + math.Log(sumExp))
}
