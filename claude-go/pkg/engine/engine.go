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
//   engine.EnableFrontierOptimizations()  // 可选: 启用前沿模型优化组件
//   resultCh := engine.SubmitMessage(ctx, userMessage)
//   for msg := range resultCh { ... }
type QueryEngine struct {
	Config       *Config
	Messages     []types.Message
	Tools        *tool.Registry
	APIClient    *api.Client
	HookRunner   *hooks.Runner
	PermChecker  *permissions.Checker
	Compactor    *compact.Compactor
	PromptMgr    *prompt.Manager
	MemoryStore  *memory.TieredStore   // 多层记忆存储 (可选, nil 则不启用)
	FactStore    *memory.FactStore    // V3 Anti-Amnesia: L2 结构化记忆 (可选)
	Ingestor     *memory.Ingestor    // V3: 记忆摄入器 (可选)
	SessionStore SessionStoreInterface // 会话持久化 (可选, nil 则不启用)

	// ========== 前沿模型优化组件 (全部可选, nil 则走基线行为) ==========
	// 设计依据: docs/query-engine-frontier-optimization.md
	// 开关由 Config.Enable* 字段控制, 默认通过 EnableFrontierOptimizations() 统一启用。
	Metrics       *EngineMetrics        // G10 共享指标
	PromptCache   *PromptCacheBuilder   // G1 Prompt cache 构建
	Budget        *TokenBudgetManager   // G2 Token 预算分级
	LoopDet       *LoopDetector         // G3 工具循环检测
	ErrClassifier *ErrorClassifier      // G4 错误族隔离 budget
	JSONRepair    *JSONRepair           // G5 工具输入 JSON 修复
	TrajStore     TrajectoryStore       // G6 轨迹记忆
	StopDet       *StopSignalDetector   // G7 CaRT 停止信号

	CumulativeUsage types.Usage // 本会话累积 token 消耗
	ContextBudget   int         // 上下文窗口大小 (tokens, 默认 200000)

	mu sync.Mutex
}

// SessionStoreInterface 会话存储抽象。
type SessionStoreInterface interface {
	AppendUserMessage(msg types.Message, cwd, model string)
	AppendAssistantMessage(msg types.Message)
	SessionID() string
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

	// ==================== 前沿优化特性开关 (默认关闭) ====================
	// 详细方案见 docs/query-engine-frontier-optimization.md。
	// 推荐通过 QueryEngine.EnableFrontierOptimizations() 统一启用 P0/P1 组件。

	// EnableMetrics 启用引擎级指标采集 (G10, 低开销)。
	EnableMetrics bool
	// EnablePromptCache 启用 prompt 稳定前缀布局与 cache 命中追踪 (G1)。
	EnablePromptCache bool
	// EnableBudget 启用 Token 预算分级降级 (G2)。
	// 到 Red/Critical 会对旧 tool_result 做摘要压缩, 可能影响老消息可回溯性。
	EnableBudget bool
	// EnableLoopDetector 启用工具循环检测 (G3)。
	EnableLoopDetector bool
	// EnableErrorClassifier 启用错误族隔离 budget (G4)。
	// 启用后替代原 consecutiveErrors 计数, 各族独立预算。
	EnableErrorClassifier bool
	// EnableJSONRepair 启用工具输入 JSON fallback 修复 (G5)。
	EnableJSONRepair bool
	// EnableTrajectory 启用轨迹记忆 (G6)。需要配套 MemoryStore。
	EnableTrajectory bool
	// EnableStopSignal 启用 CaRT 停止信号检测 (G7, 实验性, 默认关闭)。
	EnableStopSignal bool
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
	e := &QueryEngine{
		Config:        cfg,
		APIClient:     apiClient,
		Tools:         tools,
		HookRunner:    hookRunner,
		PermChecker:   permChecker,
		Compactor:     compactor,
		PromptMgr:     promptMgr,
		ContextBudget: 200000,
	}
	// 按 Config 开关懒加载对应组件。调用 EnableFrontierOptimizations() 可一次性启用 P0/P1。
	e.applyFeatureFlags()
	return e
}

