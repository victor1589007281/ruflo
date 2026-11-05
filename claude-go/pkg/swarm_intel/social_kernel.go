package swarm_intel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phase 2: social 结构化轮次内核。
// 把 social 模拟从"单 prompt 讲故事"升级为显式轮次内核:
//
//	Spawn:   一次 LLM 调用生成 N 个 agent (profile + stance + stubbornness)
//	Round r: 每个 active agent 并行决策 (speak/listen/interview/adopt + stance_delta),
//	         立场按 FJ 模型更新: stance = clamp(stub·stance + (1-stub)·Σw_ab·stance_b + delta)
//	Detect:  每轮后纯计算检测涌现 (方差/熵/聚类系数), 不依赖 LLM 叙事
//	Report:  由最终立场分布聚类生成 scenarios, 涌现行为必须带指标证据
//
// S3 对抗测试见 social_kernel_test.go: 打乱 spawn 立场重跑,
// 极化/级联类涌现必须消失, 否则说明"涌现"是 prompt 固定叙事而非测量结果。

// SimEvent 定义已并入 swarmstore.go (W1/W2 合并, 见该文件类型注释);
// 此处保留 SimEventRecorder 及 JSONL 兜底记录器, 长期统一走 SwarmStore 的
// AppendSimEvent/SimEvents (swarmstore.go)。

// SimEventRecorder SimEvent 记录接口。接口名与 SwarmStore 的 AppendSimEvent 对齐,
// 因此 *JSONLStore / *AgentDBStore 可直接作为 recorder 注入 (kernel.emit 调用点已就位)。
type SimEventRecorder interface {
	AppendSimEvent(ev *SimEvent) error
}

// jsonlSimEventRecorder JSONL 兜底记录器: Engine 未注入 SwarmStore 时的落盘实现,
// 与 JSONLStore.AppendSimEvent 同一落盘语义, 之后可整体替换为 SwarmStore。
type jsonlSimEventRecorder struct {
	mu sync.Mutex
	f  *os.File
}

// NewJSONLSimEventRecorder 创建 JSONL 兜底记录器 (追加写, 自动建目录)。
func NewJSONLSimEventRecorder(path string) (*jsonlSimEventRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &jsonlSimEventRecorder{f: f}, nil
}

// AppendSimEvent 追加一条 SimEvent。
func (r *jsonlSimEventRecorder) AppendSimEvent(ev *SimEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.NewEncoder(r.f).Encode(ev)
}

// SocialAgent 内核中的单个社会 Agent。
type SocialAgent struct {
	ID           string  `json:"id"`           // 内核分配, 如 A1..A6
	Name         string  `json:"name"`         // LLM 生成的称呼
	Profile      string  `json:"profile"`      // 身份 + 性格描述
	Stance       float64 `json:"stance"`       // 对议题立场, -1..1
	Stubbornness float64 `json:"stubbornness"` // 固执度 0..1, 默认 0.3
}

// SocialKernelConfig 内核参数。
type SocialKernelConfig struct {
	NumAgents   int     // agent 数, 默认 6
	Rounds      int     // 轮数, 默认 6
	HistoryK    int     // 每轮输入携带的最近环境消息条数, 默认 10
	MaxConcur   int     // 轮内 LLM 并发, 默认 3
	DefaultStub float64 // spawn 未给 stubbornness 时的默认值 0.3
	// ShuffleStances > 0 时, spawn 后用该 seed 随机打乱立场分配 (S3 对抗对照开关,
	// 仅测试使用): 用于验证涌现来自真实立场动力学而非 prompt 固定叙事。
	ShuffleStances int64
}

// DefaultSocialKernelConfig 返回 6 agent × 6 轮的保守默认 (约 38 次 LLM 调用)。
func DefaultSocialKernelConfig() SocialKernelConfig {
	return SocialKernelConfig{
		NumAgents:   6,
		Rounds:      6,
		HistoryK:    10,
		MaxConcur:   3,
		DefaultStub: 0.3,
	}
}

// socialSpawnAgent spawn LLM 返回的原始结构 (允许缺 id/stubbornness, 内核补齐)。
type socialSpawnAgent struct {
	Name         string  `json:"name"`
	Profile      string  `json:"profile"`
	Stance       float64 `json:"stance"`
	Stubbornness float64 `json:"stubbornness"`
}

// SocialRoundAction 单个 agent 单轮的决策输出。
type SocialRoundAction struct {
	Action      string  `json:"action"`       // speak | listen | interview | adopt
	Content     string  `json:"content"`      // 发言/私聊内容, <=200字
	StanceDelta float64 `json:"stance_delta"` // 立场增量, 限幅 ±0.2
}

