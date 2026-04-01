package guidance

import (
	"fmt"
	"time"
)

// 本文件：代码变更与命令历史分析器。封装 EnforcementGates，将非 Allow 的 GateResult 转为 Violation；
// RiskScore 将多条违规压缩为 0..1 风险标量。

// GuidanceAnalyzer 基于策略包构造门控并输出 Violation 列表。
type GuidanceAnalyzer struct {
	gates *EnforcementGates // 内部门控实例
}

// NewGuidanceAnalyzer 使用 bundle 创建 EnforcementGates（bundle 可为 nil）。
func NewGuidanceAnalyzer(bundle *PolicyBundle) *GuidanceAnalyzer {
	return &GuidanceAnalyzer{gates: NewEnforcementGates(bundle)}
}

// AnalyzeFileChange 对 path+diff 调用 EvaluateEdit，过滤非 Allow，填充 RuleID/Message/Severity/When。
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

// AnalyzeCommandHistory 对每条命令串运行 EvaluateCommand 并聚合违规。
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

// RiskScore 对各 Severity 赋权求和后除以 len*(0.25)+0.75 并 cap 到 1；无违规返回 0。
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

// min 返回较小浮点数。
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
