// adversarial.go 实现三层对抗式 Harness（生成器 vs 评估器 + 规则引擎 + 上下文管理）。
//
// 设计对齐 Anthropic Harness 思路：生成器负责产出与执行，评估器只读、多疑、多维打分；
// 规则引擎提供确定性护栏；上下文管理器在 Token 压力下触发硬重置与结构化交接（Handoff）。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 评估维度与分数
// -----------------------------------------------------------------------------

// EvalScore 评估器对生成结果的多维评分（0–10）与结论。
// Pass 表示评估器给出的总通过标志；硬门槛另见 MeetsHardPassThreshold。
//
// 新增: DesignAlignment (方案对齐度) — 参考 VERIMAP (EACL 2026)
// 检测实现与架构设计之间的偏差, 而非仅检查代码质量。
type EvalScore struct {
	Correctness     float64 `json:"correctness"`
	Completeness    float64 `json:"completeness"`
	Security        float64 `json:"security"`
	CodeQuality     float64 `json:"code_quality"`
	DesignAlignment float64 `json:"design_alignment,omitempty"` // 方案对齐度 (0-10)
	Feedback        string  `json:"feedback"`
	Pass            bool    `json:"pass"`
}

const hardPassMinScore = 6.0

// MeetsHardPassThreshold 若各维度均不低于 6/10 且 Pass 为真，则认为通过硬门槛。
// DesignAlignment > 0 时也纳入硬门槛 (方案对齐度不达标 = 不通过)
func (e EvalScore) MeetsHardPassThreshold() bool {
	if !e.Pass {
		return false
	}
	if e.DesignAlignment > 0 && e.DesignAlignment < hardPassMinScore {
		return false
	}
	return e.Correctness >= hardPassMinScore &&
		e.Completeness >= hardPassMinScore &&
		e.Security >= hardPassMinScore &&
		e.CodeQuality >= hardPassMinScore
}

// AvgScore 返回所有非零维度的平均分 (用于自适应终止判断)
func (e EvalScore) AvgScore() float64 {
	sum, count := 0.0, 0.0
	for _, v := range []float64{e.Correctness, e.Completeness, e.Security, e.CodeQuality} {
		sum += v
		count++
	}
	if e.DesignAlignment > 0 {
		sum += e.DesignAlignment
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / count
}

// IterationMemory 结构化短期记忆 (参考 MiniMax M2.7 self-evolution)。
// 记录每轮迭代的方法、评分、关键问题, 供下轮 prompt 注入, 消除重复犯错。
type IterationMemory struct {
	Round     int       `json:"round"`
	Approach  string    `json:"approach"`   // coder 采用的方法摘要
	Score     EvalScore `json:"score"`      // 该轮评分
	KeyIssues []string  `json:"key_issues"` // reviewer 指出的关键问题
	TestPass  bool      `json:"test_pass"`
	Kept      bool      `json:"kept"` // 此轮结果是否被保留 (keep/revert)
}

// FormatMemoryChain 将多轮迭代记忆格式化为 prompt 注入文本。
func FormatMemoryChain(memories []IterationMemory) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### 📋 历史迭代记忆 (避免重复犯错):\n")
	for _, m := range memories {
		status := "✅kept"
		if !m.Kept {
			status = "⏪reverted"
		}
		b.WriteString(fmt.Sprintf("- **第 %d 轮** [%s] 平均分=%.1f test=%v\n",
			m.Round, status, m.Score.AvgScore(), m.TestPass))
		if m.Approach != "" {
			b.WriteString(fmt.Sprintf("  方法: %s\n", m.Approach))
		}
		for _, issue := range m.KeyIssues {
			b.WriteString(fmt.Sprintf("  ❌ %s\n", issue))
		}
	}
	return b.String()
}

// ExtractKeyIssues 从 reviewer feedback 中提取关键问题 (最多 5 条)。
func ExtractKeyIssues(feedback string) []string {
	if feedback == "" {
		return nil
	}
	lines := strings.Split(feedback, "\n")
	var issues []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		isIssue := strings.HasPrefix(line, "-") || strings.HasPrefix(line, "•") ||
			strings.HasPrefix(line, "*") || strings.Contains(line, "FAIL") ||
			strings.Contains(line, "问题") || strings.Contains(line, "缺") ||
			strings.Contains(line, "missing") || strings.Contains(line, "error") ||
			strings.Contains(line, "不") || strings.Contains(line, "未")
		if isIssue && len(line) > 5 && len(line) < 200 {
			line = strings.TrimLeft(line, "-•* ")
			issues = append(issues, line)
			if len(issues) >= 5 {
				break
			}
		}
	}
	return issues
}

