package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const WebSearchToolName = "WebSearch"

// webSearchInput 对应 LLM 传入的 JSON。过滤字段由数据面强制执行，不是提示。
type webSearchInput struct {
	Query            string   `json:"query"`
	OriginalQuestion string   `json:"original_question,omitempty"`
	Limit            int      `json:"limit,omitempty"`
	MaxResults       int      `json:"max_results,omitempty"` // Claude 历史调用兼容别名；与 limit 冲突时拒绝
	Language         string   `json:"language,omitempty"`
	Freshness        string   `json:"freshness,omitempty"`
	AllowedDomains   []string `json:"allowed_domains,omitempty"`
	BlockedDomains   []string `json:"blocked_domains,omitempty"`
}

// WebSearchTool Web 搜索工具。
type WebSearchTool struct {
	searcher WebSearcher
}

// NewWebSearchTool 构造 WebSearch 工具实例。
func NewWebSearchTool(searcher WebSearcher) *WebSearchTool {
	if searcher == nil {
		if d := DefaultDataStack(); d != nil {
			searcher = d.Search
		}
	}
	return &WebSearchTool{searcher: searcher}
}

func (t *WebSearchTool) Name() string { return WebSearchToolName }

func (t *WebSearchTool) Description() string {
	return `Search the web through a multi-provider layer. Returns structured JSON with url, title, snippet, provider, published time, status and degradation details. Domain filters are enforced after provider responses. Use original_question to preserve the user's broader context when query is a focused keyword search.`
}

// InputSchema 与 webSearchInput 一一对应；未知字段在 Call 中也会被拒绝。
func (t *WebSearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"query": {"type": "string", "description": "Self-contained search query."},
			"original_question": {"type": "string", "description": "Optional original user question used for relevance ranking."},
			"limit": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum number of results; default 8."},
			"max_results": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Compatibility alias for limit; supplying conflicting values is an error."},
			"language": {"type": "string", "description": "Optional language preference such as zh or en."},
			"freshness": {"type": "string", "enum": ["any", "day", "week", "month", "year"], "description": "Optional recency window."},
			"allowed_domains": {"type": "array", "items": {"type": "string"}, "description": "Only return these domains and their subdomains."},
			"blocked_domains": {"type": "array", "items": {"type": "string"}, "description": "Exclude these domains and their subdomains."}
		},
		"required": ["query"]
	}`)
}

func (t *WebSearchTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *WebSearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *WebSearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 严格解析参数并调用真实数据面。高级参数绝不会静默退化为纯 query。
func (t *WebSearchTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in webSearchInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("WebSearch input error: %v", err), IsError: true}, nil
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return &tool.ToolResult{Content: fmt.Sprintf("WebSearch input error: %v", err), IsError: true}, nil
	}
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return &tool.ToolResult{Content: "WebSearch input error: query must not be empty", IsError: true}, nil
	}
	if t.searcher == nil {
		return &tool.ToolResult{Content: "WebSearch unavailable: no search data plane is configured", IsError: true}, nil
	}

	limit := in.Limit
	if in.MaxResults != 0 {
		if limit != 0 && limit != in.MaxResults {
			return &tool.ToolResult{Content: "WebSearch input error: limit and max_results must match when both are supplied", IsError: true}, nil
		}
		limit = in.MaxResults
	}
	req := search.SearchRequest{
		Query: q, OriginalQuestion: in.OriginalQuestion, Limit: limit,
		Language: in.Language, Freshness: in.Freshness,
		AllowedDomains: in.AllowedDomains, BlockedDomains: in.BlockedDomains,
	}
	if structured, ok := t.searcher.(interface {
		SearchRequestJSON(context.Context, search.SearchRequest) (string, error)
	}); ok {
		out, err := structured.SearchRequestJSON(ctx, req)
		if err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("WebSearch error: %v", err), IsError: true}, nil
		}
		return &tool.ToolResult{Content: out}, nil
	}

	// 旧浏览器接口只能承载 query。只要调用方使用了高级参数就必须明确失败，
	// 否则域名、时效或数量限制会被静默丢弃。
	if in.OriginalQuestion != "" || limit != 0 || in.Language != "" || in.Freshness != "" || len(in.AllowedDomains) > 0 || len(in.BlockedDomains) > 0 {
		return &tool.ToolResult{Content: "WebSearch error: this legacy searcher does not support advanced search parameters", IsError: true}, nil
	}
	if js, ok := t.searcher.(interface {
		SearchJSON(context.Context, string) (string, error)
	}); ok {
		out, err := js.SearchJSON(ctx, q)
		if err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("WebSearch error: %v", err), IsError: true}, nil
		}
		return &tool.ToolResult{Content: out}, nil
	}
	title, text, _, err := t.searcher.Search(ctx, q)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("WebSearch error: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(text) != "" {
		var b strings.Builder
		if title != "" {
			fmt.Fprintf(&b, "【搜索结果】%s\n\n", title)
		}
		b.WriteString(text)
		return &tool.ToolResult{Content: strings.TrimSpace(b.String())}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("WebSearch: no results for %q (providers returned empty)", q)}, nil
}
