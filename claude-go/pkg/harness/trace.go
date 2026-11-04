// trace.go — 单轮 trace（Moqi R-H14）。
//
// # 为什么 trace 是结构体而不是一行日志
//
// 业务层事后要回答的问题几乎都是**跨维度**的："上周那些回复质量下降的 turn，
// 用的是哪个模型、检索到了几条证据、缓存命中多少"。日志能回答其中任意一条，
// 但无法在一次检索里把它们对齐到同一个 turn 上 —— 而对齐正是 RCA 的全部工作。
//
// 这里把一轮的全部可观测量收进一个结构，由宿主落库（agentDB / 任意 append log）。
//
// # 两条口径纪律
//
//  1. **缺失不等于零**。usage / cost 都有各自的 Known 位；缓存读写同理
//     （ProviderCacheKnown）。把"没采到"记成 0 会让失败的 turn 看起来免费，
//     也会让缓存命中率在故障期间虚高 —— 这是本仓踩过的坑（见 TurnResult.UsageKnown）。
//  2. **本地 hash 与 provider 指标分开**。本地 PrefixHash 只说明"我们自己认为前缀
//     没变"，provider 的 cache_read_tokens 才说明真的被复用。合成一个数就再也
//     分不出"缓存没命中"和"命中了但上游没报"。
package harness

