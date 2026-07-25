package graph

// reduce_fuse.go —— 两个**确定性**聚合策略: vote (投票融合) 与 trimmed_mean (截尾均值)。
//
// 它们把 pkg/agent 里两个 mode 的融合算法搬进内核, 使
// map[N 路并行]→reduce(融合) 成为真正的 map→reduce:
//
//	ensemble_extract (graph-extract-swarm) → ReduceVote
//	  N 路差异化视角抽取 → 按同名节点 / 同 (src,dst,type) 边投票,
//	  confidence = 命中路数 / 有效路数, 别名与证据取并集 (证据留前 3 条)。
//	  源: pkg/agent/workflow_ensemble.go fuseExtractions。
//
//	review_panel → ReduceTrimmedMean
//	  N 位差异化人格评审 → 每维截尾均值 (剔两端抗离群), overall 取中位数,
//	  共识度 = 1 - 平均极差/100, 批注取并集去重 (上限 20)。
//	  源: pkg/agent/workflow_ensemble.go fuseReviews。
//
// 与 concat/longest 同类: 零 LLM、引擎直接算出结果, 但仍走完整节点生命周期
// (execNode 的 hook/journal/预算)。
//
// # 三处**刻意偏离**上游实现 (逐条说明, 都是为了不产出"看起来正常的错数")
//
//  1. **不做跨维归一化**。上游 fuseReviews 经 ByzantineFuser.TrimmedFuse 求每维截尾
//     均值, 而 TrimmedFuse 末尾调了 normalize() —— 那是把结果当**概率分布**除以各维
//     之和 (calibrator.go:228)。于是三位评审给 80/70/60 的三个维度, 融合后输出的是
//     38/33/29 (0.8/0.7/0.6 各除以 2.1 再 ×100), 而不是各自的截尾均值。
//     函数自己的文档说的是"每维截尾均值", normalize 与该契约矛盾; 且 N<3 时走
//     simpleMerge **不**归一化, 同一份数据的量纲会随评审人数跳变。本实现按文档契约
//     实现 (纯截尾均值, 不归一化)。**这意味着 review_panel 现有产出的 dimensions
//     分数偏低是上游的一个真 bug**, 见报告。
//  2. **缺席不算 0 分**。上游 TrimmedFuse 对每个维度取 p.Predictions[dim], 未给该维
//     打分的评审取到 map 零值 0。于是"只有一位评审提到的维度"= [0,0,80] → 剔两端后
//     取 0 分。本实现每维只对**真给了分**的评审求截尾均值, 并把该维的样本数与是否
//     真做了截尾 (samples/trimmed) 写进输出。
//  3. **排序键补齐到全键**。上游边排序键是 (confidence desc, src asc), 同一 src 的多条
//     边之间顺序取决于 Go map 遍历序 = 每次运行都可能不同。内核的产出必须确定性
//     (同一份输入两次运行逐字节一致, 否则 resume 与等价性测试都不成立), 故补成
//     (confidence desc, src, dst, type)。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// fuseTrimRatio 截尾比例, 与上游 NewByzantineFuser(0.34) 一致:
// 使 N>=3 时至少剔掉一高一低 (int(3*0.34)=1)。
const fuseTrimRatio = 0.34

// fuseMaxEvidence / fuseMaxAnnotations 与上游一致的产出裁剪上限。
const (
	fuseMaxEvidence    = 3
	fuseMaxAnnotations = 20
	// fuseSpreadDisagree 维度极差达到此值即在 summary 上标"评审分歧大" (上游 25)。
	fuseSpreadDisagree = 25.0
)

// ---------------------------------------------------------------------------
// vote —— 抽取文档投票融合
// ---------------------------------------------------------------------------

// 抽取文档的**图本地镜像** (JSON tag 与 pkg/agent 的 exDoc/exNode/exEdge 逐字一致,
// 于是平台侧 parseExtraction 不需要改一行)。为什么不 import: 见 spec.go
// PlacementSpec 的同款论证 —— 内核不认识 agent 语义。
type voteEvidence struct {
	Chapter string `json:"chapter,omitempty"`
	Quote   string `json:"quote,omitempty"`
}

