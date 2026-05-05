package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	mcpToolSearchName = "MCPToolSearch"
	mcpToolInvokeName = "MCPToolInvoke"
)

func registerMCPProxyTools(reg *tool.Registry, mgr *dynmcp.Manager) {
	if mgr == nil {
		return
	}
	reg.Register(&mcpToolSearch{mgr: mgr})
	reg.Register(&mcpToolInvoke{mgr: mgr})
}

type mcpToolSearch struct {
	mgr *dynmcp.Manager
}

func (t *mcpToolSearch) Name() string { return mcpToolSearchName }

func (t *mcpToolSearch) Description() string {
	return "Search connected MCP tools by keyword. Use this before MCPToolInvoke; full MCP schemas are intentionally not injected into the base prompt."
}

func (t *mcpToolSearch) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Keyword, capability, or MCP tool name to search for."},
			"max_results": {"type": "integer", "description": "Maximum results to return, default 12, max 50."},
			"include_schema": {"type": "boolean", "description": "Whether to include a truncated input schema for matching tools."}
		}
	}`)
}

func (t *mcpToolSearch) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *mcpToolSearch) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *mcpToolSearch) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *mcpToolSearch) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Query         string `json:"query"`
		MaxResults    int    `json:"max_results"`
		IncludeSchema bool   `json:"include_schema"`
	}
	_ = json.Unmarshal(input, &in)
	if in.MaxResults <= 0 {
		in.MaxResults = 12
	}
	if in.MaxResults > 50 {
		in.MaxResults = 50
	}

	refs := searchMCPToolRefs(t.mgr.GetConnections(), in.Query, in.MaxResults)
	if len(refs) == 0 {
		return &tool.ToolResult{Content: "No matching MCP tools found."}, nil
	}

	var sb strings.Builder
	sb.WriteString("Matching MCP tools:\n")
	for _, ref := range refs {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", ref.FullName, truncateText(ref.Info.Description, 220)))
		if in.IncludeSchema && len(ref.Info.InputSchema) > 0 {
			sb.WriteString(fmt.Sprintf("  schema: %s\n", truncateText(compactJSON(ref.Info.InputSchema), 1200)))
		}
	}
	sb.WriteString("\nCall MCPToolInvoke with tool_name set to one of the names above.")
	return &tool.ToolResult{Content: sb.String()}, nil
}

type mcpToolInvoke struct {
	mgr *dynmcp.Manager
}

func (t *mcpToolInvoke) Name() string { return mcpToolInvokeName }

func (t *mcpToolInvoke) Description() string {
	return "Invoke a connected MCP tool by name after discovering it with MCPToolSearch. This keeps large MCP tool schemas out of normal prompts."
}

func (t *mcpToolInvoke) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"tool_name": {"type": "string", "description": "Full MCP tool name, e.g. mcp_ruflo_search, or an unambiguous raw tool name."},
			"arguments": {"type": "object", "description": "Arguments to pass to the MCP tool."}
		},
		"required": ["tool_name"]
	}`)
}

func (t *mcpToolInvoke) IsReadOnly(input json.RawMessage) bool {
	ref, ok := findMCPToolRef(t.mgr.GetConnections(), toolNameFromInput(input))
	if !ok {
		return false
	}
	name := strings.ToLower(ref.Info.Name)
	for _, prefix := range []string{"read", "get", "list", "search", "query", "fetch", "describe", "show", "status", "info", "check", "view"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (t *mcpToolInvoke) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *mcpToolInvoke) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *mcpToolInvoke) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ToolName  string          `json:"tool_name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("MCPToolInvoke input parse failed: %v", err), IsError: true}, nil
	}
	if len(in.Arguments) == 0 {
		in.Arguments = json.RawMessage(`{}`)
	}

	ref, ok := findMCPToolRef(t.mgr.GetConnections(), in.ToolName)
	if !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("MCP tool %q not found or ambiguous. Use MCPToolSearch first.", in.ToolName), IsError: true}, nil
	}
	result, err := ref.Conn.CallTool(ctx, ref.Info.Name, in.Arguments)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("MCP tool call failed: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: result}, nil
}

type mcpToolRef struct {
	FullName string
	Conn     *mcp.Connection
	Info     mcp.ToolInfo
}

func searchMCPToolRefs(conns []*mcp.Connection, query string, limit int) []mcpToolRef {
	refs := allMCPToolRefs(conns)
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		if len(refs) > limit {
			return refs[:limit]
		}
		return refs
	}

	var matches []mcpToolRef
	for _, ref := range refs {
		haystack := strings.ToLower(ref.FullName + " " + ref.Info.Name + " " + ref.Info.Description)
		ok := true
		for _, token := range tokens {
			if !strings.Contains(haystack, token) {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, ref)
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches
}

func findMCPToolRef(conns []*mcp.Connection, name string) (mcpToolRef, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcpToolRef{}, false
	}
	var matches []mcpToolRef
	for _, ref := range allMCPToolRefs(conns) {
		if name == ref.FullName || name == ref.Info.Name || name == ref.Conn.Config.Name+"."+ref.Info.Name {
			matches = append(matches, ref)
		}
	}
	if len(matches) != 1 {
		return mcpToolRef{}, false
	}
	return matches[0], true
}

func allMCPToolRefs(conns []*mcp.Connection) []mcpToolRef {
	var refs []mcpToolRef
	for _, conn := range conns {
		for _, info := range conn.Tools {
			refs = append(refs, mcpToolRef{
				FullName: fmt.Sprintf("mcp_%s_%s", conn.Config.Name, info.Name),
				Conn:     conn,
				Info:     info,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].FullName < refs[j].FullName })
	return refs
}

func toolNameFromInput(input json.RawMessage) string {
	var in struct {
		ToolName string `json:"tool_name"`
	}
	_ = json.Unmarshal(input, &in)
	return in.ToolName
}

func compactJSON(raw json.RawMessage) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(data)
}

func truncateText(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
