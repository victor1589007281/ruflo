// 本文件提供离散动作空间上的多种强化学习策略与值函数更新规则（表格/线性近似/策略梯度类简化实现）。
//
// # Q-Learning（Watkins 1989；离策略 TD 控制）
//
// 更新：Q(s,a) ← Q(s,a) + α [ r + γ max_a' Q(s',a') − Q(s,a) ]。目标策略贪心，行为策略可带探索（如 ε-greedy）。
//
// # SARSA（Rummery & Niranjan 1994；在策略 TD）
//
// 更新：Q(s,a) ← Q(s,a) + α [ r + γ Q(s',a') − Q(s,a) ]，其中 a' 为在 s' 实际执行的动作，与行为策略一致。
//
// # DQN（Mnih et al., Nature 2015；本实现为线性函数近似）
//
// 思想：用神经网络（此处为每动作一组权重 w_a）近似 Q(s,a)≈w_a·s；标准 DQN 含经验回放与目标网络，本代码用同步更新简化。
//
// # A2C / Actor-Critic（优势 Actor-Critic 族）
//
// Critic 估计 V(s)，优势 A = r + γV(s') − V(s)；Actor 用策略梯度 ∂log π(a|s)·A 更新（此处 softmax 偏好 + 线性价值）。
//
// # PPO（Schulman et al., 2017；近端策略优化）
//
// 裁剪目标 L^CLIP = E[ min( r_t(θ) A_t, clip(r_t,1−ε,1+ε) A_t ) ]，抑制策略步长过大；本实现为表格偏好上的比率裁剪近似。
//
// # Decision Transformer（Chen et al., 2021 思想）
//
// 将回报与历史轨迹编码为序列，用自回归模型产生动作；此处用固定历史窗口拼接状态 + 线性打分简化。
//
// # Curiosity-Driven（如 Pathak et al., ICM 等内在动机线）
//
// 用前向模型预测误差等作为内在奖励，鼓励探索；此处用线性逐维预测误差范数加权到外在奖励上。
package neural

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
)

// RLAlgorithm is a tabular or parametric policy over discrete action names.
type RLAlgorithm interface {
	Name() string
	SelectAction(state []float32, actions []string) (string, float64)
	Update(state []float32, action string, reward float64, nextState []float32) error
	GetPolicy() map[string]float64
}

func stateKey(state []float32) string {
	var b strings.Builder
	for i, v := range state {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%.4f", v))
	}
	return b.String()
}

func pickEpsilonGreedy(rng *rand.Rand, actions []string, qfunc func(string) float64, epsilon float64) string {
	if len(actions) == 0 {
		return ""
	}
	if rng.Float64() < epsilon {
		return actions[rng.Intn(len(actions))]
	}
	best := actions[0]
	bestQ := qfunc(best)
	for _, a := range actions[1:] {
		if q := qfunc(a); q > bestQ {
			bestQ = q
			best = a
		}
	}
	return best
}

// --- Q-Learning -----------------------------------------------------------------

// QLearning is off-policy TD control with an explicit Q-table.
type QLearning struct {
	mu      sync.Mutex
	rng     *rand.Rand
	alpha   float64
	gamma   float64
	epsilon float64
	q       map[string]map[string]float64
}

// NewQLearning constructs Q-learning with epsilon-greedy exploration.
func NewQLearning(alpha, gamma, epsilon float64) *QLearning {
	if alpha <= 0 {
		alpha = 0.1
	}
	if gamma <= 0 {
		gamma = 0.99
	}
	if epsilon < 0 {
		epsilon = 0.1
	}
	return &QLearning{
		rng:     rand.New(rand.NewSource(1)),
		alpha:   alpha,
		gamma:   gamma,
		epsilon: epsilon,
		q:       make(map[string]map[string]float64),
	}
}

func (q *QLearning) Name() string { return "qlearning" }

func (q *QLearning) getQ(sk, a string) float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.q[sk] == nil {
		return 0
	}
	return q.q[sk][a]
}

