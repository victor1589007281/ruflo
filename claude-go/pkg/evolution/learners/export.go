// export.go —— 学习器 e: 权重进化导出器 (design/03 §4.3e, 默认关)。
//
// # 范围与不做的事
//
// 设计里 e 有两条管线, 本文件**只建 SFT/DPO 语料管线**:
//
//	SFT/DPO messages 视图  → 本文件实现。允许有损处理 (H5)。
//	RL token 对齐管线      → 不建。claude-go 经网关调外部 API 拿不到 logprob,
//	                        建了就是空壳。设计本身也写明"当前默认不建, 仅预留契约"。
//
// # H5 双管线严格分离怎么落地
//
// 有损压缩只允许出现在 SFT 侧, 而且**不用 LLM 摘要** —— 设计明确说 LLM 摘要对 RL
// 管线是污染源。这里用确定性中段截断: 保护首尾各 protectTurns 轮, 中段替换为一行
// 显式标记, 并在 system 里注明"部分历史已省略"。确定性的好处是同一条轨迹导出两次
// 字节完全一致, 数据集可复现。
//
// # H13 质量过滤
//
// 无推理覆盖的轨迹直接丢 (hermes 的做法)。对 claude-go 的具体判据见 filterReason:
// 空产出 / 无奖励证据 / 奖励低于阈值 / schema 回显 / 产出过短。每条丢弃都记原因,
// 因为"导出了 3 条"这种结果最需要解释的是"另外 97 条为什么没进去"。
package learners

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 锁定的导出判据。
const (
	// exportMinScore SFT 样本的奖励下限。0.3 与 awmMinScore / skillaudit 对齐:
	// 同一套奖励口径下"值得学"的分数线只该有一个。
	exportMinScore = 0.3
	// exportMinOutputChars 产出短于此视为无信息量 (H13 的"无推理覆盖"在本仓的对应物:
	// 我们拿不到 reasoning token, 但一句话产出必然没有推理过程)。
	exportMinOutputChars = 200
	// protectTurns 有损压缩时首尾各保护的轮数 (H5)。
	protectTurns = 2
	// dpoMinGap DPO 配对的最小奖励差。差太小的一对是噪声而非偏好。
	dpoMinGap = 0.4
)

// ExportConfig 导出参数。
type ExportConfig struct {
	// OutDir 导出目录; 空则用 <state>/evolution/export。
	OutDir string
	// MaxTurnsPerSample 单条样本最多保留多少轮; <=0 不压缩。
	MaxTurnsPerSample int
	// IncludeDPO 同时产出 DPO 偏好对 (需要同组内有奖励差)。
	IncludeDPO bool
}

// SFTMessage ShareGPT 风格的一轮。
type SFTMessage struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// SFTSample 一条 SFT 样本。
type SFTSample struct {
	RunID     string       `json:"run_id"`
	Team      string       `json:"team,omitempty"`
	Objective string       `json:"objective,omitempty"`
	Signature string       `json:"signature,omitempty"`
	Score     float64      `json:"score"`
	Messages  []SFTMessage `json:"messages"`
	Truncated bool         `json:"truncated,omitempty"`
}

// DPOPair 一条偏好对 (同组、同任务类别, 只有奖励不同)。
type DPOPair struct {
	Signature string  `json:"signature"`
	Prompt    string  `json:"prompt"`
	Chosen    string  `json:"chosen"`
	Rejected  string  `json:"rejected"`
	GapScore  float64 `json:"gap_score"`
	ChosenRun string  `json:"chosen_run"`
	RejectRun string  `json:"reject_run"`
}

// ExportResult 一次导出的账。
type ExportResult struct {
	SFTPath  string         `json:"sft_path,omitempty"`
	DPOPath  string         `json:"dpo_path,omitempty"`
	Samples  int            `json:"samples"`
	Pairs    int            `json:"pairs"`
	Filtered map[string]int `json:"filtered"` // 丢弃原因 → 条数
}

