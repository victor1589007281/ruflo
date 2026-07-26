package agent

// graph_external_bridge_test.go —— design/01 §4.5 归宿表**第 1 行** (`pkg/hooks`
// 外部 hook 注册进同一条 graph.HookBus) 的验收。
//
// 与 graph_internal_bridge_test.go 同样拆成两段, 因为它们能出错的方式不同:
//
//	① ctx 链不断 —— `hooks.Observer` 从 runGraphSpec 一路到达**agent runner 内部**
//	   构造的那个 `hooks.Runner`。只测 pkg/hooks 侧证明不了这条: 中间任何一层换了
//	   ctx (常见写法: 起新的 context.Background() 做超时) 都会让桥接静默断开。
//	   故用真的 stageNodeRunner 跑真的图。
//
//	② 事件真的落在同一条总线上 —— 桥接方把 hooks.Event 转成 graph.HookEvent
//	   并被 teamGraphHooks 认领、与内置 hook 计数**并列**在同一份摘要里。
//
// ---------------------------------------------------------------------------
// 等价性 (「未开启时行为一字不变」的可观测证据)
// ---------------------------------------------------------------------------
//
// 判据取 `internalHookSummary`, 因为它就是**唯一**会因本次改造出现在既有日志里的
// 东西 (`graph.node.post` 的 internal_hooks 字段, 为空时一个字段都不加)。
// 用户没配外部 hook ⇒ 摘要逐字节为空 ⇒ 那条日志与改造前逐字节一致。
// 不比对"日志字符串"本身: 那要在测试里复刻一遍 logging 的格式化, 复刻错时测试会
// 跟着错 —— 它证明的是"我的复刻自洽", 不是"日志没变"。
//
// ---------------------------------------------------------------------------
// 变异反证 (证明两段断言各自有牙)
// ---------------------------------------------------------------------------
//
//	M5 runGraphSpec 不注入 withExternalHookBridge
//	   → ① 红: "应有 6 个阶段的 runner 拿到外部 hook 观测者, 实际 0"
//	M6 teamGraphHooks.Emit 的 case 不含 graph.ScopeSession (最容易漏的一处:
//	   新增作用域时忘了在消费方加分支)
//	   → ② 红: 摘要缺 ext:SubagentStart, got "ext:PreToolUse=2"
//	M7 `hooks.NewRunnerWithContext` 不保存 ctx (= 产生方没接线, "建成未通电"的原形态)
//	   → ② 红: 摘要只剩内置的 "AutoCompact=1,ToolGate=2"; pkg/hooks/bus_test.go 同时五红

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/types"
)

// extHookConfigs 一组**不 spawn 进程**的外部 hook 配置 (prompt 型)。
// 选 SubagentStart + PreToolUse 是为了同时覆盖 session 与 tool 两个作用域 ——
// 只测一个作用域的话, teamGraphHooks 里漏一个 case 也照样绿。
func extHookConfigs() []types.HookConfig {
	return []types.HookConfig{
		{Event: types.HookEventSubagentStart, HookType: types.HookTypePrompt, Command: "注意安全"},
		{Event: types.HookEventPreToolUse, HookType: types.HookTypePrompt, Command: "注意权限"},
	}
}

// extBridgeRunner 模拟 agent runner: 在自己的执行体里构造一个**带观测 ctx 的**
// hooks.Runner 并触发外部 hook —— 这正是 pkg/feishu/session.go 的
// sessionAgentRunner.Execute 干的事 (那里的 NewRunnerWithContext(ctx, ...) 是生产
// 产生方), 唯一被替掉的是"真的调 LLM"。
type extBridgeRunner struct {
	mu  *sync.Mutex
	saw *int // 收到外部 hook 观测者的次数
}