// EnableFrontierOptimizations 一键启用推荐的 P0/P1 前沿优化组件。
// 详细设计见 docs/query-engine-frontier-optimization.md。
//
// 启用清单:
//   P0: Metrics, ErrorClassifier, JSONRepair, LoopDetector
//   P1: PromptCache, Budget, Trajectory (若已配置 MemoryStore)
//
// StopSignal/HeavyMode 需单独设置 Config.EnableStopSignal=true。
func (e *QueryEngine) EnableFrontierOptimizations() {
	if e == nil || e.Config == nil {
		return
	}
	e.Config.EnableMetrics = true
	e.Config.EnableErrorClassifier = true
	e.Config.EnableJSONRepair = true
	e.Config.EnableLoopDetector = true
	e.Config.EnablePromptCache = true
	e.Config.EnableBudget = true
	if e.MemoryStore != nil {
		e.Config.EnableTrajectory = true
	}
	e.applyFeatureFlags()
}

// applyFeatureFlags 根据 Config.Enable* 懒初始化组件实例。
// 幂等: 多次调用安全, 已存在的组件不会被替换。
func (e *QueryEngine) applyFeatureFlags() {
	if e == nil || e.Config == nil {
		return
	}
	if e.Config.EnableMetrics && e.Metrics == nil {
		e.Metrics = NewEngineMetrics()
	}
	if e.Config.EnablePromptCache && e.PromptCache == nil {
		e.PromptCache = NewPromptCacheBuilder()
	}
	if e.Config.EnableBudget && e.Budget == nil {
		e.Budget = NewTokenBudgetManager(e.ContextBudget)
	}
	if e.Config.EnableLoopDetector && e.LoopDet == nil {
		e.LoopDet = NewLoopDetector()
	}
	if e.Config.EnableErrorClassifier && e.ErrClassifier == nil {
		e.ErrClassifier = NewErrorClassifier()
	}
	if e.Config.EnableJSONRepair && e.JSONRepair == nil {
		e.JSONRepair = NewJSONRepair()
	}
	if e.Config.EnableTrajectory && e.TrajStore == nil && e.MemoryStore != nil {
		e.TrajStore = NewMemoryBackedTrajectoryStore(e.MemoryStore)
	}
	if e.Config.EnableStopSignal && e.StopDet == nil {
		e.StopDet = NewStopSignalDetector()
	}
}

// GetEngineMetrics 返回引擎指标快照 (供 dashboard/telemetry 采集)。
// Metrics 未启用时返回空 map。
func (e *QueryEngine) GetEngineMetrics() map[string]any {
	if e == nil || e.Metrics == nil {
		return map[string]any{}
	}
	return e.Metrics.Snapshot()
}

// ContextUsageInfo 返回上下文使用信息。
type ContextUsageInfo struct {
	TotalTokens   int
	InputTokens   int
	OutputTokens  int
	Budget        int
	Percentage    float64
	MessageCount  int
}

// GetContextUsage 获取上下文使用信息。
func (e *QueryEngine) GetContextUsage() ContextUsageInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := e.CumulativeUsage.InputTokens + e.CumulativeUsage.OutputTokens
	budget := e.ContextBudget
	if budget == 0 {
		budget = 200000
	}
	return ContextUsageInfo{
		TotalTokens:  total,
		InputTokens:  e.CumulativeUsage.InputTokens,
		OutputTokens: e.CumulativeUsage.OutputTokens,
		Budget:       budget,
		Percentage:   float64(total) / float64(budget) * 100,
		MessageCount: len(e.Messages),
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

	if e.SessionStore != nil {
		e.SessionStore.AppendUserMessage(userMsg, e.Config.Cwd, e.Config.Model)
	}

	go func() {
		defer close(ch)
		finalMsgs, terminal := e.queryLoop(ctx, messages, ch, nil)
		_ = terminal

		e.mu.Lock()
		e.Messages = finalMsgs
		e.mu.Unlock()
	}()

	return ch
}

