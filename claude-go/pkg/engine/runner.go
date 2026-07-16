// runner.go - 独立的一次性 (one-shot) Agent 运行器.
//
// 背景:
//   cliAgentRunner.Execute (cmd/claude-go/main.go) 早期实现直接调 client.SendMessage,
//   绕过了 QueryEngine 的工具循环和 XML 回退解析. 当 LLM (MiniMax M2.x 等) 在文本里
//   塞 <minimax:tool_call> 或 [TOOL_CALL] 风格的工具调用时, Agent 实际什么也没做.
//
//   本文件提供一个轻量循环: 共享 QueryEngine 的工具注册表, 但消息历史完全独立 (不污染
//   QueryEngine.Messages), 既支持 Anthropic 原生 tool_use 协议也支持 XML/Bracket 回退,
//   适合 Team workflow 的每一个 stage 独立运行.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// IsolatedRunOptions 控制独立运行器行为.
type IsolatedRunOptions struct {
	// SystemPrompt 该 Agent 专属的 system prompt (会与 PromptMgr 默认 system 一起注入).
	SystemPrompt string
	// MaxTurns 最多循环多少轮, <=0 时使用 30.
	MaxTurns int
	// MaxTokens 单次请求最大输出 tokens, <=0 时使用 8192.
	MaxTokens int
	// Client 可选, 覆盖 engine 默认的 api.Client (用于按 role/plan 切换 provider).
	Client *api.Client
	// DisableTools 为 true 时:
	//   - 不向模型暴露任何工具 (apiTools=nil)
	//   - 即使模型在文本里塞了 <minimax:tool_call> 或 [TOOL_CALL] 也只清洗文本,
	//     不会真的执行工具 (extracted tool_uses 直接丢弃)
	//   - 整个循环最多跑 1 轮就返回
	// 适合 architect / researcher / planner 这类"思考型"角色, 它们的 system prompt
	// 明确禁止工具调用, 只需要产出最终设计文档/调研报告/JSON.
	DisableTools bool
}

