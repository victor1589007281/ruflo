// Package mcp 提供 Model Context Protocol 的服务端核心类型：工具描述、统一返回体、
// 以及挂载在 ToolRegistry 上的 MCPServer。JSON-RPC 2.0 的请求解析与读写循环在
// mcp/transport 中实现；进程级「走 CLI 还是走 MCP」的双入口（TTY 交互 vs 管道 stdin）
// 由 cmd/ruflo/main.go 根据参数个数与 term.IsTerminal(stdin) 判定。
package mcp

import (
	"context"
	"encoding/json"
)

// MCPToolResult 表示工具处理函数返回给 MCP 客户端的标准 JSON 形状（ok/data/error）。
type MCPToolResult struct {
	// OK 为 true 表示业务成功；false 时通常应设置 Error。
	OK bool `json:"ok"`
	// Data 为可选的键值负载，供客户端解析结构化结果。
	Data map[string]any `json:"data,omitempty"`
	// Error 为人类可读错误信息，与 OK=false 搭配使用。
	Error string `json:"error,omitempty"`
}

// EncodeToolResult 将 MCPToolResult 序列化为 json.RawMessage，供 tools/call 响应体嵌入。
func EncodeToolResult(r MCPToolResult) (json.RawMessage, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// MCPTool 描述一个 MCP 工具：对外名称、说明、JSON Schema 入参，以及实际处理函数。
type MCPTool struct {
	// Name 为 tools/call 时使用的唯一工具名。
	Name string
	// Description 供客户端展示的人类可读说明。
	Description string
	// InputSchema 为 JSON Schema 对象（通常 type=object），用于 tools/list。
	InputSchema map[string]any
	// Handler 接收已解析的 JSON 参数原始字节，返回工具结果 JSON 或错误。
	Handler func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// MCPServer 将 ToolRegistry 与服务器元信息绑定，供传输层驱动 tools/list 与 tools/call。
type MCPServer struct {
	// Registry 保存全部已注册工具，由传输层按名分发。
	Registry *ToolRegistry
	// Name 在 initialize 响应的 serverInfo 中返回。
	Name string
	// Version 在 initialize 响应的 serverInfo 中返回。
	Version string
}

// NewMCPServer 使用给定注册表构造服务器；reg 为 nil 时会创建空注册表。
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

// ToolsListSchema 生成符合 MCP 约定的工具描述列表（name、description、inputSchema），
// 供传输层在 tools/list 方法中直接序列化返回。
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
