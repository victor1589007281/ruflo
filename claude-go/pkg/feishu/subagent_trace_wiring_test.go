package feishu

// subagent_trace_wiring_test —— design/01 §4.8 图外派生可见性在**飞书这一侧**的通电验收。
//
// pkg/agent 侧的单测证明的是"AgentTool 会写 Span";本文件证明的是"飞书装配真的把
// 轨迹底座接进去了" —— 这两件事在本仓被区分过很多次: 能力写完了但装配处没接,
// 症状与"没实现"完全一样。所以这里走的是**真实的 newProfileRegistry / runNestedAgent**,
// 不构造任何 AgentTool。
//
// ---------------------------------------------------------------------------
// 变异反证 (每条都真跑过一次红)
// ---------------------------------------------------------------------------
//
//	① session.go:366 改回 agent.NewAgentTool(opts.runAgentFn)
//	   → TestSubagent派生轨迹已接进会话工具表  FAIL "会话 Agent 工具未接轨迹底座: span 数 = 0"
//	② 删掉 runNestedAgent 里的 nested.TraceStore = sm.traceStore
//	   → TestSubagent嵌套引擎已接轨迹底座      FAIL "嵌套引擎未接轨迹底座: llm_call span 数 = 0"

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/trace"
)

// 会话工具表里的 Agent 工具必须带轨迹底座 —— 它是全部飞书会话的派生入口。
func TestSubagent派生轨迹已接进会话工具表(t *testing.T) {
	ss := statestore.NewMemStore()
	sm := newTestSessionManager(t, ss, nil)
	if sm.TraceStore() == nil {
		t.Fatal("测试前提不成立: SessionManager 没有轨迹底座")
	}

	// 走真实装配路径; runAgentFn 用桩替掉真派生 (本用例验的是接线, 不是派生本身)。
	reg := sm.newProfileRegistry(builtin.ToolProfileChat, registryOptions{
		includeAgent: true,
		runAgentFn: func(context.Context, string, agent.RunOptions) (string, error) {
			return "子代理产出", nil
		},
	})
	at, ok := reg.Get(agent.AgentToolName)
	if !ok {
		t.Fatal("会话工具表里没有 Agent 工具")
	}

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-wire", NodeID: "impl"})
	in, _ := json.Marshal(map[string]any{"prompt": "去查一下", "subagent_type": "researcher"})
	if _, err := at.Call(ctx, in, &tool.ToolContext{}); err != nil {
		t.Fatalf("Agent 工具调用失败: %v", err)
	}

	spans, err := sm.TraceStore().ReadRun("run-wire")
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	var sub *tracestore.Span
	for i := range spans {
		if spans[i].Attrs["kind"] == "subagent" {
			sub = &spans[i]
		}
	}
	if sub == nil {
		t.Fatalf("会话 Agent 工具未接轨迹底座: span 数 = %d, 无 kind=subagent", len(spans))
	}
	if sub.NodeID != "impl" {
		t.Errorf("Span.NodeID = %q, 期望父节点 impl (token 归因靠它)", sub.NodeID)
	}
	if sub.Name != "researcher" {
		t.Errorf("Span.Name = %q, 期望 researcher", sub.Name)
	}
}

// 嵌套子代理**自己那个引擎**也要接轨迹底座: 改造前它恒 nil, 于是子代理烧的每一次
// LLM 调用在轨迹上都没有记录 (父会话有、子代理没有)。
func TestSubagent嵌套引擎已接轨迹底座(t *testing.T) {
	srv := newFakeLLMServer(t, "子代理回答")
	defer srv.Close()

	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, srv.URL, nil)
	if sm.TraceStore() == nil {
		t.Fatal("测试前提不成立: SessionManager 没有轨迹底座")
	}

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-nested", NodeID: "impl"})
	out, err := sm.runNestedAgent(ctx, nil, "去查一下", agent.RunOptions{SubagentType: "researcher"})
	if err != nil {
		t.Fatalf("嵌套派生失败: %v", err)
	}
	if out != "子代理回答" {
		t.Fatalf("嵌套派生产出 = %q, 期望 子代理回答 (行为不得被观测改变)", out)
	}

	spans, err := sm.TraceStore().ReadRun("run-nested")
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	n := 0
	for _, s := range spans {
		if s.Kind == tracestore.KindLLMCall {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("嵌套引擎未接轨迹底座: llm_call span 数 = 0 (总 span %d)", len(spans))
	}
}
