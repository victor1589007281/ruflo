// budget.go — Token 预算分级管理器 (G2)。
//
// 对标:
//   - Kimi K2.5 128K/256K 溢出策略: 隐藏旧 tool 输出, 保留结论
//   - MemGPT / Letta: 滑动窗口摘要 + 工作集选择
//   - Anthropic TaskBudget: 按阶段分配 token 预算
//
// 当前 QueryEngine 仅有单一阈值 (AutoCompact@80%), 90% 以上会直接撞 PTL。
// 本组件引入 5 级预算:
//
//   Green    < 60%  : 无动作
//   Yellow   60-80% : 大块 tool_result 增量摘要
//   Orange   80-90% : 触发 AutoCompact (现有行为)
//   Red      90-95% : 紧急隐藏 3 轮前 tool_result, 只保留路径+结论摘要
//   Critical > 95%  : 拒绝新 tool_use 直到 reactive compact 成功
//
// 组件只负责 "判定当前等级 + 执行 Red 降级", AutoCompact 由现有 Compactor 处理。
package internal_hook

import (
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// BudgetLevel 预算等级。
type BudgetLevel int

const (
	BudgetGreen BudgetLevel = iota
	BudgetYellow
	BudgetOrange
	BudgetRed
	BudgetCritical
)

// String 返回等级名称, 便于日志。
func (l BudgetLevel) String() string {
	switch l {
	case BudgetGreen:
		return "green"
	case BudgetYellow:
		return "yellow"
	case BudgetOrange:
		return "orange"
	case BudgetRed:
		return "red"
	case BudgetCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// TokenBudgetManager 基于消息字符数粗估 token 的预算管理器。
type TokenBudgetManager struct {
	Budget int // 总预算, 一般等于 QueryEngine.ContextBudget

	// 阈值 (0-1), 零值使用默认
	YellowAt   float64
	OrangeAt   float64
	RedAt      float64
	CriticalAt float64

	// Red 降级时保留最近 N 轮完整 tool_result, 更早的压缩
	KeepRecentToolTurns int

	// 单条 tool_result 摘要字符上限 (Red 模式)
	RedSummaryMaxChars int
}

// NewTokenBudgetManager 构造默认预算管理器。
func NewTokenBudgetManager(budget int) *TokenBudgetManager {
	if budget == 0 {
		budget = 200000
	}
	return &TokenBudgetManager{
		Budget:              budget,
		YellowAt:            0.60,
		OrangeAt:            0.80,
		RedAt:               0.90,
		CriticalAt:          0.95,
		KeepRecentToolTurns: 3,
		RedSummaryMaxChars:  400,
	}
}

// EstimateTokens 粗略估算消息 token 数 (4 字符 ≈ 1 token)。
func (m *TokenBudgetManager) EstimateTokens(messages []types.Message) int {
	if m == nil {
		return 0
	}
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			total += len(block.Text) / 4
			total += len(block.Content) / 4
			total += len(block.Thinking) / 4
			// input 是 json.RawMessage, 也计入
			total += len(block.Input) / 4
		}
	}
	return total
}

// Level 返回当前预算等级。
func (m *TokenBudgetManager) Level(messages []types.Message) BudgetLevel {
	if m == nil || m.Budget <= 0 {
		return BudgetGreen
	}
	used := m.EstimateTokens(messages)
	ratio := float64(used) / float64(m.Budget)
	switch {
	case ratio >= m.CriticalAt:
		return BudgetCritical
	case ratio >= m.RedAt:
		return BudgetRed
	case ratio >= m.OrangeAt:
		return BudgetOrange
	case ratio >= m.YellowAt:
		return BudgetYellow
	default:
		return BudgetGreen
	}
}

// Degrade 在 Red 及以上执行紧急降级: 压缩 3 轮前的 tool_result。
// 返回新的 messages 切片 (原切片不被修改)。
//
// 行为:
//   - BudgetGreen/Yellow/Orange: 原样返回 (由 AutoCompact 处理)
//   - BudgetRed: 保留最近 KeepRecentToolTurns 轮, 更早 tool_result 压成 "[tool_result: <path/summary>]"
//   - BudgetCritical: 同 Red, 调用者应另外触发 reactive compact
func (m *TokenBudgetManager) Degrade(messages []types.Message, level BudgetLevel) []types.Message {
	if m == nil || level < BudgetRed || len(messages) == 0 {
		return messages
	}

	// 计算保留点: 从尾部倒数 KeepRecentToolTurns 个 tool_result 所在的消息, 之前的全部压缩
	keepFrom := findToolResultCutoff(messages, m.KeepRecentToolTurns)

	result := make([]types.Message, len(messages))
	for i, msg := range messages {
		if i >= keepFrom {
			result[i] = msg
			continue
		}
		result[i] = degradeMessage(msg, m.RedSummaryMaxChars)
	}
	return result
}

// findToolResultCutoff 从尾部往前数, 找到第 n 个 tool_result 所在消息的下标。
// 如果总 tool_result 数 < n, 返回 0 (不降级任何消息)。
func findToolResultCutoff(messages []types.Message, n int) int {
	if n <= 0 {
		return len(messages)
	}
	count := 0
	for i := len(messages) - 1; i >= 0; i-- {
		for _, b := range messages[i].Content {
			if b.Type == types.ContentBlockToolResult {
				count++
				break
			}
		}
		if count >= n {
			return i
		}
	}
	return 0
}

// degradeMessage 对单条消息内部的 tool_result 进行摘要化。
// 非 tool_result 块保持原样。
func degradeMessage(msg types.Message, maxChars int) types.Message {
	if len(msg.Content) == 0 {
		return msg
	}
	out := msg
	newContent := make([]types.ContentBlock, len(msg.Content))
	for i, b := range msg.Content {
		if b.Type == types.ContentBlockToolResult && len(b.Content) > maxChars {
			summary := summarizeToolResult(b.Content, maxChars)
			nb := b
			nb.Content = summary
			newContent[i] = nb
		} else {
			newContent[i] = b
		}
	}
	out.Content = newContent
	return out
}

// summarizeToolResult 对长 tool_result 生成压缩版:
//   - 提取首行(通常是命令/路径)
//   - 提取错误行(如有)
//   - 末尾 "... [truncated by budget red, original len=XXX]"
func summarizeToolResult(raw string, maxChars int) string {
	if len(raw) <= maxChars {
		return raw
	}
	// 启发式: 取首 N 行 + 尾 M 字符
	lines := strings.Split(raw, "\n")
	var head strings.Builder
	for i, ln := range lines {
		if i >= 3 || head.Len() >= maxChars/2 {
			break
		}
		head.WriteString(ln)
		head.WriteByte('\n')
	}
	// 扫描错误行
	var errLine string
	lower := strings.ToLower(raw)
	for _, marker := range []string{"error", "错误", "failed", "panic", "exception"} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			end := idx + 200
			if end > len(raw) {
				end = len(raw)
			}
			errLine = raw[idx:end]
			break
		}
	}

	var sb strings.Builder
	sb.WriteString(head.String())
	if errLine != "" {
		sb.WriteString("... [error excerpt] ")
		sb.WriteString(errLine)
		sb.WriteByte('\n')
	}
	sb.WriteString(fmt.Sprintf("... [truncated by budget red, original_len=%d]", len(raw)))
	out := sb.String()
	if len(out) > maxChars {
		out = out[:maxChars]
	}
	return out
}
