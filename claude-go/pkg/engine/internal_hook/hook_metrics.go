// hook_metrics.go — 可观测性与追踪层内置 Hook。
//
// 包含:
//   - MetricsCollectHook   工具调用计数 (PhasePostToolUse)
//   - TurnMetricsHook      Turn 级指标 + 轨迹记录 (PhasePostTurn)
package internal_hook

import (
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
)

// ============================================================================
// MetricsCollectHook — 工具调用计数
// ============================================================================

// MetricsCollectHook 在 PhasePostToolUse 阶段统计本轮 tool_use 块数量，
// 累加到 Metrics.ToolCallsTotal。属于纯观测型 hook，无干预能力。
type MetricsCollectHook struct {
	metrics *EngineMetrics // 引擎指标
}

// NewMetricsCollectHook 创建 MetricsCollectHook。
func NewMetricsCollectHook(metrics *EngineMetrics) *MetricsCollectHook {
	return &MetricsCollectHook{metrics: metrics}
}

func (h *MetricsCollectHook) Name() string                { return "metrics_collect" }
func (h *MetricsCollectHook) Priority() int               { return 200 }
func (h *MetricsCollectHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostToolUse}
}

// Execute 统计 tool_use 块数量并记录到 Metrics。
func (h *MetricsCollectHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.metrics == nil {
		return nil, nil
	}

	count := len(ctx.ToolUseBlocks)
	if count > 0 {
		h.metrics.ToolCallsTotal.Add(int64(count))
		logging.For("engine").Debug("MetricsCollectHook recorded tool calls", "count", count)
	}

	return nil, nil
}

// ============================================================================
// TurnMetricsHook — Turn 级指标与轨迹记录
// ============================================================================

// SessionStore 是 TurnMetricsHook 依赖的会话存储最小接口。
type SessionStore interface {
	SessionID() string
}

// TurnMetricsHook 在 PhasePostTurn 阶段记录 Turn 级指标和 Trajectory 轨迹。
// 统一在 queryLoop 所有返回路径触发，按 stopReason 分派不同指标。
type TurnMetricsHook struct {
	metrics      *EngineMetrics   // 引擎指标
	sessionStore SessionStore     // 会话存储 (可选)
	trajStore    TrajectoryStore // 轨迹存储 (可选)
}

// NewTurnMetricsHook 创建 TurnMetricsHook。
func NewTurnMetricsHook(metrics *EngineMetrics, sessionStore SessionStore, trajStore TrajectoryStore) *TurnMetricsHook {
	return &TurnMetricsHook{metrics: metrics, sessionStore: sessionStore, trajStore: trajStore}
}

func (h *TurnMetricsHook) Name() string                { return "turn_metrics" }
func (h *TurnMetricsHook) Priority() int               { return 10 }
func (h *TurnMetricsHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostTurn}
}

// Execute 记录 Turn 级指标和 Trajectory 轨迹。
func (h *TurnMetricsHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.metrics == nil && h.trajStore == nil {
		return nil, nil
	}

	// ---------- 1. Turn 级指标 ----------
	if h.metrics != nil {
		h.metrics.RecordTurnLatency(time.Since(ctx.TurnStart))

		switch ctx.StopReason {
		case "completed":
			h.metrics.TurnsTotal.Add(1)
			h.metrics.TurnsSuccess.Add(1)
		case "max_turns":
			h.metrics.TurnsTotal.Add(1)
			h.metrics.TurnsAborted.Add(1)
		case "aborted", "aborted_streaming":
			h.metrics.TurnsAborted.Add(1)
		case "error", "model_error", "precompact_terminal", "circuit_breaker":
			h.metrics.TurnsError.Add(1)
		default:
			// 未知或空 stopReason，按 aborted 处理（兼容原有 abort 路径行为）
			if ctx.TurnAborted {
				h.metrics.TurnsAborted.Add(1)
			}
		}
	}

	// ---------- 2. Trajectory 轨迹 ----------
	if h.trajStore != nil {
		verdict := InferVerdict(ctx.StopReason, ctx.TurnToolSigs, ctx.TurnAborted)
		sess := ""
		if h.sessionStore != nil {
			sess = h.sessionStore.SessionID()
		}
		plan := ExtractPlan(ctx.TurnPlanMsgs)
		t := &Trajectory{
			TurnID:     GenerateUUID(),
			SessionID:  sess,
			UserIntent: ctx.TurnUserIntent,
			Plan:       plan,
			ToolCalls:  ctx.TurnToolSigs,
			StopReason: ctx.StopReason,
			Verdict:    verdict,
			At:         time.Now(),
			LatencyMs:  time.Since(ctx.TurnStart).Milliseconds(),
		}
		h.trajStore.Append(t)

		if h.metrics != nil {
			switch verdict {
			case VerdictSuccess:
				h.metrics.TrajSuccessRecorded.Add(1)
			case VerdictFail:
				h.metrics.TrajFailRecorded.Add(1)
			}
		}

		logging.For("engine").Debug("TurnMetricsHook recorded trajectory",
			"turn_id", t.TurnID,
			"stop_reason", ctx.StopReason,
			"verdict", verdict,
			"latency_ms", t.LatencyMs,
		)
	}

	return nil, nil
}
