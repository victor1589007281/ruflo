package agent

// P1.4 图引擎接入回归 (design/01 M1 验收: 同一工作流新旧引擎产物等价)。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// stubStageRunner 可编程 AgentRunner: 按 prompt 回显, 记录调用次数。
type stubStageRunner struct {
	role  string
	calls *atomic.Int64
}

func (s *stubStageRunner) Execute(_ context.Context, userPrompt string) (string, error) {
	s.calls.Add(1)
	// 产出须过 validateStageOutputForRetry: 带 "## 分析" 结构化标记 + 足够长度
	return fmt.Sprintf("[%s] done\n\n## 分析\n- 结论: 桩 agent 模拟产出 (prompt %d chars)\n- 方案: 该文本仅用于满足阶段产出结构化校验, 含标记与足量正文以模拟真实 agent 的交付形态。", s.role, len(userPrompt)), nil
}

func newStubExecutor(calls *atomic.Int64) *WorkflowExecutor {
	return &WorkflowExecutor{
		factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &stubStageRunner{role: role, calls: calls}, nil
		},
		notify: func(_, _ string) {},
	}
}

func newStubTeam(t *testing.T, name string) *ProductionTeam {
	t.Helper()
	return &ProductionTeam{
		Name:      name,
		Workflow:  "graph-eq-test",
		Objective: "测试目标",
		Agents:    map[string]*BGAgent{},
		dataDir:   t.TempDir(),
	}
}

func eqTestWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name: "graph-eq-test",
		Mode: "pipeline",
		Stages: []StageDef{
			{Name: "research", Role: "researcher", Prompt: "调研 {objective}"},
			{Name: "draft", Role: "writer", Prompt: "基于 {prev_result} 起草", DependsOn: []string{"research"}},
			{Name: "review", Role: "critic", Prompt: "评审 {prev_result}", DependsOn: []string{"draft"}},
		},
	}
}

