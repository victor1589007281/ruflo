package agent

// P1.4 图引擎接入回归 (design/01 M1 验收: 同一工作流新旧引擎产物等价)。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
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

// ---------------------------------------------------------------------------
// 图能力"通电"回归: StageDef 表达不出的图能力经推导 + 覆盖表落到 GraphSpec, 且真生效。
// ---------------------------------------------------------------------------

// stubStageOutput 与 stubStageRunner 同形态的产出 (须过 validateStageOutputForRetry)。
func stubStageOutput(role string) string {
	return fmt.Sprintf("[%s] done\n\n## 分析\n- 结论: 桩 agent 模拟产出\n- 方案: 该文本仅用于满足阶段产出结构化校验, 含标记与足量正文以模拟真实 agent 的交付形态。", role)
}

// progStageRunner 可编程 AgentRunner: 完全由测试给的闭包决定行为 (可读 ctx)。
type progStageRunner struct {
	role string
	fn   func(ctx context.Context, role, prompt string) (string, error)
}

func (p *progStageRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	return p.fn(ctx, p.role, userPrompt)
}

func newProgExecutor(fn func(ctx context.Context, role, prompt string) (string, error)) *WorkflowExecutor {
	return &WorkflowExecutor{
		factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &progStageRunner{role: role, fn: fn}, nil
		},
		notify: func(_, _ string) {},
	}
}

// TestTranslateFillsRetryBudgetAndMeta 直译器必须补齐 StageDef 表达不出的图能力:
// 图级默认重试 (此前恒 nil ⇒ maxRetries=0, 图的重试/退避/journal 全空转) +
// 角色差异化节点预算 (此前恒 0) + 正确的门禁元数据 (此前内置工作流恒 false)。
func TestTranslateFillsRetryBudgetAndMeta(t *testing.T) {
	wf := &WorkflowDef{
		Name: "development", // 内置产码工作流: 元数据必须判定为 ProducesCode
		Mode: "pipeline",
		Stages: []StageDef{
			{Name: "plan", Role: "architect", Prompt: "设计 {objective}"},
			{Name: "code", Role: "coder", Prompt: "实现 {prev_result}", DependsOn: []string{"plan"}},
		},
	}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	if spec.Policies.DefaultRetry == nil || spec.Policies.DefaultRetry.MaxRetries != graphOuterMaxRetries {
		t.Fatalf("图级默认重试 = %+v, 期望 MaxRetries=%d (图层接过重试职责)",
			spec.Policies.DefaultRetry, graphOuterMaxRetries)
	}
	if spec.Policies.DefaultRetry.BackoffSec != graphRetryBackoffSec {
		t.Errorf("退避基数 = %d, 期望 %d", spec.Policies.DefaultRetry.BackoffSec, graphRetryBackoffSec)
	}
	byID := map[string]graph.NodeSpec{}
	for _, n := range spec.Nodes {
		byID[n.ID] = n
	}
	if byID["code"].TimeoutSec <= 0 || byID["plan"].TimeoutSec <= 0 {
		t.Fatalf("节点预算未推导: plan=%d code=%d", byID["plan"].TimeoutSec, byID["code"].TimeoutSec)
	}
	// 角色差异化: coder (单次 25min) 的总预算必须高于 architect (单次 6min)。
	if byID["code"].TimeoutSec <= byID["plan"].TimeoutSec {
		t.Errorf("预算未按角色区分: coder=%d architect=%d", byID["code"].TimeoutSec, byID["plan"].TimeoutSec)
	}
	// 预算是"整节点总预算"(含图层重试), 不能等于单次角色超时——否则第二次重试必撞墙。
	perAttempt := int((&WorkflowExecutor{}).computeStageTimeout("coder", 0).Seconds())
	if byID["code"].TimeoutSec <= perAttempt {
		t.Errorf("coder 预算 %ds 未覆盖重试 (单次超时 %ds)", byID["code"].TimeoutSec, perAttempt)
	}
	if !spec.Meta.ProducesCode {
		t.Error("内置产码工作流 development 的 GraphMeta.ProducesCode 必须为 true (此前恒 false)")
	}
	if spec.Meta.QualityGate == "" {
		t.Error("GraphMeta.QualityGate 应显式落 content|none, 不留空")
	}
}

// TestTranslateGateDeterministic compile/test/build 类门禁 → gate 节点 + Deterministic
// 显式化 + 秒级预算 (纯代码判定不该按 LLM 角色算预算)。
func TestTranslateGateDeterministic(t *testing.T) {
	wf := &WorkflowDef{
		Name: "graph-gate-meta",
		Mode: "graph",
		Stages: []StageDef{
			{Name: "coder", Role: "coder", Prompt: "写码"},
			{Name: "compile-gate", Role: "gate", DependsOn: []string{"coder"}},
			{Name: "content-gate", Role: "critic", DependsOn: []string{"coder"}},
		},
	}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	for _, n := range spec.Nodes {
		switch n.ID {
		case "compile-gate":
			if n.Kind != graph.NodeKindGate || !n.Agent.Deterministic {
				t.Errorf("compile-gate 应为确定性 gate, got kind=%q deterministic=%v", n.Kind, n.Agent.Deterministic)
			}
			if n.TimeoutSec != graphDeterministicBudgetSec {
				t.Errorf("确定性门禁预算 = %d, 期望 %d", n.TimeoutSec, graphDeterministicBudgetSec)
			}
		case "content-gate":
			if n.Kind != graph.NodeKindGate {
				t.Errorf("content-gate 应为 gate 节点, got %q", n.Kind)
			}
			if n.Agent.Deterministic {
				t.Error("内容评审门禁走 LLM, 不该被标成 Deterministic")
			}
		}
	}
}

