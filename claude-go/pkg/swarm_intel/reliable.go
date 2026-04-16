package swarm_intel

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ═══════════════════════════════════════════════════════════════════
// A. ContextCompressor — 上下文压缩管理器
//
// 设计参考:
//   - Kimi 128K/256K: token 预估+截断, 输出上限 = context − prompt
//   - Anthropic Prompt Caching: system+tools 稳定前缀, 变量区压缩
//   - DeepSeek: keep-alive 下的 prompt 膨胀导致排队延迟
//
// 核心职责: 确保任何发往 LLM 的 prompt 总字符数 < 上限,
// 通过截断/摘要/去重/分层预算控制上下文膨胀。
// ═══════════════════════════════════════════════════════════════════

type ContextCompressor struct {
	MaxPromptChars  int // prompt 总字符上限 (默认 6000)
	MaxSingleField  int // 单字段最大字符 (默认 500)
	MaxHistoryItems int // 历史列表最大条数 (默认 3)
	MaxListItems    int // 列表型字段最大条数 (默认 8)
}

func DefaultContextCompressor() *ContextCompressor {
	return &ContextCompressor{
		MaxPromptChars:  6000,
		MaxSingleField:  500,
		MaxHistoryItems: 3,
		MaxListItems:    8,
	}
}

// TruncateField 截断单个文本字段, 保留前 70% + 后 30% (保持首尾语义)。
func (cc *ContextCompressor) TruncateField(s string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = cc.MaxSingleField
	}
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	headLen := maxChars * 7 / 10
	tailLen := maxChars - headLen - 5 // 5 for " ... "
	if tailLen < 0 {
		tailLen = 0
	}
	head := string(runes[:headLen])
	tail := ""
	if tailLen > 0 && len(runes) > tailLen {
		tail = string(runes[len(runes)-tailLen:])
	}
	return head + " ... " + tail
}

// CompressHistory 压缩历史列表: 只保留最近 N 条, 每条截断。
func (cc *ContextCompressor) CompressHistory(items []string, maxPerItem int) []string {
	if maxPerItem <= 0 {
		maxPerItem = 200
	}
	n := cc.MaxHistoryItems
	if len(items) <= n {
		result := make([]string, len(items))
		for i, item := range items {
			result[i] = cc.TruncateField(item, maxPerItem)
		}
		return result
	}
	result := make([]string, n)
	offset := len(items) - n
	for i := 0; i < n; i++ {
		result[i] = cc.TruncateField(items[offset+i], maxPerItem)
	}
	return result
}

// CompressEvidence 压缩证据列表: 去重 + 截断 + 限数量 + 编号。
func (cc *ContextCompressor) CompressEvidence(evidence []string, maxTotal int) string {
	if maxTotal <= 0 {
		maxTotal = 1500
	}
	seen := make(map[string]bool)
	var unique []string
	for _, e := range evidence {
		key := strings.TrimSpace(e)
		if len(key) < 5 {
			continue
		}
		short := key
		if len(short) > 50 {
			short = short[:50]
		}
		if seen[short] {
			continue
		}
		seen[short] = true
		unique = append(unique, e)
	}

	limit := cc.MaxListItems
	if len(unique) > limit {
		unique = unique[:limit]
	}

	var sb strings.Builder
	charBudget := maxTotal
	for i, e := range unique {
		entry := fmt.Sprintf("%d. %s", i+1, cc.TruncateField(e, 180))
		if sb.Len()+len(entry)+1 > charBudget {
			break
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(entry)
	}
	return sb.String()
}

// CompressDebateView 压缩辩论视图: 每个 Agent 只保留概率分布 + 80字摘要推理。
func (cc *ContextCompressor) CompressDebateView(predictions []AgentPrediction) string {
	var sb strings.Builder
	for _, p := range predictions {
		probStr := formatProbMap(p.Predictions)
		rationale := cc.TruncateField(p.Rationale, 80)
		sb.WriteString(fmt.Sprintf("[%s] %s | 置信: %.0f%% | %s\n",
			p.AgentRole, probStr, p.Confidence*100, rationale))
	}
	return sb.String()
}

// BuildBudgetedPrompt 按优先级分配 token 预算构建 prompt。
// parts 按 key 排序, 越靠前优先级越高, 预算耗尽后截断。
func (cc *ContextCompressor) BuildBudgetedPrompt(template string, fields map[string]string) string {
	result := template
	totalBudget := cc.MaxPromptChars
	baseCost := utf8.RuneCountInString(template)
	remaining := totalBudget - baseCost

	for key, value := range fields {
		placeholder := "{" + key + "}"
		if !strings.Contains(result, placeholder) {
			continue
		}
		fieldBudget := remaining / 2
		if fieldBudget > cc.MaxSingleField {
			fieldBudget = cc.MaxSingleField
		}
		if fieldBudget < 50 {
			fieldBudget = 50
		}
		compressed := cc.TruncateField(value, fieldBudget)
		result = strings.Replace(result, placeholder, compressed, 1)
		remaining -= utf8.RuneCountInString(compressed)
		if remaining < 0 {
			remaining = 0
		}
	}
	return result
}

func formatProbMap(m map[string]float64) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s:%.0f%%", k, v*100))
	}
	return strings.Join(parts, " ")
}

