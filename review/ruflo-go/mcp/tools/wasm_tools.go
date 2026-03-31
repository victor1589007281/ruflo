package tools

import (
	"context"

	"github.com/ruflo/ruflo-go/mcp"
)

const wasmUnavailable = "WASM agent runtime not available"

func wasmPlaceholderHandler(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: false, Error: wasmUnavailable}
}

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

// RegisterWasmTools registers placeholder WASM agent tools.
func RegisterWasmTools(reg *mcp.ToolRegistry) error {
	for _, t := range wasmTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}
