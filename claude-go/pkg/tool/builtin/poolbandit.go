// poolbandit.go —— 13.7-P1 三因子个性化: Thompson 采样选择器 + 选择留痕
// (docforge planning-skills-surge §13.7.4)。
//
// 规划锚点:
//   - 排序键 = 相关性 × (1 + α·团队历史加权分)。"团队历史加权分" 用 Beta 后验的
//     Thompson 采样表达: 每个 (team, role, asset) 一组 Beta(α,β) 后验, 加载/奖励
//     事件更新后验, pool_search 每次按采样值重排 (探索+利用一体)。
//   - 选择留痕: pool_search / pool_load 写一条 KindPolicyDecision Span —— 记录
//     这次向模型展示了什么 (candidates) / 模型选了什么 (selected / skills)。
//     它同时是 skillaudit 配对归因 (13.8.6) 的 per-run 技能证据源 (attrs.skills,
//     与 <role_skills> 反解同一字段语义)。
//   - 选择层 ≠ 治理层: bandit 只影响 pool_search 的排序, 不碰 pool_load 的治理
//     判定 (denied/allowed fail-closed 在 §13.7.3); bandit 降权 ≠ 退役。
//
// α 从 CLAUDE_GO_POOL_ALPHA 读取, 默认 0 = 个性化关闭 (P0 行为零变化, 平滑迁移)。
// 零相关性的检索 (空查询) 个性化不表达 —— 乘法公式的字面语义: 0×(1+α·s)=0,
// 高后验资产的"常驻免 search"是 P2 的数量护栏职责, 不在这里混做。
//
// 持久化与回灌: Beta 后验落 <state>/evolution/poolbandit.json; 冷启动回灌扫描
// <state>/statestore/log/trace-*.jsonl (选择留痕) × <state>/evolution/rewards.jsonl
// (奖励), 按 RunID 配对后逐 (team, role, skill) 记一次 trial。回灌用**行数水位线**
// (reward_lines) 防重复计入 —— 仿 skillaudit 的只读文件纪律, 不为读一行 JSON 拖
// statestore 运行期依赖进本包; 文件格式由两侧 json tag 锁定, 漂移由单测焊住。
package builtin

import (
	"bufio"
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

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
)

// EnvPoolAlpha 个性化强度开关 (α=0 关闭, 平滑迁移同 PoolEnabled 模式)。
const EnvPoolAlpha = "CLAUDE_GO_POOL_ALPHA"

// 常驻面阈值 (13.7-P2「高后验资产常驻免 search」): 聚合后验均值 ≥ posteriorFloor
// 且样本量 ≥ minTrials 才进常驻面。minTrials 防小样本偶然 (1/1 也 100%); 免 search
// 的名额还受 ResidentLimit 上限约束 (超阈值太多时按均值降序取前 N —— 常驻是
// 广告面 token 预算, 不是后验合格证)。
const (
	residentPosteriorFloor = 0.7
	residentMinTrials      = 5
	ResidentLimit          = 12
)

// poolAlphaFromEnv 解析个性化强度。非法/未设置一律 0 (默认关)。
func poolAlphaFromEnv(getenv func(string) string) float64 {
	if getenv == nil {
		return 0
	}
	v := strings.TrimSpace(getenv(EnvPoolAlpha))
	if v == "" {
		return 0
	}
	var a float64
	if _, err := fmt.Sscanf(v, "%g", &a); err != nil || a < 0 {
		return 0
	}
	return a
}

// ---------------------------------------------------------------------------
// poolBandit (team, role, asset) → Beta 后验
// ---------------------------------------------------------------------------

// keySep 后验键分隔符 (单元分隔符, 不会出现在工具/技能名里)。
const keySep = "\x1f"

// poolBandit Thompson 采样 bandit: 每个 (team, role, asset) 一组 Beta 后验。
// 并发安全 (pool_load 与回灌/排序跨 goroutine)。
type poolBandit struct {
	mu    sync.Mutex
	alpha map[string]float64
	beta  map[string]float64
	// rewardLines 回灌水位线: rewards.jsonl 已消费的行数 (追加写文件, 行序稳定)。
	rewardLines int64
	// path 持久化路径 (空 = 纯内存, 不落盘)。
	path string
	// rng Thompson 采样随机源 (测试可注入定种)。
	rng *rand.Rand
}

