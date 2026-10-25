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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/anthropic/claude-go/pkg/weakmodel"
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

	// runStateDir 隔离运行 (RunIsolated) 的中间态目录 (H3-2; 空 = 不持久化)。
	// 由 SetRunStateDir 装配, 见 runner_state.go。
	runStateDir string

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

	// memHook 是注册进 HookChain 的那个 MemoryInjectHook 句柄。
	//
	// 持有它只为热改注入形态 (SetMemoryIndexMode): 形态开关要在**不重建
	// HookChain、不影响正在跑的 run** 的前提下翻转, 而 HookChain 只提供遍历、
	// 不提供按名字取。重建链是另一条路, 但会连带重置其它 hook 的会话态。
	memHook *internal_hook.MemoryInjectHook

	// RunBudget run 级 token 预算闸门 (P0-1)。nil = 不设限。
	//
	// 与 CumulativeUsage 的区别是**这件事的全部要害**: 后者只累加、从不与上限比较
	// (唯一读者 GetContextUsage 算的是单次请求的窗口占用), 而闸门管的是累计成本。
	// 两者的口径必须一致, 所以累加写在同一处 (见 queryLoop 里的 usage 累计点)。
	//
	// 复用语义: 引擎是会话级复用的, 而闸门要的是 **run 级**。run 边界由 RunID 变化
	// 识别 (queryLoop 每轮比对 lastBudgetRunID), 识别到就 Reset —— 不这么做的话,
	// 长会话跑到第 N 个 run 会被前面所有 run 的累计消耗呛死, 那不是"这个 run 太贵",
	// 是判据挂错了对象。
	RunBudget *internal_hook.RunBudgetGate
	// lastBudgetRunID 上一次计费时的 RunID, 用于识别 run 边界。仅由 queryLoop 读改写。
	lastBudgetRunID string

	// lastTerminal 最近一次 queryLoop 的终态。见 SubmitMessage 里的赋值处。
	lastTerminal types.Terminal

	// ========== 运行中入站通道 (21.8.12 P5 / D6-3 缺口 2 级 2) ==========
	//
	// 改造前 QueryEngine **没有**"往运行中的 loop 注入内容"的入口: SubmitMessage /
	// SubmitStream / SubmitStreamBlocks 三个全是**开新一轮** —— 它们 append 一条 user
	// 消息再从头跑, 而一个正在跑的 run 是独立的 goroutine, 调这三个只会并发起第二轮。
	//
	// 而"运行中送达"这件事在本仓有真实需求: 团队阶段跑着的时候用户发来一条更正,
	// 今天只能等这一轮结束才看得到 (信箱注入在阶段起点发生**一次**, 见
	// workflow.go 的 takeMailFor)。等到的可能是十分钟后, 而那时基于旧信息的活已经干完了。
	//
	// ★ 队列挂在这里而不是用引擎级的一个字段: 主循环 (queryLoop) 天然一次只有一个
	// run, 所以引擎自己那一个队列身份无歧义。**RunIsolated 不用它** —— 那条路是
	// 8 并发共用一个引擎 (agentPool), 引擎级队列会让 A 阶段的注入被 B 阶段收走。
	// 那批 run 各自从 IsolatedRunOptions.Inject 拿一个自己的队列。判据与理由
	// 见 pkg/engine/inject.go 的文件头。
	//
	// ★ 注入物只能是 **user 消息**, 不能是任意 role: 见 InjectQueue.Inject 的注释。
	inbox InjectQueue

	mu sync.Mutex
}

// InjectMessage 往**正在运行的**主循环 (queryLoop) 注入一条 user 消息 (21.8.12 P5)。
//
// 返回值:
//   - true  —— 已入队, 下一个轮次边界 (queryLoop 顶部) 会被取走并投递给模型;
//   - false —— 没有循环在跑 (见 InjectQueue.Inject 关于"不降级成新 run"的论证)。
func (e *QueryEngine) InjectMessage(msg types.Message) bool { return e.inbox.Inject(msg) }

// Running 报告主循环当前是否在跑 (即 InjectMessage 会不会被接受)。
//
// ★ 存在的理由是**取信是破坏性的**: 中继必须先问"现在能不能接"再取信, 否则引擎
// 空闲期间会"取出来又退回去", 每轮轮询都在信箱日志里追加一条。
func (e *QueryEngine) Running() bool { return e.inbox.Active() }

// CountInjectionsDropped 报告"运行结束后仍留在队列里、已被丢弃"的注入条数。
// 见 InjectQueue.Pending 关于这个差额为什么必须数得出来。
func (e *QueryEngine) CountInjectionsDropped() int { return e.inbox.Pending() }

// CountInjectionsPending 报告主循环**此刻**还没取走的注入条数 (运行中也可读)。
//
// 与 CountInjectionsDropped 的区别只在读的时机: 那个是 run 结束时取的 (队列已清空,
// 剩下的就是丢弃数), 这个是任意时刻的瞬时值 —— 运行中非零是**正常的** (注入按轮次
// 边界投递, 两条工具调用之间就是几秒), 而"结束时"非零才是丢弃。
func (e *QueryEngine) CountInjectionsPending() int { return e.inbox.Pending() }

// SetMemoryIndexMode 热改记忆注入形态 (正文 ⇄ 索引)。
//
// 供配置热加载回调调用: 改配置即生效, 不必重启进程、也不必重建 HookChain ——
// 后者会连带重置其它 hook 的会话态, 而队列里可能有正在跑的任务。
//
// 生效边界由 MemoryInjectHook 决定: 检索本身每个 run 只跑一次 (t0), 热改不重跑
// 它; 但**呈现形态**会在本 run 的下一个 turn 按新取值重渲染。所以"活跃会话立即
// 生效"成立 —— 代价是那一个 turn 的前缀缓存从 memory 块起失效一次, 而该块排在
// system 末尾, 这已经是最便宜的失效点。
//
// 未接入记忆 (memHook 为 nil) 时静默返回 —— 没有记忆的部署改这个开关本就没有
// 意义, 不该因此让热加载报错。调用方要分清这种情况用 MemoryHookBound。
func (e *QueryEngine) SetMemoryIndexMode(on bool) {
	if e == nil || e.memHook == nil {
		return
	}
	e.memHook.SetIndexMode(on)
}

// MemoryIndexMode 返回当前记忆注入形态 (未接入记忆时为 false)。
//
// 注意"未接入"与"接入但为关"都返回 false —— 要分清用 MemoryHookBound。
func (e *QueryEngine) MemoryIndexMode() bool {
	if e == nil || e.memHook == nil {
		return false
	}
	return e.memHook.IndexMode()
}

// MemoryHookBound 报告记忆注入 hook 是否已接上。
//
// 为什么需要它: memHook 只在 registerInternalHooks 跑、且 MemoryStore/FactStore
// 至少有一个非 nil 时才赋值, 而引擎是**构造之后**才赋组件的 (见 RefreshHooks)。
// 于是"开关怎么改都不生效"有两种完全不同的原因 —— 配置没送到, 或者 hook 压根
// 没建出来。只看 MemoryIndexMode 的话两者都是 false, 分不开。
func (e *QueryEngine) MemoryHookBound() bool {
	return e != nil && e.memHook != nil
}

// SessionStoreInterface 会话存储抽象。
type SessionStoreInterface interface {
	AppendUserMessage(msg types.Message, cwd, model string)
	AppendAssistantMessage(msg types.Message)
	SessionID() string
}

// outboundModel 把**身份名**翻译成**出站名**。
//
// 只对主模型生效: 相位路由的目标 (ExecutionModel/FallbackModel) 是配置里直接写下的
// 模型名, 没有"别名 vs 裸名"这一层, 原样发出即可。主模型是唯一被装配方拆成两半
// (身份带前缀 / 出站剥前缀) 的那个, 所以也只有它需要翻译 —— 判据是"这两个名字是不是
// 同一个模型", 而不是调用点在哪。
func (c *Config) outboundModel(identity string) string {
	if identity == c.Model && c.OutboundModel != "" {
		return c.OutboundModel
	}
	return identity
}

