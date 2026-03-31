package guidance

import (
	"fmt"
	"time"
)

// GuidanceAnalyzer surfaces violations using enforcement gates and heuristics.
type GuidanceAnalyzer struct {
	gates *EnforcementGates
}

// NewGuidanceAnalyzer builds an analyzer; bundle may be nil.
func NewGuidanceAnalyzer(bundle *PolicyBundle) *GuidanceAnalyzer {
	return &GuidanceAnalyzer{gates: NewEnforcementGates(bundle)}
}

// AnalyzeFileChange converts gate results on a diff into violations.
func (a *GuidanceAnalyzer) AnalyzeFileChange(path, diff string) []Violation {
	if a == nil {
		return nil
	}
	now := time.Now().UTC()
	var out []Violation
	for _, g := range a.gates.EvaluateEdit(path, diff) {
		if g.Decision == GateAllow {
			continue
		}
		sev := g.RiskClass
		if sev == "" {
			sev = RiskMedium
		}
		msg := g.Reason
		if msg == "" {
			msg = fmt.Sprint(g.Decision)
		}
		out = append(out, Violation{
			RuleID:   g.RuleID,
			Message:  msg,
			Severity: sev,
			When:     now,
		})
	}
	return out
}

// AnalyzeCommandHistory runs each command through command gates.
func (a *GuidanceAnalyzer) AnalyzeCommandHistory(commands []string) []Violation {
	if a == nil {
		return nil
	}
	now := time.Now().UTC()
	var out []Violation
	for _, cmd := range commands {
		for _, g := range a.gates.EvaluateCommand(cmd) {
			if g.Decision == GateAllow {
				continue
			}
			sev := g.RiskClass
			if sev == "" {
				sev = RiskMedium
			}
			msg := g.Reason
			if msg == "" {
				msg = cmd
			}
			out = append(out, Violation{
				RuleID:   g.RuleID,
				Message:  msg,
				Severity: sev,
				When:     now,
			})
		}
	}
	return out
}

// RiskScore aggregates violation severity into 0..1 (higher is riskier).
func RiskScore(violations []Violation) float64 {
	if len(violations) == 0 {
		return 0
	}
	var sum float64
	for _, v := range violations {
		switch v.Severity {
		case RiskCritical:
			sum += 1.0
		case RiskHigh:
			sum += 0.75
		case RiskMedium:
			sum += 0.5
		case RiskLow:
			sum += 0.25
		case RiskInfo:
			sum += 0.1
		default:
			sum += 0.3
		}
	}
	n := float64(len(violations))
	return min(1.0, sum/(n*0.25+0.75))
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