type voteNode struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Aliases      []string       `json:"aliases,omitempty"`
	Props        map[string]any `json:"props,omitempty"`
	FirstChapter string         `json:"first_chapter,omitempty"`
	Confidence   float64        `json:"confidence"`
	Votes        int            `json:"votes,omitempty"`
	Evidence     []voteEvidence `json:"evidence,omitempty"`
}

type voteEdge struct {
	Type       string         `json:"type"`
	Src        string         `json:"src"`
	Dst        string         `json:"dst"`
	Label      string         `json:"label,omitempty"`
	Status     string         `json:"status,omitempty"`
	Confidence float64        `json:"confidence"`
	Votes      int            `json:"votes,omitempty"`
	Evidence   []voteEvidence `json:"evidence,omitempty"`
}

type voteDoc struct {
	Nodes []voteNode `json:"nodes"`
	Edges []voteEdge `json:"edges"`
	// Samples 参与投票的有效样本数 (= confidence 的分母)。
	// 必须随产出一起给: confidence=1.0 在 2 路与 5 路投票下的证据强度差一个量级,
	// 只给比值等于把分母藏起来。
	Samples int `json:"samples"`
}

// fuseNormKey 归一化投票键 (与上游 normKey 一致: 小写 + 去首尾空白)。
func fuseNormKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// fuseVoteDocs 按同名节点 / 同 (src,dst,type) 边投票融合; confidence = 命中路数/总路数。
// docs 按分片序传入 —— 属性/标签"取第一个非空"因此是确定性的。
func fuseVoteDocs(docs []voteDoc) voteDoc {
	n := len(docs)
	type nodeAgg struct {
		node  voteNode
		votes int
	}
	type edgeAgg struct {
		edge  voteEdge
		votes int
	}
	nodeMap := map[string]*nodeAgg{}
	edgeMap := map[string]*edgeAgg{}
	for _, d := range docs {
		seen := map[string]bool{}
		for _, nd := range d.Nodes {
			if strings.TrimSpace(nd.Name) == "" {
				continue
			}
			k := fuseNormKey(nd.Name)
			if seen[k] {
				continue // 同一路内去重: 一路重复提到不该算两票
			}
			seen[k] = true
			a := nodeMap[k]
			if a == nil {
				a = &nodeAgg{node: nd}
				nodeMap[k] = a
			}
			a.votes++
			a.node.Aliases = fuseUniqStrings(append(a.node.Aliases, nd.Aliases...))
			a.node.Evidence = append(a.node.Evidence, nd.Evidence...)
			if a.node.FirstChapter == "" {
				a.node.FirstChapter = nd.FirstChapter
			}
			if a.node.Props == nil {
				a.node.Props = nd.Props
			}
		}
		seenE := map[string]bool{}
		for _, ed := range d.Edges {
			if strings.TrimSpace(ed.Src) == "" || strings.TrimSpace(ed.Dst) == "" {
				continue
			}
			k := fuseNormKey(ed.Src) + "→" + fuseNormKey(ed.Dst) + "|" + fuseNormKey(ed.Type)
			if seenE[k] {
				continue
			}
			seenE[k] = true
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

	out := voteDoc{Samples: n}
	for _, a := range nodeMap {
		a.node.Votes = a.votes
		a.node.Confidence = float64(a.votes) / float64(n)
		if len(a.node.Evidence) > fuseMaxEvidence {
			a.node.Evidence = a.node.Evidence[:fuseMaxEvidence]
		}
		out.Nodes = append(out.Nodes, a.node)
	}
	for _, a := range edgeMap {
		a.edge.Votes = a.votes
		a.edge.Confidence = float64(a.votes) / float64(n)
		if len(a.edge.Evidence) > fuseMaxEvidence {
			a.edge.Evidence = a.edge.Evidence[:fuseMaxEvidence]
		}
		out.Edges = append(out.Edges, a.edge)
	}
	// 全键排序: map 遍历序不确定, 排序键必须能唯一定序 (见文件头偏离说明 3)。
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Confidence != out.Nodes[j].Confidence {
			return out.Nodes[i].Confidence > out.Nodes[j].Confidence
		}
		return fuseNormKey(out.Nodes[i].Name) < fuseNormKey(out.Nodes[j].Name)
	})
	sort.Slice(out.Edges, func(i, j int) bool {
		a, b := out.Edges[i], out.Edges[j]
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if ka, kb := fuseNormKey(a.Src), fuseNormKey(b.Src); ka != kb {
			return ka < kb
		}
		if ka, kb := fuseNormKey(a.Dst), fuseNormKey(b.Dst); ka != kb {
			return ka < kb
		}
		return fuseNormKey(a.Type) < fuseNormKey(b.Type)
	})
	return out
}

