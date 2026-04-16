package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const WebSearchToolName = "WebSearch"

// webSearchInput 对应 LLM 传入的 JSON。
type webSearchInput struct {
	Query          string   `json:"query"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

// WebSearchTool Web 搜索工具。
type WebSearchTool struct {
	searcher WebSearcher
}

// NewWebSearchTool 构造 WebSearch 工具实例。
// 若 searcher 为 nil，降级为说明性指引。
func NewWebSearchTool(searcher WebSearcher) *WebSearchTool {
	return &WebSearchTool{searcher: searcher}
}

func (t *WebSearchTool) Name() string { return WebSearchToolName }

func (t *WebSearchTool) Description() string {
	return `Search the web and return results. Uses a configured search engine (default: Bing). Returns search results as structured text.`
}

func (t *WebSearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query string."
				},
				"allowed_domains": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Optional list of domains to restrict or prefer in results (hint for the search)."
				}
			},
			"required": ["query"]
		}`)
}

func (t *WebSearchTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *WebSearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 只读实现：不拒绝。
func (t *WebSearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 发起 Web 搜索。若浏览器不可用，返回说明性指引。
func (t *WebSearchTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in webSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return &tool.ToolResult{Content: "错误: query 不能为空", IsError: true}, nil
	}

	// 尝试真实搜索
	if t.searcher != nil {
		title, text, _, err := t.searcher.Search(ctx, q)
		if err == nil && strings.TrimSpace(text) != "" {
			var b strings.Builder
			if title != "" {
				fmt.Fprintf(&b, "【搜索结果】%s\n\n", title)
			}
			b.WriteString(text)
			if len(in.AllowedDomains) > 0 {
				b.WriteString("\n\n建议限制的域名: ")
				b.WriteString(strings.Join(in.AllowedDomains, ", "))
			}
			return &tool.ToolResult{Content: strings.TrimSpace(b.String())}, nil
		}
	}

	// 降级: 说明性指引
	var b strings.Builder
	b.WriteString("【WebSearch 未配置浏览器】未发起真实网络搜索。\n\n")
	fmt.Fprintf(&b, "你的查询: %q\n", q)

	if len(in.AllowedDomains) > 0 {
		doms := make([]string, 0, len(in.AllowedDomains))
		for _, d := range in.AllowedDomains {
			d = strings.TrimSpace(d)
			if d != "" {
				doms = append(doms, d)
			}
		}
		if len(doms) > 0 {
			b.WriteString("\n建议限制的域名: ")
			b.WriteString(strings.Join(doms, ", "))
			b.WriteString("\n")
		}
	}

	b.WriteString(`
建议下一步（任选）:
1) 请用户在聊天中直接粘贴搜索结果摘要或链接列表。
2) 若环境允许出站 HTTP，可使用 Bash 工具执行 curl 调用你已有的搜索 HTTP API。
3) 通过 MCP 接入带搜索能力的工具服务。

本响应为说明性结果（非错误），便于模型继续规划后续工具调用。
`)

	return &tool.ToolResult{Content: strings.TrimSpace(b.String())}, nil
}
