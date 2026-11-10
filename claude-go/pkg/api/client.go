// Package api 实现 Anthropic Messages API 客户端。
// 对应 TS 源码: review/claude/src/services/api/claude.ts
//
// 支持:
//   - 流式 (SSE) 和非流式请求
//   - Anthropic 兼容 API (如 DashScope)
//   - 工具定义传递
//   - token 使用量追踪
//   - 模型切换与回退
//   - signature_delta 签名验证
//   - server_tool_use 服务端工具调用处理
//   - refusal stop_reason 处理
//   - prompt_too_long (PTL) 错误恢复
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// PromptTooLongError 标识 API 返回 prompt_too_long 错误。
// 对应 TS: services/api/errors.ts 中的 PromptTooLongError
// queryLoop 捕获此错误后触发 reactive compact (紧急压缩)。
type PromptTooLongError struct {
	Message string
}

// Error 实现 error 接口, 直接返回上游的原始消息。
func (e *PromptTooLongError) Error() string { return e.Message }

// OverloadedError 标识 API 返回 overloaded 错误 (529)。
// 对应 TS: services/api/claude.ts 中的 overloaded 处理
type OverloadedError struct {
	Message string
}

// Error 实现 error 接口, 直接返回上游的原始消息。
func (e *OverloadedError) Error() string { return e.Message }

// LLMEventFunc 回调: LLM 调用发生重试/熔断等事件时通知外部 (如飞书)。
// eventType: "retry", "circuit_open", "circuit_close", "fatal"
type LLMEventFunc func(eventType, detail string)

// LLMCallRecord 单次 LLM 调用的可观测数据, 用于采集 token / 延迟 / 成败。
type LLMCallRecord struct {
	Model       string
	BaseURL     string
	Status      string // "success" | "error" | "retry_success"
	Stream      bool
	DurationSec float64
	// TTFTMs: 请求发出 → 首个正文/思考/工具入参增量的毫秒数（仅流式有意义）。
	// 与 DurationSec 是两个口径：DurationSec 含准入等待与整段生成，TTFTMs 只量
	// "第一块可见产出什么时候到"。0 = 没有采集到（非流式 / 一帧内容都没有）。
	TTFTMs              float64
	InputTokens         int
	InputEstimated      bool // input 由 prompt 字符数估算 (兼容网关不回 input_tokens; design/02 §1.4)
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	TotalTokens         int
	HTTPStatus          int    // 最终的 HTTP 状态码 (可能是 200)
	Retries             int    // 本次调用内部触发的重试次数
	ErrorKind           string // "timeout"|"rate_limit"|"overloaded"|"prompt_too_long"|"refusal"|"client"|"server"|""
	ErrorMessage        string // 截断后的错误信息
	StopReason          string // "end_turn"|"max_tokens"|"tool_use"|"refusal"|...
	Timestamp           time.Time

	// 追踪信息 (由 Client.Tag 或调用方 Context 注入, 支持按业务维度聚合)
	//
	// Owner 是租户/用户作用域（R-H6）。来源是 ctx 里的 types.WithOwner —— 与
	// engine/trace 同源，不另设一份"这次调用属于谁"。空值在采集器侧归一为
	// "local"（types.OwnerFromContext 本身也会把缺失归一为默认 owner）。
	Owner    string
	// App 是调用方 APP 来源标识（studyforge / claimed:moqi / internal:cron /
	// unknown），与 Owner 正交：owner 答"为谁服务"，app 答"谁调进来"。
	// 同源从 ctx 读（types.AppFromContext），缺失归一 "unknown"（pkg/types/app.go）。
	App      string
	Source   string // "chat" | "feishu" | "team" | "swarm" | "dashboard" | "vision" | "compact" | ...
	Purpose  string // 额外标签, 如团队名 / stage 名 / agent 角色
	Request  string // 粗粒度 HTTP 方法标签 (messages / stream-messages)
	Workflow string // Agent Team 工作流名, 如 development / research
	Role     string // Agent 角色名, 如 coder / tester / reviewer

	// 限流 / 熔断 观测 (用于 dashboard 速率与守护页面)
	GuardWaitSec   float64 // 本次 RateLimitGuard.Acquire 等待时长 (包含 RPM 令牌 + 退避)
	CircuitOpened  bool    // 本次调用触发了熔断 (从 closed 变 open)
	CircuitBlocked bool    // 本次调用被熔断器拒绝 (未发起真实请求)

	// PromptComponents 记录本次请求中各类提示词/上下文组件的字符数。
	// 由 QueryEngine 在请求前计算, api.Client 只负责随 LLMCallRecord 透传。
	PromptComponents PromptComponentMetrics

	// Trace 四元组 (pkg/trace, design/03 §4.1 E0): 关联 llm.jsonl ↔ 团队轨迹 ↔ transcript,
	// 修复"llm.jsonl 无 run/turn 关联键, 跨源只能按时间戳粗对齐"(design/03 §1.3)。
	RunID  string // episode 级 (团队一次执行 / 会话任务); 上游未注入时为空
	NodeID string // 图节点 / workflow stage 级
	TurnID string // QueryEngine 单轮
	CallID string // 单次 LLM HTTP 调用 (api.Client 生成, 恒非空)
}

// estimateInputTokens 按 prompt 组件字符总数估算 input token (chars/4)。
// 用于兼容网关不回 input_tokens 时的双边记账兜底 (design/02 §1.4)。
// CJK 偏保守 (1 token≈1.5 汉字), 但显著优于记 0。
func estimateInputTokens(pc PromptComponentMetrics) int {
	chars := pc.SystemChars + pc.ToolsSchemaChars + pc.MCPToolsChars +
		pc.SkillListingChars + pc.RoleSkillsChars + pc.MemoryChars +
		pc.BlackboardChars + pc.PrevResultChars + pc.MessagesChars
	if chars <= 0 {
		return 0
	}
	return chars / 4
}

// stampTraceIDs 把 trace 四元组盖到记录上; CallID 每条记录独立生成。
func stampTraceIDs(rec *LLMCallRecord, ids trace.IDs) {
	if rec == nil {
		return
	}
	rec.RunID, rec.NodeID, rec.TurnID = ids.RunID, ids.NodeID, ids.TurnID
	if rec.CallID == "" {
		rec.CallID = trace.NewID("c")
	}
}

// LLMMetricsHook 采集 LLM 调用指标的回调 (dashboard 在启动时注入, 避免循环依赖)。
type LLMMetricsHook func(rec LLMCallRecord)

// CachePlan 是调用方给出的缓存断点计划。零值 = 沿用内置启发式。
//
// 为什么需要它 (R-H2): 断点位置决定了哪些前缀能独立成缓存条目, 而"哪一段稳定、
// 哪一段每轮都变"只有组装 prompt 的一方知道 (harness 的 PromptLayout 就带着这个
// 分层)。如果 api 层继续自己按"最后一条 + 最近的 assistant"猜一遍, 调用方的布局
// 就只是**观测**, 永远影响不到真正发出去的请求 —— 观测与行为两套口径, 调参时看到
// 的命中率变化根本不能归因到布局上。这里把选择权交出去, 同时保留内置启发式作为
// nil 的兜底: 不传计划的老调用方一个字节都不会变。
//
// nil 与空切片的区别是**语义**上的, 不是实现细节:
//
//	SystemIndexes == nil      → 用内置 {0, 稳定段末尾}
//	SystemIndexes == []int{}  → 明确表示"system 上不打任何断点"
//
// 把空切片也当成 nil 会让"我要关掉 system 断点"表达不出来, 只能被迫接受默认值。
type CachePlan struct {
	// SystemIndexes 是 system 块下标; nil = 内置 {0, 稳定段末尾}。
	//
	// 下标按 systemPrompt 切分后的块计 (engine 的分层顺序:
	// base → memory → task, 最后一块每轮都变, 打在它上面等于每轮换缓存键)。
	SystemIndexes []int
	// MessageIndexes 是 messages 下标; nil = 内置 cacheBreakpointIndexes(messages, 2)。
	//
	// 断点落在该条消息**最后一个内容块**的末尾 (与 markCacheBreakpointAt 一致),
	// 因为前缀缓存按块边界切分, 打在块中间没有意义。
	MessageIndexes []int
}

// Client Anthropic Messages API 客户端。
type Client struct {
	BaseURL string
	APIKey  string
	Model   string

	// Protocol 出站协议: ""/"anthropic"(默认, POST /messages)、"openai"
	// (POST /chat/completions, 走 OpenAI 翻译层) 或 "openai-responses"
	// (POST /v1/responses, OpenAI Responses API 翻译层)。按模型配置注入 ——
	// 例如 opencode:mimo-v2.5 的 tool_use 必须走 OpenAI 协议 (opencode 的
	// Anthropic→OpenAI 转换层丢 name 字段返回 400), opencode:muse-spark-1.2-contributor
	// 走 OpenAI Responses API (见 pkg/api/openai.go / responses.go 头注)。
	Protocol string

	// Proxy 出站 HTTP 代理 URL (如 http://100.96.50.63:8888)。模型级配置:
	// 只有配置了 proxy 的模型才走代理 (经 SetProxy 注入 http.Transport.Proxy),
	// 其余模型保持直连, 不消耗代理出口流量。
	Proxy string

	Client *http.Client

	RetryCount int           // 最大重试次数 (429/5xx), 默认 4; <0 表示显式禁用
	RetryBase  time.Duration // 退避基数, 默认 3s
	RetryMax   time.Duration // 退避上限, 默认 60s

	// RetryStatuses 是显式配置的可重试状态码集合 (见 modelconfig.ModelConfig.RetryStatuses)。
	// nil/空 = 沿历史默认: 429/503/529/5xx (即 isRetryableStatus), **不含 400**。
	// 非空时**完全替换**默认集合 —— 这样"只重试 400 和 502"这种策略才表达得出来。
	//
	// 为什么与 RetryCount 分开: 次数与"哪些状态值得重试"是两个问题。历史默认不含
	// 400 是为了保护翻译路径上的既有行为; 而网关透传路径的默认策略另有 400
	// (见 RetryPolicyFromConfig / DefaultGatewayRetryPolicy)。
	RetryStatuses []int

	// 首token超时分离 (模型级可配置, 0 = 使用全局默认值)
	FirstTokenTimeout time.Duration // 首token等待时间, 默认 180s
	CallTimeout       time.Duration // 整体调用超时, 默认 600s
	DeadlineRetryBase time.Duration // deadline exceeded 退避基数, 默认 10s

	// ExtraHeaders 附加到每次出站 HTTP 请求的自定义头 (与默认头重名时覆盖默认值)。
	// 请求级注入: 调用方在 SendMessage / StreamMessage 之前设置, 不参与 Client 克隆。
	// 用途: anthropic-gateway 等协议网关按入站会话注入 x-opencode-session /
	// User-Agent (opencode zen/go 上游缺失会话头会拒 400 MissingSessionID)。
	ExtraHeaders http.Header

	OnLLMEvent   LLMEventFunc    // 事件回调 (可选, 注入飞书通知)
	OnLLMMetrics LLMMetricsHook  // 指标回调 (可选, 注入 dashboard metrics collector)
	Guard        *RateLimitGuard // 全局准入控制器 (可选, 强烈建议设置)

	// CallInterceptors 本客户端专属的 LLM 调用切面 (design/01 §4.10)。
	// 进程级链见 RegisterCallInterceptor —— 那个对所有克隆生效, 这个只影响本实例
	// (装配时进程级在外层)。多数场合应该用进程级: Client 被 WithModel/
	// ConfiguredClone 大量克隆, 逐个装必漏。
	CallInterceptors []CallInterceptor

	// ForceToolChoiceAny 强制 tool_choice=any (方案三 L1 裁定项)。
	// 默认 false; 由 WithToolChoiceAny 副本开启, 仅对携带 tools 的请求生效。
	// 纪律: 全局强制会令模型无法 end_turn 自然收尾, 只允许在执行回合临时启用。
	ForceToolChoiceAny bool

	// FallbackModels 备用模型列表: 主模型不可用时按序尝试。
	// 触发条件: 模型不支持 (400 invalid model) / 超载 (529) / 配额耗尽 (402/403)。
	FallbackModels []string

	// FallbackBaseURL / FallbackAPIKey 备用模型独立端点配置。
	// 若为空则继承主模型的 BaseURL / APIKey。
	FallbackBaseURL string
	FallbackAPIKey  string

	// onFallback 是回退观测回调 (R-H17, 见 fallback_observe.go)。
	// nil = 不观测, 热路径只多一次判断。放在 Client 上而不是逐次传参, 是因为
	// 回退发生在重试循环内部, 调用点没有机会把回调递进去。
	onFallback func(FallbackInfo)

	// PromptCacheEnabled 启用 Anthropic prompt caching (顶层 cache_control)。
	// "auto" = 自动检测 (默认，Anthropic 官方 API 启用，其他关闭)
	// "on"   = 强制启用
	// "off"  = 强制关闭
	PromptCacheMode            string
	promptCacheDisabledByError bool // 因 API 错误自适应关闭

	// CachePlan 是调用方给出的缓存断点计划 (R-H2); nil = 沿用内置启发式。
	// json:"-" 是刻意的: Client 不是请求体的一部分, 而计划本身也不该被序列化
	// 到任何地方 (它描述的是"怎么打点", 不是"打在哪里" —— 落盘的是打点结果)。
	//
	// 请求级注入 (与 ExtraHeaders 同): 调用方在 SendMessage / StreamMessage 之前
	// 设置。WithModel / ConfiguredClone 会把它**带过去** —— 这一条不是顺手为之:
	// engine 的相位路由每个回合都会用 WithModel 造一个临时副本 (ExecutionModel /
	// FallbackModel), 不传播就会让"执行模型回合"悄悄退回内置启发式。缓存计划的
	// 失效是无声的 (请求照常成功, 只是命中率变了), 正是本改动要消灭的那类缺陷。
	CachePlan *CachePlan `json:"-"`

	// ReasoningPlayback 出站回传上一轮推理所用的 OpenAI 字段名。
	// "" = 不回传; "reasoning" = messages[].reasoning; "reasoning_content" =
	// messages[].reasoning_content。**只对 protocol:"openai" 有意义**, anthropic
	// 原生协议不需要 (它的 thinking 块本来就跟着历史走), responses 协议另有形态。
	//
	// 模型级配置 (见 modelconfig.ModelConfig.ReasoningPlayback): 各家对回传的接受度
	// 相反, DeepSeek 会直接拒, 所以这里不做自动嗅探也不提供全局开关 —— 由配置逐台决定。
	ReasoningPlayback string

	// PromptDebugEnabled 开启后将每次发给 LLM 的请求和响应落盘。
	// 注意: 文件包含完整 prompt / tool schema / user content, 仅建议排查 token 问题时短期开启。
	PromptDebugEnabled    bool
	PromptDebugDir        string
	PromptDebugMaxFiles   int
	PromptDebugMaxBytes   int64
	PromptDebugSampleRate float64
	PromptDebugRedact     bool

	// 追踪标签 (可选): 调用方通过 WithTag 或直接赋值, 标识该 Client 实例所服务的业务场景。
	// 会写入 LLMCallRecord.Source, 方便在 dashboard 里按业务维度聚合 (chat/feishu/team/...)。
	Tag string

	// 智能模型切换: 连续 429 达到阈值时自动切换备用模型, 冷却后回退主模型
	fbMu                sync.Mutex
	consecutive429      int       // 连续 429 次数
	fallbackActiveModel string    // 当前激活的备用模型 (空=使用主模型)
	fallbackActiveSince time.Time // 切换到备用模型的时刻
	fallbackCooldownMin int       // 冷却时间(分钟), 默认 5, 到期后尝试恢复主模型

	// 熔断器
	cbMu             sync.Mutex
	consecutiveFails int
	circuitOpen      bool
	circuitOpenUntil time.Time
	cbThreshold      int // 默认 5

	// 统计
	TotalRetries atomic.Int64
	TotalFails   atomic.Int64
	CircuitTrips atomic.Int64
}

