// Package learners 学习器族里此前完全缺席的两个 (design/03 §4.3 d/e)。
//
//	d. 工作流/Prompt 进化器 (AWM 归纳 + GEPA 反思式改写)  → workflow.go
//	e. 权重进化导出器 (SFT/DPO 语料, 默认关)              → export.go
//
// 前三个学习器 (经验/记忆/技能) 已在 pkg/agent 与 pkg/evolution/skillaudit 里,
// 本包只补最后两个, 不搬动既有的。
//
// # 为什么不 import pkg/agent
//
// 本包被 pkg/agent 的 EvolutionLoop 调用 (那是设计要求的"一个循环"挂载点), 所以
// 方向只能是 pkg/agent → learners。于是本包对既有数据的读取只能走**磁盘格式契约**:
//
//	<state>/evolution/rewards.jsonl      RewardEvent 流水 (§4.2)
//	<state>/evolution/trajectories.json  Trajectory 数组 (团队每阶段的 input/output)
//	<state>/statestore/log/trace-*.jsonl TraceStore Span (§4.1)
//
// 这三个格式在 design/03 §七里就是被当作接口写的 ("格式即契约"), 重声明最小字段
// 比引一个会成环的依赖更诚实。字段名漂移的风险由 learners_test.go 里的一致性测试
// 兜住 (外部测试包可以 import pkg/agent, 不成环)。
package learners

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RewardRow rewards.jsonl 的一行 (字段名与 pkg/agent.RewardEvent 逐字一致)。
type RewardRow struct {
	TS     int64   `json:"ts"`
	RunID  string  `json:"run_id,omitempty"`
	NodeID string  `json:"node_id,omitempty"`
	Source string  `json:"source"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight,omitempty"`
	Team   string  `json:"team,omitempty"`
}

// TrajectoryRow trajectories.json 的一项 (字段名与 pkg/agent.Trajectory 一致)。
type TrajectoryRow struct {
	ID        string    `json:"id"`
	RunID     string    `json:"runId,omitempty"`
	TeamName  string    `json:"teamName"`
	StageName string    `json:"stageName"`
	Role      string    `json:"role"`
	Objective string    `json:"objective"`
	Input     string    `json:"input"`
	Output    string    `json:"output"`
	Error     string    `json:"error,omitempty"`
	Success   bool      `json:"success"`
	Duration  string    `json:"duration"`
	Timestamp time.Time `json:"timestamp"`
}

// fallbackWeights 老数据 (Weight 未落盘) 的兜底可信度表。
//
// 这是 pkg/agent.RewardSourceWeight 定值的**副本**, 由 learners_test.go 的
// TestFallbackWeightsMatchAgent 锁定一致性 —— 副本本身是坏味道, 但它换来的是本包
// 不必反向依赖 pkg/agent (那会成环)。测试失败就说明两边漂移了。
var fallbackWeights = map[string]float64{
	"gate.compile":  1.0,
	"gate.test":     1.0,
	"gate.lint":     1.0,
	"gate.e2e":      1.0,
	"user.explicit": 0.8,
	"user.steer":    0.8,
	"user.feedback": 0.8,
	"gate.content":  0.5,
	"review.panel":  0.5,
	"gate.review":   0.5,
	"llm.judge":     0.5,
	"episode":       0.3,
	"latency":       0.2,
	"cost":          0.2,
}

const fallbackWeightUnknown = 0.5

func weightOf(r RewardRow) float64 {
	if r.Weight > 0 {
		return r.Weight
	}
	if w, ok := fallbackWeights[strings.ToLower(strings.TrimSpace(r.Source))]; ok {
		return w
	}
	return fallbackWeightUnknown
}

// LoadRewards 读 rewards.jsonl 全量 (只在离线学习路径调用, 不在交付热路径)。
// 单行损坏跳过 —— 一条坏行不该让整轮学习空转。
func LoadRewards(path string) []RewardRow {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []RewardRow
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r RewardRow
		if json.Unmarshal([]byte(line), &r) == nil && r.RunID != "" {
			out = append(out, r)
		}
	}
	return out
}