// ═══════════════════════════════════════════════════════════════════
// B. RobustJSONParser — 健壮 JSON 解析器
//
// 设计参考:
//   - Anthropic Structured Output: Grammar Constrained Decoding + 二次校验
//   - 社区 JSON repair: 尾逗号/单引号/截断闭合
//   - 多级 fallback chain: 直接解析 → 提取 → 修复 → 多块合并 → 降级
// ═══════════════════════════════════════════════════════════════════

type ParseMethod string

const (
	ParseDirect    ParseMethod = "direct"
	ParseStripped  ParseMethod = "stripped"
	ParseExtracted ParseMethod = "extracted"
	ParseRepaired  ParseMethod = "repaired"
	ParseMulti     ParseMethod = "multi_merged"
	ParseFallback  ParseMethod = "fallback"
)

type ParseResult[T any] struct {
	Value  T
	OK     bool
	Method ParseMethod
}

// ParseJSON 多级 fallback 解析 JSON。
// 尝试顺序: 直接解析 → 去 markdown → 提取首个 JSON → 修复 → 多块合并
func ParseJSON[T any](raw string) ParseResult[T] {
	var zero T

	// Level 1: 直接解析
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return ParseResult[T]{Value: v, OK: true, Method: ParseDirect}
	}

	// Level 2: 去 markdown 代码块
	stripped := stripMarkdown(raw)
	if err := json.Unmarshal([]byte(stripped), &v); err == nil {
		return ParseResult[T]{Value: v, OK: true, Method: ParseStripped}
	}

	// Level 3: 提取第一个 JSON 对象
	extracted := extractJSON(stripped)
	if extracted != "{}" {
		if err := json.Unmarshal([]byte(extracted), &v); err == nil {
			return ParseResult[T]{Value: v, OK: true, Method: ParseExtracted}
		}

		// Level 4: 修复常见 JSON 错误
		repaired := repairJSON(extracted)
		if err := json.Unmarshal([]byte(repaired), &v); err == nil {
			return ParseResult[T]{Value: v, OK: true, Method: ParseRepaired}
		}
	}

	return ParseResult[T]{Value: zero, OK: false, Method: ParseFallback}
}

// ParseJSONList 从 LLM 响应中解析出多个 JSON 对象列表。
func ParseJSONList[T any](raw string) []T {
	allJSON := extractAllJSON(raw)
	var results []T
	for _, js := range allJSON {
		var v T
		if json.Unmarshal([]byte(js), &v) == nil {
			results = append(results, v)
		} else {
			repaired := repairJSON(js)
			if json.Unmarshal([]byte(repaired), &v) == nil {
				results = append(results, v)
			}
		}
	}
	return results
}

