package agent

// P1.4 图引擎接入回归 (design/01 M1 验收: 同一工作流新旧引擎产物等价)。

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
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

// TestGraphJournalResume 图引擎 resume: 同一 team dataDir 重跑, 已完成节点吃缓存零重跑。
func TestGraphJournalResume(t *testing.T) {
	wf := eqTestWorkflow()
	team := newStubTeam(t, "resume")

	var calls1 atomic.Int64
	if _, err := newStubExecutor(&calls1).executeGraph(context.Background(), wf, "测试目标", team); err != nil {
		t.Fatalf("首跑失败: %v", err)
	}
	if calls1.Load() != 3 {
		t.Fatalf("首跑应调用 3 次 agent, got %d", calls1.Load())
	}

	// 同一 dataDir 重跑: journal 里全部节点已完成 → 零 agent 调用
	var calls2 atomic.Int64
	res, err := newStubExecutor(&calls2).executeGraph(context.Background(), wf, "测试目标", team)
	if err != nil {
		t.Fatalf("重跑失败: %v", err)
	}
	if calls2.Load() != 0 {
		t.Errorf("resume 应零重跑, 实际调用 %d 次", calls2.Load())
	}
	if len(res) != 3 {
		t.Errorf("resume 应返回全部 3 个缓存阶段, got %d", len(res))
	}
}

// TestGraphModeInRouting graph 模式在两张路由表中都已登记。
func TestGraphModeInRouting(t *testing.T) {
	if !ModeHasDedicatedExecutor("graph") {
		t.Fatal("graph 模式必须在 dedicatedExecutorModes 表内 (自带 Journal 恢复, 不走 pipeline 检查点)")
	}
}
