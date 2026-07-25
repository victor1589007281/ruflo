package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/trace"
)

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestRewardSourceWeights 守护"源可信度"排序: 确定性门禁 > 人工反馈 > LLM 评分 > episode 终态。
// 顺序错了整个加权聚合就失去意义 (LLM 打分会压倒真跑 go test 的结论)。
func TestRewardSourceWeights(t *testing.T) {
	compile := RewardSourceWeight(RewardSourceGateCompile)
	test := RewardSourceWeight(RewardSourceGateTest)
	content := RewardSourceWeight(RewardSourceGateContent)
	episode := RewardSourceWeight(RewardSourceEpisode)
	unknown := RewardSourceWeight("brand.new.source")

	if compile != test || compile != 1.0 {
		t.Errorf("确定性门禁应同为满权重 1.0, got compile=%v test=%v", compile, test)
	}
	if !(compile > content && content > episode) {
		t.Errorf("权重排序必须是 确定性门禁 > LLM 评分 > episode, got %v/%v/%v", compile, content, episode)
	}
	if unknown <= 0 || unknown >= compile {
		t.Errorf("未知源权重应 (0, 确定性) 之间, got %v", unknown)
	}
	// 大小写/空格不敏感 (rewards.jsonl 是外部可写的流水)
	if RewardSourceWeight(" GATE.COMPILE ") != compile {
		t.Error("source 匹配应大小写/空格不敏感")
	}
}

// TestRecordRewardFillsWeight 权重必须落盘 (格式即契约): 权重表将来会调,
// 已发生的奖励应保留当时的可信度; 显式给的 Weight 不被覆盖; value 越界被钳。
func TestRecordRewardFillsWeight(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	ee.RecordReward(RewardEvent{RunID: "r1", Source: RewardSourceGateTest, Value: 1.0})
	ee.RecordReward(RewardEvent{RunID: "r1", Source: RewardSourceEpisode, Value: 3.0, Weight: 0.77})

	data, err := os.ReadFile(filepath.Join(dir, "rewards.jsonl"))
	if err != nil {
		t.Fatalf("rewards.jsonl 未写入: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 条, got %d", len(lines))
	}
	var a, b RewardEvent
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &b); err != nil {
		t.Fatal(err)
	}
	if a.Weight != 1.0 {
		t.Errorf("gate.test 应落盘默认权重 1.0, got %v", a.Weight)
	}
	if b.Weight != 0.77 {
		t.Errorf("显式 Weight 不应被覆盖, got %v", b.Weight)
	}
	if b.Value != 1.0 {
		t.Errorf("value 应被钳到 [-1,1], got %v", b.Value)
	}
}

