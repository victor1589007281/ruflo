// workflow.go —— 学习器 d: 工作流/Prompt 进化器 (design/03 §4.3d, AFlow/AWM/GEPA)。
//
// # 两条支线，一个共同约束
//
//	AWM 工作流归纳: 从真实 run 的阶段序列 + 加权奖励里归纳"这类 objective 用这个
//	                节点序列成功率高", 产出 GraphSpec 变体草案。**纯确定性**, 零 LLM。
//	GEPA prompt 进化: 取某节点的失败轨迹 + 该节点的负奖励, 让 LLM 反思产出候选 prompt。
//	                **要 LLM**, 所以低频 (每轮空闲最多一个候选) 且需显式开关。
//
// 共同约束 = **只产出 proposed 态草案, 绝不改运行期行为**。设计 §4.6 的"必过闸"要求
// 任何进化产物晋升前必过离线回放 + uplift; 本文件写盘的东西没经过闸, 所以它落在
// `<state>/evolution/proposals/`, 谁都不会自动读它。晋升由 §4.7 的 evo_promote 操作,
// 那里才会做不越权检查与回放门禁。
//
// # 为什么归纳不用 LLM
//
// 归纳的输入是"哪些阶段序列的奖励高", 这是**统计**而不是理解。让 LLM 来做等于给每次
// 空闲整理装上按次计费, 而且它会编造没出现过的节点名 (本仓已有"AI 虚构 Go 语法"的
// 前例)。LLM 只用在它真正不可替代的地方: 改写一段自然语言 prompt。
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
)

// 锁定的归纳判据 (design/03 §4.6 四律: 判据不在 agent 的动作空间里)。
//
// 不可导出、不可配置 —— 与 skillaudit 的 minSamples/promoteThreshold 同一处理。
const (
	// awmMinSamples 一个 (签名, 序列) 组合至少要有这么多次真实运行才值得归纳。
	// 3 次是"不是巧合"的最低门槛; 再低会把一次偶然成功当成模板。
	awmMinSamples = 3
	// awmMinScore 组内加权奖励均值下限。0.3 与 skillaudit.promoteThreshold 对齐:
	// 同一套奖励口径下, 值得固化成技能的分数线也该是值得固化成模板的分数线。
	awmMinScore = 0.3
	// awmMaxProposals 单轮最多产出多少草案 —— 防一次空闲期刷出上百个文件。
	awmMaxProposals = 5
	// gepaMinNegSamples 某节点至少要有这么多次负奖励才值得动它的 prompt。
	gepaMinNegSamples = 2
	// gepaMaxPromptChars 候选 prompt 长度上限: 反思式改写最常见的失败模式是越改越长
	// (LLM 倾向于"再加一条规则"), 长到爆上下文就成了系统性劣化。
	gepaMaxPromptChars = 6000
)

// ProposalKind 草案类型。
const (
	ProposalWorkflow = "workflow" // 图模板/阶段序列变体
	ProposalPrompt   = "prompt"   // prompt 版本
)

