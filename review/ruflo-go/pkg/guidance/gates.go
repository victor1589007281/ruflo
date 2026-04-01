package guidance

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// 本文件：执行门控（安全门控模式）。在命令、编辑、工具调用前输出 GateResult 列表；
// 内置破坏性 shell、密钥形态、超大 diff、工具名启发式；可选 PolicyBundle 的 ScopeGlob 阻断；
// SetActiveRules 支持按描述子串匹配的上下文规则。normalizeSeverityOrder 将结果按 Block→Confirm→Warn→Allow 重排。

// EnforcementGates 评估命令/编辑/工具使用：组合静态启发式、可选策略包与活动规则列表。
type EnforcementGates struct {
	mu sync.RWMutex // 保护 bundle 指针替换与 activeRules

	bundle      *PolicyBundle  // 可选：分片中带 ScopeGlob+block 的规则参与编辑门控
	activeRules []GuidanceRule // 动态活动规则（命令行包含 Description 子串时触发）
}

// NewEnforcementGates 创建门控；bundle 可为 nil，此时仅启发式与 activeRules 生效。
func NewEnforcementGates(bundle *PolicyBundle) *EnforcementGates {
	return &EnforcementGates{bundle: bundle}
}

var (
	reSecret = regexp.MustCompile(`(?i)(api[_-]?key|secret|password|bearer\s+[a-z0-9._-]{20,}|sk-[a-z0-9]{20,})`) // 疑似密钥
	reDanger = regexp.MustCompile(`(?i)(rm\s+-rf\s+/|mkfs\.|dd\s+if=\S+\s+of=/dev/\S+|curl\s+[^\n]+\|\s*sh)`)     // 高危命令模式
)

// rank 将 GateDecision 转为整数序用于 merge（数值大表示更严格）。
func rank(d GateDecision) int {
	return int(d)
}

// mergeDecision 取 a、b 中更严格（rank 更大）的决策。
func mergeDecision(a, b GateDecision) GateDecision {
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// SetActiveRules 设置活动规则切片副本（EvaluateCommand 时子串匹配 Description）。
func (g *EnforcementGates) SetActiveRules(rules []GuidanceRule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.activeRules = append([]GuidanceRule(nil), rules...)
}

// activeRulesCopy 在 RLock 下拷贝活动规则，避免长时间持锁执行评估。
func (g *EnforcementGates) activeRulesCopy() []GuidanceRule {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]GuidanceRule(nil), g.activeRules...)
}

// EvaluateDestructiveOps 仅检测破坏性/管道执行类 shell 模式；无命中则返回单条 Allow。
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

// EvaluateSecrets 检测自由文本中疑似 API Key/密码/Bearer/sk- 等模式。
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

// EvaluateCommand 合并破坏性检测、密钥检测与活动规则子串匹配（Severity 映射到 GateDecision）。
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

// EvaluateEdit 检查 unified diff 体积、密钥正则，以及 bundle 中与 filepath.Match(ScopeGlob,file) 匹配的阻断级规则。
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

// EvaluateToolUse 对工具名做最小白名单启发：含 exec 且非已知工具则 Warn；bash 时递归 EvaluateCommand(command)。
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

// normalizeSeverityOrder 先计算整体最严决策 best，再将 in 按 Block、RequireConfirmation、Warn、Allow 分段重排，便于 UI 优先展示阻断项。
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
