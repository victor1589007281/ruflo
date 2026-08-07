package agent

// subagent_span_test —— design/01 §4.8 图外派生可见性的验收。
//
// ---------------------------------------------------------------------------
// 变异反证 (证明这些断言不是许愿; 每条都真跑过一次红)
// ---------------------------------------------------------------------------
//
//	① 把 agent.go 里的 `t.observeSubagent(...)` 整行删掉
//	   → Test图外派生_产subagent轨迹      FAIL "span 数 = 0, 期望 1"
//	   → Test图外派生_进hook总线          FAIL "总线未收到派生事件"
//	② 把 observeSubagent 的 Span.NodeID 从 ids.NodeID 改成 SubagentNodeID(...)
//	   (即"给子代理一个自己的 NodeID"那个更直觉的做法)
//	   → Test图外派生_token仍归父节点     FAIL "token 归到了 impl~sa..., 父节点预算看不见"
//	③ 把 observeSubagent 里的 hook 事件改成 pre/post 各发一条
//	   → Test图外派生_一次派生只计一次    FAIL "Subagent 计数 = 2, 期望 1"
//	④ 把 Call 里的 `depth := SubagentDepth(ctx) + 1` 改成常量 1
//	   → Test图外派生_深度可累加          FAIL "第二层 depth = 1, 期望 2"
//	⑤ 把 subagentSpanName 的 firstNonEmpty 顺序换成 description 优先
//	   → Test图外派生_产subagent轨迹      FAIL "Name = \"查一下\", 期望 \"researcher\""

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
)

// callAgentTool 走**真实的** AgentTool.Call (而不是直接调 observeSubagent):
// 可见性挂在 Call 上才算通电, 直接调内部方法证明不了生产路径会走到它。
func callAgentTool(t *testing.T, ctx context.Context, at *AgentTool, in map[string]any) *tool.ToolResult {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("构造入参失败: %v", err)
	}
	res, err := at.Call(ctx, raw, &tool.ToolContext{})
	if err != nil {
		t.Fatalf("AgentTool.Call 返回 err: %v", err)
	}
	return res
}

// recordingObserver 收集打进 internal_hook 观测者的事件 (= 生产里桥进 graph.HookBus 的那条通道)。
type recordingObserver struct {
	mu     sync.Mutex
	events []internal_hook.Event
}

func (o *recordingObserver) Observe(_ context.Context, ev internal_hook.Event) {
	o.mu.Lock()
	o.events = append(o.events, ev)
	o.mu.Unlock()
}

func (o *recordingObserver) countHook(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, e := range o.events {
		if e.Hook == name {
			n++
		}
	}
	return n
}

// 改造前: Agent 工具派生的子代理在轨迹上**一条都没有** (KindNode 只有图节点在产)。
func Test图外派生_产subagent轨迹(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	at := NewAgentToolWithTrace(func(context.Context, string, RunOptions) (string, error) {
		return "子代理产出", nil
	}, ts)

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-sa", NodeID: "impl", TurnID: "t7"})
	res := callAgentTool(t, ctx, at, map[string]any{
		"prompt": "查一下这个库怎么用", "description": "查一下", "subagent_type": "researcher",
	})
	if res.IsError || res.Content != "子代理产出" {
		t.Fatalf("派生产出被改动了: %+v", res)
	}

	spans := readSpans(t, ts, "run-sa")
	if len(spans) != 1 {
		t.Fatalf("span 数 = %d, 期望 1", len(spans))
	}
	s := spans[0]
	if s.Kind != tracestore.KindNode {
		t.Errorf("Kind = %q, 期望 %q (复用 node 语义, 不新造第 8 种)", s.Kind, tracestore.KindNode)
	}
	if s.Attrs["kind"] != subagentSpanKind {
		t.Errorf("attrs.kind = %v, 期望 %q —— 消费方靠它把子代理与图节点分开", s.Attrs["kind"], subagentSpanKind)
	}
	if s.Name != "researcher" {
		t.Errorf("Name = %q, 期望 %q", s.Name, "researcher")
	}
	if s.Attrs["path"] != "bare-engine" {
		t.Errorf("attrs.path = %v, 期望 bare-engine (与图内 ~sp 派生区分: 这条不进 journal)", s.Attrs["path"])
	}
	// 派生身份: 有一个限定 ID 可归因 (design/01 §4.8 "有 NodeID")。
	spawn, _ := s.Attrs["spawn_node"].(string)
	if !strings.HasPrefix(spawn, "impl"+SubagentIDInfix) {
		t.Errorf("spawn_node = %q, 期望以 impl%s 开头", spawn, SubagentIDInfix)
	}
	if !strings.Contains(s.InputRef.Inline, "查一下这个库") {
		t.Errorf("InputRef 未带 prompt: %+v", s.InputRef)
	}
	if !strings.Contains(s.OutputRef.Inline, "子代理产出") {
		t.Errorf("OutputRef 未带产出: %+v", s.OutputRef)
	}
}