// RunIsolated 跑一次独立的 Agent 循环, 共享 QueryEngine 的工具集合,
// 但完全独立的消息历史, 适合 team workflow 的每个 stage.
//
// 行为:
//  1. 用 SystemPrompt + userPrompt 构造起始消息.
//  2. 进入循环: 调 client.SendMessage → 解析响应 → 处理 tool_use → 把 result 追加, 再调用.
//  3. 终止条件: 模型不再返回 tool_use, 或达到 MaxTurns, 或 ctx 取消.
//  4. 返回最终 assistant 的文本内容 (合并所有 text content block).
//
// 与 QueryEngine.queryLoop 的区别:
//   - 不持久化 SessionStore / Trajectory
//   - 不应用 Phase 1 的 autoCompact / microCompact (假设 stage 输出量适中)
//   - 但仍跑 internal_hook.MergeXMLToolCalls 回退 + RunTools, 这是核心功能.
func (e *QueryEngine) RunIsolated(ctx context.Context, userPrompt string, opts IsolatedRunOptions) (string, error) {
	if e == nil {
		return "", fmt.Errorf("nil engine")
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 30
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 8192
	}
	client := opts.Client
	if client == nil {
		client = e.APIClient
	}
	if client == nil {
		return "", fmt.Errorf("no api client configured")
	}

	// 1. 构造起始消息列表 (独立, 不污染 e.Messages)
	messages := []types.Message{
		{
			Type: types.MessageTypeUser,
			UUID: internal_hook.GenerateUUID(),
			Content: []types.ContentBlock{
				{Type: types.ContentBlockText, Text: userPrompt},
			},
			CreatedAt: time.Now(),
		},
	}

	// 2. 组装 system prompts: SystemPrompt > PromptMgr default
	// DisableTools=true 时 (思考型角色), 若已提供专属 system prompt,
	// 不再追加包含大量工具定义的默认 system prompt, 避免模型被无关工具描述干扰.
	var systemPrompts []string
	if opts.SystemPrompt != "" {
		systemPrompts = append(systemPrompts, opts.SystemPrompt)
	}
	if e.PromptMgr != nil && !(opts.DisableTools && opts.SystemPrompt != "") {
		def := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)
		systemPrompts = append(systemPrompts, def...)
	}

	// 3. 准备 APITools (经 toolExposed 统一过滤: DisabledTools 黑名单 +
	// AllowedTools 白名单). DisableTools 时直接置空, 不向模型暴露任何工具描述,
	// 进一步降低 MiniMax 等模型乱产 XML 工具调用的概率.
	var apiTools []types.APITool
	if !opts.DisableTools && e.Tools != nil {
		all := e.Tools.APITools()
		for _, t := range all {
			if !e.Config.toolExposed(t.Name) {
				continue
			}
			apiTools = append(apiTools, t)
		}
	}

	// 4. 循环
	var lastAssistantText string
	for turn := 0; turn < opts.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return lastAssistantText, ctx.Err()
		}

		apiMessages := messagesToAPI(messages, nil)

		resp, err := client.SendMessage(ctx, apiMessages, systemPrompts, apiTools, opts.MaxTokens)
		if err != nil {
			return lastAssistantText, err
		}
		if resp == nil {
			return lastAssistantText, fmt.Errorf("nil response")
		}

		// 收集 assistant 的 content blocks, 区分 text / tool_use
		var (
			assistantBlocks []types.ContentBlock
			toolUseBlocks   []types.ContentBlock
		)
		for _, blk := range resp.Content {
			b := blk
			if b.Type == types.ContentBlockToolUse {
				toolUseBlocks = append(toolUseBlocks, b)
			}
			assistantBlocks = append(assistantBlocks, b)
		}

		// 5. XML / bracket 回退: 把文本里的工具调用补齐成 tool_use blocks; 即使
		// 没有提取出可执行的工具, internal_hook.MergeXMLToolCalls 也会把 <minimax:tool_call>
		// 等噪声标签从 text 块里剥离, 这一步必须早于 lastAssistantText 计算.
		mergedAssistant, mergedToolUse, _ := internal_hook.MergeXMLToolCalls(assistantBlocks, toolUseBlocks, fmt.Sprintf("iso_t%d", turn))
		assistantBlocks = mergedAssistant
		toolUseBlocks = mergedToolUse

		// DisableTools 模式下, 即使解出来工具调用也不执行: 丢弃 tool_use blocks,
		// 仅保留清洗后的文本. 这样思考型角色 (architect/researcher/planner)
		// 单轮就返回, 不会陷入多轮工具循环.
		if opts.DisableTools {
			toolUseBlocks = nil
			kept := assistantBlocks[:0]
			for _, b := range assistantBlocks {
				if b.Type == types.ContentBlockToolUse {
					continue
				}
				kept = append(kept, b)
			}
			assistantBlocks = kept
		}

		// 用清洗后的 assistantBlocks 拼接本轮 text. 这样最终返回值不会泄漏
		// 残留的 XML 包裹标签, 上层 (architect 校验器等) 看到的是纯净文本.
		// 同时收集 thinking 块文本 (MiniMax 等模型会把推理内容放在 thinking 块).
		var turnText strings.Builder
		for _, b := range assistantBlocks {
			if b.Type == types.ContentBlockText && b.Text != "" {
				turnText.WriteString(b.Text)
			}
			if b.Type == types.ContentBlockThinking && b.Thinking != "" {
				turnText.WriteString(b.Thinking)
			}
		}
		if turnText.Len() > 0 {
			lastAssistantText = turnText.String()
		}

		// 6. 构造 assistant 消息追加进 messages
		if len(assistantBlocks) > 0 {
			messages = append(messages, types.Message{
				Type:       types.MessageTypeAssistant,
				UUID:       internal_hook.GenerateUUID(),
				Content:    assistantBlocks,
				StopReason: resp.StopReason,
				CreatedAt:  time.Now(),
			})
		}

		// 7. 没有 tool_use → 完成, 返回最后文本.
		if len(toolUseBlocks) == 0 {
			// 如果整个 turn 既没有 text 也没有 tool_use, 给个明确错误.
			if lastAssistantText == "" {
				return "", fmt.Errorf("assistant returned empty content")
			}
			return lastAssistantText, nil
		}

		// 8. 有 tool_use → 跑工具, 把结果追加为 user 消息.
		if e.Tools == nil {
			return lastAssistantText, fmt.Errorf("tool registry not configured, cannot execute %d tool_use blocks", len(toolUseBlocks))
		}

		// 8a. 黑/白名单闸: RunIsolated 不经 HookChain, 在这里应用与主循环
		// ToolGateHook 相同的 toolExposed 语义 — 未获准的 tool_use (含 XML 回退
		// 提取的凭空调用) 替换为错误 tool_result, 绝不执行.
		gated, deniedResults := gateToolUses(e.Config, toolUseBlocks)
		messages = append(messages, deniedResults...)
		toolUseBlocks = gated
		if len(toolUseBlocks) == 0 {
			continue // 全部被拒: 错误结果已入上下文, 让模型重新选择
		}

		effectivePerm := e.Config.PermissionMode
		if e.Config.DynamicPlanCheck != nil && e.Config.DynamicPlanCheck() {
			effectivePerm = types.PermissionModePlan
		}
		tctx := &tool.ToolContext{
			Cwd:              e.Config.Cwd,
			PermissionMode:   effectivePerm,
			MainLoopModel:    e.Config.Model,
			IsNonInteractive: e.Config.IsNonInteractive,
			Debug:            e.Config.Debug,
			Messages:         messages,
			GlobalPerm:       e.PermChecker,
		}
		toolResults := tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, e.HookRunner)
		// RunTools 返回的是 user-role tool_result 消息, 直接追加
		messages = append(messages, toolResults...)
	}

	// 超过 MaxTurns 仍未完成: 返回目前为止的文本 + 错误.
	if lastAssistantText != "" {
		return lastAssistantText, fmt.Errorf("max_turns (%d) reached without final answer", opts.MaxTurns)
	}
	return "", fmt.Errorf("max_turns (%d) reached without any text response", opts.MaxTurns)
}

