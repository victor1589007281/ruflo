package hooks

// bus_test.go —— `ExternalHook` 适配器 (design/01 §4.5 归宿表第 1 行) 的验收。
//
// ---------------------------------------------------------------------------
// 这组测试在钉什么
// ---------------------------------------------------------------------------
//
// 四件事, 每件都对应一种"写了但等于没写"的失败形态:
//
//	① 没配 hook ⇒ **一条事件都不发**。这是"未开启时行为一字不变"的直接证据:
//	   绝大多数部署一条外部 hook 都没配, 若这里发了事件, 图层的节点收尾日志就会
//	   凭空多出字段 (下游有按日志比对的验收)。
//	② 配了 hook ⇒ 事件带着**正确的 Scope × Phase**。映射写反不会让任何东西报错,
//	   只会让总线消费方按错误的作用域分流 —— 静默且事后不可考。
//	③ **决策可见**。§4.5 明写"决策语义 deny/block/approve 保留"。若 Decision 恒空,
//	   "用户策略拦下了这次工具调用"在图层永远看不到, 而那正是接这条桥的主要动机。
//	④ **绝不新增阻塞路径**, 且外部 hook 自己的决策**一字不变**。观测者再慢/再想
//	   拦截也不能改变 RunPreToolUseHooks 的返回值。
//
// ---------------------------------------------------------------------------
// 变异反证 (证明上面这些断言不是许愿式的)
// ---------------------------------------------------------------------------
//
// 逐个改坏后本文件必红, 且红在语义正确的那一格:
//
//	M1 `observe` 去掉 `len(hooks)==0` 那道闸 (即无条件发事件)
//	   → ① 红: "没配 hook 时不应有事件, 实际 3 条"
//	M2 ScopeOf 把 SubagentStart 归到 turn (最容易手抖的一处: 它确实每轮都可能发生)
//	   → ② 红: "SubagentStart 作用域不符: got turn want session"
//	M3 emit 点不填 Decision (只发事件不带决策)
//	   → ③ 红: "PreToolUse 决策应可见: got \"\" want block"
//	M4 给 Observer.Observe 加 error 返回并在调用点 `if err != nil { return nil, err }`
//	   → 编译期就红 (类型上说不出这句话), 这正是把"绝不阻塞"钉在类型层面的目的
//
// M4 之所以列在这里而不是写成一个 Go 测试: 它不是运行期断言, 是**类型不变式**。
// 一个能编译过的"非阻塞测试"永远只能测当前实现, 测不掉将来有人加返回值这件事。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// recObs 记录收到的事件 (并发安全: PostToolUse 那条路径可能从多个 goroutine 打进来)。
type recObs struct {
	mu  sync.Mutex
	got []Event
}

func (r *recObs) Observe(_ context.Context, ev Event) {
	r.mu.Lock()
	r.got = append(r.got, ev)
	r.mu.Unlock()
}

func (r *recObs) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.got))
	copy(out, r.got)
	return out
}

// byPhase 取某个相位的事件 (不存在返回 nil)。
func (r *recObs) byPhase(phase string) *Event {
	for _, ev := range r.events() {
		if ev.Phase == phase {
			e := ev
			return &e
		}
	}
	return nil
}

// promptHook 一个**不 spawn 任何进程**的 hook 配置 (prompt 型只回 AdditionalContext)。
// 用它而不是 command 型: command 会真的起 sh 子进程, 在 -race 下拖慢且依赖环境。
func promptHook(ev types.HookEvent) types.HookConfig {
	return types.HookConfig{Event: ev, HookType: types.HookTypePrompt, Command: "额外上下文"}
}

// Test外部hook未配置时一条事件都不发 —— ① 段。
func TestExternalHook未配置时一条事件都不发(t *testing.T) {
	obs := &recObs{}
	ctx := WithObserver(context.Background(), obs)
	// 只配了一个 SessionStart hook: 其余事件的 findHooks 恒空。
	r := NewRunnerWithContext(ctx, []types.HookConfig{promptHook(types.HookEventSessionStart)}, "s")

	if _, err := r.RunPreToolUseHooks("Read", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("RunPreToolUseHooks: %v", err)
	}
	_ = r.RunPostToolUseHooks("Read", json.RawMessage(`{}`), "ok", false)
	_ = r.RunPostToolUseFailureHooks("Read", json.RawMessage(`{}`), "boom")
	r.ExecuteSubagentStartHooks("coder", "prompt")
	r.ExecuteStopHooks(nil)

	if got := obs.events(); len(got) != 0 {
		t.Errorf("没配 hook 时不应有事件, 实际 %d 条: %+v", len(got), got)
	}

	// 反面对照: 配了的那个事件必须发 —— 否则上面的"零事件"可能只是因为桥根本没通。
	r.ExecuteSessionHooks(types.HookEventSessionStart)
	if got := obs.events(); len(got) != 1 {
		t.Fatalf("配了的事件应发一条, 实际 %d 条", len(got))
	}
}