// 失败的派生同样烧了 token 与墙钟, 只记成功会让"这个节点为什么跑了很久"不可解释。
func Test图外派生_失败也留痕(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	at := NewAgentToolWithTrace(func(context.Context, string, RunOptions) (string, error) {
		return "", errors.New("上游 503")
	}, ts)

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-err", NodeID: "impl"})
	res := callAgentTool(t, ctx, at, map[string]any{"prompt": "p"})
	if !res.IsError {
		t.Fatalf("派生失败必须仍然是 IsError 结果 (行为不能被观测改掉): %+v", res)
	}

	spans := readSpans(t, ts, "run-err")
	if len(spans) != 1 {
		t.Fatalf("span 数 = %d, 期望 1", len(spans))
	}
	if spans[0].Attrs["status"] != "error" {
		t.Errorf("status = %v, 期望 error", spans[0].Attrs["status"])
	}
	if e, _ := spans[0].Attrs["error"].(string); !strings.Contains(e, "503") {
		t.Errorf("error attr = %q, 期望含 503", e)
	}
}

// 派生事件进 hook 总线 —— 生产里由 graph_internal_bridge 桥进同一条 graph.HookBus,
// 于是节点收尾日志出现 "Subagent=N"。
func Test图外派生_进hook总线(t *testing.T) {
	obs := &recordingObserver{}
	at := NewAgentToolWithTrace(func(context.Context, string, RunOptions) (string, error) {
		return "ok", nil
	}, nil) // 轨迹未装配也要能进总线: 两条痕迹相互独立

	ctx := internal_hook.WithObserver(
		trace.With(context.Background(), trace.IDs{RunID: "r", NodeID: "impl"}), obs)
	callAgentTool(t, ctx, at, map[string]any{"prompt": "p"})

	if n := obs.countHook(subagentHookName); n != 1 {
		t.Fatalf("总线未收到派生事件: %s 计数 = %d, 期望 1", subagentHookName, n)
	}
	if obs.events[0].Scope != "tool" {
		t.Errorf("Scope = %q, 期望 tool (teamGraphHooks 按 turn|tool 分流到计数聚合)", obs.events[0].Scope)
	}
}

// 一次派生只能计一次: 发 pre/post 两条会让 "Subagent=2" 实际只是一次派生。
func Test图外派生_一次派生只计一次(t *testing.T) {
	obs := &recordingObserver{}
	at := NewAgentToolWithTrace(func(context.Context, string, RunOptions) (string, error) {
		return "ok", nil
	}, nil)
	ctx := internal_hook.WithObserver(context.Background(), obs)
	callAgentTool(t, ctx, at, map[string]any{"prompt": "p"})
	callAgentTool(t, ctx, at, map[string]any{"prompt": "q"})

	if n := obs.countHook(subagentHookName); n != 2 {
		t.Fatalf("%s 计数 = %d, 期望 2 (两次派生), 计数与派生次数必须 1:1", subagentHookName, n)
	}
}

