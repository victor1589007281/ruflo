package guidance

import (
	"fmt"
	"strings"
	"sync"
)

// 本文件：规则优化器。基于违规反馈生成 Markdown 风格规则补丁建议；EvaluateRule 用子串覆盖度打分；
// PromoteRule 累计本地规则“获胜”次数与分数，满足阈值后建议提升到根策略（由上层持久化）。

// OptimizerLoop 维护规则得分与晋升计数，与 RankViolations 排序一致。
type OptimizerLoop struct {
	mu               sync.Mutex         // 保护阈值与 map
	promoteThreshold float64            // PromoteRule 单次得分达标阈值
	winsNeeded       int                // 达标次数达到此值且最新分仍 > 阈值则建议晋升
	ruleWins         map[string]int     // 规则 ID -> 达标触发次数
	ruleScores       map[string]float64 // 规则 ID -> 最近得分
}

// ViolationPrioritySort 委托包级 RankViolations，保持与账本相同的违规排序。
func (*OptimizerLoop) ViolationPrioritySort(violations []Violation) []Violation {
	return RankViolations(violations)
}

// NewOptimizerLoop 默认 promoteThreshold=0.85、winsNeeded=5。
func NewOptimizerLoop() *OptimizerLoop {
	return &OptimizerLoop{
		promoteThreshold: 0.85,
		winsNeeded:       5,
		ruleWins:         make(map[string]int),
		ruleScores:       make(map[string]float64),
	}
}

// ProposeRuleChange 根据违规生成可粘贴的 Markdown 规则草案（含 severity、description、expression 占位）。
func (o *OptimizerLoop) ProposeRuleChange(v Violation) string {
	sev := string(v.Severity)
	if sev == "" {
		sev = "warn"
	}
	return fmt.Sprintf("- **id**: derived-%s\n- **severity**: %s\n- **description**: Reinforce: %s (triggered for rule %q)\n- **expression**: block_when:message_contains:%q",
		sanitizeID(v.RuleID), sev, shorten(v.Message, 80), v.RuleID, shorten(v.Message, 40))
}

// sanitizeID 将 ID 规范为字母数字与连字符。
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

// shorten 截断并加省略号。
func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// EvaluateRule 将规则 Name/Description/Expression 拼接为小写文本，统计 testCases 中有多少子串被包含；无用例时返回 0.5。
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

// PromoteRule 当 score≥promoteThreshold 时递增 ruleWins 并记录 ruleScores；若 wins≥winsNeeded 且分仍>阈值则返回 true 表示建议晋升。
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

// SetPromotionPolicy 更新晋升策略（仅接受正数参数）。
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
