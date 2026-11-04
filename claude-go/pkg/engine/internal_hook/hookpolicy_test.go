// hookpolicy_test.go — Moqi R-H7 的验收: advisory 失败不拖垮整轮, critical 失败可区分。
//
// 与 ephemeral_test.go 的分工: 那条验"引擎在 provider 失败时照常回复", 这条验
// **链本身**的判据 (panic/timeout/critical 三态)。混在一起时两者都失败无法区分
// "判据错了"与"判据没接上"。
package internal_hook

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// fakeHook 是可编程的 hook: name/priority/phase + 行为 + 可选策略。
type fakeHook struct {
	BaseInternalHook
	exec func(*HookContext) (*HookResult, error)
	// policy 为 nil 时不实现 HookPolicy —— 即改造前的默认 (Critical, 无超时)。
	policy *fakePolicy
}

type fakePolicy struct {
	crit    Criticality
	timeout time.Duration
}

func (p *fakePolicy) Criticality() Criticality { return p.crit }
func (p *fakePolicy) Timeout() time.Duration   { return p.timeout }

// 让 fakeHook 只在 policy 非 nil 时才满足 HookPolicy 接口。
// Go 的接口判定是静态的, 所以这里必须用两个不同的具体类型: 一个嵌入 policy,
// 一个不嵌。
type fakeHookPlain struct{ *fakeHook }

type fakeHookWithPolicy struct {
	*fakeHook
	p *fakePolicy
}

func (h *fakeHookWithPolicy) Criticality() Criticality { return h.p.crit }
func (h *fakeHookWithPolicy) Timeout() time.Duration   { return h.p.timeout }

func newFake(name string, prio int, exec func(*HookContext) (*HookResult, error)) *fakeHook {
	return &fakeHook{
		BaseInternalHook: BaseInternalHook{Name_: name, Priority_: prio, Phases_: []InternalHookPhase{PhasePreRequest}},
		exec:             exec,
	}
}

func (h *fakeHook) Execute(ctx *HookContext) (*HookResult, error) { return h.exec(ctx) }

// asPlain 返回一个**不实现** HookPolicy 的 InternalHook (改造前默认行为)。
func (h *fakeHook) asPlain() InternalHook { return &plainWrap{Base: h} }

type plainWrap struct{ Base *fakeHook }

func (w *plainWrap) Name() string                                { return w.Base.Name() }
func (w *plainWrap) Priority() int                               { return w.Base.Priority() }
func (w *plainWrap) Phases() []InternalHookPhase                 { return w.Base.Phases() }
func (w *plainWrap) Execute(c *HookContext) (*HookResult, error) { return w.Base.Execute(c) }

// asAdvisory 返回一个声明 Advisory 的 hook。
func (h *fakeHook) asAdvisory(timeout time.Duration) InternalHook {
	return &advWrap{Base: h, timeout: timeout}
}

type advWrap struct {
	Base    *fakeHook
	timeout time.Duration
}

func (w *advWrap) Name() string                                { return w.Base.Name() }
func (w *advWrap) Priority() int                               { return w.Base.Priority() }
func (w *advWrap) Phases() []InternalHookPhase                 { return w.Base.Phases() }
func (w *advWrap) Execute(c *HookContext) (*HookResult, error) { return w.Base.Execute(c) }
func (w *advWrap) Criticality() Criticality                    { return Advisory }
func (w *advWrap) Timeout() time.Duration                      { return w.timeout }

// TestHookChain_AdvisoryPanicDoesNotAbortTurn 是核心判据:
// advisory hook panic 时链必须继续, 且本轮后续 hook 仍被执行。
func TestHookChain_AdvisoryPanicDoesNotAbortTurn(t *testing.T) {
	c := NewHookChain()
	c.Register(newFake("boom", 10, func(*HookContext) (*HookResult, error) {
		panic("boom")
	}).asAdvisory(0))
	var reached bool
	c.Register(newFake("after", 20, func(*HookContext) (*HookResult, error) {
		reached = true
		return nil, nil
	}).asAdvisory(0))

	res, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()})
	if err != nil {
		t.Fatalf("advisory panic 不应让链返回 error, 实际 %v", err)
	}
	if !reached {
		t.Fatal("advisory panic 后后续 hook 没有执行 —— 链被中断了")
	}
	_ = res
}

// TestHookChain_CriticalPanicAborts 是对照: critical hook panic 必须终止。
//
// 与上一条**同一条链结构**, 只改 criticality —— 这样失败一定来自这一维。
func TestHookChain_CriticalPanicAborts(t *testing.T) {
	c := NewHookChain()
	c.Register(newFake("boom", 10, func(*HookContext) (*HookResult, error) {
		panic("boom")
	}).asPlain())
	var reached bool
	c.Register(newFake("after", 20, func(*HookContext) (*HookResult, error) {
		reached = true
		return nil, nil
	}).asPlain())

	_, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()})
	if err == nil {
		t.Fatal("critical panic 应让链返回 error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("错误应可辨认为 panic, 实际 %v", err)
	}
	if reached {
		t.Fatal("critical 失败后链仍继续了 —— 终止语义没生效")
	}
}

