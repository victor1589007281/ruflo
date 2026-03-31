package guidance

import (
	"strings"
	"testing"
)

func TestCompileMarkdown(t *testing.T) {
	t.Parallel()
	md := `
## Security baseline

R001: Validate all external input
This rule must block unsafe payloads.

## Ops

R002: Log all commands with warn severity
`
	c := NewGuidanceCompiler()
	bundle := c.Compile(md, "")
	if len(bundle.Shards) != 1 {
		t.Fatalf("shards: %d", len(bundle.Shards))
	}
	rules := bundle.Shards[0].Rules
	if len(rules) < 2 {
		t.Fatalf("expected at least 2 rules, got %d", len(rules))
	}
	byID := map[string]GuidanceRule{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	r1, ok := byID["R001"]
	if !ok || !strings.Contains(r1.Description, "Validate") {
		t.Fatalf("R001: %#v", r1)
	}
	if len(bundle.Constitution.Lines) == 0 {
		t.Fatal("expected constitution lines from preamble")
	}
}

func TestParseRuleID(t *testing.T) {
	t.Parallel()
	md := "intro\n\n## Section\n\nR001: first rule body\n"
	c := NewGuidanceCompiler()
	bundle := c.Compile(md, "")
	var found bool
	for _, r := range bundle.Shards[0].Rules {
		if r.ID == "R001" {
			found = true
			if r.Name != "Section" {
				t.Fatalf("name: %q", r.Name)
			}
			break
		}
	}
	if !found {
		t.Fatal("R001 not parsed")
	}
}

func TestRiskClassification(t *testing.T) {
	t.Parallel()
	md := `
## Hardening

R010: Shell access
risk: high
Use @security tooling and [scan] regularly.
#security review required
`
	c := NewGuidanceCompiler()
	bundle := c.Compile(md, "")
	var rule *GuidanceRule
	for i := range bundle.Shards[0].Rules {
		if bundle.Shards[0].Rules[i].ID == "R010" {
			rule = &bundle.Shards[0].Rules[i]
			break
		}
	}
	if rule == nil {
		t.Fatal("R010 missing")
	}
	if rule.RiskClass != RiskHigh {
		t.Fatalf("RiskClass=%v want high", rule.RiskClass)
	}
	if rule.Domain != "security" {
		t.Fatalf("Domain=%q want security", rule.Domain)
	}
	if !containsStr(rule.Tools, "scan") {
		t.Fatalf("Tools=%v", rule.Tools)
	}
	if rule.Intent != IntentSecurity {
		t.Fatalf("Intent=%v", rule.Intent)
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