// Proposal 一份进化草案 (proposed 态, 未过闸)。
//
// Lineage 是设计 §4.6"必留痕"的载体: 每份草案都指得出它的父版本与产生它的证据,
// 于是"一键回滚到谱系任意点"才有对象可回滚。
type Proposal struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Status    string   `json:"status"` // 恒为 "proposed" —— 本文件不产 active
	Signature string   `json:"signature,omitempty"`
	Nodes     []string `json:"nodes,omitempty"`
	Roles     []string `json:"roles,omitempty"`
	// Target prompt 类草案的作用对象 (workflow/stage 或 role 名)。
	Target string `json:"target,omitempty"`
	// Body prompt 正文 (prompt 类草案)。
	Body string `json:"body,omitempty"`
	// Evidence 归纳依据: 样本量 / 奖励均值 / 参与的 run。
	Samples   int      `json:"samples"`
	MeanScore float64  `json:"mean_score"`
	RunIDs    []string `json:"run_ids,omitempty"`
	// Lineage 谱系: 父版本 ID (空 = 首版) + 产出时刻 + 产出者。
	Parent    string `json:"parent,omitempty"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
	Reason    string `json:"reason,omitempty"`
}

// InduceResult 一轮归纳的汇总。
type InduceResult struct {
	Shapes    int        `json:"shapes"`    // 参与归纳的 run 数
	Groups    int        `json:"groups"`    // (签名, 序列) 组合数
	Proposals []Proposal `json:"proposals"` // 新写盘的草案 (已存在的同内容草案不重复写)
	Skipped   []string   `json:"skipped,omitempty"`
}

// InduceWorkflows AWM 工作流归纳 (design/03 §4.3d 第 1 条)。
//
// 输入全部来自真实运行: trajectories.json 的阶段序列 + rewards.jsonl 的加权奖励。
// 无奖励证据的 run **不参与** —— 归纳的全部价值在于"高奖励", 拿没有奖励的 run 归纳
// 出来的只是"最常见的序列", 那是统计噪声不是学习。
func InduceWorkflows(stateDir string) (*InduceResult, error) {
	dir := evoDir(stateDir)
	shapes := ShapeRuns(LoadTrajectories(filepath.Join(dir, "trajectories.json")))
	scores := ScoreRuns(LoadRewards(filepath.Join(dir, "rewards.jsonl")))

	res := &InduceResult{Shapes: len(shapes)}
	if len(shapes) == 0 {
		res.Skipped = append(res.Skipped, "无带 RunID 的团队轨迹, 无法归纳 (trajectories.json 为空或全是老格式)")
		return res, nil
	}

	type group struct {
		sig   string
		nodes []string
		roles []string
		runs  []string
		sum   float64
		n     int
		allOK int
	}
	groups := map[string]*group{}
	scored := 0
	for _, sh := range shapes {
		if len(sh.Nodes) == 0 {
			continue
		}
		sc, ok := scores[sh.RunID]
		if !ok || sc.Count == 0 {
			continue // 无奖励证据的 run 不参与归纳
		}
		scored++
		sig := ObjectiveSignature(sh.Objective)
		key := sig + "|" + strings.Join(sh.Nodes, ">")
		g := groups[key]
		if g == nil {
			g = &group{sig: sig, nodes: sh.Nodes, roles: sh.Roles}
			groups[key] = g
		}
		g.runs = append(g.runs, sh.RunID)
		g.sum += sc.Score
		g.n++
		if sh.AllOK {
			g.allOK++
		}
	}
	res.Groups = len(groups)
	if scored == 0 {
		res.Skipped = append(res.Skipped, "所有 run 都无奖励证据, 归纳跳过 (先接奖励源, 见 §4.2)")
		return res, nil
	}

	// 候选按 (均分, 样本量) 降序, 保证同样的输入产出同样的草案顺序。
	type cand struct {
		key  string
		g    *group
		mean float64
	}
	var cands []cand
	for k, g := range groups {
		if g.n < awmMinSamples {
			continue
		}
		mean := g.sum / float64(g.n)
		if mean < awmMinScore {
			continue
		}
		cands = append(cands, cand{key: k, g: g, mean: mean})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mean != cands[j].mean {
			return cands[i].mean > cands[j].mean
		}
		if cands[i].g.n != cands[j].g.n {
			return cands[i].g.n > cands[j].g.n
		}
		return cands[i].key < cands[j].key
	})
	if len(cands) == 0 {
		res.Skipped = append(res.Skipped, fmt.Sprintf(
			"%d 个 (签名,序列) 组合均未同时满足 样本≥%d 且 均分≥%.1f", len(groups), awmMinSamples, awmMinScore))
		return res, nil
	}
	if len(cands) > awmMaxProposals {
		cands = cands[:awmMaxProposals]
	}

	for _, c := range cands {
		p := Proposal{
			ID:        "wf-" + shortHash(c.key),
			Kind:      ProposalWorkflow,
			Status:    "proposed",
			Signature: c.g.sig,
			Nodes:     c.g.nodes,
			Roles:     c.g.roles,
			Samples:   c.g.n,
			MeanScore: c.mean,
			RunIDs:    c.g.runs,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			CreatedBy: "learners.InduceWorkflows",
			Reason: fmt.Sprintf("该 objective 类别下此阶段序列 %d 次运行加权奖励均值 %.3f (全阶段通过 %d 次)",
				c.g.n, c.mean, c.g.allOK),
		}
		written, err := writeProposal(dir, p)
		if err != nil {
			res.Skipped = append(res.Skipped, "写草案失败: "+err.Error())
			continue
		}
		if written {
			res.Proposals = append(res.Proposals, p)
		}
	}
	return res, nil
}

// Reflector GEPA 反思所需的最小 LLM 能力 (与 pkg/agent.LLMClient 的 SimpleComplete 同形)。
//
// 用接口而不是具体 client: 设计 §4.2 H3 明令 judge/反思器**不得**与被评估主模型同源,
// 宿主因此必须能塞一个不同档位的实现进来。
type Reflector interface {
	SimpleComplete(ctx context.Context, system, user string) (string, error)
}

// PromptEvolveInput 一次 prompt 反思的输入。
type PromptEvolveInput struct {
	// Target 作用对象标识, 如 "development/implement" 或 "role:coder"。
	Target string
	// Current 当前 prompt 正文 (由宿主提供 —— 真源在 workflow/role 定义里, 不在本包)。
	Current string
	// Failures 该 target 下的失败证据 (轨迹片段 + 负奖励)。
	Failures []string
	// MeanScore 该 target 的加权奖励均值 (负值才该来这里)。
	MeanScore float64
	Samples   int
}

// FindWeakNodes 找出加权奖励为负、且样本量够的节点 (GEPA 的作用点定位)。
//
// 返回按分数升序 (最差在前), 调用方通常只取第一个 —— 一轮空闲期改一个 prompt,
// 同时改多个会让 uplift 无法归因到具体改动。
func FindWeakNodes(stateDir string) []NodeWeakness {
	scores := ScoreRuns(LoadRewards(filepath.Join(evoDir(stateDir), "rewards.jsonl")))
	agg := map[string]*NodeWeakness{}
	for runID, sc := range scores {
		for node, s := range sc.NodeScores {
			w := agg[node]
			if w == nil {
				w = &NodeWeakness{Node: node}
				agg[node] = w
			}
			w.sum += s
			w.Samples += sc.NodeCounts[node]
			w.Runs++
			if s < 0 {
				w.NegRuns++
				if len(w.NegRunIDs) < 10 {
					w.NegRunIDs = append(w.NegRunIDs, runID)
				}
			}
		}
	}
	var out []NodeWeakness
	for _, w := range agg {
		if w.Runs > 0 {
			w.MeanScore = w.sum / float64(w.Runs)
		}
		if w.MeanScore < 0 && w.NegRuns >= gepaMinNegSamples {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MeanScore != out[j].MeanScore {
			return out[i].MeanScore < out[j].MeanScore
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// NodeWeakness 一个节点的负奖励画像。
type NodeWeakness struct {
	Node      string   `json:"node"`
	MeanScore float64  `json:"mean_score"`
	Runs      int      `json:"runs"`
	NegRuns   int      `json:"neg_runs"`
	Samples   int      `json:"samples"`
	NegRunIDs []string `json:"neg_run_ids,omitempty"`
	sum       float64
}

// EvolvePrompt GEPA 式反思改写 (design/03 §4.3d 第 3 条)。
//
// 三条硬约束, 都是为了让"反思"不变成"随机漂移":
//  1. **必须有负奖励证据**: MeanScore ≥ 0 或样本不足直接拒绝。没有失败就没有可反思的。
//  2. **产物必须变短或差不多长**: 超过 gepaMaxPromptChars 直接拒绝。反思式改写最常见的
//     劣化模式就是不断追加规则, 直到 prompt 挤爆上下文预算。
//  3. **产物必须与原文不同且非空**: LLM 回显原文 / 回一句"好的"都算失败, 不写盘。
//
// 返回的草案是 proposed 态, 带 Parent 指向上一版 (谱系), 不改任何运行期行为。
func EvolvePrompt(ctx context.Context, stateDir string, r Reflector, in PromptEvolveInput) (*Proposal, error) {
	if r == nil {
		return nil, fmt.Errorf("learners: 未提供 Reflector, prompt 进化跳过 (设计要求 judge/反思器与主模型不同源, 见 §4.2 H3)")
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

	sys := "你是提示词工程师。依据给定的失败证据改进一段 system/stage prompt。" +
		"要求: ①只输出改进后的完整 prompt 正文, 不要解释、不要代码块标记; " +
		"②必须保留原 prompt 的占位符与结构约定 (如 {objective}/{user_feedback} 等), 一个都不能丢; " +
		"③改进方向是**更具体、更可检查**, 不是更长 —— 允许删掉无效的空话; " +
		"④绝不新增对工具/权限/外部系统的要求。"
	var b strings.Builder
	fmt.Fprintf(&b, "作用对象: %s\n当前加权奖励均值: %.3f (负奖励证据 %d 条)\n\n", in.Target, in.MeanScore, in.Samples)
	b.WriteString("=== 当前 prompt ===\n")
	b.WriteString(in.Current)
	b.WriteString("\n\n=== 失败证据 ===\n")
	for i, f := range in.Failures {
		if i >= 5 {
			break // 证据超过 5 条不再增加信息量, 只增 token
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(f, 1200))
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
	// 占位符守恒检查: 原 prompt 里的 {xxx} 一个都不能丢。
	// 这是**确定性**否决 —— 丢了占位符的 prompt 上线即静默失效 (注入管线找不到锚点),
	// 而这种失效不会报错, 只会让产出变差, 是最难查的一类回归。
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
		RunIDs:    nil,
		Parent:    parent,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: "learners.EvolvePrompt",
		Reason: fmt.Sprintf("节点 %s 加权奖励均值 %.3f (负证据 %d 条), 反思式改写候选",
			in.Target, in.MeanScore, in.Samples),
	}
	if _, err := writeProposal(dir, p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ProposePrompt 直接提交一份 prompt 候选 (agent 经 evo_propose 走这条路)。
//
// 与 EvolvePrompt 的区别: 这里**没有 LLM 参与**, 正文由调用方给。但校验一条不少 ——
// 长度上限照查。占位符守恒查不了 (没有原文可比), 所以这条路适合 agent 已经读过原
// prompt 并自己改好的情形; 让它绕过长度闸是不行的, 那是本仓真实的劣化模式。
func ProposePrompt(stateDir, target, body, reason string) (*Proposal, error) {
	target = strings.TrimSpace(target)
	body = strings.TrimSpace(body)
	if target == "" || body == "" {
		return nil, fmt.Errorf("learners: target 与 body 都不能为空")
	}
	if len(body) > gepaMaxPromptChars {
		return nil, fmt.Errorf("learners: 候选 prompt %d 字符超过上限 %d, 拒绝", len(body), gepaMaxPromptChars)
	}
	dir := evoDir(stateDir)
	p := Proposal{
		ID:        "pr-" + shortHash(target+"\x00"+body),
		Kind:      ProposalPrompt,
		Status:    "proposed",
		Target:    target,
		Body:      body,
		Parent:    latestPromptProposal(dir, target),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: "evo_propose",
		Reason:    firstNonEmptyStr(reason, "agent 经 evo_propose 手工提交"),
	}
	if _, err := writeProposal(dir, p); err != nil {
		return nil, err
	}
	return &p, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 草案存储 (proposed 态 + 谱系)
// ---------------------------------------------------------------------------

// ProposalsDir 草案目录。
func ProposalsDir(stateDir string) string { return filepath.Join(evoDir(stateDir), "proposals") }

// writeProposal 幂等写盘。返回 false 表示同 ID 草案已存在 (内容相同, 不重复写)。
//
// 幂等靠 ID = 内容哈希: 空闲期每 15 分钟跑一次归纳, 数据没变就不该每次都生成新文件,
// 否则一夜之间 proposals/ 会有上百个同内容草案。
func writeProposal(dir string, p Proposal) (bool, error) {
	pdir := filepath.Join(dir, "proposals")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		return false, err
	}
	path := filepath.Join(pdir, p.ID+".json")
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, data, 0o644)
}

// ListProposals 读全部草案 (evo_inspect / 晋升门禁用)。
func ListProposals(stateDir string) []Proposal {
	pdir := ProposalsDir(stateDir)
	entries, err := os.ReadDir(pdir)
	if err != nil {
		return nil
	}
	var out []Proposal
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pdir, e.Name()))
		if err != nil {
			continue
		}
		var p Proposal
		if json.Unmarshal(data, &p) == nil && p.ID != "" {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetProposal 按 ID 取草案。
func GetProposal(stateDir, id string) (*Proposal, error) {
	// 不直接拼路径读文件: id 来自 agent 工具的入参, 拼进 filepath.Join 会被
	// "../.." 之类穿出草案目录。走清单匹配是最简单的 fail-closed 写法。
	for _, p := range ListProposals(stateDir) {
		if p.ID == id {
			cp := p
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("草案 %q 不存在", id)
}

// SetProposalStatus 迁移草案状态 (由治理层调用, 本包不自行晋升)。
func SetProposalStatus(stateDir, id, status string) error {
	switch status {
	case "proposed", "shadow", "active", "archived":
	default:
		return fmt.Errorf("非法草案状态 %q", status)
	}
	p, err := GetProposal(stateDir, id)
	if err != nil {
		return err
	}
	p.Status = status
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ProposalsDir(stateDir), p.ID+".json"), data, 0o644)
}

// latestPromptProposal 找同 target 最近的 prompt 草案 ID (谱系的父指针)。
func latestPromptProposal(dir, target string) string {
	var best Proposal
	pdir := filepath.Join(dir, "proposals")
	entries, err := os.ReadDir(pdir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pdir, e.Name()))
		if err != nil {
			continue
		}
		var p Proposal
		if json.Unmarshal(data, &p) != nil || p.Kind != ProposalPrompt || p.Target != target {
			continue
		}
		if p.CreatedAt > best.CreatedAt {
			best = p
		}
	}
	return best.ID
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// missingPlaceholders 返回 current 里有、cand 里没了的 {占位符}。
func missingPlaceholders(current, cand string) []string {
	want := placeholders(current)
	have := placeholders(cand)
	haveSet := map[string]bool{}
	for _, h := range have {
		haveSet[h] = true
	}
	var missing []string
	for _, w := range want {
		if !haveSet[w] {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	return missing
}

// placeholders 抽取 {name} 形式的占位符 (只认单层、不含空白与嵌套括号的, 与仓内
// prompt 注入管线的实际写法一致)。
func placeholders(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != '}' && s[j] != '{' && s[j] != '\n' && s[j] != ' ' {
			j++
		}
		if j < len(s) && s[j] == '}' && j > i+1 {
			out = append(out, s[i:j+1])
			i = j
		}
	}
	return out
}

// stripFence 去掉 LLM 习惯性加的 ```/```markdown 围栏。
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