// AdaptiveTerminator 自适应对抗终止器。
// 参考:
//   - MAgICoRe (EMNLP 2025): 外部 reward model 评分驱动的自适应迭代
//   - CaRT (2025): 反事实轨迹对比, 教模型判断"何时够了"
//   - MiCP (2025): 多轮推理的置信度预测 + 覆盖率保证
//   - MiniMax M2.7: 短期记忆 + 失败轨迹分析 + 阶梯策略转换
//   - arXiv:2604.10508: "repair vs resample" — 前 2 轮修复最有效, 之后重采样更优
//
// 核心策略: 跟踪评分趋势, 检测收敛/震荡/退化, 动态决定继续或停止。
type AdaptiveTerminator struct {
	MinRounds     int       // 最少轮数 (保证充分迭代)
	MaxRounds     int       // 最多轮数 (硬上限)
	ScoreHistory  []float64 // 每轮平均分历史
	PassThreshold float64   // 通过门槛 (默认 6.0)

	// 收敛检测参数
	ConvergeEpsilon  float64 // 相邻两轮评分差小于此值视为收敛
	DegradeCount     int     // 连续退化轮数
	DegradeThreshold float64 // 退化幅度阈值: 分数下降 > 此值才计为退化 (排除噪声)

	// Best-of-N 回滚: 记录每轮输出, 退化/max_rounds 退出时回滚到历史最高分
	BestScore  float64 // 历史最高平均分
	BestRound  int     // 最高分对应的轮数
	BestOutput string  // 最高分对应的产出

	// 阶梯式策略转换 (参考 GLM 5.1 staircase optimization)
	StrategyShiftCount int // 已执行的策略转换次数
	MaxStrategyShifts  int // 最大策略转换次数 (默认 2)

	// 编译状态追踪 (L2 编译硬门禁)
	BuildPassHistory []bool // 每轮编译是否通过

	// 迭代记忆 (L5 上下文压缩, 参考 Reflexion)
	Memories []IterationMemory

	// 多维度评分历史 (用于 resample 决策)
	FullScoreHistory []EvalScore
}

// NewAdaptiveTerminator 创建自适应终止器。
// 参考 MAgICoRe: 简单问题 1-2 轮, 复杂问题最多 maxRounds 轮
func NewAdaptiveTerminator(minRounds, maxRounds int) *AdaptiveTerminator {
	if minRounds <= 0 {
		minRounds = 1
	}
	if maxRounds <= 0 {
		maxRounds = 5
	}
	return &AdaptiveTerminator{
		MinRounds:         minRounds,
		MaxRounds:         maxRounds,
		PassThreshold:     hardPassMinScore,
		ConvergeEpsilon:   0.5,
		DegradeThreshold:  0.3,
		MaxStrategyShifts: 2,
	}
}

// TerminationDecision 终止决策
type TerminationDecision struct {
	ShouldStop    bool
	Reason        string
	RoundsUsed    int
	MaxRounds     int
	BestOutput    string // 非空时表示应使用此输出 (best-of-N 回滚)
	BestRound     int    // 最高分对应的轮数
	StrategyShift bool   // true = 不终止, 而是触发策略转换 (参考 GLM 5.1)
}

// RecordRoundOutput 记录每轮的评分和产出, 用于 best-of-N 回滚。
// 必须在 ShouldTerminate 之前调用。
func (at *AdaptiveTerminator) RecordRoundOutput(round int, score EvalScore, output string) {
	avg := score.AvgScore()
	if avg > at.BestScore || at.BestRound == 0 {
		at.BestScore = avg
		at.BestRound = round
		at.BestOutput = output
	}
	at.FullScoreHistory = append(at.FullScoreHistory, score)
}

// RecordBuildResult 记录每轮编译结果 (L2 编译硬门禁)。
func (at *AdaptiveTerminator) RecordBuildResult(passed bool) {
	at.BuildPassHistory = append(at.BuildPassHistory, passed)
}

// RecordIterationMemory 记录本轮迭代记忆 (L5)。
func (at *AdaptiveTerminator) RecordIterationMemory(round int, score EvalScore, approach string, issues []string, kept bool) {
	at.Memories = append(at.Memories, IterationMemory{
		Round:     round,
		Approach:  approach,
		Score:     score,
		KeyIssues: issues,
		TestPass:  len(at.BuildPassHistory) > 0 && at.BuildPassHistory[len(at.BuildPassHistory)-1],
		Kept:      kept,
	})
}

