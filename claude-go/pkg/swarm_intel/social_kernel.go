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
	Mode        string  // 模拟模式 (social/game/crisis/org/creative/market/policy/tech), 决定 prompt 措辞与立场语义
	NumAgents   int     // agent 数, 默认 6
	Rounds      int     // 轮数预算帽, 默认 6 (自适应终止可能提前收敛)
	HistoryK    int     // 每轮输入携带的最近环境消息条数, 默认 10
	MaxConcur   int     // 轮内 LLM 并发, 默认 3
	DefaultStub float64 // spawn 未给 stubbornness 时的默认值 0.3
	// 自适应终止 (①): 跑满 MinRounds 后, 全员 |Δstance| < Epsilon ⇒ 收敛;
	// 群体熵变化 < EntropyDelta 连续 2 轮 ⇒ 平台期; 都不满足 ⇒ 跑满 Rounds (budget)。
	// FJ 理论收敛是渐近的, 固定轮次只是预算帽 —— 提前收敛既省 LLM 调用又不丢信息。
	Epsilon      float64 // 默认 0.03
	EntropyDelta float64 // 默认 0.02
	MinRounds    int     // 默认 2
	// 网络拓扑 (③): 影响图结构。full=完全图 (v1 行为); ring=环格每节点连 ±K 个邻居;
	// smallworld=环格+10% 随机重连。邻居集决定 FJ 拉力的来源, 是极化/回音室的结构性来源。
	Topology string // 默认 full
	TopoK    int    // 环格度数, 默认 2
	// Rewire 每轮更新后按立场同质性重连: 断开最远的邻居, 连上最近的非邻居 (echo chamber 动力学)。
	Rewire bool
	// MemoryRounds 轮内 prompt 携带每个邻居最近 N 轮的立场轨迹 (而非只给当前值), 默认 3; 0=关闭。
	MemoryRounds int
	// ShuffleStances > 0 时, spawn 后用该 seed 随机打乱立场分配 (S3 对抗对照开关,
	// 仅测试使用): 用于验证涌现来自真实立场动力学而非 prompt 固定叙事。
	ShuffleStances int64
}

// DefaultSocialKernelConfig 返回保守默认: 6 agent × 6 轮预算帽, 完全图拓扑。
func DefaultSocialKernelConfig() SocialKernelConfig {
	return SocialKernelConfig{
		Mode:         "social",
		NumAgents:    6,
		Rounds:       6,
		HistoryK:     10,
		MaxConcur:    3,
		DefaultStub:  0.3,
		Epsilon:      0.03,
		EntropyDelta: 0.02,
		MinRounds:    2,
		Topology:     "full",
		TopoK:        2,
		MemoryRounds: 3,
	}
}

// cfgOrDefaults 把 SimulationConfig 的零值覆盖到内核默认上 (webapp/CLI 只传想改的项)。
func (c SocialKernelConfig) withOverrides(cfg SimulationConfig) SocialKernelConfig {
	if cfg.Agents > 0 {
		c.NumAgents = cfg.Agents
	}
	if cfg.Rounds > 0 {
		c.Rounds = cfg.Rounds
	}
	if cfg.Topology != "" {
		c.Topology = cfg.Topology
	}
	if cfg.Rewire {
		c.Rewire = true
	}
	if cfg.MemoryRounds > 0 {
		c.MemoryRounds = cfg.MemoryRounds
	}
	if cfg.Epsilon > 0 {
		c.Epsilon = cfg.Epsilon
	}
	if c.Mode == "" {
		c.Mode = "social"
	}
	return c
}

// modeProfile 模式档案: 同一套 FJ 轮次内核在不同模拟模式下的措辞与立场语义。
// 数学 (FJ 更新/指标/涌现判据) 与模式无关, 只有语言层按模式调味。
type modeProfile struct {
	StanceLabel string // 立场数值的含义, 如 "对议题的支持度"
	AgentNoun   string // agent 的称呼, 如 "社会角色"
	SceneNoun   string // 模拟对象的称呼, 如 "议题"
	AnalystRole string // report 润色时的分析员角色
	SpawnExtra  string // spawn prompt 的附加设计要求
}

