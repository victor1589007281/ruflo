// hook_toolresult_level.go — ToolResult 分级保留内置 Hook。
//
// 设计依据: review/context-optimization-design.md §5.2
// 目标: 根据消息"年龄"（距离当前轮次）和"重要性"对 tool_result 实施分级保留，
// 减少历史成功确认类 tool_result 对上下文的长期占用。
//
// 分级策略:
//   Level 0 (当前轮):   完整保留，原始长度
//   Level 1 (前 1-2 轮): 截断保留，最多 500 字符
//   Level 2 (前 3-5 轮): 仅保留状态标记（成功/失败 + 工具名）
//   Level 3 (> 5 轮):    成功类完全移除或替换为极简标记；失败类保留 200 字符摘要
//
// 安全原则:
//   - IsError=true 的 tool_result 永远不被完全删除（最多 Level 2 标记化）
//   - 以"assistant + 其后的 tool_result user 消息"为原子轮次判定
//   - 修改发生在 PhasePreRequest，早于 MessageFilterHook，因此过滤逻辑看到的是已降级的消息
package internal_hook

import (
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// ToolResultLevel — 保留等级枚举
// ============================================================================

type ToolResultLevel int

const (
	Level0 ToolResultLevel = iota // 当前轮: 完整保留
	Level1                        // 前 1-2 轮: 截断到 500 字符
	Level2                        // 前 3-5 轮: 仅保留状态标记
	Level3                        // > 5 轮: 成功类移除/极简标记，失败类保留 200 字符摘要
)

// ToolResultLevelConfig 分级策略配置。
type ToolResultLevelConfig struct {
	Level0KeepRounds    int    // Level 0 保留最近几轮 (默认 1)
	Level1KeepRounds    int    // Level 1 保留前 1-2 轮 (默认 2)
	Level2KeepRounds    int    // Level 2 保留前 3-5 轮 (默认 3)
	Level1MaxChars      int    // Level 1 截断阈值 (默认 500)
	Level3SuccessMarker string // Level 3 成功类替换标记模板
	Level3ErrorSummary  int    // Level 3 失败类保留字符数 (默认 200)
	PreserveErrors      bool   // 是否永远保留 IsError=true 的完整内容 (默认 true)
}

// DefaultToolResultLevelConfig 返回默认配置。
func DefaultToolResultLevelConfig() *ToolResultLevelConfig {
	return &ToolResultLevelConfig{
		Level0KeepRounds:    1,
		Level1KeepRounds:    2,
		Level2KeepRounds:    3,
		Level1MaxChars:      500,
		Level3SuccessMarker: "[历史操作] %s: %s (已完成)",
		Level3ErrorSummary:  200,
		PreserveErrors:      true,
	}
}

// ============================================================================
// ToolResultLevelHook — ToolResult 分级降级
// ============================================================================

// ToolResultLevelHook 在 PhasePreRequest 阶段对历史 tool_result 实施分级保留。
type ToolResultLevelHook struct {
	config  *ToolResultLevelConfig
	metrics *EngineMetrics
}

// NewToolResultLevelHook 创建 ToolResultLevelHook。
func NewToolResultLevelHook(config *ToolResultLevelConfig, metrics *EngineMetrics) *ToolResultLevelHook {
	if config == nil {
		config = DefaultToolResultLevelConfig()
	}
	return &ToolResultLevelHook{config: config, metrics: metrics}
}

func (h *ToolResultLevelHook) Name() string                { return "toolresult_level" }
func (h *ToolResultLevelHook) Priority() int               { return 38 }
func (h *ToolResultLevelHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 对 messages 中的 tool_result 按轮次年龄分级降级。
func (h *ToolResultLevelHook) Execute(ctx *HookContext) (*HookResult, error) {
	messages := ctx.Messages
	if len(messages) == 0 {
		return nil, nil
	}

	// 1. 计算每条消息所属的轮次（从尾部倒数第几轮）
	//    轮次边界: assistant 消息标志一轮的开始
	msgRound := computeMessageRounds(messages)

	// 2. 对每条消息的 content blocks 做分级处理
	var changed bool
	var levelStats [4]int64 // Level0/1/2/3 各自处理了多少个 tool_result
	for i := range messages {
		if messages[i].Type != types.MessageTypeUser {
			continue
		}
		round := msgRound[i]
		level := h.determineLevel(round)
		if level == Level0 {
			continue
		}

		newBlocks := make([]types.ContentBlock, 0, len(messages[i].Content))
		for _, b := range messages[i].Content {
			if b.Type != types.ContentBlockToolResult {
				newBlocks = append(newBlocks, b)
				continue
			}
			levelStats[level]++
			degraded := h.degradeBlock(b, level)
			if degraded != nil {
				newBlocks = append(newBlocks, *degraded)
			}
			// 如果 degradeBlock 返回 nil，表示完全移除该 block
		}
		if len(newBlocks) != len(messages[i].Content) {
			messages[i].Content = newBlocks
			changed = true
		}
	}

	if changed {
		logging.For("engine").Debug("ToolResultLevelHook applied",
			"l1", levelStats[Level1],
			"l2", levelStats[Level2],
			"l3", levelStats[Level3],
		)
	}

	if !changed {
		return nil, nil
	}
	return &HookResult{Messages: messages}, nil
}

// determineLevel 根据轮次偏移确定保留等级。
// round=0 表示最近一轮（当前轮），round=1 表示前一轮，以此类推。
func (h *ToolResultLevelHook) determineLevel(round int) ToolResultLevel {
	if round < h.config.Level0KeepRounds {
		return Level0
	}
	if round < h.config.Level0KeepRounds+h.config.Level1KeepRounds {
		return Level1
	}
	if round < h.config.Level0KeepRounds+h.config.Level1KeepRounds+h.config.Level2KeepRounds {
		return Level2
	}
	return Level3
}

// degradeBlock 对单个 tool_result block 按等级降级。
// 返回 nil 表示完全移除该 block。
func (h *ToolResultLevelHook) degradeBlock(b types.ContentBlock, level ToolResultLevel) *types.ContentBlock {
	switch level {
	case Level0:
		return &b
	case Level1:
		return h.degradeLevel1(b)
	case Level2:
		return h.degradeLevel2(b)
	case Level3:
		return h.degradeLevel3(b)
	}
	return &b
}

// degradeLevel1: 截断到 Level1MaxChars 字符，保留前部。
func (h *ToolResultLevelHook) degradeLevel1(b types.ContentBlock) *types.ContentBlock {
	if h.config.PreserveErrors && b.IsError {
		return &b // 错误信息完整保留
	}
	if len(b.Content) <= h.config.Level1MaxChars {
		return &b
	}
	return &types.ContentBlock{
		Type:      b.Type,
		ToolUseID: b.ToolUseID,
		Content:   b.Content[:h.config.Level1MaxChars] + "\n... (truncated by level1)",
		IsError:   b.IsError,
	}
}

// degradeLevel2: 仅保留状态标记（成功/失败 + 工具名）。
func (h *ToolResultLevelHook) degradeLevel2(b types.ContentBlock) *types.ContentBlock {
	if h.config.PreserveErrors && b.IsError {
		// 错误信息保留 300 字符摘要
		summary := b.Content
		if len(summary) > 300 {
			summary = summary[:300] + "..."
		}
		return &types.ContentBlock{
			Type:      b.Type,
			ToolUseID: b.ToolUseID,
			Content:   fmt.Sprintf("[历史错误] %s", summary),
			IsError:   true,
		}
	}
	status := "成功"
	if b.IsError {
		status = "失败"
	}
	return &types.ContentBlock{
		Type:      b.Type,
		ToolUseID: b.ToolUseID,
		Content:   fmt.Sprintf("[历史操作结果: %s]", status),
		IsError:   b.IsError,
	}
}

// degradeLevel3: 成功类完全移除（返回 nil），失败类保留 200 字符摘要。
func (h *ToolResultLevelHook) degradeLevel3(b types.ContentBlock) *types.ContentBlock {
	if b.IsError {
		summary := b.Content
		if len(summary) > h.config.Level3ErrorSummary {
			summary = summary[:h.config.Level3ErrorSummary] + "..."
		}
		return &types.ContentBlock{
			Type:      b.Type,
			ToolUseID: b.ToolUseID,
			Content:   fmt.Sprintf("[历史错误摘要] %s", summary),
			IsError:   true,
		}
	}
	// 成功类: 返回 nil → 完全移除
	return nil
}

// ============================================================================
// 辅助函数
// ============================================================================

// computeMessageRounds 计算每条消息所属的轮次（从尾部倒数第几轮）。
// 以 assistant 消息为轮次边界。例如:
//   messages: [U0, A0, T0, A1, T1, A2, T2]  (U=user, A=assistant, T=tool_result)
//   rounds:   [2,  2,  2,  1,  1,  0,  0 ]  (从尾部数，A2/T2 是第 0 轮)
func computeMessageRounds(messages []types.Message) []int {
	n := len(messages)
	rounds := make([]int, n)

	// 从尾部开始扫描，找到 assistant 消息的位置作为轮次边界
	currentRound := 0
	lastAssistantIdx := -1

	for i := n - 1; i >= 0; i-- {
		if messages[i].Type == types.MessageTypeAssistant {
			// 找到一个 assistant，它标志新一轮的开始
			if lastAssistantIdx != -1 {
				// 将 lastAssistantIdx 到 i 之间的所有消息标记为 currentRound
				for j := i + 1; j <= lastAssistantIdx; j++ {
					rounds[j] = currentRound
				}
				currentRound++
			}
			lastAssistantIdx = i
		}
	}

	// 处理最前面的消息（从第一条到最后一个 assistant）
	if lastAssistantIdx != -1 {
		for j := 0; j <= lastAssistantIdx; j++ {
			rounds[j] = currentRound
		}
	}

	return rounds
}

// ToolResultStats 统计 messages 中 tool_result 的数量和类型分布。
func ToolResultStats(messages []types.Message) (total, success, errorCount int) {
	for _, m := range messages {
		if m.Type != types.MessageTypeUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type != types.ContentBlockToolResult {
				continue
			}
			total++
			if b.IsError {
				errorCount++
			} else {
				success++
			}
		}
	}
	return
}

// IsSuccessConfirmation 判断 tool_result 内容是否为典型的成功确认消息。
// 用于启发式识别可安全降级的低价值 tool_result。
func IsSuccessConfirmation(content string) bool {
	lower := strings.ToLower(content)
	patterns := []string{
		"successfully wrote",
		"successfully edited",
		"successfully created",
		"successfully deleted",
		"exit code: 0",
		"file written",
		"operation completed",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