// envMessage 环境消息 (speak 进环境; interview 是私聊不进环境)。
type envMessage struct {
	Round   int    `json:"round"`
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// roundMetrics 一轮的纯计算指标 (涌现判定的唯一依据)。
type roundMetrics struct {
	Variance   float64 `json:"variance"`
	Entropy    float64 `json:"entropy"`
	Clustering float64 `json:"clustering"`
}

// SocialKernel social 结构化轮次内核。
type SocialKernel struct {
	llm      LLMClient
	notify   NotifyFunc
	recorder SimEventRecorder

	cfg       SocialKernelConfig
	objective string
	runID     string

	agents  []*SocialAgent
	env     []envMessage
	metrics []roundMetrics
	events  []SimEvent

	// emergentKinds 记录已触发的涌现类型, 避免同一类型每轮重复上报。
	emergent     []string
	emergentKind map[string]bool
}

// NewSocialKernel 创建 social 轮次内核。
func NewSocialKernel(llm LLMClient, notify NotifyFunc, cfg SocialKernelConfig) *SocialKernel {
	if notify == nil {
		notify = func(_, _ string) {}
	}
	return &SocialKernel{
		llm:          llm,
		notify:       notify,
		cfg:          cfg,
		runID:        fmt.Sprintf("social-%d", time.Now().UnixNano()),
		emergentKind: make(map[string]bool),
	}
}

// SetRecorder 挂接 SimEvent 记录器 (Engine 注入 JSONLStore/AgentDBStore 或兜底实现)。
func (k *SocialKernel) SetRecorder(r SimEventRecorder) { k.recorder = r }

// Events 返回本次运行产生的全部 SimEvent (测试与 JSONL 兜底之外的观测口)。
func (k *SocialKernel) Events() []SimEvent { return k.events }

// Run 执行完整模拟: spawn → rounds×(决策+FJ更新+detect) → report。
func (k *SocialKernel) Run(ctx context.Context, chatID, objective string) (*SimulationResult, error) {
	k.objective = objective
	if err := k.spawn(ctx); err != nil {
		return nil, fmt.Errorf("social 内核 spawn 失败: %w", err)
	}
	for r := 1; r <= k.cfg.Rounds; r++ {
		actions, err := k.runRound(ctx, r)
		if err != nil {
			return nil, err
		}
		k.updateStances(r, actions)
		k.detect(r)
	}
	return k.report(ctx, chatID), nil
}

// spawn 一次 LLM 调用生成全部 agent。
func (k *SocialKernel) spawn(ctx context.Context) error {
	n := k.cfg.NumAgents
	prompt := fmt.Sprintf(`你是社会模拟角色生成器。围绕议题生成 %d 个立场、身份、性格各异的 Agent。

议题: %s

要求:
1. stance 为 -1..1 的显式数值 (负=反对, 正=支持), 立场应有真实分歧, 不要挤在中间
2. stubbornness 为 0..1 (0.3 左右为普通, 0.7 以上为顽固)
3. profile 一句话写清身份和性格 (<=60字)

输出严格JSON数组 (不要markdown代码块, 不要其他文字):
[{"name":"称呼","profile":"身份+性格","stance":0.6,"stubbornness":0.3}]`, n, k.objective)

	resp, err := k.llm.SimpleComplete(ctx, "你是社会模拟角色生成器。只输出JSON数组。", prompt)
	if err != nil {
		return err
	}
	parsed := ParseJSON[[]socialSpawnAgent](resp)
	if !parsed.OK || len(parsed.Value) == 0 {
		return fmt.Errorf("spawn 解析失败: %s", truncate(resp, 120))
	}
	for i, sa := range parsed.Value {
		if i >= n {
			break
		}
		stub := sa.Stubbornness
		if stub <= 0 {
			stub = k.cfg.DefaultStub
		}
		k.agents = append(k.agents, &SocialAgent{
			ID:           fmt.Sprintf("A%d", i+1),
			Name:         sa.Name,
			Profile:      sa.Profile,
			Stance:       clampStance(sa.Stance),
			Stubbornness: clamp01(stub),
		})
	}
	// S3 对抗对照: 打乱立场在 agent 间的分配, 破坏 profile-stance 联合分布。
	if k.cfg.ShuffleStances > 0 {
		sh := rand.New(rand.NewSource(k.cfg.ShuffleStances))
		stances := make([]float64, len(k.agents))
		for i, a := range k.agents {
			stances[i] = a.Stance
		}
		sh.Shuffle(len(stances), func(i, j int) { stances[i], stances[j] = stances[j], stances[i] })
		for i, a := range k.agents {
			a.Stance = stances[i]
		}
	}
	return nil
}

// runRound 并行收集每个 agent 的决策 (speak/listen/interview/adopt + delta)。
// 复用 FanOutCollect, MaxConcurrency=3; 单 agent 失败降级为 listen。
func (k *SocialKernel) runRound(ctx context.Context, round int) (map[string]*SocialRoundAction, error) {
	branches := make(map[string]BranchFunc)
	for _, a := range k.agents {
		a := a
		branches[a.ID] = func(bctx context.Context) (string, error) {
			return k.llm.SimpleComplete(bctx, "你是社会模拟中的单个 Agent。只输出JSON。", k.roundPrompt(a, round))
		}
	}
	fanCfg := DefaultFanOutConfig()
	fanCfg.MaxConcurrency = k.cfg.MaxConcur
	results := FanOutCollect(ctx, fanCfg, branches)

	actions := make(map[string]*SocialRoundAction, len(k.agents))
	for _, r := range results {
		act := &SocialRoundAction{Action: "listen"}
		if r.Error == nil {
			if parsed := ParseJSON[SocialRoundAction](r.Value); parsed.OK && parsed.Value.Action != "" {
				act = &parsed.Value
			}
		}
		act.Action = normalizeAction(act.Action)
		act.StanceDelta = clampF(act.StanceDelta, -0.2, 0.2)
		act.Content = truncate(act.Content, 200)
		actions[r.ID] = act

		payload, _ := json.Marshal(map[string]any{
			"action": act.Action, "content": act.Content, "stance_delta": act.StanceDelta,
		})
		k.emit(SimEvent{RunID: k.runID, Round: round, Type: "action", AgentID: r.ID, Payload: payload})
	}

	// speak 进环境 (interview 是私聊不进环境), 环境只保留最近 HistoryK 条。
	for _, a := range k.agents {
		if act, ok := actions[a.ID]; ok && act.Action == "speak" && act.Content != "" {
			k.env = append(k.env, envMessage{Round: round, AgentID: a.ID, Name: a.Name, Content: act.Content})
		}
	}
	if len(k.env) > k.cfg.HistoryK {
		k.env = k.env[len(k.env)-k.cfg.HistoryK:]
	}
	return actions, nil
}

// roundPrompt 组装单个 agent 的轮内输入: profile + stance + 最近环境消息 + 邻居立场快照。
func (k *SocialKernel) roundPrompt(a *SocialAgent, round int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "议题: %s\n", k.objective)
	fmt.Fprintf(&sb, "你是: %s (%s)\n", a.Name, a.Profile)
	fmt.Fprintf(&sb, "你当前的立场: %.2f (-1反对 .. +1支持), 固执度 %.2f\n", a.Stance, a.Stubbornness)
	fmt.Fprintf(&sb, "第 %d/%d 轮。邻居立场快照:\n", round, k.cfg.Rounds)
	for _, b := range k.agents {
		if b.ID == a.ID {
			continue
		}
		fmt.Fprintf(&sb, "- %s: %.2f\n", b.Name, b.Stance)
	}
	if len(k.env) > 0 {
		sb.WriteString("最近环境消息:\n")
		for _, m := range k.env {
			fmt.Fprintf(&sb, "[%s] %s\n", m.Name, m.Content)
		}
	}
	sb.WriteString(`决策: action 四选一 (speak=公开发言进环境 / listen=静默观察 / interview=私聊不进环境 / adopt=公开表态),
content <=200字 (speak/interview 必填, 其他可空),
stance_delta 为本轮立场增量 (-0.2..0.2)。
输出严格JSON: {"action":"speak","content":"...","stance_delta":0.1}`)
	return sb.String()
}