func fuseUniqStrings(in []string) []string {
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

// ---------------------------------------------------------------------------
// trimmed_mean —— 多维评审截尾均值融合
// ---------------------------------------------------------------------------

// 评审文档的图本地镜像 (JSON tag 与 pkg/agent 的 rvDoc/rvDim/rvAnn 一致,
// 另加 samples/trimmed 两个**自述**字段)。
type reviewDim struct {
	Dimension string  `json:"dimension"`
	Score     float64 `json:"score"`
	Summary   string  `json:"summary,omitempty"`
	// Samples 真给了该维分数的评审数; Trimmed 该维是否真做了截尾 (>=3 个样本)。
	// 缺了它们, "这一维只有一个人打分"与"三个人打了同样的分"在产出里长得一模一样。
	Samples int  `json:"samples,omitempty"`
	Trimmed bool `json:"trimmed,omitempty"`
}

type reviewAnn struct {
	Dimension  string `json:"dimension"`
	Severity   string `json:"severity,omitempty"`
	Quote      string `json:"quote,omitempty"`
	Note       string `json:"note"`
	Suggestion string `json:"suggestion,omitempty"`
}

type reviewDoc struct {
	Overall     float64     `json:"overall"`
	Consensus   float64     `json:"consensus,omitempty"`
	Summary     string      `json:"summary,omitempty"`
	Dimensions  []reviewDim `json:"dimensions"`
	Annotations []reviewAnn `json:"annotations"`
	// Samples 参与融合的有效评审数; Trimmed 是否**所有**维度都真做了截尾。
	Samples int  `json:"samples"`
	Trimmed bool `json:"trimmed"`
}

// fuseReviewDocs 每维截尾均值 + overall 中位数 + 共识度 + 批注并集。
func fuseReviewDocs(docs []reviewDoc) reviewDoc {
	dimScores := map[string][]float64{} // 只收**真给了分**的评审 (见文件头偏离说明 2)
	dimSummary := map[string]string{}
	var dimKeys []string
	for _, d := range docs {
		for _, dm := range d.Dimensions {
			key := strings.TrimSpace(dm.Dimension)
			if key == "" {
				continue
			}
			if _, seen := dimScores[key]; !seen {
				dimKeys = append(dimKeys, key)
			}
			dimScores[key] = append(dimScores[key], fuseClampScore(dm.Score))
			if dimSummary[key] == "" && strings.TrimSpace(dm.Summary) != "" {
				dimSummary[key] = dm.Summary
			}
		}
	}
	sort.Strings(dimKeys) // 与上游一致: 维度按名排序, 确定性

	var (
		dims       []reviewDim
		spreadSum  float64
		spreadN    int
		allTrimmed = len(dimKeys) > 0
	)
	for _, k := range dimKeys {
		scores := dimScores[k]
		mean, trimmed := fuseTrimmedMean(scores)
		if !trimmed {
			allTrimmed = false
		}
		summary := dimSummary[k]
		if spread := fuseMaxF(scores) - fuseMinF(scores); len(scores) >= 2 {
			spreadSum += spread
			spreadN++
			if spread >= fuseSpreadDisagree {
				summary = fmt.Sprintf("[评审分歧大 极差%.0f] %s", spread, summary)
			}
		}
		dims = append(dims, reviewDim{Dimension: k, Score: mean,
			Summary: strings.TrimSpace(summary), Samples: len(scores), Trimmed: trimmed})
	}

	// overall: 各位 overall 的中位数 (稳健); 无人给 overall 时退回维度均值。
	var overalls []float64
	for _, d := range docs {
		if d.Overall > 0 {
			overalls = append(overalls, d.Overall)
		}
	}
	overall := fuseMedian(overalls)
	if overall == 0 && len(dims) > 0 {
		var s float64
		for _, d := range dims {
			s += d.Score
		}
		overall = s / float64(len(dims))
	}

	consensus := 1.0
	if spreadN > 0 {
		consensus = fuseClamp01(1.0 - (spreadSum/float64(spreadN))/100.0)
	}

	annSeen := map[string]bool{}
	var anns []reviewAnn
	for _, d := range docs {
		for _, a := range d.Annotations {
			if strings.TrimSpace(a.Note) == "" {
				continue
			}
			k := fuseNormKey(a.Dimension) + "|" + fuseNormKey(a.Quote) + "|" + fuseNormKey(a.Note)
			if annSeen[k] {
				continue
			}
			annSeen[k] = true
			anns = append(anns, a)
			if len(anns) >= fuseMaxAnnotations {
				break
			}
		}
	}

	summary := ""
	for _, d := range docs {
		if strings.TrimSpace(d.Summary) != "" {
			summary = d.Summary
			break
		}
	}
	summary = fmt.Sprintf("（%d 位评审合议，共识度 %.0f%%）%s", len(docs), consensus*100, summary)

	return reviewDoc{Overall: overall, Consensus: consensus, Summary: summary,
		Dimensions: dims, Annotations: anns, Samples: len(docs), Trimmed: allTrimmed}
}

// fuseTrimmedMean 截尾均值: 剔掉两端各 int(N*0.34) 个 (至少 1 个) 后取均值。
// 第二个返回值 = 是否真的截了尾; N<3 时截完就空, 只能退回普通均值, 此时返回 false
// —— 调用方必须把它写进产出 (trimmed=false), 因为那时"抗单个离群评审"这个性质不存在。
func fuseTrimmedMean(vals []float64) (float64, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if len(s) < 3 {
		return fuseMean(s), false
	}
	k := int(float64(len(s)) * fuseTrimRatio)
	if k < 1 {
		k = 1
	}
	t := s[k : len(s)-k]
	if len(t) == 0 { // 防御: trimRatio<0.5 时不可达
		return fuseMean(s), false
	}
	return fuseMean(t), true
}

func fuseMean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// fuseClampScore 把维度分夹到 [0,100] (上游经 clamp01(score/100) 做的同一件事)。
func fuseClampScore(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 100:
		return 100
	}
	return x
}

