package api

import (
	"context"
	"sort"
)

// PromptComponentMetrics 拆解单次 LLM 请求中各类提示词/上下文的字符数。
// 字符数比 token 更便宜、稳定, 下游可按 chars/4 近似估算 token。
type PromptComponentMetrics struct {
	SystemChars       int `json:"system_chars"`
	ToolsSchemaChars  int `json:"tools_schema_chars"`
	MCPToolsChars     int `json:"mcp_tools_chars"`
	SkillListingChars int `json:"skill_listing_chars"`
	RoleSkillsChars   int `json:"role_skills_chars"`
	MemoryChars       int `json:"memory_chars"`
	BlackboardChars   int `json:"blackboard_chars"`
	PrevResultChars   int `json:"prev_result_chars"`
	MessagesChars     int `json:"messages_chars"`
}

// LLMMetricsContext 是 QueryEngine 传给 api.Client 的单次调用标签与 prompt 账本。
type LLMMetricsContext struct {
	Source           string
	Purpose          string
	Workflow         string
	Role             string
	PromptComponents PromptComponentMetrics
}

type llmMetricsContextKey struct{}

// WithLLMMetrics 将单次 LLM 调用的业务标签和 prompt 账本放入 context。
func WithLLMMetrics(ctx context.Context, meta LLMMetricsContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, llmMetricsContextKey{}, meta)
}

func llmMetricsFromContext(ctx context.Context) LLMMetricsContext {
	if ctx == nil {
		return LLMMetricsContext{}
	}
	if meta, ok := ctx.Value(llmMetricsContextKey{}).(LLMMetricsContext); ok {
		return meta
	}
	return LLMMetricsContext{}
}

func applyLLMMetricsContext(rec *LLMCallRecord, meta LLMMetricsContext) {
	if rec == nil {
		return
	}
	if meta.Source != "" {
		rec.Source = meta.Source
	}
	if meta.Purpose != "" {
		rec.Purpose = meta.Purpose
	}
	if meta.Workflow != "" {
		rec.Workflow = meta.Workflow
	}
	if meta.Role != "" {
		rec.Role = meta.Role
	}
	if isZeroPromptComponents(rec.PromptComponents) {
		rec.PromptComponents = meta.PromptComponents
	}
}

func isZeroPromptComponents(v PromptComponentMetrics) bool {
	return v.SystemChars == 0 &&
		v.ToolsSchemaChars == 0 &&
		v.MCPToolsChars == 0 &&
		v.SkillListingChars == 0 &&
		v.RoleSkillsChars == 0 &&
		v.MemoryChars == 0 &&
		v.BlackboardChars == 0 &&
		v.PrevResultChars == 0 &&
		v.MessagesChars == 0
}

// ─── F10 表面定价 (dsh token-meter 的 Go 侧适配) ─────────────────────────
//
// dsh 的语义锚点 (packages/llm/token-meter):
//   - TokenSurfaceNode: 上下文"表面"上的一个可定价节点 (按位置逐节点定价)。
//   - TokenMeasurement: 一次测量的整体结果, 含 usage 锚点与定价表版本 (logRevision)。
//   - projection.ts 的设计纪律: 启发式构成数字**永远不是总量** ("Present these as
//     approximations of composition, never as a total") —— 估算器系统性低估 CJK 与
//     JSON schema。所以这里把启发式 (chars/4) 与 usage 锚定 (真实 InputTokens 按
//     启发式占比再分配) 作为**两条独立指标**并存, 而不是用后者修正前者。
//
// 本仓对应物: PromptComponentMetrics 的 9 个组件就是上下文表面上的 9 类节点。
// MLLMPromptComponentTokens 是启发式分量 (既有, 不动); MLLMPromptSurfaceTokens
// 是锚定分量 (新增, AllocatePromptSurface 产出)。

// TokenPricingRevision 表面定价规则的版本号。定价规则 (chars/4 启发式、组件清单、
// 再分配算法) 任何一次变化都必须递增它 —— 否则历史样本与新样本混在同一个
// histogram 里时, 消费方无从分辨口径。对齐 dsh 的 logRevision 字段。
const TokenPricingRevision = 1

// TokenSurfaceNode 表面上的一个可定价节点: 一个提示词组件及其锚定后 token 份额。
type TokenSurfaceNode struct {
	// Component 节点位置/组件名 (与启发式指标的 component 标签同一词汇表:
	// system_chars / tools_schema_chars / ... / messages_chars)。
	Component string `json:"component"`
	// Chars 原始字符数 (直接来自 PromptComponentMetrics, 不参与再分配)。
	Chars int `json:"chars"`
	// EstTokens 启发式估算 token (chars/4, 与 MLLMPromptComponentTokens 同口径)。
	EstTokens float64 `json:"est_tokens"`
	// Tokens usage 锚定后的 token 份额 (总和恰为真实 InputTokens)。
	Tokens int `json:"tokens"`
	// SharePct 该组件锚定份额占总量的百分比 (0-100, 一位小数)。
	SharePct float64 `json:"share_pct"`
}

