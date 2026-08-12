// gepa.go —— 完整 GEPA prompt 进化 (手册 13.3.3 L5, 2026-08-12 第二批)。
//
// 与既有 GEPA-lite (workflow.go EvolvePrompt) 的差距与补齐:
//
//	lite: 一轮只改最差节点、只看加权奖励均值、失败证据 <=5 条、无正例参照、无候选间比较
//	完整: ① Pareto 前沿候选池 —— 候选按 per-task 分数向量比较, 非支配集入前沿,
//	      变异父代从前沿轮值 (GEPA 核心: 在任一任务上最强的候选都有基因价值,
//	      而不是只保留均值最优); ② 批量反思 (失败证据 <=8 条);
//	      ③ T3 差距参照 —— 同类任务的成功轨迹 (Claude 教师/历史成功 run) 作为正例
//	      一并喂给反思器, 修订从"别犯错"升级为"照这个做" (arXiv:2507.19457 的
//	      reflective mutation 依赖丰富轨迹信号)。
//
// 守卫全部继承 lite 版三硬约束 (负证据/长度上限/占位符守恒/非回显/谱系 Parent),
// 产物仍是 proposed 态草案 —— 晋升必须过 replay 门禁 (13.3.6 闸门纪律不变)。
package learners

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/replay"
)

// ============================================================================
// Pareto 候选池
// ============================================================================

// ScoreVector 一个候选 prompt 在任务集上的分数向量 (池的一行)。
type ScoreVector struct {
	ProposalID string             `json:"proposal_id"`
	Target     string             `json:"target"`
	TaskScores map[string]float64 `json:"task_scores"` // task fingerprint 或 ID → [0,1]
	Mean       float64            `json:"mean"`
	Ran        int                `json:"ran"`
	At         string             `json:"at"`
}

// Dominates Pareto 支配: a 在所有共同任务上不差于 b 且至少一个严格更好。
// 只在共同任务集上比较 (缺任务的分数不参与, 避免缺考被当成零分)。
func Dominates(a, b map[string]float64) bool {
	better, worse := false, false
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			continue
		}
		if av < bv {
			worse = true
		} else if av > bv {
			better = true
		}
	}
	// b 独有的任务不构成 a 的劣势 (反之亦然), 缺考不算差。
	return better && !worse
}

// Frontier 求非支配集 (Pareto 前沿)。同分向量去重 (保留先出现的)。
func Frontier(vecs []ScoreVector) []ScoreVector {
	var out []ScoreVector
	for i, v := range vecs {
		dominated := false
		for j, u := range vecs {
			if i == j {
				continue
			}
			if Dominates(u.TaskScores, v.TaskScores) {
				dominated = true
				break
			}
		}
		if !dominated {
			out = append(out, v)
		}
	}
	// 确定性排序: 均值降序, 同分按 ProposalID (前沿展示与轮值都依赖稳定顺序)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mean != out[j].Mean {
			return out[i].Mean > out[j].Mean
		}
		return out[i].ProposalID < out[j].ProposalID
	})
	return out
}

// SelectForMutation 从池里挑变异父代: 目标 target 的前沿候选中轮值
// (round-robin 按已记录条数取模, 无状态且确定性)。池空返回空串。
//
// 为什么从前沿轮值而不是取均值最优: GEPA 的关键性质是"在任何一个任务上最强
// 的候选都保留基因"——均值最优策略会把偏科候选饿死, 多样性塌缩后反思只能
// 在同一方向上反复抛光 (GEPA 论文对 MIPRO 的优势来源之一)。
func SelectForMutation(pool []ScoreVector, target string, round int) string {
	var front []ScoreVector
	for _, v := range pool {
		if v.Target == target {
			front = append(front, v)
		}
	}
	front = Frontier(front)
	if len(front) == 0 {
		return ""
	}
	if round < 0 {
		round = 0
	}
	return front[round%len(front)].ProposalID
}

// poolPath 池文件位置。
func poolPath(stateDir string) string {
	return filepath.Join(evoDir(stateDir), "gepa_pool.jsonl")
}