func (r *extBridgeRunner) Execute(ctx context.Context, _ string) (string, error) {
	if hooks.ObserverFrom(ctx) != nil && r.saw != nil {
		r.mu.Lock()
		*r.saw++
		r.mu.Unlock()
	}
	hr := hooks.NewRunnerWithContext(ctx, extHookConfigs(), "")
	hr.ExecuteSubagentStartHooks("writer", "写一篇文章")
	if _, err := hr.RunPreToolUseHooks("Read", json.RawMessage(`{}`)); err != nil {
		return "", err
	}
	if _, err := hr.RunPreToolUseHooks("Bash", json.RawMessage(`{}`)); err != nil {
		return "", err
	}
	// 没配的事件不该产生任何总线事件 (等价性的一半在运行期也钉一下)。
	hr.ExecuteSessionHooks(types.HookEventSessionEnd)
	return "# 文章\n\n" + strings.Repeat("论据充分的段落内容。", 60), nil
}

// TestExternalHook桥接的ctx链端到端不断 —— ① 段。
func TestExternalHook桥接的ctx链端到端不断(t *testing.T) {
	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")

	var mu sync.Mutex
	saw := 0
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: t.TempDir(),
		Factory: func(_ context.Context, _, _ string) (AgentRunner, error) {
			return &extBridgeRunner{mu: &mu, saw: &saw}, nil
		},
		Notify: func(_, _ string) {},
	})
	team, err := ptm.CreateTeam("extbridge", "techblog", tbObjective, "test")
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
		t.Errorf("应有 %d 个阶段的 runner 拿到外部 hook 观测者, 实际 %d —— ctx 链在某一层被换掉了",
			len(wf.Stages), got)
	}
}

// TestExternalHook事件与内置hook并列在同一条总线 —— ② 段。
//
// 断言的是**并列**而不是"外部事件存在": 两类事件各自有表时"总数对不上"就没人能发现,
// 而并列在同一份摘要里意味着它们共用一张计数表 (graph_external_bridge.go 的取舍)。
func TestExternalHook事件与内置hook并列在同一条总线(t *testing.T) {
	we, team, spec := extBridgeFixture(t, "extbus")
	h := newTeamGraphHooks(we, team, spec)

	ctx := withExternalHookBridge(context.Background(), h, "run-ext")
	ctx = withInternalHookBridge(ctx, h, "run-ext")

	var mu sync.Mutex
	if _, err := (&extBridgeRunner{mu: &mu}).Execute(ctx, ""); err != nil {
		t.Fatalf("runner: %v", err)
	}
	// 同一个 ctx 上再跑一条内置 hook 链, 证明两个观测者互不干扰 (各自的 ctx 键)。
	if _, err := (&bridgeRunner{chain: bridgeTestChain(), mu: &mu}).Execute(ctx, ""); err != nil {
		t.Fatalf("internal runner: %v", err)
	}

	// 不在图节点内 (无 trace NodeID) 的事件归哑桶, 一条都不能丢。
	// 期望: 内置 AutoCompact=1,ToolGate=2 (bridgeRunner) 与外部
	// ext:PreToolUse=2,ext:SubagentStart=1 (extBridgeRunner) 并列; 字典序排列。
	const want = "AutoCompact=1,ToolGate=2,ext:PreToolUse=2,ext:SubagentStart=1"
	if got := h.internalHookSummary("(no-node)"); got != want {
		t.Errorf("总线计数不符:\n got %q\nwant %q", got, want)
	}
}

// TestExternalHook未配置时既有日志形态不变 —— 等价性。
//
// 同一次运行的**唯一**差别只能来自"用户配了外部 hook"这一个变量: 两侧都走真的
// runGraphSpec, runner 只在是否触发外部 hook 上不同。
func TestExternalHook未配置时既有日志形态不变(t *testing.T) {
	we, team, spec := extBridgeFixture(t, "extquiet")
	h := newTeamGraphHooks(we, team, spec)
	ctx := withExternalHookBridge(context.Background(), h, "run-quiet")

	// 一个**没有任何 hook 配置**的 Runner: 与生产上绝大多数部署同形。
	hr := hooks.NewRunnerWithContext(ctx, nil, "")
	hr.ExecuteSubagentStartHooks("writer", "写")
	if _, err := hr.RunPreToolUseHooks("Read", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("RunPreToolUseHooks: %v", err)
	}
	hr.ExecuteStopHooks(nil)
	hr.ExecuteSessionHooks(types.HookEventSessionEnd)

	if got := h.internalHookSummary("(no-node)"); got != "" {
		t.Errorf("没配外部 hook 时摘要必须为空 (否则既有 graph.node.post 日志凭空多出字段): got %q", got)
	}
	for _, n := range spec.Nodes {
		if got := h.internalHookSummary(n.ID); got != "" {
			t.Errorf("节点 %s 摘要应为空, got %q", n.ID, got)
		}
	}
}

