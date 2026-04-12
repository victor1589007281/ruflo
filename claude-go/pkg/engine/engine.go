// Package engine 实现核心查询引擎和 ReAct 循环。
// 对应 TS 源码: review/claude/src/QueryEngine.ts + review/claude/src/query.ts
//
// 这是 Claude Code 客户端的心脏。QueryEngine 管理对话状态，
// queryLoop 是一个 while(true) 循环:
//   1. (可选) auto-compact / micro-compact / snip
//   2. 调用模型 API (流式)
//   3. 解析 assistant 响应中的 tool_use 块
//   4. 如果无 tool_use → 执行 stop hooks → 返回
//   5. 如果有 tool_use → 执行工具 (partitionToolCalls) → 追加结果 → 继续循环
//
// 完整特性:
//   - MicroCompact: 截断过大的 tool_result (对应 TS: microCompact.ts)
//   - 断路器 (Circuit Breaker): 连续错误触发熔断 (对应 TS: apiRateLimiter)
//   - Withheld Errors: 暂存 API 错误等待恢复 (对应 TS: withheld error handling)
//   - max_output_tokens 恢复: stop_reason=max_tokens 时自动继续 (对应 TS: PTL recovery)
//   - PTL reactive compact: prompt_too_long 时紧急压缩后重试 (对应 TS: reactive compact)
//   - Abort: context 取消时立即终止 (对应 TS: AbortController)
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

var uuidCounter atomic.Int64

// 断路器常量 (Circuit Breaker)
// 对应 TS: query.ts 中 consecutiveErrorCount 相关逻辑
const (
	maxConsecutiveErrors = 5  // 连续错误达到此阈值触发熔断
	microCompactMaxChars = 50000 // MicroCompact 截断阈值 (字符)
)

// QueryEngine 查询引擎，管理整个对话循环。
// 对应 TS: QueryEngine.ts 中的 class QueryEngine。
//
// 使用方式:
//   engine := NewQueryEngine(config)
//   resultCh := engine.SubmitMessage(ctx, userMessage)
//   for msg := range resultCh { ... }
type QueryEngine struct {
	Config      *Config
	Messages    []types.Message
	Tools       *tool.Registry
	APIClient   *api.Client
	HookRunner  *hooks.Runner
	PermChecker *permissions.Checker
	Compactor   *compact.Compactor
	PromptMgr   *prompt.Manager
	MemoryStore *memory.TieredStore // 多层记忆存储 (可选, nil 则不启用)
	mu          sync.Mutex
}

// Config 引擎配置
type Config struct {
	Model            string
	FallbackModel    string
	MaxTokens        int
	MaxTurns         int    // queryLoop 最大迭代次数 (0 = 无限)
	Cwd              string // 当前工作目录
	PermissionMode   types.PermissionMode
	IsNonInteractive bool
	Debug            bool
	SessionID        string
	// DynamicPlanCheck 动态计划模式检查。
	// 当 LLM 调用 EnterPlanMode 时返回 true, 引擎自动切换到只读权限。
	// 对应 TS: QueryEngine 中 permissionMode 与 PlanModeActive 联动。
	DynamicPlanCheck func() bool
	// DisabledTools 禁用的工具名列表。
	// queryLoop 在构建 API 请求时过滤掉这些工具，防止 LLM 自主调用。
	// 用途: 飞书 processAndReply 场景下禁止 LLM 自主创建/删除团队。
	DisabledTools map[string]bool
}

// NewQueryEngine 创建查询引擎
func NewQueryEngine(
	cfg *Config,
	apiClient *api.Client,
	tools *tool.Registry,
	hookRunner *hooks.Runner,
	permChecker *permissions.Checker,
	compactor *compact.Compactor,
	promptMgr *prompt.Manager,
) *QueryEngine {
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 16384
	}
	return &QueryEngine{
		Config:      cfg,
		APIClient:   apiClient,
		Tools:       tools,
		HookRunner:  hookRunner,
		PermChecker: permChecker,
		Compactor:   compactor,
		PromptMgr:   promptMgr,
	}
}