// CLI 那条路径给子代理**又注册了一次** Agent 工具 (main.go:3427), 递归无限深。
// 深度经 ctx 累加才数得出来。
func Test图外派生_深度可累加(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	var at *AgentTool
	at = NewAgentToolWithTrace(func(ctx context.Context, prompt string, _ RunOptions) (string, error) {
		if prompt == "外层" {
			// 模拟 CLI 的递归: 子代理自己又调一次 Agent 工具, 复用同一个 ctx。
			callAgentToolNoT(ctx, at, `{"prompt":"内层"}`)
		}
		return "ok", nil
	}, ts)

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-d", NodeID: "impl"})
	callAgentTool(t, ctx, at, map[string]any{"prompt": "外层"})

	spans := readSpans(t, ts, "run-d")
	if len(spans) != 2 {
		t.Fatalf("span 数 = %d, 期望 2 (内外两层)", len(spans))
	}
	// 内层先结束先落盘。
	inner, outer := spans[0], spans[1]
	if d := jsonNum(inner.Attrs["depth"]); d != 2 {
		t.Errorf("第二层 depth = %v, 期望 2", d)
	}
	if d := jsonNum(outer.Attrs["depth"]); d != 1 {
		t.Errorf("第一层 depth = %v, 期望 1", d)
	}
}

func callAgentToolNoT(ctx context.Context, at *AgentTool, raw string) {
	_, _ = at.Call(ctx, json.RawMessage(raw), &tool.ToolContext{})
}

// jsonNum span 经 JSON 往返后数字是 float64。
func jsonNum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return -1
}

// ---------------------------------------------------------------------------
// 预算维度: 这是本项最容易做反的一条
// ---------------------------------------------------------------------------

// 子代理的 token 必须仍然记在**父节点**名下 —— 图侧 graphTokenReporter 取的正是
// 父节点执行前后 (RunID,NodeID) 上的差值, 归因一变父节点的 MaxTokens 闸就再也看不见
// 子代理烧的钱, 而那恰好是 §4.8 要修的洞。
//
// 这条同时是"图外派生已计入父节点预算"的通电证据。
func Test图外派生_token仍归父节点(t *testing.T) {
	ledger := api.DefaultTokenLedger
	const runID, nodeID = "run-tok", "impl"
	ledger.Forget(runID)
	t.Cleanup(func() { ledger.Forget(runID) })

	ts := tracestore.New(statestore.NewMemStore())
	at := NewAgentToolWithTrace(func(ctx context.Context, _ string, _ RunOptions) (string, error) {
		// 模拟嵌套引擎发了一次 LLM 调用: api.Client 按 trace.From(ctx) 记账
		// (client.go:1110)。这里直接用同一口径喂台账。
		ids := trace.From(ctx)
		fakeLLMCall(ledger, ids, 1234)
		return "ok", nil
	}, ts)

	ctx := trace.With(context.Background(), trace.IDs{RunID: runID, NodeID: nodeID})
	callAgentTool(t, ctx, at, map[string]any{"prompt": "p"})

	if got := ledger.Tokens(runID, nodeID); got != 1234 {
		t.Fatalf("父节点台账 = %d, 期望 1234 —— 子代理的 token 没记在父节点上, 预算闸看不见它", got)
	}
	spans := readSpans(t, ts, runID)
	if len(spans) != 1 {
		t.Fatalf("span 数 = %d", len(spans))
	}
	spawn, _ := spans[0].Attrs["spawn_node"].(string)
	if spans[0].NodeID != nodeID {
		t.Errorf("token 归到了 %q, 父节点预算看不见 (Span.NodeID 必须是父节点 %q; 派生身份在 spawn_node=%q)",
			spans[0].NodeID, nodeID, spawn)
	}
	if n := jsonNum(spans[0].Attrs["tokens"]); n != 1234 {
		t.Errorf("attrs.tokens = %v, 期望 1234 (台账已启用时 span 要能自述本次派生烧了多少)", n)
	}
}

// fakeLLMCall 按 api.TokenLedger 的公开口径记一次调用 (走真实拦截器链, 不 mock 内部)。
func fakeLLMCall(l *api.TokenLedger, ids trace.IDs, tokens int) {
	_, _ = l.Around(context.Background(), api.LLMCall{Trace: ids},
		func(context.Context, api.LLMCall) (api.LLMResult, error) {
			return api.LLMResult{Record: api.LLMCallRecord{TotalTokens: tokens}}, nil
		})
}

// 台账没启用时不写 tokens 字段 —— 写 0 会让"没开台账"与"真没烧"不可区分。
func Test图外派生_台账未启用不写假0(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	at := NewAgentToolWithTrace(func(context.Context, string, RunOptions) (string, error) {
		return "ok", nil
	}, ts)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-notok", NodeID: "n"})
	callAgentTool(t, ctx, at, map[string]any{"prompt": "p"})

	spans := readSpans(t, ts, "run-notok")
	if _, ok := spans[0].Attrs["tokens"]; ok {
		t.Errorf("台账无数据时不该写 tokens 字段, 实得 %v", spans[0].Attrs["tokens"])
	}
}

