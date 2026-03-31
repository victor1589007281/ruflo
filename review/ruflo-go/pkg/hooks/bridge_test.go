package hooks

import (
	"context"
	"testing"
	"time"
)

func TestOfficialHooksBridge_PreEdit(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	var saw string
	_ = reg.Register(HookEventPreEdit, func(_ context.Context, hc HookContext) HookResult {
		saw = hc.File
		return HookResult{Success: true}
	}, 0, "t")
	ex := NewExecutor(reg)
	ex.DefaultTimeout = time.Second
	b := NewOfficialHooksBridge(reg, ex)
	res := b.PreEdit("/tmp/x.go")
	if !res.Success || saw != "/tmp/x.go" {
		t.Fatalf("res=%+v saw=%q", res, saw)
	}
}

func TestOfficialHooksBridge_SessionLifecycle(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	var starts, ends int
	_ = reg.Register(HookEventSessionStart, func(context.Context, HookContext) HookResult {
		starts++
		return HookResult{Success: true}
	}, 0, "s")
	_ = reg.Register(HookEventSessionEnd, func(context.Context, HookContext) HookResult {
		ends++
		return HookResult{Success: true}
	}, 0, "e")
	ex := NewExecutor(reg)
	ex.DefaultTimeout = time.Second
	b := NewOfficialHooksBridge(reg, ex)
	r1, r2 := b.SessionLifecycle(context.Background(), "sid-9")
	if !r1.Success || !r2.Success || starts != 1 || ends != 1 {
		t.Fatalf("r1=%+v r2=%+v starts=%d ends=%d", r1, r2, starts, ends)
	}
}