// WithModel 返回一个共享同一 HTTP 客户端和限流器的轻量 Client 副本，仅覆盖 Model。
// 用于为不同 plan/role 指定不同模型而不影响原始 Client 的 Model 字段。
func (c *Client) WithModel(model string) *Client {
	return &Client{
		BaseURL:               c.BaseURL,
		APIKey:                c.APIKey,
		Model:                 model,
		Protocol:              c.Protocol,
		ReasoningPlayback:     c.ReasoningPlayback,
		Proxy:                 c.Proxy,
		Client:                c.Client,
		Guard:                 c.Guard,
		RetryCount:            c.RetryCount,
		RetryBase:             c.RetryBase,
		RetryMax:              c.RetryMax,
		RetryStatuses:         c.RetryStatuses,
		OnLLMEvent:            c.OnLLMEvent,
		OnLLMMetrics:          c.OnLLMMetrics,
		FallbackModels:        c.FallbackModels,
		PromptCacheMode:       c.PromptCacheMode,
		PromptDebugEnabled:    c.PromptDebugEnabled,
		PromptDebugDir:        c.PromptDebugDir,
		PromptDebugMaxFiles:   c.PromptDebugMaxFiles,
		PromptDebugMaxBytes:   c.PromptDebugMaxBytes,
		PromptDebugSampleRate: c.PromptDebugSampleRate,
		PromptDebugRedact:     c.PromptDebugRedact,
		cbThreshold:           c.cbThreshold,
		fallbackCooldownMin:   c.fallbackCooldownMin,
		Tag:                   c.Tag + ":" + model,
		CachePlan:             c.CachePlan,
		// 观测回调随克隆传播: engine 每轮用 WithModel 造临时副本, 不传播的话
		// 回退观测在真正执行的副本上静默消失 (请求照常成功, 只是没有回退链)。
		onFallback: c.onFallback,
	}
}

// ConfiguredClone 返回一个共享 HTTP 客户端和限流器的轻量 Client 副本，
// 使用指定参数覆盖 baseURL / apiKey / model / fallbackModels。
// 用于为不同 plan 使用完全不同的 API 端点而不影响原始 Client。
func (c *Client) ConfiguredClone(baseURL, apiKey, model string, fallbackModels []string) *Client {
	return c.ConfiguredCloneFull(baseURL, apiKey, model, fallbackModels, "", "")
}

// ConfiguredCloneFull 返回一个共享 HTTP 客户端和限流器的轻量 Client 副本，
// 使用指定参数覆盖 baseURL / apiKey / model / fallbackModels / fallbackBaseURL / fallbackAPIKey。
func (c *Client) ConfiguredCloneFull(baseURL, apiKey, model string, fallbackModels []string, fallbackBaseURL, fallbackAPIKey string) *Client {
	clone := &Client{
		BaseURL:               strings.TrimRight(baseURL, "/"),
		APIKey:                apiKey,
		Model:                 model,
		Protocol:              c.Protocol,
		ReasoningPlayback:     c.ReasoningPlayback,
		Proxy:                 c.Proxy,
		Client:                c.Client,
		Guard:                 c.Guard,
		RetryCount:            c.RetryCount,
		RetryBase:             c.RetryBase,
		RetryMax:              c.RetryMax,
		RetryStatuses:         c.RetryStatuses,
		OnLLMEvent:            c.OnLLMEvent,
		OnLLMMetrics:          c.OnLLMMetrics,
		PromptCacheMode:       c.PromptCacheMode,
		PromptDebugEnabled:    c.PromptDebugEnabled,
		PromptDebugDir:        c.PromptDebugDir,
		PromptDebugMaxFiles:   c.PromptDebugMaxFiles,
		PromptDebugMaxBytes:   c.PromptDebugMaxBytes,
		PromptDebugSampleRate: c.PromptDebugSampleRate,
		PromptDebugRedact:     c.PromptDebugRedact,
		cbThreshold:           c.cbThreshold,
		fallbackCooldownMin:   c.fallbackCooldownMin,
		Tag:                   c.Tag,
		// 计划跨端点克隆也带过去 (与 WithModel 同理): 备用/plan 端点的缓存
		// 布局应当与主端点一致, 否则同一段 prompt 在两个端点上命中率不同,
		// 而差异来源是"克隆时漏了一个字段" —— 无从排查。
		CachePlan: c.CachePlan,
		// 回退观测同样跨克隆传播 (理由见 WithModel)。
		onFallback: c.onFallback,
	}
	if len(fallbackModels) > 0 {
		clone.FallbackModels = fallbackModels
	} else {
		clone.FallbackModels = c.FallbackModels
	}
	if fallbackBaseURL != "" {
		clone.FallbackBaseURL = strings.TrimRight(fallbackBaseURL, "/")
	}
	if fallbackAPIKey != "" {
		clone.FallbackAPIKey = fallbackAPIKey
	}
	return clone
}

// NewClient 创建 API 客户端 (内置 429/5xx 重试 + 熔断)
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		RetryCount:        4,
		RetryBase:         3 * time.Second,
		RetryMax:          60 * time.Second,
		cbThreshold:       5,
		FirstTokenTimeout: 180 * time.Second,
		CallTimeout:       600 * time.Second,
		DeadlineRetryBase: 10 * time.Second,
	}
}

// SetTag 为该 Client 实例设置业务追踪标签 (chat / feishu / team / dashboard / ...),
// 该标签会写入 LLMCallRecord.Source, 方便 dashboard 按业务维度聚合 LLM 调用指标。
// 直接修改原 Client, 不做拷贝 (Client 内含 sync.Mutex / atomic, 浅拷贝会被 go vet 拒绝)。
func (c *Client) SetTag(tag string) *Client {
	if c == nil {
		return nil
	}
	c.Tag = tag
	return c
}

// SetEndpoint 原地更新上游地址与密钥 (配置热加载用)。
//
// 刻意做成"改原对象"而不是"返回新 Client": Client 上的限流 Guard、熔断计数、
// PromptCache 状态位要跨克隆共享 (见 pkg/feishu/bot.go 里 SessionManager 那段
// 注释), 换一个新实例等于把这些计数清零 —— 热加载一次就丢掉配额与熔断记录。
// 与 SetTag/SetProxy 同一约定: 直接修改原 Client, 不做拷贝。
//
// 空值跳过: 热加载时若新配置缺某字段, 保持原值比清空更安全。
func (c *Client) SetEndpoint(baseURL, apiKey string) {
	if c == nil {
		return
	}
	if baseURL != "" {
		c.BaseURL = baseURL
	}
	if apiKey != "" {
		c.APIKey = apiKey
	}
}

// SetProxy 为该 Client 配置出站 HTTP 代理 (仅配置了 proxy 的模型调用此方法)。
// proxy 为空是安全的 no-op (保持直连); 非空时在共享 http.Client 上注入带
// http.ProxyURL 的 Transport, 让该模型的所有出站请求经代理转发。
// 注意: Transport 是进程共享的 (NewClient/克隆共享 c.Client), 因此这里
// 用 Clone() 复制一份再改 Proxy, 避免污染其他 Client 的直连行为。
func (c *Client) SetProxy(proxy string) {
	if c == nil || c.Client == nil {
		return
	}
	c.Proxy = proxy
	if proxy == "" {
		return
	}
	u, err := url.Parse(proxy)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Printf("[api.Client] 忽略非法 proxy %q (err=%v): 该模型保持直连", proxy, err)
		return
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(u)
	c.Client.Transport = base
}

// CircuitSnapshot 熔断器的结构化状态, 供 dashboard / diagnose 端点使用。
type CircuitSnapshot struct {
	Open             bool      `json:"open"`             // 当前是否熔断
	OpenUntil        time.Time `json:"openUntil"`        // 本次熔断的解除时刻
	OpenSecondsLeft  float64   `json:"openSecondsLeft"`  // 距离解除的秒数 (0=正常)
	ConsecutiveFails int       `json:"consecutiveFails"` // 当前连续失败计数
	Threshold        int       `json:"threshold"`        // 触发阈值
	TotalRetries     int64     `json:"totalRetries"`     // 累计重试次数
	TotalFails       int64     `json:"totalFails"`       // 累计失败次数
	CircuitTrips     int64     `json:"circuitTrips"`     // 累计触发熔断次数
	Timestamp        time.Time `json:"timestamp"`
}

// GetCircuitSnapshot 返回该 Client 的熔断器运行态快照。
// 与 Snapshot() 配合, 供 dashboard LLM guard 页面展示。
func (c *Client) GetCircuitSnapshot() CircuitSnapshot {
	if c == nil {
		return CircuitSnapshot{Timestamp: time.Now()}
	}
	c.cbMu.Lock()
	open := c.circuitOpen
	openUntil := c.circuitOpenUntil
	fails := c.consecutiveFails
	threshold := c.cbThreshold
	c.cbMu.Unlock()
	left := time.Until(openUntil)
	if left < 0 || !open {
		left = 0
	}
	if threshold <= 0 {
		threshold = 5
	}
	return CircuitSnapshot{
		Open:             open,
		OpenUntil:        openUntil,
		OpenSecondsLeft:  left.Seconds(),
		ConsecutiveFails: fails,
		Threshold:        threshold,
		TotalRetries:     c.TotalRetries.Load(),
		TotalFails:       c.TotalFails.Load(),
		CircuitTrips:     c.CircuitTrips.Load(),
		Timestamp:        time.Now(),
	}
}

// isRetryableStatus 判断 HTTP 状态码是否可重试 —— 这是**历史默认集合**:
// 429/503/529/5xx, 不含 400。保留为包级函数, 供未配置 RetryStatuses 的 Client 与
// 既有测试使用。
func isRetryableStatus(code int) bool {
	return code == 429 || code == 503 || code == 529 || code >= 500
}

// retryableStatus 是本次调用实际使用的状态码判断。
//
// 配置了 RetryStatuses 时**完全替换**默认集合 (含 400 的配置才能生效);
// 未配置时逐字节保持历史行为 —— 这是"零值零影响"的底线。
func (c *Client) retryableStatus(code int) bool {
	if c != nil && len(c.RetryStatuses) > 0 {
		for _, s := range c.RetryStatuses {
			if s == code {
				return true
			}
		}
		return false
	}
	return isRetryableStatus(code)
}

// maxRetries 返回本次调用允许的额外重试次数。
//
// 负数是显式禁用 (模型配置 retryCount:-1 映射而来)。此前 <=0 一律回落 4,
// 于是"关掉重试"根本表达不出来 —— 配置 0 与不配置是同一个结果。
func (c *Client) maxRetries() int {
	if c == nil {
		return 4
	}
	if c.RetryCount < 0 {
		return 0
	}
	if c.RetryCount == 0 {
		return 4
	}
	return c.RetryCount
}

// shouldEnablePromptCache 判断当前请求是否应启用 prompt caching。

// MaxCacheBreakpoints 是上游允许的 cache_control 块数上限 (Anthropic 文档: 最多 4)。
//
// 超出的标记会被上游拒绝 —— 而"拒绝"表现为整个请求 400, 不是静默丢弃断点。
// 所以这里是硬上限, 不是建议值。
const MaxCacheBreakpoints = 4

// markCacheBreakpointAt 给 messages[idx] 的最后一个内容块贴 cache_control。
// 返回是否真的贴上了 (目标不存在 / 形态不支持 / 已有断点 都返回 false)。
//
// Content 是 json.RawMessage, 所以要先解出来再按形态处理 —— 纯字符串形态要包成
// 内容块才能挂字段。已有 cache_control 的块不重复贴 (上游对重复标记会报错)。
//
// ★ 插入必须**只加字段, 不动其余任何一个字节**。
//
// 这里原先走的是 `var content any` → 改最后一个块 → `json.Marshal(content)` 回写。
// 那条路会把整条消息的**全部内容块**重新序列化一遍, 而 tool_use 的 input 是
// json.RawMessage —— 解进 any 就成了 map[string]any, 再 Marshal 出来**键按字典序
// 重排**。实测 (if-question-expand 2fd81b6e):
//
//	模型原文        {"path":"/workspace/write_questions.py","contents":"..."}
//	贴了断点那轮    {"contents":"...","path":"/workspace/write_questions.py"}
//	下一轮          {"path":"...","contents":"..."}     ← 又变回去了
//
// 为什么"下一轮又变回去": 断点的**落点每轮都在移动** (BP3=最后一条消息,
// BP2=最后一条 assistant), 于是同一条 tool_use 这轮被打上、下轮不被打上, 两轮渲染
// 的字节不同 —— 前缀缓存断在它身上, 它后面整段作废。这与 run 长短无关, **每轮必中**
// (实测 t3→t4 首个不同字节 @29796、t5→t6 @35895, 都恰好是这个 tool_use 的参数串)。
//
// 所以改成字节级插入: 把最后那个块的收尾 `}` 前塞进 cache_control, 其余字节原样保留。
// 这类"只加不改"的改写才是缓存友好的 —— 同一条消息无论这轮贴不贴断点, 贴之前的那段
// 前缀都逐字节相同。
func markCacheBreakpointAt(messages []types.APIMessage, idx int) bool {
	if idx < 0 || idx >= len(messages) {
		return false
	}
	target := &messages[idx]
	if target.Role != "user" && target.Role != "assistant" {
		return false
	}

	// 纯字符串形态 (部分调用方直接传字符串): 没有可保留的原文, 包成内容块即可。
	var s string
	if json.Unmarshal(target.Content, &s) == nil {
		if s == "" {
			return false
		}
		raw, err := json.Marshal([]map[string]any{
			{"type": "text", "text": s, "cache_control": map[string]string{"type": "ephemeral"}},
		})
		if err != nil {
			return false
		}
		target.Content = raw
		return true
	}

	// 内容块数组: 逐个保持**原始字节**, 只对最后一块做插入。
	var blocks []json.RawMessage
	if err := json.Unmarshal(target.Content, &blocks); err != nil {
		return false // 解不出来就原样放行, 不动它
	}
	if len(blocks) == 0 {
		return false
	}
	last := bytes.TrimRight(blocks[len(blocks)-1], " \t\r\n")
	if len(last) < 2 || last[0] != '{' || last[len(last)-1] != '}' {
		return false // 最后一块不是对象, 挂不上字段
	}
	// 重复检查只读不改: 解出来看一眼键集合即可, 不用它的序列化结果。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(last, &probe); err != nil {
		return false
	}
	if _, dup := probe["cache_control"]; dup {
		return false
	}
	withBP := make([]byte, 0, len(last)+40)
	withBP = append(withBP, last[:len(last)-1]...) // 去掉收尾的 '}'
	withBP = append(withBP, `,"cache_control":{"type":"ephemeral"}}`...)
	blocks[len(blocks)-1] = withBP

	raw, err := json.Marshal(blocks) // []json.RawMessage: 各元素逐字节原样输出
	if err != nil {
		return false
	}
	target.Content = raw
	return true
}

