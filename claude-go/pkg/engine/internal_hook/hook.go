// internal/hook.go — 内置 Hook (InternalHook) 核心接口与执行链。
//
// 设计目标：将 engine.go queryLoop 中所有硬编码功能点（如 MicroCompact、
// LoopDetector、JSONRepair 等）解耦为独立的 InternalHook 实现，通过 HookChain
// 统一调度。queryLoop 中不再有任何 `if e.XXX != nil` 判断。
//
// InternalHook 与外部 Hook (hooks.Runner) 的区别：
//   - InternalHook: 引擎内部功能组件，编译期确定，通过 Priority 排序
//   - 外部 Hook: 用户自定义逻辑，运行时配置，通过 HookEvent 触发
package internal_hook

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// InternalHookPhase — 内置 Hook 执行阶段
// ============================================================================

// InternalHookPhase 定义内置 Hook 的执行阶段。
// 每个阶段对应 queryLoop 中一个原硬编码功能插槽。
type InternalHookPhase int

const (
	PhasePreCompact InternalHookPhase = iota  // AutoCompact / MicroCompact / BudgetDegrade
	PhasePostCompact                           // PostCompact 外部 hook 调用
	PhasePreRequest                            // 记忆注入 / PromptCache / MessageFilter
	PhasePostRequest                           // XML 回退解析 / PostRequest 外部 hook
	PhaseOnStreamDelta                         // OnChunk / OnTokenStream
	PhasePreToolUse                            // JSONRepair / LoopDetector(输入) / DisabledTools
	PhasePostToolUse                           // LoopDetector(产出) / StopSignal
	PhaseOnError                               // ErrorClassifier / CircuitBreaker
	PhaseOnRecovery                            // PTL reactive compact / fallback
	PhaseOnStop                                // max_tokens 恢复 / Stop 外部 hook
	PhasePostTurn                              // Turn 结束：指标 + 轨迹
)

// String 返回阶段的可读名称。
func (p InternalHookPhase) String() string {
	switch p {
	case PhasePreCompact:
		return "PreCompact"
	case PhasePostCompact:
		return "PostCompact"
	case PhasePreRequest:
		return "PreRequest"
	case PhasePostRequest:
		return "PostRequest"
	case PhaseOnStreamDelta:
		return "OnStreamDelta"
	case PhasePreToolUse:
		return "PreToolUse"
	case PhasePostToolUse:
		return "PostToolUse"
	case PhaseOnError:
		return "OnError"
	case PhaseOnRecovery:
		return "OnRecovery"
	case PhaseOnStop:
		return "OnStop"
	case PhasePostTurn:
		return "PostTurn"
	default:
		return fmt.Sprintf("Phase(%d)", p)
	}
}

// ============================================================================
// HookContext — 内置 Hook 执行上下文
// ============================================================================

// HookContext 每个 phase 只填充该 phase 相关的字段，其余为零值。
type HookContext struct {
	Ctx context.Context

	// 基础上下文
	Phase     InternalHookPhase
	Messages  []types.Message
	TurnCount int
	Model     string

	// PhasePreCompact / PhasePostRequest / PhasePreRequest 使用
	SystemPrompt []string

	// PhasePostRequest / PhasePreToolUse / PhasePostToolUse 使用
	AssistantBlocks []types.ContentBlock
	ToolUseBlocks   []types.ContentBlock
	ToolResults     []types.Message // PhasePostToolUse 使用

	// PhaseOnStreamDelta 使用
	StreamDelta string
	BlockIndex  int
	IsThinking  bool

	// PhaseOnError / PhaseOnRecovery 使用
	Error  error
	Reason string

	// PhaseOnStop / PhasePostTurn 使用
	StopReason  string
	TurnAborted bool

	// 额外状态（由前置 hook 传递）
	AssistantMsg *types.Message
	Usage        *types.Usage

	// 跨 phase 状态（由 queryLoop 维护）
	ConsecutiveErrors int       // CircuitBreaker 用
	ToolExecStart     time.Time // Trajectory 用
	TurnToolSigs      []ToolSig // Trajectory 用
	TurnUserIntent    string    // Trajectory 用
	TurnPlanMsgs      []types.Message // Trajectory 用
	TurnStart         time.Time // Trajectory 用
}

// ============================================================================
// HookResult — 内置 Hook 执行结果
// ============================================================================

// HookResult 表示 hook 只返回自己产生的增量，HookChain 负责合并。
type HookResult struct {
	// Messages 替换当前 messages（如 Compact 后的新列表）
	Messages []types.Message

	// AppendMsgs 追加到 messages 末尾（如 LoopDetector 注入的提示）
	AppendMsgs []types.Message

	// SystemPrompt 替换 system prompt（如记忆注入）
	SystemPrompt []string

	// ToolUseBlocks 替换 tool_use 列表（如 LoopDetector 拦截后）
	ToolUseBlocks []types.ContentBlock

	// AssistantBlocks 替换 assistant blocks（如 XML 回退解析后）
	AssistantBlocks []types.ContentBlock

	// SkipStreamDelta 跳过该流式 delta（OnStreamDelta phase）
	SkipStreamDelta bool

	// SkipRemaining 跳过后续同 phase hooks
	SkipRemaining bool

	// InjectContinue 注入 continue 消息进入下一轮（如 max_tokens 恢复）
	InjectContinue bool

	// ContinueMsg 自定义 continue 消息（为空则使用默认）
	ContinueMsg *types.Message

	// ReturnTerminal 直接返回终止结果
	ReturnTerminal *types.Terminal

	// StreamEvents 额外流式事件推送
	StreamEvents []types.StreamEvent

	// Backoff 非致命错误后的退避时间（OnError phase）
	Backoff time.Duration
}

