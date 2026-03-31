package mcp

import (
	"context"
	"encoding/json"
)

// MCPToolResult is the standard JSON shape returned by tool handlers.
type MCPToolResult struct {
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error string         `json:"error,omitempty"`
}

// EncodeToolResult marshals a tool result to JSON for MCP responses.
func EncodeToolResult(r MCPToolResult) (json.RawMessage, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// MCPTool describes one Model Context Protocol tool.
type MCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// MCPServer wraps a tool registry for serving MCP over a transport.
type MCPServer struct {
	Registry *ToolRegistry
	Name     string
	Version  string
}

// NewMCPServer returns a server using the given registry.
func NewMCPServer(reg *ToolRegistry) *MCPServer {
	if reg == nil {
		reg = NewToolRegistry()
	}
	return &MCPServer{
		Registry: reg,
		Name:     "ruflo",
		Version:  "3.5.0",
	}
}

// ToolsListSchema returns MCP-style tool descriptors for tools/list.
func (s *MCPServer) ToolsListSchema() []map[string]any {
	tools := s.Registry.List()
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return out
}