// kernelModeProfile 按模式取档案; 未知模式回退 social。
func kernelModeProfile(mode string) modeProfile {
	switch mode {
	case "org":
		return modeProfile{
			StanceLabel: "对组织变革的支持度", AgentNoun: "组织成员", SceneNoun: "组织情境",
			AnalystRole: "组织动力学报告分析员",
			SpawnExtra:  "成员应覆盖不同部门/职级/资历, 立场分歧来自利益位置而非抽象态度",
		}
	case "game":
		return modeProfile{
			StanceLabel: "对合作策略的偏好", AgentNoun: "博弈方", SceneNoun: "博弈情境",
			AnalystRole: "博弈推演报告分析员",
			SpawnExtra:  "各方利益诉求必须真实冲突, 立场代表其策略倾向",
		}
	case "crisis":
		return modeProfile{
			StanceLabel: "对危机严重度的判断", AgentNoun: "危机相关方", SceneNoun: "危机情境",
			AnalystRole: "危机推演报告分析员",
			SpawnExtra:  "相关方覆盖决策者/媒体/公众/对手方/盟友, 判断分歧来自信息与立场差异",
		}
	case "market":
		return modeProfile{
			StanceLabel: "对价格走向的预期", AgentNoun: "市场参与者", SceneNoun: "市场情境",
			AnalystRole: "市场竞争报告分析员",
			SpawnExtra:  "参与者覆盖不同份额/成本结构/策略类型, 预期分歧来自仓位差异",
		}
	case "policy":
		return modeProfile{
			StanceLabel: "对政策效果的态度", AgentNoun: "利益相关方", SceneNoun: "政策情境",
			AnalystRole: "政策推演报告分析员",
			SpawnExtra:  "相关方覆盖受益方/受损方/执行方/中立观察者, 态度分歧来自利益分配",
		}
	case "tech":
		return modeProfile{
			StanceLabel: "对技术路线前景的判断", AgentNoun: "技术阵营代表", SceneNoun: "技术演进情境",
			AnalystRole: "技术演进报告分析员",
			SpawnExtra:  "阵营覆盖不同技术路线及其生态位, 判断分歧来自押注差异",
		}
	case "creative":
		return modeProfile{
			StanceLabel: "对创意方向的偏好", AgentNoun: "创意视角人格", SceneNoun: "创意命题",
			AnalystRole: "创意涌现报告分析员",
			SpawnExtra:  "视角人格应来自不同学科/经验/审美 (如工程师/艺术家/商人/用户), 偏好分歧来自世界观",
		}
	case "rumor":
		return modeProfile{
			StanceLabel: "对信息真实性的相信度", AgentNoun: "信息受众", SceneNoun: "传言情境",
			AnalystRole: "信息传播报告分析员",
			SpawnExtra:  "受众覆盖不同信息渠道依赖度/媒介素养/既有立场, 相信度分歧来自信源信任差异",
		}
	case "health":
		return modeProfile{
			StanceLabel: "对防护行为的依从意愿", AgentNoun: "公众个体", SceneNoun: "公共健康情境",
			AnalystRole: "公共健康行为报告分析员",
			SpawnExtra:  "个体覆盖不同年龄/职业/健康观念/风险感知, 依从分歧来自风险收益判断差异",
		}
	case "consumer":
		return modeProfile{
			StanceLabel: "对产品的接受度", AgentNoun: "消费者画像", SceneNoun: "消费情境",
			AnalystRole: "消费行为报告分析员",
			SpawnExtra:  "消费者覆盖不同预算/品类经验/品牌忠诚度/尝鲜倾向, 接受分歧来自使用场景差异",
		}
	case "team":
		return modeProfile{
			StanceLabel: "对方案的倾向", AgentNoun: "团队成员", SceneNoun: "团队决策情境",
			AnalystRole: "团队动力学报告分析员",
			SpawnExtra:  "成员覆盖不同专业背景/职级/风险偏好/发言风格, 倾向分歧来自立场与信息差",
		}
	case "education":
		return modeProfile{
			StanceLabel: "对新做法的采纳意愿", AgentNoun: "学生与教育者", SceneNoun: "教育情境",
			AnalystRole: "教育生态报告分析员",
			SpawnExtra:  "角色覆盖不同学段/成绩层次/家庭背景/教师视角, 采纳分歧来自同伴压力与效用判断",
		}
	case "climate":
		return modeProfile{
			StanceLabel: "对环保行为的认同度", AgentNoun: "社区居民", SceneNoun: "环保行为情境",
			AnalystRole: "环保行为报告分析员",
			SpawnExtra:  "居民覆盖不同收入/年龄/居住形态/环保意识, 认同分歧来自便利性与成本感知",
		}
	default: // social
		return modeProfile{
			StanceLabel: "对议题的支持度", AgentNoun: "社会角色", SceneNoun: "议题",
			AnalystRole: "社会模拟报告分析员",
			SpawnExtra:  "角色身份/性格/立场要有真实分歧, 不要挤在中间",
		}
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
	profile   modeProfile
	objective string
	runID     string

	agents  []*SocialAgent
	env     []envMessage
	metrics []roundMetrics
	events  []SimEvent

	// 过程明细快照 (供 KernelDetail): spawn 实际 prompt / 轮内模板 / spawn 后初始终立场
	spawnPrompt   string
	roundTemplate string
	initialStance map[string]float64

	// emergentKinds 记录已触发的涌现类型, 避免同一类型每轮重复上报。
	emergent     []string
	emergentKind map[string]bool

	// ③ 网络拓扑: adjacency[a][b]=a 受 b 影响的基础权重 (0=无边)。
	// full 拓扑下所有人互为邻居 (等价 v1 行为); ring/smallworld 限制 FJ 拉力来源。
	adjacency map[string]map[string]float64
	// ③ 邻居立场记忆: stanceHistory[agentID] 每轮更新后的立场 (末位=当前),
	// roundPrompt 取最近 MemoryRounds 轮给 agent 看邻居的轨迹而非单点值。
	stanceHistory map[string][]float64
	// ① 自适应终止: 上一轮 |Δstance| < Epsilon 的 agent 本轮跳过 LLM 调用
	// (FJ 项仍然生效), 节省 token 且不动收敛结果。
	frozen map[string]bool
	// ① 停止原因: converged(全员收敛) / entropy_plateau(熵平台期) / budget(跑满预算)。
	stopReason string
	// ① 自适应终止: 实际跑的轮数 (报告与 KernelDetail 用它, 不是预算帽)。
	roundsRun int
	// ③ 可选拓扑持久化 (nil=跳过): 每轮把邻接表写进 sim_{runID}_adj 桶。
	topoStore TopologyStore
}

// NewSocialKernel 创建 social 轮次内核。
func NewSocialKernel(llm LLMClient, notify NotifyFunc, cfg SocialKernelConfig) *SocialKernel {
	if notify == nil {
		notify = func(_, _ string) {}
	}
	if cfg.Mode == "" {
		cfg.Mode = "social"
	}
	if cfg.Epsilon <= 0 {
		cfg.Epsilon = 0.03
	}
	if cfg.EntropyDelta <= 0 {
		cfg.EntropyDelta = 0.02
	}
	if cfg.MinRounds <= 0 {
		cfg.MinRounds = 2
	}
	if cfg.Topology == "" {
		cfg.Topology = "full"
	}
	if cfg.TopoK <= 0 {
		cfg.TopoK = 2
	}
	if cfg.MemoryRounds < 0 {
		cfg.MemoryRounds = 0
	}
	return &SocialKernel{
		llm:           llm,
		notify:        notify,
		cfg:           cfg,
		profile:       kernelModeProfile(cfg.Mode),
		runID:         fmt.Sprintf("%s-%d", cfg.Mode, time.Now().UnixNano()),
		emergentKind:  make(map[string]bool),
		initialStance: make(map[string]float64),
		stanceHistory: make(map[string][]float64),
		frozen:        make(map[string]bool),
	}
}

// TopologyStore 邻接表持久化接口 (③): 由 Engine 注入 AgentDBStore 实现,
// 每桶一次模拟, key="{from}·{to}", value=权重 —— agentDB KV 原语零改动复用。
type TopologyStore interface {
	PutTopologyEdge(ctx context.Context, runID, from, to string, weight float64) error
}

// SetTopologyStore 挂接邻接表持久化 (可选, nil 则不落库, 内存邻接照常工作)。
func (k *SocialKernel) SetTopologyStore(s TopologyStore) { k.topoStore = s }

// SetRecorder 挂接 SimEvent 记录器 (Engine 注入 JSONLStore/AgentDBStore 或兜底实现)。
func (k *SocialKernel) SetRecorder(r SimEventRecorder) { k.recorder = r }

// Events 返回本次运行产生的全部 SimEvent (测试与 JSONL 兜底之外的观测口)。
func (k *SocialKernel) Events() []SimEvent { return k.events }

// Run 执行完整模拟: spawn → initTopology → rounds×(决策+FJ更新+detect[+rewire]) → report。
// ① 自适应终止: 跑满 MinRounds 后, 全员 |Δstance|<Epsilon ⇒ converged;
// 群体熵变化 <EntropyDelta 连续 2 轮 ⇒ entropy_plateau; 否则跑满 Rounds ⇒ budget。
// 提前停轮的 agent 本轮已冻结 (跳过 LLM, FJ 项仍生效), 不丢收敛信息还省调用。
// spawn 后快照初始终立场, 供 KernelDetail 展示每个 agent 的立场变化幅度。
func (k *SocialKernel) Run(ctx context.Context, chatID, objective string) (*SimulationResult, error) {
	k.objective = objective
	if err := k.spawn(ctx); err != nil {
		return nil, fmt.Errorf("%s 内核 spawn 失败: %w", k.cfg.Mode, err)
	}
	for _, a := range k.agents {
		k.initialStance[a.ID] = a.Stance
		k.stanceHistory[a.ID] = []float64{a.Stance}
	}
	k.initTopology(ctx)

	roundsRun := 0
	prevEntropy := -1.0
	plateauStreak := 0
	k.stopReason = "budget"
	for r := 1; r <= k.cfg.Rounds; r++ {
		actions, err := k.runRound(ctx, r)
		if err != nil {
			return nil, err
		}
		maxDelta := k.updateStances(r, actions)
		k.detect(r)
		if k.cfg.Rewire {
			k.rewire(r)
		}
		roundsRun = r

		if r < k.cfg.MinRounds {
			continue
		}
		// converged: 本轮所有 agent (含原本冻结的) 立场变动都 < Epsilon。
		if maxDelta < k.cfg.Epsilon {
			k.stopReason = "converged"
			break
		}
		// entropy_plateau: 熵变化连续 2 轮 < EntropyDelta (第一个 MinRounds 轮不判)。
		m := k.metrics[len(k.metrics)-1]
		if prevEntropy >= 0 && math.Abs(m.Entropy-prevEntropy) < k.cfg.EntropyDelta {
			plateauStreak++
			if plateauStreak >= 2 {
				k.stopReason = "entropy_plateau"
				break
			}
		} else {
			plateauStreak = 0
		}
		prevEntropy = m.Entropy
	}
	k.roundsRun = roundsRun
	return k.report(ctx, chatID), nil
}

// spawn 一次 LLM 调用生成全部 agent。
// prompt 按模式档案调味: 立场数值的语义/角色称呼/附加设计要求随 mode 变化,
// 输出 schema (name/profile/stance/stubbornness) 全模式统一。
func (k *SocialKernel) spawn(ctx context.Context) error {
	n := k.cfg.NumAgents
	p := k.profile
	prompt := fmt.Sprintf(`你是%s模拟角色生成器。围绕%s生成 %d 个立场、身份、性格各异的 %s。

%s: %s

要求:
1. stance 为 -1..1 的显式数值, 语义是"%s" (负=反向/悲观/反对, 正=正向/乐观/支持), 立场应有真实分歧, 不要挤在中间
2. stubbornness 为 0..1 (0.3 左右为普通, 0.7 以上为顽固)
3. profile 一句话写清身份和性格 (<=60字)
4. %s

输出严格JSON数组 (不要markdown代码块, 不要其他文字):
[{"name":"称呼","profile":"身份+性格","stance":0.6,"stubbornness":0.3}]`,
		p.AgentNoun, p.SceneNoun, n, p.AgentNoun, p.SceneNoun, k.objective, p.StanceLabel, p.SpawnExtra)

	resp, err := k.llm.SimpleComplete(ctx, fmt.Sprintf("你是%s生成器。只输出JSON数组。", p.AgentNoun), prompt)
	if err != nil {
		return err
	}
	k.spawnPrompt = prompt // 过程明细: 实际发出的 spawn prompt
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
// ① 上一轮已冻结 (|Δstance|<Epsilon) 的 agent 跳过 LLM 调用, 复用 listen 零增量,
// FJ 拉力仍会在 updateStances 生效 —— 收敛后不再花 token 问"你还变吗"。
func (k *SocialKernel) runRound(ctx context.Context, round int) (map[string]*SocialRoundAction, error) {
	branches := make(map[string]BranchFunc)
	for _, a := range k.agents {
		if k.frozen[a.ID] {
			continue
		}
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
	// 冻结 agent 不发 action 事件 (没跑 LLM, 没有可记录的决策)。
	for _, a := range k.agents {
		if k.frozen[a.ID] {
			actions[a.ID] = &SocialRoundAction{Action: "listen"}
		}
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

// roundPrompt 组装单个 agent 的轮内输入: profile + stance + 最近环境消息 + 邻居立场。
// ③ 邻居集由 adjacency 限定 (full=全员, ring/smallworld=各有向边), 只见邻居不见全图;
// MemoryRounds>0 时邻居给最近 N 轮轨迹趋势 (升/降/平) 而非单点值 ——
// "他三天前反对、昨天沉默、今天中立" 比 "他现在 0.1" 更能驱动立场调整。
// 动态段 (身份/当前立场/邻居/环境消息) 每轮每 agent 不同; 静态模板段只记录一次,
// 供 KernelDetail 展示"轮内 prompt 是怎么设计的"。
func (k *SocialKernel) roundPrompt(a *SocialAgent, round int) string {
	p := k.profile
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n", p.SceneNoun, k.objective)
	fmt.Fprintf(&sb, "你是: %s (%s)\n", a.Name, a.Profile)
	fmt.Fprintf(&sb, "你当前的立场: %.2f (%s, -1反向 .. +1正向), 固执度 %.2f\n", a.Stance, p.StanceLabel, a.Stubbornness)
	fmt.Fprintf(&sb, "第 %d/%d 轮。邻居立场:\n", round, k.cfg.Rounds)
	for _, b := range k.agents {
		if b.ID == a.ID || k.adjacency[a.ID][b.ID] <= 0 {
			continue // ③ 非邻居不可见 (full 拓扑下即全员可见, 行为同 v1)
		}
		if k.cfg.MemoryRounds > 0 {
			fmt.Fprintf(&sb, "- %s: %.2f (近%d轮%s)\n", b.Name, b.Stance, k.cfg.MemoryRounds, k.trajectoryTrend(b.ID))
		} else {
			fmt.Fprintf(&sb, "- %s: %.2f\n", b.Name, b.Stance)
		}
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
	if k.roundTemplate == "" {
		// 模板段 = 静态结构说明 + 决策 schema (动态数值字段以占位符标注)
		neighborLine := "- <邻居name>: <邻居stance> × 拓扑邻居数 (full=全员)\n"
		if k.cfg.MemoryRounds > 0 {
			neighborLine = "- <邻居name>: <邻居stance> (近N轮升/降/平) × 拓扑邻居数\n"
		}
		k.roundTemplate = "<场景>: <模拟目标>\n你是: <name> (<profile>)\n" +
			"你当前的立场: <stance> (" + p.StanceLabel + ", -1反向 .. +1正向), 固执度 <stubbornness>\n" +
			"第 <r>/<rounds> 轮。邻居立场:\n" + neighborLine +
			"[最近环境消息: 上轮的 speak 发言, 最多 HistoryK 条]\n" + sb.String()[strings.LastIndex(sb.String(), "决策:"):]
	}
	return sb.String()
}

// trajectoryTrend ③ 邻居立场轨迹趋势: 取最近 MemoryRounds 轮, 首尾差 >=0.05 判升/降, 否则平。
func (k *SocialKernel) trajectoryTrend(agentID string) string {
	h := k.stanceHistory[agentID]
	n := k.cfg.MemoryRounds
	if len(h) < 2 || n <= 0 {
		return "平"
	}
	start := len(h) - n
	if start < 0 {
		start = 0
	}
	d := h[len(h)-1] - h[start]
	switch {
	case d >= 0.05:
		return "升"
	case d <= -0.05:
		return "降"
	default:
		return "平"
	}
}

// initTopology ③ spawn 后构建影响图 adjacency。
// full=完全图 (v1 行为, 每人受其他所有人影响); ring=环格, i 连 i±1..±K (N<=2K 时退化为完全图);
// smallworld=Watts-Strogatz: 先环格再随机重连 10% 的边 (rand 无种子, 每次模拟不同)。
// 有向语义: adjacency[a][b]>0 表示 "a 受 b 影响" (信息流入 a)。
func (k *SocialKernel) initTopology(ctx context.Context) {
	n := len(k.agents)
	k.adjacency = make(map[string]map[string]float64, n)
	addEdge := func(from, to string) {
		if from == to {
			return
		}
		if k.adjacency[from] == nil {
			k.adjacency[from] = make(map[string]float64)
		}
		k.adjacency[from][to] = 1.0
	}
	switch k.cfg.Topology {
	case "ring", "smallworld":
		K := k.cfg.TopoK
		for i, a := range k.agents {
			for d := 1; d <= K; d++ {
				addEdge(a.ID, k.agents[(i+d)%n].ID)
				addEdge(a.ID, k.agents[(i-d+n)%n].ID)
			}
		}
		if k.cfg.Topology == "smallworld" {
			// WS 重连: 10% 有向边换随机目标 (保持出度, 改变入度分布 → 枢纽节点)。
			ids := make([]string, 0, n)
			for _, a := range k.agents {
				ids = append(ids, a.ID)
			}
			for from, tos := range k.adjacency {
				for to := range tos {
					if rand.Float64() < 0.1 {
						delete(k.adjacency[from], to)
						addEdge(from, ids[rand.Intn(n)])
					}
				}
			}
		}
	default: // full
		for _, a := range k.agents {
			for _, b := range k.agents {
				addEdge(a.ID, b.ID)
			}
		}
	}
	k.persistTopology(ctx)
}

// rewire ③ 每轮 detect 后按立场同质性重连 (echo chamber 动力学):
// 每个 agent 断开立场差最大的邻居, 接上立场差最小的非邻居 (若存在)。
// 只改入边来源, 发出 Type=rewire 事件供过程明细观测网络演化。
func (k *SocialKernel) rewire(round int) {
	nameOf := make(map[string]string, len(k.agents))
	for _, x := range k.agents {
		nameOf[x.ID] = x.Name
	}
	for _, a := range k.agents {
		var farthest string
		farthestD := -1.0
		for to := range k.adjacency[a.ID] {
			var b *SocialAgent
			for _, x := range k.agents {
				if x.ID == to {
					b = x
					break
				}
			}
			if b == nil {
				continue
			}
			if d := math.Abs(a.Stance - b.Stance); d > farthestD {
				farthestD, farthest = d, to
			}
		}
		var closest string
		closestD := 2.0
		for _, b := range k.agents {
			if b.ID == a.ID || k.adjacency[a.ID][b.ID] > 0 {
				continue
			}
			if d := math.Abs(a.Stance - b.Stance); d < closestD {
				closestD, closest = d, b.ID
			}
		}
		if farthest != "" && closest != "" {
			delete(k.adjacency[a.ID], farthest)
			k.adjacency[a.ID][closest] = 1.0
			payload, _ := json.Marshal(map[string]string{
				"kind":     "rewire",
				"evidence": fmt.Sprintf("断开 %s (Δ=%.2f) 改连 %s (Δ=%.2f)", nameOf[farthest], farthestD, nameOf[closest], closestD),
			})
			k.emit(SimEvent{RunID: k.runID, Round: round, Type: "rewire", AgentID: a.ID, Payload: payload})
		}
	}
	k.persistTopology(context.Background())
}

// persistTopology 把当前邻接表落 TopologyStore (best-effort, 失败不阻断模拟)。
func (k *SocialKernel) persistTopology(ctx context.Context) {
	if k.topoStore == nil {
		return
	}
	for from, tos := range k.adjacency {
		for to, w := range tos {
			_ = k.topoStore.PutTopologyEdge(ctx, k.runID, from, to, w)
		}
	}
}

// updateStances FJ 模型立场更新:
//
//	stance_a(t+1) = clamp(stub_a·stance_a(t) + (1-stub_a)·Σ w_ab·stance_b(t) + delta_a)
//
// ③ 邻居集由 adjacency 限定 (full=全员, ring/smallworld=各有向边),
// 边权重 = 基础拓扑权重 × (1 + 最近一轮提及加成), 邻居内归一化。
// 无边/无提及时该 agent 本轮只受自身 stub 与 delta 影响 (孤岛行为)。
// 返回本轮 max|Δstance| 并刷新 frozen 集 (①自适应终止的依据, 也是省 LLM 的开关)。
func (k *SocialKernel) updateStances(round int, actions map[string]*SocialRoundAction) float64 {
	mentions := k.roundMentions()
	maxDelta := 0.0
	k.frozen = make(map[string]bool, len(k.agents))
	for _, a := range k.agents {
		prev := a.Stance
		act := actions[a.ID]
		var pull float64
		var wsum float64
		// ③ 拓扑限定邻居: 只累加有边的 b (full 拓扑即全员)。
		for bID, baseW := range k.adjacency[a.ID] {
			var b *SocialAgent
			for _, x := range k.agents {
				if x.ID == bID {
					b = x
					break
				}
			}
			if b == nil {
				continue
			}
			w := baseW * (1 + mentions[a.ID][b.ID])
			pull += w * b.Stance
			wsum += w
		}
		var neighborMean float64
		switch {
		case wsum > 0:
			neighborMean = pull / wsum
		case len(k.agents) > 1 && k.cfg.Topology == "full":
			// full 拓扑无提及退化: 均匀权重 (保持 v1 行为)。
			var s float64
			for _, b := range k.agents {
				if b.ID != a.ID {
					s += b.Stance
				}
			}
			neighborMean = s / float64(len(k.agents)-1)
		default:
			// 拓扑邻居都没有 (孤岛/尚无发言): 只守自己的立场。
			neighborMean = a.Stance
		}
		next := a.Stubbornness*a.Stance + (1-a.Stubbornness)*neighborMean
		if act != nil {
			next += act.StanceDelta
		}
		a.Stance = clampStance(next)
		// ① 记录本轮 |Δ| 与冻结标记。
		if d := math.Abs(a.Stance - prev); d > maxDelta {
			maxDelta = d
		}
		if math.Abs(a.Stance-prev) < k.cfg.Epsilon {
			k.frozen[a.ID] = true
		}
		k.stanceHistory[a.ID] = append(k.stanceHistory[a.ID], a.Stance)

		payload, _ := json.Marshal(map[string]any{"stance": a.Stance, "stubbornness": a.Stubbornness})
		k.emit(SimEvent{RunID: k.runID, Round: round, Type: "stance", AgentID: a.ID, Payload: payload})
	}
	k.metrics = append(k.metrics, k.computeMetrics())
	return maxDelta
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
	prompt := fmt.Sprintf(`你是%s。基于以下最终立场聚类与涌现证据, 为每个聚类命名一个演化终局场景。

%s: %s
最终立场聚类: %s
检测到的涌现 (带指标证据): %s

要求: 每个场景的概率与规模必须服从聚类数据, 不要另编; description 150字以内。
输出严格JSON: {"scenarios":[{"name":"场景名","description":"150字内","key_events":["事件1","事件2"]}],"summary":"300字以内综合分析"}`,
		k.profile.AnalystRole, k.profile.SceneNoun, k.objective, briefJSON, emergentJSON)
	if resp, err := k.llm.SimpleComplete(ctx, fmt.Sprintf("你是%s。只输出JSON。", k.profile.AnalystRole), prompt); err == nil {
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
		summary = fmt.Sprintf("%s 内核模拟 %d agent × %d/%d 轮 (%s): 终局形成 %d 个立场簇, 检测到 %d 类涌现。",
			k.cfg.Mode, len(k.agents), k.roundsRun, k.cfg.Rounds, k.stopReason, len(clusters), len(k.emergent))
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

	k.notify(chatID, fmt.Sprintf("🌐 %s 内核完成: %d 簇, 涌现 %d 类, %d/%d 轮 (%s)",
		k.cfg.Mode, len(clusters), len(k.emergent), k.roundsRun, k.cfg.Rounds, k.stopReason))
	return &SimulationResult{
		Mode:         k.cfg.Mode,
		Rounds:       k.roundsRun, // ① 实际跑的轮数 (预算帽在 kernel_detail.cfg.rounds)
		Scenarios:    scenarios,
		Emergent:     k.emergent,
		Summary:      summary,
		CreatedAt:    time.Now(),
		Trajectory:   trajectory,
		MetricCurves: curves,
		KernelDetail: k.kernelDetail(),
	}
}

// kernelDetail 汇总过程明细: 参数 + prompt 设计 + agent 档案 + 逐轮事件。
// agent 名称查表用, 事件 payload 按 Type 解到对应字段。
func (k *SocialKernel) kernelDetail() *KernelDetail {
	nameOf := map[string]string{}
	for _, a := range k.agents {
		nameOf[a.ID] = a.Name
	}
	events := make([]KernelEvent, 0, len(k.events))
	for _, ev := range k.events {
		ke := KernelEvent{Round: ev.Round, Type: ev.Type, AgentID: ev.AgentID, Name: nameOf[ev.AgentID]}
		switch ev.Type {
		case "action":
			var p struct {
				Action      string  `json:"action"`
				Content     string  `json:"content"`
				StanceDelta float64 `json:"stance_delta"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				ke.Action, ke.Content, ke.StanceDelta = p.Action, p.Content, p.StanceDelta
			}
		case "stance":
			var p struct {
				Stance float64 `json:"stance"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				ke.Stance = p.Stance
			}
		case "emergence":
			var p struct {
				Kind     string `json:"kind"`
				Evidence string `json:"evidence"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				ke.Kind, ke.Evidence = p.Kind, p.Evidence
			}
		case "rewire": // ③ 同质性重连事件: Content 记录断谁连谁, 网络演化可复现。
			var p struct {
				Kind     string `json:"kind"`
				Evidence string `json:"evidence"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				ke.Kind, ke.Evidence = p.Kind, p.Evidence
			}
		}
		events = append(events, ke)
	}
	designs := make([]KernelAgentDesign, 0, len(k.agents))
	for _, a := range k.agents {
		designs = append(designs, KernelAgentDesign{
			ID:            a.ID,
			Name:          a.Name,
			Profile:       a.Profile,
			InitialStance: k.initialStance[a.ID],
			FinalStance:   a.Stance,
			Stubbornness:  a.Stubbornness,
		})
	}
	return &KernelDetail{
		Cfg: KernelCfgView{
			NumAgents:    k.cfg.NumAgents,
			Rounds:       k.cfg.Rounds,
			HistoryK:     k.cfg.HistoryK,
			MaxConcur:    k.cfg.MaxConcur,
			DefaultStub:  k.cfg.DefaultStub,
			Epsilon:      k.cfg.Epsilon,
			EntropyDelta: k.cfg.EntropyDelta,
			MinRounds:    k.cfg.MinRounds,
			Topology:     k.cfg.Topology,
			TopoK:        k.cfg.TopoK,
			Rewire:       k.cfg.Rewire,
			MemoryRounds: k.cfg.MemoryRounds,
		},
		Prompts: KernelPrompts{
			Spawn:         k.spawnPrompt,
			RoundTemplate: k.roundTemplate,
		},
		Agents:     designs,
		Events:     events,
		StopReason: k.stopReason,
		RoundsRun:  k.roundsRun,
		Topology:   k.topologySnapshot(),
	}
}

// topologySnapshot ③ 把终态邻接表导出为 KernelTopology 边集 (过程明细的网络快照)。
// initTopology 保证 adjacency 非 nil; full/ring/smallworld 终态边集都在其中 (含 rewire 演化)。
func (k *SocialKernel) topologySnapshot() *KernelTopology {
	if k.adjacency == nil {
		return nil
	}
	snap := &KernelTopology{Type: k.cfg.Topology, K: k.cfg.TopoK, Rewire: k.cfg.Rewire}
	for from, tos := range k.adjacency {
		for to, w := range tos {
			snap.Edges = append(snap.Edges, KernelEdge{From: from, To: to, Weight: w})
		}
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		if snap.Edges[i].From != snap.Edges[j].From {
			return snap.Edges[i].From < snap.Edges[j].From
		}
		return snap.Edges[i].To < snap.Edges[j].To
	})
	return snap
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