func (q *QLearning) SelectAction(state []float32, actions []string) (string, float64) {
	sk := stateKey(state)
	q.mu.Lock()
	defer q.mu.Unlock()
	a := pickEpsilonGreedy(q.rng, actions, func(act string) float64 {
		if q.q[sk] == nil {
			return 0
		}
		return q.q[sk][act]
	}, q.epsilon)
	if q.q[sk] == nil {
		q.q[sk] = make(map[string]float64)
	}
	return a, q.q[sk][a]
}

func (q *QLearning) Update(state []float32, action string, reward float64, nextState []float32) error {
	sk := stateKey(state)
	nsk := stateKey(nextState)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.q[sk] == nil {
		q.q[sk] = make(map[string]float64)
	}
	maxNext := 0.0
	if row := q.q[nsk]; row != nil {
		for _, v := range row {
			if v > maxNext {
				maxNext = v
			}
		}
	}
	cur := q.q[sk][action]
	td := reward + q.gamma*maxNext - cur
	q.q[sk][action] = cur + q.alpha*td
	return nil
}

func (q *QLearning) GetPolicy() map[string]float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]float64)
	for sk, row := range q.q {
		var bestA string
		var bestV float64
		first := true
		for a, v := range row {
			if first || v > bestV {
				first = false
				bestV = v
				bestA = a
			}
		}
		out[sk+":"+bestA] = bestV
	}
	return out
}

// --- SARSA ----------------------------------------------------------------------

// SARSA is on-policy TD control; Update expects nextAction taken in next state.
type SARSA struct {
	mu      sync.Mutex
	rng     *rand.Rand
	alpha   float64
	gamma   float64
	epsilon float64
	q       map[string]map[string]float64
}

// NewSARSA constructs SARSA with epsilon-greedy behaviour policy.
func NewSARSA(alpha, gamma, epsilon float64) *SARSA {
	return &SARSA{
		rng:     rand.New(rand.NewSource(2)),
		alpha:   alpha,
		gamma:   gamma,
		epsilon: epsilon,
		q:       make(map[string]map[string]float64),
	}
}

func (s *SARSA) Name() string { return "sarsa" }

func (s *SARSA) SelectAction(state []float32, actions []string) (string, float64) {
	sk := stateKey(state)
	s.mu.Lock()
	defer s.mu.Unlock()
	a := pickEpsilonGreedy(s.rng, actions, func(act string) float64 {
		if s.q[sk] == nil {
			return 0
		}
		return s.q[sk][act]
	}, s.epsilon)
	if s.q[sk] == nil {
		s.q[sk] = make(map[string]float64)
	}
	return a, s.q[sk][a]
}

func (s *SARSA) Update(state []float32, action string, reward float64, nextState []float32) error {
	nsk := stateKey(nextState)
	s.mu.Lock()
	defer s.mu.Unlock()
	nextActs := make([]string, 0, 8)
	if row := s.q[nsk]; row != nil {
		for a := range row {
			nextActs = append(nextActs, a)
		}
	}
	if len(nextActs) == 0 {
		nextActs = []string{action}
	}
	nextA := pickEpsilonGreedy(s.rng, nextActs, func(act string) float64 {
		return s.q[nsk][act]
	}, s.epsilon)
	sk := stateKey(state)
	if s.q[sk] == nil {
		s.q[sk] = make(map[string]float64)
	}
	if s.q[nsk] == nil {
		s.q[nsk] = make(map[string]float64)
	}
	cur := s.q[sk][action]
	nextQ := s.q[nsk][nextA]
	td := reward + s.gamma*nextQ - cur
	s.q[sk][action] = cur + s.alpha*td
	return nil
}

// GetPolicy 同 QLearning 语义。O(S·A)。
func (s *SARSA) GetPolicy() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64)
	for sk, row := range s.q {
		var bestA string
		var bestV float64
		first := true
		for a, v := range row {
			if first || v > bestV {
				first = false
				bestV = v
				bestA = a
			}
		}
		out[sk+":"+bestA] = bestV
	}
	return out
}

// --- DQN (linear) ---------------------------------------------------------------

