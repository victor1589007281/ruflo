package learners

// canary.go —— shadow **比例**灰度 (design/03 §4.6「比例灰度」+ §4.7 `shadow_ratio`)。
//
// ---------------------------------------------------------------------------
// 设计原文与此前的现状
// ---------------------------------------------------------------------------
//
// §4.7 给 agent 的安全字段里有一条「shadow 比例上限 50%」, §4.6 的验收里写着
// 「比例灰度」。此前 `shadow_ratio` 只被 `evo_run_experiment` 写进实验 JSON,
// **运行期一个消费方都没有** —— 于是本仓的灰度是二值的: 草案处 shadow 态就完全
// 不影响运行, 一旦 `evo_promote` 就 100% 生效。中间那一档 (「让候选在 r 比例的
// 真实 run 上生效, 攒配对证据」) 从来不存在。
//
// 同一件事在两处留过字条:
//   - `data.go` 的 `SplitByTime`: "真正的随机配对需要运行期按 shadow_ratio 分流
//     并打点, 属 E4 深化" —— 所以 `evo_status` 只能做**前后对照**, 期间任何别的
//     改动都会混进 uplift。
//   - `skillaudit.go` 文件头: "真正的配对 A/B (带 vs 不带随机分组) 需运行期打点"。
//
// 本文件补的就是「分流」这一半: 一个确定性的臂分配 + 实验记录的读写。
// 「打点」在消费侧 (`pkg/agent/prompt_canary.go` 写 policy_decision Span)。
//
// ---------------------------------------------------------------------------
// 为什么不用 rand —— 这是本文件唯一真正重要的决定
// ---------------------------------------------------------------------------
//
// 直觉写法是 `rand.Float64() < ratio`。它有两个致命后果:
//
//  1. **事后不可考**: 一次 run 出了问题, 没人能回答"这次到底注入了没有"。灰度的
//     全部价值在于"坏了能归因", 而裸随机把归因所需的那一位信息扔了。日志能补记
//     一半, 但日志丢一行就再也算不回来 —— 而臂分配是可以**重算**的。
//  2. **同一 run 内不自洽**: 一次 run 会多次问同一个问题 (每个 stage 一次, 重试
//     再一次)。裸随机会让同一 run 的一部分 stage 走候选、另一部分走基线, 于是这次
//     run 的奖励既不属于实验组也不属于基线组, A/B 结果不可解释。
//
// 故按 `hash(实验ID, RunID) mod 10000` 分桶: 同一 (实验, run) 组合**永远**落在
// 同一侧, 跨进程、跨重启、跨重试都一致, 且事后拿两个 ID 就能重算。
// 用 FNV-1a 而不是 crypto 哈希: 这里要的是均匀分布而不是抗碰撞, 且它在标准库、零
// 分配。哈希输入把两个 ID 用 `\x00` 隔开, 否则 ("exp-a","b") 与 ("exp-ab","")
// 会撞进同一桶。
//
// RunID 为空时 **fail-closed 不注入**: 没有 run 标识就没有可复现的抽样键, 那一刻
// "确定性"这个前提本身不成立。宁可这次不灰度 (行为退回改造前), 也不做一次事后
// 无法解释的注入。
//
// ---------------------------------------------------------------------------
// 上限 50% 为什么要在决策点再夹一次
// ---------------------------------------------------------------------------
//
// `ClampShadowRatio` 已在 `evo_run_experiment` 写盘时夹过。决策点仍要夹, 因为实验
// 记录是**磁盘上的 JSON**: 手改一行 `"shadow_ratio": 0.95` 就能绕过写盘那道闸。
// 护栏必须长在读的那一侧, 否则它只防住了唯一一条自己愿意走正门的路径。
//
// 决策点的夹紧与写盘的夹紧**语义不同**, 这是个容易踩的坑:
//   - 写盘: `ratio<=0` ⇒ 补默认 0.5 ("未指定就 50/50, 配对实验最省样本");
//   - 决策: `ratio<=0` ⇒ **0, 一条都不注入**。
// 若决策点也复用写盘那套, 一个显式写着 0 的实验会变成 50% 灰度 —— 那是把"关掉"
// 读成了"全开", 是本文件最不能出的错。两套语义分成 `ClampShadowRatio` 与
// `capRatio` 两个函数, 而不是一个带布尔开关的函数: 布尔参数在调用点看不出含义。

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxShadowRatio design/03 §4.7 明写的 shadow 比例上限。
//
// 上限的意义是**限爆炸半径**: 候选产物是 LLM 写的, 一次实验最多影响一半流量, 另一半
// 永远是基线 —— 既保证了对照组一定存在 (没有对照组的"灰度"只是"半量上线"), 也保证
// 候选再坏也有一半流量是好的。
const MaxShadowRatio = 0.5

