// Package engine 实现核心查询引擎和 ReAct 循环。
// 对应 TS 源码: review/claude/src/QueryEngine.ts + review/claude/src/query.ts
//
// 这是 Claude Code 客户端的心脏。QueryEngine 管理对话状态，
// queryLoop 是一个 while(true) 循环:
//  1. (可选) auto-compact / micro-compact / snip
//  2. 调用模型 API (流式)
//  3. 解析 assistant 响应中的 tool_use 块
//  4. 如果无 tool_use → 执行 stop hooks → 返回
//  5. 如果有 tool_use → 执行工具 (partitionToolCalls) → 追加结果 → 继续循环
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
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// 断路器常量已迁移到 internal_hook 包: MaxConsecutiveErrors, MicroCompactMaxChars, MessageCompactChars

// QueryEngine 查询引擎，管理整个对话循环。
// 对应 TS: QueryEngine.ts 中的 class QueryEngine。
//
// 使用方式:
//
//	engine := NewQueryEngine(config)
//	engine.EnableFrontierOptimizations()  // 可选: 启用前沿模型优化组件
//	resultCh := engine.SubmitMessage(ctx, userMessage)
//	for msg := range resultCh { ... }
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
	FactStore    *memory.FactStore     // V3 Anti-Amnesia: L2 结构化记忆 (可选)
	Ingestor     *memory.Ingestor      // V3: 记忆摄入器 (可选)
	SessionStore SessionStoreInterface // 会话持久化 (可选, nil 则不启用)

	// SnapshotFn 每轮末的"整份历史"快照回调 (可选, nil 则不落盘)。
	//
	// 与 SessionStore 的分工: SessionStore 是逐条追加的 transcript (且历史上只写
	// user 文本与 assistant 消息, tool_result 从不落盘); SnapshotFn 拿到的是该轮
	// 结束时的完整 Messages —— 已过 AutoCompact/MicroCompact/BudgetDegrade,
	// 天然自截断且含 tool_result。飞书会话续聊走这条 (design/02 §七 R2)。
	//
	// 调用纪律 (实现方与调用点都必须守):
	//   - 每轮恰好一次, 在 e.Messages 更新之后;
	//   - 参数是该轮的局部 finalMsgs, 不是 e.Messages (后者需持锁, 且下一轮会改写它);
	//   - fail-open: 实现内部自行计数并吞掉错误, 绝不能阻塞或失败掉用户回复。
	SnapshotFn func([]types.Message)

	// ========== 前沿模型优化组件 (全部可选, nil 则走基线行为) ==========
	// 设计依据: docs/query-engine-frontier-optimization.md
	// 开关由 Config.Enable* 字段控制, 默认通过 EnableFrontierOptimizations() 统一启用。
	Metrics       *internal_hook.EngineMetrics      // G10 共享指标
	PromptCache   *internal_hook.PromptCacheBuilder // G1 Prompt cache 构建
	Budget        *internal_hook.TokenBudgetManager // G2 Token 预算分级
	LoopDet       *internal_hook.LoopDetector       // G3 工具循环检测
	ErrClassifier *internal_hook.ErrorClassifier    // G4 错误族隔离 budget
	JSONRepair    *internal_hook.JSONRepair         // G5 工具输入 JSON 修复
	TrajStore     internal_hook.TrajectoryStore     // G6 轨迹记忆
	StopDet       *internal_hook.StopSignalDetector // G7 CaRT 停止信号
	TraceStore    *tracestore.Store                 // design/03 §4.1 E1: 轨迹底座 (可选, nil 则不采集)

	// TaskInstruction 动态任务指令，追加到 system prompt 末尾（而非 user message）。
	// 用于 Agent Team 场景：将 20K 任务描述从 msg[0] 移到 system prompt，
	// 使 system prompt 前缀享受 cache，同时避免 user message 重复膨胀。
	TaskInstruction string

	CumulativeUsage types.Usage // 本会话累积 token 消耗
	ContextBudget   int         // 上下文窗口大小 (tokens, 默认 200000)

	// HookChain 内置 Hook 执行链，将 queryLoop 中所有硬编码功能点解耦为 InternalHook。
	HookChain *internal_hook.HookChain

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
	ContextWindow    int    // 模型上下文窗口 (tokens, 0 = 默认 200000)
	Cwd              string // 当前工作目录
	PermissionMode   types.PermissionMode
	IsNonInteractive bool
	Debug            bool
	SessionID        string
	MetricsSource    string // LLM 指标来源: feishu_main / nested_agent / team_stage / cli
	MetricsPurpose   string // 额外维度: chatID / team / stage / compact 等
	Workflow         string // Agent Team 工作流名
	Role             string // Agent 角色名
	// DynamicPlanCheck 动态计划模式检查。
	// 当 LLM 调用 EnterPlanMode 时返回 true, 引擎自动切换到只读权限。
	// 对应 TS: QueryEngine 中 permissionMode 与 PlanModeActive 联动。
	DynamicPlanCheck func() bool
	// DisabledTools 禁用的工具名列表。
	// queryLoop 在构建 API 请求时过滤掉这些工具，防止 LLM 自主调用。
	// 用途: 飞书 processAndReply 场景下禁止 LLM 自主创建/删除团队。
	DisabledTools map[string]bool

	// AllowedTools 工具白名单。语义区分 nil 与空集 (fail-closed):
	//   nil    = 未设置白名单, 全部工具可见 (仅受 DisabledTools 约束);
	//   非 nil = 仅白名单内的工具对模型可见且可执行, 其余一律硬拒 —
	//            **空集 = 全部拒绝**, 而不是不限制。
	// 用途: 受限托管 agent — 平台只放开经 MCP 暴露的工具，Bash/Write 等
	// 内置工具即使在工具表中也无法调用。白名单在会话构建时锁定。
	AllowedTools map[string]bool

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

	// ==================== Advisor push 模式 (设计文档 docs/advisor-tool-design.md 3.5) ====================
	// AdvisorConsultFn 主动咨询 advisor 的函数 (通常是 builtin.AdvisorTool.Consult)。
	// 为 nil 时 push 模式关闭; pull 模式 (advisor 工具注册) 不依赖此字段。
	AdvisorConsultFn internal_hook.AdvisorConsultFn
	// AdvisorCheckpointEveryTurns 每 N 轮无 advisor 介入时 harness 主动咨询 (0=关闭周期触发)。
	AdvisorCheckpointEveryTurns int
	// AdvisorCheckpointOnLoop LoopDetector 触发时联动主动咨询。
	AdvisorCheckpointOnLoop bool
}

