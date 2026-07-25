package agent

// graph_internal_bridge_test.go —— design/01 §4.5 "两套 hook 注册进同一总线" 的验收。
//
// 拆成两段, 因为它们各自能出错的方式不同:
//
//	① ctx 链不断  —— 观测者从 runGraphSpec 一路到达**agent runner 内部**。
//	   只测 internal_hook 侧的 Observer 证明不了这条: 中间任何一层换了 ctx
//	   (常见写法: 起新的 context.Background() 做超时) 都会让桥接静默断开,
//	   而"静默断开"正是本仓反复修过的那类缺陷。故用真的 stageNodeRunner 跑真的图。
//
//	② 事件真的落在同一条总线上 —— 桥接方把 internal_hook.Event 转成
//	   graph.HookEvent 并被 teamGraphHooks 认领、计数。
//
// 变异反证 (证明两段断言各自有牙):
//
//	M4 runGraphSpec 不注入桥接观测者
//	   → ① 红: "应有 6 个阶段的 runner 拿到带观测者的 ctx, 实际 0"
//	M5 HookChain 介入时不发事件 (只留报错事件)
//	   → ② 红: 计数 got ""; internal_hook/bus_test.go 同时三红

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/graph"
)

// bridgeStubHook 一个会"介入"的内置 hook (返回非 nil 结果 ⇒ 应产生总线事件)。
type bridgeStubHook struct{ internal_hook.BaseInternalHook }

func (h *bridgeStubHook) Execute(*internal_hook.HookContext) (*internal_hook.HookResult, error) {
	return &internal_hook.HookResult{SkipStreamDelta: true}, nil
}

func bridgeTestChain() *internal_hook.HookChain {
	c := internal_hook.NewHookChain()
	c.Register(&bridgeStubHook{internal_hook.BaseInternalHook{
		Name_:   "ToolGate",
		Phases_: []internal_hook.InternalHookPhase{internal_hook.PhasePreToolUse},
	}})
	c.Register(&bridgeStubHook{internal_hook.BaseInternalHook{
		Name_:   "AutoCompact",
		Phases_: []internal_hook.InternalHookPhase{internal_hook.PhasePreCompact},
	}})
	return c
}

// bridgeRunner 模拟 agent runner: 在自己的执行体里跑一条真的 internal_hook 链。
// 这一段在生产里是 sessionAgentRunner.Execute → engine.Query → queryLoop,
// 唯一被替掉的是"真的调 LLM"。
type bridgeRunner struct {
	chain *internal_hook.HookChain
	mu    *sync.Mutex
	saw   *int // 收到带观测者的 ctx 的次数
}

func (r *bridgeRunner) Execute(ctx context.Context, _ string) (string, error) {
	if internal_hook.ObserverFrom(ctx) != nil && r.saw != nil {
		r.mu.Lock()
		*r.saw++
		r.mu.Unlock()
	}
	// 工具相关 phase → tool 作用域; compact → turn 作用域。两档都发, 验收总线两个都认。
	hctx := func(turn int) *internal_hook.HookContext {
		return &internal_hook.HookContext{Ctx: ctx, TurnCount: turn}
	}
	_, _ = r.chain.Execute(internal_hook.PhasePreToolUse, hctx(1))
	_, _ = r.chain.Execute(internal_hook.PhasePreToolUse, hctx(2))
	_, _ = r.chain.Execute(internal_hook.PhasePreCompact, hctx(2))
	return "# 文章\n\n" + strings.Repeat("论据充分的段落内容。", 60), nil
}