// ShouldResample 判断是否应该重采样而非继续修补 (参考 arXiv:2604.10508)。
// 条件: Completeness 持续 <5 且编译未通过 → 说明当前方向错误, 需要换思路。
// 返回 true 时调用方应清空 lastGenOutput, 用简化 prompt 重新生成。
func (at *AdaptiveTerminator) ShouldResample() bool {
	if len(at.FullScoreHistory) < 2 {
		return false
	}
	consecutiveLowCompleteness := 0
	consecutiveBuildFail := 0
	for i := len(at.FullScoreHistory) - 1; i >= 0 && i >= len(at.FullScoreHistory)-2; i-- {
		if at.FullScoreHistory[i].Completeness < 5 {
			consecutiveLowCompleteness++
		}
	}
	for i := len(at.BuildPassHistory) - 1; i >= 0 && i >= len(at.BuildPassHistory)-2; i-- {
		if !at.BuildPassHistory[i] {
			consecutiveBuildFail++
		}
	}
	return consecutiveLowCompleteness >= 2 && consecutiveBuildFail >= 2
}

// ShouldRevert 每轮即时 keep/revert 决策 (参考 MiniMax M2.7)。
// 当本轮评分相比历史最高分下降超过 DegradeThreshold 时, 返回 true + 最佳输出。
// 调用方应将 coder 下一轮的基础从 lastOutput 切换为 BestOutput。
func (at *AdaptiveTerminator) ShouldRevert(currentScore EvalScore) (revert bool, bestOutput string, bestRound int) {
	avg := currentScore.AvgScore()
	if at.BestRound == 0 || at.BestScore == 0 {
		return false, "", 0
	}
	// 多维度回归检测 (参考 MiniMax M2.7): 任一核心维度下降>1.0 也触发 revert
	dimRegression := currentScore.Correctness < 5 || currentScore.Security < 5
	avgDrop := at.BestScore - avg
	if (avgDrop > at.DegradeThreshold || dimRegression) && at.BestOutput != "" {
		return true, at.BestOutput, at.BestRound
	}
	return false, "", 0
}

// ShouldTerminate 根据当前轮评分决定是否终止。
// 策略 (参考 MAgICoRe + CaRT + best-of-N rollback):
//  1. 通过硬门槛 → 立即停止 (质量达标)
//  2. 未达最小轮数 → 继续 (保证充分探索)
//  3. 达到最大轮数 → 强制停止 (附带 best-of-N 回滚)
//  4. 连续 3 轮评分下降超过 DegradeThreshold (退化) → 提前停止 + 回滚到最高分输出
//  5. 相邻评分差 < epsilon (收敛/震荡) → 停止 (改进已饱和)
func (at *AdaptiveTerminator) ShouldTerminate(round int, score EvalScore) TerminationDecision {
	avg := score.AvgScore()
	at.ScoreHistory = append(at.ScoreHistory, avg)
	n := len(at.ScoreHistory)

	// 1. 通过硬门槛
	if score.MeetsHardPassThreshold() {
		return TerminationDecision{ShouldStop: true, Reason: "quality_pass", RoundsUsed: round, MaxRounds: at.MaxRounds}
	}

	// 2. 未达最小轮数
	if round < at.MinRounds {
		return TerminationDecision{ShouldStop: false, Reason: "min_rounds", RoundsUsed: round, MaxRounds: at.MaxRounds}
	}

	// 3. 达到最大轮数 → best-of-N 回滚
	if round >= at.MaxRounds {
		d := TerminationDecision{ShouldStop: true, Reason: "max_rounds", RoundsUsed: round, MaxRounds: at.MaxRounds}
		if at.BestOutput != "" && at.BestRound != round {
			d.BestOutput = at.BestOutput
			d.BestRound = at.BestRound
		}
		return d
	}

	// 4. 宽容退化检测: 分数下降幅度 > DegradeThreshold 才计为退化
	if n >= 2 {
		drop := at.ScoreHistory[n-2] - at.ScoreHistory[n-1]
		if drop > at.DegradeThreshold {
			at.DegradeCount++
		} else if at.ScoreHistory[n-1] >= at.ScoreHistory[n-2] {
			at.DegradeCount = 0
		}
	}
	if at.DegradeCount >= 3 {
		d := TerminationDecision{ShouldStop: true, Reason: "degradation", RoundsUsed: round, MaxRounds: at.MaxRounds}
		if at.BestOutput != "" && at.BestRound != round {
			d.BestOutput = at.BestOutput
			d.BestRound = at.BestRound
		}
		return d
	}

	// 5. 收敛/震荡检测 → 策略转换 or 退出 (参考 GLM 5.1 staircase)
	if n >= 2 {
		delta := at.ScoreHistory[n-1] - at.ScoreHistory[n-2]
		if delta >= 0 && delta < at.ConvergeEpsilon {
			if at.StrategyShiftCount < at.MaxStrategyShifts {
				at.StrategyShiftCount++
				return TerminationDecision{ShouldStop: false, Reason: "strategy_shift",
					RoundsUsed: round, MaxRounds: at.MaxRounds, StrategyShift: true}
			}
			return TerminationDecision{ShouldStop: true, Reason: "converged", RoundsUsed: round, MaxRounds: at.MaxRounds}
		}
	}

	return TerminationDecision{ShouldStop: false, Reason: "improving", RoundsUsed: round, MaxRounds: at.MaxRounds}
}