// LoadPool 读池 (不存在返回空集, 不报错)。
func LoadPool(stateDir string) ([]ScoreVector, error) {
	data, err := os.ReadFile(poolPath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ScoreVector
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v ScoreVector
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("gepa 池第 %d 行解析失败: %w", i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// RecordScore 追加一条分数向量入池 (流式落盘, 中断不丢已完成记录)。
func RecordScore(stateDir string, v ScoreVector) error {
	if v.ProposalID == "" || v.Target == "" {
		return fmt.Errorf("learners: ScoreVector 缺 ProposalID/Target")
	}
	if v.At == "" {
		v.At = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(filepath.Dir(poolPath(stateDir)), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(poolPath(stateDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// LoadProposalBody 按 ID 读草案正文 (gepa-loop 选父代用)。
func LoadProposalBody(stateDir, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	entries, err := os.ReadDir(ProposalsDir(stateDir))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ProposalsDir(stateDir), e.Name()))
		if err != nil {
			continue
		}
		var p Proposal
		if json.Unmarshal(data, &p) == nil && p.ID == id {
			return p.Body, true
		}
	}
	return "", false
}

// FailuresFromReplayResults 从回放报告的失败项抽取反思证据 (gepa-loop 的自动喂入)。
// 每条: 任务 ID + 未过原因 + 产出摘录。这是"失败证据自动化"的诚实来源——
// 直接来自确定性判分的回放, 不依赖奖励流是否覆盖该 target。
func FailuresFromReplayResults(results []replay.Result, maxItems int) []string {
	var out []string
	for _, r := range results {
		if r.Passed {
			continue
		}
		excerpt := r.Output
		if len(excerpt) > 400 {
			excerpt = excerpt[:400] + "…"
		}
		out = append(out, fmt.Sprintf("任务 %s 未过: %s。产出摘录: %s", r.TaskID, r.Reason, excerpt))
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

// ============================================================================
// 批量反思 + T3 差距参照
// ============================================================================

// PromptEvolveInputV2 完整 GEPA 反思的输入 (V1 + 成功参照)。
type PromptEvolveInputV2 struct {
	PromptEvolveInput
	// SuccessRefs T3 差距参照: 同类任务的成功轨迹/教师正例 (Claude 成功段或
	// 历史高分 run 摘录)。反思器对照"失败的我们 vs 成功的参照"产出修订,
	// 比只看失败证据的修订方向性强一个档 (13.3.4 T3 通道)。
	SuccessRefs []string
}

// EvolvePromptV2 完整 GEPA 式反思改写。
//
// 与 V1 的差异: 失败证据批量上限 5→8; 新增成功参照段 (<=3 条, 单条截 1200 字符);
// 反思器指令增加"对照差距"要求。守卫与 V1 完全一致 (负证据门槛/长度上限/
// 占位符守恒/非回显/谱系), 产物写同一 proposals 目录, CreatedBy 区分来源。
func EvolvePromptV2(ctx context.Context, stateDir string, r Reflector, in PromptEvolveInputV2) (*Proposal, error) {
	if r == nil {
		return nil, fmt.Errorf("learners: 未提供 Reflector, prompt 进化跳过 (H3: judge/反思器与主模型不同源)")
	}
	if strings.TrimSpace(in.Target) == "" || strings.TrimSpace(in.Current) == "" {
		return nil, fmt.Errorf("learners: Target/Current 不能为空")
	}
	if in.MeanScore >= 0 {
		return nil, fmt.Errorf("learners: %s 的加权奖励均值 %.3f 不为负, 没有可反思的失败证据", in.Target, in.MeanScore)
	}
	if in.Samples < gepaMinNegSamples {
		return nil, fmt.Errorf("learners: %s 只有 %d 条负奖励证据, 少于 %d, 不动它的 prompt",
			in.Target, in.Samples, gepaMinNegSamples)
	}

	sys := "你是提示词工程师。依据给定的失败证据与成功参照改进一段 system/stage prompt。" +
		"要求: ①只输出改进后的完整 prompt 正文, 不要解释、不要代码块标记; " +
		"②必须保留原 prompt 的占位符与结构约定 (如 {objective}/{user_feedback} 等), 一个都不能丢; " +
		"③改进方向是**更具体、更可检查**, 不是更长 —— 允许删掉无效的空话; " +
		"④绝不新增对工具/权限/外部系统的要求; " +
		"⑤对照成功参照与失败证据的**行为差距**修订: 把参照里被验证有效的具体做法" +
		" (步骤顺序/检查动作/收尾方式) 转化为 prompt 里的显式指令, 而不是泛泛地喊口号。"
	var b strings.Builder
	fmt.Fprintf(&b, "作用对象: %s\n当前加权奖励均值: %.3f (负奖励证据 %d 条)\n\n", in.Target, in.MeanScore, in.Samples)
	b.WriteString("=== 当前 prompt ===\n")
	b.WriteString(in.Current)
	b.WriteString("\n\n=== 失败证据 (弱模型真实轨迹摘录) ===\n")
	for i, f := range in.Failures {
		if i >= 8 { // V2 批量上限 8 (V1 是 5)
			break
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(f, 1200))
	}
	if len(in.SuccessRefs) > 0 {
		b.WriteString("\n=== 成功参照 (同类任务上被验证有效的做法, T3 差距参照) ===\n")
		for i, r := range in.SuccessRefs {
			if i >= 3 {
				break // 参照超过 3 条只增 token 不增信息量
			}
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(r, 1200))
		}
	}
	b.WriteString("\n请输出改进后的完整 prompt。")

	out, err := r.SimpleComplete(ctx, sys, b.String())
	if err != nil {
		return nil, fmt.Errorf("learners: 反思调用失败: %w", err)
	}
	cand := strings.TrimSpace(stripFence(out))
	if cand == "" {
		return nil, fmt.Errorf("learners: 反思产出为空, 不写盘")
	}
	if cand == strings.TrimSpace(in.Current) {
		return nil, fmt.Errorf("learners: 反思产出与原文一致, 无改动可提")
	}
	if len(cand) > gepaMaxPromptChars {
		return nil, fmt.Errorf("learners: 候选 prompt %d 字符超过上限 %d (反思式改写的典型劣化: 越改越长), 拒绝",
			len(cand), gepaMaxPromptChars)
	}
	if missing := missingPlaceholders(in.Current, cand); len(missing) > 0 {
		return nil, fmt.Errorf("learners: 候选 prompt 丢失占位符 %v, 拒绝 (上线即静默失效)", missing)
	}

	dir := evoDir(stateDir)
	parent := latestPromptProposal(dir, in.Target)
	p := Proposal{
		ID:        "pr-" + shortHash(in.Target+"\x00"+cand),
		Kind:      ProposalPrompt,
		Status:    "proposed",
		Target:    in.Target,
		Body:      cand,
		Samples:   in.Samples,
		MeanScore: in.MeanScore,
		Parent:    parent,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: "learners.EvolvePromptV2",
		Reason: fmt.Sprintf("节点 %s 加权奖励均值 %.3f (负证据 %d 条, 成功参照 %d 条), GEPA 完整版反思候选",
			in.Target, in.MeanScore, in.Samples, len(in.SuccessRefs)),
	}
	if _, err := writeProposal(dir, p); err != nil {
		return nil, err
	}
	return &p, nil
}