// LoadTrajectories 读 trajectories.json (数组)。
func LoadTrajectories(path string) []TrajectoryRow {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []TrajectoryRow
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// RunScore 一个 run 的加权奖励分与证据量。
type RunScore struct {
	Score float64
	Count int
	// NodeScores 各节点的加权分 (仅含带 node_id 的奖励), 供 prompt 进化定位差节点。
	NodeScores map[string]float64
	NodeCounts map[string]int
}

// ScoreRuns 把奖励流水折算成每个 run 的加权分。
//
// 聚合规则与 pkg/agent.AggregateRewards **刻意保持一致**, 否则同一份 rewards.jsonl
// 在"阶段反馈"与"结构进化"两处会得出不同结论:
//  1. 同 (source, node) 只取最新一条 —— 门禁"失败→修复→通过"取均值会被旧失败拖回去;
//  2. 权重优先用事件自带的 Weight;
//  3. 加权平均而非求和 —— 奖励源接得越多不该自动让分数越高。
func ScoreRuns(rows []RewardRow) map[string]RunScore {
	// 先按 run 分桶并按 TS 升序, 才能"最新一条取代旧的"。
	byRun := map[string][]RewardRow{}
	for _, r := range rows {
		byRun[r.RunID] = append(byRun[r.RunID], r)
	}
	out := make(map[string]RunScore, len(byRun))
	for run, rs := range byRun {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].TS < rs[j].TS })
		latest := map[string]RewardRow{}
		order := []string{}
		for _, r := range rs {
			key := r.Source + "\x00" + r.NodeID
			if _, seen := latest[key]; !seen {
				order = append(order, key)
			}
			latest[key] = r
		}
		sc := RunScore{NodeScores: map[string]float64{}, NodeCounts: map[string]int{}}
		var weighted, wsum float64
		nodeW := map[string]float64{}
		for _, key := range order {
			r := latest[key]
			w := weightOf(r)
			v := clamp(r.Value)
			weighted += w * v
			wsum += w
			sc.Count++
			if r.NodeID != "" {
				sc.NodeScores[r.NodeID] += w * v
				nodeW[r.NodeID] += w
				sc.NodeCounts[r.NodeID]++
			}
		}
		if wsum > 0 {
			sc.Score = clamp(weighted / wsum)
		}
		for node, w := range nodeW {
			if w > 0 {
				sc.NodeScores[node] = clamp(sc.NodeScores[node] / w)
			}
		}
		out[run] = sc
	}
	return out
}

func clamp(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// SplitByTime 按时刻把奖励流水切成"基线组 / 实验组" (配对对照实验的数据切分)。
//
// 用时刻切分而不是"打标记": shadow 实验开始时既有的奖励已经落盘, 没法回头给它们贴
// 标签; 而实验开始时刻是确定的, 前后两段天然构成同一系统的前后对照。局限也要说清 ——
// 这是**前后对照**而不是随机对照, 期间若有别的改动 (换模型/改工作流) 会混入。真正的
// 随机配对需要运行期按 shadow_ratio 分流并打点, 属 E4 深化。
func SplitByTime(rows []RewardRow, cutMS int64) (before, after []RewardRow) {
	for _, r := range rows {
		if r.TS < cutMS {
			before = append(before, r)
		} else {
			after = append(after, r)
		}
	}
	return before, after
}

// MeanRunScore 一组奖励的 run 级加权均分与 run 数。
//
// 先按 run 聚合再对 run 取平均 (而不是直接对事件取平均): 否则奖励源接得多的 run 会
// 在均值里占更大权重, 变成"谁的奖励事件多谁说话响"。
func MeanRunScore(rows []RewardRow) (float64, int) {
	scores := ScoreRuns(rows)
	if len(scores) == 0 {
		return 0, 0
	}
	var sum float64
	for _, sc := range scores {
		sum += sc.Score
	}
	return sum / float64(len(scores)), len(scores)
}

// RunShape 一个 run 的"形状": 阶段序列 + 目标 + 成败。AWM 归纳的输入单位。
type RunShape struct {
	RunID     string
	Team      string
	Objective string
	Nodes     []string // 阶段名, 按时间顺序
	Roles     []string
	AllOK     bool
}

// ShapeRuns 把逐阶段的 trajectory 折成每个 run 的形状。
//
// 无 RunID 的老轨迹被丢弃而不是按 team 归并: 同一个 team 会跑多次 (refine/重跑),
// 按 team 归并会把三次运行的阶段拼成一条不存在的 12 阶段序列, 归纳出的"模板"是假的。
func ShapeRuns(rows []TrajectoryRow) []RunShape {
	byRun := map[string][]TrajectoryRow{}
	for _, r := range rows {
		if strings.TrimSpace(r.RunID) == "" {
			continue
		}
		byRun[r.RunID] = append(byRun[r.RunID], r)
	}
	out := make([]RunShape, 0, len(byRun))
	for run, rs := range byRun {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].Timestamp.Before(rs[j].Timestamp) })
		sh := RunShape{RunID: run, Team: rs[0].TeamName, Objective: rs[0].Objective, AllOK: true}
		for _, r := range rs {
			sh.Nodes = append(sh.Nodes, r.StageName)
			sh.Roles = append(sh.Roles, r.Role)
			if !r.Success {
				sh.AllOK = false
			}
		}
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}

