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
	"github.com/anthropic/claude-go/pkg/textutil"
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

// Name 返回 hook 名 "json_repair"。
func (h *JSONRepairHook) Name() string                { return "json_repair" }
// Priority 返回 70 —— 在输入级死循环检测(80)之前, 先修好 JSON 再判重。
func (h *JSONRepairHook) Priority() int               { return 70 }
// Phases 声明本 hook 只挂在 PhasePreToolUse。
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

// Name 返回 hook 名 "loop_detector_input"。
func (h *LoopDetectorInputHook) Name() string                { return "loop_detector_input" }
// Priority 返回 80。
func (h *LoopDetectorInputHook) Priority() int               { return 80 }
// Phases 声明本 hook 只挂在 PhasePreToolUse。
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

// Name 返回 hook 名 "loop_detector_result"。
func (h *LoopDetectorResultHook) Name() string                { return "loop_detector_result" }
// Priority 返回 90。
func (h *LoopDetectorResultHook) Priority() int               { return 90 }
// Phases 声明本 hook 只挂在 PhasePostToolUse。
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

// blocked 判断工具是否被拦截: 命中黑名单, 或白名单已设置且不含它 (黑名单先判, 同时命中时以黑名单为准)。
func (h *ToolGateHook) blocked(name string) bool {
	// 黑名单先判: 同时命中黑白名单时以黑名单为准。
	if h.disabled != nil && h.disabled[name] {
		return true
	}
	if h.hasAllow && !h.allowed[name] {
		return true
	}
	return false
}

// Name 返回 hook 名 "tool_gate"。
func (h *ToolGateHook) Name() string  { return "tool_gate" }
// Priority 返回 150 —— 排在 JSON 修复(70)与输入级死循环检测(80)之后, 拦的是最终要执行的那批工具。
func (h *ToolGateHook) Priority() int { return 150 }
// Phases 声明本 hook 只挂在 PhasePreToolUse。
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

// Name 返回 hook 名 "stop_signal"。
func (h *StopSignalHook) Name() string                { return "stop_signal" }
// Priority 返回 100。
func (h *StopSignalHook) Priority() int               { return 100 }
// Phases 声明本 hook 只挂在 PhasePostToolUse。
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
		Level3SuccessMarker: "[历史操作结果: %s]",
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

// Name 返回 hook 名 "toolresult_level"。
func (h *ToolResultLevelHook) Name() string                { return "toolresult_level" }
// Priority 返回 38 —— 在 MicroCompact(20)/BudgetDegrade(30) 之后、MessageFilter(40) 之前。
func (h *ToolResultLevelHook) Priority() int               { return 38 }
// Phases 声明本 hook 只挂在 PhasePreRequest。
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
	var blockedByWatermark int
	for i := range messages {
		// ★ 只能动 [SentUpTo, len) —— 即本轮新增、尚未进入任何已发出请求的消息。
		//
		// 纪律见 hook.go 的 SentUpTo 字段注释, 以及 engine.go:2049 那条停用注释:
		// 「历史长度应当由 AutoCompact 的折叠边界一次性收敛, 而不是每轮动态裁剪」。
		// MicroCompactHook 一直守着它; 本 hook 之前**没读** ctx.SentUpTo, 于是每轮
		// 都在最老的历史上动刀。
		//
		// 为什么"删除"比"改写"更致命: [0, SentUpTo) 已经躺在缓存前缀里, 删掉其中
		// 一条消息会让它**后面所有消息整体前移**, 首个不同字节落在删除点, 其后整段
		// 前缀一起作废。实测 (if-question-expand 2fd81b6e, t7→t8) 级联为:
		//   删最老 tool_result → 该 user 消息 Content 变空被 messagesToAPI 整条丢弃
		//   → 与之配对的 tool_use 成孤儿被 sanitizeToolPairing 摘掉
		//   → 前后两条 assistant 相邻被合并成一条。
		// 首个不同字节 @19307, 当轮 19438B 作废; 且位移此后每轮继续往下带
		// (t8→t9 再 15965B、t9→t10 13464B、t10→t11 9307B, 合计约 58KB)。
		if i < ctx.SentUpTo {
			if messages[i].Type == types.MessageTypeUser &&
				h.determineLevel(msgRound[i]) != Level0 {
				blockedByWatermark++
			}
			continue
		}
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
		// 比对**内容**而不是块数。
		//
		// 原来这里是 `len(newBlocks) != len(messages[i].Content)`, 而 L1(截断)与
		// L2(换成 [历史操作结果: 成功]) 都是**原地改写**、块数不变 —— 于是这两档
		// 算完就被丢掉, 成了不留任何痕迹的死代码 (产出里搜不到 truncated by level1 /
		// [历史操作结果, 但那是"没生效"不是"没跑过", grep 无果不能当判据)。
		// 只有 L3 的 `return nil` 会改块数, 所以整个 hook 实际只剩"删"这一档在动。
		// MicroCompactHook 判"改没改"用的就是逐块比内容, 这里对齐它。
		if !contentEqual(newBlocks, messages[i].Content) {
			messages[i].Content = newBlocks
			changed = true
		}
	}

	if changed || blockedByWatermark > 0 {
		logging.For("engine").Debug("ToolResultLevelHook applied",
			"l1", levelStats[Level1],
			"l2", levelStats[Level2],
			"l3", levelStats[Level3],
			"blocked_by_watermark", blockedByWatermark,
		)
	}

	if !changed {
		return nil, nil
	}
	return &HookResult{Messages: messages}, nil
}