// cacheBreakpointIndexes 选出消息序列里该打断点的位置 (从后往前, 最多 max 个)。
//
// 为什么不是一个断点: prompt cache 是**前缀**匹配 —— 一次请求里断点之前的全部内容
// 构成一个可复用单元。只有一个断点时, 它覆盖"到目前为止的全部前缀", 听起来最优,
// 但代价是全有全无: 只要最新那条消息与上一轮不同 (几乎必然), 上一轮那份缓存就
// 完全用不上, 每次都要把整个前缀重新写一遍。多个断点让"上一轮已经写过的部分"
// 有独立的缓存条目可以命中, 新增内容另外成段。
//
// 位置选择:
//
//	BP3 = 最后一条消息末尾        —— 覆盖到最新
//	BP2 = 上一轮最后一条 (最近一条 assistant) —— 上一轮的完整边界
//
// 为什么 BP2 取"最近的 assistant"而不是简单的 len-2: 一轮里 assistant 之后可能跟着
// 多条 tool_result user 消息 (一次 assistant 发起多个工具调用)。取 len-2 会落在这
// 组 tool_result 中间 —— 那是一轮**内部**的边界, 下一轮还会继续增长, 拿它当缓存
// 边界既不稳定也没额外收益。assistant 消息才是一轮真正的收口处。
//
// 返回的下标不重复 (BP2 可能与 BP3 重合, 如消息不足两轮时)。
func cacheBreakpointIndexes(messages []types.APIMessage, max int) []int {
	if len(messages) == 0 || max <= 0 {
		return nil
	}
	out := make([]int, 0, max)
	seen := make(map[int]bool, max)
	add := func(i int) {
		if i < 0 || i >= len(messages) || seen[i] || len(out) >= max {
			return
		}
		seen[i] = true
		out = append(out, i)
	}

	// BP3: 最新
	add(len(messages) - 1)

	// BP2: 上一条 assistant (上一轮的收口)
	for i := len(messages) - 2; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			add(i)
			break
		}
	}

	return out
}

// markMessageCacheBreakpoint 在消息序列上打 BP2/BP3 两个断点。
//
// 保留这个包装是为了既有调用点与测试; 真正的选点逻辑在 cacheBreakpointIndexes。
func markMessageCacheBreakpoint(messages []types.APIMessage) {
	for _, idx := range cacheBreakpointIndexes(messages, 2) {
		markCacheBreakpointAt(messages, idx)
	}
}

// markSystemCacheBreakpoint 在 system 上打两个断点: 块 0 与稳定段末尾。
//
// 断点缓存的是它**之前**(含自身)的全部前缀, 所以打在越靠后的稳定块上覆盖得越多。
// engine 按「base(稳定) → memory(检索结果) → task(本轮任务)」分层, 任务指令
// **永远**是最后一块 (每轮都变), 打在它上面等于每轮换一次缓存键, 绝不选它。
//
//	BP1 = 块 0        —— base, 跨**运行**稳定
//	BP2 = 倒数第二块  —— 通常是 memory 块, 稳定段的最末尾
//
// 为什么是**两个** (改前只打 block 0 一个):
//
// 改前断点打在 block 0 = base, 于是缓存条目止步于 base —— 它**后面**的 memory
// 块永远进不了缓存, 哪怕它在同一个 run 内逐轮完全没变。而 memory 恰恰是这块
// 提示里最大的一截 (面试团队实测 21K 字符, 占 system 的 2/5)。只覆盖 base 等于
// 把这 21K 每轮都按未命中计费。
//
// 补上第二个断点 (稳定段末尾) 后, base 与 memory 各有一段缓存。两者的**失效
// 节奏不同**才是这么切的理由: base 跨运行不变, memory 每跑一次就变 (记忆库被
// 运行自身改写, BM25 的 top-K 排序随之漂移)。打在两个位置, base 那截不会因为
// memory 变动而陪葬; 而 run 内 memory 不变, 第二个断点又能在逐轮对话里吃满。
//
// 两个是上限: 消息侧还要用掉 2 个 (BP3/BP4, 见 cacheBreakpointIndexes),
// 正好等于 MaxCacheBreakpoints=4; 再多会被上游 400, 不是静默丢弃。
//
// ⚠️ 实测边界 (2026-09-18, 直连 deepseek 端点): 不是所有上游都按 cache_control
// 行事。deepseek **一个断点都不打**也照样命中前缀缓存 —— 同样的 system 第二次
// 调用 input 从 3332 掉到 134、cache_read=3198。对这类端点, 断点布局是无害但也
// 无益的冗余; 只有真正按断点切分的上游 (Anthropic 官方等) 才吃这个改动。
// 别把"改了断点"直接当成"缓存一定变好" —— 先确认上游属于哪一类。
//
// 返回本次是否**新打上**了断点 (全都有则返回 false)。
func markSystemCacheBreakpoint(blocks []map[string]interface{}) bool {
	if len(blocks) == 0 {
		return false
	}
	// 稳定段末尾: 最后一块是变量 (任务指令) 时退一格; 只有一个块时就是它自己。
	stableIdx := len(blocks) - 1
	if stableIdx > 0 {
		stableIdx--
	}
	marked := false
	// 顺序刻意从 0 开始: 两个下标在单块/双块时可能重合, 靠 dup 判断去重即可。
	for _, idx := range []int{0, stableIdx} {
		if _, dup := blocks[idx]["cache_control"]; dup {
			continue
		}
		blocks[idx]["cache_control"] = map[string]string{"type": "ephemeral"}
		marked = true
	}
	return marked
}

// buildSystemParam 组装出站的 system 字段, 并在启用缓存时打上 BP1。
//
// cacheEnabled 为 false 时**完全不输出 cache_control** —— 上游不支持该字段的情况
// 由调用方的"400 后自适应关闭"兜底, 这里只负责不自找麻烦。
func buildSystemParam(systemPrompt []string, cacheEnabled bool) interface{} {
	switch len(systemPrompt) {
	case 0:
		return nil
	case 1:
		if !cacheEnabled {
			return systemPrompt[0]
		}
		return []map[string]interface{}{
			{"type": "text", "text": systemPrompt[0], "cache_control": map[string]string{"type": "ephemeral"}},
		}
	default:
		blocks := make([]map[string]interface{}, len(systemPrompt))
		for i, s := range systemPrompt {
			blocks[i] = map[string]interface{}{
				"type": "text",
				"text": s,
			}
		}
		if cacheEnabled {
			// G1 PromptCache: cache_control 贴在第一个稳定块 (block 0),
			// 而非最后一个动态块 (memory), 避免动态内容污染缓存键。
			markSystemCacheBreakpoint(blocks)
		}
		return blocks
	}
}

// buildSystemParamPlanned 是流式路径的 system 组装入口, 支持调用方的 CachePlan。
//
// 三条分支的优先级刻意写成"计划 > 内置 > 关闭":
//   - 计划给了 SystemIndexes, 就**只**按计划打点; 不额外补内置的 {0, 稳定段末尾},
//     否则调用方想"少打一个断点"永远做不到 (内置那两个会自己长回来)。
//   - 计划是 nil 或 SystemIndexes 为 nil, 原样交回 buildSystemParam —— 零值零影响
//     就落在这一行上, 不需要调用方做任何开关。
//   - cacheEnabled 为 false 时计划也不生效: 计划决定的是断点**位置**, 不是是否
//     启用缓存; 上游不认识 cache_control 时一个都不该发。
//
// 注意 SystemIndexes == []int{} (非 nil 空切片) 与 nil 是两种意思: 前者表示
// "system 侧一个断点都不要", 后者表示"用内置启发式"。这里绝不能把空切片当 nil,
// 否则调用方没有任何办法关掉 system 断点。
func buildSystemParamPlanned(systemPrompt []string, cacheEnabled bool, plan *CachePlan) interface{} {
	if !cacheEnabled || plan == nil || plan.SystemIndexes == nil || len(systemPrompt) == 0 {
		return buildSystemParam(systemPrompt, cacheEnabled)
	}
	blocks := make([]map[string]interface{}, len(systemPrompt))
	for i, s := range systemPrompt {
		blocks[i] = map[string]interface{}{
			"type": "text",
			"text": s,
		}
	}
	applySystemCachePlan(blocks, plan.SystemIndexes)
	return blocks
}

