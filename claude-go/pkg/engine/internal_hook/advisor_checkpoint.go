// AdvisorCheckpointHook — advisor push 模式 (Phase 3, 设计文档 docs/advisor-tool-design.md 3.5)。
//
// 官方 advisor 是纯 pull 模式 (靠主模型自觉调用)；claude-go 的长自治任务
// (cron/team/dreaming) 需要 push 兜底：harness 在以下时机主动咨询 advisor，
// 并把建议以 IsMeta user 消息注入下一轮：
//   - LoopDetector 检测到工具循环 (TriggerCount 增量)
//   - 连续 N 轮无 advisor 介入 (周期性方向审视)
//
// 咨询函数由装配方注入 (通常是 builtin.AdvisorTool.Consult)，与 pull 模式
// 共享 MaxCallsPerSession 预算；预算耗尽或调用失败时静默跳过，绝不中断主循环。
package internal_hook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// AdvisorConsultFn 咨询 advisor 模型：转发完整消息历史，返回建议文本。
// 与 builtin.(*AdvisorTool).Consult 签名一致，通过函数注入避免 engine → builtin 依赖。
type AdvisorConsultFn func(ctx context.Context, messages []types.Message) (string, error)

// AdvisorCheckpointHook 在 PhasePostToolUse 阶段按条件主动咨询 advisor。
type AdvisorCheckpointHook struct {
	consult     AdvisorConsultFn
	everyNTurns int           // 周期性触发间隔 (0=关闭周期触发)
	onLoop      bool          // LoopDetector 触发时联动咨询
	loopDet     *LoopDetector // 可为 nil
	metrics     *EngineMetrics

	lastConsultTurn int // 上次咨询 (含模型主动调用 advisor 工具) 时的 TurnCount
	lastLoopCount   int // 上次观察到的 LoopDetector.TriggerCount
}

// NewAdvisorCheckpointHook 创建 checkpoint hook。
// consult 为 nil 或 (everyNTurns<=0 且 !onLoop) 时返回 nil (不注册)。
func NewAdvisorCheckpointHook(consult AdvisorConsultFn, everyNTurns int, onLoop bool, loopDet *LoopDetector, metrics *EngineMetrics) *AdvisorCheckpointHook {
	if consult == nil {
		return nil
	}
	if everyNTurns <= 0 && !onLoop {
		return nil
	}
	return &AdvisorCheckpointHook{
		consult:     consult,
		everyNTurns: everyNTurns,
		onLoop:      onLoop,
		loopDet:     loopDet,
		metrics:     metrics,
	}
}

func (h *AdvisorCheckpointHook) Name() string  { return "advisor_checkpoint" }
func (h *AdvisorCheckpointHook) Priority() int { return 95 } // LoopDetectorResult(90) 之后, StopSignal(100) 之前
func (h *AdvisorCheckpointHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostToolUse}
}

// Execute 判定触发条件并咨询 advisor。任何失败静默降级。
func (h *AdvisorCheckpointHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h == nil || h.consult == nil {
		return nil, nil
	}

	// 模型本轮已主动调用 advisor 工具 → 重置周期计时，不重复咨询
	if turnHasAdvisorCall(ctx.ToolUseBlocks) {
		h.lastConsultTurn = ctx.TurnCount
		if h.loopDet != nil {
			h.lastLoopCount = h.loopDet.TriggerCount()
		}
		return nil, nil
	}

	reason := ""
	if h.onLoop && h.loopDet != nil {
		if cur := h.loopDet.TriggerCount(); cur > h.lastLoopCount {
			h.lastLoopCount = cur
			reason = "工具循环检测触发"
		}
	}
	if reason == "" && h.everyNTurns > 0 && ctx.TurnCount-h.lastConsultTurn >= h.everyNTurns {
		reason = fmt.Sprintf("已连续 %d 轮无 advisor 介入", ctx.TurnCount-h.lastConsultTurn)
	}
	if reason == "" {
		return nil, nil
	}

	advice, err := h.consult(ctx.Ctx, ctx.Messages)
	if err != nil {
		// 预算耗尽/网络失败等: 静默跳过, 但推进计时避免每轮重试
		logging.For("engine").Debug("advisor checkpoint skipped", "reason", reason, "err", err)
		h.lastConsultTurn = ctx.TurnCount
		return nil, nil
	}
	h.lastConsultTurn = ctx.TurnCount
	if strings.TrimSpace(advice) == "" {
		return nil, nil
	}

	if h.metrics != nil {
		h.metrics.AdvisorCheckpoints.Add(1)
	}
	logging.For("engine").Info("advisor checkpoint injected", "reason", reason, "turn", ctx.TurnCount)

	hintMsg := types.Message{
		Type: types.MessageTypeUser,
		UUID: GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: fmt.Sprintf("<system-reminder>Advisor checkpoint (%s)。更强的 reviewer 模型审阅了你的完整工作轨迹后给出以下建议，请认真对待；若你有一手证据与建议冲突，说明冲突点后再继续：\n\n%s</system-reminder>", reason, advice),
		}},
		IsMeta:    true,
		CreatedAt: time.Now(),
	}
	return &HookResult{AppendMsgs: []types.Message{hintMsg}}, nil
}

// turnHasAdvisorCall 检查本轮 tool_use 中是否包含 advisor 工具调用。
func turnHasAdvisorCall(blocks []types.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == types.ContentBlockToolUse && b.Name == "advisor" {
			return true
		}
	}
	return false
}
