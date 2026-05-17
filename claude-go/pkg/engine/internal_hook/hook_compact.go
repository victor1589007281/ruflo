// hook_compact.go — 上下文压缩相关内置 Hook。
//
// 包含:
//   - MicroCompactHook    截断过大 tool_result（轻量级）
//   - AutoCompactHook     上下文自动压缩 + 关键事实蒸馏
//   - BudgetDegradeHook   Token 预算分级降级（紧急摘要）
package internal_hook

import (
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/memory"
)

// ============================================================================
// MicroCompactHook — 轻量级消息截断
// ============================================================================

// MicroCompactHook 对 tool_result 内容做字符截断，属于 PhasePreCompact 的第一步。
// 若截断后消息总长度仍超过阈值，则进行二次截断。
type MicroCompactHook struct{}

// NewMicroCompactHook 创建 MicroCompactHook。
func NewMicroCompactHook() *MicroCompactHook {
	return &MicroCompactHook{}
}

func (h *MicroCompactHook) Name() string                { return "micro_compact" }
func (h *MicroCompactHook) Priority() int               { return 20 }
func (h *MicroCompactHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreCompact}
}

// Execute 执行轻量级截断。若消息长度无变化则返回 nil，避免不必要的 HookResult 分配。
func (h *MicroCompactHook) Execute(ctx *HookContext) (*HookResult, error) {
	messages := compact.MicroCompact(ctx.Messages, MicroCompactMaxChars)
	if MessageChars(messages) > MessageCompactChars {
		messages = compact.MicroCompact(messages, MicroCompactMaxChars/2)
	}
	if len(messages) != len(ctx.Messages) {
		logging.For("engine").Debug("MicroCompact applied", "before", len(ctx.Messages), "after", len(messages))
	}
	return &HookResult{Messages: messages}, nil
}

// ============================================================================
// AutoCompactHook — 自动压缩与关键事实蒸馏
// ============================================================================

// AutoCompactHook 在 PhasePreCompact 阶段调用 Compactor 进行上下文压缩，
// 并将压缩前的关键事实提取到 L1（TieredStore）和 L2（FactStore）记忆系统。
type AutoCompactHook struct {
	compactor   *compact.Compactor
	memoryStore *memory.TieredStore
	ingestor    *memory.Ingestor
	hookRunner  *hooks.Runner // 外部 HookRunner，用于调用外部 PreCompact hooks
}

// NewAutoCompactHook 创建 AutoCompactHook。
func NewAutoCompactHook(
	compactor *compact.Compactor,
	memoryStore *memory.TieredStore,
	ingestor *memory.Ingestor,
	hookRunner *hooks.Runner,
) *AutoCompactHook {
	return &AutoCompactHook{
		compactor:   compactor,
		memoryStore: memoryStore,
		ingestor:    ingestor,
		hookRunner:  hookRunner,
	}
}

func (h *AutoCompactHook) Name() string                { return "auto_compact" }
func (h *AutoCompactHook) Priority() int               { return 10 }
func (h *AutoCompactHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreCompact}
}

// Execute 先检查外部 PreCompact Hook 是否阻止压缩，若未阻止则执行 AutoCompact
// 并将关键事实蒸馏到记忆系统。
func (h *AutoCompactHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.compactor == nil {
		return nil, nil
	}

	// 外部 PreCompact Hook 干预：若返回 block/deny，跳过本轮压缩
	if h.hookRunner != nil {
		hookOut := h.hookRunner.ExecutePreCompactHooks(ctx.Messages)
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			logging.For("engine").Warn("PreCompact blocked by hook", "reason", hookOut.Reason)
			return &HookResult{SkipRemaining: true}, nil
		}
	}

	compacted, err := h.compactor.AutoCompact(ctx.Ctx, ctx.Messages, ctx.Model)
	if err != nil || compacted == nil {
		return nil, err
	}

	// PreCompact 蒸馏: 提取关键事实到 L1 + L2
	cutoff := len(ctx.Messages) - 6
	if cutoff > 0 {
		facts := h.compactor.SmartExtractKeyFacts(ctx.Ctx, ctx.Messages[:cutoff])
		// L1: TieredStore
		if h.memoryStore != nil {
			for _, fact := range facts {
				h.memoryStore.Add(&memory.MemoryEntry{
					Content:    fact,
					Source:     "pre_compact",
					Importance: 0.7,
				})
			}
		}
		// L2: FactStore（通过 Ingestor 分类）
		if h.ingestor != nil && len(facts) > 0 {
			h.ingestor.IngestFacts(facts, "pre_compact")
		}
	}

	return &HookResult{Messages: compacted}, nil
}

// ============================================================================
// BudgetDegradeHook — Token 预算分级降级
// ============================================================================

// BudgetDegradeHook 在 PhasePreCompact 阶段检测 Token 预算状态。
// 当预算达到 Red/Critical 级别时，先触发外部 OnContextOverflow Hook，
// 若未阻止则调用 Budget.Degrade 生成紧急摘要消息。
type BudgetDegradeHook struct {
	budget *TokenBudgetManager
	hookRunner *hooks.Runner // 外部 HookRunner
	metrics    *EngineMetrics     // 引擎指标
}

// NewBudgetDegradeHook 创建 BudgetDegradeHook。
func NewBudgetDegradeHook(budget *TokenBudgetManager, hookRunner *hooks.Runner, metrics *EngineMetrics) *BudgetDegradeHook {
	return &BudgetDegradeHook{budget: budget, hookRunner: hookRunner, metrics: metrics}
}

func (h *BudgetDegradeHook) Name() string                { return "budget_degrade" }
func (h *BudgetDegradeHook) Priority() int               { return 30 }
func (h *BudgetDegradeHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreCompact}
}

// Execute 检测预算级别，触发外部干预，必要时执行降级并记录指标。
func (h *BudgetDegradeHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.budget == nil {
		return nil, nil
	}

	level := h.budget.Level(ctx.Messages)
	if level < BudgetRed {
		return nil, nil
	}

	// 外部 OnContextOverflow Hook 干预
	if h.hookRunner != nil {
		hookOut := h.hookRunner.ExecuteOnContextOverflowHooks(ctx.Messages, int(level))
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			logging.For("engine").Warn("OnContextOverflow blocked degrade by hook", "reason", hookOut.Reason)
			return &HookResult{SkipRemaining: true}, nil
		}
	}

	degraded := h.budget.Degrade(ctx.Messages, level)
	if h.metrics != nil {
		h.metrics.RecordBudgetDegrade(int(level))
	}

	logging.For("engine").Debug("BudgetDegrade applied", "level", level, "before", len(ctx.Messages), "after", len(degraded))
	return &HookResult{Messages: degraded}, nil
}