// canaryBuckets 分桶数。取 10000 使 ratio 的有效精度到 0.01% ——
// 比例灰度实际只会用到 0.05/0.1/0.5 这个量级, 10000 桶远够, 且整数比较无浮点误差。
const canaryBuckets = 10000

// 臂名。落在 policy_decision Span 与日志里, 是事后归因的检索键, 别改。
const (
	ArmShadow   = "shadow"   // 本 run 注入了候选
	ArmBaseline = "baseline" // 本 run 走基线 (对照组)
)

// Experiment 一次 shadow 配对实验的登记。
//
// 从 `pkg/tool/builtin/evotools.go` 搬到本包, 因为它现在有**两个**消费方:
// 写方是 `evo_run_experiment` 工具 (pkg/tool/builtin), 读方是运行期的灰度决策
// (pkg/agent)。而 pkg/tool/builtin 传递依赖 pkg/agent (经 pkg/evolution/govern),
// 所以 pkg/agent 不能反向 import 它 —— 记录必须落在双方都能 import 的位置。
// JSON 字段与 tag 一字未改: 已存在的 experiments/*.json 必须原样可读。
type Experiment struct {
	ID          string  `json:"id"`
	Proposal    string  `json:"proposal"`
	StartedAtMS int64   `json:"started_at_ms"` // 基线切分点: 之前的奖励算基线, 之后算实验组
	SamplesWant int     `json:"samples_want"`
	ShadowRatio float64 `json:"shadow_ratio"`
	Note        string  `json:"note,omitempty"`
	// LastCheckedMS H9 限速: 上次 evo_status 的时刻, 持久化以便进程重启不重置限速。
	LastCheckedMS int64 `json:"last_checked_ms,omitempty"`
}

// ClampShadowRatio 写盘时的比例归一 (evo_run_experiment 用)。
// 返回 (生效比例, 是否因超限被压)。未指定 (<=0) 用上限 —— 配对实验 50/50 最省样本。
func ClampShadowRatio(r float64) (float64, bool) {
	if r <= 0 {
		return MaxShadowRatio, false
	}
	if r > MaxShadowRatio {
		return MaxShadowRatio, true
	}
	return r, false
}

// capRatio 决策点的比例归一: **只夹上限, 不补默认**。见文件头。
func capRatio(r float64) float64 {
	switch {
	case r <= 0:
		return 0
	case r > MaxShadowRatio:
		return MaxShadowRatio
	default:
		return r
	}
}

// ExperimentsDir 实验登记目录。
func ExperimentsDir(stateDir string) string {
	return filepath.Join(evoDir(stateDir), "experiments")
}