// TestInternalHook桥接的ctx链端到端不断 —— ① 段验收。
//
// 走真正的装配路径 (runGraphSpec + stageNodeRunner + 生产 factory), 断言
// **每个阶段的 agent runner 内部**都拿到了带观测者的 ctx。少一个都说明中途被换掉了。
func TestInternalHook桥接的ctx链端到端不断(t *testing.T) {
	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")

	var mu sync.Mutex
	saw := 0
	chain := bridgeTestChain()

	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: t.TempDir(),
		Factory: func(_ context.Context, _, _ string) (AgentRunner, error) {
			return &bridgeRunner{chain: chain, mu: &mu, saw: &saw}, nil
		},
		Notify: func(_, _ string) {},
	})
	team, err := ptm.CreateTeam("bridge", "techblog", tbObjective, "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	wf := GetWorkflow("techblog")
	spec, err := graphSpecForWorkflow(wf)
	if err != nil {
		t.Fatalf("graphSpecForWorkflow: %v", err)
	}
	we := ptm.newRunExecutor(team, ptm.newRunCoordinator(team, false))

	results, err := we.runGraphSpec(context.Background(), wf, spec, tbObjective, team,
		func(s graph.GraphSpec) graph.NodeRunner {
			return &stageNodeRunner{we: we, team: team, objective: tbObjective, deps: graphNodeDeps(s)}
		})
	if err != nil {
		t.Fatalf("runGraphSpec: %v", err)
	}
	if len(results) != len(wf.Stages) {
		t.Fatalf("阶段数不符: got %d want %d", len(results), len(wf.Stages))
	}
	mu.Lock()
	got := saw
	mu.Unlock()
	if got != len(wf.Stages) {
		t.Errorf("应有 %d 个阶段的 runner 拿到带观测者的 ctx, 实际 %d —— ctx 链在某一层被换掉了",
			len(wf.Stages), got)
	}
}

// TestInternalHook事件落在同一条总线上 —— ② 段验收。
func TestInternalHook事件落在同一条总线上(t *testing.T) {
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: t.TempDir(),
		Factory: func(_ context.Context, _, _ string) (AgentRunner, error) { return nil, nil },
		Notify:  func(_, _ string) {},
	})
	team, err := ptm.CreateTeam("bus", "techblog", tbObjective, "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	wf := GetWorkflow("techblog")
	spec, err := graphSpecForWorkflow(wf)
	if err != nil {
		t.Fatalf("graphSpecForWorkflow: %v", err)
	}
	we := ptm.newRunExecutor(team, ptm.newRunCoordinator(team, false))

	h := newTeamGraphHooks(we, team, spec)
	ctx := withInternalHookBridge(context.Background(), h, "run-bridge")
	var mu sync.Mutex
	if _, err := (&bridgeRunner{chain: bridgeTestChain(), mu: &mu}).Execute(ctx, ""); err != nil {
		t.Fatalf("runner: %v", err)
	}
	// 不在图节点内 (无 trace NodeID) 的事件归哑桶, 但**一条都不能丢** ——
	// 丢掉会让"总数对不上"这种最容易发现的异常也看不出来。
	if got := h.internalHookSummary("(no-node)"); got != "AutoCompact=1,ToolGate=2" {
		t.Errorf("总线上的内置 hook 计数不符: got %q, want \"AutoCompact=1,ToolGate=2\"", got)
	}
	// 没有干预的节点必须返回空串: 调用方据此**完全不加日志字段**, 既有日志形态不变。
	if got := h.internalHookSummary("source-analysis"); got != "" {
		t.Errorf("无干预的节点摘要应为空, got %q", got)
	}
}

// TestInternalHook桥接对空总线是零开销 —— NopBus 会把每条事件都扔掉,
// 挂上去只是白付 ctx.Value + Emit; 这里钉住"根本不挂"。
func TestInternalHook桥接对空总线是零开销(t *testing.T) {
	if internal_hook.ObserverFrom(withInternalHookBridge(context.Background(), graph.NopBus{}, "r")) != nil {
		t.Error("NopBus 不应挂观测者")
	}
	if internal_hook.ObserverFrom(withInternalHookBridge(context.Background(), nil, "r")) != nil {
		t.Error("nil 总线不应挂观测者")
	}
}

// TestInternalHook桥接绝不阻塞 —— 桥接方必须丢弃总线决策。
// 即便总线返回 deny, 观测路径也不得变成一条新的阻塞路径 (灰度期纪律)。
// 注意验收的是"发了事件且丢了决策", 而不是"干脆不发事件"——后者也不会阻塞, 但那是
// 把可见性一起丢掉了。
func TestInternalHook桥接绝不阻塞(t *testing.T) {
	deny := &denyBus{}
	b := &internalHookBridge{bus: deny, runID: "r"}
	b.Observe(context.Background(), internal_hook.Event{Scope: "tool", Phase: "PreToolUse", Hook: "X"})
	if deny.n != 1 {
		t.Fatalf("事件应到达总线一次, got %d", deny.n)
	}
}

type denyBus struct{ n int }

func (d *denyBus) Emit(context.Context, graph.HookEvent) graph.HookDecision {
	d.n++
	return graph.HookDecision{Action: graph.HookDeny, Reason: "test"}
}
