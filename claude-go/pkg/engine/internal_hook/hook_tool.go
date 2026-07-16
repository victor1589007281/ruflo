// hook_tool.go — 工具调用相关内置 Hook。
//
// 包含:
//   - JSONRepairHook          工具输入 JSON 修复
//   - LoopDetectorInputHook   死循环检测（输入级）：同工具同参数拦截
//   - LoopDetectorResultHook  死循环检测（产出级）：编辑震荡和编译错误循环检测
//   - DisabledToolHook        禁用工具拦截
//   - StopSignalHook          软停止信号检测
package internal_hook

import (
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// JSONRepairHook — JSON 输入修复
// ============================================================================

// JSONRepairHook 在 PhasePreToolUse 阶段对每个 tool_use.Input 尝试 JSON 修复。
// 修复成功时更新 block 的 Input 并记录指标；修复失败也记录指标以便后续分析。
type JSONRepairHook struct {
	repair *JSONRepair
	metrics *EngineMetrics // 引擎指标
}

// NewJSONRepairHook 创建 JSONRepairHook。
func NewJSONRepairHook(repair *JSONRepair, metrics *EngineMetrics) *JSONRepairHook {
	return &JSONRepairHook{repair: repair, metrics: metrics}
}

func (h *JSONRepairHook) Name() string                { return "json_repair" }
func (h *JSONRepairHook) Priority() int               { return 70 }
func (h *JSONRepairHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreToolUse}
}

// Execute 遍历所有 tool_use 块，尝试 JSON 修复并记录指标。
func (h *JSONRepairHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.repair == nil || len(ctx.ToolUseBlocks) == 0 {
		return nil, nil
	}

	blocks := make([]types.ContentBlock, len(ctx.ToolUseBlocks))
	copy(blocks, ctx.ToolUseBlocks)
	changed := false

	for i, blk := range blocks {
		repaired, res := h.repair.Try([]byte(blk.Input))
		if res.OK && res.Changed {
			blocks[i].Input = repaired
			changed = true
			if h.metrics != nil {
				h.metrics.JSONRepairsApplied.Add(1)
			}
		} else if !res.OK {
			if h.metrics != nil {
				h.metrics.JSONRepairsFailed.Add(1)
			}
		}
	}

	if changed {
		logging.For("engine").Debug("JSONRepair applied", "count", len(blocks))
	}
	return &HookResult{ToolUseBlocks: blocks}, nil
}

// ============================================================================
// LoopDetectorInputHook — 死循环检测（输入级）
// ============================================================================

// LoopDetectorInputHook 在 PhasePreToolUse 阶段检测同工具同参数的死循环。
// 若发现模型反复调用相同工具且输入不变，则拦截该工具调用并注入错误结果，
// 避免无限循环消耗 Token 和 API 预算。
type LoopDetectorInputHook struct {
	detector *LoopDetector
	metrics  *EngineMetrics // 引擎指标
}

// NewLoopDetectorInputHook 创建 LoopDetectorInputHook。
func NewLoopDetectorInputHook(detector *LoopDetector, metrics *EngineMetrics) *LoopDetectorInputHook {
	return &LoopDetectorInputHook{detector: detector, metrics: metrics}
}

func (h *LoopDetectorInputHook) Name() string                { return "loop_detector_input" }
func (h *LoopDetectorInputHook) Priority() int               { return 80 }
func (h *LoopDetectorInputHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreToolUse}
}

// Execute 遍历 tool_use 块，对命中死循环的块注入错误结果替代实际执行。
func (h *LoopDetectorInputHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.detector == nil || len(ctx.ToolUseBlocks) == 0 {
		return nil, nil
	}

	var allowed []types.ContentBlock
	var appendMsgs []types.Message
	var looped bool

	for _, blk := range ctx.ToolUseBlocks {
		if isLooped, suggestion := h.detector.Observe(blk.Name, []byte(blk.Input)); isLooped {
			looped = true
			noteMsg := types.Message{
				Type: types.MessageTypeUser,
				UUID: GenerateUUID(),
				Content: []types.ContentBlock{{
					Type:      types.ContentBlockToolResult,
					ToolUseID: blk.ID,
					Content:   suggestion,
					IsError:   true,
				}},
				CreatedAt: time.Now(),
			}
			appendMsgs = append(appendMsgs, noteMsg)
			if h.metrics != nil {
				h.metrics.ToolLoopsDetected.Add(1)
				h.metrics.ToolLoopsSuppressed.Add(1)
			}
		} else {
			allowed = append(allowed, blk)
		}
	}

	if !looped {
		return nil, nil
	}

	logging.For("engine").Warn("LoopDetector input triggered", "suppressed", len(ctx.ToolUseBlocks)-len(allowed))

	result := &HookResult{
		ToolUseBlocks: allowed,
		AppendMsgs:    appendMsgs,
	}
	if looped && len(allowed) == 0 {
		// 全部被拦截时，跳过后续同 phase hooks 并注入 continue 进入下一轮
		result.SkipRemaining = true
		result.InjectContinue = true
	}
	return result, nil
}

