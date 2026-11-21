package harnesspro

// orchestrator_test.go — 离线端到端: 桩 LLM 驱动完整进化 (出卷→2轮→三档),
// 验证 DAG 全节点走通、冻结卷只出一次、选点数学 (地板/帕累托/三档) 正确。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// stubLLM 桩 LLM: 按提示词角色返回确定性产出。
type stubLLM struct {
	mu    sync.Mutex
	calls map[string]int
}

func newStubLLM() *stubLLM { return &stubLLM{calls: map[string]int{}} }

func (s *stubLLM) Model() string { return "stub-model" }

func (s *stubLLM) Complete(_ context.Context, system, user string) (string, Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[system]++
	tok := 50 + len(system)/4 + len(user)/8
	switch {
	case strings.Contains(system, "测试合成专家"):
		return s.synthTests(user), Usage{Input: len(user) / 4, Output: tok}, nil
	case strings.Contains(system, "轨迹诊断专家"):
		return `{"candidate_id":"","problems":[{"problem":"输出未遵守长度纪律","impact_pass":"低","impact_cost":"中","evidence":"trace t01 rollout#1 输出冗长"}]}`, Usage{Input: len(user) / 4, Output: tok}, nil
	case strings.Contains(system, "harness 改进规划师"):
		return s.plan(user), Usage{Input: len(user) / 4, Output: tok}, nil
	case strings.Contains(system, "harness 编辑"):
		return s.edit(user), Usage{Input: len(user) / 4, Output: tok}, nil
	}
	// 执行体 (harness 指令作 system): 按任务 id 给出确定输出。
	return s.execBody(user), Usage{Input: len(user) / 4, Output: tok}, nil
}

// synthTests 出卷: 给 json-extract-errors 出可判定的强断言, 其余出通用弱断言。
// 真实桩形态: 测试 id 必须可被 selfcheck 归属回任务 —— 用"任务 id 前缀 + -tNN" 形态,
// 与真实 LLM 的出卷纪律一致 (TestID 含任务 id 前缀是论文产物 schema 的要求)。
func (s *stubLLM) synthTests(user string) string {
	taskID := extractBetween(user, "【任务 id】", "【任务描述")
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		taskID = "t01" // 兜底 (测试直接调单分片时)
	}
	var strong string
	if strings.Contains(user, "json-extract-errors") {
		strong = fmt.Sprintf(`{"test_id":"%s-t00","oracle_strength":2,"rationale":"任务要求 JSON 数组","assert":{"kind":"json_parse"}},`, taskID)
	}
	return fmt.Sprintf(`{"tests":[%s
		{"test_id":"%s-t01","oracle_strength":3,"rationale":"输出非空","assert":{"kind":"len_range","param2":"1","param3":"4000"}},
		{"test_id":"%s-t02","oracle_strength":4,"rationale":"输出有内容","assert":{"kind":"word_range","param2":"1","param3":"400"}},
		{"test_id":"%s-t03","oracle_strength":4,"rationale":"无违禁词","assert":{"kind":"not_contains","param":"IMPOSSIBLE_MARK"}},
		{"test_id":"%s-t04","oracle_strength":4,"rationale":"包含回车","assert":{"kind":"regex","param":"(?s).+"}},
		{"test_id":"%s-t05","oracle_strength":4,"rationale":"有字符","assert":{"kind":"contains","param":"e"}},
		{"test_id":"%s-t06","oracle_strength":4,"rationale":"长度下界","assert":{"kind":"len_range","param2":"2","param3":"4000"}},
		{"test_id":"%s-t07","oracle_strength":4,"rationale":"词数下界","assert":{"kind":"word_range","param2":"1","param3":"300"}},
		{"test_id":"%s-t08","oracle_strength":4,"rationale":"不重复违禁","assert":{"kind":"not_contains","param":"XYZZY"}},
		{"test_id":"%s-t09","oracle_strength":4,"rationale":"开头非空","assert":{"kind":"regex","param":"^\\s*\\S"}},
		{"test_id":"%s-t10","oracle_strength":4,"rationale":"结尾有字符","assert":{"kind":"regex","param":"\\S\\s*$"}}
	]}`, strong,
		taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID)
}