// buildSystemParamNonStream 是 sendMessageDirect 的 system 组装 (原内联代码原样搬出)。
//
// 显式抽成函数只为让计划分支能复用同一套块结构; cacheEnabled 的默认形态**逐字节**
// 保持原样 —— 非流式路径多块时内置只打 block 0 一个断点, 而流式路径打两个
// (见 markSystemCacheBreakpoint)。两条路径的这个差异是既有的, 不在本次改动里统一:
// 统一会改变非流式请求的字节, 而本次改动的底线是零值零影响。
func buildSystemParamNonStream(systemPrompt []string, cacheEnabled bool) interface{} {
	if len(systemPrompt) == 1 {
		if cacheEnabled {
			return []map[string]interface{}{
				{"type": "text", "text": systemPrompt[0], "cache_control": map[string]string{"type": "ephemeral"}},
			}
		}
		return systemPrompt[0]
	}
	if len(systemPrompt) > 1 {
		blocks := make([]map[string]interface{}, len(systemPrompt))
		for i, s := range systemPrompt {
			blocks[i] = map[string]interface{}{
				"type": "text",
				"text": s,
			}
		}
		if cacheEnabled {
			// G1 PromptCache: cache_control 贴在第一个稳定块 (block 0),
			// 而非最后一个动态块 (memory), 避免动态内容污染缓存键。
			blocks[0]["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		return blocks
	}
	return nil
}

// buildSystemParamNonStreamPlanned 是非流式路径的 system 组装入口, 支持 CachePlan。
// 判定顺序与 buildSystemParamPlanned 相同 (计划 > 内置 > 关闭)。
func buildSystemParamNonStreamPlanned(systemPrompt []string, cacheEnabled bool, plan *CachePlan) interface{} {
	if !cacheEnabled || plan == nil || plan.SystemIndexes == nil || len(systemPrompt) == 0 {
		return buildSystemParamNonStream(systemPrompt, cacheEnabled)
	}
	blocks := make([]map[string]interface{}, len(systemPrompt))
	for i, s := range systemPrompt {
		blocks[i] = map[string]interface{}{
			"type": "text",
			"text": s,
		}
	}
	applySystemCachePlan(blocks, plan.SystemIndexes)
	return blocks
}

// applySystemCachePlan 把断点贴到 blocks 的给定下标上。
//
// 这里**不做**边界/重复校验, 直接按下标取: 所有下标在 validateCachePlan 里已经
// 验过, 漏验会以 panic 的形式当场炸掉, 而不是悄悄少打一个断点。断点少打是无声缺陷
// (请求成功、缓存收益归零), 用 panic 把它变成有声缺陷是划算的。
//
// 已有 cache_control 的块跳过 (与 markSystemCacheBreakpoint 同口径): 上游对同一块
// 上的重复标记会报错, 而调用方传入的块可能已经带着上游给的标记。
func applySystemCachePlan(blocks []map[string]interface{}, indexes []int) {
	for _, idx := range indexes {
		if _, dup := blocks[idx]["cache_control"]; dup {
			continue
		}
		blocks[idx]["cache_control"] = map[string]string{"type": "ephemeral"}
	}
}

// applyMessageCachePlan 按计划在 messages 上打点; MessageIndexes 为 nil 时走内置启发式。
//
// 与 system 侧同理: 空切片 = "message 侧一个断点都不打", nil = "用内置"。
// 下标合法性由调用方先经 validateCachePlan 保证。
func applyMessageCachePlan(messages []types.APIMessage, plan *CachePlan) {
	if plan == nil || plan.MessageIndexes == nil {
		markMessageCacheBreakpoint(messages)
		return
	}
	for _, idx := range plan.MessageIndexes {
		markCacheBreakpointAt(messages, idx)
	}
}

// builtinSystemBreakpointCount 是流式路径内置启发式在给定块数下会打出的断点数。
//
// 用于把"计划侧 + 回退侧"的断点总数算准: 只数计划里的下标会漏掉另一侧由内置
// 补上的断点, 于是超限要到上游 400 才暴露 —— 那正是本改动要避免的。
//
// 只适用于流式路径 (markSystemCacheBreakpoint)。非流式路径内置只打 block 0 一个,
// 见 nonStreamBuiltinSystemBreakpoints —— 两条路径的内置形态本来就不同
// (见 buildSystemParamNonStream 的注释), 总数计算必须跟着分岔。用同一条公式会
// 把合法的非流式请求判死。
func builtinSystemBreakpointCount(sysLen int) int {
	switch {
	case sysLen <= 0:
		return 0
	case sysLen == 1:
		return 1
	default:
		return 2 // markSystemCacheBreakpoint: {0, 稳定段末尾}
	}
}

// nonStreamBuiltinSystemBreakpoints 是非流式路径内置启发式的断点数: 恒为 1。
//
// buildSystemParamNonStream 在单块时打 \`{"text":...,"cache_control":...}\`, 多块时只给
// block 0 贴标记, 两种形态都是 1 个 (sysLen == 0 时是 0)。
func nonStreamBuiltinSystemBreakpoints(sysLen int) int {
	if sysLen <= 0 {
		return 0
	}
	return 1
}

// builtinMessageBreakpointCount 是流式路径内置启发式在给定消息条数下断点数的**上界**。
//
// 这里只有条数、看不到 Role, 无法算出 cacheBreakpointIndexes 的精确结果
// (它要找"最近的 assistant", 没有 assistant 时只出 1 个)。取 min(msgLen,2)
// 作为上界: 宁可把一个实际只有 1 个断点的回退侧算成 2 而提前报错, 也不让真正
// 超限的请求漏到上游 —— 后者是整请求 400, 前者只是一条可读的本地错误。
func builtinMessageBreakpointCount(msgLen int) int {
	if msgLen <= 0 {
		return 0
	}
	if msgLen < 2 {
		return msgLen
	}
	return 2
}

// validateCachePlan 在构造请求体**之前**校验调用方给的断点计划。
//
// 语义上等价于流式路径的校验 (见 validateCachePlanPath); 保留这个两参数形态是
// 给不关心路径差异的调用方/测试用的。总数按流式路径算 —— 那是**更大**的一侧
// (两侧内置都可能打满), 因此在非流式路径上调用它最多只是多报一次错, 不会放过
// 真正超限的请求。
func (c *Client) validateCachePlan(msgLen, sysLen int) error {
	return c.validateCachePlanPath(msgLen, sysLen, true)
}

// validateCachePlanPath 是带路径参数的校验实现。stream 决定另一侧 (计划为 nil 的
// 那一侧) 按哪条内置启发式补断点数 —— 流式与非流式打点形态不同, 见
// builtinSystemBreakpointCount / nonStreamBuiltinSystemBreakpoints。
//
// 为什么必须前置失败、且必须是错误而不是跳过:
//
//  1. 超上限: Anthropic 最多接受 4 个 cache_control, 超出的表现是**整个请求 400**,
//     不是静默丢弃。等到上游拒绝时, 调用方只看到一个语义不明的 4xx, 还白付一次
//     往返; 而"少打几个就好了"这种自行降级又会让人以为计划生效了 —— 两者的共同
//     问题是调用方无法从结果里看出计划有没有被采纳。
//  2. 下标非法 (越界/负数/重复): 打点函数对非法下标是静默返回的 (见
//     markCacheBreakpointAt / markSystemCacheBreakpoint 的 skip 分支)。于是一份
//     写错下标的计划会"成功"发出, 只是缓存收益为零 —— 没有任何报错、没有任何
//     指标, 只有月底账单。这类缺陷必须在这里变成显式错误。
//  3. 重复下标: 上游对同一块上的重复 cache_control 报错; 而且重复本身就说明调用方
//     的布局计算有 bug (两个位置算成了同一个), 让它过去等于掩盖 bug。
//
// 校验与"本次是否启用缓存"无关: 是否发 cache_control 由 shouldEnablePromptCache
// 决定, 但一份**形状非法**的计划无论启不启用都是调用方的错误。今天碰巧走的是
// 不支持缓存的端点所以没炸, 明天切到 Anthropic 端点就会炸 —— 那是更难排查的情形。
//
// 统计总数时另一侧若为 nil 要按该路径内置启发式的断点数补上 (见 builtin*):
// 计划只给了 system 3 个、messages 用内置 2 个, 总数是 5 而不是 3。
func (c *Client) validateCachePlanPath(msgLen, sysLen int, stream bool) error {
	if c == nil || c.CachePlan == nil {
		return nil
	}
	plan := c.CachePlan

	if err := validateCachePlanIndexes("system", plan.SystemIndexes, sysLen); err != nil {
		return err
	}
	if err := validateCachePlanIndexes("messages", plan.MessageIndexes, msgLen); err != nil {
		return err
	}

	sysCount := builtinSystemBreakpointCount(sysLen)
	msgCount := builtinMessageBreakpointCount(msgLen)
	if !stream {
		// 非流式: 内置 system 恒 1 个、消息侧一个都没有 (既有行为, 见
		// buildSystemParamNonStream 与 sendMessageDirect 的注释)。
		sysCount = nonStreamBuiltinSystemBreakpoints(sysLen)
		msgCount = 0
	}
	if plan.SystemIndexes != nil {
		sysCount = len(plan.SystemIndexes)
	}
	if plan.MessageIndexes != nil {
		msgCount = len(plan.MessageIndexes)
	}
	if total := sysCount + msgCount; total > MaxCacheBreakpoints {
		return fmt.Errorf(
			"api: 缓存断点计划共 %d 个 (system %d + messages %d)，超过 provider 上限 %d：超限会让上游整请求失败，不会静默丢弃多余的断点",
			total, sysCount, msgCount, MaxCacheBreakpoints)
	}
	return nil
}

// validateCachePlanIndexes 校验一侧的下标集合: 非负、不越界、不重复。
//
// 三种错误分别报出来 (而不是合成一句"下标非法"), 因为调用方最需要知道的是
// **哪一侧、哪几个下标**出了问题 —— 布局是算出来的, 重算时要定位到具体位置。
func validateCachePlanIndexes(side string, indexes []int, length int) error {
	seen := make(map[int]struct{}, len(indexes))
	for i, idx := range indexes {
		if idx < 0 {
			return fmt.Errorf("api: 缓存断点计划的 %s[%d] = %d 为负数：负下标不会指向任何内容，打点会被静默跳过，调用方却以为断点已生效", side, i, idx)
		}
		if idx >= length {
			return fmt.Errorf("api: 缓存断点计划的 %s[%d] = %d 越界 (%s 共 %d 个)：越界下标会被静默跳过，调用方却以为断点已生效", side, i, idx, side, length)
		}
		if _, dup := seen[idx]; dup {
			return fmt.Errorf("api: 缓存断点计划的 %s 下标 %d 重复：重复标记会被上游拒绝，且重复本身说明布局计算有 bug", side, idx)
		}
		seen[idx] = struct{}{}
	}
	return nil
}

// shouldEnablePromptCache 决定本次请求是否启用 prompt cache: 曾因错误被禁用则恒 false;
// "on"/"off" 显式生效; auto/空 只在 Anthropic 原生端点启用。
func (c *Client) shouldEnablePromptCache() bool {
	if c.promptCacheDisabledByError {
		return false
	}
	switch strings.ToLower(c.PromptCacheMode) {
	case "on":
		return true
	case "off":
		return false
	default: // "auto" 或空
		return isAnthropicEndpoint(c.BaseURL)
	}
}

// isAnthropicEndpoint 检测是否为 Anthropic 官方 API 或兼容协议 (支持 prompt caching)。
func isAnthropicEndpoint(baseURL string) bool {
	lower := strings.ToLower(baseURL)
	return strings.Contains(lower, "anthropic.com") ||
		strings.Contains(lower, "api.claude") ||
		strings.Contains(lower, "bedrock") || // AWS Bedrock
		strings.Contains(lower, "dashscope.aliyuncs.com") || // 阿里云百炼
		strings.Contains(lower, "moonshot") || // Moonshot Anthropic 兼容端点 (api.moonshot.ai|cn/anthropic)
		strings.Contains(lower, "kimi.com") || // Kimi coding 端点 (api.kimi.com/coding, Claude Code 兼容, 支持 cache_control)
		strings.Contains(lower, "/apps/anthropic") // Anthropic 兼容代理路径
}

// isOpenAI 判断当前客户端是否走 OpenAI chat/completions 协议。
func (c *Client) isOpenAI() bool {
	return strings.EqualFold(c.Protocol, ProtocolOpenAI)
}

// isOpenAIResponses 判断当前客户端是否走 OpenAI Responses API 协议 (/v1/responses)。
func (c *Client) isOpenAIResponses() bool {
	return strings.EqualFold(c.Protocol, ProtocolOpenAIResponses)
}

// endpointPath 返回协议对应的消息端点路径
// (anthropic: /messages, openai: /chat/completions, openai-responses: /responses)。
func (c *Client) endpointPath() string {
	if c.isOpenAI() {
		return "/chat/completions"
	}
	if c.isOpenAIResponses() {
		return "/responses"
	}
	return "/messages"
}

// isCacheRelatedError 检测 API 错误是否与 prompt caching 相关。
func isCacheRelatedError(statusCode int, body string) bool {
	if statusCode != 400 && statusCode != 422 {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "cache_control") ||
		strings.Contains(lower, "cache") && strings.Contains(lower, "breakpoint") ||
		strings.Contains(lower, "cache") && strings.Contains(lower, "not supported") ||
		strings.Contains(lower, "unknown") && strings.Contains(lower, "cache")
}

// applyExtraHeaders 把调用方注入的 ExtraHeaders 合并到出站请求头 (覆盖同名默认头,
// 因此可覆盖 User-Agent 等 Go http 默认值)。nil ExtraHeaders 时为空操作。
// 每个实际发往上游的 http.Request 构造点都必须调用它 (主路径 + fallback 模型路径)。
//
// 这里顺带补 opencode zen/go 的 x-opencode-session: 该上游缺这个头直接 400
// MissingSessionID。以前只有两个网关 (anthropic-gateway / llmgw) 会补, **直连**
// 路径 (claude-go run --model opencode:...) 完全没补 —— 因为网关在中间时根本
// 看不出这个缺口。判据用 BaseURL 主机而不是 provider 名 (Client 没有那个字段);
// 经网关时 BaseURL 指向网关、这里自然不触发, 由网关负责, 不会重复。
//
// 注入位置必须在 ExtraHeaders 之前: 调用方显式给的会话 ID 永远赢。ExtraHeaders
// 自身不参与 Client 克隆 (见其注释), 所以不能把默认会话头放在那一层 —— 引擎每轮
// 都会用 WithModel 造副本, 那样 header 会在真正的请求上消失。
func (c *Client) applyExtraHeaders(httpReq *http.Request) {
	c.applyOpencodeSessionHeader(httpReq)
	if len(c.ExtraHeaders) == 0 {
		return
	}
	for k, vs := range c.ExtraHeaders {
		for _, v := range vs {
			httpReq.Header.Set(k, v)
		}
	}
}

// applyOpencodeSessionHeader 给直连 opencode 上游的请求补会话头。
//
// seed 取 BaseURL + Model 而不是随机值: 会话 ID 在 opencode 侧同时是提示缓存
// 的分片键, 每次请求换一个等于把命中率钉死在 0 (见 OpencodeSessionID 注释)。
// 同一客户端实例的 BaseURL/Model 在一次进程内稳定, 因此同一模型的连续请求
// 落在同一会话上。ExtraHeaders 里显式给了值时不覆盖。
func (c *Client) applyOpencodeSessionHeader(httpReq *http.Request) {
	if c == nil || httpReq == nil {
		return
	}
	if httpReq.Header.Get(OpencodeSessionHeader) != "" {
		return
	}
	if !IsOpenCodeUpstream("", c.BaseURL) {
		return
	}
	httpReq.Header.Set(OpencodeSessionHeader,
		OpencodeSessionID(nil, "direct\n"+c.BaseURL+"\n"+c.Model))
}

// retryDelay 计算退避时间 (指数退避 + jitter, 429 用 3x 基数, deadline exceeded 用 DeadlineRetryBase)
func (c *Client) retryDelay(attempt int, statusCode int, err error) time.Duration {
	base := c.RetryBase
	if base <= 0 {
		base = 3 * time.Second
	}
	// 429/503 是"服务器请你慢点", 基线拉长 3 倍 —— 与普通 5xx 区分开。
	if statusCode == 429 || statusCode == 503 {
		base = base * 3
	}
	// 超时类错误有自己的基线: 网络慢不等于服务端限流, 用更长的基数避免连环超时。
	if err != nil && isDeadlineError(err) {
		base = c.DeadlineRetryBase
		if base <= 0 {
			base = 10 * time.Second
		}
	}
	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)))
	maxD := c.RetryMax
	if maxD <= 0 {
		maxD = 60 * time.Second
	}
	// 封顶: 长跑时 2^attempt 会迅速涨到分钟级, 不封顶等于把重试变成"放弃"。
	if delay > maxD {
		delay = maxD
	}
	// ±25% 抖动: 让多个客户端别在同一时刻齐步重试, 否则退避反而制造尖峰。
	jitter := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	return jitter
}

// isDeadlineError 用子串匹配判断错误是否为超时/截止时间类 (deadline / timeout)。
func isDeadlineError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadline") || strings.Contains(msg, "timeout")
}

// LLMEventRecord 一次 LLM 运行事件的完整描述 (全局 sink 用)。
//
// 比 LLMEventFunc 的 (eventType, detail) 多出 Source/Model 两栏: 全局 sink 收的是
// **所有** api.Client 的事件, 不带来源就没法判断"这条 429 是主模型、advisor 还是
// dashboard 发的" —— 而这恰恰是它相对旧的单实例回调唯一多出来的信息量。
type LLMEventRecord struct {
	// Type: retry / circuit_open / circuit_close / fatal / model_switch / model_restore
	Type   string
	Detail string
	Source string // Client.Tag (feishu / advisor / dashboard / ...); 空则 "unknown"
	Model  string // Client.Model
}

// LLMEventSink 全局 LLM 事件汇聚点。
type LLMEventSink func(rec LLMEventRecord)

// ── 全局 LLM 事件 sink (跨 Client 共用) ──────────────────────────────────────
//
// 为什么在实例字段 OnLLMEvent 之外还要一个全局的 (与 globalLLMHook 同款理由,
// 但这条是**修 bug** 不是加功能): OnLLMEvent 是 Client 的一个普通字段, 只有
// 装配方显式赋值的那个实例才会发事件, 且 WithModel/ConfiguredCloneFull 靠逐字段
// 手抄来传播它。实测生产里有 7 处 api.NewClient, 只有 pkg/feishu/bot.go:309 那个
// 被赋了值 —— advisor 客户端、/model 切换临时客户端、dashboard 诊断客户端的
// 429 重试 / 熔断 / 连续失败**从来没有任何人收到过**, 而且这种漏是静默的
// (少赋一个字段不会编译报错)。全局 sink 让"哪个 Client 实例"不再决定
// "事件能不能被看见"。
var (
	globalLLMEventMu   sync.RWMutex
	globalLLMEventSink LLMEventSink
)

// SetGlobalLLMEventSink 注册/覆盖全局 LLM 事件 sink。传入 nil 可禁用。
func SetGlobalLLMEventSink(s LLMEventSink) {
	globalLLMEventMu.Lock()
	globalLLMEventSink = s
	globalLLMEventMu.Unlock()
}

// fireEvent 同时通知本实例回调与全局 sink。
//
// ⚠️ 两者都在**重试/熔断循环内部同步调用**, 实现方必须立刻返回。历史上飞书播报
// 直接挂在 OnLLMEvent 上, 于是一次 429 重试要等 N 个在跑团队的飞书 HTTP 发完才
// 继续 —— 观测把背压传染给了执行。现在的接线把播报挪到了 EventBus 订阅方那侧
// (sink 只做一次非阻塞 Publish), 见 pkg/feishu/bot.go 的 llm.event.* 订阅。
func (c *Client) fireEvent(eventType, detail string) {
	if c.OnLLMEvent != nil {
		c.OnLLMEvent(eventType, detail)
	}
	globalLLMEventMu.RLock()
	sink := globalLLMEventSink
	globalLLMEventMu.RUnlock()
	if sink == nil {
		return
	}
	source := c.Tag
	if source == "" {
		source = "unknown"
	}
	sink(LLMEventRecord{Type: eventType, Detail: detail, Source: source, Model: c.Model})
}

// ── 全局 LLM 指标采集钩子 (跨 Client 共用) ────────────────────────────────────
// dashboard 启动时注册一次, 所有 api.Client (无论从哪里创建) 都会把调用统计
// 汇总到同一处, 便于统一展示 "LLM token 使用 / 质量 / 错误" 指标。
var (
	globalLLMHookMu sync.RWMutex
	globalLLMHook   LLMMetricsHook
)