import (
	"context"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// TurnTrace 是一轮的完整观测记录。
//
// 字段只增不删（R-H16）。
type TurnTrace struct {
	OwnerID   string `json:"ownerId,omitempty"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	EpisodeID string `json:"episodeId,omitempty"`

	// Role 是成本归集用的角色；Model 是**实际使用**的模型（空 = engine 默认）。
	Role  string `json:"role,omitempty"`
	Model string `json:"model,omitempty"`

	// PromptLayout 是本轮的稳定/动态分层与断点（R-H2）。
	PromptLayout *PromptLayout `json:"promptLayout,omitempty"`

	// ContextBlockIDs 是本轮临时上下文块的 ID（R-H1）。只记 ID，不记正文 ——
	// 正文属于用户数据，不该进运维日志。
	//
	// 这是**裁剪之后**真正进入请求的那一批：被 token 预算裁掉的块不在这里，
	// 它们的 ID 在 ContextDroppedIDs 里。两份 ID 合起来才是 provider 给出的全集。
	ContextBlockIDs []string `json:"contextBlockIds,omitempty"`
	// ContextTrace 是检索过程的可观测信息（业务层自定义形状，harness 只透传）。
	// provider 在一次 turn 里只被调用一次，所以这条 trace 与 ContextBlockIDs 同源。
	ContextTrace string `json:"contextTrace,omitempty"`
	// ContextDroppedIDs 是因 token 预算被裁掉的块 ID（R-H1）。
	//
	// 单独报而不是只报"裁了几条"：事后要回答"那条关键证据为什么没出现在回复里"，
	// 只有 ID 能定位；一个计数只能说明裁过，无法说明裁掉了什么。
	ContextDroppedIDs []string `json:"contextDroppedIds,omitempty"`
	// ContextTokens 是本轮临时上下文块的估算 token 总量（裁剪后）。
	// TokensKnown=false 时它无意义：provider 没给 Tokens 且只有空文本时会算成 0。
	ContextTokens      int  `json:"contextTokens,omitempty"`
	ContextTokensKnown bool `json:"contextTokensKnown,omitempty"`

	// FallbackChain 是本轮发生的模型回退链（R-H17）。
	//
	// 回退发生在 api 层（主模型 5xx 后按 FallbackModels 顺序重试），不改
	// TurnResult.Model —— 那是**路由**结果。把回退链单列，才能同时回答
	// "路由选了谁"与"最后是谁答的"，也才能把静默换模型与合法回退区分开。
	FallbackChain []FallbackEvent `json:"fallbackChain,omitempty"`

	// Hooks 是每个内置 hook 在本轮的执行记录（R-H7）。
	//
	// 逐 hook 而不是一个总耗时：业务要回答的是"哪一步把这一轮拖慢了/静默失败了"，
	// 一个总和只能说"慢"，说不出"谁慢"。记录按 hook 实际完成顺序追加（并行工具
	// 路径下顺序不保证与 Priority 一致），每条自带 Name/Phase 可自行归组。
	Hooks []HookRecord `json:"hooks,omitempty"`

	ToolCalls []ToolCallRecord `json:"toolCalls,omitempty"`
	Citations []Citation       `json:"citations,omitempty"`

	Usage      types.Usage `json:"usage"`
	UsageKnown bool        `json:"usageKnown"`
	Cost       float64     `json:"cost,omitempty"`
	CostKnown  bool        `json:"costKnown"`

	Safety   SafetyVerdict  `json:"safety"`
	Degraded []string       `json:"degraded,omitempty"`
	Terminal types.Terminal `json:"terminal"`
	// Partial 表示 Text/Blocks 是被中断前收到的部分内容（R-H10）。
	Partial bool `json:"partial,omitempty"`

	// Blockers 是本轮阻塞/失败的可读原因列表（脱敏后）。
	Blockers []string `json:"blockers,omitempty"`

	StartedAt  time.Time `json:"startedAt"`
	DurationMS int64     `json:"durationMs"`
	EventCount int       `json:"eventCount"`
	Persisted  bool      `json:"persisted"`
}

// HookRecord 是单个内置 hook 一次执行/跳过的观测记录（R-H7）。
//
// 只含**类别标签**，不含用户正文：Result 是引擎归纳出的稳定短标签
// （如 prompt_modified / messages_injected），Err 是错误文本但已由引擎侧裁剪。
type HookRecord struct {
	Name string `json:"name"`
	// Phase 是 hook 介入的阶段（pre_request / pre_tool_use / ...）。
	Phase string `json:"phase,omitempty"`
	// Criticality 是 critical / advisory。advisory 的失败不应终止本轮，
	// 记下来才能证明"它失败了但没影响结果"。
	Criticality string `json:"criticality,omitempty"`
	// DurationMS 是该 hook 自身耗时（毫秒，保留小数以看见亚毫秒 hook）。
	DurationMS float64 `json:"durationMs"`
	// Result 是结果归纳标签；失败或跳过时可能为空。
	Result string `json:"result,omitempty"`
	// Error 是失败原因（成功为空）。
	Error string `json:"error,omitempty"`
	// Panic 是 recover 到的 panic 文本（未 panic 为空）。
	Panic string `json:"panic,omitempty"`
	// TimedOut 表示该 hook 被自身的超时中止。
	TimedOut bool `json:"timedOut,omitempty"`
	// Skipped 表示因前序 hook 终止/跳过而本次未执行。
	Skipped bool `json:"skipped,omitempty"`
}

// TraceSink 接收每轮 trace。
//
// 语义是**每轮恰好一次**，在 RunTurn 返回之前调用（包括失败轮）。调用方不应在
// 这里做重活：它是观测面，不是事务的一部分。写失败不影响 TurnResult。
type TraceSink interface {
	RecordTurnTrace(ctx context.Context, tr TurnTrace) error
}

// TraceFromResult 从 TurnResult 组装 trace。
//
// 抽成公开函数而不是在 RunTurn 里内联：宿主可能在自己的 API 层补录 trace
// （例如 RunTurn 之前就失败的请求），两边必须用同一套字段映射，否则同一张表里
// 会混进两种口径的记录。
func TraceFromResult(res *TurnResult, role string, started time.Time, events int) TurnTrace {
	if res == nil {
		return TurnTrace{}
	}
	tr := TurnTrace{
		OwnerID:    res.OwnerID,
		SessionID:  res.SessionID,
		TurnID:     res.TurnID,
		EpisodeID:  res.EpisodeID,
		Role:       role,
		Model:      res.Model,
		ToolCalls:  res.ToolCalls,
		Citations:  res.Citations,
		Usage:      res.Usage,
		UsageKnown: res.UsageKnown,
		Cost:       res.Cost,
		CostKnown:  res.CostKnown,
		Safety:     res.Safety,
		Degraded:   res.Degraded,
		Terminal:   res.Terminal,
		Partial:    res.Partial,
		StartedAt:  started,
		DurationMS: time.Since(started).Milliseconds(),
		EventCount: events,
		Persisted:  res.Persisted,
	}
	if res.Terminal.Error != nil {
		// 只记**分类与终态理由**，不记错误正文：错误里可能带请求片段
		// （密钥、用户输入），而 trace 会进运维日志。
		tr.Blockers = append(tr.Blockers, res.Terminal.Reason)
	}
	return tr
}