// gateToolUses 对 tool_use 块应用 Config.toolExposed (黑名单 + 白名单):
// 未获准的块被替换为错误 tool_result 消息以保持协议一致 (每个 tool_use 必须有
// 对应 tool_result), 获准的原样返回. RunIsolated 不走引擎 HookChain, 这是它的
// ToolGateHook 等价物.
func gateToolUses(cfg *Config, blocks []types.ContentBlock) (allowed []types.ContentBlock, denied []types.Message) {
	for _, b := range blocks {
		if cfg.toolExposed(b.Name) {
			allowed = append(allowed, b)
			continue
		}
		denied = append(denied, types.Message{
			Type: types.MessageTypeUser,
			UUID: internal_hook.GenerateUUID(),
			Content: []types.ContentBlock{{
				Type:      types.ContentBlockToolResult,
				ToolUseID: b.ID,
				Content:   fmt.Sprintf("工具 %s 不在本会话允许的工具清单内，已被拒绝。请只使用当前可见的工具。", b.Name),
				IsError:   true,
			}},
			CreatedAt: time.Now(),
		})
	}
	return allowed, denied
}

// MessagesToAPI 把内部 types.Message 列表转换成 API 协议的 APIMessage 列表
// (公开 messagesToAPI, 供外部包测试 / 集成).
func MessagesToAPI(messages []types.Message) []types.APIMessage {
	return messagesToAPI(messages, nil)
}

// 保证 json 包不会因为后续没用到 而被 goimports 删除 (这里没用, 留位).
var _ = json.RawMessage(nil)