// ============================================================================
// LoopDetectorResultHook — 死循环检测（产出级）
// ============================================================================

// LoopDetectorResultHook 在 PhasePostToolUse 阶段观察 tool 结果，
// 检测编辑震荡（反复修改同一文件）和编译错误循环（同一错误反复出现）。
// 命中时注入 hint meta 消息，提示模型反思。
type LoopDetectorResultHook struct {
	detector *LoopDetector
	metrics  *EngineMetrics // 引擎指标
}

// NewLoopDetectorResultHook 创建 LoopDetectorResultHook。
func NewLoopDetectorResultHook(detector *LoopDetector, metrics *EngineMetrics) *LoopDetectorResultHook {
	return &LoopDetectorResultHook{detector: detector, metrics: metrics}
}

func (h *LoopDetectorResultHook) Name() string                { return "loop_detector_result" }
func (h *LoopDetectorResultHook) Priority() int               { return 90 }
func (h *LoopDetectorResultHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostToolUse}
}

// Execute 检查 tool 结果，发现循环迹象时注入提示消息。
func (h *LoopDetectorResultHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.detector == nil || len(ctx.ToolUseBlocks) == 0 || len(ctx.ToolResults) == 0 {
		return nil, nil
	}

	for i, blk := range ctx.ToolUseBlocks {
		var resultSummary string
		var isError bool
		if i < len(ctx.ToolResults) {
			for _, cb := range ctx.ToolResults[i].Content {
				if cb.Type == types.ContentBlockToolResult {
					resultSummary = cb.Content
					isError = cb.IsError
					break
				}
			}
		}
		if stuck, suggestion := h.detector.ObserveResult(blk.Name, []byte(blk.Input), resultSummary, isError); stuck {
			hintMsg := types.Message{
				Type: types.MessageTypeUser,
				UUID: GenerateUUID(),
				Content: []types.ContentBlock{{
					Type: types.ContentBlockText,
					Text: suggestion,
				}},
				IsMeta:    true,
				CreatedAt: time.Now(),
			}
			if h.metrics != nil {
				h.metrics.ProgressLoopsDetected.Add(1)
			}
			return &HookResult{
				AppendMsgs:    []types.Message{hintMsg},
				SkipRemaining: true,
			}, nil
		}
	}
	return nil, nil
}

// ============================================================================
// DisabledToolHook — 禁用工具拦截
// ============================================================================

// ToolGateHook 在 PhasePreToolUse 阶段拦截不允许的工具调用，替换为错误结果。
// 拦截条件: 命中 disabled 黑名单，或 (allowed 白名单已设置且工具不在其中)。
// 白名单语义与 Config.AllowedTools 一致: nil=未设置, 非 nil 空集=全部拒绝。
// 这是即便模型凭空产出未暴露工具调用时的第二道防线 (工具表过滤为第一道)。
// 若全部被拦截，则注入 continue 消息让模型重新选择。
type ToolGateHook struct {
	disabled map[string]bool
	allowed  map[string]bool
	hasAllow bool // allowed 是否"已设置"(区分 nil 与空集, 空集 fail-closed)
}

// NewToolGateHook 创建 ToolGateHook。disabled 为空且 allowed 未设置(nil)时返回 nil。
func NewToolGateHook(disabled, allowed map[string]bool) *ToolGateHook {
	if len(disabled) == 0 && allowed == nil {
		return nil
	}
	return &ToolGateHook{disabled: disabled, allowed: allowed, hasAllow: allowed != nil}
}

func (h *ToolGateHook) blocked(name string) bool {
	if h.disabled != nil && h.disabled[name] {
		return true
	}
	if h.hasAllow && !h.allowed[name] {
		return true
	}
	return false
}

func (h *ToolGateHook) Name() string  { return "tool_gate" }
func (h *ToolGateHook) Priority() int { return 150 }
func (h *ToolGateHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreToolUse}
}