// TestGraphOverrideRoutesByGateScoreAndSkipIsNotFailure 条件边端到端:
// 门禁评分把流程路由到 ship 分支, fix 分支被跳过。
// 关键回归: 被路由掉的分支**不能**出现在 StageResult 里被算成"阶段失败"——
// 老实现把 skipped 一律映射成 TaskFailed, 一旦条件边真的用起来, 评分过关反而
// 会让 teams.go 统计出"1 个阶段失败"把整个团队判死。
func TestGraphOverrideRoutesByGateScoreAndSkipIsNotFailure(t *testing.T) {
	const name = "graph-cond-route"
	wf := &WorkflowDef{
		Name: name,
		Mode: "graph",
		Stages: []StageDef{
			{Name: "coder", Role: "coder", Prompt: "写码 {objective}"},
			{Name: "compile-gate", Role: "gate", DependsOn: []string{"coder"}},
			{Name: "ship", Role: "writer", Prompt: "交付 {prev_result}", DependsOn: []string{"compile-gate"}},
			{Name: "fix", Role: "coder", Prompt: "修 {prev_result}", DependsOn: []string{"compile-gate"}},
		},
	}
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"ship": {EdgeConditions: map[string]string{"compile-gate": "score >= 75"}},
		"fix":  {EdgeConditions: map[string]string{"compile-gate": "score < 75"}},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	conds := map[string]string{}
	for _, e := range spec.Edges {
		conds[e.From+"→"+e.To] = e.Condition
	}
	if conds["compile-gate→ship"] != "score >= 75" || conds["compile-gate→fix"] != "score < 75" {
		t.Fatalf("条件边未落到 GraphSpec: %v", conds)
	}

	var calls atomic.Int64
	team := newStubTeam(t, "cond")
	res, err := newStubExecutor(&calls).executeGraph(context.Background(), wf, "测试目标", team)
	if err != nil {
		t.Fatalf("图执行失败 (被路由掉的分支不该让整图失败): %v", err)
	}
	got := map[string]TaskStatus{}
	for _, r := range res {
		got[r.Name] = r.Status
	}
	if _, ok := got["fix"]; ok {
		t.Errorf("评分过关时 fix 分支被跳过, 不应出现在阶段结果里 (会被统计成失败): %+v", got)
	}
	for _, want := range []string{"coder", "compile-gate", "ship"} {
		if got[want] != TaskCompleted {
			t.Errorf("阶段 %s 状态 = %q, 期望 completed", want, got[want])
		}
	}
	// gate 是确定性节点 (零 LLM), 只有 coder + ship 两次 agent 调用。
	if calls.Load() != 2 {
		t.Errorf("agent 调用 %d 次, 期望 2 (coder+ship; 确定性 gate 不走 LLM)", calls.Load())
	}
}

// TestGraphInnerRetryYieldsToGraphLayer 重试让位的直接证据: 图 runner 必须给内层
// 打 outerRetryDriven 标记, 内层 executeStageWithRetry 才会退化, 否则
// 图层(1+6) × 内层(1+3+限流20) 变成嵌套放大 (design/01 §4.3 要消除的)。
func TestGraphInnerRetryYieldsToGraphLayer(t *testing.T) {
	var marked, total atomic.Int64
	we := newProgExecutor(func(ctx context.Context, role, _ string) (string, error) {
		total.Add(1)
		if isOuterRetryDriven(ctx) {
			marked.Add(1)
		}
		return stubStageOutput(role), nil
	})
	if _, err := we.executeGraph(context.Background(), eqTestWorkflow(), "测试目标", newStubTeam(t, "yield")); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	if total.Load() == 0 || marked.Load() != total.Load() {
		t.Fatalf("内层重试未让位: %d/%d 次调用带 outerRetryDriven 标记", marked.Load(), total.Load())
	}
}