// TokenMeasurement 一次 LLM 调用的表面定价结果。
type TokenMeasurement struct {
	// TotalInputTokens usage 锚点: 网关回传的真实输入 token (0 = 网关未回,
	// 此时**不产出**表面定价 —— 绝不拿启发式冒充总量, 见本文件头部纪律)。
	TotalInputTokens int `json:"total_input_tokens"`
	// InputEstimated 锚点本身是否为估算值 (网关不回 input、按 chars/4 兜底)。
	// 估算锚点仍可做构成展示, 但消费方须知道"总量"也是估的。
	InputEstimated bool `json:"input_estimated"`
	// Surfaces 锚定后的逐节点份额, 按 Component 字典序 (确定性输出, 测试可断言)。
	Surfaces []TokenSurfaceNode `json:"surfaces"`
	// PricingRevision 产出这份定价时的规则版本 (TokenPricingRevision 的快照)。
	PricingRevision int64 `json:"pricing_revision"`
}

// surfacePart 一个组件在再分配算法里的静态行。顺序即 TokenPricingRevision=1
// 的定价表; 调整组件清单必须递增 TokenPricingRevision。
var surfaceParts = []struct {
	name  string
	chars func(PromptComponentMetrics) int
}{
	{"system_chars", func(p PromptComponentMetrics) int { return p.SystemChars }},
	{"tools_schema_chars", func(p PromptComponentMetrics) int { return p.ToolsSchemaChars }},
	{"mcp_tools_chars", func(p PromptComponentMetrics) int { return p.MCPToolsChars }},
	{"skill_listing_chars", func(p PromptComponentMetrics) int { return p.SkillListingChars }},
	{"role_skills_chars", func(p PromptComponentMetrics) int { return p.RoleSkillsChars }},
	{"memory_chars", func(p PromptComponentMetrics) int { return p.MemoryChars }},
	{"blackboard_chars", func(p PromptComponentMetrics) int { return p.BlackboardChars }},
	{"prev_result_chars", func(p PromptComponentMetrics) int { return p.PrevResultChars }},
	{"messages_chars", func(p PromptComponentMetrics) int { return p.MessagesChars }},
}

// AllocatePromptSurface 把一次调用的真实输入 token 按组件启发式占比再分配到
// 表面上 (F10 usage 锚点)。规则:
//   - estTotal == 0 (无组件信息) 时返回 Surfaces=nil: **键缺失语义**, 与 ledger
//     "缺数据不落 0" 同源 —— 拿零组件去分总量会得到一个全员 0 的假象。
//   - anchorTokens <= 0 (网关未回 input 且无估算兜底) 同样返回 nil: 不捏造。
//   - 整数余数按"份额最大者 +1"修正, 保证 Σ tokens == anchorTokens (失配会
//     静默错价, 必须在分配处收敛, 对齐 dsh priceSurface 的 fail-loud 纪律)。
//   - SharePct 以 anchorTokens 为分母 (总量即锚点), 而非启发式估算总量。
func AllocatePromptSurface(pc PromptComponentMetrics, anchorTokens int, inputEstimated bool) TokenMeasurement {
	m := TokenMeasurement{
		TotalInputTokens: anchorTokens,
		InputEstimated:   inputEstimated,
		PricingRevision:  TokenPricingRevision,
	}
	if anchorTokens <= 0 {
		return m
	}

	type row struct {
		name  string
		chars int
		est   float64
	}
	var rows []row
	estTotal := 0.0
	for _, part := range surfaceParts {
		chars := part.chars(pc)
		if chars <= 0 {
			continue
		}
		est := float64(chars) / 4.0
		rows = append(rows, row{name: part.name, chars: chars, est: est})
		estTotal += est
	}
	if estTotal <= 0 {
		return m // 零组件: 键缺失, 不捏造构成
	}

	// 按启发式占比分配; 余数 (整数化的舍入误差) 交给最大份额者。
	allocated := 0
	m.Surfaces = make([]TokenSurfaceNode, 0, len(rows))
	for _, r := range rows {
		share := float64(r.est) / estTotal
		tokens := int(share * float64(anchorTokens))
		allocated += tokens
		m.Surfaces = append(m.Surfaces, TokenSurfaceNode{
			Component: r.name,
			Chars:     r.chars,
			EstTokens: r.est,
			Tokens:    tokens,
		})
	}
	if remainder := anchorTokens - allocated; remainder > 0 {
		// 最大份额者收尾: 它最可能"吞得下"舍入残差, 且不会被一个 1-char 组件
		// 抬出畸形的份额。
		maxIdx := 0
		for i := range m.Surfaces {
			if m.Surfaces[i].Tokens > m.Surfaces[maxIdx].Tokens {
				maxIdx = i
			}
		}
		m.Surfaces[maxIdx].Tokens += remainder
	}

	sort.Slice(m.Surfaces, func(i, j int) bool { return m.Surfaces[i].Component < m.Surfaces[j].Component })
	for i := range m.Surfaces {
		m.Surfaces[i].SharePct = float64(int(
			float64(m.Surfaces[i].Tokens)*1000/float64(anchorTokens))) / 10.0
	}
	return m
}