// DQN uses one weight vector per action: Q(s,a) ≈ dot(w_a, s).
type DQN struct {
	mu      sync.Mutex
	rng     *rand.Rand
	alpha   float64
	gamma   float64
	epsilon float64
	weights map[string][]float32
	dim     int
}

// NewDQN creates a linear Q-approximator. State length must stay consistent.
func NewDQN(stateDim int, alpha, gamma, epsilon float64) *DQN {
	if stateDim <= 0 {
		stateDim = 4
	}
	return &DQN{
		rng:     rand.New(rand.NewSource(3)),
		alpha:   alpha,
		gamma:   gamma,
		epsilon: epsilon,
		weights: make(map[string][]float32),
		dim:     stateDim,
	}
}

// qval 计算 w_a·s。O(d)。
func (d *DQN) qval(state []float32, action string) float64 {
	w := d.weights[action]
	if len(w) != len(state) {
		return 0
	}
	var s float64
	for i := range state {
		s += float64(w[i]) * float64(state[i])
	}
	return s
}

func (d *DQN) Name() string { return "dqn_linear" }

func (d *DQN) SelectAction(state []float32, actions []string) (string, float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range actions {
		if _, ok := d.weights[a]; !ok {
			w := make([]float32, d.dim)
			for i := range w {
				w[i] = float32((d.rng.Float64() - 0.5) * 0.2)
			}
			d.weights[a] = w
		}
	}
	a := pickEpsilonGreedy(d.rng, actions, func(act string) float64 {
		return d.qval(state, act)
	}, d.epsilon)
	return a, d.qval(state, a)
}

func (d *DQN) Update(state []float32, action string, reward float64, nextState []float32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.weights[action]
	if len(w) != len(state) {
		return fmt.Errorf("neural: dqn state dim mismatch")
	}
	maxNext := 0.0
	for a, wn := range d.weights {
		if len(wn) != len(nextState) {
			continue
		}
		v := d.qval(nextState, a)
		if v > maxNext {
			maxNext = v
		}
	}
	cur := d.qval(state, action)
	td := reward + d.gamma*maxNext - cur
	for i := range w {
		w[i] += float32(d.alpha * td * float64(state[i]))
	}
	return nil
}

func (d *DQN) GetPolicy() map[string]float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]float64)
	for a, w := range d.weights {
		var norm float64
		for _, x := range w {
			norm += float64(x * x)
		}
		out[a] = math.Sqrt(norm)
	}
	return out
}

// --- A2C ------------------------------------------------------------------------

// A2C 单步 Actor-Critic：Actor 为状态线性 logits + softmax；Critic 为线性 V(s)；用优势 A 同时更新两边。
type A2C struct {
	mu      sync.Mutex
	rng     *rand.Rand
	alphaPi float64
	alphaV  float64
	gamma   float64
	pref    map[string][]float32
	criticW []float32
	dim     int
	actions []string
}

// NewA2C 构造 A2C；actions 为离散动作全集。O(|A|·d)。
func NewA2C(stateDim int, actions []string, alphaPi, alphaV, gamma float64) *A2C {
	if stateDim <= 0 {
		stateDim = 4
	}
	pref := make(map[string][]float32)
	for _, a := range actions {
		v := make([]float32, stateDim)
		pref[a] = v
	}
	cw := make([]float32, stateDim)
	return &A2C{
		rng:     rand.New(rand.NewSource(4)),
		alphaPi: alphaPi,
		alphaV:  alphaV,
		gamma:   gamma,
		pref:    pref,
		criticW: cw,
		dim:     stateDim,
		actions: append([]string(nil), actions...),
	}
}

func (a *A2C) Name() string { return "a2c" }

// probs 数值稳定 softmax：减去 max logit 再 exp。O(|A|·d)。
func (a *A2C) probs(state []float32) map[string]float64 {
	logits := make(map[string]float64)
	var maxL float64 = -1e9
	for _, act := range a.actions {
		var s float64
		w := a.pref[act]
		for i := 0; i < len(state) && i < len(w); i++ {
			s += float64(w[i]) * float64(state[i])
		}
		logits[act] = s
		if s > maxL {
			maxL = s
		}
	}
	sum := 0.0
	exp := make(map[string]float64)
	for _, act := range a.actions {
		v := math.Exp(logits[act] - maxL)
		exp[act] = v
		sum += v
	}
	for act := range exp {
		exp[act] /= sum
	}
	return exp
}