// SetGlobalLLMMetricsHook 注册/覆盖全局 LLM 指标钩子。传入 nil 可禁用。
func SetGlobalLLMMetricsHook(h LLMMetricsHook) {
	globalLLMHookMu.Lock()
	globalLLMHook = h
	globalLLMHookMu.Unlock()
}

// emitLLMMetric 同时向 client 自己的 hook 和全局 hook 发送指标。
func (c *Client) emitLLMMetric(rec LLMCallRecord) {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	if rec.Model == "" {
		rec.Model = c.Model
	}
	if rec.BaseURL == "" {
		rec.BaseURL = c.BaseURL
	}
	if rec.Source == "" {
		rec.Source = c.Tag
	}
	// 保底: 如果调用侧完全没给出 Source, 给一个 "unknown" 而不是空串, 便于 dashboard 统计。
	if rec.Source == "" {
		rec.Source = "unknown"
	}
	if c.OnLLMMetrics != nil {
		func() {
			defer func() { _ = recover() }()
			c.OnLLMMetrics(rec)
		}()
	}
	globalLLMHookMu.RLock()
	gh := globalLLMHook
	globalLLMHookMu.RUnlock()
	if gh != nil {
		func() {
			defer func() { _ = recover() }()
			gh(rec)
		}()
	}
}

// classifyErrorKind 粗分类错误原因 (给指标上标签)。
func classifyErrorKind(status int, errStr string) string {
	low := strings.ToLower(errStr)
	switch {
	case status == 429:
		return "rate_limit"
	case status == 503 || status == 529 || strings.Contains(low, "overloaded"):
		return "overloaded"
	case strings.Contains(low, "prompt is too long") || strings.Contains(low, "prompt_too_long"):
		return "prompt_too_long"
	case strings.Contains(low, "refusal"):
		return "refusal"
	case strings.Contains(low, "context deadline") || strings.Contains(low, "timeout"):
		return "timeout"
	case status >= 400 && status < 500:
		return "client"
	case status >= 500:
		return "server"
	}
	return ""
}

// truncateErr 按 rune 截断错误文本 (错误信息里常含中文/上游多字节内容, 按字节截会劈字)。
func truncateErr(s string, n int) string {
	// 错误信息里常含中文/上游返回的多字节内容, 按 rune 截避免劈字。
	return textutil.Truncate(s, n, "…")
}

// isCircuitOpen 判断熔断器当前是否应拦截请求: 未开则放行; 已开但冷却窗口已过则就地
// 关闭熔断并清零失败计数 (允许试探), 返回 false; 仍在冷却窗口内返回 true。
func (c *Client) isCircuitOpen() bool {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	if !c.circuitOpen {
		return false
	}
	if time.Now().After(c.circuitOpenUntil) {
		c.circuitOpen = false
		c.consecutiveFails = 0
		c.fireEvent("circuit_close", "熔断器半开, 允许试探")
		return false
	}
	return true
}

// recordSuccess 记一次成功: 清零连续失败计数并关闭熔断器。
func (c *Client) recordSuccess() {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	c.consecutiveFails = 0
	c.circuitOpen = false
}

// recordFailure 记录一次失败到熔断器状态。
// 返回 (触发本次熔断, 当前连续失败数) — 调用方可用于给 LLMCallRecord 打标签。
func (c *Client) recordFailure(errMsg string) (opened bool, fails int) {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	c.consecutiveFails++
	fails = c.consecutiveFails
	threshold := c.cbThreshold
	if threshold <= 0 {
		threshold = 5
	}
	if c.consecutiveFails >= threshold && !c.circuitOpen {
		c.circuitOpen = true
		c.circuitOpenUntil = time.Now().Add(30 * time.Second)
		c.CircuitTrips.Add(1)
		opened = true
		c.fireEvent("circuit_open", fmt.Sprintf("连续 %d 次失败, 熔断 30s: %s", c.consecutiveFails, errMsg))
	}
	return opened, fails
}

// NewDashScopeClient 创建阿里百炼 DashScope API 客户端
// DashScope 提供 Anthropic 兼容端点 (coding.dashscope.aliyuncs.com)
func NewDashScopeClient(apiKey, model string) *Client {
	return NewClient("https://coding.dashscope.aliyuncs.com/apps/anthropic/v1", apiKey, model)
}

// NewAnthropicClient 创建标准 Anthropic API 客户端
func NewAnthropicClient(apiKey, model string) *Client {
	return NewClient("https://api.anthropic.com/v1", apiKey, model)
}

// NewMoonshotClient 创建 Moonshot/Kimi 客户端 (Anthropic 兼容端点)。
// 国际站默认 api.moonshot.ai；国内站请传 cn=true 使用 api.moonshot.cn。
// 模型名如 kimi-k2.6 / kimi-k2.5 / kimi-latest，鉴权走 Authorization: Bearer。
func NewMoonshotClient(apiKey, model string, cn bool) *Client {
	host := "https://api.moonshot.ai/anthropic"
	if cn {
		host = "https://api.moonshot.cn/anthropic"
	}
	return NewClient(host, apiKey, model)
}

// DefaultOllamaBaseURL 本机 Ollama 的 Anthropic 兼容端点。
// Ollama ≥0.30 在 /v1/messages 上提供完整的 Anthropic Messages API
// (含 tool_use / thinking / 图片输入)，可直接复用本客户端。
const DefaultOllamaBaseURL = "http://localhost:11434/v1"

// NewOllamaClient 创建本地 Ollama 客户端 (Anthropic 兼容端点)。
// baseURL 为空时使用 DefaultOllamaBaseURL；Ollama 无鉴权，使用占位 key。
// 本地模型解码慢 (CPU/iGPU 带宽瓶颈)，放宽首 token 与单次调用超时。
func NewOllamaClient(baseURL, model string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultOllamaBaseURL
	}
	c := NewClient(baseURL, "ollama", model)
	c.FirstTokenTimeout = 300 * time.Second
	c.CallTimeout = 1800 * time.Second
	return c
}

// WithToolChoiceAny 返回一个强制 tool_choice=any 的轻量副本。
// 用结构体值拷贝而非逐字段罗列: Client 字段一直在长 (WithModel 逐字段拷贝
// 已经漂过), 值拷贝天然免疫漂移。
func (c *Client) WithToolChoiceAny() *Client {
	cc := *c
	cc.ForceToolChoiceAny = true
	return &cc
}

// IsLocalEndpoint 判断 baseURL 是否指向本机端点 (localhost/127.0.0.1/0.0.0.0/::1)。
// 本机端点 (如 Ollama) 通常无需 API Key。
func IsLocalEndpoint(baseURL string) bool {
	u := strings.ToLower(strings.TrimSpace(baseURL))
	for _, p := range []string{"http://", "https://"} {
		u = strings.TrimPrefix(u, p)
	}
	host := u
	if i := strings.IndexAny(u, ":/"); i >= 0 {
		host = u[:i]
	}
	// IPv6 字面量形如 [::1]:11434
	if strings.HasPrefix(u, "[") {
		if j := strings.Index(u, "]"); j > 0 {
			host = u[1:j]
		}
	}
	switch host {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		return true
	}
	return false
}