// toolExposed reports whether a tool may be shown to the model and executed,
// applying DisabledTools (blacklist) and AllowedTools (whitelist, when set).
// This is the single source of truth mirrored by ToolGateHook at PreToolUse.
func (c *Config) toolExposed(name string) bool {
	if c.DisabledTools != nil && c.DisabledTools[name] {
		return false
	}
	// nil = 无白名单; 非 nil (含空集) = 仅名单内放行, 空集拒绝一切。
	if c.AllowedTools != nil && !c.AllowedTools[name] {
		return false
	}
	return true
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
	contextBudget := cfg.ContextWindow
	if contextBudget <= 0 {
		contextBudget = 200000
	}
	e := &QueryEngine{
		Config:        cfg,
		APIClient:     apiClient,
		Tools:         tools,
		HookRunner:    hookRunner,
		PermChecker:   permChecker,
		Compactor:     compactor,
		PromptMgr:     promptMgr,
		ContextBudget: contextBudget,
	}
	// 按 Config 开关懒加载对应组件。调用 EnableFrontierOptimizations() 可一次性启用 P0/P1。
	e.applyFeatureFlags()
	if e.LoopDet != nil {
		e.LoopDet.Cwd = cfg.Cwd
	}
	return e
}

// EnableFrontierOptimizations 一键启用推荐的 P0/P1 前沿优化组件。
// 详细设计见 docs/query-engine-frontier-optimization.md。
//
// 启用清单:
//
//	P0: Metrics, ErrorClassifier, JSONRepair, LoopDetector
//	P1: PromptCache, Budget, Trajectory (若已配置 MemoryStore)
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

// RefreshHooks 在构造引擎**之后**才赋组件（MemoryStore/FactStore/TraceStore/
// TrajStore 等）的调用方，必须调一次本方法，否则那些带 nil 守卫的内置 hook
// 静默缺席。
//
// 为什么需要它：registerInternalHooks 只在 NewQueryEngine 与
// EnableFrontierOptimizations 里跑，而多个 hook 的注册带守卫——
// MemoryInject 要 MemoryStore/FactStore 非 nil、TraceCapture 要 TraceStore
// 非 nil。CLI 装配路径是「构造 → 赋组件」的顺序，于是曾长期丢掉 L1/L2 记忆
// 注入与轨迹采集两个 hook，而下游平台的全部流量走的正是 CLI headless。
//
// 与 EnableFrontierOptimizations 的区别：后者会顺带把一批 Config.Enable* 翻开
// （metrics/promptcache/budget/loopdetector…），是行为变更；本方法只按**当前**
// 组件状态重建 HookChain，不动任何开关。
//
// 幂等：registerInternalHooks 每次重建 HookChain，重复调用安全。
func (e *QueryEngine) RefreshHooks() {
	if e == nil {
		return
	}
	e.registerInternalHooks()
}

// applyFeatureFlags 根据 Config.Enable* 懒初始化组件实例。
// 幂等: 多次调用安全, 已存在的组件不会被替换。
func (e *QueryEngine) applyFeatureFlags() {
	if e == nil || e.Config == nil {
		return
	}
	if e.Config.EnableMetrics && e.Metrics == nil {
		e.Metrics = internal_hook.NewEngineMetrics()
	}
	if e.Config.EnablePromptCache && e.PromptCache == nil {
		e.PromptCache = internal_hook.NewPromptCacheBuilder()
	}
	if e.Config.EnableBudget && e.Budget == nil {
		e.Budget = internal_hook.NewTokenBudgetManager(e.ContextBudget)
	}
	if e.Config.EnableLoopDetector && e.LoopDet == nil {
		e.LoopDet = internal_hook.NewLoopDetector()
	}
	if e.Config.EnableErrorClassifier && e.ErrClassifier == nil {
		e.ErrClassifier = internal_hook.NewErrorClassifier()
	}
	if e.Config.EnableJSONRepair && e.JSONRepair == nil {
		e.JSONRepair = internal_hook.NewJSONRepair()
	}
	if e.Config.EnableTrajectory && e.TrajStore == nil && e.MemoryStore != nil {
		e.TrajStore = internal_hook.NewMemoryBackedTrajectoryStore(e.MemoryStore)
	}
	if e.Config.EnableStopSignal && e.StopDet == nil {
		e.StopDet = internal_hook.NewStopSignalDetector()
	}
	e.registerInternalHooks()
}