// updateStances FJ 模型立场更新:
//
//	stance_a(t+1) = clamp(stub_a·stance_a(t) + (1-stub_a)·Σ w_ab·stance_b(t) + delta_a)
//
// 其中 w_ab 按最近一轮发言中 b 被提及次数归一化 (提及越多权重越高),
// 无人被提及时退化为对其他 agent 的均匀权重。
func (k *SocialKernel) updateStances(round int, actions map[string]*SocialRoundAction) {
	mentions := k.roundMentions()
	for _, a := range k.agents {
		act := actions[a.ID]
		var pull float64
		var wsum float64
		for _, b := range k.agents {
			if b.ID == a.ID {
				continue
			}
			w := mentions[a.ID][b.ID]
			pull += w * b.Stance
			wsum += w
		}
		var neighborMean float64
		if wsum > 0 {
			neighborMean = pull / wsum
		} else if len(k.agents) > 1 {
			// 无提及: 均匀权重
			var s float64
			for _, b := range k.agents {
				if b.ID != a.ID {
					s += b.Stance
				}
			}
			neighborMean = s / float64(len(k.agents)-1)
		} else {
			neighborMean = a.Stance
		}
		next := a.Stubbornness*a.Stance + (1-a.Stubbornness)*neighborMean
		if act != nil {
			next += act.StanceDelta
		}
		a.Stance = clampStance(next)

		payload, _ := json.Marshal(map[string]any{"stance": a.Stance, "stubbornness": a.Stubbornness})
		k.emit(SimEvent{RunID: k.runID, Round: round, Type: "stance", AgentID: a.ID, Payload: payload})
	}
	k.metrics = append(k.metrics, k.computeMetrics())
}