func fuseClamp01(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	}
	return x
}

func fuseMaxF(xs []float64) float64 {
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

func fuseMinF(xs []float64) float64 {
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

func fuseMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ---------------------------------------------------------------------------
// 分片产出 → 文档: 解析 + 样本闸
// ---------------------------------------------------------------------------

// fuseParseDocs 把分片产出解析成 T, 返回 (有效文档, 每条被丢弃的原因)。
// usable 判定交给调用方: "能 Unmarshal" 与"内容够用"不是一回事 —— 一个
// `{}` 能解析但对投票是零信息, 计进样本数会稀释 confidence 的分母。
func fuseParseDocs[T any](outs []string, usable func(T) bool) ([]T, []string) {
	var docs []T
	var bad []string
	for i, raw := range outs {
		var v T
		if err := json.Unmarshal([]byte(fuseExtractJSON(raw)), &v); err != nil {
			bad = append(bad, fmt.Sprintf("#%d 无法解析为 JSON 对象 (len=%d)", i, len(raw)))
			continue
		}
		if !usable(v) {
			bad = append(bad, fmt.Sprintf("#%d 解析成功但内容为空 (len=%d)", i, len(raw)))
			continue
		}
		docs = append(docs, v)
	}
	return docs, bad
}

// fuseExtractJSON 从 LLM 原文里取出第一个 JSON 对象。
//
// 为什么内核要自带这一小段而不是 import pkg/swarm_intel 的 ParseJSON:
// 那个包除了解析还带全局状态与文件 IO (store.go / learning.go 的 DataDir),
// 调度内核为了一个 JSON 解析把它拉进依赖不值得。这里只做上游 Level 1-3
// (直接 → 去围栏 → 提取首个对象), **不做 Level 4 的"修复"** —— 修复会把一份坏
// JSON 猜成一份能解析的 JSON, 而融合结果要进交付, 猜出来的样本比少一个样本更糟。
//
// 括号扫描必须**字符串感知**: 产出里出现 `"note": "}"` 这种内容时, 裸计数会在
// 字符串中间收尾, 截出一段非法 JSON —— 这个坑在别处踩过 (REPORT.md 回显里带
// ``` 与花括号导致围栏错配)。
func fuseExtractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "{}"
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return s // 常见情形: 分片直接吐了 JSON
	}
	// 去 markdown 围栏 (```json ... ``` / ``` ... ```)。
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			if lang := strings.TrimSpace(rest[:j]); lang == "" || !strings.ContainsAny(lang, " \t{") {
				rest = rest[j+1:]
			}
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			rest = rest[:k]
		}
		if t := strings.TrimSpace(rest); strings.HasPrefix(t, "{") {
			s = t
		}
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "{}"
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// 字符串内的括号不计数
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return "{}" // 截断的对象: 交给调用方判为"无法解析", 不猜补
}

