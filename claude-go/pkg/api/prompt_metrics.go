package api

import "context"

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