// roundMentions 统计最近一轮各 agent 发言内容中对其他 agent 的提及次数。
func (k *SocialKernel) roundMentions() map[string]map[string]float64 {
	round := 0
	if len(k.env) > 0 {
		round = k.env[len(k.env)-1].Round
	}
	mentions := make(map[string]map[string]float64)
	for _, m := range k.env {
		if m.Round != round {
			continue
		}
		if mentions[m.AgentID] == nil {
			mentions[m.AgentID] = make(map[string]float64)
		}
		for _, b := range k.agents {
			if b.ID == m.AgentID {
				continue
			}
			if strings.Contains(m.Content, b.Name) {
				mentions[m.AgentID][b.ID]++
			}
		}
	}
	return mentions
}

// computeMetrics 纯计算: 立场方差 / 5桶分布熵 / 立场图聚类系数 (|Δstance|<0.3 连边算 transitivity)。
func (k *SocialKernel) computeMetrics() roundMetrics {
	st := make([]float64, len(k.agents))
	for i, a := range k.agents {
		st[i] = a.Stance
	}
	return roundMetrics{
		Variance:   stanceVariance(st),
		Entropy:    stanceEntropy(st, 5),
		Clustering: stanceClustering(st, 0.3),
	}
}

// detect 每轮后的涌现检测 (纯计算, 无 LLM)。
// 极化: 聚类系数 <0.2 → >0.6 且方差维持 >0.08 (区分于共识);
// 共识/级联: 熵 3 轮内下降 >0.3 且方差收敛 <0.02。
// 触发即写 SimEvent(emergence) 并记录带前后指标的 evidence。
func (k *SocialKernel) detect(round int) {
	if len(k.metrics) == 0 {
		return
	}
	m := k.metrics[len(k.metrics)-1]
	ev := func(kind, evidence string) {
		if k.emergentKind[kind] {
			return
		}
		k.emergentKind[kind] = true
		k.emergent = append(k.emergent, evidence)
		payload, _ := json.Marshal(map[string]string{"kind": kind, "evidence": evidence})
		k.emit(SimEvent{RunID: k.runID, Round: round, Type: "emergence", Payload: payload})
	}

	// 极化: 相邻轮聚类跳变 + 高方差保持
	if len(k.metrics) >= 2 {
		prev := k.metrics[len(k.metrics)-2]
		if prev.Clustering < 0.2 && m.Clustering > 0.6 && m.Variance > 0.08 {
			ev("polarization", fmt.Sprintf("极化(聚类系数 %.2f→%.2f, 方差 %.2f)", prev.Clustering, m.Clustering, m.Variance))
		}
	}
	// 共识/级联: 熵 3 轮内骤降 + 方差收敛
	from := len(k.metrics) - 3
	if from < 0 {
		from = 0
	}
	if m.Entropy < k.metrics[from].Entropy-0.3 && m.Variance < 0.02 {
		ev("consensus", fmt.Sprintf("共识/级联(熵 %.2f→%.2f, 方差 %.2f)", k.metrics[from].Entropy, m.Entropy, m.Variance))
	}
}

// stanceCluster 最终立场的一维 gap 聚类 (排序后按 >0.3 的间隙切开)。
type stanceCluster struct {
	AgentIDs []string
	Mean     float64
	Min, Max float64
}