// SaveExperiment 写盘 (幂等覆盖)。
func SaveExperiment(stateDir string, e Experiment) error {
	dir := ExperimentsDir(stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	// filepath.Base: ID 来自 agent 入参, 拼进 Join 会被 "../.." 穿出实验目录。
	return os.WriteFile(filepath.Join(dir, filepath.Base(e.ID)+".json"), data, 0o644)
}

// LoadExperiment 按 ID 读一个实验。
func LoadExperiment(stateDir, id string) (*Experiment, error) {
	path := filepath.Join(ExperimentsDir(stateDir), filepath.Base(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("实验 %q 不存在 (先用 evo_run_experiment 启动)", id)
	}
	var e Experiment
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("实验 %q 记录损坏: %w", id, err)
	}
	return &e, nil
}

// ListExperiments 读全部实验登记, **按 ID 升序**返回。
//
// 定序不是洁癖: 下面 ResolvePromptCanary 的歧义判定与拒绝理由都要逐字可复现,
// 而 os.ReadDir 虽已排序, 但一旦将来换成并发扫目录, 无序会让"同样的磁盘状态给出
// 不同的拒绝理由"。损坏的记录**跳过而不报错**: 采集不到不该让阶段跑不起来。
func ListExperiments(stateDir string) []Experiment {
	entries, err := os.ReadDir(ExperimentsDir(stateDir))
	if err != nil {
		return nil // 目录不存在 = 从没开过实验: 一次 ReadDir 的代价就结束
	}
	dir := ExperimentsDir(stateDir)
	var out []Experiment
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		var e Experiment
		if json.Unmarshal(data, &e) != nil || strings.TrimSpace(e.ID) == "" {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// InShadowArm 判定 (实验, run) 这一组合是否落进 shadow 臂。
//
// 确定性: 同一 (expID, runID) 永远同一答案。ratio<=0 恒 false, ratio>=MaxShadowRatio
// 按 MaxShadowRatio 算 (见文件头"上限为什么在决策点再夹一次")。runID 为空恒 false。
func InShadowArm(expID, runID string, ratio float64) bool {
	r := capRatio(ratio)
	if r <= 0 || strings.TrimSpace(runID) == "" {
		return false
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(expID))
	_, _ = h.Write([]byte{0}) // 分隔: 防 ("exp-a","b") 与 ("exp-ab","") 撞桶
	_, _ = h.Write([]byte(runID))
	bucket := h.Sum64() % canaryBuckets
	return float64(bucket) < r*canaryBuckets
}

// PromptCanary 一次已解析出的 prompt 比例灰度决策。
//
// 两个臂都会返回一份 (Arm 区分), 而不是只在 shadow 臂返回非 nil: 基线臂不打点,
// 配对分析就只有实验组没有对照组 —— 那退化成"前后对照", 正是本文件要修的问题。
type PromptCanary struct {
	ExperimentID string
	ProposalID   string
	Target       string  // workflow/stage
	Ratio        float64 // 已夹到 [0, MaxShadowRatio] 的生效比例
	Arm          string  // ArmShadow | ArmBaseline
	// Body 候选 prompt 正文, **仅 Arm==ArmShadow 时非空** —— 基线臂拿不到候选正文,
	// 免得调用方"顺手"用了它。
	Body string
}

// ResolvePromptCanary 为一个 (target, runID) 解析 prompt 比例灰度。
//
//	target    形如 "<workflow>/<stage>", 与 Proposal.Target 同一命名 (见
//	          pkg/agent 的 resolveStagePrompt: prompt 草案就是按这个键产出的)
//	runID     trace 四元组的 episode id; 空 ⇒ 不灰度
//	baseline  该 stage 当前的 prompt 正文, 用于占位符守恒检查
//
// 返回 (决策, 拒绝理由)。两者都可能为 nil/空:
//   - (nil, "")     没有任何相关实验 —— 绝大多数 run 的路径, 代价是一次 ReadDir
//   - (nil, 理由)   有相关实验但被拒 (歧义/无 RunID/候选丢占位符), 理由必须被记下来:
//     "开了实验但什么都没发生"若无解释, 事后完全无从查证
//   - (决策, "")    命中, 按 Arm 决定用不用 Body
//
// 只认 Kind=prompt 且 Status=shadow 的草案:
//   - workflow 类草案是阶段序列变体, 不是 prompt 正文, 没有可替换的对象;
//   - proposed 态从未经 evo_run_experiment, 不该有任何运行期效力;
//   - active 态是已晋升产物, 它的全量生效是另一件事 (仍无消费方, 见 design/03 §4.3d),
//     不能借灰度这条路偷偷通电 —— 灰度只管 shadow 这一档。
func ResolvePromptCanary(stateDir, target, runID, baseline string) (*PromptCanary, string) {
	target = strings.TrimSpace(target)
	if target == "" || strings.TrimSpace(stateDir) == "" {
		return nil, ""
	}
	exps := ListExperiments(stateDir)
	if len(exps) == 0 {
		return nil, ""
	}

	type hit struct {
		exp  Experiment
		prop Proposal
	}
	var hits []hit
	for _, e := range exps {
		p, err := GetProposal(stateDir, e.Proposal)
		if err != nil || p == nil {
			continue
		}
		if p.Kind != ProposalPrompt || p.Status != "shadow" || p.Target != target {
			continue
		}
		if strings.TrimSpace(p.Body) == "" {
			continue // 没有正文可注入
		}
		hits = append(hits, hit{exp: e, prop: *p})
	}
	if len(hits) == 0 {
		return nil, ""
	}
	// **歧义即放弃** (与 resolveStagePrompt 同一条纪律): 两个实验同时灰度同一个
	// stage, 谁赢都是任意的, 而两份候选混在一起的 uplift 归因不到任何一份上。
	if len(hits) > 1 {
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.exp.ID)
		}
		return nil, fmt.Sprintf("%d 个实验同时灰度 %s (%s): 歧义, 全部不注入 —— 混在一起的证据归因不到任何一份候选上",
			len(hits), target, strings.Join(ids, ", "))
	}
	h := hits[0]
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Sprintf("实验 %s 灰度 %s, 但本次执行没有 RunID: 抽样不可复现, 拒绝注入", h.exp.ID, target)
	}
	// 占位符守恒: 候选丢了 {objective} 之类的锚点就是**上线即静默失效** —— 不报错,
	// 只是产出变差。EvolvePrompt 在产出时查过, 但 ProposePrompt (agent 手工提交)
	// 查不了 (它没有原文可比), 所以这道检查必须在注入点再做一次。
	if missing := missingPlaceholders(baseline, h.prop.Body); len(missing) > 0 {
		return nil, fmt.Sprintf("实验 %s 的候选 %s 丢失占位符 %v, 拒绝注入 (上线即静默失效)",
			h.exp.ID, h.prop.ID, missing)
	}

	c := &PromptCanary{
		ExperimentID: h.exp.ID,
		ProposalID:   h.prop.ID,
		Target:       target,
		Ratio:        capRatio(h.exp.ShadowRatio),
		Arm:          ArmBaseline,
	}
	if InShadowArm(h.exp.ID, runID, h.exp.ShadowRatio) {
		c.Arm = ArmShadow
		c.Body = h.prop.Body
	}
	return c, ""
}
