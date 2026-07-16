package engine

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestConfig_toolExposed_Whitelist(t *testing.T) {
	c := &Config{AllowedTools: map[string]bool{"sql_query": true}}
	if !c.toolExposed("sql_query") {
		t.Fatal("whitelisted tool must be exposed")
	}
	if c.toolExposed("Bash") {
		t.Fatal("non-whitelisted tool must be hidden when whitelist is set")
	}
}

func TestConfig_toolExposed_Blacklist(t *testing.T) {
	c := &Config{DisabledTools: map[string]bool{"team": true}}
	if c.toolExposed("team") {
		t.Fatal("blacklisted tool must be hidden")
	}
	if !c.toolExposed("Read") {
		t.Fatal("non-blacklisted tool must be exposed")
	}
}

func TestConfig_toolExposed_BlacklistWinsOverWhitelist(t *testing.T) {
	c := &Config{
		AllowedTools:  map[string]bool{"sql_query": true},
		DisabledTools: map[string]bool{"sql_query": true},
	}
	if c.toolExposed("sql_query") {
		t.Fatal("a tool on both lists must stay denied (blacklist wins)")
	}
}

func TestConfig_toolExposed_NoLists(t *testing.T) {
	c := &Config{}
	if !c.toolExposed("anything") {
		t.Fatal("with no lists every tool is exposed")
	}
}

// A set-but-empty whitelist is deny-all (fail-closed), NOT "no restriction":
// a caller whose template rendered blank must never silently expose every tool.
func TestConfig_toolExposed_EmptyWhitelistDeniesAll(t *testing.T) {
	c := &Config{AllowedTools: map[string]bool{}}
	for _, name := range []string{"Bash", "Write", "sql_query"} {
		if c.toolExposed(name) {
			t.Fatalf("empty (non-nil) whitelist must deny %s", name)
		}
	}
}

// gateToolUses is RunIsolated's ToolGateHook equivalent (the isolated runner
// bypasses the HookChain): denied tool_use blocks — including XML-fallback
// fabrications — must become error tool_results, never executions.
func TestGateToolUses_WhitelistEnforcedInIsolatedPath(t *testing.T) {
	cfg := &Config{AllowedTools: map[string]bool{"mcp_gw_sql_query": true}}
	blocks := []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "1", Name: "mcp_gw_sql_query"},
		{Type: types.ContentBlockToolUse, ID: "2", Name: "Bash"},
		{Type: types.ContentBlockToolUse, ID: "3", Name: "Write"},
	}
	allowed, denied := gateToolUses(cfg, blocks)
	if len(allowed) != 1 || allowed[0].Name != "mcp_gw_sql_query" {
		t.Fatalf("expected only the whitelisted tool to survive, got %+v", allowed)
	}
	if len(denied) != 2 {
		t.Fatalf("expected 2 denial results, got %d", len(denied))
	}
	for _, m := range denied {
		if len(m.Content) != 1 || !m.Content[0].IsError || m.Content[0].Type != types.ContentBlockToolResult {
			t.Fatalf("denial must be an error tool_result, got %+v", m)
		}
	}
}