// TestHookChain_AdvisoryErrorContinues 钉住 error 路径 (不只是 panic 路径)。
func TestHookChain_AdvisoryErrorContinues(t *testing.T) {
	c := NewHookChain()
	c.Register(newFake("bad", 10, func(*HookContext) (*HookResult, error) {
		return nil, errors.New("retrieval down")
	}).asAdvisory(0))
	var reached bool
	c.Register(newFake("after", 20, func(*HookContext) (*HookResult, error) {
		reached = true
		return &HookResult{SystemPrompt: []string{"ok"}}, nil
	}).asAdvisory(0))

	res, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background(), SystemPrompt: []string{"base"}})
	if err != nil {
		t.Fatalf("advisory error 不应中断链: %v", err)
	}
	if !reached {
		t.Fatal("后续 hook 未执行")
	}
	if res == nil || len(res.SystemPrompt) != 1 || res.SystemPrompt[0] != "ok" {
		t.Fatalf("后续 hook 的产出应正常合并, 实际 %+v", res)
	}
}

// TestHookChain_CriticalErrorStillAborts 是 error 路径的对照, 兼验旧行为不变。
func TestHookChain_CriticalErrorStillAborts(t *testing.T) {
	c := NewHookChain()
	c.Register(newFake("bad", 10, func(*HookContext) (*HookResult, error) {
		return nil, errors.New("gate failed")
	}).asPlain())
	_, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()})
	if err == nil || !strings.Contains(err.Error(), "gate failed") {
		t.Fatalf("critical error 应原样上抛, 实际 %v", err)
	}
}

// TestHookChain_AdvisoryTimeoutContinues 钉住超时语义, 并验"超时用的是 child ctx,
// 不取消调用方的 ctx" —— 后者若成立, 一个 advisory 超时会让整轮其他组件全部收到取消。
func TestHookChain_AdvisoryTimeoutContinues(t *testing.T) {
	c := NewHookChain()
	c.Register(newFake("slow", 10, func(hc *HookContext) (*HookResult, error) {
		// 等到超时 (或调用方取消) 才返回。
		<-hc.Ctx.Done()
		return nil, hc.Ctx.Err()
	}).asAdvisory(20 * time.Millisecond))

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Execute(PhasePreRequest, &HookContext{Ctx: parent})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("advisory 超时不应让链返回 error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("链没有在超时后继续 —— per-hook timeout 没生效")
	}

	if parent.Err() != nil {
		t.Fatalf("调用方 ctx 被取消了: %v —— 超时必须用 child ctx", parent.Err())
	}
}

// TestHookChain_ObserverSeesOutcomes 钉住可观测面: 每个 hook 的执行结果都要可读。
func TestHookChain_ObserverSeesOutcomes(t *testing.T) {
	var mu sync.Mutex
	var outcomes []HookOutcome
	c := NewHookChain()
	c.AddObserver(observerFunc(func(o HookOutcome) {
		mu.Lock()
		outcomes = append(outcomes, o)
		mu.Unlock()
	}))
	c.Register(newFake("ok", 10, func(*HookContext) (*HookResult, error) { return nil, nil }).asAdvisory(0))
	c.Register(newFake("bad", 20, func(*HookContext) (*HookResult, error) {
		return nil, errors.New("x")
	}).asAdvisory(0))

	if _, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(outcomes) != 2 {
		t.Fatalf("应记录 2 条 outcome, 实际 %d", len(outcomes))
	}
	if outcomes[1].Err == nil {
		t.Error("失败 hook 的 outcome 应带 Err")
	}
	if !outcomes[1].Degraded() {
		t.Error("advisory 失败应被标为 Degraded")
	}
	if outcomes[0].Degraded() {
		t.Error("成功的 hook 不应标为 Degraded")
	}
}

type observerFunc func(HookOutcome)

func (f observerFunc) OnHookOutcome(o HookOutcome) { f(o) }

// TestHookChain_DefaultCriticalityKeepsLegacyBehavior 是回归判据:
// 不实现 HookPolicy 的 hook 得到 Critical + 无超时, 与改造前逐字节相同。
func TestHookChain_DefaultCriticalityKeepsLegacyBehavior(t *testing.T) {
	crit, timeout := hookPolicyOf(newFake("legacy", 0, nil).asPlain())
	if crit != Critical {
		t.Fatalf("未声明策略的 hook 应为 Critical, 实际 %v", crit)
	}
	if timeout != 0 {
		t.Fatalf("未声明策略的 hook 应无超时, 实际 %v", timeout)
	}
}

// TestHookChain_AdvisoryReturnTerminalStillTerminates 钉住一个容易漏的边界:
// advisory 只影响**失败**语义。一个 advisory hook 主动返回 ReturnTerminal 时,
// 它的意图仍然必须生效 —— 否则"降级"被错误地扩大成"忽略产出"。
func TestHookChain_AdvisoryReturnTerminalStillTerminates(t *testing.T) {
	c := NewHookChain()
	c.Register(newFake("stopper", 10, func(*HookContext) (*HookResult, error) {
		return &HookResult{ReturnTerminal: &types.Terminal{Reason: "safety_block"}}, nil
	}).asAdvisory(0))

	res, err := c.Execute(PhasePreRequest, &HookContext{Ctx: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.ReturnTerminal == nil || res.ReturnTerminal.Reason != "safety_block" {
		t.Fatalf("advisory hook 显式返回的 terminal 必须生效, 实际 %+v", res)
	}
}