// TestGraphNodeRetryTakesEffect 图层重试真生效 (此前 maxRetries=0, RetryPolicy 空转)。
// 用覆盖表把次数压到 1 次以缩短退避, 并用"永久错误"让内层一次即返回, 于是
// agent 调用次数 = 图层尝试次数。
func TestGraphNodeRetryTakesEffect(t *testing.T) {
	const name = "graph-retry-once"
	RegisterGraphOverride(name, WorkflowGraphOverride{
		DefaultRetry: &graph.RetryPolicy{MaxRetries: 1, BackoffSec: 1},
	})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "solo", Role: "writer", Prompt: "干活 {objective}"},
	}}
	var calls atomic.Int64
	we := newProgExecutor(func(_ context.Context, _, _ string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("桩故障: 永久失败") // 非瞬态: 内层不重试
	})
	team := newStubTeam(t, "retry")
	if _, err := we.executeGraph(context.Background(), wf, "测试目标", team); err == nil {
		t.Fatal("唯一节点恒失败, 整图应失败")
	}
	if calls.Load() != 2 {
		t.Fatalf("agent 调用 %d 次, 期望 2 (图层 1 次 + 1 次重试)", calls.Load())
	}
	// journal 必须留下重试痕迹 (事件溯源是唯一进度真源)。
	j, err := graph.NewFileJournal(filepath.Join(team.dataDir, "graph-journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	retried := 0
	for _, ev := range evs {
		if ev.Type == graph.EvNodeRetried {
			retried++
		}
	}
	if retried != 1 {
		t.Errorf("journal node.retried 事件 %d 条, 期望 1", retried)
	}
}

// TestGraphHooksFlushStagesDuringRun HookBus 通电的核心价值: 运行**中**就能在
// team.Stages 上看到进度 (dashboard 实时可见)。此前 executeGraph 不注入 Hooks
// (恒 NopBus), 图模式要等整图跑完才一次性出现全部阶段。
func TestGraphHooksFlushStagesDuringRun(t *testing.T) {
	team := newStubTeam(t, "flush")
	var mid []StageResult
	we := newProgExecutor(func(_ context.Context, role, _ string) (string, error) {
		if role == "writer" { // draft 阶段执行中: 快照此刻的团队阶段表
			team.mu.Lock()
			mid = append([]StageResult(nil), team.Stages...)
			team.mu.Unlock()
		}
		return stubStageOutput(role), nil
	})
	if _, err := we.executeGraph(context.Background(), eqTestWorkflow(), "测试目标", team); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	byName := map[string]StageResult{}
	for _, sr := range mid {
		byName[sr.Name] = sr
	}
	if len(mid) == 0 {
		t.Fatal("draft 执行中 team.Stages 为空: 阶段级增量刷盘未生效, dashboard 看不到进度")
	}
	if got := byName["research"]; got.Status != TaskCompleted || got.Output == "" {
		t.Errorf("运行中应已看到 research 完成且带产出, got %+v", got)
	}
	if got := byName["draft"]; got.Status != TaskRunning {
		t.Errorf("运行中应看到 draft 处于 running 占位, got %+v", got)
	}
	if _, ok := byName["review"]; ok {
		t.Error("尚未开始的 review 不该出现在运行中快照里")
	}
	// 终态: 全部阶段落盘且带实测耗时 (指标/看板都靠它)。
	team.mu.Lock()
	final := append([]StageResult(nil), team.Stages...)
	team.mu.Unlock()
	if len(final) != 3 {
		t.Fatalf("终态阶段数 = %d, 期望 3", len(final))
	}
	for _, sr := range final {
		if sr.Status != TaskCompleted || sr.Duration == "" {
			t.Errorf("阶段 %s 终态异常: status=%q duration=%q", sr.Name, sr.Status, sr.Duration)
		}
	}
}

// TestGraphOverrideLoopFeedback 节点级 Loop 经覆盖表表达并真生效 (此前生产零产生方)。
func TestGraphOverrideLoopFeedback(t *testing.T) {
	const name = "graph-loop-test"
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"polish": {Loop: &graph.LoopPolicy{MaxIterations: 2, Feedback: "上一轮产出: {prev_output}"}},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "polish", Role: "writer", Prompt: "润色 {objective}"},
	}}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	if spec.Nodes[0].Loop == nil || spec.Nodes[0].Loop.MaxIterations != 2 {
		t.Fatalf("Loop 未落到 GraphSpec: %+v", spec.Nodes[0].Loop)
	}

	var calls atomic.Int64
	var sawFeedback atomic.Bool
	we := newProgExecutor(func(_ context.Context, role, prompt string) (string, error) {
		if calls.Add(1) > 1 && strings.Contains(prompt, "上一轮反馈") && strings.Contains(prompt, "上一轮产出:") {
			sawFeedback.Store(true)
		}
		return stubStageOutput(role), nil
	})
	if _, err := we.executeGraph(context.Background(), wf, "测试目标", newStubTeam(t, "loop")); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("Loop 应跑 2 轮, 实际 %d 轮", calls.Load())
	}
	if !sawFeedback.Load() {
		t.Error("第二轮 prompt 未带上一轮回灌反馈, Loop.Feedback 未通到 agent")
	}
}

// TestGraphOverrideToolProfileAndMaxTurns AgentSpec 的 ToolProfile/MaxTurns 从
// "字段存在零消费"变为真下传: ToolProfile 经 NodeExecHints 给宿主 factory,
// MaxTurns 直接改写 ctx 里的 ResolvedConfig。
func TestGraphOverrideToolProfileAndMaxTurns(t *testing.T) {
	const name = "graph-hints-test"
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"dig": {ToolProfile: "analysis", MaxTurns: 7},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "dig", Role: "researcher", Prompt: "调研 {objective}"},
	}}
	var gotProfile atomic.Value
	var gotTurns atomic.Int64
	we := &WorkflowExecutor{
		// 有 planCfgResolver 才会有 ResolvedConfig 进 ctx (与生产一致)。
		planCfgResolver: &PlanConfigResolver{},
		notify:          func(_, _ string) {},
		factory: func(ctx context.Context, role, _ string) (AgentRunner, error) {
			gotProfile.Store(NodeExecHintsFromContext(ctx).ToolProfile)
			if mc, ok := ctx.Value(ModelConfigKey{}).(modelconfig.ResolvedConfig); ok {
				gotTurns.Store(int64(mc.MaxTurns))
			}
			return &progStageRunner{role: role, fn: func(_ context.Context, r, _ string) (string, error) {
				return stubStageOutput(r), nil
			}}, nil
		},
	}
	if _, err := we.executeGraph(context.Background(), wf, "测试目标", newStubTeam(t, "hints")); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	if p, _ := gotProfile.Load().(string); p != "analysis" {
		t.Errorf("ToolProfile 未下传给 factory: %q", p)
	}
	if gotTurns.Load() != 7 {
		t.Errorf("MaxTurns 未覆盖到 ResolvedConfig: %d", gotTurns.Load())
	}
}