// StreamMessage 以流式方式发送消息，返回事件通道。
// 对应 TS: services/api/claude.ts 中的 queryModelWithStreaming()
//
// SSE 流式协议:
//
//	每行格式: "data: {json}\n\n" 或 "event: {type}\n"
//	事件类型: message_start, content_block_start, content_block_delta,
//	          content_block_stop, message_delta, message_stop
//
// 算法:
//  1. 构建请求体 (model, messages, system, tools, stream=true)
//  2. 发送 POST 请求
//  3. 逐行读取 SSE 流
//  4. 解析事件并通过 channel 发送
func (c *Client) StreamMessage(
	ctx context.Context,
	messages []types.APIMessage,
	systemPrompt []string,
	tools []types.APITool,
	maxTokens int,
) (<-chan types.StreamDelta, <-chan error) {
	eventCh := make(chan types.StreamDelta, 100)
	errCh := make(chan error, 1)

	go func() {
		startTS := time.Now()
		callMeta := llmMetricsFromContext(ctx)
		var debug *promptDebugCapture
		streamRec := LLMCallRecord{
			Stream:  true,
			Status:  "success",
			Model:   c.Model,
			BaseURL: c.BaseURL,
			Source:  c.Tag,
			Request: "stream_messages",
		}
		applyLLMMetricsContext(&streamRec, callMeta)
		stampTraceIDs(&streamRec, trace.From(ctx))
		streamErrMsg := ""
		streamRetries := 0
		defer func() {
			streamRec.DurationSec = time.Since(startTS).Seconds()
			// token 双边记账修复 (design/02 §1.4): Kimi 类兼容网关的 SSE 常不在
			// message_start 回 input_tokens → InputTokens 恒 0, 下游 >0 守卫跳过 →
			// 只记了输出。这里 input 缺失时按 prompt 组件字符数估算 (chars/4 近似)
			// 并打 InputEstimated 标记, 使总量不再系统性偏小。
			if streamRec.InputTokens == 0 {
				if est := estimateInputTokens(streamRec.PromptComponents); est > 0 {
					streamRec.InputTokens = est
					streamRec.InputEstimated = true
				}
			}
			streamRec.TotalTokens = streamRec.InputTokens + streamRec.OutputTokens + streamRec.CacheReadTokens + streamRec.CacheCreationTokens
			streamRec.Retries = streamRetries
			if streamErrMsg != "" {
				streamRec.Status = "error"
				streamRec.ErrorMessage = truncateErr(streamErrMsg, 256)
				if streamRec.ErrorKind == "" {
					streamRec.ErrorKind = classifyErrorKind(streamRec.HTTPStatus, streamErrMsg)
				}
			} else if streamRetries > 0 {
				streamRec.Status = "retry_success"
			}
			applyLLMMetricsContext(&streamRec, callMeta)
			streamRec.Owner = types.OwnerFromContext(ctx)
			streamRec.App = types.AppFromContext(ctx)
			c.emitLLMMetric(streamRec)
			if debug != nil {
				debug.finish(c, streamRec, streamErrMsg)
			}
		}()
		defer close(eventCh)
		defer close(errCh)

		if c.isCircuitOpen() {
			c.fireEvent("circuit_open", "熔断器开启, StreamMessage 被拒绝")
			streamErrMsg = "circuit open"
			streamRec.ErrorKind = "client"
			streamRec.CircuitBlocked = true
			errCh <- fmt.Errorf("LLM 熔断器开启: 连续多次失败, 30s 后重试")
			return
		}

		// 缓存断点计划的前置校验 (R-H2)。
		//
		// 必须排在**任何**断点落笔与请求体构造之前, 四个理由:
		//  1. 超限/非法下标一旦拖到上游, 就是 400 或者"请求成功但缓存零收益";
		//  2. markMessageCacheBreakpoint / markCacheBreakpointAt 会**就地改写**
		//     调用方传进来的 messages (改 Content 字节)。先打点再发现计划非法,
		//     调用方拿回的历史已经被改过一遍, 重试时字节就与第一次不同了;
		//  3. 本函数在 goroutine 里跑, 早退只需往 errCh 送一条, 不发任何 HTTP;
		//  4. 排在 Guard.Acquire 之前: 一份写错的计划不该占用限流令牌, 更不该
		//     在队列里排完才被告知"下标越界"。
		if err := c.validateCachePlanPath(len(messages), len(systemPrompt), true); err != nil {
			streamErrMsg = err.Error()
			streamRec.ErrorKind = "client"
			errCh <- err
			return
		}

		// 全局准入控制
		var guardRelease func()
		if c.Guard != nil {
			waitStart := time.Now()
			guardRelease = c.Guard.Acquire()
			streamRec.GuardWaitSec = time.Since(waitStart).Seconds()
			defer guardRelease()
		}

		// 对话历史的缓存断点 (2026-09-12)。
		//
		// 此前 cache_control 只贴在 system 上, Messages 上一次都没有 —— 而 Anthropic
		// 的 prompt cache 是**前缀匹配**: 只标 system, 缓存就只覆盖 system 那一段,
		// 之后每轮增长的历史全按"新内容"计费。实测 claude-go 的 cache 命中只有
		// 48~53%, 而 Claude Code 在历史里打了多个断点, 能到 98%。两者差距主要在这。
		//
		// 这里给最后一条消息的最后一个块打断点: 它覆盖的正是"到目前为止的全部前缀",
		// 是单断点里收益最大的位置。多断点(最多 4 个)留给后续按命中率调优。
		// 只在 shouldEnablePromptCache() 为真时打 —— 不支持该字段的上游已有
		// "400 后自适应关闭" 的兜底 (见下方重试分支)。
		//
		// 调用方给了 CachePlan.MessageIndexes 时按计划打点 (见 applyMessageCachePlan);
		// nil 时与改动前一致, 走内置 cacheBreakpointIndexes(messages, 2)。
		if c.shouldEnablePromptCache() && len(messages) > 0 {
			applyMessageCachePlan(messages, c.CachePlan)
		}

		system := buildSystemParamPlanned(systemPrompt, c.shouldEnablePromptCache(), c.CachePlan)

		req := types.APIRequest{
			Model:     c.effectiveModel(),
			Messages:  messages,
			System:    system,
			MaxTokens: maxTokens,
			Stream:    true,
		}
		if len(tools) > 0 {
			req.Tools = tools
			if c.ForceToolChoiceAny {
				req.ToolChoice = &types.APIToolChoice{Type: "any"}
			}
		}

		// 协议分支: openai 协议走 /chat/completions, openai-responses 走 /responses,
		// 各自配流式翻译器 (把上游 SSE 事件翻译回 Anthropic 事件序列)。
		var err error
		var body []byte
		var oaiReq *openAIRequest
		var oaiTranslator *openAIStreamTranslator
		var respReq *responsesRequest
		var respTranslator *openAIResponsesStreamTranslator
		if c.isOpenAI() {
			oaiReq, body, err = c.buildOpenAIRequest(req, true)
			if err != nil {
				errCh <- fmt.Errorf("构建 OpenAI 请求失败: %w", err)
				return
			}
			oaiTranslator = newOpenAIStreamTranslator(req.Model)
		} else if c.isOpenAIResponses() {
			respReq, body, err = c.buildOpenAIResponsesRequest(req, true)
			if err != nil {
				errCh <- fmt.Errorf("构建 OpenAI Responses 请求失败: %w", err)
				return
			}
			respTranslator = newOpenAIResponsesStreamTranslator(req.Model)
		} else {
			body, err = json.Marshal(req)
			if err != nil {
				errCh <- fmt.Errorf("序列化请求失败: %w", err)
				return
			}
		}
		debug = c.newPromptDebugCapture("stream_messages", true)

		maxRetry := c.maxRetries()

		var resp *http.Response
		effectiveBaseURL := c.BaseURL
		effectiveAPIKey := c.APIKey
		// 首字耗时的起点必须贴着 Do()：startTS 早于准入等待(Guard.Acquire)，
		// 从它算会把"排队等令牌"的时间记成"模型首字慢"。每次尝试重新取值，
		// ttftSet 保证只认真正收到内容的第一帧（重试前的空流不覆盖它）。
		var reqSentAt time.Time
		ttftSet := false
	streamRetryLoop:
		for attempt := 0; attempt <= maxRetry; attempt++ {
			if ctx.Err() != nil {
				errCh <- ctx.Err()
				return
			}
			if debug != nil {
				debug.addAttempt(attempt+1, effectiveBaseURL+c.endpointPath(), req.Model, body)
			}

			httpReq, err := http.NewRequestWithContext(ctx, "POST", effectiveBaseURL+c.endpointPath(), bytes.NewReader(body))
			if err != nil {
				errCh <- fmt.Errorf("创建请求失败: %w", err)
				return
			}

			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("x-api-key", effectiveAPIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
			httpReq.Header.Set("Authorization", "Bearer "+effectiveAPIKey)
			setTraceHeaders(httpReq, ctx) // trace 四元组 (见 trace.go); 未设置时无副作用
			c.applyExtraHeaders(httpReq)  // 调用方注入的会话头等 (覆盖默认头)

			reqSentAt = time.Now()
			resp, err = c.Client.Do(httpReq)
			if err != nil {
				if attempt < maxRetry {
					delay := c.retryDelay(attempt, 0, err)
					c.TotalRetries.Add(1)
					streamRetries++
					c.fireEvent("retry", fmt.Sprintf("Stream 网络错误(尝试 %d/%d), %.0fs 后重试", attempt+1, maxRetry+1, delay.Seconds()))
					select {
					case <-time.After(delay):
						continue
					case <-ctx.Done():
						streamErrMsg = ctx.Err().Error()
						errCh <- ctx.Err()
						return
					}
				}
				opened, _ := c.recordFailure(err.Error())
				streamRec.CircuitOpened = opened
				streamErrMsg = err.Error()
				errCh <- fmt.Errorf("API 请求失败: %w", err)
				return
			}
			streamRec.HTTPStatus = resp.StatusCode

			if resp.StatusCode == 200 {
				c.recordSuccess()
				if c.Guard != nil {
					c.Guard.OnSuccess()
				}
				break
			}

			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if debug != nil {
				debug.setResponse(resp.StatusCode, respBody)
			}

			// 不可重试: prompt_too_long / refusal / 4xx (不计入熔断器)
			var apiErr types.APIError
			if json.Unmarshal(respBody, &apiErr) == nil {
				errType := apiErr.Error.Type
				if errType == "invalid_request_error" && strings.Contains(apiErr.Error.Message, "prompt is too long") {
					streamErrMsg = apiErr.Error.Message
					streamRec.ErrorKind = "prompt_too_long"
					errCh <- &PromptTooLongError{Message: apiErr.Error.Message}
					return
				}
				if errType == "refusal" {
					streamErrMsg = apiErr.Error.Message
					streamRec.ErrorKind = "refusal"
					errCh <- fmt.Errorf("模型拒绝回答 (refusal): %s", apiErr.Error.Message)
					return
				}
			}

			// 自适应关闭 prompt cache (流式路径)。
			//
			// 这是主路径 —— 引擎所有实际调用都走 StreamMessage, 非流式那条分支
			// 只在少数诊断场景触发。多断点策略会把 cache_control 贴到更多位置,
			// 碰到"不认识该字段"的上游概率也随之上升, 所以这里必须有一道兜底:
			// 一旦上游以 400/422 明确抱怨 cache, 就**本次请求立刻去掉全部断点重试**,
			// 并把该客户端后续请求的缓存整体关掉。
			//
			// 关键性质: 关掉缓存只是退化成"全部按新 token 计费", 请求照常成功 ——
			// 缓冲收益是**偏好**, 不是可用性前提。
			if c.shouldEnablePromptCache() && isCacheRelatedError(resp.StatusCode, string(respBody)) {
				logging.For("api").Warn("上游不支持 cache_control, 本次请求已去断点重试并关闭后续缓存",
					"status", resp.StatusCode, "body", textutil.TruncateBytes(string(respBody), 200, "…"))
				c.promptCacheDisabledByError = true
				// 注意: system 是 interface{} (可能是 string, 也可能是 []map), 不能
				// 就地改——重建一次请求体最稳妥。
				req.System = buildSystemParam(systemPrompt, false)
				if c.isOpenAI() {
					oaiReq, body, err = c.buildOpenAIRequest(req, true)
				} else if c.isOpenAIResponses() {
					respReq, body, err = c.buildOpenAIResponsesRequest(req, true)
				} else {
					body, err = json.Marshal(req)
				}
				if err != nil {
					errCh <- fmt.Errorf("重建去缓存请求失败: %w", err)
					return
				}
				streamRec.ErrorKind = "cache_unsupported"
				continue streamRetryLoop
			}

			if !c.retryableStatus(resp.StatusCode) {
				streamErrMsg = fmt.Sprintf("API %d: %s", resp.StatusCode, string(respBody))
				errCh <- fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
				return
			}

			// 429 分类 + Guard 通知
			retryAfterSec := ParseRetryAfter(resp, respBody)
			if resp.StatusCode == 429 {
				kind := Classify429(resp.StatusCode, string(respBody))
				if !kind.ShouldRetry() {
					errCh <- fmt.Errorf("API 429 (%s): %s", kind, string(respBody))
					return
				}
				if c.Guard != nil {
					c.Guard.On429(retryAfterSec)
				}
				// 智能模型切换: 连续 429 达阈值时切换备用模型
				if newModel := c.on429OrFallback(); newModel != "" {
					// 先观测再切换: From 必须是切换前的模型。
					c.notifyFallback(req.Model, newModel, resp.StatusCode)
					req.Model = newModel
					if respReq != nil {
						respReq.Model = newModel
						body, _ = json.Marshal(respReq)
					} else if oaiReq != nil {
						oaiReq.Model = newModel
						body, _ = json.Marshal(oaiReq)
					} else {
						body, _ = json.Marshal(req)
					}
					if c.FallbackBaseURL != "" {
						effectiveBaseURL = c.FallbackBaseURL
					}
					if c.FallbackAPIKey != "" {
						effectiveAPIKey = c.FallbackAPIKey
					}
				}
			} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
				if c.Guard != nil {
					c.Guard.On429(retryAfterSec)
				}
			}

			// 可重试: 429 / 503 / 529 / 5xx
			if attempt < maxRetry {
				delay := c.retryDelay(attempt, resp.StatusCode, nil)
				if retryAfterSec > 0 {
					raDelay := time.Duration(retryAfterSec*1000)*time.Millisecond + time.Duration(rand.Float64()*2000)*time.Millisecond
					if raDelay > delay {
						delay = raDelay
					}
				}
				c.TotalRetries.Add(1)
				streamRetries++
				statusHint := "服务端错误"
				if resp.StatusCode == 429 {
					statusHint = fmt.Sprintf("限流(%s)", Classify429(resp.StatusCode, string(respBody)))
				} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
					statusHint = "过载"
				}
				c.fireEvent("retry", fmt.Sprintf("Stream %s(尝试 %d/%d), %.0fs 后重试", statusHint, attempt+1, maxRetry+1, delay.Seconds()))
				select {
				case <-time.After(delay):
					continue
				case <-ctx.Done():
					streamErrMsg = ctx.Err().Error()
					errCh <- ctx.Err()
					return
				}
			}

			// Stream 全部重试失败 — 尝试 FallbackModels
			if len(c.FallbackModels) > 0 && isFallbackEligible(resp.StatusCode, fmt.Errorf("%s", string(respBody))) {
				for _, fbModel := range c.FallbackModels {
					if fbModel == "" || fbModel == req.Model {
						continue
					}
					log.Printf("[api] Stream 主模型失败, 尝试备用模型: %s", fbModel)
					c.fireEvent("retry", fmt.Sprintf("Stream 切换备用模型 %s", fbModel))
					// 观测点与真正切换相邻: 夹在日志/事件之后、赋值之前, 保证
					// From 仍是本次失败所用的模型。
					c.notifyFallback(req.Model, fbModel, resp.StatusCode)
					req.Model = fbModel
					if respReq != nil {
						respReq.Model = fbModel
						body, _ = json.Marshal(respReq)
					} else if oaiReq != nil {
						oaiReq.Model = fbModel
						body, _ = json.Marshal(oaiReq)
					} else {
						body, _ = json.Marshal(req)
					}
					// 若配置了独立的 fallback 端点, 切换局部 baseURL/apiKey (不污染 client 实例)
					if c.FallbackBaseURL != "" {
						effectiveBaseURL = c.FallbackBaseURL
					}
					if c.FallbackAPIKey != "" {
						effectiveAPIKey = c.FallbackAPIKey
					}
					// 用备用模型再走一轮完整重试
					goto streamRetryLoop
				}
			}

			opened, _ := c.recordFailure(string(respBody))
			streamRec.CircuitOpened = opened
			c.TotalFails.Add(1)
			c.fireEvent("fatal", fmt.Sprintf("Stream %d 次全部失败", maxRetry+1))
			streamErrMsg = fmt.Sprintf("API %d: %s", resp.StatusCode, string(respBody))
			errCh <- fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
			return
		}

		if resp == nil || resp.StatusCode != 200 {
			errCh <- fmt.Errorf("未获得有效响应")
			return
		}
		defer resp.Body.Close()

		// 解析 SSE 流
		// SSE 格式: "event: <type>\ndata: <json>\n\n" 或 "data:<json>\n"
		// DashScope 格式使用 "data:" (无空格) 前缀
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		// 收尾判据的两个记分牌 (2026-10-28)。
		//
		// 此前这里没有任何"流是否完整"的判断: 非法 JSON 帧被 continue 丢掉,
		// 断流只是 scanner.Scan() 返回 false, 两种情形都以 err=nil 收场。
		// 上游于是可以"一个字都没说"而引擎报 completed —— 业务层据此把空回复
		// 存成正常助手消息。用假上游实测: 0 个事件 / err=nil / completed。
		//
		// 判据: 收到过 message_delta(stop_reason) 或 message_stop 才算完成。
		//
		// 两者都认，因为协议里它们是**两种不同的正常收尾**：标准 Anthropic 流
		// 两个都发；而部分网关/翻译层只发 message_stop。只认 message_delta 会把
		// 后者的正常流判成断流（pkg/llmgw 的既有夹具就是这种形状）。
		// 真正要抓的是"两个都没来"——那才是截断。
		sawStopReason := false
		sawMessageStop := false
		// malformedFrames 统计 data 行里 JSON 解析失败的帧数。
		//
		// 不立刻报错: 个别网关会混入非协议行 (心跳、注释), 单帧失败就中断会
		// 把可用的流打断。但**一帧都没成功**时必然报错 —— 那说明整个响应不是
		// SSE 协议, 而不是偶尔多了一行。
		malformedFrames := 0
		validFrames := 0

		for scanner.Scan() {
			line := scanner.Text()

			// 支持 "data: " 和 "data:" 两种格式
			var data string
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimPrefix(line, "data:")
				data = strings.TrimSpace(data)
			} else {
				continue
			}

			if data == "" || data == "[DONE]" {
				continue
			}

			// 协议分支: openai / openai-responses 流式 chunk 需翻译成 Anthropic
			// 事件序列 (可能一帧多事件)。
			var deltas []types.StreamDelta
			if oaiTranslator != nil {
				evs, oerr := oaiTranslator.translate([]byte(data))
				if oerr != nil {
					malformedFrames++
					continue
				}
				deltas = evs
			} else if respTranslator != nil {
				evs, oerr := respTranslator.translate([]byte(data))
				if oerr != nil {
					malformedFrames++
					continue
				}
				deltas = evs
			} else {
				var d types.StreamDelta
				if err := json.Unmarshal([]byte(data), &d); err != nil {
					malformedFrames++
					continue
				}
				deltas = []types.StreamDelta{d}
			}
			validFrames++
			for _, d := range deltas {
				if d.Delta != nil && d.Delta.StopReason != "" {
					sawStopReason = true
				}
				if d.Type == "message_stop" {
					sawMessageStop = true
				}
			}
			if debug != nil {
				debug.addStreamEvent(data)
			}

			// 首字耗时：一批增量里第一次出现可见产出（正文/思考/工具入参）时结算。
			// 起点是 reqSentAt(贴着 Do)，不是 startTS(含准入等待)，也不是首帧到达
			// —— message_start 到第一段内容之间常还隔着一次模型预热。
			if !ttftSet && !reqSentAt.IsZero() && deltasCarryContent(deltas) {
				streamRec.TTFTMs = float64(time.Since(reqSentAt).Milliseconds())
				ttftSet = true
			}

			for _, delta := range deltas {
				// 采集 token 使用量 (message_start / message_delta)
				if delta.Message != nil && delta.Message.Usage != nil {
					u := delta.Message.Usage
					if u.InputTokens > 0 {
						streamRec.InputTokens = u.InputTokens
					}
					if u.OutputTokens > 0 {
						streamRec.OutputTokens = u.OutputTokens
					}
					if u.CacheReadInputTokens > 0 {
						streamRec.CacheReadTokens = u.CacheReadInputTokens
					}
					if u.CacheCreationInputTokens > 0 {
						streamRec.CacheCreationTokens = u.CacheCreationInputTokens
					}
				}
				if delta.Usage != nil {
					if delta.Usage.OutputTokens > 0 {
						streamRec.OutputTokens = delta.Usage.OutputTokens
					}
					if delta.Usage.InputTokens > 0 {
						streamRec.InputTokens = delta.Usage.InputTokens
					}
				}
				if delta.Delta != nil && delta.Delta.StopReason != "" {
					streamRec.StopReason = delta.Delta.StopReason
				}

				// 处理 signature_delta (签名验证增量)
				// 对应 TS: content_block_delta 中 delta.type=="signature_delta"
				if delta.Delta != nil && delta.Delta.Signature != "" {
					// 签名数据附加到当前块，不做额外处理
				}

				// 处理 server_tool_use / server_tool_result 块
				// 对应 TS: 服务端工具（如 web_search_tool）直接由 API 执行
				if delta.ContentBlock != nil {
					switch delta.ContentBlock.Type {
					case types.ContentBlockServerToolUse:
						// 服务端工具调用 - 传递给消费者
					case types.ContentBlockServerToolResult:
						// 服务端工具结果 - 传递给消费者
					}
				}

				// 处理 message_delta 中的 stop_reason
				// 对应 TS: stop_reason=="refusal" 时的特殊处理
				if delta.Delta != nil && delta.Delta.StopReason == string(types.StopReasonRefusal) {
					// 模型拒绝继续: 在 engine 层处理
				}

				select {
				case eventCh <- delta:
				case <-ctx.Done():
					return
				}
			}
		}

		// 流结束 (EOF): openai 翻译器可能因 usage 独立尾帧挂起了 message_delta
		// (finishPending)。若 usage 尾帧始终未到 (上游忽略 include_usage 的退化情形),
		// 在此补发, 保证引擎总能收到 stop_reason 收尾。
		if oaiTranslator != nil {
			for _, delta := range oaiTranslator.Flush() {
				if delta.Usage != nil {
					if delta.Usage.OutputTokens > 0 {
						streamRec.OutputTokens = delta.Usage.OutputTokens
					}
					if delta.Usage.InputTokens > 0 {
						streamRec.InputTokens = delta.Usage.InputTokens
					}
				}
				if delta.Delta != nil && delta.Delta.StopReason != "" {
					streamRec.StopReason = delta.Delta.StopReason
				}
				select {
				case eventCh <- delta:
				case <-ctx.Done():
					return
				}
			}
		}

		// ===== 流完整性判定 (R-H15 / Q-H7) =====
		//
		// 位置必须在翻译器 flush **之后**: flush 是"上游只是没发 usage 尾帧"的
		// 合法补救, 它补出的 message_delta 足以证明流是完整的。放在 flush 之前
		// 会把这类正常收尾误判成断流。
		//
		// 此前这里没有任何"流是否完整"的判断: 非法 JSON 帧被 continue 丢掉,
		// 断流只是 scanner.Scan() 返回 false, 两种情形都以 err=nil 收场。上游
		// 于是可以"一个字都没说"而引擎报 completed —— 业务层据此把空回复存成
		// 正常助手消息。用假上游实测: 0 个事件 / err=nil / completed。
		//
		// ctx 已取消时不算上游故障: 是调用方主动放弃, 引擎那边会报 cancelled。
		if ctx.Err() == nil {
			switch {
			case scanner.Err() != nil:
				// 读流本身出错 (连接被重置/超时)。断流最典型的形态。
				streamErrMsg = fmt.Sprintf("读取响应流失败: %v", scanner.Err())
				streamRec.ErrorKind = "server"
				errCh <- fmt.Errorf("claude-go: 读取响应流失败: %w", scanner.Err())
				return
			case malformedFrames > 0 && validFrames == 0:
				// 一帧都没解析成功 = 这不是 SSE 协议内容。常见于代理返回了
				// HTML 错误页、或上游把 JSON 截断了。
				streamErrMsg = fmt.Sprintf("响应流非法: %d 帧 data 无法解析", malformedFrames)
				streamRec.ErrorKind = "server"
				errCh <- fmt.Errorf("claude-go: 响应流非法: %d 帧 data 无法解析 (协议不匹配或响应被截断)", malformedFrames)
				return
			case !sawStopReason && !sawMessageStop:
				// 帧本身合法但流在收口前断了 —— 半截正文。这是最危险的一种:
				// 内容看起来正常, 只是少了后半句。
				streamErrMsg = "响应流在 message_delta 之前结束 (断流)"
				streamRec.ErrorKind = "server"
				errCh <- fmt.Errorf("claude-go: 响应流在 message_delta 之前结束 (断流), 已完成 %d 帧", validFrames)
				return
			}
		}
	}()

	return eventCh, errCh
}