// plan 出案: 三份计划 (I1 短指令 / I3 长指令), 回显父 harness。
func (s *stubLLM) plan(user string) string {
	parent := extractBetween(user, "【本候选 harness 指令全文】", "【结构化诊断")
	if parent == "" {
		parent = "PARENT-MISSING"
	}
	return fmt.Sprintf(`{"plan_id":"","goal":"g","changes":[{"component":"body","change":"tighten","expected_pass_effect":"+","expected_cost_effect":"-"}],"parent_id":"","parent_harness":%q}`, strings.TrimSpace(parent))
}

func (s *stubLLM) edit(user string) string {
	plan := extractBetween(user, "【改进计划】", "请把计划落地")
	parent := extractBetween(plan, `"parent_harness":`, `,"parent_id"`)
	parent = strings.Trim(parent, ` "`)
	newH := parent + "\n## 进化增量\n遵守输出纪律, 先想后写, 一次成型。"
	return fmt.Sprintf(`{"new_harness":%q,"commit_message":"stub edit"}`, newH)
}

// execBody 执行体: h0 朴素应答; 含"进化增量"的 harness 输出更短更合规。
func (s *stubLLM) execBody(user string) string {
	if strings.Contains(user, "json-extract-errors") {
		return `[{"ts":"2026-11-10T08:00:03Z","msg":"connection refused by upstream db1"},{"ts":"2026-11-10T08:00:09Z","msg":"timeout reading shard 7"}]`
	}
	return "the quick brown fox jumps over the lazy dog. summary result 42 YES 2026-11-16 done."
}

func extractBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return s
	}
	return s[:j]
}