// SkepticalReviewerPersona 用于评估器系统提示的「多疑评审者」人设（硬门槛：各维度 ≥6/10）。
const SkepticalReviewerPersona = `你是一名严格、多疑的独立评审者（只读验证，不得改代码）。
你的职责是挑错：假设生成器可能遗漏需求、引入安全隐患或低质量实现。
请从以下四个维度分别给出 0–10 的分数（整数或一位小数），并给出可执行的改进反馈（Feedback）：
1) Correctness — 逻辑与事实是否正确
2) Completeness — 是否覆盖目标与边界情况
3) Security — 密钥、注入、权限与供应链风险
4) CodeQuality — 可读性、结构与可维护性

硬通过条件：四个维度均 ≥ 6，且你明确认为可以交付（Pass=true）。
若任一维度 < 6 或存在阻塞问题，必须 Pass=false，并在 Feedback 中列出具体修复项。
输出必须为 JSON，字段名：correctness, completeness, security, code_quality, feedback, pass。`

// BuildSkepticalEvaluatorUserPrompt 组装发给评估模型的用户消息（objective + 生成器产出摘要）。
func BuildSkepticalEvaluatorUserPrompt(objective, generatorOutput string) string {
	var b strings.Builder
	b.WriteString("【任务目标】\n")
	b.WriteString(objective)
	b.WriteString("\n\n【生成器产出（供审查）】\n")
	if len(generatorOutput) > 24000 {
		b.WriteString(generatorOutput[:24000])
		b.WriteString("\n\n…(已截断)")
	} else {
		b.WriteString(generatorOutput)
	}
	b.WriteString("\n\n请严格按系统说明输出 JSON 格式的 EvalScore。")
	return b.String()
}

// ParseEvalScoreJSON 从模型返回的文本中提取并解析 EvalScore。
// 支持多种格式: 纯 JSON、```json 代码块包裹、markdown 前后包含说明文本。
// 根因修复: reviewer 倾向于输出详细的 markdown 审查报告后附带 JSON，
// 原实现只做 json.Unmarshal(全文) 导致解析失败, 所有评分变为 0/0/0/0。
func ParseEvalScoreJSON(raw []byte) (EvalScore, error) {
	text := string(raw)

	// 策略 1: 直接解析全文 (纯 JSON 输出)
	var s EvalScore
	if err := json.Unmarshal(raw, &s); err == nil && (s.Correctness > 0 || s.Completeness > 0) {
		return s, nil
	}

	// 策略 2: 提取 ```json ... ``` 代码块中的 JSON
	if idx := strings.Index(text, "```json"); idx >= 0 {
		start := idx + 7
		if end := strings.Index(text[start:], "```"); end >= 0 {
			block := strings.TrimSpace(text[start : start+end])
			if err := json.Unmarshal([]byte(block), &s); err == nil {
				return s, nil
			}
		}
	}
	// 也尝试 ``` 无语言标记的代码块
	if idx := strings.Index(text, "```\n{"); idx >= 0 {
		start := idx + 4
		if end := strings.Index(text[start:], "```"); end >= 0 {
			block := strings.TrimSpace(text[start : start+end])
			if err := json.Unmarshal([]byte(block), &s); err == nil {
				return s, nil
			}
		}
	}

	// 策略 3: 找到最后一个 {...} JSON 对象 (reviewer 通常在末尾输出 JSON)
	lastBrace := strings.LastIndex(text, "}")
	if lastBrace >= 0 {
		// 从 lastBrace 往前找匹配的 {
		depth := 0
		for i := lastBrace; i >= 0; i-- {
			if text[i] == '}' {
				depth++
			} else if text[i] == '{' {
				depth--
				if depth == 0 {
					candidate := text[i : lastBrace+1]
					if err := json.Unmarshal([]byte(candidate), &s); err == nil && (s.Correctness > 0 || s.Completeness > 0) {
						return s, nil
					}
					break
				}
			}
		}
	}

	// 策略 4: 用正则提取各维度数值 (兜底: reviewer 输出了表格或非标准格式)
	s = extractScoreFromText(text)
	if s.Correctness > 0 || s.Completeness > 0 || s.Security > 0 || s.CodeQuality > 0 {
		return s, nil
	}

	return EvalScore{}, fmt.Errorf("无法从 evaluator 输出中提取评分 (长度: %d)", len(raw))
}

