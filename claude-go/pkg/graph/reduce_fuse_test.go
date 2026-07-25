package graph

// 确定性聚合策略 vote / trimmed_mean 回归 (reduce_fuse.go)。
//
// 这两个策略的产出**直接进交付** (平台按 JSON schema 解析), 所以断言的重点不是
// "跑通了", 而是:
//   - 数字对不对 (confidence 是命中率; 维度分是截尾均值而**不是**归一化后的分布);
//   - 样本不足时**拒绝出数**而不是给一个看起来正常的数;
//   - 同一份输入两次运行逐字节一致 (map 遍历序不得泄漏进产出)。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// exOut 造一份抽取文档产出 (裸 JSON)。
func exOut(t *testing.T, d voteDoc) string {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rvOut 造一份评审文档产出 (裸 JSON)。
func rvOut(t *testing.T, d reviewDoc) string {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parseVote 解析融合产出。
func parseVote(t *testing.T, out string) voteDoc {
	t.Helper()
	var d voteDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("融合产出不是合法 JSON: %v\n%s", err, out)
	}
	return d
}

func parseReview(t *testing.T, out string) reviewDoc {
	t.Helper()
	var d reviewDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("融合产出不是合法 JSON: %v\n%s", err, out)
	}
	return d
}

// ---------------------------------------------------------------------------
// vote
// ---------------------------------------------------------------------------

// confidence = 命中路数/有效路数; 同名节点合并别名与证据; 一路内重复不重复计票。
func TestReduceVote_投票融合(t *testing.T) {
	outs := []string{
		exOut(t, voteDoc{
			Nodes: []voteNode{
				{Type: "person", Name: "李云", Aliases: []string{"云哥"}, FirstChapter: "c1",
					Evidence: []voteEvidence{{Chapter: "c1", Quote: "甲"}}},
				{Type: "person", Name: "李云"}, // 同一路内重复: 不该算两票
				{Type: "item", Name: "剑"},
			},
			Edges: []voteEdge{{Type: "owns", Src: "李云", Dst: "剑"}},
		}),
		exOut(t, voteDoc{
			Nodes: []voteNode{{Type: "person", Name: " 李云 ", Aliases: []string{"小云"},
				Evidence: []voteEvidence{{Chapter: "c2", Quote: "乙"}}}},
			Edges: []voteEdge{{Type: "owns", Src: "李云", Dst: "剑", Label: "佩剑"}},
		}),
		exOut(t, voteDoc{
			Nodes: []voteNode{{Type: "person", Name: "李云",
				Evidence: []voteEvidence{{Chapter: "c3", Quote: "丙"}, {Chapter: "c3", Quote: "丁"}}}},
		}),
	}
	res := reduceVoteResult(outs, 0)
	if res.Status != NodeStatusCompleted {
		t.Fatalf("融合失败: %s", res.Err)
	}
	got := parseVote(t, res.Output)
	if got.Samples != 3 {
		t.Errorf("samples = %d, 期望 3 (confidence 的分母必须随产出给出)", got.Samples)
	}
	byName := map[string]voteNode{}
	for _, n := range got.Nodes {
		byName[strings.TrimSpace(n.Name)] = n
	}
	li := byName["李云"]
	if li.Votes != 3 || li.Confidence != 1.0 {
		t.Errorf("李云 votes=%d conf=%v, 期望 3 / 1.0 (三路都提到, 一路内重复不计两票)", li.Votes, li.Confidence)
	}
	if len(li.Aliases) != 2 {
		t.Errorf("别名应取并集去重, 实得 %v", li.Aliases)
	}
	if li.FirstChapter != "c1" {
		t.Errorf("first_chapter 应取第一个非空, 实得 %q", li.FirstChapter)
	}
	if len(li.Evidence) != 3 {
		t.Errorf("证据应取并集并留前 3 条, 实得 %d 条", len(li.Evidence))
	}
	if sw := byName["剑"]; sw.Votes != 1 || sw.Confidence > 0.34 {
		t.Errorf("剑 votes=%d conf=%v, 期望 1 / 1/3", sw.Votes, sw.Confidence)
	}
	if len(got.Edges) != 1 || got.Edges[0].Votes != 2 || got.Edges[0].Label != "佩剑" {
		t.Errorf("边应按 (src,dst,type) 合并且补上非空 label, 实得 %+v", got.Edges)
	}
	// 置信度降序 → 李云 (1.0) 在前。
	if strings.TrimSpace(got.Nodes[0].Name) != "李云" {
		t.Errorf("节点应按 confidence 降序, 首位 = %q", got.Nodes[0].Name)
	}
}

