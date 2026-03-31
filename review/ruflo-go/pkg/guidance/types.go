package guidance

import (
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// TaskIntent classifies work for shard retrieval.
type TaskIntent string

const (
	IntentBugFix      TaskIntent = "bug-fix"
	IntentFeature     TaskIntent = "feature"
	IntentRefactor    TaskIntent = "refactor"
	IntentSecurity    TaskIntent = "security"
	IntentPerformance TaskIntent = "performance"
	IntentTesting     TaskIntent = "testing"
	IntentDocs        TaskIntent = "docs"
)

// RunEvent is one ledger timeline entry.
type RunEvent struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Constitution captures high-level policy preamble lines.
type Constitution struct {
	Lines  []string `json:"lines"`
	Source string   `json:"source"`
}

// RiskClass is coarse risk labeling for rules and gate results.
type RiskClass string

const (
	RiskCritical RiskClass = "critical"
	RiskHigh     RiskClass = "high"
	RiskMedium   RiskClass = "medium"
	RiskLow      RiskClass = "low"
	RiskInfo     RiskClass = "info"
)

// GuidanceRule extends api.GuidanceRule with parsed markdown facets.
type GuidanceRule struct {
	api.GuidanceRule
	RiskClass RiskClass  `json:"risk_class,omitempty"`
	Domain    string     `json:"domain,omitempty"`
	Tools     []string   `json:"tools,omitempty"`
	Intent    TaskIntent `json:"intent,omitempty"`
	ScopeGlob string     `json:"scope_glob,omitempty"`
}

// RuleShard is a versioned slice of rules with optional intent filters.
type RuleShard struct {
	ShardID string         `json:"shard_id"`
	Version string         `json:"version"`
	Rules   []GuidanceRule `json:"rules"`
	Intents []TaskIntent   `json:"intents,omitempty"`
}

// RuleManifest summarizes a compiled bundle.
type RuleManifest struct {
	BundleID   string         `json:"bundle_id"`
	Version    string         `json:"version"`
	ShardCount int            `json:"shard_count"`
	RuleCount  int            `json:"rule_count"`
	ByRisk     map[string]int `json:"by_risk"`
	ByIntent   map[string]int `json:"by_intent"`
	CreatedAt  time.Time      `json:"created_at"`
}

// PolicyBundle is a compiled guidance artifact.
type PolicyBundle struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	Shards       []RuleShard  `json:"shards"`
	Constitution Constitution `json:"constitution"`
	Manifest     RuleManifest `json:"manifest"`
	CreatedAt    time.Time    `json:"created_at"`
}

// GateDecision is enforcement outcome severity.
type GateDecision int

const (
	GateAllow GateDecision = iota
	GateWarn
	GateRequireConfirmation
	GateBlock
)

// GateResult is one evaluated check.
type GateResult struct {
	RuleID    string       `json:"rule_id,omitempty"`
	Decision  GateDecision `json:"decision"`
	Reason    string       `json:"reason,omitempty"`
	RiskClass RiskClass    `json:"risk_class,omitempty"`
}

// Violation records a policy breach for auditing.
type Violation struct {
	RuleID   string    `json:"rule_id"`
	Message  string    `json:"message"`
	Severity RiskClass `json:"severity"`
	When     time.Time `json:"when"`
}