// TestTranslateAllBuiltinPipelines 所有内置 pipeline 形态工作流必须可直译且过 Validate。
func TestTranslateAllBuiltinPipelines(t *testing.T) {
	n := 0
	for _, wf := range ListWorkflows() {
		if wf.Mode != "" && wf.Mode != "pipeline" && wf.Mode != "fanout" {
			continue
		}
		w := wf
		if _, err := TranslateWorkflow(&w); err != nil {
			t.Errorf("内置工作流 %q 直译失败: %v", wf.Name, err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("未发现任何 pipeline 形态内置工作流, 测试前提失效")
	}
	t.Logf("直译通过 %d 个内置 pipeline 工作流", n)
}

// TestGraphPipelineEquivalence 图引擎与旧 pipeline 路径产物等价 (M1 核心验收)。
func TestGraphPipelineEquivalence(t *testing.T) {
	wf := eqTestWorkflow()

	var callsOld, callsNew atomic.Int64
	oldResults, err := newStubExecutor(&callsOld).executePipeline(context.Background(), wf, "测试目标", newStubTeam(t, "old"))
	if err != nil {
		t.Fatalf("旧引擎失败: %v", err)
	}
	newResults, err := newStubExecutor(&callsNew).executeGraph(context.Background(), wf, "测试目标", newStubTeam(t, "new"))
	if err != nil {
		t.Fatalf("图引擎失败: %v", err)
	}

	if callsOld.Load() != callsNew.Load() {
		t.Errorf("agent 调用次数不等价: old=%d new=%d", callsOld.Load(), callsNew.Load())
	}
	toMap := func(rs []StageResult) map[string]StageResult {
		m := map[string]StageResult{}
		for _, r := range rs {
			m[r.Name] = r
		}
		return m
	}
	om, nm := toMap(oldResults), toMap(newResults)
	if len(om) != len(nm) {
		t.Fatalf("阶段数不等价: old=%d new=%d", len(om), len(nm))
	}
	for name, o := range om {
		n, ok := nm[name]
		if !ok {
			t.Errorf("图引擎缺少阶段 %q", name)
			continue
		}
		if o.Status != n.Status || o.Role != n.Role {
			t.Errorf("阶段 %q 状态/角色不等价: old=%+v new=%+v", name, o, n)
		}
		// 产出都来自 stub, 前缀应一致 (prompt 长度可能微差, 不比较全文)
		if !strings.HasPrefix(n.Output, "["+n.Role+"] done") {
			t.Errorf("阶段 %q 产出形态异常: %q", name, n.Output)
		}
	}
}

// TestGraphJournalResume_崩溃后续跑 图引擎 resume 的**正当用途**: 上一轮跑到一半
// 进程被 kill (journal 有 node.completed 但无 run.finished), 重启后已完成节点吃缓存。
//
// 注意本测试的前身断言的是"同一 dataDir 重跑 → 零 agent 调用", 那其实是把缺陷
// 当成了期望行为: journal 在生产是 per-team 的, 于是任何第二次运行 (包括 refine
// 与用户手动重跑) 都会重放上一轮的 completed 事件、调度零个节点、直接返回旧产出
// 并报 completed。已按"只重放最近一次且未完结的 run"修正, 见 pkg/graph.Replay
// 与 TestGraphRerunAfterCompleted_不吃旧缓存。
func TestGraphJournalResume_崩溃后续跑(t *testing.T) {
	wf := eqTestWorkflow()
	team := newStubTeam(t, "resume")

	// 手写一个"跑了一半就崩"的 journal: research 已完成, 无 run.finished
	journalDir := filepath.Join(team.dataDir, "graph-journal")
	j, err := graph.NewFileJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-resume-crashed"
	for _, ev := range []graph.Event{
		{Seq: 1, Type: graph.EvRunCreated, RunID: runID},
		{Seq: 2, Type: graph.EvNodeCompleted, RunID: runID, NodeID: "research",
			Data: map[string]any{"output": "上一轮的调研产出"}},
	} {
		if err := j.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	var calls atomic.Int64
	res, err := newStubExecutor(&calls).executeGraph(context.Background(), wf, "测试目标", team)
	if err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	// research 吃缓存, 只需再跑 draft + review
	if calls.Load() != 2 {
		t.Errorf("崩溃续跑应只补跑 2 个节点, 实际调用 %d 次", calls.Load())
	}
	if len(res) != 3 {
		t.Errorf("应返回全部 3 个阶段 (1 缓存 + 2 新跑), got %d", len(res))
	}
	for _, r := range res {
		if r.Name == "research" && r.Output != "上一轮的调研产出" {
			t.Errorf("research 未吃到缓存产出, got %q", r.Output)
		}
	}
}

// 回归 B-1: 上一轮已**完整跑完**的团队再次运行, 必须真的重跑, 不能把上一轮
// 的产出当成本轮结果直接返回。这正是 refine / 用户手动重跑走的路径。
func TestGraphRerunAfterCompleted_不吃旧缓存(t *testing.T) {
	wf := eqTestWorkflow()
	team := newStubTeam(t, "rerun")

	var calls1 atomic.Int64
	if _, err := newStubExecutor(&calls1).executeGraph(context.Background(), wf, "测试目标", team); err != nil {
		t.Fatalf("首跑失败: %v", err)
	}
	if calls1.Load() != 3 {
		t.Fatalf("首跑应调用 3 次 agent, got %d", calls1.Load())
	}

	var calls2 atomic.Int64
	res, err := newStubExecutor(&calls2).executeGraph(context.Background(), wf, "测试目标", team)
	if err != nil {
		t.Fatalf("重跑失败: %v", err)
	}
	if calls2.Load() != 3 {
		t.Errorf("已完结的 run 再次运行必须全量重跑, 实际只调用 %d 次 (=静默零执行返回旧产出)", calls2.Load())
	}
	if len(res) != 3 {
		t.Errorf("应返回 3 个阶段, got %d", len(res))
	}
}

// TestGraphModeInRouting graph 模式在两张路由表中都已登记。
func TestGraphModeInRouting(t *testing.T) {
	if !ModeHasDedicatedExecutor("graph") {
		t.Fatal("graph 模式必须在 dedicatedExecutorModes 表内 (自带 Journal 恢复, 不走 pipeline 检查点)")
	}
}

// TestGateNodeDeterministicScoring 门禁节点差异化执行: 确定性 gate 输出 score。
func TestGateNodeDeterministicScoring(t *testing.T) {
	// 用直接 stageNodeRunner + 确定性 gate (compile/test 关键词, 无需 LLM)
	team := newStubTeam(t, "gate-test")
	we := newStubExecutor(new(atomic.Int64))
	r := &stageNodeRunner{we: we, team: team, objective: "obj"}

	// compile gate: 上游有产出 → score 80+; 结构化 → 90
	res := r.runGate(context.Background(),
		graph.NodeSpec{ID: "compile-gate", Kind: graph.NodeKindGate},
		graph.NodeInput{PrevOutputs: map[string]string{"coder": "package main\nfunc main(){}"}})
	if res.Status != graph.NodeStatusCompleted {
		t.Fatalf("gate 应 completed: %+v", res)
	}
	if res.Score < 80 {
		t.Fatalf("有产出的 compile gate 应 score>=80, got %.0f", res.Score)
	}

	// 无产出 → score 0
	res2 := r.runGate(context.Background(),
		graph.NodeSpec{ID: "test-gate", Kind: graph.NodeKindGate},
		graph.NodeInput{PrevOutputs: map[string]string{}})
	if res2.Score != 0 {
		t.Fatalf("无产出应 score=0, got %.0f", res2.Score)
	}
}

// TestGateConditionRouting gate 分数经条件边路由到不同分支。
func TestGateConditionRouting(t *testing.T) {
	// 图: coder → gate(compile) → [pass: score>=75 → deploy] / [fail: score<75 → fix]
	wf := &WorkflowDef{
		Name: "gate-routing",
		Mode: "graph",
		Stages: []StageDef{
			{Name: "coder", Role: "coder", Prompt: "写代码 {objective}"},
			{Name: "compile-gate", Role: "gate", Prompt: "", DependsOn: []string{"coder"}},
		},
	}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatal(err)
	}
	// compile-gate 应被译为 gate 节点
	var gateKind graph.NodeKind
	for _, n := range spec.Nodes {
		if n.ID == "compile-gate" {
			gateKind = n.Kind
		}
	}
	if gateKind != graph.NodeKindGate {
		t.Fatalf("含 gate 关键词的 stage 应译为 gate 节点, got %q", gateKind)
	}
}

func TestStageIsGate(t *testing.T) {
	if !stageIsGate(StageDef{Name: "compile-gate", Role: "gate"}) {
		t.Error("含 gate 应识别为门禁")
	}
	if !stageIsGate(StageDef{Name: "质量门禁", Role: "critic"}) {
		t.Error("含门禁应识别")
	}
	if stageIsGate(StageDef{Name: "coder", Role: "coder"}) {
		t.Error("普通 coder 不应识别为门禁")
	}
}