// ExportSFT 从真实轨迹 + 奖励导出 SFT (可选 DPO) 语料。
//
// 数据源全部是生产落盘的真实文件, 不合成任何样本。若 trajectories.json 为空,
// 结果就是 0 条 —— 那是诚实的 0, 不是失败。
func ExportSFT(stateDir string, cfg ExportConfig) (*ExportResult, error) {
	dir := evoDir(stateDir)
	trajs := LoadTrajectories(filepath.Join(dir, "trajectories.json"))
	scores := ScoreRuns(LoadRewards(filepath.Join(dir, "rewards.jsonl")))

	out := cfg.OutDir
	if out == "" {
		out = filepath.Join(dir, "export")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	res := &ExportResult{Filtered: map[string]int{}}

	// 按 run 分组: 一个 run = 一条多轮样本 (阶段序列就是对话序列)。
	byRun := map[string][]TrajectoryRow{}
	for _, t := range trajs {
		if strings.TrimSpace(t.RunID) == "" {
			res.Filtered["无 RunID (老格式轨迹, 无法与奖励对齐)"]++
			continue
		}
		byRun[t.RunID] = append(byRun[t.RunID], t)
	}
	runIDs := make([]string, 0, len(byRun))
	for id := range byRun {
		runIDs = append(runIDs, id)
	}
	sort.Strings(runIDs) // 导出顺序确定 = 数据集可复现

	var samples []SFTSample
	for _, runID := range runIDs {
		rows := byRun[runID]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Timestamp.Before(rows[j].Timestamp) })
		sc, hasScore := scores[runID]
		if !hasScore || sc.Count == 0 {
			res.Filtered["无奖励证据 (H13: 不知好坏的样本不进数据集)"]++
			continue
		}
		if sc.Score < exportMinScore {
			res.Filtered[fmt.Sprintf("奖励低于阈值 %.1f", exportMinScore)]++
			continue
		}
		s, reason := buildSample(runID, rows, sc.Score, cfg.MaxTurnsPerSample)
		if reason != "" {
			res.Filtered[reason]++
			continue
		}
		samples = append(samples, *s)
	}

	res.Samples = len(samples)
	if len(samples) > 0 {
		p := filepath.Join(out, "sft.jsonl")
		if err := writeJSONL(p, samples); err != nil {
			return nil, err
		}
		res.SFTPath = p
	}

	if cfg.IncludeDPO {
		pairs := buildDPOPairs(byRun, scores)
		res.Pairs = len(pairs)
		if len(pairs) > 0 {
			p := filepath.Join(out, "dpo.jsonl")
			if err := writeJSONL(p, pairs); err != nil {
				return nil, err
			}
			res.DPOPath = p
		}
	}
	return res, nil
}

// buildSample 把一个 run 的阶段序列折成多轮对话。第二返回值非空 = 被过滤及原因。
func buildSample(runID string, rows []TrajectoryRow, score float64, maxTurns int) (*SFTSample, string) {
	s := &SFTSample{RunID: runID, Score: score}
	if len(rows) > 0 {
		s.Team, s.Objective = rows[0].TeamName, rows[0].Objective
		s.Signature = ObjectiveSignature(rows[0].Objective)
	}
	totalOut := 0
	turns := make([][2]SFTMessage, 0, len(rows))
	for _, r := range rows {
		in, outText := strings.TrimSpace(r.Input), strings.TrimSpace(r.Output)
		if in == "" || outText == "" {
			continue // 半条轮次无法构成 (user, assistant) 对
		}
		if looksLikeSchemaEcho(outText) {
			return nil, "含 schema 回显 (H13: 模型把 JSON 模板抄回来了, 不是产出)"
		}
		totalOut += len(outText)
		turns = append(turns, [2]SFTMessage{
			{Role: "user", Content: in},
			{Role: "assistant", Content: outText},
		})
	}
	if len(turns) == 0 {
		return nil, "无完整轮次 (input/output 有一侧为空)"
	}
	if totalOut < exportMinOutputChars {
		return nil, fmt.Sprintf("产出总长 <%d 字符 (H13: 无推理过程)", exportMinOutputChars)
	}

	sysNote := fmt.Sprintf("以下是一次多阶段协作的真实轨迹 (目标类别: %s)。", s.Signature)
	// 有损压缩 (H5, 仅 SFT 侧): 保护首尾各 protectTurns 轮, 中段用显式标记替换。
	if maxTurns > 0 && len(turns) > maxTurns && maxTurns > 2*protectTurns {
		dropped := len(turns) - 2*protectTurns
		head := turns[:protectTurns]
		tail := turns[len(turns)-protectTurns:]
		s.Truncated = true
		sysNote += fmt.Sprintf(" 注意: 中间 %d 轮历史已省略 (确定性截断, 非摘要)。", dropped)
		s.Messages = append(s.Messages, SFTMessage{Role: "system", Content: sysNote})
		for _, t := range head {
			s.Messages = append(s.Messages, t[0], t[1])
		}
		s.Messages = append(s.Messages, SFTMessage{
			Role:    "user",
			Content: fmt.Sprintf("[省略 %d 轮中间历史]", dropped),
		})
		for _, t := range tail {
			s.Messages = append(s.Messages, t[0], t[1])
		}
		return s, ""
	}
	s.Messages = append(s.Messages, SFTMessage{Role: "system", Content: sysNote})
	for _, t := range turns {
		s.Messages = append(s.Messages, t[0], t[1])
	}
	return s, ""
}