// reduceVoteResult 投票融合的 reduce 结果。okOuts 是 completed 分片的产出 (按分片序)。
func reduceVoteResult(okOuts []string, minSamples int) NodeResult {
	if minSamples <= 0 {
		minSamples = DefaultVoteMinSamples
	}
	docs, bad := fuseParseDocs(okOuts, func(d voteDoc) bool {
		return len(d.Nodes) > 0 || len(d.Edges) > 0
	})
	if len(docs) < minSamples {
		return NodeResult{Status: NodeStatusFailed, Err: fmt.Sprintf(
			"reduce(%s): 只有 %d/%d 个分片产出可用于投票, 少于 min_samples=%d%s",
			ReduceVote, len(docs), len(okOuts), minSamples, fuseBadNote(bad))}
	}
	fused := fuseVoteDocs(docs)
	out, err := json.MarshalIndent(fused, "", "  ")
	if err != nil { // 不可达 (全是基本类型), 但绝不静默交空产出
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("reduce(%s): 融合结果序列化失败: %v", ReduceVote, err)}
	}
	// Score 留 0 (= 未回报, 与 NodeResult.Tokens 同款约定): 投票没有自然的评分口径,
	// 填一个平均 confidence 会让 `score >= 75` 的条件边看起来可用 —— 它的量纲是
	// 0-1 而 gate 的是 0-100, 那种条件边永假, 且没有任何报错。
	return NodeResult{Status: NodeStatusCompleted, Output: string(out)}
}

// reduceTrimmedMeanResult 截尾均值融合的 reduce 结果。
func reduceTrimmedMeanResult(okOuts []string, minSamples int) NodeResult {
	if minSamples <= 0 {
		minSamples = DefaultTrimmedMeanMinSamples
	}
	docs, bad := fuseParseDocs(okOuts, func(d reviewDoc) bool {
		return len(d.Dimensions) > 0 || d.Overall > 0
	})
	if len(docs) < minSamples {
		return NodeResult{Status: NodeStatusFailed, Err: fmt.Sprintf(
			"reduce(%s): 只有 %d/%d 个分片产出可用于融合, 少于 min_samples=%d "+
				"(样本 <3 时剔两端后为空, 截尾均值退化为普通均值 —— 抗离群这个它唯一的作用消失, "+
				"但产出的数字看起来完全正常, 故默认拒绝出数)%s",
			ReduceTrimmedMean, len(docs), len(okOuts), minSamples, fuseBadNote(bad))}
	}
	fused := fuseReviewDocs(docs)
	out, err := json.MarshalIndent(fused, "", "  ")
	if err != nil {
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("reduce(%s): 融合结果序列化失败: %v", ReduceTrimmedMean, err)}
	}
	// Score = 融合后的 overall: 与评审维度同量纲 (0-100), 下游可直接挂
	// `score >= 75` 的条件边 —— 这正是 review_panel 接进图后要的东西。
	return NodeResult{Status: NodeStatusCompleted, Output: string(out), Score: fused.Overall}
}

// fuseBadNote 被丢弃样本的诊断串 (最多 3 条)。
// 失败原因必须带上这个: "只有 1/3 个可用"不说清另外两个为什么废掉, 排查只能靠翻
// 每个分片的原文。
func fuseBadNote(bad []string) string {
	if len(bad) == 0 {
		return ""
	}
	if len(bad) > 3 {
		bad = append(bad[:3:3], fmt.Sprintf("…另有 %d 条", len(bad)-3))
	}
	return " [" + strings.Join(bad, "; ") + "]"
}
