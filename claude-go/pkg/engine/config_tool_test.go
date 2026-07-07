package engine

import "testing"

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