// SubmitMessage 提交用户消息，返回响应消息通道。
// 对应 TS: QueryEngine.submitMessage()
//
// 流程:
//   1. 将用户消息追加到 Messages
//   2. 组装系统提示词 (PromptMgr)
//   3. 启动 queryLoop (goroutine)
//   4. 通过 channel 返回所有响应消息
func (e *QueryEngine) SubmitMessage(ctx context.Context, userContent string) <-chan types.Message {
	ch := make(chan types.Message, 50)

	e.mu.Lock()
	userMsg := types.Message{
		Type:      types.MessageTypeUser,
		UUID:      generateUUID(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: userContent}},
		CreatedAt: time.Now(),
	}
	e.Messages = append(e.Messages, userMsg)
	messages := make([]types.Message, len(e.Messages))
	copy(messages, e.Messages)
	e.mu.Unlock()

	go func() {
		defer close(ch)
		finalMsgs, terminal := e.queryLoop(ctx, messages, ch)
		_ = terminal

		// [BUG FIX] 将 queryLoop 中产生的完整对话历史 (包括 assistant 回复
		// 和 tool 结果) 持久化回 e.Messages，否则下一次 SubmitMessage 会丢失
		// 之前的 assistant/tool 消息。
		// 对应 TS: QueryEngine 在 query.ts 结束后保存完整 messages 列表。
		e.mu.Lock()
		e.Messages = finalMsgs
		e.mu.Unlock()
	}()

	return ch
}