// 产出必须**逐字节确定性**: map 遍历序不得泄漏进结果 (否则 resume 与等价性测试都不成立)。
// 同 confidence 同 src 的多条边是最容易漏的一档 —— 上游排序键只到 src。
func TestReduceVote_产出逐字节确定性(t *testing.T) {
	doc := voteDoc{
		Nodes: []voteNode{{Name: "甲"}, {Name: "乙"}, {Name: "丙"}, {Name: "丁"}},
		Edges: []voteEdge{
			{Type: "rel", Src: "甲", Dst: "乙"}, {Type: "rel", Src: "甲", Dst: "丙"},
			{Type: "owns", Src: "甲", Dst: "丁"}, {Type: "rel", Src: "乙", Dst: "丙"},
		},
	}
	outs := []string{exOut(t, doc), exOut(t, doc)}
	first := reduceVoteResult(outs, 0).Output
	for i := 0; i < 12; i++ {
		if got := reduceVoteResult(outs, 0).Output; got != first {
			t.Fatalf("第 %d 次融合结果不同 (排序键无法唯一定序):\n%s\n---\n%s", i, first, got)
		}
	}
}

// 分片产出常带 ```json 围栏或前后散文: 必须能取出对象; 截断的对象判为不可用而**不猜补**。
func TestReduceVote_围栏与散文包裹的产出(t *testing.T) {
	body := exOut(t, voteDoc{Nodes: []voteNode{{Name: "甲"}}})
	outs := []string{
		"分析如下:\n```json\n" + body + "\n```\n以上。",
		"结果: " + body,
		"```\n" + body + "\n```",
	}
	res := reduceVoteResult(outs, 3)
	if res.Status != NodeStatusCompleted {
		t.Fatalf("围栏/散文包裹的 JSON 应能解析: %s", res.Err)
	}
	if got := parseVote(t, res.Output); got.Samples != 3 {
		t.Errorf("三路都应被识别为有效样本, 实得 %d", got.Samples)
	}
	// 截断的对象 (缺右括号) 不许被"修复"成一份能解析的样本。
	trunc := `{"nodes":[{"name":"甲"`
	if res := reduceVoteResult([]string{trunc, trunc, trunc}, 0); res.Status != NodeStatusFailed {
		t.Errorf("截断产出应判为不可用, 实得 %s / %s", res.Status, res.Output)
	}
	// 字符串里带花括号不该让括号扫描提前收尾。
	tricky := `{"nodes":[{"name":"甲","type":"a}b"}]}`
	if res := reduceVoteResult([]string{tricky}, 1); res.Status != NodeStatusCompleted {
		t.Errorf("字符串内的花括号不该干扰扫描: %s", res.Err)
	}
}

// 空集合与"能解析但没内容"都不算样本 —— 计进分母会稀释 confidence。
func TestReduceVote_样本闸(t *testing.T) {
	body := exOut(t, voteDoc{Nodes: []voteNode{{Name: "甲"}}})
	res := reduceVoteResult([]string{body, "{}", "这不是 JSON"}, 3)
	if res.Status != NodeStatusFailed {
		t.Fatalf("只有 1 路有效却按 min_samples=3 出数了: %s", res.Output)
	}
	if !strings.Contains(res.Err, "min_samples=3") || !strings.Contains(res.Err, "1/3") {
		t.Errorf("失败原因必须写清样本数: %s", res.Err)
	}
	if !strings.Contains(res.Err, "#1") || !strings.Contains(res.Err, "#2") {
		t.Errorf("失败原因必须带上被丢弃样本的诊断: %s", res.Err)
	}
	// 缺省下限 = 1: 单路投票是弱证据但不是错数 (confidence 的分母自述为 1)。
	if res := reduceVoteResult([]string{body}, 0); res.Status != NodeStatusCompleted {
		t.Errorf("vote 缺省下限应为 1, 实得 %s / %s", res.Status, res.Err)
	}
}

