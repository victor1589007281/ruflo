package learners

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- 测试脚手架 ---

// writeEvo 在临时 stateDir 下写出 rewards.jsonl 与 trajectories.json。
func writeEvo(t *testing.T, rewards []RewardRow, trajs []TrajectoryRow) string {
	t.Helper()
	state := t.TempDir()
	dir := filepath.Join(state, "evolution")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rewards {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "rewards.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(trajs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trajectories.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return state
}

// run3 造 3 个同形状同签名的成功 run (刚好够 awmMinSamples)。
func run3(t *testing.T) ([]RewardRow, []TrajectoryRow) {
	t.Helper()
	var rw []RewardRow
	var tr []TrajectoryRow
	base := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"run-a", "run-b", "run-c"} {
		rw = append(rw, RewardRow{TS: base.Add(time.Duration(i) * time.Hour).UnixMilli(),
			RunID: id, Source: "gate.compile", Value: 1, Weight: 1, Team: "t"})
		for j, stage := range []string{"design", "implement"} {
			tr = append(tr, TrajectoryRow{
				RunID: id, TeamName: "t", StageName: stage, Role: "coder",
				Objective: "给订单服务实现分布式事务补偿",
				Input:     "请设计并实现" + stage,
				Output:    strings.Repeat("这是一段足够长的真实产出内容, 用于通过导出的信息量下限。", 8),
				Success:   true,
				Timestamp: base.Add(time.Duration(i)*time.Hour + time.Duration(j)*time.Minute),
			})
		}
	}
	return rw, tr
}

// --- ScoreRuns ---

// 同 (source,node) 只取最新一条: 门禁"失败→修复→通过"若取均值会被旧失败拖回去。
func TestScoreRuns_同源同节点只认最新观测(t *testing.T) {
	rows := []RewardRow{
		{TS: 100, RunID: "r1", NodeID: "n", Source: "gate.test", Value: -1, Weight: 1},
		{TS: 200, RunID: "r1", NodeID: "n", Source: "gate.test", Value: 1, Weight: 1},
	}
	sc := ScoreRuns(rows)["r1"]
	if sc.Count != 1 {
		t.Fatalf("同源同节点应折成 1 条证据, got %d", sc.Count)
	}
	if sc.Score != 1 {
		t.Fatalf("应取最新的 +1 (修复后通过), got %.3f", sc.Score)
	}
}

// 权重优先用事件自带的 Weight; 缺失时按 source 回落, 且加权而非求和。
func TestScoreRuns_加权平均且回落权重表(t *testing.T) {
	rows := []RewardRow{
		{TS: 1, RunID: "r", Source: "gate.compile", Value: 1}, // Weight 缺失 → 1.0
		{TS: 2, RunID: "r", Source: "latency", Value: -1},     // Weight 缺失 → 0.2
	}
	sc := ScoreRuns(rows)["r"]
	want := (1.0*1 + 0.2*-1) / (1.0 + 0.2)
	if diff := sc.Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("加权均值应为 %.4f, got %.4f", want, sc.Score)
	}
	if sc.Score <= 0 {
		t.Error("确定性门禁权重 1.0 应压过 shaping 负项 0.2, 结果不该为负")
	}
}

// 节点级分数用于 GEPA 定位差节点。
func TestScoreRuns_节点级分数独立聚合(t *testing.T) {
	rows := []RewardRow{
		{TS: 1, RunID: "r", NodeID: "good", Source: "gate.content", Value: 0.8, Weight: 0.5},
		{TS: 2, RunID: "r", NodeID: "bad", Source: "gate.content", Value: -0.6, Weight: 0.5},
	}
	sc := ScoreRuns(rows)["r"]
	if sc.NodeScores["bad"] >= 0 || sc.NodeScores["good"] <= 0 {
		t.Fatalf("节点分数应各自独立: %v", sc.NodeScores)
	}
}

// --- 目标签名 ---