// registerInternalHooks 根据当前引擎组件状态注册所有内置 Hook。
// 幂等：每次调用会重建 HookChain，确保组件懒加载后被正确注册。
func (e *QueryEngine) registerInternalHooks() {
	if e == nil {
		return
	}
	e.HookChain = internal_hook.NewHookChain()

	// PhasePreCompact: AutoCompact(10) + MicroCompact(20) + BudgetDegrade(30)
	if e.Compactor != nil {
		e.HookChain.Register(internal_hook.NewAutoCompactHook(e.Compactor, e.MemoryStore, e.Ingestor, e.HookRunner))
	}
	e.HookChain.Register(internal_hook.NewMicroCompactHook())
	if e.Budget != nil {
		e.HookChain.Register(internal_hook.NewBudgetDegradeHook(e.Budget, e.HookRunner, e.Metrics))
	}

	// PhasePreRequest: ToolResultLevel(38) + MessageFilter(40) + MessageMetrics(48) + MemoryInject(50) + PromptCache(55)
	e.HookChain.Register(internal_hook.NewToolResultLevelHook(nil, e.Metrics))
	e.HookChain.Register(internal_hook.NewMessageFilterHook())
	e.HookChain.Register(internal_hook.NewMessageMetricsHook(e.Metrics))
	if e.MemoryStore != nil || e.FactStore != nil {
		e.HookChain.Register(internal_hook.NewMemoryInjectHook(e.MemoryStore, e.FactStore))
	}
	if e.PromptCache != nil {
		e.HookChain.Register(internal_hook.NewPromptCacheHook(e.PromptCache, e.Metrics))
	}

	// PhasePostRequest: XMLToolFallback(130)
	e.HookChain.Register(internal_hook.NewXMLToolFallbackHook())

	// PhasePreToolUse: JSONRepair(70) + LoopDetectorInput(80) + DisabledTool(150)
	if e.JSONRepair != nil {
		e.HookChain.Register(internal_hook.NewJSONRepairHook(e.JSONRepair, e.Metrics))
	}
	if e.LoopDet != nil {
		e.HookChain.Register(internal_hook.NewLoopDetectorInputHook(e.LoopDet, e.Metrics))
	}
	if len(e.Config.DisabledTools) > 0 || e.Config.AllowedTools != nil {
		if h := internal_hook.NewToolGateHook(e.Config.DisabledTools, e.Config.AllowedTools); h != nil {
			e.HookChain.Register(h)
		}
	}

	// PhasePostToolUse: LoopDetectorResult(90) + AdvisorCheckpoint(95) + StopSignal(100)
	if e.LoopDet != nil {
		e.HookChain.Register(internal_hook.NewLoopDetectorResultHook(e.LoopDet, e.Metrics))
	}
	if h := internal_hook.NewAdvisorCheckpointHook(
		e.Config.AdvisorConsultFn,
		e.Config.AdvisorCheckpointEveryTurns,
		e.Config.AdvisorCheckpointOnLoop,
		e.LoopDet,
		e.Metrics,
	); h != nil {
		e.HookChain.Register(h)
	}
	if e.StopDet != nil {
		e.HookChain.Register(internal_hook.NewStopSignalHook(e.StopDet, e.Metrics))
	}

	// PhaseOnError: ErrorClassifier(110) + CircuitBreaker(120)
	if e.ErrClassifier != nil {
		e.HookChain.Register(internal_hook.NewErrorClassifierHook(e.ErrClassifier, e.Compactor, e.Metrics))
	}
	e.HookChain.Register(internal_hook.NewCircuitBreakerHook(e.ErrClassifier, e.Metrics))

	// PhaseOnStop: MaxTokensRecovery(60)
	e.HookChain.Register(internal_hook.NewMaxTokensRecoveryHook())

	// PhasePostToolUse: MetricsCollect(200)
	e.HookChain.Register(internal_hook.NewMetricsCollectHook(e.Metrics))

	// PhasePostTurn: TurnMetrics(10)
	e.HookChain.Register(internal_hook.NewTurnMetricsHook(e.Metrics, e.SessionStore, e.TrajStore))

	// PhasePostToolUse + PhasePostTurn: TraceCapture (design/03 §4.1 E1)。
	// 仿 MemoryInjectHook 守卫: 仅 TraceStore 非 nil 才采集。
	if e.TraceStore != nil {
		e.HookChain.Register(internal_hook.NewTraceCaptureHook(e.TraceStore))
	}
}

// firePhasePostTurn 触发 PhasePostTurn 内置 hooks（TurnMetricsHook）。
// 在所有 queryLoop 返回路径上统一调用，确保 Turn 级指标和轨迹被记录。
func (e *QueryEngine) firePhasePostTurn(
	ctx context.Context,
	messages []types.Message,
	turnCount int,
	currentModel string,
	stopReason string,
	assistantBlocks []types.ContentBlock,
	turnToolSigs []internal_hook.ToolSig,
	turnUserIntent string,
	turnPlanMsgs []types.Message,
	turnStart time.Time,
	turnAborted bool,
) {
	if e.HookChain == nil {
		return
	}
	postTurnCtx := &internal_hook.HookContext{
		Ctx:             ctx,
		Phase:           internal_hook.PhasePostTurn,
		Messages:        messages,
		TurnCount:       turnCount,
		Model:           currentModel,
		StopReason:      stopReason,
		AssistantBlocks: assistantBlocks,
		TurnToolSigs:    turnToolSigs,
		TurnUserIntent:  turnUserIntent,
		TurnPlanMsgs:    turnPlanMsgs,
		TurnStart:       turnStart,
		TurnAborted:     turnAborted,
	}
	if _, err := e.HookChain.Execute(internal_hook.PhasePostTurn, postTurnCtx); err != nil {
		logging.For("engine").Error("PostTurn hook failed", "err", err)
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
	TotalTokens  int
	InputTokens  int
	OutputTokens int
	Budget       int
	Percentage   float64
	MessageCount int
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
//  1. 将用户消息追加到 Messages
//  2. 组装系统提示词 (PromptMgr)
//  3. 启动 queryLoop (goroutine)
//  4. 通过 channel 返回所有响应消息
func (e *QueryEngine) SubmitMessage(ctx context.Context, userContent string) <-chan types.Message {
	ch := make(chan types.Message, 50)

	e.mu.Lock()
	text := userContent
	if text == "" && e.TaskInstruction != "" {
		text = e.TaskInstruction
	}
	if text == "" {
		text = "."
	}
	userMsg := types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
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

		// 会话历史落盘: 每轮一次, fail-open (详见 SnapshotFn 字段注释)。
		// 读局部 finalMsgs 而非 e.Messages —— 后者要持锁才安全, 且下一轮会 append 它。
		if fn := e.SnapshotFn; fn != nil {
			fn(finalMsgs)
		}
	}()

	return ch
}

// SubmitStream 提交用户消息，返回 StreamEvent 通道实现 token-by-token 流式输出。
func (e *QueryEngine) SubmitStream(ctx context.Context, userContent string) <-chan types.StreamEvent {
	return e.SubmitStreamBlocks(ctx, userContent, nil)
}

// SubmitStreamBlocks 提交带附加内容块 (如图片) 的用户消息。
// extraBlocks 排在文本之前 (Anthropic 推荐图片在前)，用于多模态输入。
func (e *QueryEngine) SubmitStreamBlocks(ctx context.Context, userContent string, extraBlocks []types.ContentBlock) <-chan types.StreamEvent {
	streamCh := make(chan types.StreamEvent, 200)
	msgCh := make(chan types.Message, 50)

	e.mu.Lock()
	text := userContent
	if text == "" && e.TaskInstruction != "" {
		text = e.TaskInstruction
	}
	if text == "" {
		text = "."
	}
	content := make([]types.ContentBlock, 0, len(extraBlocks)+1)
	content = append(content, extraBlocks...)
	content = append(content, types.ContentBlock{Type: types.ContentBlockText, Text: text})
	userMsg := types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		Content:   content,
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

		// 同 SubmitMessage: 每轮一次整份快照, fail-open。
		if fn := e.SnapshotFn; fn != nil {
			fn(finalMsgs)
		}
	}()

	return streamCh
}