// Execute 遍历 tool_use 块，拦截不允许的工具并生成错误结果。
func (h *ToolGateHook) Execute(ctx *HookContext) (*HookResult, error) {
	if (len(h.disabled) == 0 && !h.hasAllow) || len(ctx.ToolUseBlocks) == 0 {
		return nil, nil
	}

	var allowed []types.ContentBlock
	var appendMsgs []types.Message

	for _, block := range ctx.ToolUseBlocks {
		if h.blocked(block.Name) {
			errResult := types.Message{
				Type: types.MessageTypeUser,
				UUID: GenerateUUID(),
				Content: []types.ContentBlock{{
					Type:      types.ContentBlockToolResult,
					ToolUseID: block.ID,
					Content:   fmt.Sprintf("工具 %s 不在本会话允许的工具清单内，已被拒绝。请只使用当前可见的工具。", block.Name),
					IsError:   true,
				}},
				CreatedAt: time.Now(),
			}
			appendMsgs = append(appendMsgs, errResult)
		} else {
			allowed = append(allowed, block)
		}
	}

	if len(allowed) == len(ctx.ToolUseBlocks) {
		return nil, nil
	}

	logging.For("engine").Warn("Disabled tools intercepted", "before", len(ctx.ToolUseBlocks), "after", len(allowed))

	result := &HookResult{
		ToolUseBlocks: allowed,
		AppendMsgs:    appendMsgs,
	}
	if len(allowed) == 0 {
		result.InjectContinue = true
	}
	return result, nil
}

// ============================================================================
// StopSignalHook — 软停止信号检测
// ============================================================================

// StopSignalHook 在 PhasePostToolUse 阶段观察 tool 结果，
// 当检测到模型可能已经完成目标但仍继续调用工具时，
// 注入 soft stop hint（IsMeta 消息）提示模型自然结束对话。
type StopSignalHook struct {
	detector *StopSignalDetector
	metrics  *EngineMetrics // 引擎指标
}

// NewStopSignalHook 创建 StopSignalHook。
func NewStopSignalHook(detector *StopSignalDetector, metrics *EngineMetrics) *StopSignalHook {
	return &StopSignalHook{detector: detector, metrics: metrics}
}

func (h *StopSignalHook) Name() string                { return "stop_signal" }
func (h *StopSignalHook) Priority() int               { return 100 }
func (h *StopSignalHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostToolUse}
}

// Execute 检查 tool 结果，必要时注入 soft stop hint。
func (h *StopSignalHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.detector == nil || len(ctx.ToolUseBlocks) == 0 || len(ctx.ToolResults) == 0 {
		return nil, nil
	}

	for i, blk := range ctx.ToolUseBlocks {
		var resultSummary string
		if i < len(ctx.ToolResults) {
			for _, cb := range ctx.ToolResults[i].Content {
				if cb.Type == types.ContentBlockToolResult {
					resultSummary = cb.Content
					break
				}
			}
		}
		if suggest, reason := h.detector.Observe(blk.Name, string(blk.Input), resultSummary); suggest {
			hintMsg := types.Message{
				Type: types.MessageTypeUser,
				UUID: GenerateUUID(),
				Content: []types.ContentBlock{{
					Type: types.ContentBlockText,
					Text: BuildHintMessage(reason),
				}},
				IsMeta:    true,
				CreatedAt: time.Now(),
			}
			if h.metrics != nil {
				h.metrics.StopSuggestionsEmitted.Add(1)
			}
			return &HookResult{
				AppendMsgs:    []types.Message{hintMsg},
				SkipRemaining: true,
			}, nil
		}
	}
	return nil, nil
}

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

	msgRound := computeMessageRounds(messages)

	var changed bool
	var levelStats [4]int64
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

func (h *ToolResultLevelHook) degradeLevel1(b types.ContentBlock) *types.ContentBlock {
	if h.config.PreserveErrors && b.IsError {
		return &b
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

func (h *ToolResultLevelHook) degradeLevel2(b types.ContentBlock) *types.ContentBlock {
	if h.config.PreserveErrors && b.IsError {
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
	return nil
}

// ============================================================================
// 辅助函数
// ============================================================================

func computeMessageRounds(messages []types.Message) []int {
	n := len(messages)
	rounds := make([]int, n)
	currentRound := 0
	lastAssistantIdx := -1

	for i := n - 1; i >= 0; i-- {
		if messages[i].Type == types.MessageTypeAssistant {
			if lastAssistantIdx != -1 {
				for j := i + 1; j <= lastAssistantIdx; j++ {
					rounds[j] = currentRound
				}
				currentRound++
			}
			lastAssistantIdx = i
		}
	}

	if lastAssistantIdx != -1 {
		for j := 0; j <= lastAssistantIdx; j++ {
			rounds[j] = currentRound
		}
	}

	return rounds
}

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