// 签名必须跨调用稳定 (同输入同输出), 否则样本量永远攒不够、归纳永不触发。
func TestObjectiveSignature_同输入恒同签名(t *testing.T) {
	const obj = "实现分布式事务的补偿机制"
	first := ObjectiveSignature(obj)
	if first == "" || first == "misc" {
		t.Fatalf("中文目标应产出非空签名, got %q", first)
	}
	for i := 0; i < 20; i++ {
		if got := ObjectiveSignature(obj); got != first {
			t.Fatalf("第 %d 次调用签名变了: %q → %q", i, first, got)
		}
	}
	if ObjectiveSignature("") != "misc" {
		t.Error("空目标应归 misc 而不是空串 (空串会和别的空签名混成一组)")
	}
	// ASCII 词的出现顺序不影响签名 (字典序), 这是文档明确承诺的那一半。
	if a, b := ObjectiveSignature("fix auth bug"), ObjectiveSignature("bug fix auth"); a != b {
		t.Errorf("ASCII 词序应无关: %q vs %q", a, b)
	}
	// 不同任务类别必须分得开, 否则所有 run 会挤进同一组。
	if a, b := ObjectiveSignature("实现分布式事务补偿"), ObjectiveSignature("撰写产品发布公告"); a == b {
		t.Errorf("不同类别的目标不该同签名: %q", a)
	}
}

// --- AWM 归纳 ---

// 无奖励证据的 run 不参与归纳: 否则归纳出的只是"最常见序列"这种统计噪声。
func TestInduceWorkflows_无奖励证据不归纳(t *testing.T) {
	_, tr := run3(t)
	state := writeEvo(t, nil, tr)
	res, err := InduceWorkflows(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Proposals) != 0 {
		t.Fatalf("无奖励时不该产草案, got %d", len(res.Proposals))
	}
	if len(res.Skipped) == 0 {
		t.Error("跳过必须给出原因, 否则无从判断是没数据还是判据没过")
	}
}

// 样本量不足 (2 < awmMinSamples=3) 不归纳。
func TestInduceWorkflows_样本不足不归纳(t *testing.T) {
	rw, tr := run3(t)
	// 只留前两个 run
	rw, tr = rw[:2], tr[:4]
	state := writeEvo(t, rw, tr)
	res, err := InduceWorkflows(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Proposals) != 0 {
		t.Fatalf("样本 2 < 3 不该产草案, got %d", len(res.Proposals))
	}
}

// 达标时产 proposed 态草案, 且带样本量/均分/参与 run 的证据。
func TestInduceWorkflows_达标产proposed草案带证据(t *testing.T) {
	rw, tr := run3(t)
	state := writeEvo(t, rw, tr)
	res, err := InduceWorkflows(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Proposals) != 1 {
		t.Fatalf("应产 1 份草案, got %d (skipped=%v)", len(res.Proposals), res.Skipped)
	}
	p := res.Proposals[0]
	if p.Status != "proposed" {
		t.Errorf("归纳只能产 proposed 态 (未过闸), got %q", p.Status)
	}
	if p.Samples != 3 || p.MeanScore < 0.9 || len(p.RunIDs) != 3 {
		t.Errorf("证据不完整: samples=%d mean=%.3f runs=%v", p.Samples, p.MeanScore, p.RunIDs)
	}
	if strings.Join(p.Nodes, ">") != "design>implement" {
		t.Errorf("阶段序列应按时间还原, got %v", p.Nodes)
	}
	// 幂等: 数据没变再跑一次不该重复写盘。
	res2, err := InduceWorkflows(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Proposals) != 0 {
		t.Errorf("同内容草案应幂等不重复写, 第二轮又产了 %d 份", len(res2.Proposals))
	}
	if got := len(ListProposals(state)); got != 1 {
		t.Errorf("磁盘上应只有 1 份草案, got %d", got)
	}
}

// 无 RunID 的老轨迹不得按 team 归并 —— 那会拼出一条不存在的超长序列。
func TestShapeRuns_丢弃无RunID的老轨迹(t *testing.T) {
	rows := []TrajectoryRow{
		{TeamName: "t", StageName: "a"},
		{TeamName: "t", StageName: "b"},
		{RunID: "r1", TeamName: "t", StageName: "c"},
	}
	shapes := ShapeRuns(rows)
	if len(shapes) != 1 || len(shapes[0].Nodes) != 1 {
		t.Fatalf("只应留下带 RunID 的那一条: %+v", shapes)
	}
}