// finalClusters 对最终立场做 gap 聚类, 供 report 生成 scenarios。
func (k *SocialKernel) finalClusters() []stanceCluster {
	sorted := make([]*SocialAgent, len(k.agents))
	copy(sorted, k.agents)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Stance < sorted[j].Stance })
	var out []stanceCluster
	var cur []float64
	var ids []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		cl := stanceCluster{AgentIDs: ids, Min: cur[0], Max: cur[len(cur)-1]}
		var s float64
		for _, v := range cur {
			s += v
		}
		cl.Mean = s / float64(len(cur))
		out = append(out, cl)
		cur, ids = nil, nil
	}
	for i, a := range sorted {
		if i > 0 && a.Stance-sorted[i-1].Stance > 0.3 {
			flush()
		}
		cur = append(cur, a.Stance)
		ids = append(ids, a.ID)
	}
	flush()
	return out
}

// report 生成最终 SimulationResult: scenarios (终局聚类→1次LLM润色, 失败则用纯计算回退)
// + emergent_behaviors (带指标证据) + 立场轨迹/指标曲线。
func (k *SocialKernel) report(ctx context.Context, chatID string) *SimulationResult {
	clusters := k.finalClusters()

	// 构造 scenario 骨架 (纯计算回退 + LLM 润色输入)
	type clusterBrief struct {
		Idx     int     `json:"idx"`
		Size    int     `json:"size"`
		Mean    float64 `json:"mean"`
		Members string  `json:"members"`
	}
	briefs := make([]clusterBrief, 0, len(clusters))
	for i, cl := range clusters {
		names := make([]string, 0, len(cl.AgentIDs))
		for _, id := range cl.AgentIDs {
			for _, a := range k.agents {
				if a.ID == id {
					names = append(names, fmt.Sprintf("%s(%.2f)", a.Name, a.Stance))
				}
			}
		}
		briefs = append(briefs, clusterBrief{Idx: i + 1, Size: len(cl.AgentIDs), Mean: cl.Mean, Members: strings.Join(names, ", ")})
	}

	// 纯计算回退 scenarios
	fallbackScenarios := make([]ScenarioOutcome, 0, len(briefs))
	for _, b := range briefs {
		side := "中间派"
		if b.Mean > 0.3 {
			side = "支持派"
		} else if b.Mean < -0.3 {
			side = "反对派"
		}
		fallbackScenarios = append(fallbackScenarios, ScenarioOutcome{
			Name:        fmt.Sprintf("%s收敛 (立场 %.2f)", side, b.Mean),
			Probability: float64(b.Size) / float64(len(k.agents)),
			Description: fmt.Sprintf("%d 个 agent 收敛于 %.2f 附近: %s", b.Size, b.Mean, truncate(b.Members, 120)),
		})
	}

	// LLM 润色: 让 scenario 有叙事, 但概率与规模必须服从计算的聚类结果
	var scenarios []ScenarioOutcome
	var summary string
	briefJSON, _ := json.Marshal(briefs)
	emergentJSON, _ := json.Marshal(k.emergent)
	prompt := fmt.Sprintf(`你是社会模拟报告分析员。基于以下最终立场聚类与涌现证据, 为每个聚类命名一个演化终局场景。

议题: %s
最终立场聚类: %s
检测到的涌现 (带指标证据): %s

要求: 每个场景的概率与规模必须服从聚类数据, 不要另编; description 150字以内。
输出严格JSON: {"scenarios":[{"name":"场景名","description":"150字内","key_events":["事件1","事件2"]}],"summary":"300字以内综合分析"}`,
		k.objective, briefJSON, emergentJSON)
	if resp, err := k.llm.SimpleComplete(ctx, "你是社会模拟报告分析员。只输出JSON。", prompt); err == nil {
		type reportJSON struct {
			Scenarios []struct {
				Name        string   `json:"name"`
				Description string   `json:"description"`
				KeyEvents   []string `json:"key_events"`
			} `json:"scenarios"`
			Summary string `json:"summary"`
		}
		if parsed := ParseJSON[reportJSON](resp); parsed.OK && len(parsed.Value.Scenarios) > 0 {
			for i, sc := range parsed.Value.Scenarios {
				out := ScenarioOutcome{Name: sc.Name, Description: sc.Description, KeyEvents: sc.KeyEvents}
				if i < len(fallbackScenarios) {
					out.Probability = fallbackScenarios[i].Probability // 概率服从计算
				}
				scenarios = append(scenarios, out)
			}
			summary = parsed.Value.Summary
		}
	}
	if len(scenarios) == 0 {
		scenarios = fallbackScenarios
	}
	if summary == "" {
		summary = fmt.Sprintf("social 内核模拟 %d agent × %d 轮: 终局形成 %d 个立场簇, 检测到 %d 类涌现。",
			len(k.agents), k.cfg.Rounds, len(clusters), len(k.emergent))
	}

	trajectory := make(map[string][]float64, len(k.agents))
	// 立场轨迹: 从 stance 事件重建
	for _, a := range k.agents {
		for _, ev := range k.events {
			if ev.Type != "stance" || ev.AgentID != a.ID {
				continue
			}
			var p struct {
				Stance float64 `json:"stance"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				trajectory[a.ID] = append(trajectory[a.ID], p.Stance)
			}
		}
	}
	curves := map[string][]float64{
		"variance":   {},
		"entropy":    {},
		"clustering": {},
	}
	for _, m := range k.metrics {
		curves["variance"] = append(curves["variance"], m.Variance)
		curves["entropy"] = append(curves["entropy"], m.Entropy)
		curves["clustering"] = append(curves["clustering"], m.Clustering)
	}

	k.notify(chatID, fmt.Sprintf("🌐 social 内核完成: %d 簇, 涌现 %d 类", len(clusters), len(k.emergent)))
	return &SimulationResult{
		Mode:         "social",
		Rounds:       k.cfg.Rounds,
		Scenarios:    scenarios,
		Emergent:     k.emergent,
		Summary:      summary,
		CreatedAt:    time.Now(),
		Trajectory:   trajectory,
		MetricCurves: curves,
	}
}

// emit 写 SimEvent: 内存留档 + 挂接的 recorder (JSONL 兜底或注入的 SwarmStore)。
func (k *SocialKernel) emit(ev SimEvent) {
	k.events = append(k.events, ev)
	if k.recorder != nil {
		_ = k.recorder.AppendSimEvent(&ev) // 记录失败不阻断模拟
	}
}

// normalizeAction 收敛非法 action 为 listen。
func normalizeAction(a string) string {
	switch a {
	case "speak", "listen", "interview", "adopt":
		return a
	default:
		return "listen"
	}
}

// clampStance 限幅到 [-1,1]。
func clampStance(x float64) float64 { return clampF(x, -1, 1) }

// clamp01 限幅到 [0,1]。
func clamp01(x float64) float64 { return clampF(x, 0, 1) }

// clampF 通用限幅。
func clampF(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// stanceVariance 立场方差。
func stanceVariance(st []float64) float64 {
	if len(st) == 0 {
		return 0
	}
	var mean float64
	for _, v := range st {
		mean += v
	}
	mean /= float64(len(st))
	var s float64
	for _, v := range st {
		d := v - mean
		s += d * d
	}
	return s / float64(len(st))
}

// stanceEntropy 立场分布熵: 5 桶直方图的 Shannon 熵。
func stanceEntropy(st []float64, bins int) float64 {
	if len(st) == 0 {
		return 0
	}
	counts := make([]int, bins)
	for _, v := range st {
		idx := int((clampStance(v) + 1) / 2 * float64(bins))
		if idx >= bins {
			idx = bins - 1
		}
		if idx < 0 {
			idx = 0
		}
		counts[idx]++
	}
	var h float64
	n := float64(len(st))
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// stanceClustering 立场图聚类系数 (transitivity):
// 按 |Δstance| < threshold 连边, 对每个度>=2 的节点统计邻居间实际边数 / 可能边数, 取平均。
func stanceClustering(st []float64, threshold float64) float64 {
	n := len(st)
	if n < 3 {
		return 0
	}
	adj := make([][]bool, n)
	for i := range adj {
		adj[i] = make([]bool, n)
	}
	deg := make([]int, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if math.Abs(st[i]-st[j]) < threshold {
				adj[i][j], adj[j][i] = true, true
				deg[i]++
				deg[j]++
			}
		}
	}
	var sum float64
	var cnt int
	for i := 0; i < n; i++ {
		if deg[i] < 2 {
			continue
		}
		links := 0
		for j := 0; j < n; j++ {
			if !adj[i][j] {
				continue
			}
			for l := j + 1; l < n; l++ {
				if adj[i][l] && adj[j][l] {
					links++
				}
			}
		}
		sum += 2 * float64(links) / float64(deg[i]*(deg[i]-1))
		cnt++
	}
	if cnt == 0 {
		return 0
	}
	return sum / float64(cnt)
}
