package guidance

import (
	"strings"
	"sync"
)

// MemoryGate enforces namespace policies for memory operations.
type MemoryGate struct {
	mu          sync.RWMutex
	agentNS     map[string]map[string]struct{} // agentID -> allowed namespaces
	deniedNS    map[string]struct{}            // globally denied prefixes
	readOnlyNS  map[string]struct{}
}

// NewMemoryGate builds a gate with empty policies (allow by default).
func NewMemoryGate() *MemoryGate {
	return &MemoryGate{
		agentNS:  make(map[string]map[string]struct{}),
		deniedNS: make(map[string]struct{}),
		readOnlyNS: make(map[string]struct{}),
	}
}

// DenyNamespace blocks all access to a namespace prefix.
func (g *MemoryGate) DenyNamespace(prefix string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deniedNS[strings.TrimSpace(prefix)] = struct{}{}
}

// MarkReadOnly marks a namespace as read-only (write/delete blocked).
func (g *MemoryGate) MarkReadOnly(namespace string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readOnlyNS[normalizeNS(namespace)] = struct{}{}
}

// AllowAgentNamespace grants an agent access to a namespace.
func (g *MemoryGate) AllowAgentNamespace(agentID, namespace string) {
	if g == nil || agentID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ns := normalizeNS(namespace)
	if g.agentNS[agentID] == nil {
		g.agentNS[agentID] = make(map[string]struct{})
	}
	g.agentNS[agentID][ns] = struct{}{}
}

func normalizeNS(ns string) string {
	s := strings.TrimSpace(ns)
	if s == "" {
		return "default"
	}
	return s
}

// EvaluateMemoryAccess checks an operation against global and read-only rules.
func (g *MemoryGate) EvaluateMemoryAccess(namespace, key, operation string) GateResult {
	_ = key
	ns := normalizeNS(namespace)
	op := strings.ToLower(strings.TrimSpace(operation))
	if g == nil {
		return GateResult{Decision: GateAllow}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	for prefix := range g.deniedNS {
		if prefix != "" && strings.HasPrefix(ns, prefix) {
			return GateResult{
				RuleID:    "G-MEM-NS-DENY",
				Decision:  GateBlock,
				Reason:    "namespace denied",
				RiskClass: RiskHigh,
			}
		}
	}
	if _, ro := g.readOnlyNS[ns]; ro {
		if op == "write" || op == "delete" || op == "store" {
			return GateResult{
				RuleID:    "G-MEM-RO",
				Decision:  GateBlock,
				Reason:    "namespace is read-only",
				RiskClass: RiskMedium,
			}
		}
	}
	return GateResult{Decision: GateAllow}
}

// CheckNamespacePermission verifies an agent is allowed to touch a namespace when ACLs are set.
func (g *MemoryGate) CheckNamespacePermission(agentID, namespace string) GateResult {
	if g == nil || agentID == "" {
		return GateResult{Decision: GateAllow}
	}
	ns := normalizeNS(namespace)
	g.mu.RLock()
	defer g.mu.RUnlock()
	allowed, ok := g.agentNS[agentID]
	if !ok || len(allowed) == 0 {
		return GateResult{Decision: GateAllow}
	}
	if _, ok := allowed[ns]; ok {
		return GateResult{Decision: GateAllow}
	}
	if _, ok := allowed["*"]; ok {
		return GateResult{Decision: GateAllow}
	}
	return GateResult{
		RuleID:    "G-MEM-ACL",
		Decision:  GateBlock,
		Reason:    "agent not permitted for namespace",
		RiskClass: RiskHigh,
	}
}
