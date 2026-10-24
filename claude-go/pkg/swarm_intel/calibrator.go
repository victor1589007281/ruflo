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
	Width    float64             `json:"width"`    // 区间宽度
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

// computeQuantile 取历史保形分数的 (1-alpha) 分位数, 作为区间半宽。
//
// 算法解读：
//
//	原理：分裂式保形预测的分位数取法 —— 把历史分数排序后取第 ⌈(n+1)(1-alpha)⌉ 个。
//	      那个 +1 不是凑数: 它让分位数在**有限样本**下仍然满足覆盖率下界
//	      P(y ∈ C) >= 1-alpha, 少了它覆盖率会系统性偏低。
//	关键不变量：返回的必须是**已排序**样本里的一个真实取值, 且索引要 clamp 到
//	      n-1。idx 越界会 panic; 不做 clamp 的话, n 很小时 (n+1)(1-alpha) 会
//	      直接越出切片末尾 —— 而样本少正是校准器刚启动时的常态。
//	代价与边界：每次调用都要拷一份并 O(n log n) 排序 (n <= maxHist=500), 无缓存 ——
//	      因为 AddScore 会不断让排序结果失效。样本为空时返回 0.15 这个保守默认
//	      (相当于先验认为半宽 15 个百分点), 而不是 0 —— 返回 0 会宣称"完全确定"。
//	设计权衡：用朴素排序而不是维护有序结构 (插入 O(n)), 因为读取频率远低于写入,
//	      且 n 有上界。
func (c *ConformalCalibrator) computeQuantile() float64 {
	if len(c.scoreHist) == 0 {
		return 0.15 // 保守默认
	}
	sorted := make([]float64, len(c.scoreHist))
	copy(sorted, c.scoreHist)
	sort.Float64s(sorted)

	idx := int(math.Ceil(float64(len(sorted)+1) * (1 - c.alpha)))
	// clamp 上界: n 很小时 (n+1)(1-alpha) 会越出切片末尾, 而"样本少"正是校准器刚启动的常态。
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

// TrimmedFuse 修剪后融合 — 去掉最极端的预测后再融合, **并做跨维归一化**。
//
// 归一化把结果当概率分布处理(各维除以总和), 适用于"各维是同一事件的互斥概率"的场景
// (Predict 那类)。若你的各维是**彼此独立的评分**(如 0-100 的多维评审), 用
// TrimmedFuseRaw —— 归一会毁掉量纲, 见那个函数的注释。
func (bf *ByzantineFuser) TrimmedFuse(predictions []AgentPrediction) map[string]float64 {
	result := bf.trimmedFuse(predictions)
	if len(predictions) >= trimmedFuseMinSamples {
		// N<3 走的是 simpleMerge, 它本就不归一 —— 保持既有行为不变。
		normalize(result)
	}
	return result
}

// TrimmedFuseRaw 逐维截尾均值, **不做跨维归一化**。
//
// 为什么需要它: 同一份数据在 N=2 与 N=3 之间会跳变(N<3 走 simpleMerge 不归一,
// N>=3 归一):
//
//	输入 (80/70/60, 82/71/61, 78/69/59):
//	  N=2 → plot=81.00  character=70.50  prose=60.50   ← 量纲正确
//	  N=3 → plot=38.10  character=33.33  prose=28.57   ← 归一后量纲崩了
//
// 这不是假设: 多维评审(pkg/agent 的 review-panel)一直走 N>=3 那条路, 于是它产出的
// dimensions 分数与**同一份 JSON 里的 overall**不同量纲。TrimmedFuse 的文档写的是
// "每维截尾均值", normalize 与该契约矛盾。
//
// **刻意不直接把 normalize 从 TrimmedFuse 里删掉**: 它另有依赖归一语义的调用方
// (概率分布场景), 一刀切会把这个 bug 换成另一个方向的 bug。
func (bf *ByzantineFuser) TrimmedFuseRaw(predictions []AgentPrediction) map[string]float64 {
	return bf.trimmedFuse(predictions)
}

// trimmedFuseMinSamples 截尾需要的最小样本数; 低于它退回普通均值。
const trimmedFuseMinSamples = 3

// trimmedFuse 截尾均值本体(不归一)。抽出来共用而不是复制一份 ——
// 窗口大小与信任权重的逻辑只该有一处, 两份必然漂移。
func (bf *ByzantineFuser) trimmedFuse(predictions []AgentPrediction) map[string]float64 {
	bf.mu.Lock()
	defer bf.mu.Unlock()

	if len(predictions) < trimmedFuseMinSamples {
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

// simpleMerge 逐维求算术平均 (不截尾、不归一), 供样本不足时退回使用。
//
// 注意：分母固定为 len(preds) 而不是"出现了该 outcome 的预测者数" —— 某个 Agent 完全
// 没提这个 outcome 时, 它按 0 计入平均, 而不是被跳过。这是"缺席即 0 分"的语义。
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

// normalize 就地按总和归一, 使 dist 成为概率分布; 总和 <= 0 时原样返回。
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

// betaSample 从 Beta(alpha, beta) 抽一个样本, 用两个 Gamma 变量之比构造。
//
// 这是 Thompson Sampling 的采样核心: 每轮为每个 Agent 抽一次后验, 谁抽得大谁上场。
func betaSample(alpha, beta float64) float64 {
	x := gammaVariate(alpha)
	y := gammaVariate(beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaVariate 用 Marsaglia-Tsang 挤压法从 Gamma(shape, 1) 抽一个样本。
//
// 算法解读：
//
//	原理：shape >= 1 时用挤压法 —— 从正态提议分布里抽 x, 变换成 d*v 后由两条接受
//	      判据筛选; shape < 1 时先用 Boost 恒等式 Gamma(a) = Gamma(a+1) * U^(1/a)
//	      升到 shape+1 再递归 (这是一次递归, 不会退化成长递归)。
//	关键不变量：内层循环必须拒绝 v <= 0 的样本 (v = 1+c*x 可能为负, 而 v^3 与
//	      log(v) 都要求 v > 0)。少了这个内层循环, 负数会一路流进 math.Log 变成 NaN。
//	代价与边界：拒绝采样, 单次调用期望常数次迭代; 用全局 math/rand, 并发调用会
//	      争用同一把锁, 且不可复现 (没有注入 *rand.Rand 的入口)。
//	设计权衡：取标准实现而不是自己推公式 —— 挤压法的接受率与数值稳定性都经过检验,
//	      手写近似会引入难以察觉的分布偏差, 而偏差直接表现为"路由选出的 Agent 不对"。
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

// price 无锁版 LMSR 定价 (调用方须已持 pm.mu)。
//
// 算法解读：
//
//	原理：LMSR 的价格就是 softmax: p(outcome) = exp(q_i/b) / Σ exp(q_j/b)。它保证了
//	      价格之和恒为 1, 于是"价格"可以直接读作"市场认为的概率"。
//	关键不变量：指数必须先减去 max(q/b) 再求和 (log-sum-exp 技巧)。直接 exp(q/b)
//	      在 q/b 较大时会溢出成 +Inf, 于是分子分母同时 Inf, 结果是 NaN —— 而 NaN
//	      不会报错, 只会让整个市场价格变成一片 NaN。
//	代价与边界：每次定价 O(结果数) 两次遍历; 结果数通常是个位数, 无需缓存。
//	设计权衡：为了省一次遍历, maxQ 那轮用了 `if q/liquidity > maxQ` 而不是
//	      math.Max —— 语义等价, 但少了函数调用开销。
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

// cost 返回把 outcome 的份额增加 deltaQ 所需的成本 (LMSR 成本函数之差)。
//
// 先加后减地"试探"而不是复制一份 shares 再算: 成本函数只看各维份额, 原地改完立刻
// 还原是安全的, 且省一次 map 拷贝。调用方必须已持锁。
func (pm *PredictionMarket) cost(outcome string, deltaQ float64) float64 {
	before := pm.costFunction()
	pm.shares[outcome] += deltaQ
	after := pm.costFunction()
	pm.shares[outcome] -= deltaQ
	return after - before
}

// costFunction 是 LMSR 的成本函数 C(q) = b·ln(Σ exp(q_j/b)), 同样用 log-sum-exp 防溢出。
//
// 注意：这里把 maxQ 提出来写成 b·(maxQ + ln Σ exp(q_j/b - maxQ)) —— 与 price 里
// 减 maxQ 是同一个技巧, 只是提公因子后形式上多了一层括号, 数值上完全等价。
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
