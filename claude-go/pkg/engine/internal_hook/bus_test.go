package internal_hook

// bus_test.go —— design/01 §4.5: internal_hook 事件注册进同一总线的验收。

import (
	"context"
	"errors"
	"testing"
)

// recObserverT 记录收到的事件。
type recObserverT struct{ got []Event }

func (r *recObserverT) Observe(_ context.Context, ev Event) { r.got = append(r.got, ev) }

// noopHook 什么都不做的 hook (返回 nil 结果)。
type noopHook struct{ BaseInternalHook }

func (h *noopHook) Execute(*HookContext) (*HookResult, error) { return nil, nil }

// actHook 有介入的 hook (返回非 nil 结果)。
type actHook struct{ BaseInternalHook }

func (h *actHook) Execute(*HookContext) (*HookResult, error) {
	return &HookResult{SkipStreamDelta: true}, nil
}

// errHook 报错的 hook。
type errHook struct{ BaseInternalHook }

func (h *errHook) Execute(*HookContext) (*HookResult, error) {
	return nil, errors.New("boom")
}

func newChainWith(hs ...InternalHook) *HookChain {
	c := NewHookChain()
	for _, h := range hs {
		c.Register(h)
	}
	return c
}

// TestBus未挂观测者时零事件 无观测者 = 不改变任何行为 (默认行为不变的底线)。
func TestBus未挂观测者时零事件(t *testing.T) {
	c := newChainWith(&actHook{BaseInternalHook{Name_: "Act", Phases_: []InternalHookPhase{PhasePreToolUse}}})
	r, err := c.Execute(PhasePreToolUse, &HookContext{Ctx: context.Background()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r == nil || !r.SkipStreamDelta {
		t.Fatalf("hook 结果被观测逻辑改动了: %+v", r)
	}
	// 没有可断言的"零事件"直接证据 (没有观测者), 但 ObserverFrom 必须返回 nil,
	// 否则 emit 会走进非空分支。
	if ObserverFrom(context.Background()) != nil {
		t.Error("空 ctx 不应有观测者")
	}
}

// TestBus只为真正介入的hook发事件 —— 每 turn 11 个 phase, 给"什么都没做"发事件
// 既淹没信号又白烧最热路径 (bus.go 文件头第 3 点)。
func TestBus只为真正介入的hook发事件(t *testing.T) {
	obs := &recObserverT{}
	c := newChainWith(
		&noopHook{BaseInternalHook{Name_: "Noop", Priority_: 1, Phases_: []InternalHookPhase{PhasePreToolUse}}},
		&actHook{BaseInternalHook{Name_: "Act", Priority_: 2, Phases_: []InternalHookPhase{PhasePreToolUse}}},
	)
	ctx := WithObserver(context.Background(), obs)
	if _, err := c.Execute(PhasePreToolUse, &HookContext{Ctx: ctx, TurnCount: 7}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(obs.got) != 1 {
		t.Fatalf("应只为介入的 hook 发 1 条事件, got %d: %+v", len(obs.got), obs.got)
	}
	ev := obs.got[0]
	if ev.Hook != "Act" {
		t.Errorf("事件来源应是 Act, got %q", ev.Hook)
	}
	if ev.Scope != "tool" {
		t.Errorf("PreToolUse 应归 tool 作用域, got %q", ev.Scope)
	}
	if ev.Phase != "PreToolUse" {
		t.Errorf("phase 名不符: %q", ev.Phase)
	}
	if ev.TurnCount != 7 {
		t.Errorf("TurnCount 应原样透传, got %d", ev.TurnCount)
	}
}

// TestBus非工具phase归turn作用域 —— 作用域只有 turn/tool 两档 (见 scopeOf 注释)。
func TestBus非工具phase归turn作用域(t *testing.T) {
	obs := &recObserverT{}
	c := newChainWith(&actHook{BaseInternalHook{Name_: "Compact", Phases_: []InternalHookPhase{PhasePreCompact}}})
	ctx := WithObserver(context.Background(), obs)
	if _, err := c.Execute(PhasePreCompact, &HookContext{Ctx: ctx}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(obs.got) != 1 || obs.got[0].Scope != "turn" {
		t.Fatalf("PreCompact 应归 turn 作用域, got %+v", obs.got)
	}
}

// TestBusHook报错也发事件 —— 链在这里中断, 不发事件的话图层只看得到"少了点什么",
// 看不到是谁断的。
func TestBusHook报错也发事件(t *testing.T) {
	obs := &recObserverT{}
	c := newChainWith(&errHook{BaseInternalHook{Name_: "Boom", Phases_: []InternalHookPhase{PhaseOnError}}})
	ctx := WithObserver(context.Background(), obs)
	if _, err := c.Execute(PhaseOnError, &HookContext{Ctx: ctx}); err == nil {
		t.Fatal("应返回错误")
	}
	if len(obs.got) != 1 || obs.got[0].Err == "" {
		t.Fatalf("报错事件缺失或未带 Err: %+v", obs.got)
	}
}

// TestBus观测者不能改变链的结果 —— 这是"绝不新增阻塞路径"的行为面证据:
// Observer 无返回值 (类型上说不出阻塞), 这里再钉住它连结果都碰不到。
func TestBus观测者不能改变链的结果(t *testing.T) {
	act := &actHook{BaseInternalHook{Name_: "Act", Phases_: []InternalHookPhase{PhasePostToolUse}}}
	c := newChainWith(act)

	base, errBase := c.Execute(PhasePostToolUse, &HookContext{Ctx: context.Background()})
	obs := &recObserverT{}
	withObs, errObs := c.Execute(PhasePostToolUse, &HookContext{
		Ctx: WithObserver(context.Background(), obs)})

	if (errBase == nil) != (errObs == nil) {
		t.Fatalf("挂观测者改变了错误返回: base=%v obs=%v", errBase, errObs)
	}
	if (base == nil) != (withObs == nil) {
		t.Fatalf("挂观测者改变了结果是否为空: base=%v obs=%v", base, withObs)
	}
	// HookResult 含切片, 不能直接 ==; 逐个比对本用例涉及的字段与各切片长度。
	if base != nil {
		if base.SkipStreamDelta != withObs.SkipStreamDelta ||
			base.SkipRemaining != withObs.SkipRemaining ||
			base.InjectContinue != withObs.InjectContinue ||
			base.Backoff != withObs.Backoff ||
			len(base.Messages) != len(withObs.Messages) ||
			len(base.AppendMsgs) != len(withObs.AppendMsgs) ||
			len(base.SystemPrompt) != len(withObs.SystemPrompt) ||
			len(base.ToolUseBlocks) != len(withObs.ToolUseBlocks) ||
			len(base.AssistantBlocks) != len(withObs.AssistantBlocks) ||
			len(base.StreamEvents) != len(withObs.StreamEvents) {
			t.Fatalf("挂观测者改变了链结果:\n  base=%+v\n  obs =%+v", *base, *withObs)
		}
	}
	if len(obs.got) == 0 {
		t.Fatal("观测者应收到事件 (否则上面的比对是空对空)")
	}
}
