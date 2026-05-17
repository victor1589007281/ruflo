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

// DisabledToolHook 在 PhasePreToolUse 阶段将 Config.DisabledTools 中匹配的工具
// 替换为错误结果，避免模型调用不应使用的工具。
// 若全部被禁用，则注入 continue 消息让模型重新选择。
type DisabledToolHook struct {
	disabled map[string]bool
}

// NewDisabledToolHook 创建 DisabledToolHook。
// 若 disabled 为空则返回 nil，避免注册无意义的 hook。
func NewDisabledToolHook(disabled map[string]bool) *DisabledToolHook {
	if len(disabled) == 0 {
		return nil
	}
	return &DisabledToolHook{disabled: disabled}
}

func (h *DisabledToolHook) Name() string                { return "disabled_tool" }
func (h *DisabledToolHook) Priority() int               { return 150 }
func (h *DisabledToolHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreToolUse}
}

// Execute 遍历 tool_use 块，拦截禁用工具并生成错误结果。
func (h *DisabledToolHook) Execute(ctx *HookContext) (*HookResult, error) {
	if len(h.disabled) == 0 || len(ctx.ToolUseBlocks) == 0 {
		return nil, nil
	}

	var allowed []types.ContentBlock
	var appendMsgs []types.Message

	for _, block := range ctx.ToolUseBlocks {
		if h.disabled[block.Name] {
			errResult := types.Message{
				Type: types.MessageTypeUser,
				UUID: GenerateUUID(),
				Content: []types.ContentBlock{{
					Type:      types.ContentBlockToolResult,
					ToolUseID: block.ID,
					Content:   fmt.Sprintf("工具 %s 在当前会话中不可用。团队操作请通过 /team 命令或意图识别完成。", block.Name),
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