// ---------------------------------------------------------------------------
// trimmed_mean
// ---------------------------------------------------------------------------

// 每维截尾均值: 三位评审给 80/70/60 → 剔两端留中位 70。
// **关键断言: 不是 33** —— 上游 ByzantineFuser.TrimmedFuse 末尾的 normalize() 会把
// 各维除以各维之和当概率分布 (0.7/2.1≈0.33), 与它自己文档写的"每维截尾均值"矛盾。
func TestReduceTrimmedMean_每维截尾均值不做跨维归一(t *testing.T) {
	mk := func(plot, char, prose, overall float64) reviewDoc {
		return reviewDoc{Overall: overall, Dimensions: []reviewDim{
			{Dimension: "plot", Score: plot}, {Dimension: "character", Score: char},
			{Dimension: "prose", Score: prose}}}
	}
	outs := []string{
		rvOut(t, mk(80, 70, 60, 70)),
		rvOut(t, mk(82, 71, 61, 72)),
		rvOut(t, mk(20, 69, 59, 30)), // 离群评审: 截尾后 plot 不受它影响
	}
	res := reduceTrimmedMeanResult(outs, 0)
	if res.Status != NodeStatusCompleted {
		t.Fatalf("融合失败: %s", res.Err)
	}
	got := parseReview(t, res.Output)
	dims := map[string]reviewDim{}
	for _, d := range got.Dimensions {
		dims[d.Dimension] = d
	}
	if s := dims["plot"].Score; s != 80 {
		t.Errorf("plot 截尾均值 = %v, 期望 80 (剔掉 20 与 82 后只剩 80); 若得到 ~33 说明做了跨维归一", s)
	}
	if s := dims["character"].Score; s != 70 {
		t.Errorf("character = %v, 期望 70", s)
	}
	if !dims["plot"].Trimmed || dims["plot"].Samples != 3 {
		t.Errorf("维度应自述样本数与是否截尾: %+v", dims["plot"])
	}
	if got.Overall != 70 {
		t.Errorf("overall 应为各位 overall 的中位数 (30/70/72 → 70), 实得 %v", got.Overall)
	}
	if res.Score != got.Overall {
		t.Errorf("NodeResult.Score 应等于融合 overall (供 `score >= N` 条件边), 实得 %v", res.Score)
	}
	if !got.Trimmed || got.Samples != 3 {
		t.Errorf("文档级应自述 samples/trimmed: samples=%d trimmed=%v", got.Samples, got.Trimmed)
	}
	if !strings.Contains(got.Summary, "3 位评审") {
		t.Errorf("总评应带合议人数与共识度: %q", got.Summary)
	}
}

// 缺席不算 0 分: 只有一位评审提到的维度, 分数应是那一位给的分, 而不是 [0,0,x] 剔完的 0。
func TestReduceTrimmedMean_缺席维度不算零分(t *testing.T) {
	outs := []string{
		rvOut(t, reviewDoc{Overall: 70, Dimensions: []reviewDim{
			{Dimension: "plot", Score: 70}, {Dimension: "冷门维度", Score: 88}}}),
		rvOut(t, reviewDoc{Overall: 72, Dimensions: []reviewDim{{Dimension: "plot", Score: 72}}}),
		rvOut(t, reviewDoc{Overall: 68, Dimensions: []reviewDim{{Dimension: "plot", Score: 68}}}),
	}
	got := parseReview(t, reduceTrimmedMeanResult(outs, 0).Output)
	for _, d := range got.Dimensions {
		if d.Dimension != "冷门维度" {
			continue
		}
		if d.Score != 88 {
			t.Errorf("冷门维度 = %v, 期望 88 (缺席的两位不该被当成 0 分)", d.Score)
		}
		if d.Samples != 1 || d.Trimmed {
			t.Errorf("单样本维度必须自述 samples=1 trimmed=false, 实得 %+v", d)
		}
		return
	}
	t.Error("冷门维度丢了")
}