// ObjectiveSignature 目标的类别签名 (AWM 的"这类 objective")。
//
// 确定性实现: 取目标里的词, 去停用词, 取字典序前 4 个作签名。
//
// 为什么不用 embedding / LLM 分类: 归纳是每次空闲期都要跑的批处理, 用 LLM 就等于
// 给"整理"这件事装上按次计费; 而且签名必须**跨进程稳定** —— 同一目标两次得出不同
// 签名会让样本量永远攒不够, 归纳永远不触发。
//
// 精度边界说清楚, 免得被当成语义聚类:
//   - 字典序让 ASCII 词的**出现顺序**不影响签名 ("fix auth bug" == "auth bug fix");
//   - 中文走二元切分, 重排语序会改变二元组集合, 因此**不保证**语序无关。这是可接受的:
//     归纳只需要"同一批反复出现的同类任务"落进同一组, 而同类任务的措辞通常是复用的
//     (工作流的 objective 多由模板或同一个人写出)。分错组的后果只是样本量攒得慢, 不会
//     产生错误的模板 —— 序列本身仍是那些 run 真跑出来的。
func ObjectiveSignature(objective string) string {
	toks := tokenize(objective)
	if len(toks) == 0 {
		return "misc"
	}
	sort.Strings(toks)
	uniq := toks[:0]
	var prev string
	for _, t := range toks {
		if t != prev {
			uniq = append(uniq, t)
			prev = t
		}
	}
	if len(uniq) > 4 {
		uniq = uniq[:4]
	}
	return strings.Join(uniq, "-")
}

// stopWords 高频无区分度词 (中英各一批)。留在签名里会让所有目标签名相同。
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "to": true, "of": true,
	"for": true, "with": true, "in": true, "on": true, "please": true, "help": true,
	"一个": true, "这个": true, "帮我": true, "请": true, "需要": true, "实现": true,
	"以及": true, "然后": true, "使用": true, "我们": true, "可以": true,
}

// tokenize 极简分词: ASCII 按非字母数字切, CJK 按二元切 (中文无空格, 单字太碎)。
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var ascii strings.Builder
	var cjk []rune
	flushASCII := func() {
		if ascii.Len() >= 3 {
			w := ascii.String()
			if !stopWords[w] {
				out = append(out, w)
			}
		}
		ascii.Reset()
	}
	flushCJK := func() {
		for i := 0; i+1 < len(cjk); i++ {
			w := string(cjk[i : i+2])
			if !stopWords[w] {
				out = append(out, w)
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			flushCJK()
			ascii.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff:
			flushASCII()
			cjk = append(cjk, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return out
}

// evoDir 定位 <state>/evolution。
func evoDir(stateDir string) string { return filepath.Join(stateDir, "evolution") }

// shortHash 内容指纹 (草案 ID 用)。
//
// 用内容而非序号/时间戳: 草案 ID 必须是内容的函数, 否则空闲期每 15 分钟跑一次归纳
// 就会为同一份结论生成一个新文件, 一夜之间 proposals/ 里全是重复品。
func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}