// queryLoop 核心 ReAct 循环。
// 对应 TS: query.ts 中的 queryLoop()
//
// 这是整个系统最关键的函数。算法如下:
//
//   while (true) {
//     // Phase 1: 上下文压缩 (compact)
//     messages = autoCompact(messages)
//     messages = microCompact(messages)
//
//     // Phase 2: 组装提示词
//     systemPrompt = buildEffectiveSystemPrompt(...)
//
//     // Phase 3: 调用模型 API (流式)
//     for event in stream(messages, systemPrompt, tools):
//       收集 assistant 消息和 tool_use 块
//
//     // Phase 4: 执行 post-sampling hooks
//     executePostSamplingHooks(...)
//
//     // Phase 5a: 如果没有 tool_use → stop hooks → return
//     if no tool_use:
//       stopResult = handleStopHooks(...)
//       if stopResult.blocking → 追加 blocking message → continue
//       else → return "completed"
//
//     // Phase 5b: 如果有 tool_use → 执行工具 → 追加结果 → continue
//     toolResults = runTools(toolUseBlocks, registry, context)
//     messages = append(messages, assistantMsgs, toolResults)
//   }
func (e *QueryEngine) queryLoop(ctx context.Context, messages []types.Message, ch chan<- types.Message) ([]types.Message, types.Terminal) {
	turnCount := 0
	currentModel := e.Config.Model
	consecutiveErrors := 0 // 断路器: 连续错误计数
	stopReason := ""       // 模型停止原因

	for {
		// ============================================================
		// Phase 0: Abort 检查
		// 对应 TS: queryLoop 顶部 if (abortController.signal.aborted)
		// ============================================================
		if ctx.Err() != nil {
			return messages, types.Terminal{Reason: "aborted"}
		}

		// ============================================================
		// Phase 1a: Auto-compact (上下文压缩)
		// 对应 TS: queryLoop 中的 deps.autocompact(...) 调用
		// ============================================================
		if e.Compactor != nil {
			compacted, err := e.Compactor.AutoCompact(ctx, messages, currentModel)
			if err == nil && compacted != nil {
				// [NEW] 压缩前智能提取关键事实到 Episodic Memory
				// 使用 LLM Anchored Iterative Summarization (回退到启发式)
				if e.MemoryStore != nil {
					cutoff := len(messages) - 4
					if cutoff > 0 {
						facts := e.Compactor.SmartExtractKeyFacts(ctx, messages[:cutoff])
						for _, fact := range facts {
							e.MemoryStore.Add(&memory.MemoryEntry{
								Content:    fact,
								Source:     "pre_compact",
								Importance: 0.7,
							})
						}
					}
				}
				messages = compacted
			}
		}

		// ============================================================
		// Phase 1b: MicroCompact (截断过大 tool_result)
		// 对应 TS: services/compact/microCompact.ts
		// 每轮迭代前对历史消息中的 tool_result 进行截断，
		// 防止单个工具输出过大撑满上下文窗口。
		// ============================================================
		messages = compact.MicroCompact(messages, microCompactMaxChars)

		// ============================================================
		// Phase 2: 组装系统提示词
		// ============================================================
		systemPrompt := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)

		// 仅在首轮注入相关记忆（避免多轮 tool_use 循环中反复注入膨胀上下文）
		if turnCount == 0 && e.MemoryStore != nil && e.MemoryStore.Count() > 0 && len(messages) > 0 {
			lastUserText := ""
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Type == types.MessageTypeUser {
					for _, b := range messages[i].Content {
						if b.Text != "" {
							lastUserText = b.Text
							break
						}
					}
					break
				}
			}
			if lastUserText != "" {
				relevant := e.MemoryStore.Retrieve(lastUserText, 5)
				// 过滤超过 2 小时的记忆，降低历史上下文对当前对话的干扰
				var fresh []*memory.MemoryEntry
				for _, m := range relevant {
					if time.Since(m.CreatedAt) < 2*time.Hour {
						fresh = append(fresh, m)
					}
				}
				if len(fresh) > 0 {
					memPrompt := memory.FormatForPrompt(fresh)
					if len(systemPrompt) > 0 {
						systemPrompt[0] += "\n" + memPrompt
					}
				}
			}
		}

		// ============================================================
		// Phase 3: 调用模型 API (流式)
		// 对应 TS: while(attemptWithFallback) + for await(callModel(...))
		// ============================================================
		apiMessages := messagesToAPI(messages)
		allTools := e.Tools.APITools()
		var apiTools []types.APITool
		if len(e.Config.DisabledTools) > 0 {
			for _, t := range allTools {
				if !e.Config.DisabledTools[t.Name] {
					apiTools = append(apiTools, t)
				}
			}
		} else {
			apiTools = allTools
		}

		eventCh, errCh := e.APIClient.StreamMessage(ctx, apiMessages, systemPrompt, apiTools, e.Config.MaxTokens)

		var assistantBlocks []types.ContentBlock
		var toolUseBlocks []types.ContentBlock
		var currentText strings.Builder
		var currentToolInput strings.Builder
		var currentBlock *types.ContentBlock
		var usage *types.Usage
		stopReason = ""

		// 收集流式响应
		// 使用 range eventCh 而非 select{eventCh, errCh}，避免 closed errCh 导致无限旋转
		for event := range eventCh {
			switch event.Type {
			case "message_start":
				if event.Message != nil {
					usage = event.Message.Usage
				}
			case "ping":
				// DashScope 发送 ping 心跳, 忽略
			case "content_block_start":
				if event.ContentBlock != nil {
					currentBlock = event.ContentBlock
					currentText.Reset()
					currentToolInput.Reset()
				}
			case "content_block_delta":
				if event.Delta != nil {
					switch event.Delta.Type {
					case "text_delta":
						currentText.WriteString(event.Delta.Text)
					case "input_json_delta":
						currentToolInput.WriteString(event.Delta.PartialJSON)
					case "thinking_delta":
						currentText.WriteString(event.Delta.Thinking)
					}
					// 捕获 stop_reason (可能在 delta 中)
					if event.Delta.StopReason != "" {
						stopReason = event.Delta.StopReason
					}
				}
			case "content_block_stop":
				if currentBlock != nil {
					block := *currentBlock
					switch block.Type {
					case types.ContentBlockText:
						block.Text = currentText.String()
					case types.ContentBlockToolUse:
						block.Input = json.RawMessage(currentToolInput.String())
						toolUseBlocks = append(toolUseBlocks, block)
					case types.ContentBlockThinking:
						block.Thinking = currentText.String()
					case types.ContentBlockServerToolUse:
						// 服务端工具调用 (如 web_search_tool)
						block.Name = currentBlock.Name
						block.Input = json.RawMessage(currentToolInput.String())
					case types.ContentBlockServerToolResult:
						// 服务端工具结果
						block.Content = currentText.String()
					}
					assistantBlocks = append(assistantBlocks, block)
				}
			case "message_delta":
				if event.Usage != nil {
					usage = event.Usage
				}
				if event.Delta != nil && event.Delta.StopReason != "" {
					stopReason = event.Delta.StopReason
				}
			}
		}

		// ============================================================
		// 错误处理: PTL / fallback / 断路器
		// 对应 TS: FallbackTriggeredError, PromptTooLongError 处理
		// ============================================================
		if streamErr := <-errCh; streamErr != nil {
			// PTL (Prompt Too Long) → 触发 reactive compact
			// 对应 TS: catch PromptTooLongError → runCompaction
			var ptlErr *api.PromptTooLongError
			if errors.As(streamErr, &ptlErr) && e.Compactor != nil {
				compacted, compactErr := e.Compactor.AutoCompact(ctx, messages, currentModel)
				if compactErr == nil && compacted != nil {
					messages = compacted
					continue // 压缩后重试
				}
			}

			// 尝试 fallback model
			if e.Config.FallbackModel != "" && currentModel != e.Config.FallbackModel {
				currentModel = e.Config.FallbackModel
				for _, block := range toolUseBlocks {
					messages = append(messages, types.Message{
						Type: types.MessageTypeUser,
						Content: []types.ContentBlock{{
							Type:      types.ContentBlockToolResult,
							ToolUseID: block.ID,
							Content:   "工具调用因模型回退而取消",
							IsError:   true,
						}},
					})
				}
				continue
			}

			// 断路器: 累计连续错误
			// 对应 TS: consecutiveErrorCount++ → 超阈值时退出循环
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveErrors {
				errMsg := types.Message{
					Type: types.MessageTypeAssistant,
					UUID: generateUUID(),
					Content: []types.ContentBlock{{
						Type: types.ContentBlockText,
						Text: fmt.Sprintf("断路器触发: 连续 %d 次 API 错误, 最后错误: %v", consecutiveErrors, streamErr),
					}},
					IsApiErrorMessage: true,
					CreatedAt:         time.Now(),
				}
				ch <- errMsg
				return messages, types.Terminal{Reason: "circuit_breaker", Error: streamErr}
			}

			// Withheld error: 将错误作为 assistant 消息暂存, 继续循环等待恢复
			// 对应 TS: withheld error 在特定条件下不立即终止
			errMsg := types.Message{
				Type: types.MessageTypeAssistant,
				UUID: generateUUID(),
				Content: []types.ContentBlock{{
					Type: types.ContentBlockText,
					Text: fmt.Sprintf("API Error (attempt %d/%d): %v", consecutiveErrors, maxConsecutiveErrors, streamErr),
				}},
				IsApiErrorMessage: true,
				CreatedAt:         time.Now(),
			}
			ch <- errMsg

			// overloaded 错误时短暂等待后重试
			var overloadErr *api.OverloadedError
			if errors.As(streamErr, &overloadErr) {
				time.Sleep(time.Duration(consecutiveErrors) * 2 * time.Second)
				continue
			}

			return messages, types.Terminal{Reason: "model_error", Error: streamErr}
		}

		// 成功收到响应, 重置断路器
		consecutiveErrors = 0

		// 检查 context 是否被取消
		if ctx.Err() != nil {
			return messages, types.Terminal{Reason: "aborted_streaming"}
		}

		// ============================================================
		// 构建 assistant 消息
		// ============================================================
		if len(assistantBlocks) > 0 {
			assistantMsg := types.Message{
				Type:       types.MessageTypeAssistant,
				UUID:       generateUUID(),
				Content:    assistantBlocks,
				Model:      currentModel,
				StopReason: stopReason,
				Usage:      usage,
				CreatedAt:  time.Now(),
			}
			ch <- assistantMsg
			messages = append(messages, assistantMsg)
		}

		// ============================================================
		// Phase 3.5: max_output_tokens 恢复
		// 对应 TS: stop_reason=="max_tokens" → 自动继续
		// 当模型因输出 token 上限停止（非因工具调用结束），
		// 注入一个 user 消息要求继续，然后重新进入循环。
		// ============================================================
		if stopReason == string(types.StopReasonMaxTokens) && len(toolUseBlocks) == 0 {
			continueMsg := types.Message{
				Type: types.MessageTypeUser,
				UUID: generateUUID(),
				Content: []types.ContentBlock{{
					Type: types.ContentBlockText,
					Text: "Continue from where you left off. Do not repeat what you already said.",
				}},
				IsMeta:    true,
				CreatedAt: time.Now(),
			}
			messages = append(messages, continueMsg)
			continue
		}

		// ============================================================
		// Phase 4: Post-sampling hooks
		// 对应 TS: executePostSamplingHooks(...)
		// ============================================================
		if e.HookRunner != nil {
			e.HookRunner.ExecutePostSamplingHooks(messages)
		}

		// ============================================================
		// Phase 5: 判断是否有 tool_use
		// ============================================================
		if len(toolUseBlocks) == 0 {
			// 无 tool_use → 执行 stop hooks
			if e.HookRunner != nil {
				blockingMsgs := e.HookRunner.ExecuteStopHooks(messages)
				if len(blockingMsgs) > 0 {
					for _, bm := range blockingMsgs {
						ch <- bm
						messages = append(messages, bm)
					}
					continue
				}
			}
			return messages, types.Terminal{Reason: "completed"}
		}

		// ============================================================
		// Phase 5b: 有 tool_use → 执行工具
		// ============================================================

		// 拦截被禁用的工具调用 — 将其替换为错误结果，不实际执行
		if len(e.Config.DisabledTools) > 0 {
			var allowedToolUse []types.ContentBlock
			for _, block := range toolUseBlocks {
				if e.Config.DisabledTools[block.Name] {
					// 返回错误结果给 LLM，让它知道此工具不可用
					errResult := types.Message{
						Type: types.MessageTypeUser,
						UUID: generateUUID(),
						Content: []types.ContentBlock{{
							Type:      types.ContentBlockToolResult,
							ToolUseID: block.ID,
							Content:   fmt.Sprintf("工具 %s 在当前会话中不可用。团队操作请通过 /team 命令或意图识别完成。", block.Name),
							IsError:   true,
						}},
						CreatedAt: time.Now(),
					}
					ch <- errResult
					messages = append(messages, errResult)
				} else {
					allowedToolUse = append(allowedToolUse, block)
				}
			}
			toolUseBlocks = allowedToolUse
			if len(toolUseBlocks) == 0 {
				continue // 所有工具都被禁用，回到循环让 LLM 重新回复
			}
		}

		// maxTurns 检查 (在 tool 执行后检查, 对应 TS)
		turnCount++
		if e.Config.MaxTurns > 0 && turnCount > e.Config.MaxTurns {
			return messages, types.Terminal{Reason: "max_turns"}
		}

		// 动态 Plan Mode 联动: LLM 调用 EnterPlanMode → 自动切换只读
		effectivePerm := e.Config.PermissionMode
		if e.Config.DynamicPlanCheck != nil && e.Config.DynamicPlanCheck() {
			effectivePerm = types.PermissionModePlan
		}

		tctx := &tool.ToolContext{
			Cwd:              e.Config.Cwd,
			PermissionMode:   effectivePerm,
			MainLoopModel:    currentModel,
			IsNonInteractive: e.Config.IsNonInteractive,
			Debug:            e.Config.Debug,
			Messages:         messages,
			GlobalPerm:       e.PermChecker,
		}

		toolResults := tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, e.HookRunner)

		for _, result := range toolResults {
			ch <- result
			messages = append(messages, result)
		}
	}
}

