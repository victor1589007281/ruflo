package tools

// 本文件注册 WASM Agent 相关 MCP 工具名与 Schema，与上层「Agent Booster / WASM 沙箱」概念对齐。
//
// 设计思路：
//   - Go 侧尚未嵌入 WASM 运行时，所有调用由 wasmPlaceholderHandler 返回不可用错误，保证工具清单完整、行为可预期。
//   - 与 browser_tools 占位模式一致，便于后续替换为真实 wasm 执行实现。

import (
	"context"

	"github.com/ruflo/ruflo-go/mcp"
)

// wasmUnavailable 占位错误常量（英文，供客户端识别）。
const wasmUnavailable = "WASM agent runtime not available"

// wasmPlaceholderHandler 拒绝一切 WASM 相关调用。
func wasmPlaceholderHandler(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: false, Error: wasmUnavailable}
}

// wasmTools 构建 WASM agent / gallery 占位工具列表（空 object Schema）。
func wasmTools() []*mcp.MCPTool {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	names := []struct{ name, desc string }{
		{"wasm_agent_create", "Create a WASM sandbox agent"},
		{"wasm_agent_prompt", "Send prompt to WASM agent"},
		{"wasm_agent_tool", "Invoke tool inside WASM agent"},
		{"wasm_agent_list", "List WASM agents"},
		{"wasm_agent_terminate", "Terminate WASM agent"},
		{"wasm_agent_files", "List files in WASM agent workspace"},
		{"wasm_agent_export", "Export WASM agent artifacts"},
		{"wasm_gallery_list", "List WASM gallery entries"},
		{"wasm_gallery_search", "Search WASM gallery"},
		{"wasm_gallery_create", "Create WASM gallery entry"},
	}
	out := make([]*mcp.MCPTool, 0, len(names))
	for _, n := range names {
		out = append(out, &mcp.MCPTool{
			Name:        n.name,
			Description: n.desc,
			InputSchema: schema,
			Handler:     toolHandler(wasmPlaceholderHandler),
		})
	}
	return out
}

// RegisterWasmTools 向注册表登记 WASM Agent 占位 MCP 工具。
func RegisterWasmTools(reg *mcp.ToolRegistry) error {
	for _, t := range wasmTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}