// 分歧大 (极差 >= 25) 的维度要在 summary 上标出来, 共识度随平均极差下降。
func TestReduceTrimmedMean_分歧标注与共识度(t *testing.T) {
	outs := []string{
		rvOut(t, reviewDoc{Overall: 60, Dimensions: []reviewDim{{Dimension: "plot", Score: 90, Summary: "很好"}}}),
		rvOut(t, reviewDoc{Overall: 60, Dimensions: []reviewDim{{Dimension: "plot", Score: 50}}}),
		rvOut(t, reviewDoc{Overall: 60, Dimensions: []reviewDim{{Dimension: "plot", Score: 55}}}),
	}
	got := parseReview(t, reduceTrimmedMeanResult(outs, 0).Output)
	if !strings.Contains(got.Dimensions[0].Summary, "评审分歧大") {
		t.Errorf("极差 40 应标分歧大, 实得 %q", got.Dimensions[0].Summary)
	}
	if got.Consensus > 0.61 || got.Consensus < 0.59 {
		t.Errorf("共识度 = %v, 期望 1-40/100 = 0.6", got.Consensus)
	}
}

// 样本不足**拒绝出数** (默认下限 3): 剔两端后为空时截尾均值退化成普通均值,
// 抗离群这个它唯一的作用消失, 而数字看起来完全正常。
func TestReduceTrimmedMean_样本不足拒绝出数(t *testing.T) {
	two := []string{
		rvOut(t, reviewDoc{Overall: 90, Dimensions: []reviewDim{{Dimension: "plot", Score: 90}}}),
		rvOut(t, reviewDoc{Overall: 10, Dimensions: []reviewDim{{Dimension: "plot", Score: 10}}}),
	}
	res := reduceTrimmedMeanResult(two, 0)
	if res.Status != NodeStatusFailed {
		t.Fatalf("2 个样本应拒绝出数 (默认下限 3), 实得 %s / %s", res.Status, res.Output)
	}
	if !strings.Contains(res.Err, "min_samples=3") {
		t.Errorf("失败原因应点名下限: %s", res.Err)
	}
	// 图作者显式写小下限时允许出数, 但产出必须自述 trimmed=false (责任转移给作者)。
	res2 := reduceTrimmedMeanResult(two, 2)
	if res2.Status != NodeStatusCompleted {
		t.Fatalf("显式 min_samples=2 应出数: %s", res2.Err)
	}
	got := parseReview(t, res2.Output)
	if got.Trimmed || got.Dimensions[0].Trimmed {
		t.Error("2 个样本没做截尾, trimmed 必须为 false")
	}
	if got.Dimensions[0].Score != 50 {
		t.Errorf("退化为普通均值 = %v, 期望 50", got.Dimensions[0].Score)
	}
}

// 截尾窗口逐档对齐上游 ByzantineFuser(0.34): N=3 取中位, N=4 取中间两个, N=6 剔两端各 2。
func TestReduceTrimmedMean_截尾窗口对齐上游(t *testing.T) {
	cases := []struct {
		vals    []float64
		want    float64
		trimmed bool
	}{
		{vals: []float64{10}, want: 10, trimmed: false},
		{vals: []float64{10, 90}, want: 50, trimmed: false},
		{vals: []float64{10, 50, 90}, want: 50, trimmed: true},            // k=1 → 中位
		{vals: []float64{10, 40, 60, 90}, want: 50, trimmed: true},        // k=1 → 中间两个
		{vals: []float64{0, 10, 50, 90, 100}, want: 50, trimmed: true},    // k=1 → 三个
		{vals: []float64{0, 1, 40, 60, 99, 100}, want: 50, trimmed: true}, // k=2 → 中间两个
	}
	for _, c := range cases {
		got, trimmed := fuseTrimmedMean(c.vals)
		if got != c.want || trimmed != c.trimmed {
			t.Errorf("fuseTrimmedMean(%v) = (%v,%v), 期望 (%v,%v)", c.vals, got, trimmed, c.want, c.trimmed)
		}
	}
}

// ---------------------------------------------------------------------------
// 走完整节点生命周期 (与 concat/longest 同类: 零 LLM 但 hook/journal 一样不少)
// ---------------------------------------------------------------------------