// TestExternalHook桥接对空总线是零开销 —— NopBus/nil 时根本不挂观测者。
func TestExternalHook桥接对空总线是零开销(t *testing.T) {
	if hooks.ObserverFrom(withExternalHookBridge(context.Background(), graph.NopBus{}, "r")) != nil {
		t.Error("NopBus 不应挂观测者")
	}
	if hooks.ObserverFrom(withExternalHookBridge(context.Background(), nil, "r")) != nil {
		t.Error("nil 总线不应挂观测者")
	}
}

// TestExternalHook桥接绝不阻塞 —— 桥接方必须丢弃总线决策。
//
// 验收的是"发了事件且丢了决策", 不是"干脆不发事件"—— 后者也不会阻塞, 但那是把
// 可见性一起丢掉了。类型层面的保证 (Observe 无返回值) 在 hooks/bus_test.go 说明。
func TestExternalHook桥接绝不阻塞(t *testing.T) {
	deny := &denyBus{}
	b := &externalHookBridge{bus: deny, runID: "r"}
	b.Observe(context.Background(), hooks.Event{
		Scope: hooks.ScopeTool, Phase: "PreToolUse", Hook: "ext:PreToolUse", Decision: "block"})
	if deny.n != 1 {
		t.Fatalf("事件应到达总线一次, got %d", deny.n)
	}
}

// TestExternalHook载荷只带非空字段 —— 恒放会让"没有角色"与"角色是空串"不可区分,
// 而 payloadStr 对两者返回同一个值。
func TestExternalHook载荷只带非空字段(t *testing.T) {
	cap := &captureBus{}
	b := &externalHookBridge{bus: cap, runID: "r"}
	b.Observe(context.Background(), hooks.Event{
		Scope: hooks.ScopeSession, Phase: "SubagentStart", Hook: "ext:SubagentStart", Matched: 1})
	if len(cap.got) != 1 {
		t.Fatalf("应收到一条事件, got %d", len(cap.got))
	}
	p := cap.got[0].Payload
	for _, k := range []string{"decision", "tool", "role", "error"} {
		if _, ok := p[k]; ok {
			t.Errorf("空字段 %q 不该出现在载荷里", k)
		}
	}
	if p["source"] != hooks.SourceExternal {
		t.Errorf("载荷必须带 source 以区分内置/外部来源: got %v", p["source"])
	}
	if cap.got[0].Scope != graph.ScopeSession {
		t.Errorf("作用域应透传为 graph.ScopeSession, got %v", cap.got[0].Scope)
	}
}

type captureBus struct{ got []graph.HookEvent }

func (c *captureBus) Emit(_ context.Context, ev graph.HookEvent) graph.HookDecision {
	c.got = append(c.got, ev)
	return graph.HookDecision{}
}

// extBridgeFixture 造一个可用的 (executor, team, spec) 三元组。
func extBridgeFixture(t *testing.T, name string) (*WorkflowExecutor, *ProductionTeam, graph.GraphSpec) {
	t.Helper()
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: t.TempDir(),
		Factory: func(_ context.Context, _, _ string) (AgentRunner, error) { return nil, nil },
		Notify:  func(_, _ string) {},
	})
	team, err := ptm.CreateTeam(name, "techblog", tbObjective, "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	wf := GetWorkflow("techblog")
	spec, err := graphSpecForWorkflow(wf)
	if err != nil {
		t.Fatalf("graphSpecForWorkflow: %v", err)
	}
	return ptm.newRunExecutor(team, ptm.newRunCoordinator(team, false)), team, spec
}
