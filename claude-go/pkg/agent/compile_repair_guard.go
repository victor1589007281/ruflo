package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// CompileRepairGuard 编译修复循环的混合循环检测 + 置信度早期终止守卫。
// 不依赖外部 embedding 模型，使用自研字符级 n-gram 相似度。
type CompileRepairGuard struct {
	maxRetries          int
	similarityThreshold float64
	history             []repairRound
}

type repairRound struct {
	retryNum        int
	errorSignatures []string
	modifiedFiles   []string
	modifiedLines   int
	agentOutput     string
	confidence      int // LLM 置信度 0-100, -1 表示未解析到
}

// GuardDecision 守卫决策结果。
type GuardDecision struct {
	Action      string // "continue", "abort"
	Reason      string
	ShouldSplit bool // true 时建议触发任务拆分
}

// NewCompileRepairGuard 创建编译修复守卫。
func NewCompileRepairGuard(maxRetries int) *CompileRepairGuard {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	return &CompileRepairGuard{
		maxRetries:          maxRetries,
		similarityThreshold: 0.85,
		history:             make([]repairRound, 0, maxRetries+1),
	}
}

// RecordRound 记录一轮编译修复结果并返回决策。
func (g *CompileRepairGuard) RecordRound(r repairRound) GuardDecision {
	g.history = append(g.history, r)

	// L1: 轮次上限 (>= maxRetries 时终止)
	if len(g.history) >= g.maxRetries {
		return GuardDecision{
			Action:      "abort",
			Reason:      fmt.Sprintf("编译修复达到最大轮次 %d", g.maxRetries),
			ShouldSplit: true,
		}
	}

	// L2: 重复错误检测 (同一签名出现 >=3 次)
	sigCount := make(map[string]int)
	for _, h := range g.history {
		for _, sig := range h.errorSignatures {
			sigCount[sig]++
		}
	}
	for sig, count := range sigCount {
		if count >= 3 {
			return GuardDecision{
				Action:      "abort",
				Reason:      fmt.Sprintf("编译错误签名重复 %d 次: %s", count, sig),
				ShouldSplit: true,
			}
		}
	}

	// L3: 语义相似度检测 (连续两轮 agent 输出相似度 >= threshold)
	if len(g.history) >= 2 {
		prev := g.history[len(g.history)-2]
		curr := g.history[len(g.history)-1]
		sim := SimpleSimilarity(prev.agentOutput, curr.agentOutput)
		if sim >= g.similarityThreshold {
			return GuardDecision{
				Action:      "abort",
				Reason:      fmt.Sprintf("连续两轮 agent 输出语义相似度 %.2f 超过阈值 %.2f", sim, g.similarityThreshold),
				ShouldSplit: true,
			}
		}
	}

	// L4: 置信度检测 (confidence < 40)
	if r.confidence >= 0 && r.confidence < 40 {
		return GuardDecision{
			Action:      "abort",
			Reason:      fmt.Sprintf("LLM 置信度 %d 低于阈值 40", r.confidence),
			ShouldSplit: true,
		}
	}

	return GuardDecision{Action: "continue", Reason: "继续修复"}
}

// ExtractErrorSignatures 从编译错误文本中提取结构化签名。
func ExtractErrorSignatures(buildErrors string) []string {
	lines := strings.Split(buildErrors, "\n")
	var sigs []string
	seen := make(map[string]bool)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sig := extractErrorSignature(line)
		if sig != "" && !seen[sig] {
			seen[sig] = true
			sigs = append(sigs, sig)
		}
	}
	return sigs
}

func extractErrorSignature(line string) string {
	// 提取 "path/to/file.go:错误类型" 作为签名
	if idx := strings.Index(line, ":"); idx > 0 {
		candidate := line[:idx]
		if strings.Contains(candidate, "/") || strings.Contains(candidate, "\\") || strings.HasSuffix(candidate, ".go") {
			rest := line[idx:]
			if errType := extractErrorType(rest); errType != "" {
				return candidate + ":" + errType
			}
		}
	}
	// 回退: 提取已知错误类型关键词
	if errType := extractErrorType(line); errType != "" {
		return "global:" + errType
	}
	return ""
}

func extractErrorType(s string) string {
	lower := strings.ToLower(s)
	knownErrors := []string{
		"undefined", "undeclared", "not declared",
		"mismatched", "mismatch", "cannot use",
		"too many", "too few", "missing",
		"invalid", "expected", "not found",
		"no such file", "import", "package",
		"syntax", "unexpected", "ambiguous",
		"redeclared", "unused", "not used",
		"type mismatch", "incompatible", "conversion",
	}
	for _, e := range knownErrors {
		if strings.Contains(lower, e) {
			return e
		}
	}
	return ""
}

// ParseConfidenceFromOutput 从 LLM 输出末尾解析置信度标记。
// 期望格式: <!--confidence:85--> 或 confidence: 85
func ParseConfidenceFromOutput(output string) int {
	output = strings.TrimSpace(output)
	// 尝试从末尾的 HTML 注释格式解析
	if idx := strings.LastIndex(output, "<!--confidence:"); idx >= 0 {
		suffix := output[idx:]
		if end := strings.Index(suffix, "-->"); end > 0 {
			valStr := strings.TrimSpace(suffix[len("<!--confidence:"):end])
			if v, err := strconv.Atoi(valStr); err == nil && v >= 0 && v <= 100 {
				return v
			}
		}
	}
	// 尝试从末尾行解析
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= max(0, len(lines)-5); i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "confidence:") {
			valStr := strings.TrimSpace(strings.TrimPrefix(line, "confidence:"))
			if v, err := strconv.Atoi(valStr); err == nil && v >= 0 && v <= 100 {
				return v
			}
		}
	}
	return -1 // 未解析到
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ConfidencePromptSuffix 附加到修复 prompt 末尾的置信度要求。
const ConfidencePromptSuffix = `

## ── CONFIDENCE SCORE (MANDATORY) ──
在输出最后，你必须添加一行：<!--confidence:0-100-->
其中数字表示你对本次修复成功通过编译的把握程度。
0 = 完全不确定，100 = 绝对确定。`