// extractScoreFromText 从非 JSON 文本中提取评分 (正则兜底)。
// 匹配模式: "correctness: 8" "正确性: 8/10" "security = 9" 等。
func extractScoreFromText(text string) EvalScore {
	text = strings.ToLower(text)
	var s EvalScore

	// 有序匹配: 长关键词优先, 避免 "正确" 匹配到 "正确性" 的中间位置
	type kv struct {
		keyword string
		target  *float64
	}
	patterns := []kv{
		{"correctness", &s.Correctness},
		{"正确性", &s.Correctness},
		{"正确", &s.Correctness},
		{"completeness", &s.Completeness},
		{"完整性", &s.Completeness},
		{"完整", &s.Completeness},
		{"security", &s.Security},
		{"安全性", &s.Security},
		{"安全", &s.Security},
		{"code_quality", &s.CodeQuality},
		{"代码质量", &s.CodeQuality},
		{"质量", &s.CodeQuality},
	}

	for _, p := range patterns {
		if *p.target > 0 {
			continue // 已被更长的关键词匹配
		}
		idx := strings.Index(text, p.keyword)
		if idx < 0 {
			continue
		}
		after := text[idx+len(p.keyword):]
		// 跳过分隔符和非数字字符 (: = 空格 中文标点等)
		for len(after) > 0 {
			r := rune(after[0])
			if (r >= '0' && r <= '9') || r == '.' {
				break
			}
			after = after[1:]
			if len(after) == 0 {
				break
			}
		}
		var numStr string
		for _, ch := range after {
			if ch >= '0' && ch <= '9' || ch == '.' {
				numStr += string(ch)
			} else {
				break
			}
		}
		if numStr != "" {
			val := 0.0
			fmt.Sscanf(numStr, "%f", &val)
			if val > 0 && val <= 10 {
				*p.target = val
			}
		}
	}

	// pass 判断
	if strings.Contains(text, "\"pass\": true") || strings.Contains(text, "\"pass\":true") ||
		strings.Contains(text, "pass: true") || strings.Contains(text, "通过: true") {
		s.Pass = true
	}

	return s
}

// -----------------------------------------------------------------------------
// 规则引擎
// -----------------------------------------------------------------------------

// Rule 单条确定性规则：对单次工具调用做校验。
type Rule struct {
	Name string
	Check func(toolName string, input json.RawMessage) error
}

// RuleEngine 工具调用前的护栏集合；可配置 TargetPathPrefixes 以约束「仅允许操作目标路径前缀」。
type RuleEngine struct {
	mu sync.RWMutex

	// TargetPathPrefixes 非空时：删除类 Shell 命令所针对的路径必须匹配其中任一条前缀（规范化后比较）。
	// 为空时不启用「非目标删除」校验（避免误杀未配置场景）。
	TargetPathPrefixes []string

	rules []Rule
}

// NewRuleEngine 构造带内置规则的引擎：禁止非目标删除、禁止提交密钥、禁止明显死循环命令。
func NewRuleEngine() *RuleEngine {
	e := &RuleEngine{}
	e.AddRule(Rule{Name: "no-delete-non-target", Check: e.checkNoDeleteNonTarget})
	e.AddRule(Rule{Name: "no-commit-secrets", Check: checkNoCommitSecrets})
	e.AddRule(Rule{Name: "no-infinite-loop-patterns", Check: checkNoInfiniteLoopPatterns})
	return e
}

// AddRule 追加规则（线程安全）。
func (e *RuleEngine) AddRule(r Rule) {
	if e == nil || r.Check == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = append(e.rules, r)
}