// value 线性 Critic V(s)=w·s。O(d)。
func (a *A2C) value(state []float32) float64 {
	var s float64
	for i := 0; i < len(state) && i < len(a.criticW); i++ {
		s += float64(a.criticW[i]) * float64(state[i])
	}
	return s
}

// SelectAction 按 probs 累积分布采样。O(|A|·d)。
func (a *A2C) SelectAction(state []float32, actions []string) (string, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.probs(state)
	if len(actions) == 0 {
		return "", 0
	}
	u := a.rng.Float64()
	var cum float64
	var chosen string
	for _, act := range actions {
		cum += p[act]
		if u <= cum {
			chosen = act
			break
		}
		chosen = act
	}
	return chosen, p[chosen]
}

// Update TD(0) Critic：w_V += α_V · A · s；Actor：对 softmax 用策略梯度 ∂logπ/∂θ·A。O(|A|·d)。
func (a *A2C) Update(state []float32, action string, reward float64, nextState []float32) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.value(state)
	vn := a.value(nextState)
	adv := reward + a.gamma*vn - v
	for i := 0; i < len(a.criticW) && i < len(state); i++ {
		a.criticW[i] += float32(a.alphaV * adv * float64(state[i]))
	}
	p := a.probs(state)
	for _, act := range a.actions {
		w := a.pref[act]
		var grad float64
		if act == action {
			grad = (1 - p[act]) * adv
		} else {
			grad = (-p[act]) * adv
		}
		for i := 0; i < len(w) && i < len(state); i++ {
			w[i] += float32(a.alphaPi * grad * float64(state[i]))
		}
	}
	return nil
}

// GetPolicy 返回零状态下的动作分布。O(|A|·d)。
func (a *A2C) GetPolicy() map[string]float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	zero := make([]float32, a.dim)
	return a.probs(zero)
}

// --- PPO ------------------------------------------------------------------------

// PPO 在 softmax 偏好参数上做裁剪比率更新；SelectAction 前快照 oldPref 供 ratio = π_new/π_old。
type PPO struct {
	mu       sync.Mutex
	rng      *rand.Rand
	epsClip  float64
	lr       float64
	oldPref  map[string][]float32
	pref     map[string][]float32
	dim      int
	actions  []string
	baseline float64
}

// NewPPO clip 为 ε，默认 0.2。O(|A|·d)。
func NewPPO(stateDim int, actions []string, lr, clip float64) *PPO {
	if stateDim <= 0 {
		stateDim = 4
	}
	if clip <= 0 {
		clip = 0.2
	}
	pref := make(map[string][]float32)
	old := make(map[string][]float32)
	for _, a := range actions {
		pref[a] = make([]float32, stateDim)
		old[a] = make([]float32, stateDim)
	}
	return &PPO{
		rng:     rand.New(rand.NewSource(5)),
		epsClip: clip,
		lr:      lr,
		oldPref: old,
		pref:    pref,
		dim:     stateDim,
		actions: append([]string(nil), actions...),
	}
}

func (p *PPO) Name() string { return "ppo" }

// softmaxActionProbs 复用 A2C 的 softmax 逻辑。O(|A|·d)。
func (p *PPO) softmaxActionProbs(state []float32) map[string]float64 {
	a := &A2C{pref: p.pref, criticW: make([]float32, p.dim), dim: p.dim, actions: p.actions}
	return a.probs(state)
}

// oldProbs 基于快照 oldPref 的 softmax。O(|A|·d)。
func (p *PPO) oldProbs(state []float32) map[string]float64 {
	a := &A2C{pref: p.oldPref, criticW: make([]float32, p.dim), dim: p.dim, actions: p.actions}
	return a.probs(state)
}