// buildDPOPairs 组语义 (H10): 同 (objective 签名, 阶段名) 才配对。
//
// 为什么必须同签名同阶段: DPO 的前提是"同一个提示下两个不同回答"。跨任务配对得到的
// "偏好"实际上是任务难度差异, 学出来的是噪声。设计 §4.3e 的组卫生说的就是这件事。
//
// 组内奖励全同的组丢弃 (无学习信号) —— 这也是 H10 明写的。
func buildDPOPairs(byRun map[string][]TrajectoryRow, scores map[string]RunScore) []DPOPair {
	type item struct {
		runID  string
		input  string
		output string
		score  float64
	}
	groups := map[string][]item{}
	for runID, rows := range byRun {
		sc, ok := scores[runID]
		if !ok || sc.Count == 0 {
			continue
		}
		for _, r := range rows {
			in, outText := strings.TrimSpace(r.Input), strings.TrimSpace(r.Output)
			if in == "" || outText == "" || looksLikeSchemaEcho(outText) {
				continue
			}
			key := ObjectiveSignature(r.Objective) + "|" + r.StageName
			groups[key] = append(groups[key], item{runID: runID, input: in, output: outText, score: sc.Score})
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []DPOPair
	for _, k := range keys {
		its := groups[k]
		if len(its) < 2 {
			continue
		}
		sort.Slice(its, func(i, j int) bool {
			if its[i].score != its[j].score {
				return its[i].score > its[j].score
			}
			return its[i].runID < its[j].runID
		})
		best, worst := its[0], its[len(its)-1]
		gap := best.score - worst.score
		if gap < dpoMinGap {
			continue // 组内奖励几乎相同 = 无偏好信号, 丢弃 (H10)
		}
		out = append(out, DPOPair{
			Signature: k,
			Prompt:    best.input,
			Chosen:    best.output,
			Rejected:  worst.output,
			GapScore:  gap,
			ChosenRun: best.runID,
			RejectRun: worst.runID,
		})
	}
	return out
}

// looksLikeSchemaEcho 判定产出是不是把 JSON 模板抄回来了 (本仓真实事故类型:
// "jsonExtract 挑信息量最大防 schema 回显")。
//
// 判据: 含典型的占位类型名且几乎没有实际内容。刻意保守 —— 宁可漏过几条, 不可把
// 正常的 JSON 产出误判成回显丢掉。
func looksLikeSchemaEcho(s string) bool {
	low := strings.ToLower(s)
	markers := []string{`"string"`, `"number"`, `"boolean"`, `0到100的整数`, `<整数>`, `"..."`}
	hits := 0
	for _, m := range markers {
		if strings.Contains(low, m) {
			hits++
		}
	}
	return hits >= 2
}

func writeJSONL[T any](path string, items []T) error {
	var b strings.Builder
	for _, it := range items {
		data, err := json.Marshal(it)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