// determineLevel 按轮次年龄把消息分到 Level0~3 四档。
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

// degradeBlock 按等级分派到对应的降级实现; Level0 原样返回。
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

// degradeLevel1 截断到 Level1MaxChars; IsError 且 PreserveErrors 为真时原样保留。
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
		Content:   textutil.Truncate(b.Content, h.config.Level1MaxChars, "\n... (truncated by level1)"),
		IsError:   b.IsError,
	}
}

// degradeLevel2 把成功结果换成 "[历史操作结果: 成功]" 一行, 错误结果换成 300 字符摘要。
func (h *ToolResultLevelHook) degradeLevel2(b types.ContentBlock) *types.ContentBlock {
	if h.config.PreserveErrors && b.IsError {
		summary := textutil.Truncate(b.Content, 300, "...")
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

// degradeLevel3 最老一档: 成功结果换成一行占位, 失败结果换摘要。
//
// ★ 这里曾经对成功类 `return nil` —— 静默把整个 tool_result 删掉。那是级联崩溃的
// 源头, 不是"降级": 块数 1→0 导致该 user 消息 Content 变空, messagesToAPI 的
// `len(kept)==0 → continue` 把**整条消息**丢掉; 与之配对的 tool_use 随即成孤儿,
// 被 sanitizeToolPairing 摘掉; 于是前后两条 assistant 相邻, 又被合并循环拼成一条。
// 实测 (if-question-expand 2fd81b6e t7→t8) 三条消息塌成一条, 首个不同字节 @19307,
// 后面 19438B 全部作废, 且位移此后每轮继续往下带。
//
// 留一行占位就没有这个问题: 块还在, 配对不破, 消息不空, 结构零位移 —— 只是内容
// 变短。省下的 token 几乎一样 (几千字符 → 一行), 却把"删除"降级成"改写", 而改写
// 只影响它自己那一段, 不会让后面的消息整体前移。
//
// 配置里本来就有 Level3SuccessMarker 字段却一直没被使用 —— 占位才是它原本的意图。
func (h *ToolResultLevelHook) degradeLevel3(b types.ContentBlock) *types.ContentBlock {
	if b.IsError {
		summary := textutil.Truncate(b.Content, h.config.Level3ErrorSummary, "...")
		return &types.ContentBlock{
			Type:      b.Type,
			ToolUseID: b.ToolUseID,
			Content:   fmt.Sprintf("[历史错误摘要] %s", summary),
			IsError:   true,
		}
	}
	return &types.ContentBlock{
		Type:      b.Type,
		ToolUseID: b.ToolUseID,
		Content:   fmt.Sprintf(h.config.Level3SuccessMarker, "成功"),
		IsError:   false,
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

// contentEqual 逐块比较两组 content 是否相同 (只比降级会碰的字段)。
//
// 存在的理由: 降级里有"原地改写"(截断 / 换摘要)与"删除"(返回 nil)两种形态,
// 前者不改变块数。任何用 len() 判断"改没改"的闸门都会把原地改写静默丢掉。
func contentEqual(a, b []types.ContentBlock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type ||
			a[i].ToolUseID != b[i].ToolUseID ||
			a[i].Content != b[i].Content ||
			a[i].IsError != b[i].IsError {
			return false
		}
	}
	return true
}

// computeMessageRounds 计算每条消息的"轮次年龄": 两条 assistant 之间的消息算一轮, round 0 为最新。
//
// 算法解读：
//
//	原理：从后往前扫, 以 assistant 消息为界把消息切成轮 —— 两条 assistant 之间的消息属于同一轮,
//	      越靠前的轮年龄越大。年龄只用于分级降级, 不参与任何语义判断。
//	关键不变量：年龄必须**单调不减**地往回增 (下标越小轮次越大)。降级策略假设"老的可以更狠",
//	      年龄一旦乱序, 就会出现新消息被重降级、老消息被完整保留。
//	代价与边界：两趟 O(n) 扫描 (倒序定界 + 补最老的一段); 没有 assistant 的消息 (纯
//	      system/user 前缀) 全部归为同一轮。
func computeMessageRounds(messages []types.Message) []int {
	n := len(messages)
	rounds := make([]int, n)
	currentRound := 0
	// 倒序扫: 遇到 assistant 说明"上一轮的边界到了", 把 (它, 下一条 assistant] 之间的消息标成当前轮。
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

// ToolResultStats 统计消息里 tool_result 块的总数、成功数与失败数。
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

// IsSuccessConfirmation 用一组英文成功措辞判断工具结果是否表示成功 (子串匹配)。
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