// SelectAction 拷贝旧策略后按新策略采样。O(|A|·d)。
func (p *PPO) SelectAction(state []float32, actions []string) (string, float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// snapshot old policy
	for _, a := range p.actions {
		copy(p.oldPref[a], p.pref[a])
	}
	pr := p.softmaxActionProbs(state)
	if len(actions) == 0 {
		return "", 0
	}
	u := p.rng.Float64()
	var cum float64
	chosen := actions[0]
	for _, act := range actions {
		cum += pr[act]
		if u <= cum {
			chosen = act
			break
		}
		chosen = act
	}
	return chosen, pr[chosen]
}

// Update 用基线减回报得优势 A，ratio=π_new(a|s)/π_old，surrogate=min(rA, clip(r)A)，再反传梯度到偏好向量（缩放系数含 0.1）。
// 时间复杂度 O(|A|·d)。
func (p *PPO) Update(state []float32, action string, reward float64, _ []float32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.baseline = 0.95*p.baseline + 0.05*reward
	adv := reward - p.baseline
	piNew := p.softmaxActionProbs(state)
	piOld := p.oldProbs(state)
	ratio := 1.0
	if piOld[action] > 1e-8 {
		ratio = piNew[action] / piOld[action]
	}
	clipRatio := math.Max(1-p.epsClip, math.Min(1+p.epsClip, ratio))
	surr := math.Min(ratio*adv, clipRatio*adv)
	g := surr / (piNew[action] + 1e-8)
	for _, act := range p.actions {
		w := p.pref[act]
		coef := -piNew[act]
		if act == action {
			coef = 1 - piNew[act]
		}
		for i := 0; i < len(w) && i < len(state); i++ {
			w[i] += float32(p.lr * g * coef * float64(state[i]) * 0.1)
		}
	}
	return nil
}

// GetPolicy 零状态下新策略分布。O(|A|·d)。
func (p *PPO) GetPolicy() map[string]float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	zero := make([]float32, p.dim)
	return p.softmaxActionProbs(zero)
}

// --- Decision Transformer (simplified) -----------------------------------------

// DecisionTransformer 将最近 maxHist 步状态拼接为固定长向量，再与每动作一组权重打分（极简序列决策近似 DT）。
type DecisionTransformer struct {
	mu       sync.Mutex
	rng      *rand.Rand
	history  [][]float32
	maxHist  int
	stateDim int
	weights  []float32
	lr       float64
	actions  []string
}

// NewDecisionTransformer 初始化历史窗口与权重。O(maxHist·d)。
func NewDecisionTransformer(stateDim int, actions []string, maxHist int, lr float64) *DecisionTransformer {
	if maxHist <= 0 {
		maxHist = 8
	}
	if stateDim <= 0 {
		stateDim = 4
	}
	rng := rand.New(rand.NewSource(6))
	w := make([]float32, stateDim*maxHist)
	for i := range w {
		w[i] = float32((rng.Float64() - 0.5) * 0.1)
	}
	return &DecisionTransformer{
		rng:      rng,
		history:  nil,
		maxHist:  maxHist,
		stateDim: stateDim,
		weights:  w,
		lr:       lr,
		actions:  append([]string(nil), actions...),
	}
}

func (d *DecisionTransformer) Name() string { return "decision_transformer" }

// flatState 追加当前状态到 history，输出长度 maxHist·d 的拼接（前部不足补零语义由布局实现）。O(maxHist·d)。
func (d *DecisionTransformer) flatState(state []float32) []float32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	dim := d.stateDim
	if dim <= 0 {
		dim = len(state)
	}
	norm := make([]float32, dim)
	copy(norm, state)
	d.history = append(d.history, norm)
	if len(d.history) > d.maxHist {
		d.history = d.history[len(d.history)-d.maxHist:]
	}
	out := make([]float32, dim*d.maxHist)
	base := (d.maxHist - len(d.history)) * dim
	if base < 0 {
		base = 0
	}
	for i, h := range d.history {
		off := base + i*dim
		if off+dim > len(out) {
			break
		}
		copy(out[off:], h)
	}
	return out
}