// TestExternalHook事件名映射到ScopeAndPhase —— ② 段。
//
// 逐个事件核对, 而不是抽查几个: 映射是一张表, 表里错一行的后果与错一整张一样
// (那一类事件在总线上被错误分流), 而抽查恰好会漏掉最容易手抖的那几行。
func TestExternalHook事件名映射到ScopeAndPhase(t *testing.T) {
	cases := []struct {
		ev    types.HookEvent
		scope string
	}{
		{types.HookEventPreToolUse, ScopeTool},
		{types.HookEventPostToolUse, ScopeTool},
		{types.HookEventPostToolUseFailure, ScopeTool},
		{types.HookEventSessionStart, ScopeSession},
		{types.HookEventSessionEnd, ScopeSession},
		{types.HookEventSubagentStart, ScopeSession},
		{types.HookEventSubagentStop, ScopeSession},
		{types.HookEventTeammateIdle, ScopeNode},
		{types.HookEventTaskCompleted, ScopeNode},
		{types.HookEventStop, ScopeTurn},
		{types.HookEventStopFailure, ScopeTurn},
		{types.HookEventPreCompact, ScopeTurn},
		{types.HookEventPreTurn, ScopeTurn},
		{types.HookEventPreRequest, ScopeTurn},
		{types.HookEventOnError, ScopeTurn},
		{types.HookEventOnRateLimit, ScopeTurn},
		{types.HookEventNotification, ScopeTurn},
	}
	for _, c := range cases {
		if got := ScopeOf(c.ev); got != c.scope {
			t.Errorf("%s 作用域不符: got %s want %s", c.ev, got, c.scope)
		}
	}

	// 真跑一遍, 证明 emit 点填的 Scope 与 ScopeOf 一致 (两处各写一遍就会漂移)。
	obs := &recObs{}
	ctx := WithObserver(context.Background(), obs)
	r := NewRunnerWithContext(ctx, []types.HookConfig{
		promptHook(types.HookEventSubagentStart),
		promptHook(types.HookEventPreToolUse),
	}, "s")
	r.ExecuteSubagentStartHooks("coder", "写代码")
	if _, err := r.RunPreToolUseHooks("Bash", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("RunPreToolUseHooks: %v", err)
	}

	sa := obs.byPhase(string(types.HookEventSubagentStart))
	if sa == nil {
		t.Fatal("SubagentStart 事件缺失")
	}
	if sa.Scope != ScopeSession {
		t.Errorf("SubagentStart 作用域不符: got %s want %s", sa.Scope, ScopeSession)
	}
	if sa.Role != "coder" {
		t.Errorf("SubagentStart 应带角色: got %q want \"coder\"", sa.Role)
	}
	if sa.Hook != "ext:SubagentStart" {
		t.Errorf("hook 名应带 ext: 前缀 (与内置 hook 名分命名空间): got %q", sa.Hook)
	}

	pt := obs.byPhase(string(types.HookEventPreToolUse))
	if pt == nil {
		t.Fatal("PreToolUse 事件缺失")
	}
	if pt.Scope != ScopeTool || pt.Tool != "Bash" {
		t.Errorf("PreToolUse 事件不符: scope=%s tool=%q", pt.Scope, pt.Tool)
	}
}