// SendMessage 非流式发送消息 (内置全局准入 + 429/5xx 自动重试 + 熔断)。
//
// design/01 §4.10: 这里是 CallInterceptor 链的唯一挂载点 (见 call_interceptor.go)。
// 链为空时**原路直通** sendMessageDirect —— 不构造 LLMCall、不挂记录槽, 空链零开销。
func (c *Client) SendMessage(
	ctx context.Context,
	messages []types.APIMessage,
	systemPrompt []string,
	tools []types.APITool,
	maxTokens int,
) (*types.APIResponse, error) {
	ics := c.callChain()
	if len(ics) == 0 {
		return c.sendMessageDirect(ctx, messages, systemPrompt, tools, maxTokens)
	}
	if err := ValidateCallInterceptors(ics); err != nil {
		// 链装不起来 = 观测/治理设施坏了。fail-closed: 不偷偷绕过链去发请求。
		return nil, err
	}
	call := LLMCall{
		Model: c.effectiveModel(), Request: "messages",
		Messages: messages, SystemPrompt: systemPrompt, Tools: tools,
		MaxTokens: maxTokens, Trace: trace.From(ctx),
	}
	exec := func(ctx context.Context, cl LLMCall) (LLMResult, error) {
		sinkCtx, sink := withRecordSink(ctx)
		resp, err := c.sendMessageDirect(sinkCtx, cl.Messages, cl.SystemPrompt, cl.Tools, cl.MaxTokens)
		return LLMResult{Response: resp, Record: sink.snapshot()}, err
	}
	res, err := chainCall(ics, exec)(ctx, call)
	return res.Response, err
}

// sendMessageDirect 是 SendMessage 的原实现 (未经切面链)。
func (c *Client) sendMessageDirect(
	ctx context.Context,
	messages []types.APIMessage,
	systemPrompt []string,
	tools []types.APITool,
	maxTokens int,
) (*types.APIResponse, error) {
	callMeta := llmMetricsFromContext(ctx)
	traceIDs := trace.From(ctx)
	var debug *promptDebugCapture
	var debugRec LLMCallRecord
	var debugErr string
	emit := func(rec LLMCallRecord) {
		applyLLMMetricsContext(&rec, callMeta)
		stampTraceIDs(&rec, traceIDs)
		// owner 与 trace/engine 同源：都从 ctx 读（R-H6）。放在 emit 里而不是
		// 各返回点，理由与 sinkRecord 相同 —— emit 是记录成型点，另找位置必漏。
		rec.Owner = types.OwnerFromContext(ctx)
		rec.App = types.AppFromContext(ctx)
		debugRec = rec
		// 把这份记录交给切面链的记录槽 (链为空/未挂槽时是空操作)。放在 emit 里而不是
		// 各返回点: emit 已经是"这次调用的记录成型点"的单一出口, 另找位置必然漏一条。
		sinkRecord(ctx, rec)
		c.emitLLMMetric(rec)
	}
	defer func() {
		if debug != nil {
			debug.finish(c, debugRec, debugErr)
		}
	}()

	if c.isCircuitOpen() {
		c.fireEvent("circuit_open", "熔断器开启, SendMessage 被拒绝")
		rec := LLMCallRecord{
			Status:         "error",
			Request:        "messages",
			DurationSec:    0,
			HTTPStatus:     0,
			ErrorKind:      "client",
			ErrorMessage:   "circuit open",
			CircuitBlocked: true,
		}
		debugRec = rec
		emit(rec)
		return nil, fmt.Errorf("LLM 熔断器开启: 连续多次失败, 30s 后重试")
	}

	// 缓存断点计划的前置校验 (R-H2)。与流式路径同口径, 也必须排在 system 组装
	// 与请求体构造之前: 非法计划不该产生任何可发送的字节, 更不该花一次往返去
	// 让上游告诉你"断点太多"。同样排在 Guard.Acquire 之前, 不占限流令牌。
	if err := c.validateCachePlanPath(len(messages), len(systemPrompt), false); err != nil {
		rec := LLMCallRecord{
			Status:       "error",
			Request:      "messages",
			DurationSec:  0,
			HTTPStatus:   0,
			ErrorKind:    "client",
			ErrorMessage: truncateErr(err.Error(), 256),
		}
		debugErr = rec.ErrorMessage
		emit(rec)
		return nil, err
	}

	// 全局准入控制 (RPM 令牌桶 + 并发信号量)
	var guardRelease func()
	var guardWaitSec float64
	if c.Guard != nil {
		waitStart := time.Now()
		guardRelease = c.Guard.Acquire()
		guardWaitSec = time.Since(waitStart).Seconds()
		defer guardRelease()
	}

	// 非流式路径同样接受 CachePlan (见 buildSystemParamNonStreamPlanned)。
	// MessageIndexes 为 nil 时**不打消息断点** —— 这是既有行为, 不改: 本路径原本
	// 就没有消息侧断点 (只有 system), 补上会改变所有非流式请求的字节。
	// 只有调用方显式给了计划, 才在这里补上消息侧打点。
	if c.shouldEnablePromptCache() && len(messages) > 0 && c.CachePlan != nil && c.CachePlan.MessageIndexes != nil {
		applyMessageCachePlan(messages, c.CachePlan)
	}
	system := buildSystemParamNonStreamPlanned(systemPrompt, c.shouldEnablePromptCache(), c.CachePlan)

	req := types.APIRequest{
		Model:     c.effectiveModel(),
		Messages:  messages,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    false,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	// 协议分支: openai 走 /chat/completions, openai-responses 走 /responses。
	var err error
	var body []byte
	var oaiReq *openAIRequest
	var respReq *responsesRequest
	if c.isOpenAI() {
		oaiReq, body, err = c.buildOpenAIRequest(req, false)
		if err != nil {
			return nil, err
		}
	} else if c.isOpenAIResponses() {
		respReq, body, err = c.buildOpenAIResponsesRequest(req, false)
		if err != nil {
			return nil, err
		}
	} else {
		body, err = json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("序列化请求失败: %w", err)
		}
	}
	debug = c.newPromptDebugCapture("messages", false)

	maxRetry := c.maxRetries()

	var lastErr error
	var lastStatus int
	startTS := time.Now()
	retries := 0
	effectiveBaseURL := c.BaseURL
	effectiveAPIKey := c.APIKey
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if debug != nil {
			debug.addAttempt(attempt+1, effectiveBaseURL+c.endpointPath(), req.Model, body)
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", effectiveBaseURL+c.endpointPath(), bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", effectiveAPIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("Authorization", "Bearer "+effectiveAPIKey)
		setTraceHeaders(httpReq, ctx) // trace 四元组 (见 trace.go); 未设置时无副作用
		c.applyExtraHeaders(httpReq)  // 调用方注入的会话头等 (覆盖默认头)

		resp, err := c.Client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("API 请求失败: %w", err)
			if attempt < maxRetry {
				delay := c.retryDelay(attempt, 0, err)
				c.TotalRetries.Add(1)
				retries++
				log.Printf("[api] SendMessage 网络错误(尝试 %d/%d): %v, %.1fs 后重试", attempt+1, maxRetry+1, err, delay.Seconds())
				c.fireEvent("retry", fmt.Sprintf("网络错误(尝试 %d/%d): %v, %.0fs 后重试", attempt+1, maxRetry+1, err, delay.Seconds()))
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			opened, _ := c.recordFailure(lastErr.Error())
			c.TotalFails.Add(1)
			rec := LLMCallRecord{
				Status:        "error",
				Request:       "messages",
				DurationSec:   time.Since(startTS).Seconds(),
				HTTPStatus:    0,
				Retries:       retries,
				ErrorKind:     classifyErrorKind(0, err.Error()),
				ErrorMessage:  truncateErr(err.Error(), 256),
				GuardWaitSec:  guardWaitSec,
				CircuitOpened: opened,
			}
			debugErr = rec.ErrorMessage
			emit(rec)
			return nil, lastErr
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("读取响应失败: %w", err)
		}
		lastStatus = resp.StatusCode
		if debug != nil {
			debug.setResponse(resp.StatusCode, respBody)
		}

		if resp.StatusCode == 200 {
			c.recordSuccess()
			if c.Guard != nil {
				c.Guard.OnSuccess()
			}
			var result types.APIResponse
			if respReq != nil {
				result, err = parseOpenAIResponsesResponse(req.Model, respBody)
				if err != nil {
					return nil, err
				}
			} else if oaiReq != nil {
				result, err = parseOpenAIResponse(req.Model, respBody)
				if err != nil {
					return nil, err
				}
			} else if err := json.Unmarshal(respBody, &result); err != nil {
				return nil, fmt.Errorf("解析响应失败: %w", err)
			}
			c.onSuccess429Reset()
			rec := LLMCallRecord{
				Status:       "success",
				Request:      "messages",
				DurationSec:  time.Since(startTS).Seconds(),
				HTTPStatus:   200,
				Retries:      retries,
				StopReason:   result.StopReason,
				GuardWaitSec: guardWaitSec,
			}
			if result.Model != "" {
				rec.Model = result.Model
			}
			if retries > 0 {
				rec.Status = "retry_success"
			}
			if result.Usage != nil {
				rec.InputTokens = result.Usage.InputTokens
				rec.OutputTokens = result.Usage.OutputTokens
				rec.CacheReadTokens = result.Usage.CacheReadInputTokens
				rec.CacheCreationTokens = result.Usage.CacheCreationInputTokens
				rec.TotalTokens = rec.InputTokens + rec.OutputTokens + rec.CacheReadTokens + rec.CacheCreationTokens
			}
			emit(rec)
			return &result, nil
		}

		// 自适应关闭 prompt cache 必须**优先于**普通重试 (与流式路径同口径)。
		//
		// 顺序理由: 一旦 400 被配置进 RetryStatuses, 若先走重试分支, 就会用同一份
		// 带 cache_control 的请求体重发 —— 上游会以同样的理由再拒一次, 白白耗掉
		// 重试次数。先去断点重发才是"自适应"的原意: 缓存是偏好, 可用性才是前提。
		//
		// 判据从 req.CacheControl != nil 改为 shouldEnablePromptCache(): 非流式路径
		// 从不设置顶层 CacheControl (cache_control 打在 system 块上, 见
		// buildSystemParamNonStream), 旧判据恒为假, 这条兜底其实从未生效过。
		if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
			c.shouldEnablePromptCache() && isCacheRelatedError(resp.StatusCode, string(respBody)) {
			log.Printf("[api] prompt cache 不被支持, 自适应关闭: %s", string(respBody)[:min(len(respBody), 200)])
			c.promptCacheDisabledByError = true
			req.CacheControl = nil
			req.System = buildSystemParamNonStreamPlanned(systemPrompt, false, nil)
			body, _ = json.Marshal(req)
			continue // 重试一次
		}

		// 不可重试的客户端错误 (401/403 等) — 不计入熔断器
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && !c.retryableStatus(resp.StatusCode) {
			errMsg := fmt.Sprintf("API 返回 %d: %s", resp.StatusCode, string(respBody))
			rec := LLMCallRecord{
				Status:       "error",
				Request:      "messages",
				DurationSec:  time.Since(startTS).Seconds(),
				HTTPStatus:   resp.StatusCode,
				Retries:      retries,
				ErrorKind:    classifyErrorKind(resp.StatusCode, string(respBody)),
				ErrorMessage: truncateErr(errMsg, 256),
				GuardWaitSec: guardWaitSec,
			}
			debugErr = rec.ErrorMessage
			emit(rec)
			return nil, fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
		}

		// 可重试: 429 / 5xx
		lastErr = fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))

		// 解析 Retry-After + 分类 429
		retryAfterSec := ParseRetryAfter(resp, respBody)
		if resp.StatusCode == 429 {
			kind := Classify429(resp.StatusCode, string(respBody))
			if !kind.ShouldRetry() {
				return nil, fmt.Errorf("API 429 (%s): %s", kind, string(respBody))
			}
			if c.Guard != nil {
				c.Guard.On429(retryAfterSec)
			}
			// 智能模型切换: 连续 429 达阈值时切换备用模型, 用新模型重试
			if newModel := c.on429OrFallback(); newModel != "" {
				// 先观测再切换: From 必须是切换前的模型。
				c.notifyFallback(req.Model, newModel, resp.StatusCode)
				req.Model = newModel
				if respReq != nil {
					respReq.Model = newModel
					body, _ = json.Marshal(respReq)
				} else if oaiReq != nil {
					oaiReq.Model = newModel
					body, _ = json.Marshal(oaiReq)
				} else {
					body, _ = json.Marshal(req)
				}
				if c.FallbackBaseURL != "" {
					effectiveBaseURL = c.FallbackBaseURL
				}
				if c.FallbackAPIKey != "" {
					effectiveAPIKey = c.FallbackAPIKey
				}
				continue
			}
		} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
			if c.Guard != nil {
				c.Guard.On429(retryAfterSec)
			}
		}

		if attempt < maxRetry {
			delay := c.retryDelay(attempt, resp.StatusCode, nil)
			// 优先使用 Retry-After
			if retryAfterSec > 0 {
				raDelay := time.Duration(retryAfterSec*1000) * time.Millisecond
				jitter := time.Duration(rand.Float64()*2000) * time.Millisecond
				raDelay += jitter
				if raDelay > delay {
					delay = raDelay
				}
			}
			c.TotalRetries.Add(1)
			retries++
			statusHint := "服务端错误"
			if resp.StatusCode == 429 {
				statusHint = fmt.Sprintf("限流(%s)", Classify429(resp.StatusCode, string(respBody)))
			} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
				statusHint = "过载"
			}
			guardStats := ""
			if c.Guard != nil {
				guardStats = " [" + c.Guard.Stats() + "]"
			}
			log.Printf("[api] SendMessage %s(尝试 %d/%d): %.0fs 后重试%s", statusHint, attempt+1, maxRetry+1, delay.Seconds(), guardStats)
			c.fireEvent("retry", fmt.Sprintf("LLM %s(尝试 %d/%d), %.0fs 后重试%s", statusHint, attempt+1, maxRetry+1, delay.Seconds(), guardStats))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
	}

	// 主模型全部重试失败 — 尝试 FallbackModels
	if len(c.FallbackModels) > 0 && isFallbackEligible(lastStatus, lastErr) {
		// 若配置了独立的 fallback 端点, 计算一次
		fbBaseURL := c.BaseURL
		fbAPIKey := c.APIKey
		if c.FallbackBaseURL != "" {
			fbBaseURL = c.FallbackBaseURL
		}
		if c.FallbackAPIKey != "" {
			fbAPIKey = c.FallbackAPIKey
		}
		for fi, fbModel := range c.FallbackModels {
			if fbModel == c.Model || fbModel == "" {
				continue
			}
			log.Printf("[api] 主模型 %s 不可用, 尝试备用模型 %d/%d: %s",
				c.Model, fi+1, len(c.FallbackModels), fbModel)
			c.fireEvent("retry", fmt.Sprintf("切换备用模型 %s", fbModel))
			// 同上: 观测在赋值之前, From 取 req.Model (可能已被 429 智能切换改过),
			// 而不是 c.Model —— 否则 A→B 之后接着回退时 From 会错记成 A。
			c.notifyFallback(req.Model, fbModel, lastStatus)

			req.Model = fbModel
			var fbBody []byte
			if respReq != nil {
				respReq.Model = fbModel
				fbBody, err = json.Marshal(respReq)
			} else if oaiReq != nil {
				oaiReq.Model = fbModel
				fbBody, err = json.Marshal(oaiReq)
			} else {
				fbBody, err = json.Marshal(req)
			}
			if err != nil {
				continue
			}
			if debug != nil {
				debug.addAttempt(maxRetry+fi+2, fbBaseURL+c.endpointPath(), req.Model, fbBody)
			}
			httpReq, err := http.NewRequestWithContext(ctx, "POST", fbBaseURL+c.endpointPath(), bytes.NewReader(fbBody))
			if err != nil {
				continue
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("x-api-key", fbAPIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
			httpReq.Header.Set("Authorization", "Bearer "+fbAPIKey)
			setTraceHeaders(httpReq, ctx) // 降级路径同样带 trace, 否则 fallback 的调用在网关侧对不上账
			c.applyExtraHeaders(httpReq)  // 降级路径保持同一会话头
			resp, err := c.Client.Do(httpReq)
			if err != nil {
				continue
			}
			respBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				continue
			}
			if debug != nil {
				debug.setResponse(resp.StatusCode, respBody)
			}
			if resp.StatusCode == 200 {
				c.recordSuccess()
				var result types.APIResponse
				if respReq != nil {
					result, err = parseOpenAIResponsesResponse(fbModel, respBody)
					if err != nil {
						continue
					}
				} else if oaiReq != nil {
					result, err = parseOpenAIResponse(fbModel, respBody)
					if err != nil {
						continue
					}
				} else if err := json.Unmarshal(respBody, &result); err != nil {
					continue
				}
				rec := LLMCallRecord{
					Status:       "retry_success",
					Request:      "messages",
					DurationSec:  time.Since(startTS).Seconds(),
					HTTPStatus:   200,
					Retries:      retries + fi + 1,
					StopReason:   result.StopReason,
					GuardWaitSec: guardWaitSec,
					Model:        fbModel,
					BaseURL:      fbBaseURL,
				}
				if result.Usage != nil {
					rec.InputTokens = result.Usage.InputTokens
					rec.OutputTokens = result.Usage.OutputTokens
					rec.CacheReadTokens = result.Usage.CacheReadInputTokens
					rec.CacheCreationTokens = result.Usage.CacheCreationInputTokens
					rec.TotalTokens = rec.InputTokens + rec.OutputTokens + rec.CacheReadTokens + rec.CacheCreationTokens
				}
				emit(rec)
				log.Printf("[api] 备用模型 %s 成功", fbModel)
				return &result, nil
			}
		}
	}

	opened, _ := c.recordFailure(lastErr.Error())
	c.TotalFails.Add(1)
	c.fireEvent("fatal", fmt.Sprintf("LLM 调用 %d 次全部失败: %v", maxRetry+1, lastErr))
	rec := LLMCallRecord{
		Status:        "error",
		Request:       "messages",
		DurationSec:   time.Since(startTS).Seconds(),
		HTTPStatus:    lastStatus,
		Retries:       retries,
		ErrorKind:     classifyErrorKind(lastStatus, lastErr.Error()),
		ErrorMessage:  truncateErr(lastErr.Error(), 256),
		GuardWaitSec:  guardWaitSec,
		CircuitOpened: opened,
	}
	debugErr = rec.ErrorMessage
	emit(rec)
	return nil, fmt.Errorf("LLM 调用 %d 次全部失败: %w", maxRetry+1, lastErr)
}

