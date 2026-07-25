package agent

import (
	"context"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// 图 gate 节点必须与 pipeline 侧同样产 gate 轨迹 —— 否则同一个团队在灰度开关两侧
// 产出**不同形状**的轨迹, 学习管线拿到的数据取决于开关状态。
func TestGraphGate_产生gate轨迹(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	r := &stageNodeRunner{we: &WorkflowExecutor{traceStore: ts}, team: &ProductionTeam{Name: "t"}}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-g"})

	// 确定性门禁 (零 LLM 路径)
	node := graph.NodeSpec{ID: "compile-gate", Kind: graph.NodeKindGate,
		Agent: graph.AgentSpec{Role: "gatekeeper", Deterministic: true}}
	res := r.runGate(ctx, node, graph.NodeInput{
		NodeRef: "compile-gate", PrevOutputs: map[string]string{"impl": "package main\nfunc main(){}"},
	})
	if res.Status != graph.NodeStatusCompleted {
		t.Fatalf("gate 状态 = %q", res.Status)
	}
	spans := readSpans(t, ts, "run-g")
	if len(spans) != 1 {
		t.Fatalf("gate span 数 = %d, 期望 1", len(spans))
	}
	s := spans[0]
	if s.Kind != tracestore.KindGate {
		t.Errorf("Kind = %q, 期望 %q", s.Kind, tracestore.KindGate)
	}
	if s.Attrs["gate"] != "deterministic" || s.Attrs["path"] != "graph" {
		t.Errorf("attrs 不对: %v", s.Attrs)
	}
	if s.Attrs["status"] != "pass" {
		t.Errorf("status = %v, 有产出应 pass", s.Attrs["status"])
	}
	if s.NodeID != "compile-gate" {
		t.Errorf("NodeID = %q", s.NodeID)
	}
}

// 评审器不可用时放行, 必须与"评审通过"在轨迹上可区分 —— 否则一段 LLM 故障会在
// 学习数据里表现为"这些产出质量都还行"。
func TestGraphGate_不可用与通过可区分(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	// we.llm 为 nil ⇒ 走 fail-open 的 unavailable 分支
	r := &stageNodeRunner{we: &WorkflowExecutor{traceStore: ts}, team: &ProductionTeam{Name: "t"}}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-u"})
	node := graph.NodeSpec{ID: "content-gate", Kind: graph.NodeKindGate,
		Agent: graph.AgentSpec{Role: "critic"}}
	res := r.runGate(ctx, node, graph.NodeInput{NodeRef: "content-gate",
		PrevOutputs: map[string]string{"draft": "一些内容"}})
	if res.Score != 60 {
		t.Errorf("不可用时应给中性分 60, 实得 %v", res.Score)
	}
	spans := readSpans(t, ts, "run-u")
	if len(spans) != 1 {
		t.Fatalf("span 数 = %d", len(spans))
	}
	if spans[0].Attrs["status"] != "unavailable" {
		t.Errorf("status = %v, 期望 unavailable (与 pass 必须可区分)", spans[0].Attrs["status"])
	}
	if spans[0].Attrs["gate"] != "unavailable" {
		t.Errorf("gate = %v", spans[0].Attrs["gate"])
	}
}

// 无底座时 gate 照常工作 (采集是观测不是治理)。
func TestGraphGate_无底座不影响判定(t *testing.T) {
	r := &stageNodeRunner{we: &WorkflowExecutor{}, team: &ProductionTeam{Name: "t"}}
	res := r.runGate(context.Background(),
		graph.NodeSpec{ID: "g", Kind: graph.NodeKindGate, Agent: graph.AgentSpec{Deterministic: true}},
		graph.NodeInput{PrevOutputs: map[string]string{"a": "x"}})
	if res.Status != graph.NodeStatusCompleted || res.Score == 0 {
		t.Errorf("无底座时 gate 判定被影响了: %+v", res)
	}
}