// TestGraphOverridesFromJSON 覆盖表可从外部 JSON 注入 (免重编译即可给生产工作流
// 加条件边/Loop/预算), 且非法 kind 在直译期就被拦下。
func TestGraphOverridesFromJSON(t *testing.T) {
	const name = "graph-json-test"
	raw := `{"graph-json-test":{"maxParallel":2,"stages":{
		"a":{"timeoutSec":30,"loop":{"max_iterations":2}},
		"b":{"condition":"ok"}}}}`
	if err := LoadGraphOverridesJSON([]byte(raw)); err != nil {
		t.Fatalf("载入覆盖表失败: %v", err)
	}
	t.Cleanup(func() { UnregisterGraphOverride(name) })
	if ov, ok := LookupGraphOverride(name); !ok || ov.MaxParallel != 2 {
		t.Fatalf("覆盖表未注册: %+v ok=%v", ov, ok)
	}

	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "a", Role: "writer", Prompt: "一"},
		{Name: "b", Role: "critic", Prompt: "二", DependsOn: []string{"a"}},
	}}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	if spec.Policies.MaxParallel != 2 {
		t.Errorf("MaxParallel = %d, 期望 2", spec.Policies.MaxParallel)
	}
	if spec.Nodes[0].TimeoutSec != 30 {
		t.Errorf("显式 timeoutSec 应优先于推导: %d", spec.Nodes[0].TimeoutSec)
	}
	if spec.Nodes[0].Loop == nil || spec.Nodes[0].Loop.MaxIterations != 2 {
		t.Errorf("JSON 里的 loop 未生效: %+v", spec.Nodes[0].Loop)
	}
	if len(spec.Edges) != 1 || spec.Edges[0].Condition != "ok" {
		t.Errorf("JSON 里的 condition 未生效: %+v", spec.Edges)
	}

	// 非法 kind: 直译期报错, 不许带着坏声明进引擎。
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"a": {Kind: "router"},
	}})
	if _, err := TranslateWorkflow(wf); err == nil {
		t.Error("非法 kind 应在直译期被拒")
	}
}

// TestGraphOverrideKindAgentSuppressesGateKeyword 显式 kind=agent 压制关键词推断
// (名字里带"门禁"却是普通写作阶段的情况)。
func TestGraphOverrideKindAgentSuppressesGateKeyword(t *testing.T) {
	const name = "graph-kind-test"
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"质量门禁说明": {Kind: "agent"},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })
	wf := &WorkflowDef{Name: name, Mode: "graph", Stages: []StageDef{
		{Name: "质量门禁说明", Role: "writer", Prompt: "写说明"},
	}}
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	if spec.Nodes[0].Kind != graph.NodeKindAgent {
		t.Errorf("显式 kind=agent 未压制门禁关键词推断: %q", spec.Nodes[0].Kind)
	}
}

// TestWorkflowGateMetaByName 门禁元数据对内置与动态工作流都成立 (供门禁侧以元数据
// 取代按工作流名白名单, design/01 §4.1)。
func TestWorkflowGateMetaByName(t *testing.T) {
	if m := WorkflowGateMetaByName("development"); !m.ProducesCode {
		t.Error("development 应判定为产码工作流")
	}
	if m := WorkflowGateMetaByName("techblog"); m.QualityGate != "content" {
		t.Errorf("techblog 应走内容质量门禁, got %q", m.QualityGate)
	}
	if m := WorkflowGateMetaByName("techblog"); m.ProducesCode {
		t.Error("写作类工作流不该跑编译门禁")
	}
	if m := WorkflowGateMetaByName("不存在的工作流"); m.ProducesCode || m.QualityGate != "none" {
		t.Errorf("未知工作流应零门禁, got %+v", m)
	}
	// 动态工作流: 读自己声明的字段。
	custom := &WorkflowDef{Name: "my-dyn-wf", Custom: true, ProducesCode: true, QualityGate: "content"}
	if m := WorkflowGateMeta(custom); !m.ProducesCode || m.QualityGate != "content" {
		t.Errorf("动态工作流声明未被采纳: %+v", m)
	}
}

// TestMergeStageOverride 覆盖表压住阶段自身声明, 且逐字段合并 (不整体替换)、
// 不改入参 map —— 这是 StageDef 将来补字段后的合并规则, 先锁死。
func TestMergeStageOverride(t *testing.T) {
	decl := StageGraphOverride{
		Kind:           "gate",
		Condition:      "ok",
		EdgeConditions: map[string]string{"a": "ok", "b": "fail"},
		TimeoutSec:     100,
		MaxTurns:       3,
		ToolProfile:    "team",
	}
	ov := StageGraphOverride{
		Condition:      "score >= 75",
		EdgeConditions: map[string]string{"b": "score < 60"},
		TimeoutSec:     200,
		Loop:           &graph.LoopPolicy{MaxIterations: 2},
	}
	got := mergeStageOverride(decl, ov)
	if got.Condition != "score >= 75" || got.TimeoutSec != 200 {
		t.Errorf("覆盖表未压住阶段声明: %+v", got)
	}
	if got.Kind != "gate" || got.MaxTurns != 3 || got.ToolProfile != "team" {
		t.Errorf("覆盖表未声明的字段应保留阶段声明: %+v", got)
	}
	if got.EdgeConditions["a"] != "ok" || got.EdgeConditions["b"] != "score < 60" {
		t.Errorf("入边条件应逐条合并 (同名以覆盖表为准): %v", got.EdgeConditions)
	}
	if got.Loop == nil || got.Loop.MaxIterations != 2 {
		t.Errorf("Loop 未合并: %+v", got.Loop)
	}
	if decl.EdgeConditions["b"] != "fail" || len(ov.EdgeConditions) != 1 {
		t.Error("合并不得改写入参的 map")
	}
}

// ---------------------------------------------------------------------------
// map/reduce/loop-group/expand 的生产可表达性 (design/01 §4.1/§4.2/§4.4)
// ---------------------------------------------------------------------------

// fanoutTestWorkflow src → fan(map) → merge(reduce) 三阶段。
func fanoutTestWorkflow(name string) *WorkflowDef {
	return &WorkflowDef{
		Name: name,
		Mode: "graph",
		Stages: []StageDef{
			{Name: "src", Role: "researcher", Prompt: "列清单 {objective}"},
			{Name: "fan", Role: "writer", Prompt: "处理一片 {prev_result}", DependsOn: []string{"src"}},
			{Name: "merge", Role: "writer", Prompt: "汇总 {prev_result}", DependsOn: []string{"fan"}},
		},
	}
}

