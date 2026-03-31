package guidance

// Capability describes a guidance subsystem feature.
type Capability struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Version     string `json:"version"`
}

var defaultCapabilities = []Capability{
	{Name: "policy_compile", Description: "Compile markdown constitution and rules into PolicyBundle", Enabled: true, Version: "1.0"},
	{Name: "enforcement_gates", Description: "Evaluate commands, edits, and tool use", Enabled: true, Version: "1.0"},
	{Name: "run_ledger", Description: "Structured run timeline for audits", Enabled: true, Version: "1.0"},
	{Name: "shard_retrieval", Description: "Intent-scoped rule shard retrieval", Enabled: true, Version: "1.0"},
	{Name: "persistence", Description: "Save and load PolicyBundle to disk", Enabled: true, Version: "1.0"},
	{Name: "memory_gate", Description: "Namespace-scoped memory access checks", Enabled: true, Version: "1.0"},
	{Name: "continuation_gate", Description: "Multi-turn session limits", Enabled: true, Version: "1.0"},
	{Name: "analyzer", Description: "Diff and command-history risk analysis", Enabled: true, Version: "1.0"},
	{Name: "trust", Description: "Agent trust scoring", Enabled: true, Version: "1.0"},
	{Name: "manifest_validation", Description: "Validate bundle manifests", Enabled: true, Version: "1.0"},
	{Name: "rule_evolution", Description: "Track rule version changes", Enabled: true, Version: "1.0"},
}

// ListCapabilities returns the static capability catalog.
func ListCapabilities() []Capability {
	out := make([]Capability, len(defaultCapabilities))
	copy(out, defaultCapabilities)
	return out
}

// HasCapability reports whether a named capability exists and is enabled.
func HasCapability(name string) bool {
	for _, c := range defaultCapabilities {
		if c.Name == name {
			return c.Enabled
		}
	}
	return false
}