// banditState 持久化形态 (与 poolBandit 字段一一对应; json tag 即契约)。
type banditState struct {
	Alpha       map[string]float64 `json:"alpha"`
	Beta        map[string]float64 `json:"beta"`
	RewardLines int64              `json:"reward_lines"`
}

func banditKey(team, role, asset string) string {
	return team + keySep + role + keySep + asset
}

// newPoolBandit 构建后验表; path 非空且文件存在时先载入 (坏文件静默重开 —— 后验
// 是可重建的缓存, 不是台账, 丢了就从空表+重新回灌来)。
func newPoolBandit(path string) *poolBandit {
	b := &poolBandit{
		alpha: make(map[string]float64),
		beta:  make(map[string]float64),
		path:  path,
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if path == "" {
		return b
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var st banditState
	if json.Unmarshal(data, &st) != nil {
		return b
	}
	if st.Alpha != nil {
		b.alpha = st.Alpha
	}
	if st.Beta != nil {
		b.beta = st.Beta
	}
	b.rewardLines = st.RewardLines
	return b
}

// posterior 返回 (α, β); 未登记的键即先验 Beta(1,1) (均匀, 纯探索)。
func (b *poolBandit) posterior(team, role, asset string) (float64, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := banditKey(team, role, asset)
	return b.alpha[k], b.beta[k]
}

// mean 后验均值 (测试/诊断用; 选择走 sample)。
func (b *poolBandit) mean(team, role, asset string) float64 {
	a, beta := b.posterior(team, role, asset)
	if a+beta <= 0 {
		return 0.5
	}
	return a / (a + beta)
}

// update 记一次 trial: success=true 加权 α, 否则 β。
func (b *poolBandit) update(team, role, asset string, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := banditKey(team, role, asset)
	if success {
		b.alpha[k]++
	} else {
		b.beta[k]++
	}
}

// sample Thompson 采样 Beta(α,β)。Marsaglia-Tsang gamma 变换 (shape<1 用
// u^(1/shape) boost), 与 swarm_intel.BanditRouter 的 betaSample 同算法 —— 那边
// 的实现是小写私有不可 import, 本包重实现, 两处漂移由各自单测锁。
func (b *poolBandit) sample(team, role, asset string) float64 {
	a, beta := b.posterior(team, role, asset)
	if a <= 0 && beta <= 0 {
		return 0.5
	}
	b.mu.Lock()
	rng := b.rng
	b.mu.Unlock()
	return betaSample(rng, math.Max(a, 1), math.Max(beta, 1))
}

// save 落盘 (临时文件+rename 原子替换; 失败静默 —— 后验丢了可回灌重建)。
func (b *poolBandit) save() {
	if b == nil || b.path == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := banditState{Alpha: b.alpha, Beta: b.beta, RewardLines: b.rewardLines}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, b.path)
}

// gammaSample Marsaglia-Tsang (2000) gamma 采样, shape > 0。
func gammaSample(rng *rand.Rand, shape float64) float64 {
	if shape < 1 {
		return gammaSample(rng, shape+1) * math.Pow(rng.Float64(), 1/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// betaSample Beta(a,b) 采样 = X/(X+Y), X~Gamma(a,1), Y~Gamma(b,1)。
func betaSample(rng *rand.Rand, a, b float64) float64 {
	if a+b == 0 {
		return 0.5
	}
	x := gammaSample(rng, a)
	y := gammaSample(rng, b)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// ---------------------------------------------------------------------------
// 冷启动回灌: 选择留痕 × rewards.jsonl, 按 RunID 配对
// ---------------------------------------------------------------------------

// poolSpanRow 选择留痕 Span 的读侧形态 (与 tracestore.Span 的 json tag 对齐)。
type poolSpanRow struct {
	TraceID string `json:"trace_id"`
	Kind    string `json:"kind"`
	Attrs   struct {
		Team   string `json:"team"`
		Role   string `json:"role"`
		Skills []any  `json:"skills"`
	} `json:"attrs"`
}

// poolRewardRow 奖励行的读侧形态 (与 agent.RewardEvent 的 json tag 对齐)。
type poolRewardRow struct {
	TS     int64   `json:"ts"`
	RunID  string  `json:"run_id"`
	Source string  `json:"source"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
}

// runEvidence 一个 run 的选择留痕。
type runEvidence struct {
	Team   string
	Role   string
	Skills map[string]bool
}

// runAgg 一个 run 的新增奖励聚合 (行内按源加权, 与 skillaudit runScore 同口径)。
type runAgg struct {
	weighted  float64
	weightSum float64
}

func (a runAgg) positive() bool {
	if a.weightSum <= 0 {
		return false
	}
	return a.weighted/a.weightSum > 0
}

// coldStart 回灌: 只消费水位线之后的新奖励行, 与选择留痕按 RunID 配对,
// 有留痕的 (team, role, skill) 记一次 trial (success = run 加权分为正), 然后
// 推进水位线并落盘。trace 文件被 TTL 清理而奖励还在时, 配对自然落空 —— 只推进
// 水位线, 不产生 trial (可接受的已知边界, 与 skillaudit 配对语义一致)。
func (b *poolBandit) coldStart(stateRoot string) {
	if b == nil || stateRoot == "" {
		return
	}
	// 1) runID → 选择留痕 (team/role/skills)。
	evidence := map[string]runEvidence{}
	entries, err := os.ReadDir(filepath.Join(stateRoot, "statestore", "log"))
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "trace-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			readJSONLines(filepath.Join(filepath.Join(stateRoot, "statestore", "log"), name), func(line []byte) {
				var row poolSpanRow
				if json.Unmarshal(line, &row) != nil {
					return
				}
				if row.Kind != tracestore.KindPolicyDecision || strings.TrimSpace(row.TraceID) == "" {
					return
				}
				ev := evidence[row.TraceID]
				ev.Team, ev.Role = row.Attrs.Team, row.Attrs.Role
				if ev.Skills == nil {
					ev.Skills = make(map[string]bool)
				}
				for _, s := range row.Attrs.Skills {
					if name, ok := s.(string); ok && name != "" {
						ev.Skills[name] = true
					}
				}
				evidence[row.TraceID] = ev
			})
		}
	}
	// 2) 水位线之后的新奖励行按 run 聚合。
	rewardsPath := filepath.Join(stateRoot, "evolution", "rewards.jsonl")
	var aggs = map[string]*runAgg{}
	var newTotal int64
	var watermark int64
	b.mu.Lock()
	watermark = b.rewardLines
	b.mu.Unlock()
	idx := int64(0)
	readJSONLines(rewardsPath, func(line []byte) {
		idx++
		if idx <= watermark {
			return
		}
		var row poolRewardRow
		if json.Unmarshal(line, &row) != nil {
			return
		}
		newTotal = idx
		if strings.TrimSpace(row.RunID) == "" {
			return // 无法归因, 只推进水位线
		}
		w := row.Weight
		if w <= 0 {
			w = 0.5 // 未落盘权重的回退值, 与 RewardSourceWeight(unknown) 同口径
		}
		agg := aggs[row.RunID]
		if agg == nil {
			agg = &runAgg{}
			aggs[row.RunID] = agg
		}
		agg.weighted += w * row.Value
		agg.weightSum += w
	})
	// 3) 配对: 有留痕且有新奖励的 run → 逐 (team, role, skill) 记 trial。
	trials := 0
	for runID, agg := range aggs {
		ev, ok := evidence[runID]
		if !ok || len(ev.Skills) == 0 {
			continue
		}
		success := agg.positive()
		for skill := range ev.Skills {
			b.update(ev.Team, ev.Role, skill, success)
			trials++
		}
	}
	// 4) 水位线推进到实际读到的行数 (文件被外部截短/替换时 newTotal 也会如实
	//    回退, 下次从头重灌 —— 后验会被重复计入, 但那是文件被破坏后的次要问题)。
	if newTotal > watermark || newTotal > 0 {
		b.mu.Lock()
		b.rewardLines = newTotal
		b.mu.Unlock()
	}
	if trials > 0 {
		b.save()
	}
}

// readJSONLines 逐行读 JSONL, 坏行/不存在静默跳过 (回灌是增强项, 不阻塞装配)。
func readJSONLines(path string, fn func(line []byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 与 skillaudit 同规格
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		fn(line)
	}
}

// ---------------------------------------------------------------------------
// PoolPolicy 共享选择策略 (包装三件的 P1 增强面)
// ---------------------------------------------------------------------------

// PoolPolicy 池选择策略: Thompson bandit + 个性化强度 α + span 留痕。
// 由装配方创建并注入 RegisterPoolTools; 三个包装工具共享同一实例。
// nil *PoolPolicy 处处安全 = 纯 P0 行为。
type PoolPolicy struct {
	mu         sync.Mutex
	bandit     *poolBandit
	stateRoot  string
	traceStore *tracestore.Store
	alpha      float64
}

// NewPoolPolicy 构建池选择策略。stateRoot 非空时立即初始化 bandit (载入持久化 +
// 冷启动回灌); stateRoot 运行期才就绪的装配方 (CLI) 先传空串, 之后调 SetStateDir。
func NewPoolPolicy(stateRoot string, ts *tracestore.Store) *PoolPolicy {
	pp := &PoolPolicy{stateRoot: stateRoot, traceStore: ts, alpha: poolAlphaFromEnv(os.Getenv)}
	pp.initBandit(stateRoot)
	return pp
}

// initBandit 惰性初始化 bandit (幂等; 已初始化则不动)。
func (pp *PoolPolicy) initBandit(stateRoot string) {
	if pp == nil || stateRoot == "" {
		return
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.bandit == nil {
		pp.bandit = newPoolBandit(filepath.Join(stateRoot, "evolution", "poolbandit.json"))
	}
	pp.stateRoot = stateRoot
	pp.bandit.coldStart(stateRoot)
}

// SetStateDir 后置注入状态根 (CLI 装配序: 池装配在前、stateDir 在后才可算出,
// 仿 agentTool.SetTraceStore 的两段式注入先例)。
func (pp *PoolPolicy) SetStateDir(stateRoot string) {
	pp.initBandit(stateRoot)
}

// SetTraceStore 后置注入轨迹底座 (span 留痕; nil = 不留痕, fail-open)。
func (pp *PoolPolicy) SetTraceStore(ts *tracestore.Store) {
	if pp == nil {
		return
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.traceStore = ts
}

// rankBoost 个性化加成 = α·ThompsonSample。nil/未启用/α≤0 恒 0 (保 P0 排序)。
func (pp *PoolPolicy) rankBoost(team, role, asset string) float64 {
	if pp == nil || pp.alpha <= 0 {
		return 0
	}
	pp.mu.Lock()
	b, ts := pp.bandit, pp.traceStore
	pp.mu.Unlock()
	if b == nil {
		return 0
	}
	_ = ts
	return pp.alpha * b.sample(team, role, asset)
}

// noteSelection 选择反馈: 加载成功记一次弱正 trial (popularity 先验 —— "这个团队
// 真的加载过"), 负向修正来自奖励回灌 (rewards.jsonl 的 gate/user 信号)。落盘
// 每次同步执行: 加载是人/模型时间尺度事件, 频率低到不值得做批量缓冲。
func (pp *PoolPolicy) noteSelection(team, role, asset string) {
	if pp == nil {
		return
	}
	pp.mu.Lock()
	b := pp.bandit
	pp.mu.Unlock()
	if b == nil {
		return
	}
	b.update(team, role, asset, true)
	b.save()
}

// writeSpan 选择留痕: 一条 KindPolicyDecision Span。fail-open —— 未装配 traceStore
// 时零成本跳过 (与 agent.writePolicyDecisionSpan 同纪律: 采集是观测不是治理)。
// TraceID = ctx RunID; feishu 主会话/嵌套路径无 RunID 时落 trace-orphan 桶
// (与 policy_decision 同一已知边界)。
func (pp *PoolPolicy) writeSpan(ctx context.Context, name, input string, attrs map[string]any) {
	if pp == nil {
		return
	}
	pp.mu.Lock()
	ts := pp.traceStore
	pp.mu.Unlock()
	if ts == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := trace.From(ctx)
	attrs["pool_alpha"] = pp.alpha
	ts.Write(tracestore.Span{
		TraceID:  ids.RunID,
		SpanID:   trace.NewID("s"),
		ParentID: ids.TurnID,
		Kind:     tracestore.KindPolicyDecision,
		Name:     name,
		NodeID:   ids.NodeID,
		TurnID:   ids.TurnID,
		InputRef: ts.MakeRef(input),
		Attrs:    attrs,
		TS:       time.Now().UnixMilli(),
	})
}

// poolIdentity 从 ToolContext 取 (team, role); nil tctx 安全 (单测直调形态)。
func poolIdentity(tctx *tool.ToolContext) (team, role string) {
	if tctx == nil {
		return "", ""
	}
	return tctx.Team, tctx.Role
}

// ---------------------------------------------------------------------------
// 13.7-P2 高后验常驻面
// ---------------------------------------------------------------------------

// residentAgg 常驻候选的聚合视图 (单资产跨全部 (team,role) 键)。
type residentAgg struct {
	Name   string
	Mean   float64 // 该资产全部键中最高后验均值
	Trials float64 // 全部键的 (α+β) 总和 (样本量)
}

// residentCandidates 扫后验表, 返回达标的资产 (按 均值×样本量 降序, 超出
// ResidentLimit 截断)。键形如 team\x1frole\x1fasset —— 常驻面是**会话级广告位**
// (主会话无团队语义, CLI 的空 team/role 键与 team_stage 键分开), 这里跨键聚合:
// 同一资产任一身份达标即候选。仅统计池内名字是调用方 (SetResidentFromBandit) 的
// 事: 本方法只管后验表语义。
func (b *poolBandit) residentCandidates() []residentAgg {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	best := map[string]*residentAgg{}
	// update 只写 alpha 或 beta 之一 (全失败键只在 beta 表), 遍历两表键的并集
	// 才能不错过任何 trial —— 只扫 alpha 会漏掉全失败键的样本量累加。
	keys := make(map[string]struct{}, len(b.alpha)+len(b.beta))
	for k := range b.alpha {
		keys[k] = struct{}{}
	}
	for k := range b.beta {
		keys[k] = struct{}{}
	}
	for k := range keys {
		a, beta := b.alpha[k], b.beta[k]
		parts := strings.SplitN(k, keySep, 3)
		if len(parts) < 3 || parts[2] == "" {
			continue
		}
		name := parts[2]
		total := a + beta
		m := 0.5
		if total > 0 {
			m = a / total
		}
		cur := best[name]
		if cur == nil {
			cur = &residentAgg{Name: name}
			best[name] = cur
		}
		cur.Trials += total
		if m > cur.Mean {
			cur.Mean = m
		}
	}
	var cands []residentAgg
	for _, ag := range best {
		if ag.Mean < residentPosteriorFloor || ag.Trials < residentMinTrials {
			continue
		}
		cands = append(cands, *ag)
	}
	sort.Slice(cands, func(i, j int) bool {
		si, sj := cands[i].Mean*cands[i].Trials, cands[j].Mean*cands[j].Trials
		if si != sj {
			return si > sj
		}
		return cands[i].Name < cands[j].Name // 同分按名称确定性
	})
	if len(cands) > ResidentLimit {
		cands = cands[:ResidentLimit]
	}
	return cands
}

// assetMean 跨全部 (team,role) 键的最高后验均值 (L1 配给评分; 无记录 = 0)。
func (b *poolBandit) assetMean(asset string) float64 {
	if b == nil || asset == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	best := 0.0
	// 并集遍历 (与 residentCandidates 同因: 全失败键只在 beta 表)。
	keys := make(map[string]struct{}, len(b.alpha)+len(b.beta))
	for k := range b.alpha {
		keys[k] = struct{}{}
	}
	for k := range b.beta {
		keys[k] = struct{}{}
	}
	for k := range keys {
		parts := strings.SplitN(k, keySep, 3)
		if len(parts) < 3 || parts[2] != asset {
			continue
		}
		a, beta := b.alpha[k], b.beta[k]
		total := a + beta
		m := 0.5
		if total > 0 {
			m = a / total
		}
		if m > best {
			best = m
		}
	}
	return best
}

// SetResidentFromBandit 聚合后验并刷新池常驻面 (13.7-P2)。nil 安全 (policy/bandit/
// pool 任一未装配 = 零操作)。返回本次常驻面名单 (len 0 = 无达标或无 bandit)。
func (pp *PoolPolicy) SetResidentFromBandit(pool *tool.Pool) []string {
	if pp == nil || pool == nil {
		return nil
	}
	pp.mu.Lock()
	b := pp.bandit
	pp.mu.Unlock()
	if b == nil {
		return nil
	}
	cands := b.residentCandidates()
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.Name
	}
	pool.SetResident(names)
	return names
}

// RankScore 资产的清单描述位配给评分 (13.7-P2 L1 配给, SetRanker 注入):
// 跨全部 (team,role) 键取该资产最高后验均值 —— 与 residentCandidates 同一聚合
// 语义 (常驻面和描述位都是会话级广告资源, 不分团队切)。后验表无记录 = 0
// (排到无数据队尾, 与"低频资产只留名称"同义); 全失败资产均值 0, 自然垫底。
func (pp *PoolPolicy) RankScore(asset string) float64 {
	if pp == nil {
		return 0
	}
	pp.mu.Lock()
	b := pp.bandit
	pp.mu.Unlock()
	if b == nil {
		return 0
	}
	return b.assetMean(asset)
}