// TestOrchestratorEndToEnd 桩驱动完整进化: 机制全链路走通。
func TestOrchestratorEndToEnd(t *testing.T) {
	cfg := Config{Epochs: 2, K: 2, TrainN: 4, Pool: DefaultPool(), VersionDir: filepath.Join(t.TempDir(), "versions")}
	orch, err := NewOrchestrator(cfg, newStubLLM())
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	// 测试桩只给训练集出卷; 地板评测要扫全验证集 —— 预冻结合成测试卷补齐,
	// 模拟真实部署里"验证集也走过一次出卷图"的形态.
	trainSel := SelectTrainTasks(cfg.Pool, cfg.TrainN)
	trainSet := map[string]bool{}
	for _, tt := range trainSel {
		trainSet[tt.ID] = true
	}
	for _, task := range cfg.Pool {
		if trainSet[task.ID] {
			continue
		}
		stubTests := make([]FrozenTest, 0, 2)
		for i, a := range []Assertion{
			{Kind: AssertLenRange, Param2: json.RawMessage(`"1"`), Param3: json.RawMessage(`"8000"`)},
			{Kind: AssertNotContains, Param: "IMPOSSIBLE_MARKER_NEVER_APPEARS"},
		} {
			stubTests = append(stubTests, FrozenTest{
				TestID: fmt.Sprintf("%s-v%02d", task.ID, i+1), OracleStrength: 3,
				Rationale: "测试预冻结 (验证集)", Assert: a,
			})
		}
		if err := orch.store.FreezeTests(task.ID, stubTests); err != nil {
			t.Fatalf("预冻结 %s: %v", task.ID, err)
		}
	}
	report, err := orch.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Epochs) != 2 {
		t.Fatalf("want 2 epochs, got %d", len(report.Epochs))
	}
	if report.Floor < 0 || report.Floor > 1 {
		t.Fatalf("floor out of range: %v", report.Floor)
	}
	for _, ep := range report.Epochs {
		if len(ep.Survivors) == 0 {
			t.Fatalf("epoch %d 无幸存者", ep.Epoch)
		}
		if len(ep.Frontier) == 0 {
			t.Fatalf("epoch %d 前沿为空", ep.Epoch)
		}
	}
	if len(report.Final) == 0 {
		t.Fatal("final survivors 为空")
	}
	if report.TotalTokens <= 0 {
		t.Fatal("总 token 未累计")
	}
	// 新 harness 必须已注册 (edit 节点写入 store)。
	found := false
	for _, h := range report.Harnesses {
		if strings.Contains(h.Instructions, "进化增量") {
			found = true
		}
		if h.ID == "" || h.Instructions == "" {
			t.Fatalf("harness 缺字段: %+v", h)
		}
	}
	if !found {
		t.Fatal("未见进化后的 harness (edit 节点未生效)")
	}
	// 训练集选题: 4 题且属于池子。
	if len(report.TrainIDs) != 4 {
		t.Fatalf("train ids = %v", report.TrainIDs)
	}
	if report.TaskSet.Name == "" || report.TaskSet.Hash == "" || report.FloorEval == nil {
		t.Fatalf("报告缺任务集/地板明细: taskset=%+v floor_eval=%v", report.TaskSet, report.FloorEval != nil)
	}
	registry, err := NewVersionRegistry(cfg.VersionDir)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := registry.List(report.RunID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != len(report.Harnesses) {
		t.Fatalf("登记版本 %d 与报告 harness %d 不一致", len(versions), len(report.Harnesses))
	}
	var sawFloor, sawTrain, sawEval bool
	for _, v := range versions {
		if v.VersionID == "" || v.ContentID == "" || v.RunID != report.RunID || v.TaskSet.Hash != report.TaskSet.Hash {
			t.Fatalf("版本坐标不完整: %+v", v)
		}
		for _, obs := range v.Observations {
			switch obs.Phase {
			case "floor":
				sawFloor = true
			case "train_rollout":
				sawTrain = true
			case "eval":
				sawEval = true
			}
		}
	}
	if !sawFloor || !sawTrain || !sawEval {
		t.Fatalf("版本观测缺阶段: floor=%v train=%v eval=%v", sawFloor, sawTrain, sawEval)
	}
}

// TestTestSynthFrozenOnce 冻结语义: 重复 FreezeTests 拒绝。
func TestTestSynthFrozenOnce(t *testing.T) {
	s := NewStore(DefaultPool())
	if err := s.FreezeTests("t1", []FrozenTest{{TestID: "t1-t01"}}); err != nil {
		t.Fatalf("首次冻结应成功: %v", err)
	}
	if err := s.FreezeTests("t1", []FrozenTest{{TestID: "x"}}); err == nil {
		t.Fatal("重复冻结应拒绝 (评分器漂移防线)")
	}
	if got := len(s.Tests("t1")); got != 1 {
		t.Fatalf("冻结卷被改写: %d 条", got)
	}
}

