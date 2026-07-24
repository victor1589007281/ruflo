package graph

import (
	"strings"
	"testing"
)

// vNode 构造合法 agent 节点 (测试辅助)。
func vNode(id string) NodeSpec {
	return NodeSpec{ID: id, Kind: NodeKindAgent, Agent: AgentSpec{Role: "worker"}}
}

// TestValidateOK 合法图 (线性+条件边+loop) 应通过校验。
func TestValidateOK(t *testing.T) {
	g := GraphSpec{
		Name: "ok",
		Nodes: []NodeSpec{
			vNode("a"),
			{ID: "g", Kind: NodeKindGate, Agent: AgentSpec{Role: "gate", Deterministic: true}},
			{ID: "b", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"},
				Loop: &LoopPolicy{MaxIterations: 3, Until: "score >= 75"}},
		},
		Edges: []EdgeSpec{
			{From: "a", To: "g"},
			{From: "g", To: "b", Condition: "score >= 75"},
		},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("合法图不应报错: %v", err)
	}
}

// TestValidateErrors Validate 全错误分支。
func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		g       GraphSpec
		wantSub string // 错误信息必须包含的子串
	}{
		{
			name:    "空图",
			g:       GraphSpec{Name: "empty"},
			wantSub: "图为空",
		},
		{
			name: "节点ID为空",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				{ID: "  ", Kind: NodeKindAgent},
			}},
			wantSub: "ID 为空",
		},
		{
			name: "节点ID重复",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				vNode("a"), vNode("a"),
			}},
			wantSub: "重复",
		},
		{
			name: "预留Kind暂不支持",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				{ID: "m", Kind: NodeKindMap},
			}},
			wantSub: "暂不支持",
		},
		{
			name: "loop-group暂不支持",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				{ID: "lg", Kind: "loop-group"},
			}},
			wantSub: "暂不支持",
		},
		{
			name: "未知Kind",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				{ID: "b", Kind: "banana"},
			}},
			wantSub: "未知",
		},
		{
			name: "循环无上限",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				{ID: "a", Kind: NodeKindAgent, Loop: &LoopPolicy{MaxIterations: 0}},
			}},
			wantSub: "max_iterations",
		},
		{
			name: "Until条件语法错",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{
				{ID: "a", Kind: NodeKindAgent, Loop: &LoopPolicy{MaxIterations: 2, Until: "score >> 5"}},
			}},
			wantSub: "Loop.until",
		},
		{
			name: "悬空边-起点不存在",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a")},
				Edges: []EdgeSpec{{From: "ghost", To: "a"}}},
			wantSub: "不存在",
		},
		{
			name: "悬空边-终点不存在",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a")},
				Edges: []EdgeSpec{{From: "a", To: "ghost"}}},
			wantSub: "不存在",
		},
		{
			name: "自环",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a")},
				Edges: []EdgeSpec{{From: "a", To: "a"}}},
			wantSub: "自环",
		},
		{
			name: "边条件语法错",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a"), vNode("b")},
				Edges: []EdgeSpec{{From: "a", To: "b", Condition: "score >= abc"}}},
			wantSub: "条件语法错误",
		},
		{
			name: "无入口的纯环",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a"), vNode("b")},
				Edges: []EdgeSpec{{From: "a", To: "b"}, {From: "b", To: "a"}}},
			wantSub: "有向环",
		},
		{
			name: "入口下游有环",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a"), vNode("b"), vNode("c")},
				Edges: []EdgeSpec{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "b"}}},
			wantSub: "有向环",
		},
		{
			name: "不可达节点-游离环",
			g: GraphSpec{Name: "x", Nodes: []NodeSpec{vNode("a"), vNode("c"), vNode("d")},
				Edges: []EdgeSpec{{From: "c", To: "d"}, {From: "d", To: "c"}}},
			wantSub: "不可达",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.g.Validate()
			if err == nil {
				t.Fatalf("应报错 (含 %q), 实际通过", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 %q 不含期望子串 %q", err.Error(), tc.wantSub)
			}
		})
	}
}