// Config 引擎配置
type Config struct {
	Model string
	// OutboundModel 是 Model 的**出站形态** —— 身份与实际发到线上的字节未必相同,
	// 留空 = 与 Model 相同 (旧行为)。
	//
	// 为什么需要这个字段: Model 是**身份** (进 transcript/metrics, 带 provider 前缀
	// 更利于归因), 而出站发什么是**链路形态的函数** —— 经网关发完整别名 (别名是网关
	// 的查找键), 直连 provider 发裸名 (厂商端点与 Ollama 的 /v1/messages 兼容层只认
	// 裸名, 发前缀直接 400 invalid model name)。这一分工在装配方 (cmd/claude-go) 定下,
	// 见 modelconfig.ResolvedConfig.OutboundModelName。
	//
	// 引擎里必须显式带上它, 否则下面的相位路由会把它抹掉: 那段代码按
	// "turnModel != client.Model 才换客户端" 判断, 比的是身份名, 换上去的也是身份名。
	// 装配方把 client.Model 设成裸名之后, turnModel (别名) 与之不等 → 第一回合就把
	// 客户端"纠正"回别名 → 出站重新变成前缀, 直连端点 400。故引擎里凡是要拿 Model
	// 去写客户端的地方, 都该改用 OutboundModel。
	OutboundModel string
	// ExecutionModel 自动相位路由的快速执行模型 (Path B)。
	// 设置后, 工具消费回合 (消息尾部携带 tool_result) 自动路由到该模型,
	// 规划/推理回合仍走 Model。留空 = 关闭自动路由, 全部回合走 Model。
	ExecutionModel string
	FallbackModel  string
	MaxTokens      int
	MaxTurns       int // queryLoop 最大迭代次数 (0 = 无限)
	ContextWindow  int // 模型上下文窗口 (tokens, 0 = 默认 200000)

	// MaxRunTokens 一个 run 的 token 消耗上限 (0 = 不设限; 也可经
	// CLAUDE_GO_MAX_RUN_TOKENS 配置)。超过即终止, 终态 Reason = "budget_exhausted"。
	//
	// 为什么必须有这一条: maxTurns 是**步数**上限不是成本上限 —— 一次 WebFetch 与
	// 一次 Read 都算 1 轮, 成本可能差三个数量级 (实测 209 个 run 里 WebSearch +
	// WebFetch 合计 3206 次)。步数跑满 ≠ 钱没花超, 所以需要一道按 tokens 计的止损线。
	//
	// 口径 = 输入 + 输出 + 缓存读 + 缓存写 (与 CumulativeUsage 的四项一致)。
	// 只在**成功**调用上累加: 失败调用不带 usage 是正常的 (流里没有 message_start),
	// 把它当 0 计入会让失败的 run 显得没花钱。
	MaxRunTokens int

	// TaskRoster 提供"还剩什么没做完"的快照, 压缩时会被嵌进摘要 (H1-3)。
	//
	// 用函数签名而不是具体类型: 提供服务的是 pkg/tool/builtin 的 TodoWriteTool,
	// 而 builtin 经 evotools → evolution/govern 与引擎侧成环, 引擎 import 不了它。
	// 装配处 (cmd / feishu) 把 todo 工具的 Snapshot 方法包一层传进来。
	//
	// nil = 不注入清单, 行为与改造前逐字节相同。
	TaskRoster func() []internal_hook.TaskRosterItem

	// AgentDBSemantic 层 2 语义增强面: agentDB 语义引擎客户端 (可选, nil 则不注册
	// AgentDBSemanticInjectHook)。经 serve /v1/memory|retrieve|files 跨源检索并注入
	// system prompt (MemoryInject 的 agentDB 后端, 规划 14.1.4.3)。
	AgentDBSemantic     *agentdbsemantic.Client
	SemanticTopK        int    // 每源检索条数 (默认 5)
	SemanticTokenBudget int    // 注入上下文 token 预算 (默认 600)
	SemanticReserve     int    // 为任务正文预留 token (默认 200)
	Cwd                 string // 当前工作目录
	PermissionMode      types.PermissionMode
	IsNonInteractive    bool
	Debug               bool
	SessionID           string
	MetricsSource       string // LLM 指标来源: feishu_main / nested_agent / team_stage / cli
	MetricsPurpose      string // 额外维度: chatID / team / stage / compact 等
	Workflow            string // Agent Team 工作流名
	Role                string // Agent 角色名
	// Team 池个性化身份 (13.7-P1): 经 ToolContext.Team 透传给 pool_search/pool_load,
	// 供 Beta 后验按 (team, role, asset) 记 trial。engine 不 import pkg/agent
	// (避免 agent↔engine 环), 团队名由装配方显式填入, 不走 RunMetadataFromContext。
	Team string // Agent Team 名 (池个性化身份)
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

	// ConstraintOrigin 记录上面两个名单/权限档由哪个 ConstraintSet 装配而来
	// (design/01 §4.6 审计)。多次 ApplyConstraints 会以 " > " 串成继承链。
	// 纯观测字段, 不参与任何判定。
	ConstraintOrigin string

	// WeakModel 弱模型 harness 增强 (手册 13.3 方案三: L2 schema 校验修复环 /
	// L3 tool_result 后处理 / L4 工具掩码 / L6b 规划指引)。零值 = 全关,
	// 不改变任何既有行为。装配入口见 cmd/claude-go main.go weakmodel.Resolve。
	WeakModel weakmodel.Profile

	// ==================== 前沿优化特性开关 (默认关闭) ====================
	// 详细方案见 docs/query-engine-frontier-optimization.md。
	// 推荐通过 QueryEngine.EnableFrontierOptimizations() 统一启用 P0/P1 组件。

	// EnableMetrics 启用引擎级指标采集 (G10, 低开销)。
	EnableMetrics bool
	// EnablePromptCache 启用 prompt 稳定前缀布局与 cache 命中追踪 (G1)。
	EnablePromptCache bool

	// MemoryIndexMode 记忆注入形态: nil = 未配置 (回落到环境变量), true = 只注入
	// 索引 (id + 标题 + 预览), false = 注入正文。
	//
	// 指针而非 bool: 「没配」与「显式配了 false」必须能区分 —— 前者要回落到
	// CLAUDE_GO_MEMORY_INDEX_MODE, 后者不该被 env 覆盖。见 resolvedMemoryIndexMode。
	//
	// 运行期翻转走 SetMemoryIndexMode (配置热加载调用), 不要直接改这个字段:
	// 它只在 registerInternalHooks 读一次, 改了要下次重建链才生效。
	MemoryIndexMode *bool
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

	// ==================== AutoMode 引擎级自判断 (复杂任务 → 自动 Plan + Advisor) ====================
	// 对齐 Claude 客户端: 会话思考中引擎自行判断任务复杂度, 复杂任务自动进入只读规划
	// (plan mode) 并可选自动咨询 advisor; 模型规划完调 ExitPlanMode 自动转入实施, 全程无人参与。
	// AutoPlanMode 总开关。开启后 AutoModeHook 在 PhasePreRequest 首轮判定复杂度,
	// 复杂则经 PlanModeSetter 写入会话级 plan flag → DynamicPlanCheck 读到 → 只读强制。
	AutoPlanMode bool
	// ComplexityMode 复杂度判定模式: heuristic(默认,零额外 LLM)|llm|hybrid。
	ComplexityMode string
	// LLMComplexityFn 可选 LLM 分类器: (是否复杂, 原因, 错误)。错误时回落启发式。
	LLMComplexityFn func(ctx context.Context, text string) (bool, string, error)
	// AutoAdvisor 复杂任务是否自动咨询 advisor 并注入建议。
	AutoAdvisor bool
	// PlanModeSetter 写会话级 plan flag 的回调 (装配方注入, 绑定 builtin.SetPlanModeForSession)。
	// 与模型 EnterPlanMode/ExitPlanMode 工具共用**同一个** flag, 保证自动开启与模型
	// ExitPlanMode 天然衔接 (自动结束自动放行)。
	PlanModeSetter func(on bool)
	// PlanFileDir 计划文件目录。配置后 ExitPlanMode 会把提交的计划落盘,
	// 实施阶段模型可读取该文件作为执行依据 (对齐 Claude 客户端计划持久化)。
	// 空 = 不落盘 (仅回显计划文本)。
	PlanFileDir string
	// PlanClient 规划期独立模型客户端 (Tag=plan, 类似 advisor 独立客户端)。
	// 会话 plan flag ON (规划期) 时 queryLoop 全程路由到此客户端:
	//   - 独立 provider/模型, 规划质量更高, 不挤占主模型配额;
	//   - 优先于 ExecutionModel 相位路由 (规划期连续回合 sticky, 共享前缀 KV 缓存);
	//   - nil = 不启用, 规划期沿用 主模型/ExecutionModel 现有路由。
	// 判定时机放在 PhasePreRequest 之后: AutoModeHook 在该 phase 置位 plan flag,
	// 首个规划回合 (TurnCount==0) 也能命中 plan 客户端。
	PlanClient *api.Client
}

// toolExposed 报告某工具能否暴露给模型并执行: 先过 DisabledTools 黑名单, 再过 AllowedTools 白名单。
// 白名单非 nil (含空集) 即"仅名单内放行", 空集拒绝一切; nil 表示未设置。
// 注意: 这是与 ToolGateHook 互为镜像的**单一真源**, 两处判据必须一起改。
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