// TestAggregateRewardsWeighting 加权聚合的正确性:
// 分数 = Σ(w·v)/Σw, 且 run/team/node/源前缀过滤生效。
func TestAggregateRewardsWeighting(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	// run-1: 确定性门禁失败(-1, w=1.0) + LLM 内容分不错(+0.6, w=0.5) + episode 交付(+1, w=0.3)
	ee.RecordReward(RewardEvent{RunID: "run-1", Team: "tm", NodeID: "gate.compile", Source: RewardSourceGateCompile, Value: -1})
	ee.RecordReward(RewardEvent{RunID: "run-1", Team: "tm", NodeID: "write", Source: RewardSourceGateContent, Value: 0.6})
	ee.RecordReward(RewardEvent{RunID: "run-1", Team: "tm", Source: RewardSourceEpisode, Value: 1})
	// 另一个 run 的奖励绝不能混进来
	ee.RecordReward(RewardEvent{RunID: "run-2", Team: "tm", Source: RewardSourceEpisode, Value: -1})

	// 全 run 聚合: (1.0*-1 + 0.5*0.6 + 0.3*1) / (1.0+0.5+0.3) = -0.4/1.8
	agg := ee.AggregateRewards(RewardQuery{RunID: "run-1", Team: "tm"})
	if agg.Count != 3 {
		t.Fatalf("应纳入 3 条证据, got %d (%v)", agg.Count, agg.Sources)
	}
	want := (-1.0*1.0 + 0.5*0.6 + 0.3*1.0) / 1.8
	if !nearly(agg.Score, want) {
		t.Errorf("加权分 = %v, want %v", agg.Score, want)
	}
	if !nearly(agg.WeightSum, 1.8) {
		t.Errorf("权重和 = %v, want 1.8", agg.WeightSum)
	}

	// 若是简单算术平均会得到 (-1+0.6+1)/3 = 0.2 (正), 加权后必须为负:
	// 真跑 go build 的失败不能被两个软信号中和掉。
	if agg.Score >= 0 {
		t.Errorf("确定性门禁失败应压过软信号, 得到非负分 %v", agg.Score)
	}

	// 节点过滤: 只看 write 阶段 → 只剩内容门禁分
	if score, ok := ee.StageRewardScore("run-1", "tm", "write"); !ok || !nearly(score, 0.6) {
		t.Errorf("StageRewardScore(write) = %v ok=%v, want 0.6 true", score, ok)
	}
	// 没有奖励的阶段 = 无证据 (调用方必须回退二值)
	if score, ok := ee.StageRewardScore("run-1", "tm", "no-such-stage"); ok || score != 0 {
		t.Errorf("无证据阶段应返回 (0,false), got (%v,%v)", score, ok)
	}
	// team 不匹配 = 无证据
	if _, ok := ee.RunRewardScore("run-1", "other-team"); ok {
		t.Error("team 不匹配不应有证据")
	}
	// 空 RunID 拒绝聚合 (否则跨 run 混算)
	if _, ok := ee.RunRewardScore("", "tm"); ok {
		t.Error("空 RunID 必须拒绝聚合")
	}

	// 门禁类前缀过滤 (排除 episode): (1.0*-1 + 0.5*0.6)/1.5
	gate, ok := ee.GateRewardScore("run-1", "tm")
	if !ok {
		t.Fatal("门禁类奖励应有证据")
	}
	if wantGate := (-1.0 + 0.3) / 1.5; !nearly(gate, wantGate) {
		t.Errorf("GateRewardScore = %v, want %v", gate, wantGate)
	}
}

// TestAggregateRewardsLatestPerSourceNode 同 (source,node) 只取最新一条:
// "门禁失败 → 自动修复 → 门禁通过" 的最终结论必须胜出, 否则修好了还被旧失败拖回去。
func TestAggregateRewardsLatestPerSourceNode(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	ee.RecordReward(RewardEvent{TS: 100, RunID: "r", NodeID: "gate.compile", Source: RewardSourceGateCompile, Value: -1})
	ee.RecordReward(RewardEvent{TS: 200, RunID: "r", NodeID: "gate.compile", Source: RewardSourceGateCompile, Value: -1})
	ee.RecordReward(RewardEvent{TS: 300, RunID: "r", NodeID: "gate.compile", Source: RewardSourceGateCompile, Value: 1})

	agg := ee.AggregateRewards(RewardQuery{RunID: "r"})
	if agg.Count != 1 {
		t.Fatalf("同源同节点只应保留最新 1 条, got %d", agg.Count)
	}
	if !nearly(agg.Score, 1.0) {
		t.Errorf("应取最新的通过结论 +1, got %v", agg.Score)
	}
}

