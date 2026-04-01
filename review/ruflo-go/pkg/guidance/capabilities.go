package guidance

// 本文件：治理能力静态编目。ListCapabilities/HasCapability 供 CLI 或控制面发现已编译入二进制的子系统开关。

// Capability 描述治理子系统的一项能力（名称、说明、是否启用、版本）。
type Capability struct {
	Name        string `json:"name"`        // 能力标识，如 policy_compile
	Description string `json:"description"` // 人类可读说明
	Enabled     bool   `json:"enabled"`     // 是否启用
	Version     string `json:"version"`     // 能力版本号
}

// defaultCapabilities 包内静态列表，与 guidance 各模块一一对应。
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

// ListCapabilities 返回能力目录的副本（避免调用方修改全局切片）。
func ListCapabilities() []Capability {
	out := make([]Capability, len(defaultCapabilities))
	copy(out, defaultCapabilities)
	return out
}

// HasCapability 判断名称是否存在且 Enabled 为 true。
func HasCapability(name string) bool {
	for _, c := range defaultCapabilities {
		if c.Name == name {
			return c.Enabled
		}
	}
	return false
}
