// hook_agentdb_semantic.go — 层 2 语义增强面注入 (规划 14.1.4.3)。
//
// AgentDBSemanticInjectHook 是 MemoryInject 的 agentDB 后端: 在 PhasePreRequest
// 首轮 (TurnCount==0) 经 agentDB 语义引擎做跨源检索 (memory + file), RRF 组装出
// 上下文片段并追加到 system prompt, 让模型在回答前感知相关记忆与文件。
//
// 与 MemoryInjectHook (进程内 TieredStore/FactStore) 互补: 本 hook 检索的是
// agentDB 持久化的语义层 (可被多 worker 共享 / 跨会话复用)。agentDB 未部署或
// 检索失败时静默跳过, 不影响主流程。
package internal_hook

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
)

// AgentDBSemanticInjectHook 将 agentDB 语义检索结果注入 system prompt。
type AgentDBSemanticInjectHook struct {
	client  *agentdbsemantic.Client
	topK    int // 每源检索条数
	budget  int // 注入上下文 token 预算
	reserve int // 为任务正文预留 token
}

// NewAgentDBSemanticInjectHook 创建注入 hook。client 为 nil 时不注册 (守卫在引擎层)。
func NewAgentDBSemanticInjectHook(client *agentdbsemantic.Client, topK, budget, reserve int) *AgentDBSemanticInjectHook {
	return &AgentDBSemanticInjectHook{
		client:  client,
		topK:    topK,
		budget:  budget,
		reserve: reserve,
	}
}

func (h *AgentDBSemanticInjectHook) Name() string                { return "agentdb_semantic_inject" }
func (h *AgentDBSemanticInjectHook) Priority() int               { return 50 }
func (h *AgentDBSemanticInjectHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 检索并注入 agentDB 语义上下文。失败静默跳过 (agentDB 非必备依赖)。
func (h *AgentDBSemanticInjectHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.client == nil || ctx.TurnCount != 0 || len(ctx.Messages) == 0 {
		return nil, nil
	}
	lastUserText := lastNonMetaUserText(ctx.Messages)
	if lastUserText == "" {
		return nil, nil
	}

	// 跨源检索: 默认 memory + file (sources 为空)。
	res, err := h.client.RetrieveQuery(ctx.Ctx, lastUserText, nil, h.topK)
	if err != nil {
		// agentDB 不可用 → 静默跳过 (层 2 是增强面, 不阻断主流程)。
		return nil, nil
	}
	if len(res.Items) == 0 {
		return nil, nil
	}

	asm, err := h.client.AssembleContext(ctx.Ctx, lastUserText, h.budget, h.reserve, res.Items)
	if err != nil {
		return nil, nil
	}
	if asm.Body == "" {
		return nil, nil
	}

	// 与 MemoryInjectHook 相同: 追加到 system prompt。
	systemPrompt := append(ctx.SystemPrompt,
		fmt.Sprintf("<memory_context source=\"agentdb\" truncated=\"%v\" items=\"%d\">\n%s\n</memory_context>",
			asm.Truncated, asm.Items, asm.Body))
	return &HookResult{SystemPrompt: systemPrompt}, nil
}
