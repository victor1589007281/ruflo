package guidance

import (
	"fmt"
	"strings"
	"sync"
)

// OptimizerLoop ranks violations, proposes rule tweaks, scores rules, and promotes locals to root.
type OptimizerLoop struct {
	mu               sync.Mutex
	promoteThreshold float64
	winsNeeded       int
	ruleWins         map[string]int
	ruleScores       map[string]float64
}

// ViolationPrioritySort applies the same frequency×severity ordering as the package-level RankViolations function.
func (*OptimizerLoop) ViolationPrioritySort(violations []Violation) []Violation {
	return RankViolations(violations)
}

// NewOptimizerLoop returns a loop with default promotion gates (score > 0.85 after 5 wins).
func NewOptimizerLoop() *OptimizerLoop {
	return &OptimizerLoop{
		promoteThreshold: 0.85,
		winsNeeded:       5,
		ruleWins:         make(map[string]int),
		ruleScores:       make(map[string]float64),
	}
}

// ProposeRuleChange suggests a markdown-style rule adjustment for a recurring violation.
func (o *OptimizerLoop) ProposeRuleChange(v Violation) string {
	sev := string(v.Severity)
	if sev == "" {
		sev = "warn"
	}
	return fmt.Sprintf("- **id**: derived-%s\n- **severity**: %s\n- **description**: Reinforce: %s (triggered for rule %q)\n- **expression**: block_when:message_contains:%q",
		sanitizeID(v.RuleID), sev, shorten(v.Message, 80), v.RuleID, shorten(v.Message, 40))
}

func sanitizeID(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
	s = strings.Trim(s, "-")
	if s == "" {
		return "unknown"
	}
	return s
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// EvaluateRule scores how well a rule's text covers the given test case strings (0–1).
func (*OptimizerLoop) EvaluateRule(rule GuidanceRule, testCases []string) float64 {
	if len(testCases) == 0 {
		return 0.5
	}
	text := strings.ToLower(rule.Description + " " + rule.Expression + " " + rule.Name)
	var hits int
	for _, tc := range testCases {
		tc = strings.ToLower(strings.TrimSpace(tc))
		if tc == "" {
			continue
		}
		if strings.Contains(text, tc) {
			hits++
		}
	}
	return float64(hits) / float64(len(testCases))
}

// PromoteRule records a win for a local rule id; returns true if it should be promoted to root.
func (o *OptimizerLoop) PromoteRule(localRuleID string, score float64) bool {
	if localRuleID == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if score >= o.promoteThreshold {
		o.ruleWins[localRuleID]++
		o.ruleScores[localRuleID] = score
	}
	return o.ruleWins[localRuleID] >= o.winsNeeded && o.ruleScores[localRuleID] > o.promoteThreshold
}

// SetPromotionPolicy updates thresholds used by PromoteRule.
func (o *OptimizerLoop) SetPromotionPolicy(minScore float64, wins int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if minScore > 0 {
		o.promoteThreshold = minScore
	}
	if wins > 0 {
		o.winsNeeded = wins
	}
}