// SubmitStream 提交用户消息，返回 StreamEvent 通道实现 token-by-token 流式输出。
func (e *QueryEngine) SubmitStream(ctx context.Context, userContent string) <-chan types.StreamEvent {
	streamCh := make(chan types.StreamEvent, 200)
	msgCh := make(chan types.Message, 50)

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

	if e.SessionStore != nil {
		e.SessionStore.AppendUserMessage(userMsg, e.Config.Cwd, e.Config.Model)
	}

	go func() {
		defer close(streamCh)
		defer close(msgCh)
		finalMsgs, terminal := e.queryLoop(ctx, messages, msgCh, streamCh)
		_ = terminal

		e.mu.Lock()
		e.Messages = finalMsgs
		e.mu.Unlock()
	}()

	return streamCh
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
func (e *QueryEngine) queryLoop(ctx context.Context, messages []types.Message, ch chan<- types.Message, streamCh chan<- types.StreamEvent) ([]types.Message, types.Terminal) {
	turnCount := 0
	currentModel := e.Config.Model
	consecutiveErrors := 0 // 断路器: 连续错误计数 (ErrClassifier 启用后被绕过)
	stopReason := ""       // 模型停止原因

	// ==================== Trajectory 轨迹追踪状态 ====================
	// 仅在 EnableTrajectory 时有效, 末尾统一写入 TrajStore。
	turnStart := time.Now()
	var turnUserIntent string
	var turnToolSigs []ToolSig
	var turnPlanMsgs []types.Message
	if e.TrajStore != nil {
		turnUserIntent = extractLatestUserIntent(messages)
	}

	for {
		// ============================================================
		// Phase 0: Abort 检查
		// 对应 TS: queryLoop 顶部 if (abortController.signal.aborted)
		// ============================================================
		if ctx.Err() != nil {
			e.recordTrajectoryIfNeeded(turnUserIntent, turnPlanMsgs, turnToolSigs, stopReason, true, turnStart)
			if e.Metrics != nil {
				e.Metrics.TurnsAborted.Add(1)
			}
			return messages, types.Terminal{Reason: "aborted"}
		}

		// ============================================================
		// Phase 0.5: Token Budget 分级降级 (G2)
		// 对应 docs/query-engine-frontier-optimization.md §3.2
		// Red/Critical 时对 3 轮前的 tool_result 做紧急摘要, 防止撞 PTL。
		// ============================================================
		if e.Budget != nil {
			level := e.Budget.Level(messages)
			if level >= BudgetRed {
				messages = e.Budget.Degrade(messages, level)
				if e.Metrics != nil {
					e.Metrics.RecordBudgetDegrade(int(level))
				}
			}
		}

		// ============================================================
		// Phase 1a: Auto-compact (上下文压缩)
		// 对应 TS: queryLoop 中的 deps.autocompact(...) 调用
		// ============================================================
		if e.Compactor != nil {
			compacted, err := e.Compactor.AutoCompact(ctx, messages, currentModel)
			if err == nil && compacted != nil {
				// PreCompact 蒸馏: 提取关键事实到 L1 + L2
				cutoff := len(messages) - 6
				if cutoff > 0 {
					facts := e.Compactor.SmartExtractKeyFacts(ctx, messages[:cutoff])
					// L1: TieredStore
					if e.MemoryStore != nil {
						for _, fact := range facts {
							e.MemoryStore.Add(&memory.MemoryEntry{
								Content:    fact,
								Source:     "pre_compact",
								Importance: 0.7,
							})
						}
					}
					// L2: FactStore (通过 Ingestor 分类)
					if e.Ingestor != nil && len(facts) > 0 {
						e.Ingestor.IngestFacts(facts, "pre_compact")
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
		// 集成 G1 PromptCache: 稳定前缀 + 动态后缀 分层, 追踪本地 cache 命中率。
		// ============================================================
		systemPrompt := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)

		// 仅在首轮注入相关记忆（避免多轮 tool_use 循环中反复注入膨胀上下文）
		// 注入的记忆是 "动态内容", 但它每 turn 0 就固定, 所以 turn>0 时 prefix 是稳定的 — cache friendly。
		if turnCount == 0 && len(messages) > 0 {
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
				var memPrompt string

				// L1: TieredStore 情景记忆 (BM25 + Ebbinghaus)
				if e.MemoryStore != nil && e.MemoryStore.Count() > 0 {
					relevant := e.MemoryStore.Retrieve(lastUserText, 7)
					if len(relevant) > 0 {
						memPrompt += memory.FormatForPrompt(relevant)
					}
				}

				// L2: FactStore 结构化记忆 (分类衰减 + 混合检索)
				if e.FactStore != nil && e.FactStore.Count() > 0 {
					facts := e.FactStore.Retrieve(lastUserText, 5)
					if len(facts) > 0 {
						memPrompt += memory.FormatFactsForPrompt(facts)
					}
				}

				if memPrompt != "" && len(systemPrompt) > 0 {
					systemPrompt[0] += "\n" + memPrompt
				}
			}
		}

		// G1 PromptCache: 本地记录稳定前缀是否未变, 近似后端 cache 命中率。
		// 当前策略保守 — 不改变传递内容, 只做指标追踪; 后续 API 支持 cache_control 时再贴标。
		if e.PromptCache != nil {
			static, dynamic := SplitStaticDynamic(systemPrompt)
			_, hit := e.PromptCache.Build(static, dynamic)
			if e.Metrics != nil {
				e.Metrics.RecordCache(hit)
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
					if streamCh != nil {
						streamCh <- types.StreamEvent{
							Kind:       types.StreamEventDelta,
							DeltaText:  event.Delta.Text,
							BlockIndex: event.Index,
						}
					}
				case "input_json_delta":
					currentToolInput.WriteString(event.Delta.PartialJSON)
				case "thinking_delta":
					currentText.WriteString(event.Delta.Thinking)
					if streamCh != nil {
						streamCh <- types.StreamEvent{
							Kind:       types.StreamEventDelta,
							DeltaText:  event.Delta.Thinking,
							BlockIndex: event.Index,
							IsThinking: true,
						}
					}
				}
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
					if streamCh != nil {
						inputSummary := currentToolInput.String()
						if len(inputSummary) > 200 {
							inputSummary = inputSummary[:200] + "..."
						}
						streamCh <- types.StreamEvent{
							Kind:      types.StreamEventToolStart,
							ToolName:  block.Name,
							ToolInput: inputSummary,
						}
					}
				case types.ContentBlockThinking:
					block.Thinking = currentText.String()
				case types.ContentBlockServerToolUse:
					block.Name = currentBlock.Name
					block.Input = json.RawMessage(currentToolInput.String())
				case types.ContentBlockServerToolResult:
					block.Content = currentText.String()
				}
				assistantBlocks = append(assistantBlocks, block)
				if streamCh != nil {
					streamCh <- types.StreamEvent{Kind: types.StreamEventBlockDone, BlockIndex: event.Index}
				}
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
		//
		// G4 ErrorClassifier 启用时:
		//   - 按 HTTP 族分桶 budget (429/5xx/400/PTL 各有独立额度)
		//   - PTL → reactive compact (保留现有分支)
		//   - BadRequest/Auth → 立即终止, 不消耗其他族的预算
		//   - RateLimit/Overload/Network → 按族 backoff
		// ============================================================
		if streamErr := <-errCh; streamErr != nil {
			// PTL (Prompt Too Long) → 触发 reactive compact
			// 对应 TS: catch PromptTooLongError → runCompaction
			var ptlErr *api.PromptTooLongError
			if errors.As(streamErr, &ptlErr) && e.Compactor != nil {
				compacted, compactErr := e.Compactor.AutoCompact(ctx, messages, currentModel)
				if compactErr == nil && compacted != nil {
					messages = compacted
					if e.Metrics != nil {
						e.Metrics.RecordError(int(ErrFamilyPTL))
					}
					continue // 压缩后重试
				}
			}

			// 尝试 fallback model (保持原有行为, 不计入 budget)
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

			// ---- G4: ErrorClassifier 路径 (启用时替代原 consecutive 计数) ----
			if e.ErrClassifier != nil {
				family, abort, backoff, useCompact := e.ErrClassifier.Observe(streamErr)
				if e.Metrics != nil {
					e.Metrics.RecordError(int(family))
				}
				if abort {
					errMsg := types.Message{
						Type: types.MessageTypeAssistant,
						UUID: generateUUID(),
						Content: []types.ContentBlock{{
							Type: types.ContentBlockText,
							Text: fmt.Sprintf("%s 族 API 错误预算耗尽: %v", family, streamErr),
						}},
						IsApiErrorMessage: true,
						CreatedAt:         time.Now(),
					}
					ch <- errMsg
					if streamCh != nil {
						streamCh <- types.StreamEvent{Kind: types.StreamEventError, Error: streamErr}
					}
					if e.Metrics != nil {
						e.Metrics.TurnsError.Add(1)
					}
					e.recordTrajectoryIfNeeded(turnUserIntent, turnPlanMsgs, turnToolSigs, "error", false, turnStart)
					return messages, types.Terminal{Reason: "error_family_exhausted_" + family.String(), Error: streamErr}
				}
				// 非致命: 向用户 channel 推送一条 withheld error 消息
				errMsg := types.Message{
					Type: types.MessageTypeAssistant,
					UUID: generateUUID(),
					Content: []types.ContentBlock{{
						Type: types.ContentBlockText,
						Text: fmt.Sprintf("API %s (retry in %.1fs): %v", family, backoff.Seconds(), streamErr),
					}},
					IsApiErrorMessage: true,
					CreatedAt:         time.Now(),
				}
				ch <- errMsg
				// 超时/PTL 类建议做一次 reactive compact 以减小 prompt
				if useCompact && e.Compactor != nil {
					if compacted, cerr := e.Compactor.AutoCompact(ctx, messages, currentModel); cerr == nil && compacted != nil {
						messages = compacted
					}
				}
				if backoff > 0 {
					select {
					case <-time.After(backoff):
					case <-ctx.Done():
						return messages, types.Terminal{Reason: "aborted"}
					}
				}
				continue
			}

			// ---- 原始路径 (ErrorClassifier 未启用) ----
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
				if streamCh != nil {
					streamCh <- types.StreamEvent{Kind: types.StreamEventError, Error: streamErr}
				}
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
		if e.ErrClassifier != nil {
			e.ErrClassifier.ResetAll()
		}

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
			// Trajectory: 首条 assistant 消息的文字/thinking 用于提取 plan
			if e.TrajStore != nil && len(turnPlanMsgs) == 0 {
				turnPlanMsgs = append(turnPlanMsgs, assistantMsg)
			}
			ch <- assistantMsg
			if streamCh != nil {
				streamCh <- types.StreamEvent{Kind: types.StreamEventMessageDone, Message: &assistantMsg}
			}
			if e.SessionStore != nil {
				e.SessionStore.AppendAssistantMessage(assistantMsg)
			}
			if usage != nil {
				e.mu.Lock()
				e.CumulativeUsage.InputTokens += usage.InputTokens
				e.CumulativeUsage.OutputTokens += usage.OutputTokens
				e.CumulativeUsage.CacheReadInputTokens += usage.CacheReadInputTokens
				e.CumulativeUsage.CacheCreationInputTokens += usage.CacheCreationInputTokens
				e.mu.Unlock()
			}
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
			// 完成出口: 记录 trajectory + metrics
			if e.Metrics != nil {
				e.Metrics.TurnsTotal.Add(1)
				e.Metrics.TurnsSuccess.Add(1)
				e.Metrics.RecordTurnLatency(time.Since(turnStart))
			}
			e.recordTrajectoryIfNeeded(turnUserIntent, turnPlanMsgs, turnToolSigs, stopReason, false, turnStart)
			return messages, types.Terminal{Reason: "completed"}
		}

		// ============================================================
		// Phase 5b: 有 tool_use → 执行工具
		// ============================================================

		// G5 JSONRepair: 对 tool_use.Input 做 fallback 修复, 降低因流式截断导致的工具失败率。
		if e.JSONRepair != nil {
			for i, blk := range toolUseBlocks {
				repaired, res := e.JSONRepair.Try([]byte(blk.Input))
				if res.OK && res.Changed {
					toolUseBlocks[i].Input = repaired
					if e.Metrics != nil {
						e.Metrics.JSONRepairsApplied.Add(1)
					}
				} else if !res.OK {
					if e.Metrics != nil {
						e.Metrics.JSONRepairsFailed.Add(1)
					}
				}
			}
		}

		// G3 LoopDetector: 检测同工具同参数的死循环, 命中时注入反向提示替代实际执行。
		if e.LoopDet != nil {
			var suppressed []types.ContentBlock
			var looped bool
			for _, blk := range toolUseBlocks {
				if isLooped, suggestion := e.LoopDet.Observe(blk.Name, []byte(blk.Input)); isLooped {
					looped = true
					// 注入一个 tool_result 错误, 让模型看到系统反馈
					noteMsg := types.Message{
						Type: types.MessageTypeUser,
						UUID: generateUUID(),
						Content: []types.ContentBlock{{
							Type:      types.ContentBlockToolResult,
							ToolUseID: blk.ID,
							Content:   suggestion,
							IsError:   true,
						}},
						CreatedAt: time.Now(),
					}
					ch <- noteMsg
					messages = append(messages, noteMsg)
					if e.Metrics != nil {
						e.Metrics.ToolLoopsDetected.Add(1)
						e.Metrics.ToolLoopsSuppressed.Add(1)
					}
				} else {
					suppressed = append(suppressed, blk)
				}
			}
			toolUseBlocks = suppressed
			if looped && len(toolUseBlocks) == 0 {
				// 全部被循环检测拦下 — 回到循环让模型重新回复
				continue
			}
		}

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
			if e.Metrics != nil {
				e.Metrics.TurnsTotal.Add(1)
				e.Metrics.TurnsAborted.Add(1)
			}
			e.recordTrajectoryIfNeeded(turnUserIntent, turnPlanMsgs, turnToolSigs, "max_turns", false, turnStart)
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

		toolExecStart := time.Now()
		toolResults := tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, e.HookRunner)
		toolExecLatency := time.Since(toolExecStart)

		// 用于 G6 Trajectory: 记录本批工具签名
		if e.TrajStore != nil {
			for i, blk := range toolUseBlocks {
				ok := true
				if i < len(toolResults) {
					for _, cb := range toolResults[i].Content {
						if cb.Type == types.ContentBlockToolResult && cb.IsError {
							ok = false
							break
						}
					}
				}
				turnToolSigs = append(turnToolSigs, ToolSig{
					Name:      blk.Name,
					InputHash: normalizeHash([]byte(blk.Input)),
					OK:        ok,
					LatencyMs: toolExecLatency.Milliseconds() / int64(maxInt(len(toolUseBlocks), 1)),
					At:        toolExecStart,
				})
			}
		}

		// G7 StopSignalDetector: 观察 tool 结果, 必要时注入 soft stop hint (作为 assistant meta 消息)。
		if e.StopDet != nil {
			for i, blk := range toolUseBlocks {
				var resultSummary string
				if i < len(toolResults) {
					for _, cb := range toolResults[i].Content {
						if cb.Type == types.ContentBlockToolResult {
							resultSummary = cb.Content
							break
						}
					}
				}
				if suggest, reason := e.StopDet.Observe(blk.Name, string(blk.Input), resultSummary); suggest {
					hintMsg := types.Message{
						Type: types.MessageTypeUser,
						UUID: generateUUID(),
						Content: []types.ContentBlock{{
							Type: types.ContentBlockText,
							Text: BuildHintMessage(reason),
						}},
						IsMeta:    true,
						CreatedAt: time.Now(),
					}
					messages = append(messages, hintMsg)
					if e.Metrics != nil {
						e.Metrics.StopSuggestionsEmitted.Add(1)
					}
					break // 一轮至多注入一次, 避免膨胀
				}
			}
		}

		if e.Metrics != nil {
			e.Metrics.ToolCallsTotal.Add(int64(len(toolUseBlocks)))
		}

		for _, result := range toolResults {
			ch <- result
			messages = append(messages, result)
			if streamCh != nil {
				for _, cb := range result.Content {
					if cb.Type == types.ContentBlockToolResult {
						summary := cb.Content
						if len(summary) > 200 {
							summary = summary[:200] + "..."
						}
						streamCh <- types.StreamEvent{
							Kind:       types.StreamEventToolDone,
							ToolResult: summary,
						}
					}
				}
			}
		}
	}
}

// recordTrajectoryIfNeeded 在 turn 结束或被中止时写入一次轨迹。
// 调用约定: 每条 return 路径上都应调用一次 (成功 return 在 Phase 5a 已在 stop hooks 分支处理)。
// 为避免过多出口改动, 这里做幂等保护: TrajStore 为 nil 时直接返回。
func (e *QueryEngine) recordTrajectoryIfNeeded(intent string, planMsgs []types.Message, tools []ToolSig, stopReason string, aborted bool, start time.Time) {
	if e == nil || e.TrajStore == nil {
		return
	}
	verdict := InferVerdict(stopReason, tools, aborted)
	sess := ""
	if e.SessionStore != nil {
		sess = e.SessionStore.SessionID()
	}
	plan := ExtractPlan(planMsgs)
	t := &Trajectory{
		TurnID:     generateUUID(),
		SessionID:  sess,
		UserIntent: intent,
		Plan:       plan,
		ToolCalls:  tools,
		StopReason: stopReason,
		Verdict:    verdict,
		At:         time.Now(),
		LatencyMs:  time.Since(start).Milliseconds(),
	}
	e.TrajStore.Append(t)
	if e.Metrics != nil {
		switch verdict {
		case VerdictSuccess:
			e.Metrics.TrajSuccessRecorded.Add(1)
		case VerdictFail:
			e.Metrics.TrajFailRecorded.Add(1)
		}
	}
}

// extractLatestUserIntent 从消息尾部反向找到最近的用户自然语言意图。
func extractLatestUserIntent(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Type != types.MessageTypeUser {
			continue
		}
		for _, b := range messages[i].Content {
			if b.Type == types.ContentBlockText && b.Text != "" {
				return truncateStr(b.Text, 400)
			}
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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

// RecentMessages 返回最近 N 条消息（从尾部取）。
func (e *QueryEngine) RecentMessages(n int) []types.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n <= 0 || len(e.Messages) == 0 {
		return nil
	}
	start := len(e.Messages) - n
	if start < 0 {
		start = 0
	}
	result := make([]types.Message, len(e.Messages)-start)
	copy(result, e.Messages[start:])
	return result
}

// ClearMessages 清空对话历史
func (e *QueryEngine) ClearMessages() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Messages = nil
}