// --- GEPA prompt 进化 ---

type fakeReflector struct {
	out string
	err error
}

func (f fakeReflector) SimpleComplete(_ context.Context, _, _ string) (string, error) {
	return f.out, f.err
}

func TestEvolvePrompt_四类拒绝(t *testing.T) {
	state := t.TempDir()
	base := PromptEvolveInput{
		Target: "development/implement", Current: "按 {objective} 实现代码。",
		MeanScore: -0.4, Samples: 3,
	}
	cases := []struct {
		name string
		in   PromptEvolveInput
		r    Reflector
		want string
	}{
		{"未注入反思器", base, nil, "未提供 Reflector"},
		{"无负奖励证据", func() PromptEvolveInput { c := base; c.MeanScore = 0.2; return c }(),
			fakeReflector{out: "x"}, "不为负"},
		{"负证据样本不足", func() PromptEvolveInput { c := base; c.Samples = 1; return c }(),
			fakeReflector{out: "x"}, "条负奖励证据"},
		{"产出回显原文", base, fakeReflector{out: "按 {objective} 实现代码。"}, "与原文一致"},
		{"产出为空", base, fakeReflector{out: "   "}, "为空"},
		{"越改越长", base, fakeReflector{out: "{objective}" + strings.Repeat("啰", gepaMaxPromptChars)}, "超过上限"},
		{"丢失占位符", base, fakeReflector{out: "实现代码, 要求更具体。"}, "丢失占位符"},
		{"反思调用失败", base, fakeReflector{err: errors.New("boom")}, "反思调用失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := EvolvePrompt(context.Background(), state, c.r, c.in)
			if err == nil {
				t.Fatalf("应拒绝但通过了")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("拒绝理由应含 %q, got %v", c.want, err)
			}
		})
	}
	// 没有任何草案被写盘 —— 拒绝路径不许留半成品。
	if got := len(ListProposals(state)); got != 0 {
		t.Errorf("全部拒绝的情况下不该有草案落盘, got %d", got)
	}
}