// isFallbackEligible 判断失败是否适合切换备用模型。
// 适用: 模型不支持(400)、配额耗尽(402/403)、超载(529)、连续限流(429 多次)。
func isFallbackEligible(httpStatus int, err error) bool {
	if err == nil {
		return false
	}
	switch httpStatus {
	case 400, 402, 403, 529:
		return true
	}
	errStr := strings.ToLower(err.Error())
	for _, kw := range []string{"not supported", "not found", "invalid model",
		"quota", "insufficient", "overloaded", "unavailable"} {
		if strings.Contains(errStr, kw) {
			return true
		}
	}
	return false
}

// effectiveModel 返回当前应使用的模型名 (考虑智能切换状态)。
// 如果备用模型冷却期已过, 自动恢复主模型。
func (c *Client) effectiveModel() string {
	if len(c.FallbackModels) == 0 {
		return c.Model
	}
	c.fbMu.Lock()
	defer c.fbMu.Unlock()

	if c.fallbackActiveModel == "" {
		return c.Model
	}

	cooldown := c.fallbackCooldownMin
	if cooldown <= 0 {
		cooldown = 5
	}
	if time.Since(c.fallbackActiveSince) > time.Duration(cooldown)*time.Minute {
		log.Printf("[api] 冷却期(%d分钟)已过, 恢复主模型 %s (从备用 %s)",
			cooldown, c.Model, c.fallbackActiveModel)
		c.fallbackActiveModel = ""
		c.consecutive429 = 0
		c.fireEvent("model_restore", fmt.Sprintf("恢复主模型 %s", c.Model))
		return c.Model
	}

	return c.fallbackActiveModel
}

// on429OrFallback 处理 429 限流, 达到阈值时切换备用模型。
// 返回切换后的模型名 (空字符串=不切换, 继续等待)。
func (c *Client) on429OrFallback() string {
	if len(c.FallbackModels) == 0 {
		return ""
	}
	c.fbMu.Lock()
	defer c.fbMu.Unlock()

	c.consecutive429++
	const threshold = 3 // 连续 3 次 429 触发切换

	if c.consecutive429 < threshold {
		return ""
	}

	// 选择第一个可用的备用模型
	for _, fb := range c.FallbackModels {
		if fb != "" && fb != c.Model && fb != c.fallbackActiveModel {
			log.Printf("[api] 连续 %d 次 429 限流, 切换到备用模型: %s → %s",
				c.consecutive429, c.Model, fb)
			c.fallbackActiveModel = fb
			c.fallbackActiveSince = time.Now()
			c.consecutive429 = 0
			c.fireEvent("model_switch", fmt.Sprintf("429 限流切换: %s → %s", c.Model, fb))
			return fb
		}
	}
	return ""
}

// onSuccess429Reset 成功调用后重置 429 计数器。
func (c *Client) onSuccess429Reset() {
	c.fbMu.Lock()
	c.consecutive429 = 0
	c.fbMu.Unlock()
}

// SimpleComplete 简单文本补全: 发送 system+user prompt, 返回回复文本。
// 实现 dreaming.LLMClient 接口。
// simpleCompleteMaxTokens 是 SimpleComplete / CompleteDiag 的输出上限。
// 与单 agent 路径(executePipeline 用配置 AI.maxTokens=32768/子配置 65536)看齐并从宽取 65536。
// 演进史(踩坑记录):
//
//	① 原 4096 → 大章节抽取 JSON 中途截断 → ParseJSON 失败;
//	② 抬到 16384 修好多数, 但后端模型(kimi)会输出 thinking 块且 thinking token 计入本上限,
//	   密集章节 thinking+JSON >16384 → stop=max_tokens: 要么 JSON 截断在半途(不可解析),
//	   要么 thinking 吃光 16384 令 text 为空("空响应")。单 agent 走 32768 故同内容不炸。
//	③ 故从宽取 65536, 给 thinking+完整 JSON 充足余量。是"上限非目标", 短输出正常 end_turn 早停,
//	   对其它调用方零副作用。注意:此值硬编码, 若日后调大 config AI.maxTokens 需同步(未做动态解析)。
const simpleCompleteMaxTokens = 65536

// SimpleCompleteMaxTokens 是上面那个上限的导出形态, 给 L1 网关的 remote 实现
// (pkg/llmgw/remote.go) 复用同一个值。
//
// 为什么不让那边自己写 65536: 两处各写一份的漂移表现很具体 ——
// "同一段 prompt 在 local 档能出完整 JSON, 换成 remote 档就被截断在半途",
// 而截断的现场只会留下一句 stop=max_tokens, 没人会想到是两个常量不一样。
const SimpleCompleteMaxTokens = simpleCompleteMaxTokens

// SimpleComplete 发起一次最简补全 (system + 单条 user 文本), 只回拼文本块 ——
// 供 advisor/裁判这类内部工具复用, 不挂工具也不做流式。
func (c *Client) SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []types.APIMessage{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":` + string(mustMarshalString(userPrompt)) + `}]`),
	}}
	resp, err := c.SendMessage(ctx, messages, []string{systemPrompt}, nil, simpleCompleteMaxTokens)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// CompleteDiag 等价于 SimpleComplete(相同请求/maxTokens),但额外回传一行诊断元数据(diag):
// stop=停止原因 / outTok=输出token / blocks=内容块数与类型 / textLen=文本长度 / tail=文本尾部。
// 供合议扇出(graph-extract-swarm)定位"空响应/截断/超时"的确切根因:
//
//	· 空响应 + stop=max_tokens + outTok 大 → 内容进了非 text 块(如 thinking), 我们没取到;
//	· 空响应 + outTok=0 → 后端确实返回空(模型/网关问题);
//	· 非空但 parseOK=false + stop=max_tokens → 输出被上限截断在 JSON 中途(需再抬上限/减输入);
//	· err=context deadline → PerBranchTimeout 截断(需放宽超时)。
//
// 仅诊断用途, 不改变 SimpleComplete 的行为与其它调用方。
func (c *Client) CompleteDiag(ctx context.Context, systemPrompt, userPrompt string) (text, diag string, err error) {
	messages := []types.APIMessage{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":` + string(mustMarshalString(userPrompt)) + `}]`),
	}}
	resp, err := c.SendMessage(ctx, messages, []string{systemPrompt}, nil, simpleCompleteMaxTokens)
	if err != nil {
		return "", fmt.Sprintf("SendMessage 出错: %v", err), err
	}
	var sb strings.Builder
	blockTypes := make([]string, 0, len(resp.Content))
	for _, block := range resp.Content {
		blockTypes = append(blockTypes, string(block.Type))
		if block.Type == types.ContentBlockText {
			sb.WriteString(block.Text)
		}
	}
	text = sb.String()
	outTok := 0
	if resp.Usage != nil {
		outTok = resp.Usage.OutputTokens
	}
	tail := text
	if len(tail) > 160 {
		tail = "…" + tail[len(tail)-160:]
	}
	diag = fmt.Sprintf("stop=%s outTok=%d blocks=%d%v textLen=%d tail=%q",
		resp.StopReason, outTok, len(resp.Content), blockTypes, len(text), tail)
	return text, diag, nil
}

// mustMarshalString 把字符串序列化成 JSON RawMessage (json.Marshal 对 string 不会失败, 故忽略 error)。
func mustMarshalString(s string) json.RawMessage {
	data, _ := json.Marshal(s)
	return data
}

// RawComplete 发送原始 content blocks 并返回文本结果。
// contentJSON 为 Anthropic Messages API content 数组的 JSON (如 [{"type":"text","text":"..."}])。
// 支持多模态消息(图文混排)。
func (c *Client) RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	messages := []types.APIMessage{{
		Role:    "user",
		Content: contentJSON,
	}}
	resp, err := c.SendMessage(ctx, messages, nil, nil, maxTokens)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// deltasCarryContent 判断一批 SSE 增量里是否已经出现"用户屏幕上看得见"的产出：
// 正文、思考，或工具调用（content_block_start 带 tool_use 就会画出工具卡片）。
//
// 刻意**不**把 message_start / message_delta(usage) 算进去：那是流建立的信号，
// 不是产出。按它们记出来的数是 TTFB，不是首字 —— 两者在上游预热时能差好几秒。
func deltasCarryContent(deltas []types.StreamDelta) bool {
	for _, d := range deltas {
		if d.Delta != nil && (d.Delta.Text != "" || d.Delta.Thinking != "" || d.Delta.PartialJSON != "") {
			return true
		}
		if d.ContentBlock != nil {
			switch d.ContentBlock.Type {
			case types.ContentBlockText, types.ContentBlockThinking, types.ContentBlockToolUse:
				return true
			}
		}
	}
	return false
}
