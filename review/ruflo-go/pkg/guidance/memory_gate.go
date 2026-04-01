package guidance

import (
	"strings"
	"sync"
)

// 本文件：记忆访问门控。全局拒绝前缀、只读命名空间、按 Agent 的 ACL（未配置 ACL 时默认放行）；
// EvaluateMemoryAccess 与 CheckNamespacePermission 可组合用于 store/search 前校验。

// MemoryGate 对记忆命名空间实施拒绝、只读与按 Agent 白名单策略。
type MemoryGate struct {
	mu         sync.RWMutex                   // 保护三张 map
	agentNS    map[string]map[string]struct{} // agentID -> 允许访问的命名空间集合
	deniedNS   map[string]struct{}            // 被拒绝的命名空间前缀（HasPrefix）
	readOnlyNS map[string]struct{}            // 只读命名空间（写/删/store 拦截）
}

// NewMemoryGate 创建空策略（默认全部允许，直至配置 Deny/ReadOnly/ACL）。
func NewMemoryGate() *MemoryGate {
	return &MemoryGate{
		agentNS:    make(map[string]map[string]struct{}),
		deniedNS:   make(map[string]struct{}),
		readOnlyNS: make(map[string]struct{}),
	}
}

// DenyNamespace 注册全局拒绝前缀（任意命名空间以此前缀开头则 Block）。
func (g *MemoryGate) DenyNamespace(prefix string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deniedNS[strings.TrimSpace(prefix)] = struct{}{}
}

// MarkReadOnly 将某命名空间标为只读（write/delete/store 操作返回 Block）。
func (g *MemoryGate) MarkReadOnly(namespace string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readOnlyNS[normalizeNS(namespace)] = struct{}{}
}

// AllowAgentNamespace 为 Agent 增加可访问命名空间；可与 "*" 通配配合 CheckNamespacePermission。
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

// normalizeNS 去空白，空则使用 "default"。
func normalizeNS(ns string) string {
	s := strings.TrimSpace(ns)
	if s == "" {
		return "default"
	}
	return s
}

// EvaluateMemoryAccess 先匹配 denied 前缀，再检查只读命名空间与 operation（key 当前未使用，预留扩展）。
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

// CheckNamespacePermission：若该 Agent 未配置任何 ACL 则 Allow；否则要求 ns 在白名单或存在 "*"。
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
