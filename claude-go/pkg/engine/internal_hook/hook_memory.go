// hook_memory.go — 记忆系统相关内置 Hook。
//
// 包含:
//   - MemoryInjectHook   L1/L2 记忆注入 system prompt
//   - PromptCacheHook    Prompt cache 构建与命中率追踪
package internal_hook

import (
	"github.com/anthropic/claude-go/pkg/memory"
)

// ============================================================================
// MemoryInjectHook — L1/L2 记忆注入
// ============================================================================

// MemoryInjectHook 在 PhasePreRequest 阶段从记忆系统中检索与当前用户请求相关的记忆，
// 并追加到 system prompt 中。仅在首轮（TurnCount==0）注入，避免多轮 tool_use 循环中
// 反复注入导致上下文膨胀。
type MemoryInjectHook struct {
	memoryStore *memory.TieredStore
	factStore   *memory.FactStore
}

// NewMemoryInjectHook 创建 MemoryInjectHook。
func NewMemoryInjectHook(memoryStore *memory.TieredStore, factStore *memory.FactStore) *MemoryInjectHook {
	return &MemoryInjectHook{memoryStore: memoryStore, factStore: factStore}
}

func (h *MemoryInjectHook) Name() string                { return "memory_inject" }
func (h *MemoryInjectHook) Priority() int               { return 50 }
func (h *MemoryInjectHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 检索 L1/L2 记忆并追加到 system prompt。
func (h *MemoryInjectHook) Execute(ctx *HookContext) (*HookResult, error) {
	// 仅在首轮注入（避免多轮 tool_use 循环中反复注入膨胀上下文）
	if ctx.TurnCount != 0 || len(ctx.Messages) == 0 {
		return nil, nil
	}

	lastUserText := ""
	for i := len(ctx.Messages) - 1; i >= 0; i-- {
		if ctx.Messages[i].Type == "user" {
			for _, b := range ctx.Messages[i].Content {
				if b.Text != "" {
					lastUserText = b.Text
					break
				}
			}
			break
		}
	}
	if lastUserText == "" {
		return nil, nil
	}

	var memPrompt string

	// L1: TieredStore 情景记忆
	if h.memoryStore != nil && h.memoryStore.Count() > 0 {
		relevant := h.memoryStore.Retrieve(lastUserText, 7)
		if len(relevant) > 0 {
			memPrompt += memory.FormatForPrompt(relevant)
		}
	}

	// L2: FactStore 结构化记忆
	if h.factStore != nil && h.factStore.Count() > 0 {
		facts := h.factStore.Retrieve(lastUserText, 5)
		if len(facts) > 0 {
			memPrompt += memory.FormatFactsForPrompt(facts)
		}
	}

	if memPrompt == "" {
		return nil, nil
	}

	systemPrompt := append(ctx.SystemPrompt, memPrompt)
	return &HookResult{SystemPrompt: systemPrompt}, nil
}

// ============================================================================
// PromptCacheHook — Prompt Cache 追踪
// ============================================================================

// PromptCacheHook 在 PhasePreRequest 阶段拆分 static/dynamic system prompt，
// 构建 prompt cache 并记录 cache 命中率到 Metrics。
type PromptCacheHook struct {
	cache  *PromptCacheBuilder
	metrics *EngineMetrics // 引擎指标
}

// NewPromptCacheHook 创建 PromptCacheHook。
func NewPromptCacheHook(cache *PromptCacheBuilder, metrics *EngineMetrics) *PromptCacheHook {
	return &PromptCacheHook{cache: cache, metrics: metrics}
}

func (h *PromptCacheHook) Name() string                { return "prompt_cache" }
func (h *PromptCacheHook) Priority() int               { return 55 }
func (h *PromptCacheHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 拆分 system prompt，构建 cache 并记录命中率。
func (h *PromptCacheHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.cache == nil || len(ctx.SystemPrompt) == 0 {
		return nil, nil
	}

	static, dynamic := SplitStaticDynamic(ctx.SystemPrompt)
	_, hit := h.cache.Build(static, dynamic)
	if h.metrics != nil {
		h.metrics.RecordCache(hit)
	}

	return nil, nil
}
