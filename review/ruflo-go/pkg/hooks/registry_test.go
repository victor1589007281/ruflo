package hooks

import (
	"context"
	"testing"
)

func noopHook(_ context.Context, _ HookContext) HookResult {
	return HookResult{Success: true}
}

func TestRegisterAndGet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(HookEventPreTask, noopHook, 10, "h1"); err != nil {
		t.Fatal(err)
	}
	regs := r.GetForEvent(HookEventPreTask)
	if len(regs) != 1 || regs[0].Name != "h1" {
		t.Fatalf("unexpected regs: %#v", regs)
	}
}

func TestPriorityOrdering(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventPreEdit, noopHook, 10, "low")
	_ = r.Register(HookEventPreEdit, noopHook, 90, "high")
	_ = r.Register(HookEventPreEdit, noopHook, 50, "mid")
	regs := r.GetForEvent(HookEventPreEdit)
	if len(regs) != 3 {
		t.Fatalf("want 3 hooks, got %d", len(regs))
	}
	if regs[0].Name != "high" || regs[1].Name != "mid" || regs[2].Name != "low" {
		t.Fatalf("wrong order: %v, %v, %v", regs[0].Name, regs[1].Name, regs[2].Name)
	}
}

func TestUnregister(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventPostTask, noopHook, 1, "x")
	if !r.Unregister("x") {
		t.Fatal("expected unregister true")
	}
	if r.Unregister("x") {
		t.Fatal("second unregister should be false")
	}
	if len(r.GetForEvent(HookEventPostTask)) != 0 {
		t.Fatal("event list should be empty")
	}
}
