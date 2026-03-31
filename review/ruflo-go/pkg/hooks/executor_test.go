package hooks

import (
	"context"
	"testing"
	"time"
)

func TestExecuteHooks(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	var n int
	h := func(_ context.Context, _ HookContext) HookResult {
		n++
		return HookResult{Success: true}
	}
	_ = reg.Register(HookEventPreCommand, h, 1, "a")
	_ = reg.Register(HookEventPreCommand, h, 2, "b")
	ex := NewExecutor(reg)
	ex.DefaultTimeout = time.Second
	res := ex.Execute(HookEventPreCommand, HookContext{})
	if !res.Success || n != 2 {
		t.Fatalf("Success=%v n=%d", res.Success, n)
	}
}

func TestAbortOnFailure(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	var calls int
	_ = reg.Register(HookEventPostCommand, func(_ context.Context, _ HookContext) HookResult {
		calls++
		return HookResult{Success: true, Abort: true, Message: "stop"}
	}, 10, "first")
	_ = reg.Register(HookEventPostCommand, func(_ context.Context, _ HookContext) HookResult {
		calls++
		return HookResult{Success: true}
	}, 5, "second")
	ex := NewExecutor(reg)
	ex.DefaultTimeout = time.Second
	res := ex.Execute(HookEventPostCommand, HookContext{})
	if !res.Abort || calls != 1 {
		t.Fatalf("Abort=%v calls=%d", res.Abort, calls)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	_ = reg.Register(HookEventSessionStart, func(_ context.Context, _ HookContext) HookResult {
		time.Sleep(2 * time.Second)
		return HookResult{Success: true}
	}, 1, "slow")
	ex := NewExecutor(reg)
	res := ex.ExecuteWithTimeout(context.Background(), HookEventSessionStart, HookContext{}, 50*time.Millisecond)
	if res.Success || !res.Abort {
		t.Fatalf("expected timeout abort, got Success=%v Abort=%v Error=%q", res.Success, res.Abort, res.Error)
	}
	if res.Error == "" {
		t.Fatal("expected error message")
	}
}