// TestAggregateRewardsTailWindow 大文件只扫尾部窗口: 不整文件 load。
// 窗口起点落在行中间时那半行必须被丢弃, 不能解析出错误事件。
func TestAggregateRewardsTailWindow(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	// 先写 400 条旧 run 的奖励把文件撑大 (每条 ~200B)
	pad := strings.Repeat("x", 150)
	for i := 0; i < 400; i++ {
		ee.RecordReward(RewardEvent{RunID: "old-run", Source: RewardSourceEpisode, Value: -1, Raw: pad})
	}
	ee.RecordReward(RewardEvent{RunID: "new-run", Source: RewardSourceGateTest, Value: 1})

	st, err := os.Stat(filepath.Join(dir, "rewards.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() < 4096 {
		t.Fatalf("测试前提不成立: 文件太小 (%d B)", st.Size())
	}

	// 只给 2KiB 窗口: 最新那条一定在窗口内
	agg := ee.AggregateRewards(RewardQuery{RunID: "new-run", MaxScanBytes: 2048})
	if agg.Count != 1 || !nearly(agg.Score, 1.0) {
		t.Errorf("尾部窗口应命中最新事件, got count=%d score=%v", agg.Count, agg.Score)
	}
	// 窗口内的旧 run 事件数受窗口限制, 但解析不能出错 (半行被丢弃)
	old := ee.AggregateRewards(RewardQuery{RunID: "old-run", MaxScanBytes: 2048})
	if old.Count == 0 {
		t.Error("窗口内应仍能聚合到 old-run 的部分事件")
	}
	if !nearly(old.Score, -1.0) {
		t.Errorf("old-run 全为 -1, 聚合分应为 -1, got %v", old.Score)
	}
	// 事件数上限生效
	capped := ee.AggregateRewards(RewardQuery{RunID: "old-run", MaxEvents: 1, MaxScanBytes: 1 << 20})
	if capped.Count != 1 {
		t.Errorf("MaxEvents=1 应只纳入 1 条, got %d", capped.Count)
	}
}

// TestAggregateRewardsRobustness 脏行/缺文件/nil 引擎都不能炸, 且不产生假证据。
func TestAggregateRewardsRobustness(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	// 无文件 → 无证据
	if _, ok := ee.RunRewardScore("r", ""); ok {
		t.Error("无 rewards.jsonl 时不应有证据")
	}
	path := filepath.Join(dir, "rewards.jsonl")
	if err := os.WriteFile(path, []byte("not json\n{\"run_id\":\"r\",\"source\":\"gate.test\",\"value\":1}\n\n{broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agg := ee.AggregateRewards(RewardQuery{RunID: "r"})
	if agg.Count != 1 || !nearly(agg.Score, 1.0) {
		t.Errorf("脏行应被跳过而不毁掉整次聚合, got count=%d score=%v", agg.Count, agg.Score)
	}
	// 老数据没有 weight 字段 → 按 source 取默认权重
	if !nearly(agg.WeightSum, RewardSourceWeight(RewardSourceGateTest)) {
		t.Errorf("缺 weight 的老数据应按 source 补默认权重, got %v", agg.WeightSum)
	}

	var nilEE *EvolutionEngine
	if _, ok := nilEE.RunRewardScore("r", ""); ok {
		t.Error("nil 引擎不应有证据")
	}
	nilEE.RecordFeedbackScored("x", 1)
	nilEE.RecordBatchFeedbackScored([]string{"x"}, 1)
	nilEE.RecordInjectionScored([]string{"x"}, "t", "tm", "coder", 1)
	nilEE.UpdateBaselineScored(1)
}

// TestScoredFeedbackEquivalence 加权分路径在端点上与旧二值路径完全等价 (向后兼容),
// 中间分给出部分信用。这是"保持旧调用方不破"的核心断言。
func TestScoredFeedbackEquivalence(t *testing.T) {
	newEngine := func() *EvolutionEngine {
		ee := NewEvolutionEngine(t.TempDir(), nil)
		ee.experiences = append(ee.experiences, &Experience{ID: "e1", Quality: 0.5})
		return ee
	}

	// 成功: 二值 true ≡ score +1
	a, b := newEngine(), newEngine()
	a.RecordFeedback("e1", true)
	b.RecordFeedbackScored("e1", 1.0)
	if !nearly(a.experiences[0].Quality, b.experiences[0].Quality) {
		t.Errorf("success=true 应等价于 score=+1: %v vs %v", a.experiences[0].Quality, b.experiences[0].Quality)
	}
	if a.experiences[0].SuccessCount != 1 || b.experiences[0].SuccessCount != 1 {
		t.Error("正分应计入 SuccessCount")
	}

	// 失败: 二值 false ≡ score -1
	c, d := newEngine(), newEngine()
	c.RecordFeedback("e1", false)
	d.RecordFeedbackScored("e1", -1.0)
	if !nearly(c.experiences[0].Quality, d.experiences[0].Quality) {
		t.Errorf("success=false 应等价于 score=-1: %v vs %v", c.experiences[0].Quality, d.experiences[0].Quality)
	}
	if c.experiences[0].SuccessCount != 0 || d.experiences[0].SuccessCount != 0 {
		t.Error("负分不应计入 SuccessCount")
	}
	if c.experiences[0].Quality >= 0.5 {
		t.Errorf("失败应压低质量, got %v", c.experiences[0].Quality)
	}

	// 中间分: 0.6 的质量提升应严格介于失败与满分之间
	e := newEngine()
	e.RecordFeedbackScored("e1", 0.6)
	if !(e.experiences[0].Quality > c.experiences[0].Quality && e.experiences[0].Quality < a.experiences[0].Quality) {
		t.Errorf("中间分应给部分信用: fail=%v mid=%v ok=%v",
			c.experiences[0].Quality, e.experiences[0].Quality, a.experiences[0].Quality)
	}
	// UsageCount 计数不受分值影响
	if e.experiences[0].UsageCount != 1 {
		t.Errorf("UsageCount 应为 1, got %d", e.experiences[0].UsageCount)
	}
}

// TestUpdateBaselineScoredEquivalence 基线 EMA: ±1 端点与旧二值实现逐位一致, 中间分线性。
func TestUpdateBaselineScoredEquivalence(t *testing.T) {
	mk := func() *EvolutionEngine { return NewEvolutionEngine(t.TempDir(), nil) }

	a, b := mk(), mk()
	a.UpdateBaseline(true)
	b.UpdateBaselineScored(1.0)
	if !nearly(a.baselineSuccess, b.baselineSuccess) || !nearly(a.baselineSuccess, 0.1) {
		t.Errorf("true ≡ +1 且应为 0.1: %v vs %v", a.baselineSuccess, b.baselineSuccess)
	}
	c, d := mk(), mk()
	c.UpdateBaseline(false)
	d.UpdateBaselineScored(-1.0)
	if !nearly(c.baselineSuccess, d.baselineSuccess) || !nearly(c.baselineSuccess, 0) {
		t.Errorf("false ≡ -1 且应为 0: %v vs %v", c.baselineSuccess, d.baselineSuccess)
	}
	e := mk()
	e.UpdateBaselineScored(0) // 中位分 → 目标 0.5
	if !nearly(e.baselineSuccess, 0.05) {
		t.Errorf("score=0 应把基线拉向 0.5 (EMA 一步 0.05), got %v", e.baselineSuccess)
	}
	if e.baselineTotal != 1 {
		t.Errorf("baselineTotal 应递增, got %d", e.baselineTotal)
	}
}

// TestRecordInjectionScoredKeepsScore 注入记录保留连续分 (不在这层压回 bool),
// 同时 Success 口径不变 (score>0), Uplift 指标语义不破。
func TestRecordInjectionScoredKeepsScore(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	ee.experiences = append(ee.experiences, &Experience{ID: "e1", Quality: 0.5})
	ee.RecordInjectionScored([]string{"e1"}, "stage-a", "tm", "coder", 0.4)
	ee.RecordInjection([]string{"e1"}, "stage-b", "tm", "coder", false)

	if len(ee.injections) != 2 {
		t.Fatalf("应有 2 条注入记录, got %d", len(ee.injections))
	}
	if !ee.injections[0].Success || !nearly(ee.injections[0].Score, 0.4) {
		t.Errorf("正分注入: Success=true Score=0.4, got %+v", ee.injections[0])
	}
	if ee.injections[1].Success || !nearly(ee.injections[1].Score, -1) {
		t.Errorf("二值 false 应记为 Score=-1 Success=false, got %+v", ee.injections[1])
	}
	if ee.experiences[0].InjectionCount != 2 || ee.experiences[0].InjectionSuccess != 1 {
		t.Errorf("注入统计错: count=%d success=%d",
			ee.experiences[0].InjectionCount, ee.experiences[0].InjectionSuccess)
	}
}

// TestGlobalGatesEmitRewards 守护 design/03 §4.2 里价值排第 1 的信号源真的进了总线:
// runGlobalCompileGate / runGlobalTestGate 真跑 go build / go test, 结论必须落 rewards.jsonl
// (通过 +1 / 失败 -1, 满权重), 且被跳过的门禁不得伪造证据。
func TestGlobalGatesEmitRewards(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("无 go 工具链, 跳过真门禁测试")
	}
	writeMod := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		files["go.mod"] = "module gatetest\n\ngo 1.26\n"
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	// readRewards 读出某 run 的奖励事件 (按写入顺序)。
	readRewards := func(t *testing.T, dir, runID string) []RewardEvent {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "rewards.jsonl"))
		if err != nil {
			return nil
		}
		var out []RewardEvent
		for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var ev RewardEvent
			if json.Unmarshal([]byte(ln), &ev) == nil && ev.RunID == runID {
				out = append(out, ev)
			}
		}
		return out
	}

	t.Run("compile 通过发正奖励", func(t *testing.T) {
		evoDir := t.TempDir()
		ptm := &ProductionTeamManager{evolution: NewEvolutionEngine(evoDir, nil)}
		team := &ProductionTeam{Name: "tm", Cwd: writeMod(t, map[string]string{
			"main.go": "package main\n\nfunc main() {}\n",
		})}
		ctx := trace.With(context.Background(), trace.IDs{RunID: "run-ok"})
		if msg := ptm.runGlobalCompileGate(ctx, team); msg != "" {
			t.Fatalf("门禁应通过, got %s", msg)
		}
		evs := readRewards(t, evoDir, "run-ok")
		if len(evs) != 1 {
			t.Fatalf("应发 1 条奖励, got %d", len(evs))
		}
		ev := evs[0]
		if ev.Source != RewardSourceGateCompile || ev.Value != 1 || ev.Team != "tm" || ev.NodeID != RewardSourceGateCompile {
			t.Errorf("奖励字段不对: %+v", ev)
		}
		if ev.Weight != 1.0 {
			t.Errorf("确定性门禁应满权重, got %v", ev.Weight)
		}
	})

	t.Run("compile 失败发负奖励且带错误摘要", func(t *testing.T) {
		evoDir := t.TempDir()
		ptm := &ProductionTeamManager{evolution: NewEvolutionEngine(evoDir, nil)}
		team := &ProductionTeam{Name: "tm", Cwd: writeMod(t, map[string]string{
			"main.go": "package main\n\nfunc main() { thisDoesNotExist() }\n",
		})}
		ctx := trace.With(context.Background(), trace.IDs{RunID: "run-bad"})
		if msg := ptm.runGlobalCompileGate(ctx, team); msg == "" {
			t.Fatal("门禁应失败")
		}
		evs := readRewards(t, evoDir, "run-bad")
		if len(evs) != 1 || evs[0].Value != -1 {
			t.Fatalf("应发 1 条 -1 奖励, got %+v", evs)
		}
		if raw, _ := evs[0].Raw.(string); !strings.Contains(raw, "go build") {
			t.Errorf("Raw 应带门禁错误摘要供排障, got %v", evs[0].Raw)
		}
		// 聚合视图: 加权分为 -1
		if score, ok := ptm.evolution.GateRewardScore("run-bad", "tm"); !ok || !nearly(score, -1) {
			t.Errorf("GateRewardScore = %v ok=%v, want -1 true", score, ok)
		}
	})

	t.Run("跳过的门禁不发奖励", func(t *testing.T) {
		evoDir := t.TempDir()
		ptm := &ProductionTeamManager{evolution: NewEvolutionEngine(evoDir, nil)}
		ctx := trace.With(context.Background(), trace.IDs{RunID: "run-skip"})
		// 无 cwd
		_ = ptm.runGlobalCompileGate(ctx, &ProductionTeam{Name: "tm"})
		// 有 cwd 但不是 Go 模块 (无 go.mod)
		_ = ptm.runGlobalCompileGate(ctx, &ProductionTeam{Name: "tm", Cwd: t.TempDir()})
		if evs := readRewards(t, evoDir, "run-skip"); len(evs) != 0 {
			t.Errorf("没跑过的门禁不是证据, 却发了 %d 条奖励", len(evs))
		}
	})

	t.Run("test 门禁通过与失败", func(t *testing.T) {
		evoDir := t.TempDir()
		ptm := &ProductionTeamManager{evolution: NewEvolutionEngine(evoDir, nil)}
		dir := writeMod(t, map[string]string{
			"main.go":      "package main\n\nfunc main() {}\n",
			"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n",
		})
		team := &ProductionTeam{Name: "tm", Cwd: dir}
		ctx := trace.With(context.Background(), trace.IDs{RunID: "run-test"})
		if msg := ptm.runGlobalTestGate(ctx, team); msg != "" {
			t.Fatalf("测试门禁应通过, got %s", msg)
		}
		// 改成失败的测试, 再跑一次: 同 (source,node) 只取最新 ⇒ 聚合分翻负
		if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
			[]byte("package main\n\nimport \"testing\"\n\nfunc TestBad(t *testing.T) { t.Fatal(\"boom\") }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if msg := ptm.runGlobalTestGate(ctx, team); msg == "" {
			t.Fatal("测试门禁应失败")
		}
		evs := readRewards(t, evoDir, "run-test")
		if len(evs) != 2 || evs[0].Value != 1 || evs[1].Value != -1 {
			t.Fatalf("应有 [+1,-1] 两条 gate.test 奖励, got %+v", evs)
		}
		if evs[0].Source != RewardSourceGateTest {
			t.Errorf("source 应为 %s, got %s", RewardSourceGateTest, evs[0].Source)
		}
		if score, ok := ptm.evolution.GateRewardScore("run-test", "tm"); !ok || !nearly(score, -1) {
			t.Errorf("最新结论应胜出 (=-1), got %v ok=%v", score, ok)
		}
	})
}