// TestTranslateMapReduceStages 覆盖表能把阶段声明成 map/reduce 节点, 且策略如实落进图。
func TestTranslateMapReduceStages(t *testing.T) {
	const name = "graph-fanout-translate"
	RegisterGraphOverride(name, WorkflowGraphOverride{
		MaxTotalNodes: 50,
		// 重试压到 0: 默认的 6 次重试会让单节点预算直接撞 2h 硬顶, 乘 MaxShards
		// 也还是 2h, 就验不出"预算按分片数放大"这件事了。
		DefaultRetry: &graph.RetryPolicy{MaxRetries: 0},
		Stages: map[string]StageGraphOverride{
			"fan": {Kind: "map", Map: &graph.MapPolicy{
				Source: graph.SourceObjective, Split: graph.SplitLines, MaxShards: 2, MinShards: 1}},
			"merge": {Kind: "reduce", Reduce: &graph.ReducePolicy{Strategy: graph.ReduceConcat}},
		},
	})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	spec, err := TranslateWorkflow(fanoutTestWorkflow(name))
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	byID := map[string]graph.NodeSpec{}
	for _, n := range spec.Nodes {
		byID[n.ID] = n
	}
	fan := byID["fan"]
	if fan.Kind != graph.NodeKindMap || fan.Map == nil || fan.Map.MaxShards != 2 {
		t.Fatalf("fan 节点 = %+v (map=%+v), 期望 map 形态 + 策略落图", fan, fan.Map)
	}
	if byID["merge"].Kind != graph.NodeKindReduce || byID["merge"].Reduce.Strategy != graph.ReduceConcat {
		t.Fatalf("merge 节点 = %+v", byID["merge"])
	}
	if spec.Policies.MaxTotalNodes != 50 {
		t.Errorf("图级节点总量上限未落图: %+v", spec.Policies)
	}
	// map 预算必须包住全部分片, 否则合法的扇出会被 deadline 掐死。
	single := graphNodeBudgetSec("writer", 0, false)
	if want := capNodeBudget(single * 2); fan.TimeoutSec != want {
		t.Errorf("map 节点预算 = %d, 期望 %d (单分片 %d × MaxShards 2, 夹 2h 硬顶)",
			fan.TimeoutSec, want, single)
	}
	if fan.TimeoutSec <= single {
		t.Errorf("map 节点预算 %d 未按分片数放大 (单分片 %d)", fan.TimeoutSec, single)
	}
}

// TestTranslateMapWithoutPolicyRejected kind=map 但缺策略必须开图即失败, 不猜。
func TestTranslateMapWithoutPolicyRejected(t *testing.T) {
	const name = "graph-map-nopolicy"
	RegisterGraphOverride(name, WorkflowGraphOverride{
		Stages: map[string]StageGraphOverride{"fan": {Kind: "map"}},
	})
	t.Cleanup(func() { UnregisterGraphOverride(name) })
	if _, err := TranslateWorkflow(fanoutTestWorkflow(name)); err == nil ||
		!strings.Contains(err.Error(), "缺少 map 策略") {
		t.Fatalf("kind=map 缺策略应被拒绝, 实得 %v", err)
	}
}