// queryLoop 核心 ReAct 循环。
// 对应 TS: query.ts 中的 queryLoop()
//
// 这是整个系统最关键的函数。算法如下:
//
//	while (true) {
//	  // Phase 1: 上下文压缩 (compact)
//	  messages = autoCompact(messages)
//	  messages = microCompact(messages)
//
//	  // Phase 2: 组装提示词
//	  systemPrompt = buildEffectiveSystemPrompt(...)
//
//	  // Phase 3: 调用模型 API (流式)
//	  for event in stream(messages, systemPrompt, tools):
//	    收集 assistant 消息和 tool_use 块
//
//	  // Phase 4: 执行 post-sampling hooks
//	  executePostSamplingHooks(...)
//
//	  // Phase 5a: 如果没有 tool_use → stop hooks → return
//	  if no tool_use:
//	    stopResult = handleStopHooks(...)
//	    if stopResult.blocking → 追加 blocking message → continue
//	    else → return "completed"
//
//	  // Phase 5b: 如果有 tool_use → 执行工具 → 追加结果 → continue
//	  toolResults = runTools(toolUseBlocks, registry, context)
//	  messages = append(messages, assistantMsgs, toolResults)
//	}
func (e *QueryEngine) queryLoop(ctx context.Context, messages []types.Message, ch chan<- types.Message, streamCh chan<- types.StreamEvent) ([]types.Message, types.Terminal) {
	turnCount := 0
	currentModel := e.Config.Model
	consecutiveErrors := 0 // 断路器: 连续错误计数 (ErrClassifier 启用后被绕过)
	stopReason := ""       // 模型停止原因

	// ==================== Trajectory 轨迹追踪状态 ====================
	// 仅在 EnableTrajectory 时有效, 末尾统一写入 TrajStore。
	turnStart := time.Now()
	var turnUserIntent string
	var turnToolSigs []internal_hook.ToolSig
	var turnPlanMsgs []types.Message
	if e.TrajStore != nil {
		turnUserIntent = extractLatestUserIntent(messages)
	}

	for {
		// PreTurn Hook: 单轮开始
		if e.HookRunner != nil {
			hookOut := e.HookRunner.ExecutePreTurnHooks(messages, turnCount)
			if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
				logging.For("engine").Warn("PreTurn blocked by hook", "reason", hookOut.Reason)
				continue
			}
		}

		// ============================================================
		// Phase 0: Abort 检查
		// 对应 TS: queryLoop 顶部 if (abortController.signal.aborted)
		// ============================================================
		if ctx.Err() != nil {
			if e.HookRunner != nil {
				e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
				e.HookRunner.ExecuteOnErrorHooks(messages, "aborted", ctx.Err())
				e.HookRunner.ExecuteStopFailureHooks(messages)
			}
			e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "aborted", nil, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, true)
			return messages, types.Terminal{Reason: "aborted"}
		}

		// ============================================================
		// PhasePreCompact: BudgetDegrade + AutoCompact + MicroCompact
		// ============================================================
		if e.HookChain != nil {
			preCompactCtx := &internal_hook.HookContext{
				Ctx:       ctx,
				Phase:     internal_hook.PhasePreCompact,
				Messages:  messages,
				TurnCount: turnCount,
				Model:     currentModel,
			}
			preCompactResult, err := e.HookChain.Execute(internal_hook.PhasePreCompact, preCompactCtx)
			if err != nil {
				logging.For("engine").Error("PreCompact hook failed", "err", err)
			}
			if preCompactResult != nil {
				if preCompactResult.Messages != nil {
					messages = preCompactResult.Messages
				}
				for _, m := range preCompactResult.AppendMsgs {
					ch <- m
				}
				if preCompactResult.ReturnTerminal != nil {
					if e.HookRunner != nil {
						e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
						e.HookRunner.ExecuteOnErrorHooks(messages, "precompact_terminal", nil)
						e.HookRunner.ExecuteStopFailureHooks(messages)
					}
					e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "precompact_terminal", nil, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
					return messages, *preCompactResult.ReturnTerminal
				}
			}
		}

		// PostCompact external hook
		if e.HookRunner != nil {
			hookOut := e.HookRunner.ExecutePostCompactHooks(messages)
			if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
				logging.For("engine").Warn("PostCompact blocked by hook", "reason", hookOut.Reason)
			}
		}

		// ============================================================
		// Phase 2: 组装系统提示词
		// 集成 G1 PromptCache: 稳定前缀 + 动态后缀 分层, 追踪本地 cache 命中率。
		// ============================================================
		systemPrompt := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)

		// 将动态任务指令追加到 system prompt 末尾（作为最后一个 block）。
		// 这样 system prompt 前缀仍可享受 cache，任务指令不占用 user message 空间。
		if e.TaskInstruction != "" {
			systemPrompt = append(systemPrompt, e.TaskInstruction)
		}

		// PhasePreRequest: MessageFilter + MemoryInject + PromptCache
		if e.HookChain != nil {
			preRequestCtx := &internal_hook.HookContext{
				Ctx:          ctx,
				Phase:        internal_hook.PhasePreRequest,
				Messages:     messages,
				TurnCount:    turnCount,
				Model:        currentModel,
				SystemPrompt: systemPrompt,
			}
			preRequestResult, err := e.HookChain.Execute(internal_hook.PhasePreRequest, preRequestCtx)
			if err != nil {
				logging.For("engine").Error("PreRequest hook failed", "err", err)
			}
			if preRequestResult != nil {
				if preRequestResult.Messages != nil {
					messages = preRequestResult.Messages
				}
				if preRequestResult.SystemPrompt != nil {
					systemPrompt = preRequestResult.SystemPrompt
				}
				for _, m := range preRequestResult.AppendMsgs {
					ch <- m
				}
				if preRequestResult.ReturnTerminal != nil {
					if e.HookRunner != nil {
						e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
						e.HookRunner.ExecuteOnErrorHooks(messages, "prerequest_terminal", nil)
						e.HookRunner.ExecuteStopFailureHooks(messages)
					}
					e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "prerequest_terminal", nil, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
					return messages, *preRequestResult.ReturnTerminal
				}
			}
		}

		// ============================================================
		// Phase 3: 调用模型 API (流式)
		// 对应 TS: while(attemptWithFallback) + for await(callModel(...))
		// ============================================================
		if e.HookRunner != nil {
			hookOut := e.HookRunner.ExecutePreRequestHooks(messages, currentModel)
			if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
				reason := hookOut.Reason
				if reason == "" {
					reason = "PreRequest hook 阻止了模型调用"
				}
				messages = append(messages, types.Message{
					Type: types.MessageTypeUser,
					Content: []types.ContentBlock{{
						Type: types.ContentBlockText,
						Text: fmt.Sprintf("[hook blocked] %s", reason),
					}},
					IsMeta: true,
				})
				continue
			}
		}
		apiMessages := messagesToAPI(messages, e.HookRunner)
		allTools := e.Tools.APITools()
		var apiTools []types.APITool
		if len(e.Config.DisabledTools) > 0 || len(e.Config.AllowedTools) > 0 {
			for _, t := range allTools {
				if e.Config.toolExposed(t.Name) {
					apiTools = append(apiTools, t)
				}
			}
		} else {
			apiTools = allTools
		}

		components := internal_hook.BuildPromptComponentMetrics(systemPrompt, apiTools, messages)
		apiCtx := api.WithLLMMetrics(ctx, api.LLMMetricsContext{
			Source:           e.Config.MetricsSource,
			Purpose:          e.Config.MetricsPurpose,
			Workflow:         e.Config.Workflow,
			Role:             e.Config.Role,
			PromptComponents: components,
		})
		// trace 四元组 TurnID (design/03 §4.1 E0): 本轮内的 LLM 调用(含重试)共享同一 TurnID。
		apiCtx = trace.With(apiCtx, trace.IDs{TurnID: fmt.Sprintf("t%d", turnCount)})

		apiCallStart := time.Now()
		eventCh, errCh := e.APIClient.StreamMessage(apiCtx, apiMessages, systemPrompt, apiTools, e.Config.MaxTokens)

		var assistantBlocks []types.ContentBlock
		var toolUseBlocks []types.ContentBlock
		var currentText strings.Builder
		var currentToolInput strings.Builder
		var currentBlock *types.ContentBlock
		var usage *types.Usage
		var firstTokenRecorded bool
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
				if !firstTokenRecorded {
					firstTokenRecorded = true
					if e.Metrics != nil {
						e.Metrics.RecordFirstTokenLatency(currentModel, time.Since(apiCallStart))
					}
				}
				if event.Delta != nil {
					switch event.Delta.Type {
					case "text_delta":
						currentText.WriteString(event.Delta.Text)
						skipStream := false
						if e.HookRunner != nil {
							hookOut := e.HookRunner.ExecuteOnChunkHooks(event.Delta.Text, event.Index, false)
							if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
								logging.For("engine").Warn("OnChunk blocked stream delta by hook", "reason", hookOut.Reason)
								skipStream = true
							} else {
								hookOut = e.HookRunner.ExecuteOnTokenStreamHooks(event.Delta.Text, event.Index)
								if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
									logging.For("engine").Warn("OnTokenStream blocked stream delta by hook", "reason", hookOut.Reason)
									skipStream = true
								}
							}
						}
						if streamCh != nil && !skipStream {
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
						skipStream := false
						if e.HookRunner != nil {
							hookOut := e.HookRunner.ExecuteOnChunkHooks(event.Delta.Thinking, event.Index, true)
							if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
								logging.For("engine").Warn("OnChunk blocked thinking delta by hook", "reason", hookOut.Reason)
								skipStream = true
							}
						}
						if streamCh != nil && !skipStream {
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

		// PostRequest Hook: API 调用完成（无论成功或失败，都在错误处理前触发）
		if e.HookRunner != nil {
			e.HookRunner.ExecutePostRequestHooks(messages, currentModel)
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
						e.Metrics.RecordError(int(internal_hook.ErrFamilyPTL))
					}
					skipRetry := false
					if e.HookRunner != nil {
						hookOut := e.HookRunner.ExecuteOnRecoveryHooks(messages, "ptl_reactive_compact")
						if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
							logging.For("engine").Warn("OnRecovery blocked ptl retry by hook", "reason", hookOut.Reason)
							skipRetry = true
						}
						hookOut = e.HookRunner.ExecuteOnRetryHooks(messages, "ptl_reactive_compact", 0)
						if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
							logging.For("engine").Warn("OnRetry blocked ptl retry by hook", "reason", hookOut.Reason)
							skipRetry = true
						}
					}
					if !skipRetry {
						continue // 压缩后重试
					}
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
				skipRetry := false
				if e.HookRunner != nil {
					hookOut := e.HookRunner.ExecuteOnRecoveryHooks(messages, "fallback_model_switch")
					if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
						logging.For("engine").Warn("OnRecovery blocked fallback by hook", "reason", hookOut.Reason)
						skipRetry = true
					}
					hookOut = e.HookRunner.ExecuteOnRetryHooks(messages, "fallback_model_switch", 0)
					if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
						logging.For("engine").Warn("OnRetry blocked fallback by hook", "reason", hookOut.Reason)
						skipRetry = true
					}
				}
				if !skipRetry {
					continue
				}
			}

			// PhaseOnError: ErrorClassifier + CircuitBreaker
			if e.HookChain != nil {
				errorCtx := &internal_hook.HookContext{
					Ctx:               ctx,
					Phase:             internal_hook.PhaseOnError,
					Messages:          messages,
					TurnCount:         turnCount,
					Model:             currentModel,
					Error:             streamErr,
					ConsecutiveErrors: consecutiveErrors,
				}
				errorResult, err := e.HookChain.Execute(internal_hook.PhaseOnError, errorCtx)
				if err != nil {
					logging.For("engine").Error("OnError hook failed", "err", err)
				}
				if errorResult != nil {
					if errorResult.Messages != nil {
						messages = errorResult.Messages
					}
					for _, m := range errorResult.AppendMsgs {
						ch <- m
					}
					for _, ev := range errorResult.StreamEvents {
						if streamCh != nil {
							streamCh <- ev
						}
					}
					if errorResult.ReturnTerminal != nil {
						if e.HookRunner != nil {
							e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
							e.HookRunner.ExecuteOnErrorHooks(messages, "error_family_exhausted", streamErr)
							e.HookRunner.ExecuteStopFailureHooks(messages)
						}
						e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "error", assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
						return messages, *errorResult.ReturnTerminal
					}
					if errorResult.Backoff > 0 {
						select {
						case <-time.After(errorResult.Backoff):
						case <-ctx.Done():
							if e.HookRunner != nil {
								e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
								e.HookRunner.ExecuteOnErrorHooks(messages, "aborted", ctx.Err())
								e.HookRunner.ExecuteStopFailureHooks(messages)
							}
							e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "aborted", assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, true)
							return messages, types.Terminal{Reason: "aborted"}
						}
					}
					if errorResult.InjectContinue {
						continue
					}
				}
			}

			// ---- 原始路径 (ErrorClassifier 未启用且 CircuitBreaker 未触发) ----
			consecutiveErrors++
			if consecutiveErrors >= internal_hook.MaxConsecutiveErrors {
				// 理论上 CircuitBreakerHook 已处理此情况，保留兜底
				consecutiveErrors = 0
			}

			// Withheld error: 将错误作为 assistant 消息暂存, 继续循环等待恢复
			// 对应 TS: withheld error 在特定条件下不立即终止
			errMsg := types.Message{
				Type: types.MessageTypeAssistant,
				UUID: internal_hook.GenerateUUID(),
				Content: []types.ContentBlock{{
					Type: types.ContentBlockText,
					Text: fmt.Sprintf("API Error (attempt %d/%d): %v", consecutiveErrors, internal_hook.MaxConsecutiveErrors, streamErr),
				}},
				IsApiErrorMessage: true,
				CreatedAt:         time.Now(),
			}
			ch <- errMsg

			// overloaded 错误时短暂等待后重试
			var overloadErr *api.OverloadedError
			if errors.As(streamErr, &overloadErr) {
				backoff := time.Duration(consecutiveErrors) * 2 * time.Second
				skipRetry := false
				if e.HookRunner != nil {
					hookOut := e.HookRunner.ExecuteOnRateLimitHooks(streamErr, backoff)
					if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
						logging.For("engine").Warn("OnRateLimit blocked retry by hook", "reason", hookOut.Reason)
						skipRetry = true
					}
					hookOut = e.HookRunner.ExecuteOnRetryHooks(messages, "overloaded", consecutiveErrors)
					if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
						logging.For("engine").Warn("OnRetry blocked overloaded retry by hook", "reason", hookOut.Reason)
						skipRetry = true
					}
				}
				if !skipRetry {
					time.Sleep(backoff)
					continue
				}
			}

			if e.HookRunner != nil {
				e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
				e.HookRunner.ExecuteOnErrorHooks(messages, "model_error", streamErr)
				e.HookRunner.ExecuteStopFailureHooks(messages)
			}
			e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "model_error", assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
			return messages, types.Terminal{Reason: "model_error", Error: streamErr}
		}

		// 成功收到响应, 重置断路器
		consecutiveErrors = 0
		if e.ErrClassifier != nil {
			e.ErrClassifier.ResetAll()
		}

		// 检查 context 是否被取消
		if ctx.Err() != nil {
			if e.HookRunner != nil {
				e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
				e.HookRunner.ExecuteOnErrorHooks(messages, "aborted_streaming", ctx.Err())
				e.HookRunner.ExecuteStopFailureHooks(messages)
			}
			e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "aborted_streaming", assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, true)
			return messages, types.Terminal{Reason: "aborted_streaming"}
		}

		// PhasePostRequest: XML 风格工具调用回退解析
		if e.HookChain != nil {
			postRequestCtx := &internal_hook.HookContext{
				Ctx:             ctx,
				Phase:           internal_hook.PhasePostRequest,
				Messages:        messages,
				TurnCount:       turnCount,
				Model:           currentModel,
				SystemPrompt:    systemPrompt,
				AssistantBlocks: assistantBlocks,
				ToolUseBlocks:   toolUseBlocks,
			}
			postRequestResult, err := e.HookChain.Execute(internal_hook.PhasePostRequest, postRequestCtx)
			if err != nil {
				logging.For("engine").Error("PostRequest hook failed", "err", err)
			}
			if postRequestResult != nil {
				if postRequestResult.AssistantBlocks != nil {
					assistantBlocks = postRequestResult.AssistantBlocks
				}
				if postRequestResult.ToolUseBlocks != nil {
					toolUseBlocks = postRequestResult.ToolUseBlocks
					if len(postRequestResult.ToolUseBlocks) > len(postRequestCtx.ToolUseBlocks) {
						stopReason = string(types.StopReasonToolUse)
					}
				}
				for _, ev := range postRequestResult.StreamEvents {
					if streamCh != nil {
						streamCh <- ev
					}
				}
			}
		}

		// ============================================================
		// 构建 assistant 消息
		// ============================================================
		if len(assistantBlocks) > 0 {
			assistantMsg := types.Message{
				Type:       types.MessageTypeAssistant,
				UUID:       internal_hook.GenerateUUID(),
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

		// PhaseOnStop: max_output_tokens 恢复 + Stop hooks
		if e.HookChain != nil {
			stopCtx := &internal_hook.HookContext{
				Ctx:             ctx,
				Phase:           internal_hook.PhaseOnStop,
				Messages:        messages,
				TurnCount:       turnCount,
				Model:           currentModel,
				StopReason:      stopReason,
				AssistantBlocks: assistantBlocks,
			}
			stopResult, err := e.HookChain.Execute(internal_hook.PhaseOnStop, stopCtx)
			if err != nil {
				logging.For("engine").Error("OnStop hook failed", "err", err)
			}
			if stopResult != nil {
				if stopResult.InjectContinue {
					if stopResult.ContinueMsg != nil {
						messages = append(messages, *stopResult.ContinueMsg)
					}
					continue
				}
				if stopResult.ReturnTerminal != nil {
					return messages, *stopResult.ReturnTerminal
				}
			}
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
			if e.HookRunner != nil {
				e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
			}
			e.firePhasePostTurn(ctx, messages, turnCount, currentModel, stopReason, assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
			return messages, types.Terminal{Reason: "completed"}
		}

		// ============================================================
		// Phase 5b: 有 tool_use → 执行工具
		// ============================================================

		// PhasePreToolUse: JSONRepair + LoopDetector input + DisabledTools
		if e.HookChain != nil {
			preToolCtx := &internal_hook.HookContext{
				Ctx:           ctx,
				Phase:         internal_hook.PhasePreToolUse,
				Messages:      messages,
				TurnCount:     turnCount,
				Model:         currentModel,
				ToolUseBlocks: toolUseBlocks,
			}
			preToolResult, err := e.HookChain.Execute(internal_hook.PhasePreToolUse, preToolCtx)
			if err != nil {
				logging.For("engine").Error("PreToolUse hook failed", "err", err)
			}
			if preToolResult != nil {
				if preToolResult.ToolUseBlocks != nil {
					toolUseBlocks = preToolResult.ToolUseBlocks
				}
				for _, m := range preToolResult.AppendMsgs {
					ch <- m
					messages = append(messages, m)
				}
				if preToolResult.InjectContinue && len(toolUseBlocks) == 0 {
					continue
				}
			}
		}

		// maxTurns 检查 (在 tool 执行后检查, 对应 TS)
		turnCount++
		if e.Config.MaxTurns > 0 && turnCount > e.Config.MaxTurns {
			if e.HookRunner != nil {
				hookOut := e.HookRunner.ExecuteOnMaxTurnsReachedHooks(messages, turnCount)
				if hookOut != nil && (hookOut.ContinueDecision == "block" || hookOut.ContinueDecision == "deny") {
					reason := hookOut.Reason
					if reason == "" {
						reason = "OnMaxTurnsReached hook 要求继续对话"
					}
					messages = append(messages, types.Message{
						Type: types.MessageTypeUser,
						Content: []types.ContentBlock{{
							Type: types.ContentBlockText,
							Text: reason,
						}},
						IsMeta: true,
					})
					turnCount = 0
					continue
				}
				e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
			}
			e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "max_turns", assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
			return messages, types.Terminal{Reason: "max_turns"}
		}

		// 动态 Plan Mode 联动: LLM 调用 EnterPlanMode → 自动切换只读
		effectivePerm := e.Config.PermissionMode
		if e.Config.DynamicPlanCheck != nil && e.Config.DynamicPlanCheck() {
			effectivePerm = types.PermissionModePlan
		}

		tctx := &tool.ToolContext{
			Cwd:                e.Config.Cwd,
			PermissionMode:     effectivePerm,
			MainLoopModel:      currentModel,
			IsNonInteractive:   e.Config.IsNonInteractive,
			Debug:              e.Config.Debug,
			Messages:           messages,
			MaxToolResultChars: internal_hook.MicroCompactMaxChars,
			GlobalPerm:         e.PermChecker,
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
				turnToolSigs = append(turnToolSigs, internal_hook.ToolSig{
					Name:      blk.Name,
					InputHash: internal_hook.NormalizeHash([]byte(blk.Input)),
					OK:        ok,
					LatencyMs: toolExecLatency.Milliseconds() / int64(maxInt(len(toolUseBlocks), 1)),
					At:        toolExecStart,
				})
			}
		}

		// PhasePostToolUse: LoopDetector result + StopSignal
		if e.HookChain != nil {
			postToolCtx := &internal_hook.HookContext{
				Ctx:           ctx,
				Phase:         internal_hook.PhasePostToolUse,
				Messages:      messages,
				TurnCount:     turnCount,
				Model:         currentModel,
				ToolUseBlocks: toolUseBlocks,
				ToolResults:   toolResults,
			}
			postToolResult, err := e.HookChain.Execute(internal_hook.PhasePostToolUse, postToolCtx)
			if err != nil {
				logging.For("engine").Error("PostToolUse hook failed", "err", err)
			}
			if postToolResult != nil {
				for _, m := range postToolResult.AppendMsgs {
					messages = append(messages, m)
				}
			}
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

		// PostTurn Hook: 单轮正常结束，即将进入下一轮
		if e.HookRunner != nil {
			e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
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
				return internal_hook.TruncateStr(b.Text, 400)
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

// sanitizeToolPairing 移除"孤儿"工具块: 保证每个 tool_use 都有对应 tool_result, 反之亦然。
// 消息过滤/预算降级/压缩可能破坏 tool_use↔tool_result 配对, 严格网关 (Kimi/OpenAI 协议) 会 400。
func sanitizeToolPairing(messages []types.Message) []types.Message {
	toolUseIDs := make(map[string]bool)
	toolResultIDs := make(map[string]bool)
	for _, m := range messages {
		for _, b := range m.Content {
			switch b.Type {
			case types.ContentBlockToolUse:
				if b.ID != "" {
					toolUseIDs[b.ID] = true
				}
			case types.ContentBlockToolResult:
				if b.ToolUseID != "" {
					toolResultIDs[b.ToolUseID] = true
				}
			}
		}
	}
	out := make([]types.Message, 0, len(messages))
	for _, m := range messages {
		kept := make([]types.ContentBlock, 0, len(m.Content))
		for _, b := range m.Content {
			switch b.Type {
			case types.ContentBlockToolUse:
				if b.ID == "" || !toolResultIDs[b.ID] {
					continue // 无对应 tool_result, 丢弃孤儿 tool_use
				}
			case types.ContentBlockToolResult:
				if b.ToolUseID == "" || !toolUseIDs[b.ToolUseID] {
					continue // 无对应 tool_use, 丢弃孤儿 tool_result
				}
				// 修复: 工具返回空内容时, 严格网关 (Kimi/OpenAI 翻译层) 会因
				// "message ... must not be empty" 报 400, 导致 agent 在 WebSearch
				// 等循环里反复撞 400 烧 token。补占位符保证 tool_result 非空。
				if strings.TrimSpace(b.Content) == "" {
					b.Content = "(工具无输出)"
				}
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			continue // 整条消息只剩孤儿块, 整条丢弃
		}
		// tool_result 块必须位于 user 消息开头: Anthropic 硬性要求, 且严格网关 (Kimi/OpenAI 翻译层)
		// 要求 assistant 的 tool_calls 紧跟 tool 响应, 否则报 "tool_calls must be followed by tool messages"。
		if m.Type == types.MessageTypeUser {
			var results, others []types.ContentBlock
			for _, b := range kept {
				if b.Type == types.ContentBlockToolResult {
					results = append(results, b)
				} else {
					others = append(others, b)
				}
			}
			if len(results) > 0 && len(others) > 0 {
				kept = append(results, others...)
			}
		}
		m.Content = kept
		out = append(out, m)
	}
	return out
}

// messagesToAPI 将内部 Message 格式转换为 API 格式。
// 对应 TS: utils/messages.ts 中的 normalizeMessagesForAPI()
//
// 关键逻辑: 合并连续同角色的消息。
// Anthropic API 要求 user/assistant 严格交替。多个 tool_result
// 作为独立 user 消息存在时，必须合并到同一个 user 消息中。
func messagesToAPI(messages []types.Message, hookRunner *hooks.Runner) []types.APIMessage {
	// OnMessageFilter Hook: 消息过滤前观测点
	// OnMessageFilter Hook: 消息过滤前观测点
	if hookRunner != nil {
		hookOut := hookRunner.ExecuteOnMessageFilterHooks(messages)
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			logging.For("engine").Warn("OnMessageFilter blocked API request by hook", "reason", hookOut.Reason)
			return nil
		}
	}

	// 优化1: 过滤不含 reasoning 的古老 assistant + tool_result 原子单元
	messages = internal_hook.FilterPureToolUseUnits(messages, 6)

	// 优化1b: 修复工具配对 —— 过滤/压缩可能产生孤儿 tool_use 或 tool_result,
	// Anthropic 原生 API 容忍, 但 Kimi 等严格网关 (OpenAI 协议翻译层) 会 400
	// ("tool_calls must be followed by tool messages" / "role 'tool' must be a response...").
	messages = sanitizeToolPairing(messages)

	var result []types.APIMessage

	for i, msg := range messages {
		role := ""
		switch msg.Type {
		case types.MessageTypeUser:
			role = "user"
		case types.MessageTypeAssistant:
			role = "assistant"
		default:
			continue
		}

		// 优化5: 对古老的 user tool_result 做轻量级内容压缩
		if role == "user" && i < len(messages)-4 {
			msg = internal_hook.CompressMessageContent(msg)
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

	// 合并后再做一次 tool_result 前置: 注入的提示/文本可能夹在 assistant(tool_use) 与
	// tool_result 之间, 合并连续 user 消息后变成 [text, tool_result, ...]。Anthropic 容忍,
	// 但 Kimi 等严格网关要求 assistant 的 tool_calls 后紧跟 tool 响应, 否则 400。
	// 这里保证每条 user 消息的 tool_result 块都排在最前面。
	for i := range result {
		if result[i].Role != "user" {
			continue
		}
		var blocks []types.ContentBlock
		if err := json.Unmarshal(result[i].Content, &blocks); err != nil {
			continue
		}
		var res, oth []types.ContentBlock
		for _, b := range blocks {
			if b.Type == types.ContentBlockToolResult {
				res = append(res, b)
			} else {
				oth = append(oth, b)
			}
		}
		if len(res) > 0 && len(oth) > 0 {
			if reordered, err := json.Marshal(append(res, oth...)); err == nil {
				result[i].Content = reordered
			}
		}
	}

	return result
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