// ToolConstraintSource 是"已编译的约束"的最小读接口, 由 pkg/agent.ConstraintSet
// **结构性**满足 (design/01 §4.6 单一真源)。
//
// 为什么是接口而不是直接 import pkg/agent: pkg/engine 在每轮每工具的热路径上,
// 让它反向依赖 pkg/agent 那棵重依赖树 (orchestrator/swarm_intel/media/...) 是架构
// 倒挂, 而且一旦 pkg/agent 将来需要 import pkg/engine 就是依赖环。名单仍然只在
// ConstraintSet 里声明一次, 执行点仍然只有 toolExposed 一个 —— 单一真源成立。
type ToolConstraintSource interface {
	// CompiledToolAllow 白名单。nil = 不设白名单; 非 nil (含空 map) = 仅名单内放行,
	// 空 map 即全部拒绝 —— 与 Config.AllowedTools 的 fail-closed 语义一致。
	CompiledToolAllow() map[string]bool
	// CompiledToolDeny 黑名单 (nil = 不禁)。
	CompiledToolDeny() map[string]bool
	// NarrowedPermissionMode 给定引擎当前权限档, 返回应下发的档位; "" = 不覆盖。
	// **契约**: 返回值只能是 current 或比 current 更严的一档。权限严格度阶梯是
	// 一张有序表, 全仓只有 pkg/agent/constraints.go 一份 (permissionStrictness);
	// 这里让实现方回答"收窄后是哪一档", 而不是在 engine 里再抄一份阶梯 —— 抄一份
	// 就又是一个第二真源, 正是 §4.6 要消除的东西。
	NarrowedPermissionMode(current string) string
	// ConstraintOrigin 审计: 约束的来源链。
	ConstraintOrigin() string
}