// ============================================================================
// InternalHook — 内置 Hook 接口
// ============================================================================

// InternalHook 所有硬编码功能拆分为独立的 InternalHook 实现。
type InternalHook interface {
	Name() string
	Priority() int
	Phases() []InternalHookPhase
	Execute(ctx *HookContext) (*HookResult, error)
}

// BaseInternalHook 提供默认实现，方便具体 hook 嵌入。
type BaseInternalHook struct {
	Name_     string
	Priority_ int
	Phases_   []InternalHookPhase
}

func (h *BaseInternalHook) Name() string                { return h.Name_ }
func (h *BaseInternalHook) Priority() int               { return h.Priority_ }
func (h *BaseInternalHook) Phases() []InternalHookPhase { return h.Phases_ }

// ============================================================================
// HookChain — 内置 Hook 执行链
// ============================================================================

// HookChain 按阶段管理 InternalHook，负责排序和执行合并。
type HookChain struct {
	hooks map[InternalHookPhase][]InternalHook
}

// NewHookChain 创建空的 HookChain。
func NewHookChain() *HookChain {
	return &HookChain{hooks: make(map[InternalHookPhase][]InternalHook)}
}

// Register 注册一个内置 Hook。
// 同一 phase 按 Priority 升序排序（值越小越先执行）。
func (c *HookChain) Register(hook InternalHook) {
	for _, phase := range hook.Phases() {
		c.hooks[phase] = append(c.hooks[phase], hook)
		sort.Slice(c.hooks[phase], func(i, j int) bool {
			return c.hooks[phase][i].Priority() < c.hooks[phase][j].Priority()
		})
	}
}

// Execute 执行指定 phase 的所有已注册 hooks。
//
// 合并规则（按优先级顺序）：
//   - Messages: 后执行的覆盖先执行的
//   - AppendMsgs: 累积追加
//   - SystemPrompt: 后执行的覆盖
//   - ToolUseBlocks / AssistantBlocks: 后执行的覆盖
//   - SkipStreamDelta: 任一 true 即 true
//   - SkipRemaining: 遇到 true 立即终止后续 hooks
//   - InjectContinue: 任一 true 即 true
//   - ContinueMsg: 后执行的覆盖
//   - ReturnTerminal: 遇到非空立即终止并返回
//   - StreamEvents: 累积
//   - Backoff: 取最大值
func (c *HookChain) Execute(phase InternalHookPhase, ctx *HookContext) (*HookResult, error) {
	hooks := c.hooks[phase]
	if len(hooks) == 0 {
		return nil, nil
	}

	var result *HookResult
	messages := ctx.Messages
	systemPrompt := ctx.SystemPrompt

	for _, hook := range hooks {
		ctx.Messages = messages
		ctx.SystemPrompt = systemPrompt

		r, err := hook.Execute(ctx)
		if err != nil {
			return nil, fmt.Errorf("internal hook %s: %w", hook.Name(), err)
		}
		if r == nil {
			continue
		}

		if result == nil {
			result = &HookResult{}
		}

		if r.Messages != nil {
			result.Messages = r.Messages
			messages = r.Messages
		}
		if len(r.AppendMsgs) > 0 {
			result.AppendMsgs = append(result.AppendMsgs, r.AppendMsgs...)
			messages = append(messages, r.AppendMsgs...)
		}
		if r.SystemPrompt != nil {
			result.SystemPrompt = r.SystemPrompt
			systemPrompt = r.SystemPrompt
		}
		if r.ToolUseBlocks != nil {
			result.ToolUseBlocks = r.ToolUseBlocks
		}
		if r.AssistantBlocks != nil {
			result.AssistantBlocks = r.AssistantBlocks
		}
		if r.SkipStreamDelta {
			result.SkipStreamDelta = true
		}
		if r.InjectContinue {
			result.InjectContinue = true
		}
		if r.ContinueMsg != nil {
			result.ContinueMsg = r.ContinueMsg
		}
		if len(r.StreamEvents) > 0 {
			result.StreamEvents = append(result.StreamEvents, r.StreamEvents...)
		}
		if r.Backoff > result.Backoff {
			result.Backoff = r.Backoff
		}
		if r.ReturnTerminal != nil {
			result.ReturnTerminal = r.ReturnTerminal
			break
		}
		if r.SkipRemaining {
			break
		}
	}

	return result, nil
}

// HasPhase 检查指定 phase 是否有已注册的 hooks。
func (c *HookChain) HasPhase(phase InternalHookPhase) bool {
	return len(c.hooks[phase]) > 0
}