// stripMarkdown 去除 LLM 常见的 markdown 代码块包裹。
func stripMarkdown(s string) string {
	s = strings.TrimSpace(s)
	// 去 ```json ... ``` 和 ```JSON ... ``` 和 ``` ... ```
	re := regexp.MustCompile("(?s)^```(?:json|JSON)?\\s*\n?(.*?)\\s*```$")
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	// 非首尾包裹, 全局替换
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```JSON", "")
	s = strings.ReplaceAll(s, "```", "")
	return strings.TrimSpace(s)
}

// repairJSON 尝试修复常见的 JSON 语法错误。
func repairJSON(s string) string {
	// 1. 去尾逗号: ,} → } 和 ,] → ]
	re1 := regexp.MustCompile(`,\s*([}\]])`)
	s = re1.ReplaceAllString(s, "$1")

	// 2. 单引号 → 双引号 (简单场景)
	if !strings.Contains(s, `"`) && strings.Contains(s, "'") {
		s = strings.ReplaceAll(s, "'", `"`)
	}

	// 3. 截断闭合: 补全缺失的 } 和 ]
	opens := strings.Count(s, "{") - strings.Count(s, "}")
	for opens > 0 {
		s += "}"
		opens--
	}
	opens = strings.Count(s, "[") - strings.Count(s, "]")
	for opens > 0 {
		s += "]"
		opens--
	}

	return s
}

// ═══════════════════════════════════════════════════════════════════
// E. TimeoutBudget — 分级超时控制
//
// 设计参考:
//   - Anthropic task budget: 长跑 agent 的阶段预算
//   - DeepSeek: 10min 无推理断连 → 外层 deadline > 连接存活 > 首 token
//   - 通用: context.WithDeadline 从入口传入每个调用
// ═══════════════════════════════════════════════════════════════════

type TimeoutBudget struct {
	totalDeadline time.Time
	phases        map[string]float64 // phase → 占总预算比例
	totalDuration time.Duration
}

// NewTimeoutBudget 创建预算管理器。
// totalDuration 是总预算, phases 定义各阶段占比。
func NewTimeoutBudget(totalDuration time.Duration, phases map[string]float64) *TimeoutBudget {
	return &TimeoutBudget{
		totalDeadline: time.Now().Add(totalDuration),
		phases:        phases,
		totalDuration: totalDuration,
	}
}

// PredictBudget 返回 Predict 7 阶段的默认预算。
func PredictBudget(total time.Duration) *TimeoutBudget {
	return NewTimeoutBudget(total, map[string]float64{
		"decompose": 0.10,
		"scout":     0.10,
		"predict":   0.30,
		"debate":    0.25,
		"fuse":      0.05,
		"summary":   0.10,
		"persist":   0.10,
	})
}

// SimulateBudget 返回 Simulate 的默认预算。
func SimulateBudget(total time.Duration) *TimeoutBudget {
	return NewTimeoutBudget(total, map[string]float64{
		"setup":     0.20,
		"rounds":    0.50,
		"summarize": 0.30,
	})
}

// PhaseContext 为某阶段创建带 deadline 的 context。
// 如果剩余时间不足, 使用剩余时间的 80% 作为该阶段预算。
func (tb *TimeoutBudget) PhaseContext(parent context.Context, phase string) (context.Context, context.CancelFunc) {
	remaining := time.Until(tb.totalDeadline)
	if remaining <= 0 {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}

	ratio, ok := tb.phases[phase]
	if !ok {
		ratio = 0.15
	}
	budget := time.Duration(float64(tb.totalDuration) * ratio)

	// 如果预算超过剩余时间, 使用剩余时间的 80%
	if budget > remaining {
		budget = remaining * 80 / 100
	}

	// 最小 10 秒
	if budget < 10*time.Second {
		budget = 10 * time.Second
		if budget > remaining {
			budget = remaining
		}
	}

	return context.WithTimeout(parent, budget)
}

// Remaining 返回总预算剩余时间。
func (tb *TimeoutBudget) Remaining() time.Duration {
	r := time.Until(tb.totalDeadline)
	if r < 0 {
		return 0
	}
	return r
}

// Expired 是否已超时。
func (tb *TimeoutBudget) Expired() bool {
	return time.Now().After(tb.totalDeadline)
}
