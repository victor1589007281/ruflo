package agent

// evolution_wiring_e2e_test.go —— "写了 ≠ 通电"的验收: 跑一次**真的** executeWorkflow,
// 看新接的三个采集点/奖励源是不是真的在生产路径上发出来了。
//
// 单元测试只能证明函数本身对; 证明不了它挂在了会被执行的位置。本文件走的是与
// run_interceptors_equiv_test 同一条路 (真 ProductionTeamManager + 真工作流 + 桩 agent),
// 只是把 TraceStore 与 Evolution 也装上 —— 那正是生产装配 (main.go / feishu bot) 的形态。
//
// 一处如实的边界: verdict.heuristic 的输入是**引擎**写的 llm_call/turn Span, 而桩 agent
// 不打 LLM。所以这里由桩 runner 按 pkg/engine/trace_llm.go 的同一形状补写 llm_call Span,
// 验的是"run 收尾这一环真的读得到本 run 的轨迹并据此发出奖励"这一段接线。裁决口径本身
// 由 verdict_reward_test / verdict_consistency_test 覆盖。

import (
	"context"
	"github.com/anthropic/claude-go/pkg/metrics"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

const wireWorkflowName = "wire-evo-flow"

// wireRunner 桩 agent: 顺带按引擎的形状补一条 llm_call Span。
type wireRunner struct {
	role string
	ts   *tracestore.Store
}

func (r *wireRunner) Execute(ctx context.Context, prompt string) (string, error) {
	ids := trace.From(ctx)
	// 形状对齐 pkg/engine/trace_llm.go 的 writeLLMCallSpan (Kind/NodeID/TurnID/Attrs)。
	r.ts.Write(tracestore.Span{
		TraceID: ids.RunID, SpanID: "llm-" + r.role, Kind: tracestore.KindLLMCall,
		Name: "stub-model", NodeID: ids.NodeID, TurnID: ids.TurnID,
		Attrs: map[string]any{"status": "success", "stop_reason": "end_turn"},
		TS:    time.Now().UnixMilli(),
	})
	return "OUT[" + r.role + "]" + strings.Repeat("内容充实的一段产出。", 12), nil
}

func wireWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name: wireWorkflowName, Description: "进化采集接线验收夹具", Mode: "pipeline",
		Stages: []StageDef{
			{Name: "s0", Role: "wire-lead", Prompt: "S0 目标: {objective}"},
			{Name: "s1", Role: "wire-two", Prompt: "S1 依据: {prev_result}", DependsOn: []string{"s0"}},
		},
	}
}

// TestEvolutionWiring_一次真实run产出run与policy_decision轨迹并发出奖励
//
// 这条测试同时钉住四件事的**通电**:
//
//	KindRun            beginRun 的收尾闭包 (run_span.go)
//	KindPolicyDecision executeStage 的注入点旁 (policy_decision.go)
//	episode 奖励        运行级链的 evolution 环 (既有)
//	verdict.heuristic  同一环, 从本 run 轨迹派生 (verdict_reward.go)
func TestEvolutionWiring_一次真实run产出run与policy_decision轨迹并发出奖励(t *testing.T) {
	if err := RegisterWorkflow(wireWorkflow(), nil); err != nil {
		t.Fatalf("注册夹具工作流: %v", err)
	}
	t.Cleanup(func() { _ = UnregisterWorkflow(wireWorkflowName) })

	// **刻意不用 t.TempDir()**: 这个用例真跑一次 executeWorkflow, 团队会派生出活过
	// 测试体的 goroutine(指标采集器是**被捕获的指针**, 重指全局单例对它无效), 它们在
	// 测试返回后仍会写 state/metrics —— 与框架的 TempDir 清理竞态, 实测 10 连跑红 2-3 次。
	// 报错来自 testing 框架("directory not empty"), 与被测逻辑毫无关系, 是本仓记过的
	// **第三类假测试(非密闭)**。自管目录让这些迟到的写入变成无害。
	tmp, mkErr := os.MkdirTemp("", "evo-wiring-")
	if mkErr != nil {
		t.Fatalf("建临时目录失败: %v", mkErr)
	}
	t.Cleanup(func() {
		// 先给迟到的写入一点时间落完, 再删; 删失败也不让它变成测试失败。
		metrics.WaitRestore()
		_ = os.RemoveAll(tmp)
	})
	state := filepath.Join(tmp, "state")
	ts := tracestore.New(statestore.NewFileStore(filepath.Join(state, "statestore")))
	ee := NewEvolutionEngine(filepath.Join(state, "evolution"), nil)
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(state, "teams"),
		Factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &wireRunner{role: role, ts: ts}, nil
		},
		Notify:     func(_, _ string) {},
		Evolution:  ee,
		TraceStore: ts, // 与生产装配同形 (main.go / feishu bot 都注入这两个)
	})
	team, err := ptm.CreateTeam("wire-team", wireWorkflowName, "把 X 做成 Y", "chat-1")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	team.mu.Lock()
	team.Status = TeamStatusRunning
	team.StartedAt = time.Now()
	team.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ptm.executeWorkflow(context.Background(), team, false) }()
	wg.Wait()

	team.mu.Lock()
	runID := team.LastRunID
	status := team.Status
	team.mu.Unlock()
	if runID == "" {
		t.Fatal("run 没起来")
	}
	if status != TeamStatusCompleted {
		t.Fatalf("夹具应正常完成, got %s", status)
	}

	spans := readSpans(t, ts, runID)
	byKind := map[string]int{}
	for _, sp := range spans {
		byKind[sp.Kind]++
	}
	// KindRun: 每次 run 恰好一条 —— 多于一条说明收尾闭包被重复调用。
	if byKind[tracestore.KindRun] != 1 {
		t.Errorf("run Span 应恰好 1 条, got %d (全部: %v)", byKind[tracestore.KindRun], byKind)
	}
	// KindPolicyDecision: 每个阶段一条 (两个阶段)。
	if byKind[tracestore.KindPolicyDecision] != 2 {
		t.Errorf("policy_decision Span 应每阶段一条 (共 2), got %d (全部: %v)",
			byKind[tracestore.KindPolicyDecision], byKind)
	}
	// run Span 必须自述终态 —— 这就是"由 TraceID 隐含"补不上的那部分。
	for _, sp := range spans {
		if sp.Kind != tracestore.KindRun {
			continue
		}
		if sp.Attrs["status"] != string(TeamStatusCompleted) || sp.Attrs["workflow"] != wireWorkflowName {
			t.Errorf("run Span 未自述终态/工作流: %+v", sp.Attrs)
		}
	}

	// 奖励侧: episode 与 verdict.heuristic 都必须真的落进本 run。
	got := map[string]int{}
	for _, ev := range readRewards(t, state) {
		if ev.RunID == runID {
			got[ev.Source]++
		}
	}
	if got[RewardSourceEpisode] == 0 {
		t.Errorf("episode 奖励缺失, 已有: %v", got)
	}
	if got[RewardSourceVerdictHeuristic] == 0 {
		t.Errorf("verdict.heuristic 没在生产路径上发出来 (建成未通电), 已有: %v", got)
	}
	// 且能被读侧聚合到 —— 落盘不等于进了总线。
	if _, ok := ee.RunRewardScore(runID, "wire-team"); !ok {
		t.Error("AggregateRewards 读不到本 run 的奖励证据")
	}
}