// Check 依次执行所有规则；返回第一条失败的错误。
func (e *RuleEngine) Check(toolName string, input json.RawMessage) error {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	rs := append([]Rule(nil), e.rules...)
	e.mu.RUnlock()
	for _, r := range rs {
		if r.Check == nil {
			continue
		}
		if err := r.Check(toolName, input); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	return nil
}

// SetTargetPathPrefixes 设置允许作为「目标工作区」的路径前缀列表。
func (e *RuleEngine) SetTargetPathPrefixes(prefixes []string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.TargetPathPrefixes = append([]string(nil), prefixes...)
}

func normalizePathKey(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return p
}

func pathMatchesAnyPrefix(path string, prefixes []string) bool {
	path = normalizePathKey(path)
	for _, pre := range prefixes {
		pre = normalizePathKey(pre)
		if pre == "" {
			continue
		}
		if strings.HasPrefix(path, pre) || strings.HasPrefix(path, pre+"/") {
			return true
		}
	}
	return false
}

func (e *RuleEngine) checkNoDeleteNonTarget(toolName string, input json.RawMessage) error {
	e.mu.RLock()
	prefixes := append([]string(nil), e.TargetPathPrefixes...)
	e.mu.RUnlock()
	if len(prefixes) == 0 {
		return nil
	}
	tn := strings.TrimSpace(toolName)
	switch tn {
	case "Shell":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) != nil {
			return nil
		}
		cmd := strings.ToLower(strings.TrimSpace(in.Command))
		if !looksLikeDeleteCommand(cmd) {
			return nil
		}
		for _, tok := range tokenizePathsFromShell(cmd) {
			if tok == "" {
				continue
			}
			if !pathMatchesAnyPrefix(tok, prefixes) {
				return fmt.Errorf("禁止删除或破坏非目标路径: %q（允许前缀: %v）", tok, prefixes)
			}
		}
		return nil
	default:
		return nil
	}
}

func looksLikeDeleteCommand(cmd string) bool {
	if strings.Contains(cmd, "rm ") || strings.HasPrefix(cmd, "rm\t") {
		return true
	}
	if strings.Contains(cmd, "unlink ") || strings.Contains(cmd, "shred ") {
		return true
	}
	if strings.Contains(cmd, "git rm") || strings.Contains(cmd, "git clean") {
		return true
	}
	return false
}

// tokenizePathsFromShell 从删除类命令中粗提取路径 token（生产级启发式，非完整 shell 解析）。
func tokenizePathsFromShell(cmd string) []string {
	// 去掉管道后段，聚焦第一段删除命令
	if idx := strings.Index(cmd, "|"); idx >= 0 {
		cmd = cmd[:idx]
	}
	fields := strings.Fields(cmd)
	var out []string
	// git rm <paths>...
	for k := 0; k < len(fields)-1; k++ {
		if fields[k] == "git" && fields[k+1] == "rm" {
			for m := k + 2; m < len(fields); m++ {
				if strings.HasPrefix(fields[m], "-") {
					continue
				}
				tok := strings.Trim(fields[m], `"'`)
				if tok != "" && tok != "&&" && tok != ";" {
					out = append(out, tok)
				}
			}
			return out
		}
	}
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "rm" || f == "unlink" || f == "shred" {
			j := i + 1
			for j < len(fields) && strings.HasPrefix(fields[j], "-") {
				j++
			}
			for j < len(fields) && !strings.HasPrefix(fields[j], "-") {
				tok := strings.Trim(fields[j], `"'`)
				if tok != "" && tok != "&&" && tok != ";" {
					out = append(out, tok)
				}
				j++
			}
			i = j - 1
		}
	}
	return out
}

func checkNoCommitSecrets(toolName string, input json.RawMessage) error {
	tn := strings.TrimSpace(toolName)
	switch tn {
	case "Shell":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) == nil {
			if hits := detectSecretPatterns(in.Command); len(hits) > 0 {
				return fmt.Errorf("疑似密钥或敏感内容，禁止通过 Shell 提交/写入: %s", strings.Join(hits, ", "))
			}
			low := strings.ToLower(in.Command)
			if strings.Contains(low, "git commit") && (strings.Contains(low, ".env") || strings.Contains(low, "id_rsa") || strings.Contains(low, "credentials")) {
				return fmt.Errorf("禁止将 .env / 私钥 / 凭证类文件纳入提交命令")
			}
		}
	case "Write":
		var in struct {
			Path     string `json:"path"`
			Contents string `json:"contents"`
		}
		if json.Unmarshal(input, &in) == nil {
			if hits := detectSecretPatterns(in.Contents); len(hits) > 0 {
				return fmt.Errorf("写入内容疑似包含敏感信息: %s", strings.Join(hits, ", "))
			}
		}
	case "StrReplace":
		var in struct {
			NewString string `json:"new_string"`
		}
		if json.Unmarshal(input, &in) == nil {
			if hits := detectSecretPatterns(in.NewString); len(hits) > 0 {
				return fmt.Errorf("替换内容疑似包含敏感信息: %s", strings.Join(hits, ", "))
			}
		}
	}
	return nil
}