// TestStageFeedbackScoreFallback 守护学习反馈的"有证据用加权分 / 无证据回退二值"。
// 回退这一条是硬要求: 没接奖励源的工作流若被喂 0 分, 成功与失败会被反馈成同一个值。
func TestStageFeedbackScoreFallback(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	// 只有 write 阶段有内容门禁证据 (60/100 → 0.2); 另有一条 run 级门禁失败奖励
	ee.RecordReward(RewardEvent{RunID: "run-1", Team: "tm", NodeID: "write", Source: RewardSourceGateContent, Value: 0.2})
	ee.RecordReward(RewardEvent{RunID: "run-1", Team: "tm", NodeID: RewardSourceGateCompile, Source: RewardSourceGateCompile, Value: -1})

	// 有同节点证据 → 用加权分 (而不是 stage 二值 +1)
	if got := stageFeedbackScore(ee, "run-1", "tm", "write", true); !nearly(got, 0.2) {
		t.Errorf("有证据应用加权分 0.2, got %v", got)
	}
	// 无同节点证据 → 回退二值; 且不得被 run 级的门禁失败奖励污染
	// (否则"修复阶段"会被它正要修的失败倒打一耙)
	if got := stageFeedbackScore(ee, "run-1", "tm", "remediation-compile-1", true); !nearly(got, 1) {
		t.Errorf("无同节点证据应回退 +1, got %v", got)
	}
	if got := stageFeedbackScore(ee, "run-1", "tm", "research", false); !nearly(got, -1) {
		t.Errorf("失败阶段无证据应回退 -1, got %v", got)
	}
	// 未接奖励源 (空 run / 别的团队) → 回退二值
	if got := stageFeedbackScore(ee, "", "tm", "write", true); !nearly(got, 1) {
		t.Errorf("无 RunID 应回退 +1, got %v", got)
	}
	if got := stageFeedbackScore(ee, "run-1", "other", "write", false); !nearly(got, -1) {
		t.Errorf("团队不匹配应回退 -1, got %v", got)
	}
	// nil 引擎 (未启用进化) 不能 panic, 回退二值
	var nilEE *EvolutionEngine
	if got := stageFeedbackScore(nilEE, "run-1", "tm", "write", true); !nearly(got, 1) {
		t.Errorf("nil 引擎应回退 +1, got %v", got)
	}
}

// TestSkillDistillAllowed 技能提炼闸 (design/03 §4.6): 无奖励证据放行 (回退原行为),
// 有证据时负分拦下。
func TestSkillDistillAllowed(t *testing.T) {
	if !skillDistillAllowed(0, false) {
		t.Error("无证据必须放行, 否则未接奖励源的工作流永不产技能")
	}
	if !skillDistillAllowed(-1, false) {
		t.Error("无证据时分值不该被看 (hasEvidence=false)")
	}
	if !skillDistillAllowed(0, true) {
		t.Error("0 分不应拦 (只拦负分)")
	}
	if skillDistillAllowed(-0.01, true) {
		t.Error("有证据且为负必须拦下")
	}
	if !skillDistillAllowed(0.8, true) {
		t.Error("正分应放行")
	}
}