// ApplyConstraints 把一个 ConstraintSet 编译结果装配进 Config。
//
// **只收窄, 永不放宽**:
//   - 黑名单取并集 (只增不减);
//   - 白名单: 本来没有名单则采用来源的名单 (设名单本身就是收窄); 两边都有则取交集;
//   - 权限档只接受更严的一档 (由来源方按唯一的严格度阶梯裁决)。
//
// 返回非 nil error 表示来源里**有放宽意图已被丢弃**(白名单里出现了当前名单之外的
// 工具), Config 保持更严的现状。调用方应记日志/告警, 但不必回滚 —— 按构造这里
// 不可能变松。
//
// 不在热路径上: 每个会话/每个 agent 建立时调用一次; toolExposed 一个字节没动。
func (c *Config) ApplyConstraints(src ToolConstraintSource) error {
	if c == nil || src == nil {
		return nil
	}
	var rejected []string

	if deny := src.CompiledToolDeny(); len(deny) > 0 {
		if c.DisabledTools == nil {
			c.DisabledTools = make(map[string]bool, len(deny))
		}
		for name := range deny {
			c.DisabledTools[name] = true
		}
	}

	if allow := src.CompiledToolAllow(); allow != nil {
		if c.AllowedTools == nil {
			next := make(map[string]bool, len(allow))
			for name := range allow {
				next[name] = true
			}
			c.AllowedTools = next
		} else {
			next := make(map[string]bool, len(allow))
			var escaped []string
			for name := range allow {
				if c.AllowedTools[name] {
					next[name] = true
				} else {
					escaped = append(escaped, name)
				}
			}
			// 交集: 现有名单外的工具一律丢掉, 绝不因为来源"也允许"就放进来。
			c.AllowedTools = next
			if len(escaped) > 0 {
				sort.Strings(escaped)
				rejected = append(rejected, "白名单越界工具已丢弃: "+strings.Join(escaped, ","))
			}
		}
	}

	if pm := src.NarrowedPermissionMode(string(c.PermissionMode)); pm != "" {
		c.PermissionMode = types.PermissionMode(pm)
	}

	if origin := src.ConstraintOrigin(); origin != "" {
		if c.ConstraintOrigin == "" {
			c.ConstraintOrigin = origin
		} else {
			c.ConstraintOrigin += " > " + origin
		}
	}

	if len(rejected) > 0 {
		return fmt.Errorf("约束装配拒绝了放宽声明 (%s): %s", src.ConstraintOrigin(), strings.Join(rejected, "; "))
	}
	return nil
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
	// L4 工具掩码 (方案三): 弱模型 Profile 生效时把默认掩码清单并入 DisabledTools
	// (并集幂等, RefreshHooks 重注册 ToolGateHook 也拿到合并后的集合),
	// 从而 API 面过滤 (queryLoop) 与执行面拦截 (ToolGateHook) 共用同一真源。
	if cfg.WeakModel.Enabled && cfg.WeakModel.ToolMasking {
		if cfg.DisabledTools == nil {
			cfg.DisabledTools = make(map[string]bool, len(weakmodel.DefaultMaskedTools))
		}
		for _, name := range weakmodel.DefaultMaskedTools {
			cfg.DisabledTools[name] = true
		}
		for _, name := range cfg.WeakModel.MaskedExtra {
			cfg.DisabledTools[name] = true
		}
		logging.For("engine").Info("weak-model tool masking active", "masked", len(cfg.DisabledTools))
	}
	// hook 池化 (21.8.12 P3) 的两段式收口: 池在 hook 运行器之前装配 (工具注册期),
	// 所以"运行期装载 hook"的装卸口只能在这里注入 —— 它是全仓唯一同时拿得到
	// **工具注册表**与**hook 运行器**的地方, 而那两样正是这条接头需要的两端。
	//
	// 放在这里而不是每个调用点各写一遍: 调用点有 5 处 (飞书 3 + CLI 2), 而漏掉
	// 其中任意一处的表现是"那条路径上 pool_load 一条 hook 报'运行时不支持'" ——
	// 一个看起来像配置问题的行为差异, 查起来要去数调用点。一处收口则 5 条全通。
	//
	// 两端任一为 nil 时是**无操作**: 池没装配 (env 开关默认关) 或本次运行没有
	// Runner, 这时 pool_load 一条 hook 会如实报"不支持", 不静默成功。
	if tools != nil && hookRunner != nil {
		if p := tools.Pool(); p != nil {
			p.SetHookSink(hookRunner)
		}
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

// VisibleTools 返回**当前实际可见**的工具名 (已排序), 即经 toolExposed 过滤后的
// 结果。nil 接收者返回 nil。
//
// 与 Tools.Names() 的差别就在过滤: Names() 是注册表里有什么, 本方法是这次装配
// 下模型**能看见**什么。装配自报要的是后者 —— 白名单误配的表现恰恰是"注册表
// 里全都在, 但对模型一个都不可见", 拿注册表去报会把这件事完全掩盖掉。
//
// 注意它反映的是**调用时刻**的 Config: 平台可在运行期经 ApplyConstraints 收窄,
// 收窄之后要重取一次才准。
func (e *QueryEngine) VisibleTools() []string {
	if e == nil || e.Tools == nil {
		return nil
	}
	names := e.Tools.Names()
	out := make([]string, 0, len(names))
	for _, n := range names {
		if e.Config.toolExposed(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// HiddenTools 返回被约束挡在模型视野之外的工具名 (已排序)。
//
// 单列出来是因为"看不见"有两种成因且处置完全不同: 被 DisabledTools 拉黑的
// (工具坏了/不该用) 与被白名单挡掉的 (不在本岗位职责内)。装配对账要能区分它们,
// 否则一次白名单写错看起来就像"这台机器上没装那个工具"。
func (e *QueryEngine) HiddenTools() []string {
	if e == nil || e.Tools == nil {
		return nil
	}
	names := e.Tools.Names()
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !e.Config.toolExposed(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// InternalHookPhases 返回内置 Hook 链上每个相位已注册的 hook 名 (含空相位)。
// 引擎未构造 HookChain / 链为 nil 时返回 nil。
func (e *QueryEngine) InternalHookPhases() map[string][]string {
	if e == nil || e.HookChain == nil {
		return nil
	}
	return e.HookChain.PhaseHooks()
}

// resolvedMemoryIndexMode 决定记忆注入形态的初值。
//
// 优先级: 配置项 engine.memoryIndexMode > 环境变量 CLAUDE_GO_MEMORY_INDEX_MODE。
//
// 为什么配置优先而不是环境变量优先: 环境变量是**进程启动时读一次**的, 改了要
// rollout 重启; 而配置走 hotreload.WatchConfig, 5 秒比对一次内容, 改完即生效,
// 对 k8s 里长跑、队列里还压着任务的进程尤其重要。环境变量保留作兜底 ——
// 容器里没有配置文件、只有 env 的部署仍然能用。
//
// Config.MemoryIndexMode 是指针: nil(未配置) 与 false(显式关) 必须区分开,
// 否则显式关会被 env 的兜底值反向覆盖。
func (e *QueryEngine) resolvedMemoryIndexMode() bool {
	if e != nil && e.Config != nil && e.Config.MemoryIndexMode != nil {
		return *e.Config.MemoryIndexMode
	}
	return builtin.MemoryIndexModeEnabled(os.Getenv)
}

// mergeUsage 把一帧用量并入已收集的用量。nil 安全 (两帧都可能缺席)。
//
// # 为什么是"按非零填写"而不是"后者整体覆盖"
//
// 一次流式响应里用量被拆在两个地方, 拆法**随协议而异**:
//
//	anthropic (Protocol 零值, 即默认) —— message_start 带 input/cache_read/cache_creation,
//	  message_delta 只带 output_tokens;
//	openai / openai-responses —— 翻译器把 message_start 的 usage 造成空对象,
//	  全部真实用量挂在 message_delta 上。
//
// 原实现是 `usage = event.X` 的整体覆盖, 于是 anthropic 协议下输入侧会被 message_delta
// 那一帧抹掉, 累计只剩输出 —— 而输入通常是输出的一到两个数量级。这个丢失是**静默**的:
// run 照常成功、终态正常, 只有账不对。P0-1 的预算闸门正建立在这个账上, 所以它必须先对。
//
// 按非零填写对两种拆法都成立: 拆开时两帧各填一半, 合起来正好; 两帧都报全量时
// 后一帧的值覆盖同名的前一帧的值, 不会翻倍。
//
// 代价 (需明说): 同一个字段在两帧里分别报 x 与 y (y≠x, 都非零) 时, 以 y 为准。
// 这不是本函数的选择, 是协议本身没给"谁是最终值"的语义; 取后到的那帧是唯一合理的读法。
//
// # 两处调用点里真正承重的是哪一处 (变异测试结论, 不是推测)
//
// 把 message_delta 那处改回整体覆盖 ⇒ TestUsage_MergesBothFrames 立刻失败;
// 把 message_start 那处改回整体覆盖 ⇒ 测试**仍然通过** (因为后到的 message_delta
// 会把输入侧补回来)。所以修掉这个缺陷的是 message_delta 那一处。
//
// message_start 那处保留 merge 不是冗余: 它让本函数的语义在**任意帧序**下都成立,
// 而上游发帧顺序不在我们控制之内 (翻译器、网关都可能改序)。但读代码的人该知道
// 哪一行在承重 —— 否则将来有人"简化"掉 message_delta 那处时会以为两处对等。
func mergeUsage(dst, src *types.Usage) *types.Usage {
	if src == nil {
		return dst // 这一帧没带用量, 保持已收集的
	}
	if dst == nil {
		cp := *src
		return &cp
	}
	if src.InputTokens != 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheReadInputTokens != 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens != 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	return dst
}

// chargeRunBudget 把一次成功调用的用量记进 run 级闸门, 并识别 run 边界。
//
// run 边界的识别方式 (RunID 变化) 是刻意的: CumulativeUsage 的注释写着"本会话累积",
// 而闸门要的是 run 级语义。不复位的话, 长会话跑到第 N 个 run 时会被前 N-1 个 run 的
// 累计消耗呛死。判据取 trace 四元组里的 RunID 而不是"重新构造了引擎" —— 后者在
// 复用引擎的编排路径上是假的 (agent 的 factory 复用同一个 eng 跑每个 stage)。
//
// 只在这里被调用, 由 queryLoop 单线程驱动, 故 lastBudgetRunID 无需加锁。
func (e *QueryEngine) chargeRunBudget(ctx context.Context, usage *types.Usage) {
	if e == nil || e.RunBudget == nil || usage == nil {
		return
	}
	if runID := trace.From(ctx).RunID; runID != e.lastBudgetRunID {
		e.RunBudget.Reset()
		e.lastBudgetRunID = runID
	}
	// 四项求和与 CumulativeUsage 的累计项逐一对应 (含缓存读写)。合计为 0 的调用
	// 不报账: 一个报了全 0 用量的响应不该占用一次计费事件。
	total := usage.InputTokens + usage.OutputTokens +
		usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	if total <= 0 {
		return
	}
	e.RunBudget.Spend(total)
}

// runTokenBudget 解析一次调用的预算上限: Config.MaxRunTokens 优先, 未配置时回落
// 到 CLAUDE_GO_MAX_RUN_TOKENS; 都不可用则返回 0 = 不设限 (保持现状行为)。
//
// **必须允许配置一个显式关闭**: "没配" 与 "配成 0 (不设限)" 在部署上都会出现, 而
// 后者是有意的运营选择。两者都返回 0, 但只有前者会去看 env —— 这正是配置优先于
// env 的落点 (与 resolvedMemoryIndexMode 同一族: 显式配置不该被 env 反向覆盖)。
func (e *QueryEngine) runTokenBudget() int {
	if e == nil || e.Config == nil {
		return 0
	}
	if e.Config.MaxRunTokens > 0 {
		return e.Config.MaxRunTokens
	}
	if e.Config.MaxRunTokens < 0 {
		return 0 // 显式负值 = 明确关闭, 不再看 env
	}
	return runTokenLimitFromEnv()
}

// runTokenLimitFromEnv 读 CLAUDE_GO_MAX_RUN_TOKENS 得到 run 级 token 上限; 未配或非法都返回 0 (不限)。
func runTokenLimitFromEnv() int {
	v := strings.TrimSpace(os.Getenv("CLAUDE_GO_MAX_RUN_TOKENS"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		// 解析不了就当作没配。**不 fail-closed 成 0 上限** —— 那会把一个手滑的
		// 环境变量变成"所有 run 一步都跑不了", 而配置错误该是吵一声然后走默认。
		if err != nil {
			logging.For("engine").Warn("CLAUDE_GO_MAX_RUN_TOKENS 不是整数, 忽略", "value", v)
		}
		return 0
	}
	return n
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
	// P0-1 run 级预算闸门。与上面那道 EnableBudget 是**两件事**, 不要合并:
	// TokenBudgetManager 管的是"这一轮请求的上下文窗口还剩多少"(超了压缩旧消息),
	// RunBudgetGate 管的是"整个 run 累计花了多少钱"(超了停止 run)。
	// 前者是质量治理, 后者是成本止损; 混在一起的后果是"上下文还很空所以继续烧钱"。
	if e.RunBudget == nil {
		if limit := e.runTokenBudget(); limit > 0 {
			e.RunBudget = internal_hook.NewRunBudgetGate(limit)
			logging.For("engine").Info("run 级 token 预算已启用", "limit", limit)
		}
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

// HasCompactionSink 报告折叠读数出口是否已接线 (H2-1)。
//
// 给"接线本身"一条可断言的判据: 采集逻辑的单测直接构造 hook, 测不到引擎有没有把
// sink 装上 —— 而漏装的线上表现恰好是"折叠照常发生、轨迹里一条读数都没有"。这种
// "逻辑对、接线漏"的形态两边测试都绿 (design/03 §4.1 记过同一类教训)。
func (e *QueryEngine) HasCompactionSink() bool {
	if e == nil || e.HookChain == nil {
		return false
	}
	for _, h := range e.HookChain.Hooks(internal_hook.PhasePreCompact) {
		switch v := h.(type) {
		case *internal_hook.AutoCompactHook:
			if v.HasCompactionSink() {
				return true
			}
		case *internal_hook.MicroCompactHook:
			if v.HasCompactionSink() {
				return true
			}
		}
	}
	return false
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
		// 折叠边界那次折叠也走文件引用 (与 MicroCompact 同目录), 否则被摘要
		// 掉的那段历史里的大 tool_result 就永久丢了 —— 而那恰恰是模型刚据以
		// 推理过、最可能要回读的一段。
		e.Compactor.SetToolResultRefStore(e.Config.Cwd)
		acHook := internal_hook.NewAutoCompactHook(e.Compactor, e.MemoryStore, e.Ingestor, e.HookRunner)
		// H1-3: 压缩时把待办清单嵌进摘要。清单原先只活在摘要之前的历史里, 折掉
		// 之后模型就看不见"还剩什么没做完"了 —— 这是长任务跑到第 80 轮时进度
		// 不可信的直接成因。
		if e.Config.TaskRoster != nil {
			acHook.SetTaskRosterSource(e.Config.TaskRoster)
		}
		// H2-1 压缩可观测: 把折叠读数接到 compaction Span。装在 compactor 上
		// (SetOnCompact) 而不是只报本 hook 这一次 —— 折叠还有 PTL 应急压缩与
		// queryLoop 直调两条路径, 只报这里就漏掉那两条。
		if e.TraceStore != nil {
			acHook.SetCompactionSink(e)
		}
		e.HookChain.Register(acHook)
	}
	// 传 Cwd: 折叠原文落盘到 <cwd>/.claude-go/tool-results/, 占位里给出可 Read
	// 回读的路径。Cwd 为空时自动退化为纯截断 (行为与改造前一致)。
	microHook := internal_hook.NewMicroCompactHook(e.Config.Cwd)
	if e.TraceStore != nil {
		microHook.SetCompactionSink(e)
	}
	e.HookChain.Register(microHook)
	if e.Budget != nil {
		e.HookChain.Register(internal_hook.NewBudgetDegradeHook(e.Budget, e.HookRunner, e.Metrics))
	}

	// PhasePreRequest: ToolResultLevel(38) + MessageFilter(40) + AutoMode(45) + MessageMetrics(48) + MemoryInject(50) + PromptCache(55)
	e.HookChain.Register(internal_hook.NewToolResultLevelHook(nil, e.Metrics))
	e.HookChain.Register(internal_hook.NewMessageFilterHook())
	if h := internal_hook.NewAutoModeHook(
		e.Config.AutoPlanMode,
		e.Config.ComplexityMode,
		e.Config.LLMComplexityFn,
		e.Config.AutoAdvisor,
		e.Config.AdvisorConsultFn,
		e.Config.PlanModeSetter,
		e.Config.PlanFileDir,
		e.Metrics,
	); h != nil {
		e.HookChain.Register(h)
	}
	e.HookChain.Register(internal_hook.NewMessageMetricsHook(e.Metrics))
	if e.MemoryStore != nil || e.FactStore != nil {
		memHook := internal_hook.NewMemoryInjectHook(e.MemoryStore, e.FactStore)
		// 检索遥测 (mem_retrieval_count_l1/l2 + mem_injection_chars)。落点取全局
		// LLM 采集器: 它由进程启动时按 stateDir 初始化, 与 llm.jsonl 同目录, 于是
		// "记忆注入多少"和"这次调用烧了多少 token"能在同一块面板上对账。
		//
		// 必须先判 c != nil 再传: GlobalLLMCollector 返回的是 *Collector, 未初始化
		// 时是个**带类型的 nil**。直接塞进 SetMetrics(MemoryMetricsRecorder) 会被
		// 装箱成一个**非 nil 的接口**, hook 里那句 `h.metrics == nil` 于是拦不住,
		// 第一次检索就 Record 到 nil 接收者上 panic —— 而且只在"没初始化采集器 +
		// 真跑到记忆检索"这个组合下出现, 探针与单测正好都符合, 生产因为采集器总
		// 是开着的反而看不见。想"不采集"就必须把 nil 挡在这里。
		if c := metrics.GlobalLLMCollector(); c != nil {
			memHook.SetMetrics(c)
		}
		// 索引注入形态 (默认关)。配置项优先, 环境变量作兜底 —— 见
		// resolvedMemoryIndexMode 的注释说明为什么是这个优先级。
		memHook.SetIndexMode(e.resolvedMemoryIndexMode())
		e.memHook = memHook // 热加载路径要能 Set, 见 SetMemoryIndexMode
		e.HookChain.Register(memHook)
	}
	if e.Config.AgentDBSemantic != nil {
		topK, budget, reserve := e.Config.SemanticTopK, e.Config.SemanticTokenBudget, e.Config.SemanticReserve
		if topK <= 0 {
			topK = 5
		}
		if budget <= 0 {
			budget = 600
		}
		if reserve <= 0 {
			reserve = 200
		}
		e.HookChain.Register(internal_hook.NewAgentDBSemanticInjectHook(e.Config.AgentDBSemantic, topK, budget, reserve))
	}
	if e.PromptCache != nil {
		e.HookChain.Register(internal_hook.NewPromptCacheHook(e.PromptCache, e.Metrics))
	}

	// PhasePostRequest: XMLToolFallback(130)
	e.HookChain.Register(internal_hook.NewXMLToolFallbackHook())

	// PhasePreToolUse: JSONRepair(70) + SchemaValidate(75) + LoopDetectorInput(80) + DisabledTool(150)
	if e.JSONRepair != nil {
		e.HookChain.Register(internal_hook.NewJSONRepairHook(e.JSONRepair, e.Metrics))
	}
	// 方案三 L2: 弱模型 schema 校验修复环 —— 执行前校验 tool_use 入参,
	// 失败回注错误让模型原地重填 (≤2 次), 替代 JSONRepair 的沉默修复。
	// schema 表按掩码后的可见工具集构建 (与下发给模型的一致, 避免"拦可见工具以外"的错杀)。
	if e.Config.WeakModel.Enabled && e.Config.WeakModel.SchemaRetry && e.Tools != nil {
		schemas := make(map[string]json.RawMessage)
		for _, t := range e.Tools.APITools() {
			if e.Config.toolExposed(t.Name) && len(t.InputSchema) > 0 {
				schemas[t.Name] = t.InputSchema
			}
		}
		if h := internal_hook.NewSchemaValidateHook(schemas, e.Metrics, 2); h != nil {
			e.HookChain.Register(h)
		}
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

// taskInstructionPointer 是 TaskInstruction 已进 system 时首条 user 消息的占位文本。
//
// 为什么不是空串: 部分 provider 要求消息序列以非空 user 轮开始, 而 SubmitStreamBlocks
// 的图片路径也依赖有一条 user 消息承载 content blocks。留一句指向 system 的短提示
// 既不丢语义, 也不复制正文。
const taskInstructionPointer = "（任务指令已置于 system prompt 末尾，请按其中的任务目标与产出要求执行。）"

// initialUserTextLocked 计算首条 user 消息的文本 (调用方须持有 e.mu)。
//
// 历史坑: TaskInstruction 同时被 queryLoop 追加到 system prompt 末尾 (§Phase 2)
// **和**这里塞进 user message, 于是每次 llm_call 把整份阶段提示词发两遍 —— 实测
// TraceStore 里团队 stage 的 user 段与最后一个 system 段逐字节相同, if-question-expand
// 单次请求 76KB 里有 29KB 是这份重复 (38%), 全量 5645 次 llm_call 100% 命中, 浪费
// 26.6MB 请求体。TaskInstruction 字段注释写的就是"移到 system、避免 user message
// 重复膨胀", 这里的兜底与那个意图正好相反, 是改造时漏删的遗留分支。
func (e *QueryEngine) initialUserTextLocked(userContent string) string {
	if userContent != "" {
		return userContent
	}
	if e.TaskInstruction != "" {
		return taskInstructionPointer
	}
	return "."
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
	text := e.initialUserTextLocked(userContent)
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
		// 记下终态。**这不是给测试开的后门, 而是补一个真实缺口**: 在此之前
		// queryLoop 的返回值被 `_ = terminal` 丢掉, 于是"这个 run 为什么结束"
		// 只有 firePhasePostTurn 的 span 侧知道, 进程内的调用方无从判断。
		// H0-2/H0-5 的验收都要求"终态原因可断言且带身份", 没有这块就只剩日志。
		e.mu.Lock()
		e.lastTerminal = terminal
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

	// msgCh 必须有人排空, 否则**长会话必然死锁**。
	//
	// queryLoop 同时往两个通道写 (消息流 ch + 事件流 streamCh), 但本函数的返回值
	// 只有 streamCh —— 调用方 (cmd/claude-go 的 REPL 与 run) 也只读 streamCh。
	// 于是 msgCh 没有读者, 它的 50 格缓冲一满, queryLoop 就永远卡在 `ch <- m` 上,
	// 而 main 永远等在 streamCh 上: 两边互等, 谁也不会退出。
	//
	// 实测: 一个只产生 50 条以内消息的短会话跑得好好的 (缓冲吃得下), 一个跑了 10
	// 分钟、消息数超过 50 的长会话则**永久挂住** —— 进程还在, CPU 0%, 无网络连接,
	// 协程 dump 里 queryLoop 停在 chansend。这个缺陷只在长任务上现形。
	//
	// 消息本身不会因此丢失: 它们同样经由下面的 e.Messages = finalMsgs 与
	// SessionStore 落盘, msgCh 只是 SubmitMessage 那条路径的历史遗留。
	go func() {
		for range msgCh {
		}
	}()

	e.mu.Lock()
	text := e.initialUserTextLocked(userContent)
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

		// 同 SubmitMessage: 记下终态供调用方判断"这个 run 为什么结束"。
		e.mu.Lock()
		e.lastTerminal = terminal
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
//
// injectHookContext 把 hook 返回的 AdditionalContext 作为**非 meta 的 user 消息**追加进
// 消息序列。这是 P0「去装饰」的核心: 在本次改动之前, hookOut.AdditionalContext 全仓
// 只有 PreToolUse 一个消费方 (pkg/tool/orchestration.go:241), 其余 20+ 处只读 Decision/Reason
// —— 一条 prompt 型 hook 挂在 PreTurn/PreRequest/PostCompact 上时,
// **跑了、返回了、文本静默消失**。这正是 21.8.2「声明了但没接线」的典型形态。
//
// 两条设计约束:
//
//  1. **不加 IsMeta**。IsMeta 的消息在组装 API 请求时会被过滤掉, 加了就等于重新把这行
//     代码变回装饰。是否投递与"是不是 hook 注入"是两件事, 用 Metadata 标记而不是 IsMeta。
//
//  2. **不破坏 prompt 前缀缓存**。同一 hook 每轮可能返回同样的内容, 无条件追加会让前缀
//     每轮都在变。所以逐字符比对: 内容已出现在序列里就跳过 (同时挡连续重复与跨轮重复)。
//     空内容直接不注入 —— 注入一条空 user 消息会白白改变前缀, 是纯亏损。
func (e *QueryEngine) injectHookContext(messages []types.Message, hookOut *types.HookOutput, where string) []types.Message {
	if hookOut == nil {
		return messages
	}
	ctxText := strings.TrimSpace(hookOut.AdditionalContext)
	if ctxText == "" {
		return messages
	}
	for _, m := range messages {
		if m.IsMeta {
			continue // meta 消息不参与去重: 它本来就不会被投递
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockText && strings.TrimSpace(b.Text) == ctxText {
				return messages // 已经在序列里了, 不重复注入
			}
		}
	}
	if messages == nil {
		messages = make([]types.Message, 0, 1)
	}
	logging.For("engine").Debug("hook AdditionalContext 注入", "where", where, "len", len(ctxText))
	return append(messages, types.Message{
		Type:      types.MessageTypeUser,
		CreatedAt: time.Now(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: ctxText,
		}},
	})
}

// queryLoop 是引擎的主回合循环: 组装请求 → 流式取回 → 执行工具 → 收敛判定, 直到某个终态。
//
// 算法解读：
//
//	原理：每轮把当前消息序列发给选定的客户端, 取回 assistant 内容, 执行其中的工具调用并把
//	      结果追加回消息序列, 然后判定是否该停。相位路由 (主模型 / 执行模型 / 规划模型) 与
//	      各种压缩都挂在"每轮至多切一次模型、连续同类回合 sticky"这条约束下。
//	关键不变量：sentUpTo 之前的消息**一个字节都不能改** —— 前缀缓存按字节比对, 改动点之后
//	      的全部缓存当场失效。所有压缩/裁剪只能作用于 [sentUpTo, len)。
//	代价与边界：单轮内的连续错误计数是断路器而不是重试器, 达到上限直接走终态; 它不区分
//	      错误是否可重试, 因此上限必须留够瞬态抖动 (如 429) 的余地。
//	设计权衡：模型切换以"回合"为粒度而不是以"请求"为粒度, 用一次可能的切换成本换同模型
//	      连续回合的前缀 KV 缓存命中。
func (e *QueryEngine) queryLoop(ctx context.Context, messages []types.Message, ch chan<- types.Message, streamCh chan<- types.StreamEvent) ([]types.Message, types.Terminal) {
	turnCount := 0
	currentModel := e.Config.Model

	// 运行中入站通道 (21.8.12 P5) 的两半之一: 标记"有 loop 可以接注入"。
	//
	// 计数而不是布尔: 同一个引擎上可能同时跑着多个 queryLoop (嵌套派生 / 多阶段复用
	// 同一个引擎), 用 bool 的话先结束的那个会把标记清掉, 而另一个还在跑 —— 于是注入
	// 被拒, 而拒的理由 (running==0) 是假的。
	e.inbox.Enter()
	// defer 而不是在每条 return 前手动减: 这个函数有十几条 return (abort / 各类终态),
	// 手写的必然漏掉其中几条, 而漏掉的后果是**队列永久认为有人接** —— 之后所有
	// 注入都被收下、永远不被消费, 且没有任何信号。
	defer e.inbox.Leave()
	// sentUpTo 是"已经发给过上游的消息条数"水位线。
	//
	// 用途: 缓存感知的压缩只能动 [sentUpTo, len(messages)) —— 即本轮新增、尚未
	// 出现在任何已发出请求里的消息。**[0, sentUpTo) 是已经躺在缓存里的前缀, 一个
	// 字节都不能改**: 前缀缓存按字节比对, 改动位置之后的全部缓存当场失效。
	//
	// 这在改造前是没有约束的: MicroCompact 每轮遍历全部历史、FilterPureToolUseUnits
	// 每轮删历史尾部以外的纯 tool_use 单元, 于是同一条 tool_result 在第 1 轮和第 3
	// 轮发出去的字节不一样。实测 if-question-expand 的相邻请求 LCP 只有 64.6%。
	//
	// 注意它也必须跟着 AutoCompact 一起挪: 折叠换掉的是整段序列, 旧水位线在新序列
	// 上不再有意义 (且折叠后的新前缀尚未发出, 理应可再折)。
	sentUpTo := 0
	// 方案三 L8: 确定性失败信号升级 (单向棘轮)。信号全部来自 harness 自有 hook 的
	// 标记文本 (schema_validate 终态拦截 / loop_detector 抑制), 绝不取模型自评置信度。
	escalated := false
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
		// ============ Path B: 自动相位路由 (工具消费回合 → ExecutionModel) ============
		// 判定信号: 消息尾部最后一条是 User 消息且携带 tool_result (由上一轮工具执行 append,
		// 见下方 toolResults 追加处)。缓存感知: 连续工具回合 sticky 在同一模型,
		// 每轮用户请求至多切换 1 次 (主模型 → 执行模型 → 主模型), 同一模型连续回合共享前缀 KV 缓存。
		// ExecutionModel 为空 = 关闭, 行为与旧版完全一致。WithModel 是共享 HTTP/限流器的轻量副本。
		// 路由例外: 委派工具 (delegate_task/Agent/Task) 的报告需要主模型综合,
		// 该消费回合回主模型, 不切执行模型 (见 isDelegationResultTurn)。
		client := e.APIClient
		turnModel := e.Config.Model
		if e.Config.ExecutionModel != "" && isToolExecTurn(messages) && !isDelegationResultTurn(messages) {
			turnModel = e.Config.ExecutionModel
		}
		// L8: 升级优先于相位路由——已升级则本 run 剩余回合全部走 FallbackModel。
		if escalated {
			turnModel = e.Config.FallbackModel
		}
		if turnModel != client.Model {
			client = client.WithModel(e.Config.outboundModel(turnModel))
		}
		// L1 裁定项 (D2): tool_choice=any 只在"执行回合"且显式 opt-in 时启用,
		// 其余回合保持 auto——全局强制会破坏自然收尾 (13.3.9 探针实录)。
		if e.Config.WeakModel.ToolChoiceAny && isToolExecTurn(messages) {
			client = client.WithToolChoiceAny()
		}
		currentModel = turnModel

		// PreTurn Hook: 单轮开始
		if e.HookRunner != nil {
			hookOut := e.HookRunner.ExecutePreTurnHooks(messages, turnCount)
			if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
				logging.For("engine").Warn("PreTurn blocked by hook", "reason", hookOut.Reason)
				continue
			}
			// AdditionalContext 是 prompt 型 hook 的唯一产出。不接这一行, 挂在
			// PreTurn 上的 hook 就是"跑了、返回了、文本静默消失"。
			messages = e.injectHookContext(messages, hookOut, "PreTurn")
		}

		// 运行中入站消息 (21.8.12 P5 / D6-3 缺口 2 级 2): 取走交给本引擎的注入。
		//
		// 位置在 PreTurn 之后、Phase 0 (abort 检查) 之前, 理由是三条:
		//
		//  1. **轮次边界**: 每完成一次工具调用就回到这里, 于是注入的延迟是"到下一次
		//     模型调用", 而不是整个 run 的时长 —— 后者正是改造前信箱注入 (只在阶段
		//     起点一次) 的处境。
		//  2. **前缀缓存安全**: 注入只发生在**本轮新增、尚未发出**的区域, 已发出的前缀
		//     一个字节不动 (与上面 sentUpTo 那条纪律同一口径)。
		//  3. **与 abort 检查的相对位置**: 放在它之后的话, 一个即将退出的 run 会先收下
		//     消息再退出, 消息落进 inbox 无人消费 —— 而它本该被 CountInjectionsDropped
		//     如实报出来。放在之前, 同样的处境下消息**根本没被收下** (InjectMessage
		//     已经拒了), 少一条路径。
		for _, m := range e.inbox.Drain() {
			logging.For("engine").Info("运行中注入 user 消息", "turn", turnCount, "len", len(m.Content))
			messages = append(messages, m)
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
				SentUpTo:  sentUpTo,
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
			// 压缩把上下文换掉了, 这里注入的是"压缩后还想让模型记住什么"——
			// 典型用途正是把被压掉的约束重新钉回来。丢掉它等于压缩后失忆。
			messages = e.injectHookContext(messages, hookOut, "PostCompact")
		}

		// ============================================================
		// Phase 2: 组装系统提示词
		// 集成 G1 PromptCache: 稳定前缀 + 动态后缀 分层, 追踪本地 cache 命中率。
		// ============================================================
		systemPrompt := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)

		// 稳定/可变分层: 任务指令排在 hook 追加块**之前**。
		//
		// 顺序是「base(稳定) → task(本轮任务) → memory(检索结果)」, 理由是把
		// **易变的放最后**:
		//
		//   · task 块同一个业务键下每轮逐字节相同 (10K 字符级), 而 memory 块是
		//     BM25 对当前记忆库算出来的 top-K, 记忆库在每次 run 结尾被 memory
		//     拦截器写一次, 于是**下一次 run 的 top-K 与上一次必然不同** ——
		//     实测两次 t0 之间库从 21 条涨到 24 条, 注入从 18576 掉到 17939。
		//   · 前缀缓存断在第一个不同的字节。memory 排在 task 前面时, 那 637 字符
		//     的漂移会让它**后面**的整个 task 块一起作废: 实测跨 run 首轮只读到
		//     1024 token, 而仅仅把两者对调就能读到 ~4200。
		//
		// 历史: 这段注释原先写着"任务指令排在所有 hook 追加的块之后", 且拿
		// if-question-expand 的 system_chars 从 4.8KB 涨到 23.8KB 作为依据。那个
		// 观察本身没错, 但结论只对**同一个 run 内**成立 —— memory 在 t0 检索一次后
		// 整个 run 复用 (见 MemoryInjectHook.cachedIn), 所以 run 内它看着是稳定的。
		// 跨 run 时恰恰相反: 稳定的是 task, 漂移的是 memory。
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
				SentUpTo:     sentUpTo,
				// 任务指令此刻**已经在** system 里 (见上方"稳定/可变分层", 它排在
				// hook 追加块之前)。仍然单独传一份, 是因为 hook 需要的是"本轮任务
				// 正文"这个语义 —— 让它们各自去 SystemPrompt 里翻找是脆的: 末段在
				// 记忆块注入后就变成了记忆本身。
				TaskInstruction: e.TaskInstruction,
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

		// (任务指令已在 PhasePreRequest 之前追加, 见上方"稳定/可变分层"一段。)

		// 水位线推进点: 到这里 messages 已经定型, 下面就要发出去。
		//
		// 放在所有改写历史的 phase **之后**、StreamMessage **之前**。此刻的
		// len(messages) 就是"即将进入缓存前缀"的条数, 从此往后任何 hook 都不得
		// 再触碰 [0, sentUpTo)。重试分支 (fallback / PTL reactive compact) 会
		// 重新走一遍循环体, 届时水位线随新的 messages 一并刷新。
		sentUpTo = len(messages)

		// ============ Plan 模型路由: 规划期独立模型 (类似 advisor 独立客户端) ============
		// 放在 PhasePreRequest 之后判定: AutoModeHook 在该 phase 已置位会话级 plan flag,
		// 因此首个规划回合 (TurnCount==0) 也能切到 plan 客户端 (顶部 Path B 路由在此
		// 时点还读不到 flag)。优先级高于 ExecutionModel 相位路由: 规划期连续回合
		// sticky 在 plan 模型上 (共享前缀 KV 缓存), 直到模型调用 ExitPlanMode 清 flag
		// 才回落 主模型/ExecutionModel 现有路由。PlanClient=nil 或 flag OFF 时此块
		// 为空操作, 行为与旧版完全一致。
		if e.Config.PlanClient != nil && e.Config.DynamicPlanCheck != nil && e.Config.DynamicPlanCheck() {
			client = e.Config.PlanClient
			currentModel = e.Config.PlanClient.Model
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
			// PreRequest 是"每次调模型前"的位置 —— 挂在这里的 prompt hook 想影响的
			// 就是**这一次**请求, 所以必须在 messagesToAPI 之前落下。
			messages = e.injectHookContext(messages, hookOut, "PreRequest")
		}
		apiMessages := messagesToAPI(messages, e.HookRunner)
		allTools := e.Tools.APITools()
		var apiTools []types.APITool
		// ★ 白名单这一维的判据是 **!= nil (存在性)**, 不是 len() > 0。
		//
		// 这个 else 分支原本写着 `if len(DisabledTools) > 0 || len(AllowedTools) > 0`,
		// 而 AllowedTools 的契约是 "非 nil 的空集 = 拒绝一切" (见本文件 toolExposed 与
		// Config.AllowedTools 的注释)。空集长度是 0 ⇒ 走进 else ⇒ **apiTools 直接等于
		// 全部工具, 一次过滤都不做**。于是一份显式写的 fail-closed 白名单, 在唯一的
		// 单源判据之外被这个前置闸门整个架空: `toolExposed` 那几行是对的、被调用时也
		// 判得对, 只是**根本没人调它**。
		//
		// 更坏的是它对不上账: VisibleTools() 走的是 toolExposed (报告里"零个工具可见"),
		// 而出站请求走的是这里 (21 个工具照发) —— 装配对账会因为两边读的不是同一段代码
		// 而报"一致"。黑名单用 len() 无妨 (空黑名单与没有黑名单行为相同), 但为省掉一次
		// 遍历而把语义判错的收益是负数, 所以两维统一按存在性判。
		if e.Config.DisabledTools != nil || e.Config.AllowedTools != nil {
			for _, t := range allTools {
				if e.Config.toolExposed(t.Name) {
					apiTools = append(apiTools, t)
				}
			}
		} else {
			apiTools = allTools
		}
		// 方案三 L4 动态掩码 (使用感知渐进披露): API 面收窄到
		// 核心集 ∪ 本会话已实际调用的工具 ∪ ToolSearch; 执行面不拦。
		if e.Config.WeakModel.Enabled && e.Config.WeakModel.ToolMasking {
			apiTools = filterDynamicMaskedTools(apiTools, messages)
		}
		// 13.7-P0 统一池 (§13.7.2「注册不懒、广告懒」): 池内未加载资产移出广告面
		// (基础工具+包装三件+已加载面保留); 执行面不拦 —— 未加载资产仍可直调,
		// 结果不变 (平滑迁移: reg.pool 为 nil 时原样返回)。
		apiTools = e.Tools.Pool().FilterAPITools(apiTools)

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
		eventCh, errCh := client.StreamMessage(apiCtx, apiMessages, systemPrompt, apiTools, e.Config.MaxTokens)

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
					usage = mergeUsage(usage, event.Message.Usage)
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
						// 以当前块类型判定思考, 不只信 delta.Type: 部分 OpenAI 兼容
						// 网关会把 thinking 块内的增量标成 text_delta, 只按 delta.Type
						// 判断会让思维链漏进 --output-format json 的 final 字段。
						isThinking := currentBlock != nil && currentBlock.Type == types.ContentBlockThinking
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
								IsThinking: isThinking,
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
					usage = mergeUsage(usage, event.Usage)
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

		// llm_call Span (design/03 §4.1 E1 第 4 种 Kind)。
		//
		// 位置必须在读 errCh **之后**、错误分支 continue **之前**: 失败的调用同样是
		// 轨迹的一部分 (重试次数/PTL/换模型都是 action 的组成), 若只在成功路径写,
		// 学习管线看到的会是一串"一次就成"的假象。errCh 从 if 语句作用域提出来,
		// 下面的错误处理逐字未动。
		streamErr := <-errCh
		e.writeLLMCallSpan(apiCtx, llmCallSpan{
			Start: apiCallStart, Model: currentModel, System: systemPrompt,
			Messages: apiMessages, Tools: apiTools,
			Output: internal_hook.JoinAssistantText(assistantBlocks),
			Usage:  usage, StopReason: stopReason, Err: streamErr, Stream: true,
			Attempt: consecutiveErrors,
		})

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
		if streamErr != nil {
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
				// 兜底: 走到这里说明 OnError 链上的 error_classifier 与
				// circuit_breaker 都没返回 Terminal (例如 HookChain 为 nil)。
				// 此前这里是 `consecutiveErrors = 0` —— 重置计数器后继续跑,
				// 这正是"系统从不主动终止"的直接来源: 计到阈值只换来一次清零。
				// 现在改为真终止, 理由与 hook 侧区分开, 便于事后归因。
				if e.HookRunner != nil {
					e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
					e.HookRunner.ExecuteOnErrorHooks(messages, "circuit_breaker_no_hookchain", streamErr)
					e.HookRunner.ExecuteStopFailureHooks(messages)
				}
				e.firePhasePostTurn(ctx, messages, turnCount, currentModel, "circuit_breaker", assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
				return messages, types.Terminal{Reason: "circuit_breaker", Error: streamErr}
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
				// P0-1: 同一个 usage 也记进 run 级闸门。**必须写在这一处** ——
				// 口径与 CumulativeUsage 逐项一致是这道闸门可信的前提, 两处各算一遍
				// 必然漂移 (漂移的表现是"闸门说没超、账单说超了")。
				//
				// 只有在 usage != nil (即调用成功且有上报) 时才计费: 失败调用不带
				// usage 是正常的 (流里没有 message_start), 当成 0 计入会让失败的 run
				// 显得没花钱, 闸门等于没设。
				e.chargeRunBudget(ctx, usage)
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
					// L8: 检测 harness 自有 hook 的失败标记文本, 触发升级棘轮
					if !escalated && e.Config.FallbackModel != "" {
						for _, b := range m.Content {
							if b.Type != types.ContentBlockToolResult || !b.IsError {
								continue
							}
							schemaTerminal := strings.Contains(b.Content, "[schema_validate]") && strings.Contains(b.Content, "被终止")
							loopSuppressed := strings.Contains(b.Content, "系统检测到你连续") && strings.Contains(b.Content, "参数完全相同")
							if schemaTerminal || loopSuppressed {
								escalated = true
								logging.For("engine").Warn("L8 确定性失败信号触发升级",
									"to", e.Config.FallbackModel,
									"schema_terminal", schemaTerminal, "loop_suppressed", loopSuppressed)
							}
						}
					}
				}
				if preToolResult.InjectContinue && len(toolUseBlocks) == 0 {
					continue
				}
			}
		}

		// run 级预算检查 (P0-1)。**放在 tool 执行之后**, 与 maxTurns 同一位置。
		//
		// 为什么不放在 usage 累加的那一刻就返回: 那时 assistant 消息里的 tool_use
		// 还没执行, 直接返回会让 transcript 以一条**没有 tool_result 的 tool_use**
		// 收尾 —— 那不是"提前停下", 是留下了一段任何后续 turn 都发不出去的畸形历史
		// (H1/H3 要的正是"停下之后还能接着干")。代价是超支一个工具回合, 而那不会
		// 再触发下一次 LLM 调用, 上界是确定的。
		//
		// 顺序上让 max_turns 优先: 两个判据同时命中时, "步数用完了"是更准确的自述。
		turnCount++
		if e.RunBudget != nil && e.RunBudget.Exhausted() {
			stopped := fmt.Sprintf(
				"⛔ 本 run 的 token 预算已耗尽 (上限 %d, 已消耗 %d), 停止继续调用模型。",
				e.RunBudget.Limit(), e.RunBudget.Spent())
			logging.For("engine").Warn("run 预算耗尽, 终止",
				"limit", e.RunBudget.Limit(), "spent", e.RunBudget.Spent(),
				"turn", turnCount, "run", e.lastBudgetRunID)
			messages = append(messages, types.Message{
				Type:    types.MessageTypeUser,
				Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: stopped}},
				IsMeta:  true,
			})
			if e.HookRunner != nil {
				e.HookRunner.ExecutePostTurnHooks(messages, turnCount)
			}
			e.firePhasePostTurn(ctx, messages, turnCount, currentModel, internal_hook.RunBudgetExhaustedReason, assistantBlocks, turnToolSigs, turnUserIntent, turnPlanMsgs, turnStart, false)
			return messages, types.Terminal{
				Reason: internal_hook.RunBudgetExhaustedReason,
				Error: fmt.Errorf("run token budget exhausted: limit=%d spent=%d",
					e.RunBudget.Limit(), e.RunBudget.Spent()),
			}
		}
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
			ExecutionModel:     e.Config.ExecutionModel, // delegate_task 用它确定子代理模型
			IsNonInteractive:   e.Config.IsNonInteractive,
			Debug:              e.Config.Debug,
			Messages:           messages,
			MaxToolResultChars: internal_hook.MicroCompactMaxChars,
			// 方案三 L3: 弱模型 tool_result 确定性后处理 (单行截断+保头尾)。
			WeakResultPostprocess: e.Config.WeakModel.Enabled && e.Config.WeakModel.ResultPostprocess,
			// 方案三 L2 护栏: edit/write 写入前语法校验 (与 SchemaRetry 同属 L2 开关)。
			WeakEditGuard: e.Config.WeakModel.Enabled && e.Config.WeakModel.SchemaRetry,
			SessionID:     e.Config.SessionID,   // EnterPlanMode/ExitPlanMode 用会话级 plan flag
			PlanFileDir:   e.Config.PlanFileDir, // ExitPlanMode 将计划落盘供实施阶段读取
			// 13.7-P1 池个性化身份透传 (team_stage 路径才有值; CLI/主会话为空)。
			Team:       e.Config.Team,
			Role:       e.Config.Role,
			GlobalPerm: e.PermChecker,
		}

		toolExecStart := time.Now()
		toolResults := tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, e.HookRunner)
		toolExecLatency := time.Since(toolExecStart)

		// run 级进展: 有任何一个工具**成功执行**, 就认为这个 run 还在产出,
		// 清空无进展错误账本 (见 internal_hook.RunFailureLedgerWindow)。
		//
		// 判据刻意用"工具成功"而不是"模型回了话": 后者在退避重试的 run 里
		// 每次都会发生, 拿它当进展就等于从不推进基线 —— 那正是交错失败
		// 一路空转到 maxTurns 的原因。也不计入 IsError 的结果, 因为失败的工具
		// 调用不构成产出 (与上方 TrajStore 的 ok 判定同源)。
		if e.ErrClassifier != nil && toolProducedWork(toolResults) {
			e.ErrClassifier.NoteProgress()
		}

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

// filterDynamicMaskedTools 方案三 L4 动态掩码 (使用感知渐进披露)。
//
// 可见 = CoreVisibleTools ∪ 本会话已实际调用过的工具 (粘性) ∪ ToolSearch (逃生通道)。
// 与静态掩码 (DisabledTools, 进硬门) 的分工: 这里只收窄下发给模型的工具表,
// 不影响执行面——模型经 ToolSearch 发现未暴露工具后仍可调用成功, 这正是
// "渐进披露"与"硬掩码"的安全差异 (13.3.9 偏差 B2 的动态版落地)。
func filterDynamicMaskedTools(tools []types.APITool, messages []types.Message) []types.APITool {
	used := make(map[string]bool, 16)
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolUse && b.Name != "" {
				used[b.Name] = true
			}
		}
	}
	out := make([]types.APITool, 0, len(tools))
	for _, t := range tools {
		if weakmodelCoreSet[t.Name] || used[t.Name] || t.Name == "ToolSearch" {
			out = append(out, t)
		}
	}
	return out
}

// weakmodelCoreSet CoreVisibleTools 的查找集 (包级初始化一次)。
var weakmodelCoreSet = func() map[string]bool {
	m := make(map[string]bool, len(weakmodel.CoreVisibleTools))
	for _, n := range weakmodel.CoreVisibleTools {
		m[n] = true
	}
	return m
}()

// isToolExecTurn 判定下一轮 LLM 调用是否处于"工具消费回合":
// 消息尾部最后一条是 User 消息且携带 tool_result 内容块 (工具执行后 append 的形态)。
// Path B 用它把工具消费回合路由到快速执行模型, 规划/推理回合留在主模型。
func isToolExecTurn(messages []types.Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if last.Type != types.MessageTypeUser {
		return false
	}
	for _, b := range last.Content {
		if b.Type == types.ContentBlockToolResult {
			return true
		}
	}
	return false
}

// isDelegationResultTurn 判定当前工具消费回合消费的是否是"子代理委派"的结果:
// 委派报告需要主模型综合, 不应路由到快速执行模型 (Path B 路由例外)。
func isDelegationResultTurn(messages []types.Message) bool {
	return tool.IsDelegationTool(lastToolUseName(messages))
}

// lastToolUseName 从消息尾部最近一条 tool_result 的 ToolUseID, 反向找到对应的
// assistant tool_use 并返回其工具名。找不到返回 ""。
// 同一回合混用委派+普通工具时, 取**最后完成**的工具 (tool_results 追加顺序),
// 这是可接受启发式 —— nudge 引导规划方分开委派。
func lastToolUseName(messages []types.Message) string {
	// ① 尾部最近的 tool_result 的 ToolUseID
	var id string
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Type != types.MessageTypeUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolResult && b.ToolUseID != "" {
				id = b.ToolUseID
			}
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		return ""
	}
	// ② 反向找携带该 ID 的 assistant tool_use
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Type != types.MessageTypeAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolUse && b.ID == id {
				return b.Name
			}
		}
	}
	return ""
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

// maxInt 返回两整数中的较大者。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// toolProducedWork 报告本批工具调用里是否至少有一个**成功**完成。
//
// 语义是 run 级"有没有产出", 供 ErrorClassifier.NoteProgress 使用:
// 全部失败 = 没有产出, 账本继续累积; 任一成功 = 有产出, 账本清零。
//
// 空批次 (模型没调工具) 返回 false —— 那种情况下 run 的进展由模型自己
// 的终止决定, 不该被记为产出。
func toolProducedWork(toolResults []types.Message) bool {
	for _, msg := range toolResults {
		hasResult, failed := false, false
		for _, cb := range msg.Content {
			if cb.Type != types.ContentBlockToolResult {
				continue
			}
			hasResult = true
			if cb.IsError {
				failed = true
			}
		}
		if hasResult && !failed {
			return true
		}
	}
	return false
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

	// 优化1 [已停用]: 过滤不含 reasoning 的古老 assistant + tool_result 原子单元。
	//
	// 停用原因: 它是**每轮重算**的滑动窗口 —— 窗口随 len(messages) 前移, 于是一条
	// 上一轮还在的消息这轮被删掉, 后面所有消息整体前移一个位置。前缀缓存按字节
	// 比对, "删中间" 比"改写"更彻底: 改写至少还留着断点之前的部分, 删除点之后的
	// 全部前缀一起作废。历史长度应当由 AutoCompact 的折叠边界一次性收敛, 而不是
	// 每轮动态裁剪。
	//
	// 若将来要恢复类似能力, 必须做成"只作用本轮新增消息"的形态, 而不是全量重算。

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