func detectSecretPatterns(s string) []string {
	var hits []string
	lower := strings.ToLower(s)
	checks := []struct {
		needle, label string
	}{
		{"sk-ant-", "Anthropic API key pattern"},
		{"sk_live_", "Stripe-style secret"},
		{"sk_test_", "Stripe-style test secret"},
		{"-----begin private key-----", "PEM private key"},
		{"-----begin rsa private key-----", "RSA private key"},
		{"api_key=", "api_key assignment"},
		{"aws_secret_access_key", "AWS secret key"},
		{"ghp_", "GitHub PAT (ghp_)"},
		{"github_pat_", "GitHub PAT (github_pat_)"},
		{"xoxb-", "Slack bot token"},
	}
	for _, c := range checks {
		if strings.Contains(lower, strings.ToLower(c.needle)) {
			hits = append(hits, c.label)
		}
	}
	return hits
}

func checkNoInfiniteLoopPatterns(toolName string, input json.RawMessage) error {
	if strings.TrimSpace(toolName) != "Shell" {
		return nil
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	cmd := strings.ToLower(in.Command)
	patterns := []string{
		"while true",
		"while :",
		"for ((;;))",
		":(){ :|:& };:",
		"fork bomb",
	}
	for _, p := range patterns {
		if strings.Contains(cmd, p) {
			return fmt.Errorf("禁止可能无限循环或 fork bomb 的 Shell 模式: %q", p)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// 上下文管理器与 Handoff
// -----------------------------------------------------------------------------

// HandoffArtifact 硬重置时可序列化传递的结构化状态（JSON）。
type HandoffArtifact struct {
	Progress     string   `json:"progress"`
	Decisions    []string `json:"decisions"`
	PendingTasks []string `json:"pending_tasks"`
	Summary      string   `json:"summary"`
}

// ContextManager 跟踪 Token 使用量，并在接近预算时建议硬重置与 Handoff。
type ContextManager struct {
	mu sync.Mutex

	// EstimatedTokens 当前估计已消耗 Token（由调用方按需累加）。
	EstimatedTokens int64
	// MaxTokens 预算上限；ShouldReset 可与显式参数联用。
	MaxTokens int64

	LastResetAt    time.Time
	HandoffHistory []HandoffArtifact
}

// NewContextManager 创建上下文管理器；maxTokens<=0 时不在实例方法中使用默认阈值。
func NewContextManager(maxTokens int64) *ContextManager {
	return &ContextManager{MaxTokens: maxTokens}
}

// AddTokens 累加估计 Token 用量。
func (c *ContextManager) AddTokens(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.mu.Lock()
	c.EstimatedTokens += n
	c.mu.Unlock()
}

// ResetTokens 将计数清零并记录重置时间。
func (c *ContextManager) ResetTokens() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.EstimatedTokens = 0
	c.LastResetAt = time.Now()
	c.mu.Unlock()
}

// ShouldReset 当 currentTokens > 80% * maxTokens 时返回 true（容量「硬重置」阈值）。
func (c *ContextManager) ShouldReset(currentTokens, maxTokens int64) bool {
	if maxTokens <= 0 {
		return false
	}
	return currentTokens*10 > 8*maxTokens
}

// ShouldResetWithBudget 使用实例内 EstimatedTokens 与 MaxTokens（若 maxOverride>0 则覆盖上限）。
func (c *ContextManager) ShouldResetWithBudget(maxOverride int64) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	cur := c.EstimatedTokens
	max := c.MaxTokens
	c.mu.Unlock()
	if maxOverride > 0 {
		max = maxOverride
	}
	return c.ShouldReset(cur, max)
}

// CreateHandoff 根据进度与决策生成交接工件；Summary 由进度与决策自动摘要。
func (c *ContextManager) CreateHandoff(progress string, decisions []string) HandoffArtifact {
	sum := progress
	if len(decisions) > 0 {
		if sum != "" {
			sum += " | "
		}
		sum += "决策: " + strings.Join(decisions, "; ")
	}
	art := HandoffArtifact{
		Progress:     progress,
		Decisions:    append([]string(nil), decisions...),
		PendingTasks: nil,
		Summary:      sum,
	}
	if c != nil {
		c.mu.Lock()
		c.HandoffHistory = append(c.HandoffHistory, art)
		c.mu.Unlock()
	}
	return art
}

// SetPendingOnArtifact 返回附带待办列表的副本（不修改接收者语义，便于链式构造）。
func SetPendingOnArtifact(a HandoffArtifact, pending []string) HandoffArtifact {
	a.PendingTasks = append([]string(nil), pending...)
	return a
}

// MarshalHandoff 将 HandoffArtifact 序列化为 JSON。
func MarshalHandoff(a HandoffArtifact) ([]byte, error) {
	return json.Marshal(a)
}

// -----------------------------------------------------------------------------
// 对抗式编排器
// -----------------------------------------------------------------------------

const defaultAdversarialMaxRetries = 3

// AdversarialGenerator 生成器：根据目标、团队上下文与上一轮反馈产出文本结果（如代码或任务报告）。
type AdversarialGenerator func(ctx context.Context, objective string, team *ProductionTeam, priorFeedback string, attempt int) (output string, err error)

// AdversarialEvaluator 评估器：只读审查生成结果并返回 EvalScore。
type AdversarialEvaluator func(ctx context.Context, objective string, generatorOutput string, team *ProductionTeam) (EvalScore, error)

// AdversarialRunner 三层编排：L1 生成 vs 评估；L2 规则引擎（工具层应调用 RuleEngine.Check）；L3 上下文与 Handoff。
type AdversarialRunner struct {
	Generator  AdversarialGenerator
	Evaluator  AdversarialEvaluator
	Rules      *RuleEngine
	Context    *ContextManager
	MaxRetries int
}

// AdversarialOutcome 一次 Execute 的最终结果。
type AdversarialOutcome struct {
	Output     string
	FinalScore EvalScore
	Attempts   int
	Handoff    *HandoffArtifact
}

// HandoffResetError 表示上下文超过阈值，需要硬重置并携带交接工件。
type HandoffResetError struct {
	Artifact HandoffArtifact
}

func (e *HandoffResetError) Error() string {
	return "context manager: 建议硬重置（已超过 80% Token 预算）"
}

// Execute 运行生成器 → 评估器，若未满足硬门槛则用 EvalScore.Feedback 作为下一轮提示，最多重试 MaxRetries 次（默认 3）。
// 在 ContextManager 存在且 ShouldResetWithBudget(0) 为真时，返回 *HandoffResetError。
func (r *AdversarialRunner) Execute(ctx context.Context, objective string, team *ProductionTeam) (*AdversarialOutcome, error) {
	if r == nil {
		return nil, fmt.Errorf("AdversarialRunner 为空")
	}
	if r.Generator == nil || r.Evaluator == nil {
		return nil, fmt.Errorf("Generator 与 Evaluator 必须非空")
	}
	max := r.MaxRetries
	if max <= 0 {
		max = defaultAdversarialMaxRetries
	}

	var lastOut string
	var lastScore EvalScore

	for attempt := 1; attempt <= max; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if r.Context != nil && r.Context.ShouldResetWithBudget(0) {
			ho := r.Context.CreateHandoff(
				fmt.Sprintf("第 %d 轮前检测到上下文接近上限", attempt),
				[]string{"触发硬重置阈值（>80%）", "暂停对抗循环，等待交接续跑"},
			)
			return &AdversarialOutcome{
				Output:     lastOut,
				FinalScore: lastScore,
				Attempts:   attempt - 1,
				Handoff:    &ho,
			}, &HandoffResetError{Artifact: ho}
		}

		feedback := lastScore.Feedback
		if attempt > 1 && feedback == "" {
			feedback = "上一轮未通过硬门槛，请针对正确性、完整性、安全与代码质量全面改进。"
		}

		out, err := r.Generator(ctx, objective, team, feedback, attempt)
		if err != nil {
			return nil, fmt.Errorf("generator: %w", err)
		}
		lastOut = out

		score, err := r.Evaluator(ctx, objective, out, team)
		if err != nil {
			return nil, fmt.Errorf("evaluator: %w", err)
		}
		lastScore = score

		if score.MeetsHardPassThreshold() {
			return &AdversarialOutcome{
				Output:     out,
				FinalScore: score,
				Attempts:   attempt,
				Handoff:    nil,
			}, nil
		}
	}

	return &AdversarialOutcome{
		Output:     lastOut,
		FinalScore: lastScore,
		Attempts:   max,
		Handoff:    nil,
	}, nil
}
