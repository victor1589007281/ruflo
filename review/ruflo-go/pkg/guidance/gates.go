package guidance

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// EnforcementGates evaluates commands, edits, and tool use against heuristics and optional bundles.
type EnforcementGates struct {
	mu sync.RWMutex

	bundle      *PolicyBundle
	activeRules []GuidanceRule
}

// NewEnforcementGates builds gates; bundle may be nil for defaults-only checks.
func NewEnforcementGates(bundle *PolicyBundle) *EnforcementGates {
	return &EnforcementGates{bundle: bundle}
}

var (
	reSecret = regexp.MustCompile(`(?i)(api[_-]?key|secret|password|bearer\s+[a-z0-9._-]{20,}|sk-[a-z0-9]{20,})`)
	reDanger = regexp.MustCompile(`(?i)(rm\s+-rf\s+/|mkfs\.|dd\s+if=\S+\s+of=/dev/\S+|curl\s+[^\n]+\|\s*sh)`)
)

func rank(d GateDecision) int {
	return int(d)
}

func mergeDecision(a, b GateDecision) GateDecision {
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// SetActiveRules applies rules consulted during command evaluation (context-aware gates).
func (g *EnforcementGates) SetActiveRules(rules []GuidanceRule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.activeRules = append([]GuidanceRule(nil), rules...)
}

func (g *EnforcementGates) activeRulesCopy() []GuidanceRule {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]GuidanceRule(nil), g.activeRules...)
}

// EvaluateDestructiveOps returns gate results for destructive shell patterns only.
func (g *EnforcementGates) EvaluateDestructiveOps(cmd string) []GateResult {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return []GateResult{{Decision: GateAllow, Reason: "empty command"}}
	}
	var out []GateResult
	if reDanger.MatchString(c) {
		out = append(out, GateResult{RuleID: "G-CMD-DANGER", Decision: GateBlock, Reason: "dangerous shell pattern", RiskClass: RiskCritical})
	}
	if strings.Contains(strings.ToLower(c), "curl") && strings.Contains(c, "|") {
		out = append(out, GateResult{RuleID: "G-CMD-PIPE", Decision: GateRequireConfirmation, Reason: "piped curl", RiskClass: RiskHigh})
	}
	if len(out) == 0 {
		out = append(out, GateResult{Decision: GateAllow})
	}
	return normalizeSeverityOrder(out)
}

// EvaluateSecrets returns gate results for possible secret material in free text.
func (g *EnforcementGates) EvaluateSecrets(content string) []GateResult {
	c := strings.TrimSpace(content)
	if c == "" {
		return []GateResult{{Decision: GateAllow, Reason: "empty content"}}
	}
	var out []GateResult
	if reSecret.MatchString(c) {
		out = append(out, GateResult{RuleID: "G-SECRET", Decision: GateBlock, Reason: "possible secret in content", RiskClass: RiskCritical})
	}
	if len(out) == 0 {
		out = append(out, GateResult{Decision: GateAllow})
	}
	return normalizeSeverityOrder(out)
}

// EvaluateCommand returns gate results for shell-like commands.
func (g *EnforcementGates) EvaluateCommand(cmd string) []GateResult {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return []GateResult{{Decision: GateAllow, Reason: "empty command"}}
	}
	out := append(append([]GateResult{}, g.EvaluateDestructiveOps(c)...), g.EvaluateSecrets(c)...)
	for _, rule := range g.activeRulesCopy() {
		if rule.Description != "" && len(rule.Description) >= 3 && strings.Contains(strings.ToLower(c), strings.ToLower(rule.Description)) {
			dec := GateWarn
			switch strings.ToLower(strings.TrimSpace(rule.Severity)) {
			case "block":
				dec = GateBlock
			case "warn":
				dec = GateWarn
			case "require_confirmation", "confirm":
				dec = GateRequireConfirmation
			}
			rid := rule.ID
			if rid == "" {
				rid = "G-ACTIVE-RULE"
			}
			out = append(out, GateResult{RuleID: rid, Decision: dec, Reason: "active guidance rule match", RiskClass: rule.RiskClass})
		}
	}
	if len(out) == 0 {
		out = append(out, GateResult{Decision: GateAllow})
	}
	return normalizeSeverityOrder(out)
}

// EvaluateEdit checks diff size and secret patterns in unified diff text.
func (g *EnforcementGates) EvaluateEdit(file, diff string) []GateResult {
	var out []GateResult
	if len(diff) > 200_000 {
		out = append(out, GateResult{RuleID: "G-EDIT-SIZE", Decision: GateRequireConfirmation, Reason: "very large diff", RiskClass: RiskMedium})
	}
	if reSecret.MatchString(diff) {
		out = append(out, GateResult{RuleID: "G-EDIT-SECRET", Decision: GateBlock, Reason: "possible secret in diff", RiskClass: RiskCritical})
	}
	if g.bundle != nil && file != "" {
		for _, sh := range g.bundle.Shards {
			for _, r := range sh.Rules {
				if r.ScopeGlob != "" {
					ok, _ := filepath.Match(r.ScopeGlob, file)
					if !ok {
						continue
					}
					if strings.EqualFold(r.Severity, "block") {
						out = append(out, GateResult{RuleID: r.ID, Decision: GateBlock, Reason: r.Description, RiskClass: r.RiskClass})
					}
				}
			}
		}
	}
	if len(out) == 0 {
		out = append(out, GateResult{Decision: GateAllow})
	}
	return normalizeSeverityOrder(out)
}

// EvaluateToolUse checks tool name against a minimal allowlist heuristic.
func (g *EnforcementGates) EvaluateToolUse(tool string, args map[string]any) []GateResult {
	var out []GateResult
	t := strings.ToLower(strings.TrimSpace(tool))
	if t == "" {
		return []GateResult{{Decision: GateAllow}}
	}
	allowed := map[string]struct{}{
		"read": {}, "grep": {}, "glob": {}, "edit": {}, "bash": {}, "task": {},
		"memory_store": {}, "memory_retrieve": {}, "memory_search": {},
	}
	if _, ok := allowed[t]; !ok && strings.Contains(t, "exec") {
		out = append(out, GateResult{RuleID: "G-TOOL-UNKNOWN", Decision: GateWarn, Reason: "unusual tool", RiskClass: RiskLow})
	}
	if t == "bash" && args != nil {
		if c, _ := args["command"].(string); c != "" {
			out = append(out, g.EvaluateCommand(c)...)
		}
	}
	if len(out) == 0 {
		out = append(out, GateResult{Decision: GateAllow})
	}
	return normalizeSeverityOrder(out)
}

func normalizeSeverityOrder(in []GateResult) []GateResult {
	best := GateAllow
	for _, g := range in {
		best = mergeDecision(best, g.Decision)
	}
	// If any block, surface block first
	var ordered []GateResult
	for _, g := range in {
		if g.Decision == GateBlock {
			ordered = append(ordered, g)
		}
	}
	for _, g := range in {
		if g.Decision == GateRequireConfirmation {
			ordered = append(ordered, g)
		}
	}
	for _, g := range in {
		if g.Decision == GateWarn {
			ordered = append(ordered, g)
		}
	}
	for _, g := range in {
		if g.Decision == GateAllow {
			ordered = append(ordered, g)
		}
	}
	if len(ordered) == 0 {
		return []GateResult{{Decision: best}}
	}
	return ordered
}
