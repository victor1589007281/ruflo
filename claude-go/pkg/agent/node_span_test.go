package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// KindNode 此前**全仓零产生方** —— 常量在、没人写。缺了 node 这一层, 奖励无法
// 归因到具体节点, 而"哪个阶段该改"正是结构学习器要回答的问题。
func TestWriteNodeSpan_产生node轨迹(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	we := &WorkflowExecutor{traceStore: ts}
	r := &stageNodeRunner{we: we, team: &ProductionTeam{Name: "t1"}}

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-1"})
	node := graph.NodeSpec{ID: "impl", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "coder"}}
	in := graph.NodeInput{
		NodeRef:     "impl",
		PrevOutputs: map[string]string{"design": "设计产出"},
	}
	r.writeNodeSpan(ctx, node, in, StageResult{Status: TaskCompleted, Output: "代码产出"}, time.Now())

	spans := readSpans(t, ts, "run-1")
	if len(spans) != 1 {
		t.Fatalf("span 数 = %d, 期望 1", len(spans))
	}
	s := spans[0]
	if s.Kind != tracestore.KindNode {
		t.Errorf("Kind = %q, 期望 %q", s.Kind, tracestore.KindNode)
	}
	if s.NodeID != "impl" || s.Name != "impl" {
		t.Errorf("归因不对: NodeID=%q Name=%q", s.NodeID, s.Name)
	}
	if s.Attrs["role"] != "coder" || s.Attrs["team"] != "t1" {
		t.Errorf("attrs 缺关键字段: %v", s.Attrs)
	}
	// 上游产出要进 InputRef, 否则无法回答"这个节点看到了什么"
	if !strings.Contains(s.InputRef.Inline, "设计产出") && s.InputRef.Blob == "" {
		t.Errorf("InputRef 未带上游产出: %+v", s.InputRef)
	}
	if !strings.Contains(s.OutputRef.Inline, "代码产出") && s.OutputRef.Blob == "" {
		t.Errorf("OutputRef 未带产出: %+v", s.OutputRef)
	}
}

// 分片与轮次必须能各自归因: 用声明名会把 N 个分片的 span 全挂在同一节点上。
func TestWriteNodeSpan_分片与轮次各自归因(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	r := &stageNodeRunner{we: &WorkflowExecutor{traceStore: ts}, team: &ProductionTeam{Name: "t"}}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-2"})
	node := graph.NodeSpec{ID: "m", Kind: graph.NodeKindMap, Agent: graph.AgentSpec{Role: "w"}}

	for i := 0; i < 3; i++ {
		r.writeNodeSpan(ctx, node, graph.NodeInput{
			NodeRef: "m#" + string(rune('0'+i)),
			Shard:   &graph.ShardInput{Index: i, Total: 3, Value: "片" + string(rune('0'+i))},
		}, StageResult{Status: TaskCompleted, Output: "o"}, time.Now())
	}
	r.writeNodeSpan(ctx, graph.NodeSpec{ID: "g", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "w"}},
		graph.NodeInput{NodeRef: "g", Iteration: 2, GroupIteration: 1},
		StageResult{Status: TaskCompleted, Output: "o"}, time.Now())

	spans := readSpans(t, ts, "run-2")
	if len(spans) != 4 {
		t.Fatalf("span 数 = %d, 期望 4", len(spans))
	}
	refs := map[string]bool{}
	for _, s := range spans {
		refs[s.NodeID] = true
	}
	for _, want := range []string{"m#0", "m#1", "m#2", "g"} {
		if !refs[want] {
			t.Errorf("缺 NodeID=%q 的 span —— 分片没能各自归因", want)
		}
	}
	for _, s := range spans {
		if s.NodeID == "g" {
			// 经 JSON 往返数字是 float64, 用数值比较而不是接口相等
			if numAttr(s.Attrs["iteration"]) != 2 || numAttr(s.Attrs["group_iteration"]) != 1 {
				t.Errorf("轮次未入 attrs: %v —— 不带轮次就无法做\"第几轮才收敛\"的分析", s.Attrs)
			}
		}
		if strings.HasPrefix(s.NodeID, "m#") && numAttr(s.Attrs["shard_total"]) != 3 {
			t.Errorf("分片信息未入 attrs: %v", s.Attrs)
		}
	}
}

// traceStore 为 nil 时静默 no-op —— 采集是观测不是治理, 不能影响执行。
func TestWriteNodeSpan_无底座不影响执行(t *testing.T) {
	r := &stageNodeRunner{we: &WorkflowExecutor{}, team: &ProductionTeam{Name: "t"}}
	r.writeNodeSpan(context.Background(), graph.NodeSpec{ID: "x"}, graph.NodeInput{}, StageResult{}, time.Now())
	(&stageNodeRunner{}).writeNodeSpan(context.Background(), graph.NodeSpec{ID: "x"}, graph.NodeInput{}, StageResult{}, time.Now())
}

// 输入摘要要按依赖名排序 (span 内容需可比对)。
func TestStageSpanInput_确定性(t *testing.T) {
	in := graph.NodeInput{PrevOutputs: map[string]string{"z": "1", "a": "2", "m": "3"}}
	node := graph.NodeSpec{Agent: graph.AgentSpec{Role: "r"}}
	first := stageSpanInput(node, in)
	for i := 0; i < 20; i++ {
		if stageSpanInput(node, in) != first {
			t.Fatal("输入摘要不确定 —— span 内容无法比对")
		}
	}
	ai, mi, zi := strings.Index(first, "from a"), strings.Index(first, "from m"), strings.Index(first, "from z")
	if !(ai < mi && mi < zi) {
		t.Errorf("依赖未按名排序: a=%d m=%d z=%d", ai, mi, zi)
	}
}

// numAttr 取 attrs 里的数值 (span 经 JSON 往返后整数会变成 float64)。
func numAttr(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}