// TestSelectionMath 选点数学: 地板 + 帕累托 + 三档。
func TestSelectionMath(t *testing.T) {
	r := &Runner{Floor: 0.5, Tiers: DefaultTiers(), Epoch: 1}
	evals := []CandidateEval{
		{CandidateID: "a", PassRate: 0.9, CostTokens: 100},
		{CandidateID: "b", PassRate: 0.6, CostTokens: 40},
		{CandidateID: "c", PassRate: 0.3, CostTokens: 10},  // 地板过滤
		{CandidateID: "d", PassRate: 0.9, CostTokens: 300}, // 被 a 支配
	}
	var shards []graph.ShardResult
	for _, ev := range evals {
		shards = append(shards, graph.ShardResult{Status: "completed", Output: mustJSON(ev)})
	}
	res := r.runSelect(context.Background(), graph.NodeInput{Shards: shards})
	if res.Status != "completed" {
		t.Fatalf("select failed: %s", res.Err)
	}
	var rep SelectionReport
	if err := json.Unmarshal([]byte(res.Output), &rep); err != nil {
		t.Fatalf("report parse: %v", err)
	}
	if len(rep.Filtered) != 1 || rep.Filtered[0] != "c" {
		t.Fatalf("地板过滤应只剔 c: %v", rep.Filtered)
	}
	// 前沿应为 {a, b} (d 被 a 支配)。
	if len(rep.Frontier) != 2 {
		t.Fatalf("frontier = %v", rep.Frontier)
	}
	// lite (α=1,β=3) 偏成本 → b; heavy (α=3,β=1) 偏性能 → a。
	var lite, heavy string
	for _, s := range rep.Survivors {
		if s.Tier == "lite" {
			lite = s.CandidateID
		}
		if s.Tier == "heavy" {
			heavy = s.CandidateID
		}
	}
	if lite != "b" {
		t.Fatalf("lite 应选 b (成本最低), got %s", lite)
	}
	if heavy != "a" {
		t.Fatalf("heavy 应选 a (性能最高), got %s", heavy)
	}
	if rep.Fallback {
		t.Fatal("存在过地板候选时不应标记全灭兜底")
	}
}

func TestSelectionFallbackMarked(t *testing.T) {
	r := &Runner{Floor: 0.8, Tiers: DefaultTiers(), Epoch: 1}
	evals := []CandidateEval{
		{CandidateID: "a", VersionID: "hv-a", PassRate: 0.4, CostTokens: 100},
		{CandidateID: "b", VersionID: "hv-b", PassRate: 0.6, CostTokens: 40},
	}
	shards := []graph.ShardResult{
		{Status: "completed", Output: mustJSON(evals[0])},
		{Status: "completed", Output: mustJSON(evals[1])},
	}
	res := r.runSelect(context.Background(), graph.NodeInput{Shards: shards})
	if res.Status != "completed" {
		t.Fatalf("select failed: %s", res.Err)
	}
	var rep SelectionReport
	if err := json.Unmarshal([]byte(res.Output), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Fallback || len(rep.Filtered) != 2 || len(rep.Frontier) != 1 || rep.Frontier[0] != "b" {
		t.Fatalf("全灭必须显式标记，且只把兜底最佳者放入后续选点: %+v", rep)
	}
	if len(rep.Survivors) != 3 {
		t.Fatalf("工程兜底仍应保留三档选点: %+v", rep.Survivors)
	}
	for _, s := range rep.Survivors {
		if s.CandidateID != "b" || s.VersionID != "hv-b" {
			t.Fatalf("兜底应保留 pass 最高候选及其版本: %+v", s)
		}
	}
}

// TestEpochGraphValidate 每轮 DAG 过引擎的静态校验。
func TestEpochGraphValidate(t *testing.T) {
	spec := BuildEpochGraph(1)
	if err := spec.Validate(); err != nil {
		t.Fatalf("epoch graph validate: %v", err)
	}
	ts := BuildTestSynthGraph(DefaultPool()[:2])
	if err := ts.Validate(); err != nil {
		t.Fatalf("testsynth graph validate: %v", err)
	}
}

// TestDPPSelection 选题: 规模正确 + 确定性。
func TestDPPSelection(t *testing.T) {
	pool := DefaultPool()
	a := SelectTrainTasks(pool, 10)
	b := SelectTrainTasks(pool, 10)
	if len(a) != 10 {
		t.Fatalf("want 10, got %d", len(a))
	}
	if mustJSON(a) != mustJSON(b) {
		t.Fatal("选题不确定 (同一池两次结果不同)")
	}
	// 交集即训练集本身; 补集 = 验证集非空。
	seen := map[string]bool{}
	for _, x := range a {
		seen[x.ID] = true
	}
	if len(seen) != 10 {
		t.Fatal("选题重复")
	}
}
