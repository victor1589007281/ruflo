package agent

// 群体智能「合议」工作流：把单 agent 一次的抽取/评审升级为多 agent 并行 + 融合共识，
// 显著降低单次幻觉/异常打分。复用 swarm_intel 的 FanOutCollect(并发扇出) / ParseJSON(健壮解析)
// / ByzantineFuser(截尾均值抗离群) 原语，绕开重耦合的 Engine。
//
//   - graph-extract-swarm: N 路差异化视角抽取 → 按同名节点/同边投票，confidence = 命中率。
//   - review-panel:        N 位差异化人格评审 → 每维截尾均值 + 共识度 + 分歧标注，批注取并集。
//
// 两者输出 **与单 agent 版完全相同的 JSON schema**，故平台侧 parseExtraction / parseReview 无需改动，
// 只需把 workflow 名换成 -swarm/-panel 即可。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

// ensembleFanOutConfig：合议扇出超时【放宽版】。并发保持默认 4(并发从来不是病因——早前误改
// MaxConcurrency=1 反而更差, 已证伪)。真正需要放宽的是 **单路超时**:
// ceiling 抬到 65536 后模型不再被 max_tokens 截断, 会跑完整 thinking+JSON, 密集章单路生成
// 偶 >180s(DefaultFanOutConfig.PerBranchTimeout=3min)→ context deadline exceeded 三路齐崩。
// 故 PerBranchTimeout=6min 给足生成时间;TotalTimeout=7.5min 容纳并发下的最慢一路,
// 仍 < storyloom 异步轮询 480s? 否——450s<480s 才安全, 取 450s(7.5min)。
// 仅供 graph-extract-swarm 使用, 不改全局 DefaultFanOutConfig(Simulate/Predict/novel-v3 等不受影响)。
func ensembleFanOutConfig() swarm_intel.FanOutConfig {
	return swarm_intel.FanOutConfig{
		MaxConcurrency:   4,
		PerBranchTimeout: 6 * time.Minute,
		TotalTimeout:     450 * time.Second,
	}
}

// ================= graph-extract-swarm =================

func graphExtractSwarmWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "graph-extract-swarm",
		Description: "合议图谱抽取: N 路差异化视角并行抽取 + 投票融合(confidence=命中率), 抗单次幻觉",
		Mode:        "ensemble_extract",
		QualityGate: "none",
	}
}

// 三种差异化抽取视角（提升覆盖：各有侧重但都全量抽取）。
var extractLenses = []string{
	"你尤其擅长识别【人物】及其相互【关系(rel)】。",
	"你尤其擅长识别【事件】及其【因果链(causes/triggers/enables/affects)】。",
	"你尤其擅长识别【伏笔(setup/payoff)】【物品(owns)】与【世界观设定】。",
}

type exEvidence struct {
	Chapter string `json:"chapter,omitempty"`
	Quote   string `json:"quote,omitempty"`
}
type exNode struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Aliases      []string       `json:"aliases,omitempty"`
	Props        map[string]any `json:"props,omitempty"`
	FirstChapter string         `json:"first_chapter,omitempty"`
	Confidence   float64        `json:"confidence"`
	Votes        int            `json:"votes,omitempty"`
	Evidence     []exEvidence   `json:"evidence,omitempty"`
}
type exEdge struct {
	Type       string       `json:"type"`
	Src        string       `json:"src"`
	Dst        string       `json:"dst"`
	Label      string       `json:"label,omitempty"`
	Status     string       `json:"status,omitempty"`
	Confidence float64      `json:"confidence"`
	Votes      int          `json:"votes,omitempty"`
	Evidence   []exEvidence `json:"evidence,omitempty"`
}
type exDoc struct {
	Nodes []exNode `json:"nodes"`
	Edges []exEdge `json:"edges"`
}

func normKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func (we *WorkflowExecutor) executeEnsembleExtract(ctx context.Context, _ *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	if we.llm == nil {
		return nil, fmt.Errorf("graph-extract-swarm 需要 LLMClient")
	}
	notify := func(m string) {
		if team != nil {
			we.notify(team.ChatID, m)
		}
	}
	notify(fmt.Sprintf("⚡ **合议抽取**: %d 路差异化视角并行抽取中…", len(extractLenses)))

	// 每路诊断元数据(stop/outTok/blocks/tail),用于定位"空响应/截断/超时"根因。
	// 走 *api.Client.CompleteDiag 时才有;并发写入,读取发生在 FanOutCollect(wg.Wait)之后。
	var diagMu sync.Mutex
	branchMeta := map[string]string{}
	branches := map[string]swarm_intel.BranchFunc{}
	for i, lens := range extractLenses {
		l := lens
		bid := fmt.Sprintf("extractor-%d", i)
		branches[bid] = func(ctx context.Context) (string, error) {
			sys := "你是小说设定分析师。" + l + " 严格按用户要求只输出一个 JSON。"
			if cli, ok := we.llm.(*api.Client); ok {
				text, meta, err := cli.CompleteDiag(ctx, sys, objective)
				diagMu.Lock()
				branchMeta[bid] = meta
				diagMu.Unlock()
				return text, err
			}
			return we.llm.SimpleComplete(ctx, sys, objective)
		}
	}
	results := swarm_intel.FanOutCollect(ctx, ensembleFanOutConfig(), branches)

	var docs []exDoc
	var diag []string
	for _, r := range results {
		meta := branchMeta[r.ID]
		secs := r.Latency.Seconds()
		if r.Error != nil {
			diag = append(diag, fmt.Sprintf("%s(%.0fs): err=%v | %s", r.ID, secs, r.Error, meta))
			continue
		}
		if strings.TrimSpace(r.Value) == "" {
			diag = append(diag, fmt.Sprintf("%s(%.0fs): 空响应 | %s", r.ID, secs, meta))
			continue
		}
		pr := swarm_intel.ParseJSON[exDoc](r.Value)
		diag = append(diag, fmt.Sprintf("%s(%.0fs): len=%d parseOK=%v nodes=%d edges=%d | %s", r.ID, secs, len(r.Value), pr.OK, len(pr.Value.Nodes), len(pr.Value.Edges), meta))
		if pr.OK && (len(pr.Value.Nodes) > 0 || len(pr.Value.Edges) > 0) {
			docs = append(docs, pr.Value)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("合议抽取: 无任何可解析的抽取结果 [%s]", strings.Join(diag, " | "))
	}
	fused := fuseExtractions(docs)
	data, _ := json.MarshalIndent(fused, "", "  ")
	notify(fmt.Sprintf("✅ 合议抽取完成: %d/%d 路有效 → 融合 %d 节点/%d 边", len(docs), len(extractLenses), len(fused.Nodes), len(fused.Edges)))
	output := fmt.Sprintf("合议抽取(%d 路投票)结果:\n\n```json\n%s\n```\n", len(docs), string(data))
	return []StageResult{{Name: "ensemble-extract", Role: "graph-swarm", Status: TaskCompleted, Output: output}}, nil
}

// fuseExtractions 按同名节点 / 同 (src,dst,type) 边投票融合；confidence = 命中路数 / 总有效路数。
func fuseExtractions(docs []exDoc) exDoc {
	n := len(docs)
	type nodeAgg struct {
		node  exNode
		votes int
	}
	nodeMap := map[string]*nodeAgg{}
	for _, d := range docs {
		seen := map[string]bool{}
		for _, nd := range d.Nodes {
			if strings.TrimSpace(nd.Name) == "" {
				continue
			}
			k := normKey(nd.Name)
			if seen[k] {
				continue // 同一路内去重，避免重复计票
			}
			seen[k] = true
			a := nodeMap[k]
			if a == nil {
				a = &nodeAgg{node: nd}
				nodeMap[k] = a
			}
			a.votes++
			// 合并别名/证据/属性
			a.node.Aliases = uniqStrings(append(a.node.Aliases, nd.Aliases...))
			a.node.Evidence = append(a.node.Evidence, nd.Evidence...)
			if a.node.FirstChapter == "" {
				a.node.FirstChapter = nd.FirstChapter
			}
			if a.node.Props == nil {
				a.node.Props = nd.Props
			}
		}
	}
	type edgeAgg struct {
		edge  exEdge
		votes int
	}
	edgeMap := map[string]*edgeAgg{}
	for _, d := range docs {
		seen := map[string]bool{}
		for _, ed := range d.Edges {
			if strings.TrimSpace(ed.Src) == "" || strings.TrimSpace(ed.Dst) == "" {
				continue
			}
			k := normKey(ed.Src) + "→" + normKey(ed.Dst) + "|" + normKey(ed.Type)
			if seen[k] {
				continue
			}
			seen[k] = true
			a := edgeMap[k]
			if a == nil {
				a = &edgeAgg{edge: ed}
				edgeMap[k] = a
			}
			a.votes++
			a.edge.Evidence = append(a.edge.Evidence, ed.Evidence...)
			if a.edge.Label == "" {
				a.edge.Label = ed.Label
			}
		}
	}
	out := exDoc{}
	for _, a := range nodeMap {
		a.node.Votes = a.votes
		a.node.Confidence = float64(a.votes) / float64(n)
		if len(a.node.Evidence) > 3 {
			a.node.Evidence = a.node.Evidence[:3]
		}
		out.Nodes = append(out.Nodes, a.node)
	}
	for _, a := range edgeMap {
		a.edge.Votes = a.votes
		a.edge.Confidence = float64(a.votes) / float64(n)
		if len(a.edge.Evidence) > 3 {
			a.edge.Evidence = a.edge.Evidence[:3]
		}
		out.Edges = append(out.Edges, a.edge)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Confidence != out.Nodes[j].Confidence {
			return out.Nodes[i].Confidence > out.Nodes[j].Confidence
		}
		return out.Nodes[i].Name < out.Nodes[j].Name
	})
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].Confidence != out.Edges[j].Confidence {
			return out.Edges[i].Confidence > out.Edges[j].Confidence
		}
		return out.Edges[i].Src < out.Edges[j].Src
	})
	return out
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ================= review-panel =================

func reviewPanelWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "review-panel",
		Description: "合议评审团: N 位差异化人格评审 + 每维截尾均值(抗离群) + 共识度/分歧标注, 批注取并集",
		Mode:        "review_panel",
		QualityGate: "none",
	}
}

var reviewLenses = []string{
	"你是要求严苛的资深主编，最擅长挑出结构、逻辑与人物动机的硬伤。",
	"你是重视读者代入与情感体验的编辑，关注共鸣、悬念与阅读快感。",
	"你是文笔与叙事张力的鉴赏者，关注语言质感、节奏与画面感。",
}

type rvDim struct {
	Dimension string  `json:"dimension"`
	Score     float64 `json:"score"`
	Summary   string  `json:"summary,omitempty"`
}
type rvAnn struct {
	Dimension  string `json:"dimension"`
	Severity   string `json:"severity,omitempty"`
	Quote      string `json:"quote,omitempty"`
	Note       string `json:"note"`
	Suggestion string `json:"suggestion,omitempty"`
}
type rvDoc struct {
	Overall     float64  `json:"overall"`
	Consensus   float64  `json:"consensus,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Dimensions  []rvDim  `json:"dimensions"`
	Annotations []rvAnn  `json:"annotations"`
}

func (we *WorkflowExecutor) executeReviewPanel(ctx context.Context, _ *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	if we.llm == nil {
		return nil, fmt.Errorf("review-panel 需要 LLMClient")
	}
	notify := func(m string) {
		if team != nil {
			we.notify(team.ChatID, m)
		}
	}
	notify(fmt.Sprintf("⚡ **合议评审**: %d 位差异化人格评审并行中…", len(reviewLenses)))

	branches := map[string]swarm_intel.BranchFunc{}
	for i, lens := range reviewLenses {
		l := lens
		branches[fmt.Sprintf("judge-%d", i)] = func(ctx context.Context) (string, error) {
			return we.llm.SimpleComplete(ctx, "你是小说评审专家。"+l+" 严格按用户要求只输出一个 JSON。", objective)
		}
	}
	results := swarm_intel.FanOutCollect(ctx, swarm_intel.DefaultFanOutConfig(), branches)

	var docs []rvDoc
	var diag []string
	for id, r := range results {
		if r.Error != nil {
			diag = append(diag, fmt.Sprintf("%d: err=%v", id, r.Error))
			continue
		}
		if strings.TrimSpace(r.Value) == "" {
			diag = append(diag, fmt.Sprintf("%d: 空响应", id))
			continue
		}
		pr := swarm_intel.ParseJSON[rvDoc](r.Value)
		diag = append(diag, fmt.Sprintf("%d: len=%d parseOK=%v dims=%d overall=%.0f", id, len(r.Value), pr.OK, len(pr.Value.Dimensions), pr.Value.Overall))
		if pr.OK && (len(pr.Value.Dimensions) > 0 || pr.Value.Overall > 0) {
			docs = append(docs, pr.Value)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("合议评审: 无任何可解析的评审结果 [%s]", strings.Join(diag, " | "))
	}
	fused := fuseReviews(docs)
	data, _ := json.MarshalIndent(fused, "", "  ")
	notify(fmt.Sprintf("✅ 合议评审完成: %d/%d 位有效 → 融合 overall %.0f, 共识 %.0f%%", len(docs), len(reviewLenses), fused.Overall, fused.Consensus*100))
	output := fmt.Sprintf("合议评审(%d 位, 截尾均值)结果:\n\n```json\n%s\n```\n", len(docs), string(data))
	return []StageResult{{Name: "review-panel", Role: "review-swarm", Status: TaskCompleted, Output: output}}, nil
}

// fuseReviews 每维用 ByzantineFuser 截尾均值(抗单个异常打分)，共识度 = 1 - 平均分歧；批注取并集去重。
func fuseReviews(docs []rvDoc) rvDoc {
	// 每位评审 → AgentPrediction(Predictions = {dim: score/100})，喂 ByzantineFuser.TrimmedFuse
	bf := swarm_intel.NewByzantineFuser(0.34) // trimRatio 使 N≥3 时至少剔 1
	var preds []swarm_intel.AgentPrediction
	dimSpread := map[string][]float64{}
	dimSummary := map[string]string{}
	for i, d := range docs {
		pm := map[string]float64{}
		for _, dm := range d.Dimensions {
			key := strings.TrimSpace(dm.Dimension)
			if key == "" {
				continue
			}
			pm[key] = clamp01(dm.Score / 100.0)
			dimSpread[key] = append(dimSpread[key], dm.Score)
			if dimSummary[key] == "" && strings.TrimSpace(dm.Summary) != "" {
				dimSummary[key] = dm.Summary
			}
		}
		preds = append(preds, swarm_intel.AgentPrediction{AgentID: fmt.Sprintf("judge-%d", i), Predictions: pm})
	}
	fusedDims := bf.TrimmedFuse(preds)

	var dims []rvDim
	var spreadSum float64
	var spreadN int
	dimKeys := make([]string, 0, len(fusedDims))
	for k := range fusedDims {
		dimKeys = append(dimKeys, k)
	}
	sort.Strings(dimKeys)
	for _, k := range dimKeys {
		scores := dimSpread[k]
		spread := maxF(scores) - minF(scores)
		if len(scores) >= 2 {
			spreadSum += spread
			spreadN++
		}
		summary := dimSummary[k]
		if spread >= 25 {
			summary = fmt.Sprintf("[评审分歧大 极差%.0f] %s", spread, summary)
		}
		dims = append(dims, rvDim{Dimension: k, Score: fusedDims[k] * 100.0, Summary: strings.TrimSpace(summary)})
	}

	// overall：各位 overall 的中位数(稳健)
	var overalls []float64
	for _, d := range docs {
		if d.Overall > 0 {
			overalls = append(overalls, d.Overall)
		}
	}
	overall := medianF(overalls)
	if overall == 0 && len(dims) > 0 { // 无 overall 时取维度均值
		var s float64
		for _, d := range dims {
			s += d.Score
		}
		overall = s / float64(len(dims))
	}

	consensus := 1.0
	if spreadN > 0 {
		consensus = clamp01(1.0 - (spreadSum/float64(spreadN))/100.0)
	}

	// 批注并集，按 (dimension+quote) 去重，最多 20 条
	annSeen := map[string]bool{}
	var anns []rvAnn
	for _, d := range docs {
		for _, a := range d.Annotations {
			if strings.TrimSpace(a.Note) == "" {
				continue
			}
			k := normKey(a.Dimension) + "|" + normKey(a.Quote) + "|" + normKey(a.Note)
			if annSeen[k] {
				continue
			}
			annSeen[k] = true
			anns = append(anns, a)
			if len(anns) >= 20 {
				break
			}
		}
	}

	// 综合总评：取第一位有 summary 的
	summary := ""
	for _, d := range docs {
		if strings.TrimSpace(d.Summary) != "" {
			summary = d.Summary
			break
		}
	}
	summary = fmt.Sprintf("（%d 位评审合议，共识度 %.0f%%）%s", len(docs), consensus*100, summary)

	return rvDoc{Overall: overall, Consensus: consensus, Summary: summary, Dimensions: dims, Annotations: anns}
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
func maxF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}
func minF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}
func medianF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64{}, xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