// ---------------------------------------------------------------------------
// 等价性: 未注入轨迹底座时, 行为必须与改造前一字不变
// ---------------------------------------------------------------------------

// 8+ 下游平台在用 :18080, runNestedAgent 是全部飞书会话的 Agent 工具路径。
// 观测不得改变: 返回内容、错误语义、传给 runAgent 的 RunOptions。
func Test图外派生_未装配轨迹时行为等价(t *testing.T) {
	var gotOpts []RunOptions
	var gotPrompts []string
	run := func(_ context.Context, p string, o RunOptions) (string, error) {
		gotPrompts = append(gotPrompts, p)
		gotOpts = append(gotOpts, o)
		return "产出:" + p, nil
	}
	// 老构造函数 (生产里 TraceStore 未装配时的等价形态)。
	old := NewAgentTool(run)
	in := map[string]any{
		"prompt": "干活", "subagent_type": "coder", "model": "m1", "readonly": true,
	}
	res := callAgentTool(t, context.Background(), old, in)
	if res.IsError || res.Content != "产出:干活" {
		t.Fatalf("产出被改动: %+v", res)
	}
	if len(gotOpts) != 1 {
		t.Fatalf("runAgent 调用次数 = %d, 期望 1 (观测不得多调一次)", len(gotOpts))
	}
	want := RunOptions{SubagentType: "coder", Model: "m1", ReadOnly: true}
	if gotOpts[0] != want {
		t.Errorf("RunOptions = %+v, 期望 %+v", gotOpts[0], want)
	}
	if gotPrompts[0] != "干活" {
		t.Errorf("prompt = %q, 期望原样透传", gotPrompts[0])
	}

	// 同一入参在装了轨迹底座之后, 返回值必须逐字节相同。
	gotOpts, gotPrompts = nil, nil
	withTrace := NewAgentToolWithTrace(run, tracestore.New(statestore.NewMemStore()))
	res2 := callAgentTool(t, context.Background(), withTrace, in)
	if res2.Content != res.Content || res2.IsError != res.IsError {
		t.Errorf("装了轨迹后产出不等价: %+v vs %+v", res2, res)
	}
	if len(gotOpts) != 1 || gotOpts[0] != want || gotPrompts[0] != "干活" {
		t.Errorf("装了轨迹后透传不等价: opts=%+v prompts=%v", gotOpts, gotPrompts)
	}
}

// 指纹而非序号: 父节点重跑时序号从 0 重来, 同一个 ~sa0 下的两条轨迹会被当成
// "同一个子代理跑了两次"。
func Test图外派生_命名空间用指纹(t *testing.T) {
	a := SubagentNodeID("impl", "任务甲")
	b := SubagentNodeID("impl", "任务乙")
	if a == b {
		t.Fatalf("不同 prompt 必须落不同命名空间: %q", a)
	}
	if a != SubagentNodeID("impl", "任务甲") {
		t.Errorf("同一 prompt 必须稳定: %q vs %q", a, SubagentNodeID("impl", "任务甲"))
	}
	// 会话路径没有父节点时不硬造假父节点名。
	if got := SubagentNodeID("", "x"); !strings.HasPrefix(got, SubagentIDInfix) {
		t.Errorf("无父节点时 = %q, 期望以 %q 开头", got, SubagentIDInfix)
	}
}

// nil 守卫: 轨迹是观测不是治理, 任何一处 nil 都不该把派生打断。
func Test图外派生_nil守卫(t *testing.T) {
	at := NewAgentToolWithTrace(func(context.Context, string, RunOptions) (string, error) {
		return "ok", nil
	}, nil)
	at.SetTraceStore(nil)
	res := callAgentTool(t, context.Background(), at, map[string]any{"prompt": "p"})
	if res.IsError || res.Content != "ok" {
		t.Fatalf("nil 轨迹底座下派生应照常: %+v", res)
	}
	var nilTool *AgentTool
	nilTool.SetTraceStore(nil) // 不 panic
	observeSubagent(nil, context.Background(), agentInput{}, 1, "", nil, time.Now(), 0)
}
