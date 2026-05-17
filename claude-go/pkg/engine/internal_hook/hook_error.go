// hook_error.go — 错误处理相关内置 Hook。
//
// 包含:
//   - ErrorClassifierHook   G4 错误族分类与隔离预算
//   - CircuitBreakerHook    原始断路器（ErrorClassifier 未启用时兜底）
package internal_hook

import (
	"fmt"
	"time"

	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// ErrorClassifierHook — 错误族分类与隔离
// ============================================================================

// ErrorClassifierHook 在 PhaseOnError 阶段按 HTTP 错误族（4xx/5xx/429/PTL）分类错误，
// 并根据族级别预算决定是终止（abort）还是继续重试（非致命）。
// 非致命时推送 withheld error 消息 + backoff + 可选 compact。
type ErrorClassifierHook struct {
	classifier *ErrorClassifier
	compactor  *compact.Compactor
	metrics    *EngineMetrics // 引擎指标
}

// NewErrorClassifierHook 创建 ErrorClassifierHook。
func NewErrorClassifierHook(classifier *ErrorClassifier, compactor *compact.Compactor, metrics *EngineMetrics) *ErrorClassifierHook {
	return &ErrorClassifierHook{classifier: classifier, compactor: compactor, metrics: metrics}
}

func (h *ErrorClassifierHook) Name() string                { return "error_classifier" }
func (h *ErrorClassifierHook) Priority() int               { return 110 }
func (h *ErrorClassifierHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhaseOnError}
}

// Execute 分类错误，按族预算决定终止或重试策略。
func (h *ErrorClassifierHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.classifier == nil || ctx.Error == nil {
		return nil, nil
	}

	family, abort, backoff, useCompact := h.classifier.Observe(ctx.Error)
	if h.metrics != nil {
		h.metrics.RecordError(int(family))
	}

	// abort: 族预算耗尽，终止并返回
	if abort {
		errMsg := types.Message{
			Type: types.MessageTypeAssistant,
			UUID: GenerateUUID(),
			Content: []types.ContentBlock{{
				Type: types.ContentBlockText,
				Text: fmt.Sprintf("%s 族 API 错误预算耗尽: %v", family, ctx.Error),
			}},
			IsApiErrorMessage: true,
			CreatedAt:         time.Now(),
		}

		var events []types.StreamEvent
		events = append(events, types.StreamEvent{Kind: types.StreamEventError, Error: ctx.Error})

		return &HookResult{
			AppendMsgs:     []types.Message{errMsg},
			StreamEvents:   events,
			ReturnTerminal: &types.Terminal{Reason: "error_family_exhausted_" + family.String(), Error: ctx.Error},
		}, nil
	}

	// 非致命: 推送 withheld error 消息 + 可能的 compact + backoff
	errMsg := types.Message{
		Type: types.MessageTypeAssistant,
		UUID: GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: fmt.Sprintf("API %s (retry in %.1fs): %v", family, backoff.Seconds(), ctx.Error),
		}},
		IsApiErrorMessage: true,
		CreatedAt:         time.Now(),
	}

	result := &HookResult{
		AppendMsgs:     []types.Message{errMsg},
		Backoff:        backoff,
		InjectContinue: true,
	}

	if useCompact && h.compactor != nil {
		compacted, cerr := h.compactor.AutoCompact(ctx.Ctx, ctx.Messages, ctx.Model)
		if cerr == nil && compacted != nil {
			result.Messages = compacted
		}
	}

	return result, nil
}

// ============================================================================
// CircuitBreakerHook — 断路器兜底
// ============================================================================

// CircuitBreakerHook 在 PhaseOnError 阶段作为兜底保护。
// 当 ErrorClassifier 未启用时，若连续错误次数达到阈值，触发熔断终止，
// 防止在极端故障场景下无限重试消耗资源。
type CircuitBreakerHook struct {
	classifier *ErrorClassifier // 错误分类器（nil 表示未启用）
}

// NewCircuitBreakerHook 创建 CircuitBreakerHook。
func NewCircuitBreakerHook(classifier *ErrorClassifier, metrics *EngineMetrics) *CircuitBreakerHook {
	return &CircuitBreakerHook{classifier: classifier}
}

func (h *CircuitBreakerHook) Name() string                { return "circuit_breaker" }
func (h *CircuitBreakerHook) Priority() int               { return 120 }
func (h *CircuitBreakerHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhaseOnError}
}

// Execute 仅在 ErrorClassifier 未启用时检查连续错误次数，达到阈值则熔断。
func (h *CircuitBreakerHook) Execute(ctx *HookContext) (*HookResult, error) {
	// 仅在 ErrorClassifier 未启用时作为兜底
	if h.classifier != nil {
		return nil, nil
	}

	consecutiveErrors := ctx.ConsecutiveErrors
	if consecutiveErrors < MaxConsecutiveErrors {
		return nil, nil
	}

	errMsg := types.Message{
		Type: types.MessageTypeAssistant,
		UUID: GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: fmt.Sprintf("断路器触发: 连续 %d 次 API 错误, 最后错误: %v", consecutiveErrors, ctx.Error),
		}},
		IsApiErrorMessage: true,
		CreatedAt:         time.Now(),
	}

	var events []types.StreamEvent
	events = append(events, types.StreamEvent{Kind: types.StreamEventError, Error: ctx.Error})

	return &HookResult{
		AppendMsgs:     []types.Message{errMsg},
		StreamEvents:   events,
		ReturnTerminal: &types.Terminal{Reason: "circuit_breaker", Error: ctx.Error},
	}, nil
}