// TestGraphMapReduceEndToEnd 真跑一次 map→reduce: 分片各自一次 LLM 调用、
// 分片内容真的进了 prompt、reduce 拿到全部分片产出、分片在 team.Stages 里可见。
func TestGraphMapReduceEndToEnd(t *testing.T) {
	const name = "graph-fanout-e2e"
	RegisterGraphOverride(name, WorkflowGraphOverride{
		Stages: map[string]StageGraphOverride{
			// 集合取 objective: executeGraph 只透传 Objective (不传 Params),
			// 这是生产里最直接可用的确定性来源。
			"fan":   {Kind: "map", Map: &graph.MapPolicy{Source: graph.SourceObjective, MaxShards: 5}},
			"merge": {Kind: "reduce"},
		},
	})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	var mu sync.Mutex
	shardPrompts := map[string]string{}
	var mergePrompt string
	we := newProgExecutor(func(ctx context.Context, role, prompt string) (string, error) {
		node := NodeExecHintsFromContext(ctx).Node
		mu.Lock()
		switch {
		case strings.Contains(node, "#"):
			shardPrompts[node] = prompt
		case node == "merge":
			mergePrompt = prompt
		}
		mu.Unlock()
		return stubStageOutput(role) + "\n产出于节点 " + node, nil
	})
	team := newStubTeam(t, "fanout")
	results, err := we.executeGraph(context.Background(), fanoutTestWorkflow(name), "任务甲\n任务乙\n任务丙", team)
	if err != nil {
		t.Fatalf("图执行失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(shardPrompts) != 3 {
		t.Fatalf("分片执行了 %d 次, 期望 3 (objective 三行 → 三片): %v", len(shardPrompts), shardPrompts)
	}
	for i, want := range []string{"任务甲", "任务乙", "任务丙"} {
		id := fmt.Sprintf("fan#%d", i)
		p, ok := shardPrompts[id]
		if !ok {
			t.Fatalf("缺分片 %s 的调用", id)
		}
		if !strings.Contains(p, want) {
			t.Errorf("分片 %s 的 prompt 未含本片内容 %q (N 片会拿到同样输入 ⇒ 重复产出还烧 N 倍 token)", id, want)
		}
		if !strings.Contains(p, "本分片任务") {
			t.Errorf("分片 %s 的 prompt 缺分片小节: %s", id, truncateForLog(p))
		}
	}
	if !strings.Contains(mergePrompt, "待聚合的分片产出") {
		t.Fatalf("reduce 的 prompt 未带分片产出小节: %s", truncateForLog(mergePrompt))
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(mergePrompt, fmt.Sprintf("fan#%d", i)) {
			t.Errorf("reduce prompt 缺分片 fan#%d 的产出", i)
		}
	}
	// 顶层阶段结果: src/fan/merge (分片不是顶层阶段)
	got := map[string]TaskStatus{}
	for _, r := range results {
		got[r.Name] = r.Status
	}
	for _, want := range []string{"src", "fan", "merge"} {
		if got[want] != TaskCompleted {
			t.Errorf("阶段 %s = %q, 期望 completed", want, got[want])
		}
	}
	// 分片在 team.Stages 里可见 (dashboard 能逐片看进度)
	team.mu.Lock()
	stageNames := map[string]bool{}
	for _, s := range team.Stages {
		stageNames[s.Name] = true
	}
	team.mu.Unlock()
	for i := 0; i < 3; i++ {
		if !stageNames[fmt.Sprintf("fan#%d", i)] {
			t.Errorf("分片 fan#%d 未出现在 team.Stages (dashboard 上 8 分片扇出会显示成什么都没跑)", i)
		}
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// TestFoldStageGroupsIntoLoopGroup 阶段组折叠: 成员进组、组内边保留、跨界边改接组节点。
func TestFoldStageGroupsIntoLoopGroup(t *testing.T) {
	const name = "graph-group-fold"
	wf := &WorkflowDef{
		Name: name,
		Mode: "graph",
		Stages: []StageDef{
			{Name: "plan", Role: "researcher", Prompt: "计划"},
			{Name: "gen", Role: "writer", Prompt: "写", DependsOn: []string{"plan"}},
			{Name: "check", Role: "critic", Prompt: "查 {prev_result}", DependsOn: []string{"gen"}},
			{Name: "ship", Role: "writer", Prompt: "交付", DependsOn: []string{"check"}},
		},
	}
	RegisterGraphOverride(name, WorkflowGraphOverride{
		DefaultRetry: &graph.RetryPolicy{MaxRetries: 0}, // 同上: 避开 2h 硬顶才验得出乘法
		Groups: []StageGroupOverride{{
			ID: "adv", Members: []string{"gen", "check"}, MaxIterations: 3,
			Until: `output contains "验收通过"`, Feedback: "上轮: {prev_output}", ResultFrom: "check",
		}}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	spec, err := TranslateWorkflow(wf)
	if err != nil {
		t.Fatalf("直译失败: %v", err)
	}
	var ids []string
	for _, n := range spec.Nodes {
		ids = append(ids, n.ID)
	}
	if len(ids) != 3 || ids[0] != "plan" || ids[1] != "adv" || ids[2] != "ship" {
		t.Fatalf("顶层节点 = %v, 期望 [plan adv ship] (组成员被折进组节点)", ids)
	}
	var grp graph.NodeSpec
	for _, n := range spec.Nodes {
		if n.ID == "adv" {
			grp = n
		}
	}
	if grp.Kind != graph.NodeKindLoopGroup || grp.Group == nil {
		t.Fatalf("组节点 = %+v", grp)
	}
	if len(grp.Group.Nodes) != 2 || grp.Group.Nodes[0].ID != "gen" || grp.Group.Nodes[1].ID != "check" {
		t.Fatalf("组成员 = %+v, 期望按阶段声明序 [gen check]", grp.Group.Nodes)
	}
	if len(grp.Group.Edges) != 1 || grp.Group.Edges[0].From != "gen" || grp.Group.Edges[0].To != "check" {
		t.Fatalf("组内边 = %+v, 期望 gen→check", grp.Group.Edges)
	}
	if grp.Group.Loop.MaxIterations != 3 || grp.Group.ResultFrom != "check" ||
		grp.Group.Loop.Feedback != "上轮: {prev_output}" {
		t.Fatalf("组循环策略 = %+v", grp.Group.Loop)
	}
	edges := map[string]bool{}
	for _, e := range spec.Edges {
		edges[e.From+"→"+e.To] = true
	}
	if !edges["plan→adv"] || !edges["adv→ship"] || len(spec.Edges) != 2 {
		t.Fatalf("跨界边未改接到组节点: %v", spec.Edges)
	}
	// 组节点预算 = 成员预算之和 × 轮次上限 (夹 2h 硬顶), 作卡死兜底。
	sum := grp.Group.Nodes[0].TimeoutSec + grp.Group.Nodes[1].TimeoutSec
	if want := capNodeBudget(sum * 3); grp.TimeoutSec != want {
		t.Errorf("组节点预算 = %d, 期望 %d (成员之和 %d × 3 轮)", grp.TimeoutSec, want, sum)
	}
}

// TestFoldStageGroupsErrors 组声明的各类误用必须开图即失败。
func TestFoldStageGroupsErrors(t *testing.T) {
	base := func() *WorkflowDef {
		return &WorkflowDef{Name: "g", Mode: "graph", Stages: []StageDef{
			{Name: "a", Role: "writer", Prompt: "a"},
			{Name: "b", Role: "critic", Prompt: "b", DependsOn: []string{"a"}},
			{Name: "c", Role: "writer", Prompt: "c"}, // 与 a 无边: 用于构造组产出歧义
		}}
	}
	cases := []struct {
		name    string
		groups  []StageGroupOverride
		wantSub string
	}{
		{"成员不存在", []StageGroupOverride{{Members: []string{"ghost"}, MaxIterations: 2}}, "不是本工作流的阶段"},
		{"无成员", []StageGroupOverride{{ID: "x", MaxIterations: 2}}, "没有成员"},
		{"无轮次上限", []StageGroupOverride{{Members: []string{"a"}}}, "maxIterations"},
		{"组ID与阶段同名", []StageGroupOverride{{ID: "b", Members: []string{"a"}, MaxIterations: 2}}, "与已有阶段同名"},
		{"resultFrom不是成员", []StageGroupOverride{{Members: []string{"a"}, MaxIterations: 2, ResultFrom: "b"}}, "不是组成员"},
		{"成员被两组收编", []StageGroupOverride{
			{ID: "g1", Members: []string{"a"}, MaxIterations: 2},
			{ID: "g2", Members: []string{"a"}, MaxIterations: 2},
		}, "被两个组同时收编"},
		// a 与 c 之间没有边 ⇒ 两个出度 0 成员 ⇒ "组产出是谁"必须显式声明
		{"组产出歧义", []StageGroupOverride{{ID: "g1", Members: []string{"a", "c"}, MaxIterations: 2}}, "result_from"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := base()
			wf.Name = "grp-err-" + tc.name
			RegisterGraphOverride(wf.Name, WorkflowGraphOverride{Groups: tc.groups})
			t.Cleanup(func() { UnregisterGraphOverride(wf.Name) })
			_, err := TranslateWorkflow(wf)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("应报错 (含 %q), 实得 %v", tc.wantSub, err)
			}
		})
	}
}

// TestGraphLoopGroupEndToEnd 真跑组级循环: 两轮后 Until 命中, 回灌下发给全部成员。
func TestGraphLoopGroupEndToEnd(t *testing.T) {
	const name = "graph-group-e2e"
	wf := &WorkflowDef{
		Name: name, Mode: "graph",
		Stages: []StageDef{
			{Name: "gen", Role: "writer", Prompt: "写稿"},
			{Name: "check", Role: "critic", Prompt: "审 {prev_result}", DependsOn: []string{"gen"}},
			{Name: "ship", Role: "writer", Prompt: "交付", DependsOn: []string{"check"}},
		},
	}
	RegisterGraphOverride(name, WorkflowGraphOverride{Groups: []StageGroupOverride{{
		ID: "adv", Members: []string{"gen", "check"}, MaxIterations: 4,
		Until: `output contains "验收通过"`, Feedback: "按上轮意见改: {prev_output}", ResultFrom: "check",
	}}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	var mu sync.Mutex
	var genPrompts, refs []string
	checkCalls := 0
	we := newProgExecutor(func(ctx context.Context, role, prompt string) (string, error) {
		h := NodeExecHintsFromContext(ctx)
		mu.Lock()
		defer mu.Unlock()
		// Node = 成员语义名 (每轮相同); NodeRef = 带轮次的限定 ID (与 journal 对齐)。
		switch h.Node {
		case "gen":
			refs = append(refs, h.NodeRef)
			genPrompts = append(genPrompts, prompt)
			return stubStageOutput(role) + "\n第 " + fmt.Sprint(len(genPrompts)) + " 稿", nil
		case "check":
			checkCalls++
			if checkCalls >= 2 {
				return stubStageOutput(role) + "\n验收通过", nil
			}
			return stubStageOutput(role) + "\n还需打磨", nil
		}
		return stubStageOutput(role), nil
	})
	team := newStubTeam(t, "grp")
	results, err := we.executeGraph(context.Background(), wf, "写点东西", team)
	if err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(genPrompts) != 2 || checkCalls != 2 {
		t.Fatalf("组内成员调用 gen=%d check=%d, 期望各 2 轮 (第二轮 Until 命中)", len(genPrompts), checkCalls)
	}
	if strings.Contains(genPrompts[0], "按上轮意见改") {
		t.Error("第一轮不应有回灌")
	}
	if !strings.Contains(genPrompts[1], "按上轮意见改") || !strings.Contains(genPrompts[1], "还需打磨") {
		t.Errorf("第二轮 gen 未收到组回灌: %s", truncateForLog(genPrompts[1]))
	}
	// 限定 ID 带轮次: 宿主据此把两轮的记录分开 (Node 两轮都是 "gen")
	if len(refs) != 2 || refs[0] != "adv#it0/gen" || refs[1] != "adv#it1/gen" {
		t.Errorf("NodeRef = %v, 期望 [adv#it0/gen adv#it1/gen]", refs)
	}
	got := map[string]TaskStatus{}
	for _, r := range results {
		got[r.Name] = r.Status
	}
	if got["adv"] != TaskCompleted || got["ship"] != TaskCompleted {
		t.Fatalf("顶层阶段 = %+v, 期望组节点与下游都 completed", got)
	}
	// 组产出流向下游 + 黑板回写用组节点 ID
	if team.Blackboard != nil {
		if v, ok := team.Blackboard.Read("adv-result"); !ok || !strings.Contains(v, "验收通过") {
			t.Errorf("黑板 adv-result = %q, 期望组产出 (下游 harvest/handoff 依赖这个键)", v)
		}
	}
	// 组内成员按轮次出现在 team.Stages
	team.mu.Lock()
	names := map[string]bool{}
	for _, s := range team.Stages {
		names[s.Name] = true
	}
	team.mu.Unlock()
	for _, want := range []string{"adv#it0/gen", "adv#it1/check"} {
		if !names[want] {
			t.Errorf("组内成员 %s 未出现在 team.Stages: %v", want, names)
		}
	}
}

// TestParseStageExpansion 三种产出形态都能被解析成子图, 且悬空子任务自动挂到父节点下。
func TestParseStageExpansion(t *testing.T) {
	t.Run("原生nodes/edges形态", func(t *testing.T) {
		out := "分解如下\n```json\n" + `{"nodes":[{"id":"t1","kind":"agent","agent":{"role":"coder"}}],"edges":[{"from":"planner","to":"t1"}]}` + "\n```"
		ex := parseStageExpansion("planner", "planner", out)
		if ex == nil || len(ex.Nodes) != 1 || ex.Nodes[0].ID != "t1" || len(ex.Edges) != 1 {
			t.Fatalf("原生形态解析结果 = %+v", ex)
		}
	})
	t.Run("WBS形态_tasks_title_dependsOn_数字ID", func(t *testing.T) {
		// 与 orchestrator.go 既有 planner 产出格式一致 (数字 id / title / dependsOn)
		out := `前言{"tasks":[{"id":1,"title":"做甲","role":"coder"},{"id":2,"title":"做乙","role":"tester","dependsOn":[1]}]}尾巴`
		ex := parseStageExpansion("plan", "writer", out)
		if ex == nil || len(ex.Nodes) != 2 {
			t.Fatalf("WBS 形态解析结果 = %+v", ex)
		}
		if ex.Nodes[0].ID != "1" || ex.Nodes[0].Agent.Prompt != "做甲" || ex.Nodes[0].Agent.Role != "coder" {
			t.Fatalf("节点1 = %+v", ex.Nodes[0])
		}
		edges := map[string]bool{}
		for _, e := range ex.Edges {
			edges[e.From+"→"+e.To] = true
		}
		// 无依赖的挂父节点; 有依赖的连兄弟
		if !edges["plan→1"] || !edges["1→2"] || len(ex.Edges) != 2 {
			t.Fatalf("边 = %v, 期望 plan→1 与 1→2", ex.Edges)
		}
	})
	t.Run("subtasks形态_未给角色则继承父角色", func(t *testing.T) {
		ex := parseStageExpansion("p", "architect", `{"subtasks":[{"id":"x","task":"干活"}]}`)
		if ex == nil || ex.Nodes[0].Agent.Role != "architect" {
			t.Fatalf("未给角色应继承父节点角色: %+v", ex)
		}
	})
	t.Run("无子图产出返回nil", func(t *testing.T) {
		for _, out := range []string{"", "没有任何 JSON", `{"score":80}`, `{"tasks":[]}`, `{"tasks":[{"title":"缺id"}]}`} {
			if ex := parseStageExpansion("p", "r", out); ex != nil {
				t.Errorf("产出 %q 不该解析出子图: %+v", out, ex)
			}
		}
	})
}

// TestGraphExpandEndToEnd 真跑一次动态展开: 分解阶段的产出被并入运行图并执行,
// 下游阶段等到展开子图跑完。
func TestGraphExpandEndToEnd(t *testing.T) {
	const name = "graph-expand-e2e"
	wf := &WorkflowDef{
		Name: name, Mode: "graph",
		Stages: []StageDef{
			// 刻意不叫 plan / planner 角色: 那会触发 workflow.go 的 WBS 强校验,
			// 与本测试要验的展开解析无关。
			{Name: "decompose", Role: "architect", Prompt: "分解 {objective}"},
			{Name: "sum", Role: "writer", Prompt: "汇总 {prev_result}", DependsOn: []string{"decompose"}},
		},
	}
	RegisterGraphOverride(name, WorkflowGraphOverride{Stages: map[string]StageGraphOverride{
		"decompose": {Expand: &graph.ExpandSpec{MaxNodes: 4, MaxDepth: 1}},
	}})
	t.Cleanup(func() { UnregisterGraphOverride(name) })

	var mu sync.Mutex
	ran := map[string]int{}
	var sumPrompt string
	we := newProgExecutor(func(ctx context.Context, role, prompt string) (string, error) {
		node := NodeExecHintsFromContext(ctx).Node
		mu.Lock()
		ran[node]++
		if node == "sum" {
			sumPrompt = prompt
		}
		mu.Unlock()
		if node == "decompose" {
			return stubStageOutput(role) + "\n```json\n" +
				`{"subtasks":[{"id":"甲","role":"writer","task":"做甲"},{"id":"乙","role":"writer","task":"做乙"}]}` +
				"\n```", nil
		}
		return stubStageOutput(role) + "\n产出于 " + node, nil
	})
	team := newStubTeam(t, "expand")
	if _, err := we.executeGraph(context.Background(), wf, "干三件事", team); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"decompose/甲", "decompose/乙"} {
		if ran[id] != 1 {
			t.Fatalf("展开产物 %s 执行 %d 次, 期望 1 (ran=%v)", id, ran[id], ran)
		}
	}
	// 重接线: sum 拿得到展开节点的产出 (否则"先分解再逐项执行再汇总"的汇总是空的)
	for _, id := range []string{"decompose/甲", "decompose/乙"} {
		if !strings.Contains(sumPrompt, id) {
			t.Errorf("sum 的 prompt 未含展开节点 %s 的产出: %s", id, truncateForLog(sumPrompt))
		}
	}
	// journal 里有 graph.expanded
	j, err := graph.NewFileJournal(filepath.Join(team.dataDir, "graph-journal"))
	if err != nil {
		t.Fatalf("打开 journal: %v", err)
	}
	defer j.Close()
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == graph.EvGraphExpanded {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("journal 里 graph.expanded 事件数 = %d, 期望 1", n)
	}
}

// TestGraphExpandOnlyWhenAuthorized 没声明 Expand 的阶段, 产出里的子图必须被忽略
// (展开能力必须由图显式授予, 否则任何 runner 都能往运行图塞节点)。
func TestGraphExpandOnlyWhenAuthorized(t *testing.T) {
	const name = "graph-expand-unauth"
	wf := &WorkflowDef{
		Name: name, Mode: "graph",
		Stages: []StageDef{{Name: "decompose", Role: "architect", Prompt: "分解"}},
	}
	var mu sync.Mutex
	ran := map[string]int{}
	we := newProgExecutor(func(ctx context.Context, role, _ string) (string, error) {
		node := NodeExecHintsFromContext(ctx).Node
		mu.Lock()
		ran[node]++
		mu.Unlock()
		return stubStageOutput(role) + "\n" + `{"subtasks":[{"id":"甲","task":"做甲"}]}`, nil
	})
	if _, err := we.executeGraph(context.Background(), wf, "目标", newStubTeam(t, "unauth")); err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran["decompose"] != 1 {
		t.Fatalf("未授权展开却多跑了节点: %v", ran)
	}
}