// SelectAction 对每动作用 flatState 与权重及动作哈希扰动项打分，取 argmax。O(|A|·maxHist·d)。
func (d *DecisionTransformer) SelectAction(state []float32, actions []string) (string, float64) {
	fs := d.flatState(state)
	d.mu.Lock()
	defer d.mu.Unlock()
	best := ""
	var bestS float64 = -1e9
	for _, a := range actions {
		h := fnvString(a)
		var s float64
		for i := 0; i < len(fs) && i < len(d.weights); i++ {
			s += float64(fs[i]) * float64(d.weights[i]) * (1 + float64(int(h>>uint(i%8))&1)*0.01)
		}
		if s > bestS {
			bestS = s
			best = a
		}
	}
	return best, bestS
}

// Update 简单 REINFORCE 式：权重 += lr · reward · flatState（与具体选中动作弱相关，教学化简化）。O(maxHist·d)。
func (d *DecisionTransformer) Update(state []float32, action string, reward float64, nextState []float32) error {
	fs := d.flatState(state)
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := 0; i < len(d.weights) && i < len(fs); i++ {
		d.weights[i] += float32(d.lr * reward * float64(fs[i]) * 0.01)
	}
	return nil
}

// GetPolicy 返回动作名到哈希标量（占位诊断）。O(|A|)。
func (d *DecisionTransformer) GetPolicy() map[string]float64 {
	out := make(map[string]float64)
	for _, a := range d.actions {
		out[a] = float64(fnvString(a) % 1000)
	}
	return out
}

// fnvString FNV-1a 64 哈希，用于 DecisionTransformer 中动作相关扰动。O(len(s))。
func fnvString(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// --- Curiosity ------------------------------------------------------------------

// CuriosityDriven 包装基算法，在 Update 中把下一步预测误差范数（经 β 缩放）加到奖励上，鼓励访问难预测状态。
type CuriosityDriven struct {
	mu        sync.Mutex
	base      RLAlgorithm
	wModel    []float32
	lrModel   float64
	beta      float64
	lastState []float32
}

// NewCuriosityDriven inner 为外在 RL；wModel 为逐维线性预测参数。O(d)。
func NewCuriosityDriven(inner RLAlgorithm, stateDim int, lrModel, beta float64) *CuriosityDriven {
	if stateDim <= 0 {
		stateDim = 4
	}
	w := make([]float32, stateDim)
	return &CuriosityDriven{base: inner, wModel: w, lrModel: lrModel, beta: beta}
}

func (c *CuriosityDriven) Name() string { return "curiosity_" + c.base.Name() }

// predictNext 简化前向：pred_i = s_i * w_i。O(d)。
func (c *CuriosityDriven) predictNext(state []float32) []float32 {
	out := make([]float32, len(state))
	for i := range state {
		if i < len(c.wModel) {
			out[i] = state[i] * c.wModel[i]
		}
	}
	return out
}

// SelectAction 委托 base并缓存 lastState。O(基算法)。
func (c *CuriosityDriven) SelectAction(state []float32, actions []string) (string, float64) {
	c.mu.Lock()
	c.lastState = append([]float32(nil), state...)
	c.mu.Unlock()
	return c.base.SelectAction(state, actions)
}

// Update 计算预测误差 ||next - pred||² 的平方根加权 β 作为内在奖励，再调用 base.Update(..., reward+intrinsic, ...)；
// 并用梯度式规则更新 wModel。O(d)。
func (c *CuriosityDriven) Update(state []float32, action string, reward float64, nextState []float32) error {
	c.mu.Lock()
	pred := c.predictNext(state)
	var predErr float64
	for i := 0; i < len(nextState) && i < len(pred); i++ {
		d := float64(nextState[i] - pred[i])
		predErr += d * d
	}
	for i := 0; i < len(c.wModel) && i < len(state) && i < len(nextState); i++ {
		c.wModel[i] += float32(c.lrModel * float64(state[i]) * float64(nextState[i]-pred[i]))
	}
	c.mu.Unlock()
	intrinsic := c.beta * math.Sqrt(predErr+1e-8)
	return c.base.Update(state, action, reward+intrinsic, nextState)
}

// GetPolicy 委托内层。O(基算法)。
func (c *CuriosityDriven) GetPolicy() map[string]float64 {
	return c.base.GetPolicy()
}
