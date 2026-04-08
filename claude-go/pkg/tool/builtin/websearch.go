// websearch.go 实现 WebSearch 工具：声明式 Web 搜索（本仓库为简化桩实现）。
// 对应 TS 源码: review/claude/src/tools/WebSearchTool/WebSearchTool.ts
//
// 设计要点:
//   - 只读、并发安全；TS 侧常标记 shouldDefer（真正发起搜索前需用户/策略确认）。
//   - 无内置搜索 API Key 时无法调用 Google/Bing 等，故 Call 不发起真实搜索请求，
//     而是返回可操作指引：请用户粘贴结果、或使用 Bash+curl 调用公开 API、或配置 MCP。
//
// 算法说明:
//   1. 解析 query 与可选 allowed_domains。
//   2. 将域名约束格式化为人类可读列表写入回复，便于模型在后续步骤中遵守。
//   3. 组装固定模板的指导文本返回给模型（IsError=false，属于“说明性成功响应”）。
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

// WebSearchTool Web 搜索占位/指引工具。
type WebSearchTool struct{}

// NewWebSearchTool 构造 WebSearch 工具实例。
func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{}
}

func (t *WebSearchTool) Name() string { return WebSearchToolName }

func (t *WebSearchTool) Description() string {
	return `Web search (simplified stub). Returns guidance when no search API is configured: ask the user to provide results, use Bash with curl against a search API, or use an MCP search tool.`
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
				"description": "Optional list of domains to restrict or prefer in results (hint for the user/model)."
			}
		},
		"required": ["query"]
	}`)
}

func (t *WebSearchTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *WebSearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 只读桩实现：不拒绝；真实搜索若接入需在上层策略中 defer/确认。
func (t *WebSearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 不调用外部搜索 API，返回结构化指引文本。
func (t *WebSearchTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in webSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return &tool.ToolResult{Content: "错误: query 不能为空", IsError: true}, nil
	}

	var b strings.Builder
	b.WriteString("【WebSearch 简化实现】当前未集成在线搜索 API，未发起真实网络搜索。\n\n")
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
			b.WriteString("\n建议限制的域名（allowed_domains）: ")
			b.WriteString(strings.Join(doms, ", "))
			b.WriteString("\n")
		}
	}

	b.WriteString(`
建议下一步（任选）:
1) 请用户在聊天中直接粘贴搜索结果摘要或链接列表。
2) 若环境允许出站 HTTP，可使用 Bash 工具执行 curl 调用你已有的搜索 HTTP API（注意 API Key 与速率限制）。
3) 通过 MCP 接入带搜索能力的工具服务。

本响应为说明性结果（非错误），便于模型继续规划后续工具调用。
`)

	return &tool.ToolResult{Content: strings.TrimSpace(b.String())}, nil
}
