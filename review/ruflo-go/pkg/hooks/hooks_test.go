package hooks

import (
	"context"
	"testing"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

func TestHookRegistryGet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventPreTask, noopHook, 5, "named")
	got, ok := r.Get("named")
	if !ok || got == nil || got.Name != "named" {
		t.Fatalf("Get: ok=%v %#v", ok, got)
	}
}

func TestHookRegistryEnableDisable(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventPostEdit, noopHook, 1, "ed")
	if len(r.GetForEvent(HookEventPostEdit)) != 1 {
		t.Fatal("expected hook active")
	}
	if err := r.Disable("ed"); err != nil {
		t.Fatal(err)
	}
	if len(r.GetForEvent(HookEventPostEdit)) != 0 {
		t.Fatal("disabled hook should be skipped")
	}
	if err := r.Enable("ed"); err != nil {
		t.Fatal(err)
	}
	if len(r.GetForEvent(HookEventPostEdit)) != 1 {
		t.Fatal("expected hook re-enabled")
	}
}

func TestHookRegistryHas(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if r.Has("missing") {
		t.Fatal("unexpected Has")
	}
	_ = r.Register(HookEventPreRoute, noopHook, 1, "route-a")
	if !r.Has("route-a") {
		t.Fatal("expected Has")
	}
}

func TestHookRegistrySize(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventPreRead, noopHook, 1, "a")
	_ = r.Register(HookEventPostRead, noopHook, 1, "b")
	if r.Size() != 2 {
		t.Fatalf("Size: %d", r.Size())
	}
}

func TestHookRegistryClear(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventPreTask, noopHook, 1, "t")
	r.Clear()
	if r.Size() != 0 || len(r.GetForEvent(HookEventPreTask)) != 0 {
		t.Fatal("expected empty registry")
	}
}

func TestHookRegistryGetStats(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(HookEventAgentSpawn, noopHook, 1, "s1")
	_ = r.Register(HookEventAgentTerminate, noopHook, 1, "s2")
	_ = r.Disable("s2")
	st := r.GetStats()
	if st.TotalRegistered != 2 || st.EnabledCount != 1 || st.DisabledCount != 1 {
		t.Fatalf("GetStats: %#v", st)
	}
	if st.EventCounts[HookEventAgentSpawn] != 1 {
		t.Fatalf("event counts: %#v", st.EventCounts)
	}
}

func TestExecutorConvenienceMethods(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	type call struct {
		ev  HookEvent
		key string
	}
	var seen []call
	capture := func(ev HookEvent, k string) HookHandler {
		return func(_ context.Context, hc HookContext) HookResult {
			seen = append(seen, call{ev: ev, key: k})
			if hc.Args == nil {
				hc.Args = map[string]any{}
			}
			return HookResult{Success: true, Data: map[string]any{"k": k}}
		}
	}
	_ = reg.Register(HookEventPreToolUse, capture(HookEventPreToolUse, "ptu"), 1, "ptu")
	_ = reg.Register(HookEventPostToolUse, capture(HookEventPostToolUse, "potu"), 1, "potu")
	_ = reg.Register(HookEventPreEdit, capture(HookEventPreEdit, "pe"), 1, "pe")
	_ = reg.Register(HookEventPostEdit, capture(HookEventPostEdit, "poe"), 1, "poe")
	_ = reg.Register(HookEventPreCommand, capture(HookEventPreCommand, "pc"), 1, "pc")
	_ = reg.Register(HookEventPostCommand, capture(HookEventPostCommand, "poc"), 1, "poc")
	_ = reg.Register(HookEventSessionStart, capture(HookEventSessionStart, "ss"), 1, "ss")
	_ = reg.Register(HookEventSessionEnd, capture(HookEventSessionEnd, "se"), 1, "se")
	_ = reg.Register(HookEventAgentSpawn, capture(HookEventAgentSpawn, "as"), 1, "as")
	_ = reg.Register(HookEventAgentTerminate, capture(HookEventAgentTerminate, "at"), 1, "at")

	ex := NewExecutor(reg)
	ex.DefaultTimeout = time.Second

	check := func(label string, res HookResult) {
		t.Helper()
		if !res.Success {
			t.Fatalf("%s: %#v", label, res)
		}
	}

	check("PreToolUse", ex.PreToolUse("Read", "{}"))
	check("PostToolUse", ex.PostToolUse("Read", "ok"))
	check("PreEdit", ex.PreEdit("/tmp/a.go"))
	check("PostEdit", ex.PostEdit("/tmp/a.go", "diff"))
	check("PreCommand", ex.PreCommand("go test ./..."))
	check("PostCommand", ex.PostCommand("go test ./...", 0))
	check("SessionStart", ex.SessionStart("sess-1"))
	check("SessionEnd", ex.SessionEnd("sess-1"))
	check("AgentSpawn", ex.AgentSpawn("ag-1", string(api.AgentTypeCoder)))
	check("AgentTerminate", ex.AgentTerminate("ag-1"))

	if len(seen) != 10 {
		t.Fatalf("expected 10 hook invocations, got %d", len(seen))
	}
}
