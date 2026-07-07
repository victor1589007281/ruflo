package internal_hook

import (
	"context"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func toolUse(id, name string) types.ContentBlock {
	return types.ContentBlock{Type: types.ContentBlockToolUse, ID: id, Name: name}
}

// A whitelist must hard-deny any tool call outside the set, even if the model
// emits it — this is the second line of defense behind tool-table filtering.
func TestToolGateHook_WhitelistDeniesUnlisted(t *testing.T) {
	h := NewToolGateHook(nil, map[string]bool{"sql_query": true, "submit_finding": true})
	if h == nil {
		t.Fatal("expected a hook for a non-empty whitelist")
	}
	ctx := &HookContext{
		Ctx: context.Background(),
		ToolUseBlocks: []types.ContentBlock{
			toolUse("1", "sql_query"),
			toolUse("2", "Bash"),  // not whitelisted
			toolUse("3", "Write"), // not whitelisted
		},
	}
	res, err := h.Execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res == nil {
		t.Fatal("expected interception result")
	}
	if len(res.ToolUseBlocks) != 1 || res.ToolUseBlocks[0].Name != "sql_query" {
		t.Fatalf("expected only sql_query to survive, got %+v", res.ToolUseBlocks)
	}
	// Two denied tools -> two error tool_results injected.
	if len(res.AppendMsgs) != 2 {
		t.Fatalf("expected 2 error results, got %d", len(res.AppendMsgs))
	}
}

func TestToolGateHook_AllWhitelistedPassThrough(t *testing.T) {
	h := NewToolGateHook(nil, map[string]bool{"sql_query": true})
	ctx := &HookContext{Ctx: context.Background(), ToolUseBlocks: []types.ContentBlock{toolUse("1", "sql_query")}}
	res, err := h.Execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil (nothing intercepted), got %+v", res)
	}
}

func TestToolGateHook_BlacklistStillWorks(t *testing.T) {
	h := NewToolGateHook(map[string]bool{"team": true}, nil)
	ctx := &HookContext{Ctx: context.Background(), ToolUseBlocks: []types.ContentBlock{toolUse("1", "team"), toolUse("2", "Read")}}
	res, err := h.Execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res == nil || len(res.ToolUseBlocks) != 1 || res.ToolUseBlocks[0].Name != "Read" {
		t.Fatalf("expected team blocked, Read allowed, got %+v", res)
	}
}

func TestToolGateHook_NilWhenEmpty(t *testing.T) {
	if NewToolGateHook(nil, nil) != nil {
		t.Fatal("expected nil hook when neither list is set")
	}
}

// When every emitted tool is denied, the model must be told to continue.
func TestToolGateHook_AllDeniedInjectsContinue(t *testing.T) {
	h := NewToolGateHook(nil, map[string]bool{"sql_query": true})
	ctx := &HookContext{Ctx: context.Background(), ToolUseBlocks: []types.ContentBlock{toolUse("1", "Bash")}}
	res, err := h.Execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res == nil || !res.InjectContinue {
		t.Fatalf("expected InjectContinue when all tools denied, got %+v", res)
	}
}