func TestReduce新策略_零LLM但走完整生命周期(t *testing.T) {
	body := func(name string) string {
		return exOut(t, voteDoc{Nodes: []voteNode{{Name: name}}})
	}
	stub := newStub()
	stub.fn["src"] = func(int, NodeInput) NodeResult {
		// 三行 → 三个分片
		return NodeResult{Status: NodeStatusCompleted, Output: "甲\n乙\n丙"}
	}
	shardFn := func(_ int, in NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: body(in.Shard.Value)}
	}
	for i := 0; i < 3; i++ { // 分片 ID 是 <map>#<序号>, stub 按节点 ID 派发
		stub.fn[shardNodeID("m", i)] = shardFn
	}
	red := NodeSpec{ID: "r", Kind: NodeKindReduce, Agent: AgentSpec{Role: "fuser"},
		Reduce: &ReducePolicy{Strategy: ReduceVote}}
	mapNode := NodeSpec{ID: "m", Kind: NodeKindAgent, Agent: AgentSpec{Role: "extractor"}}
	mapNode.Kind = NodeKindMap
	mapNode.Map = &MapPolicy{MaxShards: 5}
	spec := mapSpec(mapNode, &red)

	bus := &recordBus{}
	j := NewMemoryJournal()
	e := fastEngine(stub, j)
	e.Hooks = bus
	res, err := e.Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Nodes["r"].Status != NodeStatusCompleted {
		t.Fatalf("reduce 结果 = %+v", res.Nodes["r"])
	}
	if stub.callCount("r") != 0 {
		t.Errorf("确定性策略不该调 runner, 实调 %d 次", stub.callCount("r"))
	}
	got := parseVote(t, res.Nodes["r"].Output)
	if got.Samples != 3 || len(got.Nodes) != 3 {
		t.Errorf("三个分片应各贡献一个节点: samples=%d nodes=%d", got.Samples, len(got.Nodes))
	}
	// 生命周期: hook pre/post + journal started/completed 一样不少。
	if _, ok := bus.find(ScopeNode, "pre", "r"); !ok {
		t.Error("reduce 节点缺 node pre hook")
	}
	ev, ok := bus.find(ScopeNode, "post", "r")
	if !ok {
		t.Fatal("reduce 节点缺 node post hook")
	}
	if evInt(t, ev.Payload, "shards") != 3 {
		t.Errorf("post 载荷 shards = %v, 期望 3", ev.Payload["shards"])
	}
	var completed bool
	for _, c := range findEvents(t, j, EvNodeCompleted) {
		if c.NodeID == "r" {
			completed = true
		}
	}
	if !completed {
		t.Error("reduce 节点缺 journal node.completed")
	}
}

// min_samples 声明在不做样本融合的策略上 = 死配置, 开图即拒。
func TestValidate_reduce新策略与样本下限(t *testing.T) {
	mk := func(pol *ReducePolicy) GraphSpec {
		m := NodeSpec{ID: "m", Kind: NodeKindMap, Agent: AgentSpec{Role: "w"}, Map: &MapPolicy{MaxShards: 3}}
		r := NodeSpec{ID: "r", Kind: NodeKindReduce, Agent: AgentSpec{Role: "w"}, Reduce: pol}
		return GraphSpec{Name: "g", Nodes: []NodeSpec{m, r}, Edges: []EdgeSpec{{From: "m", To: "r"}}}
	}
	for _, s := range []string{ReduceVote, ReduceTrimmedMean} {
		if err := mk(&ReducePolicy{Strategy: s, MinSamples: 2}).Validate(); err != nil {
			t.Errorf("%s 应接受 min_samples: %v", s, err)
		}
	}
	err := mk(&ReducePolicy{Strategy: ReduceConcat, MinSamples: 2}).Validate()
	if err == nil || !strings.Contains(err.Error(), "min_samples") {
		t.Errorf("concat 上的 min_samples 是死配置, 应拒: %v", err)
	}
	err = mk(&ReducePolicy{Strategy: ReduceVote, MinSamples: -1}).Validate()
	if err == nil {
		t.Error("负的 min_samples 应拒")
	}
	err = mk(&ReducePolicy{Strategy: "投票"}).Validate()
	if err == nil || !strings.Contains(err.Error(), ReduceTrimmedMean) {
		t.Errorf("未知策略的报错应列出全部合法值: %v", err)
	}
}