// 合法改写: 落 proposed 草案, 并把父版本记进谱系。
func TestEvolvePrompt_通过时落草案并记谱系(t *testing.T) {
	state := t.TempDir()
	in := PromptEvolveInput{Target: "development/implement",
		Current: "按 {objective} 实现代码。", MeanScore: -0.4, Samples: 3,
		Failures: []string{"产出没有编译通过"}}

	p1, err := EvolvePrompt(context.Background(), state,
		fakeReflector{out: "按 {objective} 实现代码, 必须先跑通编译。"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Status != "proposed" || p1.Parent != "" {
		t.Errorf("首版应为 proposed 且无父版本: status=%q parent=%q", p1.Status, p1.Parent)
	}
	p2, err := EvolvePrompt(context.Background(), state,
		fakeReflector{out: "按 {objective} 实现代码, 必须先跑通编译与单测。"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Parent != p1.ID {
		t.Errorf("第二版的 Parent 应指向第一版 %q, got %q", p1.ID, p2.Parent)
	}
}

// FindWeakNodes: 只报负分且负样本够的节点, 最差在前。
func TestFindWeakNodes_只报负分且按最差排序(t *testing.T) {
	state := writeEvo(t, []RewardRow{
		{TS: 1, RunID: "r1", NodeID: "ok", Source: "gate.content", Value: 0.9, Weight: 0.5},
		{TS: 2, RunID: "r1", NodeID: "bad", Source: "gate.content", Value: -0.9, Weight: 0.5},
		{TS: 3, RunID: "r2", NodeID: "bad", Source: "gate.content", Value: -0.5, Weight: 0.5},
		{TS: 4, RunID: "r3", NodeID: "mid", Source: "gate.content", Value: -0.2, Weight: 0.5},
	}, nil)
	weak := FindWeakNodes(state)
	if len(weak) != 1 || weak[0].Node != "bad" {
		t.Fatalf("只有 bad 满足 (负分 + 负样本≥2), got %+v", weak)
	}
}

// --- 权重导出 (e) ---

func TestExportSFT_过滤原因逐条可解释(t *testing.T) {
	base := time.Now()
	rw := []RewardRow{{TS: base.UnixMilli(), RunID: "hi", Source: "gate.compile", Value: 1, Weight: 1}}
	tr := []TrajectoryRow{
		{RunID: "hi", TeamName: "t", StageName: "s", Objective: "写点东西",
			Input: "in", Output: strings.Repeat("足够长的真实产出。", 30), Timestamp: base},
		// 无 RunID → 无法与奖励对齐
		{TeamName: "t", StageName: "s", Input: "in", Output: "out", Timestamp: base},
		// 有 RunID 但无奖励
		{RunID: "noreward", TeamName: "t", StageName: "s", Input: "in",
			Output: strings.Repeat("长产出。", 40), Timestamp: base},
	}
	state := writeEvo(t, rw, tr)
	res, err := ExportSFT(state, ExportConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 1 {
		t.Fatalf("只有 hi 该进数据集, got %d (filtered=%v)", res.Samples, res.Filtered)
	}
	if len(res.Filtered) < 2 {
		t.Errorf("每条丢弃都要有原因, got %v", res.Filtered)
	}
	if res.SFTPath == "" {
		t.Error("有样本时应落盘")
	}
	// 落盘内容必须是逐行 JSON
	data, err := os.ReadFile(res.SFTPath)
	if err != nil {
		t.Fatal(err)
	}
	var s SFTSample
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(data)), "\n")[0]), &s); err != nil {
		t.Fatalf("导出行不是合法 JSON: %v", err)
	}
	if s.Messages[0].Role != "system" {
		t.Error("首条消息应为 system 说明")
	}
}

// H13: 含 schema 回显的轨迹整条丢弃。
func TestExportSFT_丢弃schema回显(t *testing.T) {
	base := time.Now()
	state := writeEvo(t,
		[]RewardRow{{TS: base.UnixMilli(), RunID: "r", Source: "gate.compile", Value: 1, Weight: 1}},
		[]TrajectoryRow{{RunID: "r", StageName: "s", Input: "in",
			Output: `{"score": 0到100的整数, "issues": ["..."]}`, Timestamp: base}})
	res, err := ExportSFT(state, ExportConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 0 {
		t.Fatal("schema 回显的轨迹必须被丢弃")
	}
	found := false
	for k := range res.Filtered {
		if strings.Contains(k, "schema") {
			found = true
		}
	}
	if !found {
		t.Errorf("丢弃原因应指明 schema 回显, got %v", res.Filtered)
	}
}

// H5: 有损压缩只做确定性中段截断, 首尾各保护 protectTurns 轮, 并在 system 注明。
func TestExportSFT_中段截断保护首尾且注明(t *testing.T) {
	base := time.Now()
	var tr []TrajectoryRow
	for i := 0; i < 10; i++ {
		tr = append(tr, TrajectoryRow{RunID: "r", StageName: "s", Objective: "长任务",
			Input: "in", Output: strings.Repeat("长产出内容。", 10),
			Timestamp: base.Add(time.Duration(i) * time.Minute)})
	}
	state := writeEvo(t,
		[]RewardRow{{TS: base.UnixMilli(), RunID: "r", Source: "gate.compile", Value: 1, Weight: 1}}, tr)
	res, err := ExportSFT(state, ExportConfig{MaxTurnsPerSample: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 1 {
		t.Fatalf("应导出 1 条, got %d (%v)", res.Samples, res.Filtered)
	}
	data, _ := os.ReadFile(res.SFTPath)
	var s SFTSample
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Truncated {
		t.Error("超过上限应标记 Truncated")
	}
	if !strings.Contains(s.Messages[0].Content, "已省略") {
		t.Errorf("system 必须注明部分历史已省略 (否则模型学到的是断裂的对话): %q", s.Messages[0].Content)
	}
	// 保护首尾各 2 轮 + 1 条省略标记 + 1 条 system = 2*2*2+1+1
	if want := 2*protectTurns*2 + 2; len(s.Messages) != want {
		t.Errorf("消息数应为 %d (首尾各 %d 轮), got %d", want, protectTurns, len(s.Messages))
	}
}

// H10 组语义: 只在同 (签名, 阶段) 内配对, 且组内奖励差不足则丢弃。
func TestExportSFT_DPO组内配对且丢弃无信号组(t *testing.T) {
	base := time.Now()
	long := strings.Repeat("真实产出内容。", 30)
	rw := []RewardRow{
		{TS: base.UnixMilli(), RunID: "good", Source: "gate.compile", Value: 1, Weight: 1},
		{TS: base.UnixMilli(), RunID: "bad", Source: "gate.compile", Value: -1, Weight: 1},
		{TS: base.UnixMilli(), RunID: "same1", Source: "gate.compile", Value: 1, Weight: 1},
		{TS: base.UnixMilli(), RunID: "same2", Source: "gate.compile", Value: 1, Weight: 1},
	}
	tr := []TrajectoryRow{
		{RunID: "good", StageName: "impl", Objective: "实现登录鉴权模块", Input: "in", Output: long + "好", Timestamp: base},
		{RunID: "bad", StageName: "impl", Objective: "实现登录鉴权模块", Input: "in", Output: long + "坏", Timestamp: base},
		{RunID: "same1", StageName: "other", Objective: "整理项目文档结构", Input: "in", Output: long + "1", Timestamp: base},
		{RunID: "same2", StageName: "other", Objective: "整理项目文档结构", Input: "in", Output: long + "2", Timestamp: base},
	}
	state := writeEvo(t, rw, tr)
	res, err := ExportSFT(state, ExportConfig{IncludeDPO: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pairs != 1 {
		t.Fatalf("只有 impl 组有奖励差, other 组奖励全同应丢弃; got %d 对", res.Pairs)
	}
	data, _ := os.ReadFile(res.DPOPath)
	var p DPOPair
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.Chosen, "好") || !strings.HasSuffix(p.Rejected, "坏") {
		t.Errorf("chosen/rejected 方向错了: chosen=%q rejected=%q",
			p.Chosen[len(p.Chosen)-3:], p.Rejected[len(p.Rejected)-3:])
	}
	if !strings.Contains(p.Signature, "impl") {
		t.Errorf("配对签名应含阶段名, got %q", p.Signature)
	}
}

// --- 草案存储的路径安全 ---

// GetProposal 的 id 来自 agent 入参, 不得被 ../ 穿出草案目录。
func TestGetProposal_拒绝路径穿越(t *testing.T) {
	state := t.TempDir()
	if _, err := GetProposal(state, "../../etc/passwd"); err == nil {
		t.Fatal("路径穿越的 id 必须查不到 (走清单匹配而不是拼路径)")
	}
}

// SplitByTime + MeanRunScore: 实验前后对照的数据切分。
func TestSplitByTime_按时刻切分前后对照(t *testing.T) {
	rows := []RewardRow{
		{TS: 100, RunID: "old", Source: "episode", Value: -1, Weight: 0.3},
		{TS: 300, RunID: "new", Source: "episode", Value: 1, Weight: 0.3},
	}
	before, after := SplitByTime(rows, 200)
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("切分错误: before=%d after=%d", len(before), len(after))
	}
	bs, bn := MeanRunScore(before)
	as, an := MeanRunScore(after)
	if bn != 1 || an != 1 || bs >= 0 || as <= 0 {
		t.Errorf("前后均分方向错: before=%.2f(%d) after=%.2f(%d)", bs, bn, as, an)
	}
	if s, n := MeanRunScore(nil); n != 0 || s != 0 {
		t.Error("空输入应返回 (0,0) 而不是 NaN")
	}
}