// TestExternalHook决策在总线上可见且执行语义不变 —— ③ + ④ 段。
//
// 用 httptest 起一个真的 http hook 返回 block: prompt 型表达不了决策, 而 command 型
// 要 spawn 进程。http 是本仓 hook 的一等类型, 且完全 hermetic。
func TestExternalHook决策在总线上可见且执行语义不变(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"block","reason":"策略拒绝"}`))
	}))
	defer srv.Close()

	cfg := []types.HookConfig{{
		Event: types.HookEventPreToolUse, HookType: types.HookTypeHTTP, URL: srv.URL,
	}}

	// —— 无观测者: 这是"改造前"的行为基线 ——
	base := NewRunner(cfg, "s")
	baseOut, baseErr := base.RunPreToolUseHooks("Bash", json.RawMessage(`{}`))

	// —— 有观测者, 且观测者是个"想拦人"的坏公民 (Observe 里干活/取消 ctx) ——
	obs := &recObs{}
	octx, cancel := context.WithCancel(context.Background())
	cancel() // 故意先取消: obsCtx 若被误接进 hook 执行路径, http hook 会直接失败
	r := NewRunnerWithContext(WithObserver(octx, obs), cfg, "s")
	gotOut, gotErr := r.RunPreToolUseHooks("Bash", json.RawMessage(`{}`))

	// ④ 执行语义逐项等价 —— 观测绝不改变返回值。
	if (baseErr == nil) != (gotErr == nil) {
		t.Fatalf("err 形态不等价: base=%v got=%v", baseErr, gotErr)
	}
	if (baseOut == nil) != (gotOut == nil) {
		t.Fatalf("output 形态不等价: base=%v got=%v", baseOut, gotOut)
	}
	if baseOut != nil && gotOut != nil {
		if baseOut.Decision != gotOut.Decision || baseOut.Reason != gotOut.Reason {
			t.Errorf("决策不等价: base=%+v got=%+v", *baseOut, *gotOut)
		}
	}
	if gotOut == nil || gotOut.Decision != "block" {
		t.Fatalf("hook 的 block 决策必须仍然生效 (fail-closed 不许改成 fail-open): got %+v", gotOut)
	}

	// ③ 决策在总线上可见。
	ev := obs.byPhase(string(types.HookEventPreToolUse))
	if ev == nil {
		t.Fatal("PreToolUse 事件缺失")
	}
	if ev.Decision != "block" {
		t.Errorf("PreToolUse 决策应可见: got %q want block", ev.Decision)
	}
	if ev.Matched != 1 {
		t.Errorf("Matched 应为 1 (配了几条 hook): got %d", ev.Matched)
	}
}

// TestExternalHook观测ctx不参与取消 —— 上一个测试里 cancel 过的 ctx 仍能跑通 http hook,
// 这里把这条不变式单独钉住 (它是 obsCtx 那个字段注释的可执行版本)。
//
// 为什么值得单测: 把观测 ctx 顺手接进 executeHook 的 timeout 派生是极自然的"顺手改进",
// 而后果是**图运行一被取消, 全部 Stop/SessionEnd hook 立刻失效** —— 那些正是用户
// 用来做收尾清理与审计的 hook, 静默不执行且无人察觉。
func TestExternalHook观测ctx不参与取消(t *testing.T) {
	obs := &recObs{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := NewRunnerWithContext(WithObserver(ctx, obs), []types.HookConfig{
		promptHook(types.HookEventSessionEnd),
	}, "s")
	if out := r.ExecuteSessionHooks(types.HookEventSessionEnd); out != nil {
		t.Fatalf("prompt 型 hook 无决策, 应返回 nil: %+v", out)
	}
	if len(obs.events()) != 1 {
		t.Errorf("ctx 已取消也必须照常执行并观测, 实际事件 %d 条", len(obs.events()))
	}
}

// TestExternalHook无观测者时零开销 —— NewRunner 与 ctx 无观测者两种形态都不该找观测者。
func TestExternalHook无观测者时零开销(t *testing.T) {
	if ObserverFrom(context.Background()) != nil {
		t.Error("空 ctx 不应有观测者")
	}
	if ObserverFrom(WithObserver(context.Background(), nil)) != nil {
		t.Error("WithObserver(nil) 不应挂观测者")
	}
	// NewRunner (无 obsCtx) 走 observe 时必须直接返回, 不 panic。
	NewRunner([]types.HookConfig{promptHook(types.HookEventSessionStart)}, "s").
		ExecuteSessionHooks(types.HookEventSessionStart)
}

// TestExternalHook执行报错也进总线 —— 外部 hook 是子进程/网络调用, 会失败;
// 改造前这类失败被 `continue` 静默吞掉, 图层永远看不到"用户的审计 hook 一直在报错"。
func TestExternalHook执行报错也进总线(t *testing.T) {
	obs := &recObs{}
	r := NewRunnerWithContext(WithObserver(context.Background(), obs), []types.HookConfig{{
		Event: types.HookEventPreToolUse, HookType: types.HookTypeHTTP, URL: "", Command: "",
	}}, "s")
	if _, err := r.RunPreToolUseHooks("Read", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("hook 报错不应传播给调用方 (改造前也不传播): %v", err)
	}
	ev := obs.byPhase(string(types.HookEventPreToolUse))
	if ev == nil {
		t.Fatal("PreToolUse 事件缺失")
	}
	if ev.Err == "" {
		t.Error("hook 自身报错应进总线载荷, got 空")
	}
}