// messagesToAPI 将内部 Message 格式转换为 API 格式。
// 对应 TS: utils/messages.ts 中的 normalizeMessagesForAPI()
//
// 关键逻辑: 合并连续同角色的消息。
// Anthropic API 要求 user/assistant 严格交替。多个 tool_result
// 作为独立 user 消息存在时，必须合并到同一个 user 消息中。
func messagesToAPI(messages []types.Message) []types.APIMessage {
	var result []types.APIMessage

	for _, msg := range messages {
		role := ""
		switch msg.Type {
		case types.MessageTypeUser:
			role = "user"
		case types.MessageTypeAssistant:
			role = "assistant"
		default:
			continue
		}

		// 合并连续同角色消息的 content 块
		if len(result) > 0 && result[len(result)-1].Role == role {
			var existingBlocks []types.ContentBlock
			_ = json.Unmarshal(result[len(result)-1].Content, &existingBlocks)
			existingBlocks = append(existingBlocks, msg.Content...)
			merged, _ := json.Marshal(existingBlocks)
			result[len(result)-1].Content = merged
		} else {
			content, _ := json.Marshal(msg.Content)
			result = append(result, types.APIMessage{
				Role:    role,
				Content: content,
			})
		}
	}

	return result
}

// generateUUID 生成唯一标识 (时间戳 + 全局原子计数器，避免并发碰撞)。
func generateUUID() string {
	seq := uuidCounter.Add(1)
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), seq)
}

// GetMessages 返回当前对话消息列表
func (e *QueryEngine) GetMessages() []types.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]types.Message, len(e.Messages))
	copy(result, e.Messages)
	return result
}

// ClearMessages 清空对话历史
func (e *QueryEngine) ClearMessages() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Messages = nil
}
