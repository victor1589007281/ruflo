package tools

// 本文件注册「浏览器自动化」相关 MCP 工具名称与 Schema，与 Node/Playwright 等实现对齐的 API 表面。
//
// 设计思路：
//   - 当前 Go 运行时未嵌入真实浏览器驱动，所有工具统一走 browserPlaceholderHandler，返回固定不可用错误，避免客户端因缺工具而崩溃。
//   - 保留完整工具清单便于协议兼容与未来接入真实实现时替换 Handler。

import (
	"context"

	"github.com/ruflo/ruflo-go/mcp"
)

// browserUnavailable 为占位实现返回给调用方的错误文案（英文便于与既有客户端匹配）。
const browserUnavailable = "browser automation not available in Go runtime"

// browserPlaceholderHandler 统一拒绝浏览器类调用，说明当前运行时未提供自动化能力。
func browserPlaceholderHandler(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: false, Error: browserUnavailable}
}

// browserTools 构建浏览器相关 MCP 工具切片：名称、英文描述、输入 Schema 均预定义，Handler 指向占位实现。
func browserTools() []*mcp.MCPTool {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}
	with := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	names := []struct {
		name, desc string
		schema     map[string]any
	}{
		{"browser_open", "Open a URL in the browser", with(map[string]any{"url": map[string]any{"type": "string"}})},
		{"browser_back", "Navigate back", empty},
		{"browser_forward", "Navigate forward", empty},
		{"browser_reload", "Reload current page", empty},
		{"browser_close", "Close browser or tab", empty},
		{"browser_snapshot", "Accessibility snapshot of the page", empty},
		{"browser_screenshot", "Capture screenshot", with(map[string]any{"path": map[string]any{"type": "string"}})},
		{"browser_click", "Click element", with(map[string]any{"selector": map[string]any{"type": "string"}})},
		{"browser_fill", "Fill input", with(map[string]any{"selector": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}})},
		{"browser_type", "Type text", with(map[string]any{"selector": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}})},
		{"browser_press", "Press key", with(map[string]any{"key": map[string]any{"type": "string"}})},
		{"browser_hover", "Hover element", with(map[string]any{"selector": map[string]any{"type": "string"}})},
		{"browser_select", "Select option", with(map[string]any{"selector": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}})},
		{"browser_check", "Check checkbox", with(map[string]any{"selector": map[string]any{"type": "string"}})},
		{"browser_uncheck", "Uncheck checkbox", with(map[string]any{"selector": map[string]any{"type": "string"}})},
		{"browser_scroll", "Scroll page", with(map[string]any{"delta_y": map[string]any{"type": "number"}})},
		{"browser_get-text", "Read element text", with(map[string]any{"selector": map[string]any{"type": "string"}})},
		{"browser_get-value", "Read input value", with(map[string]any{"selector": map[string]any{"type": "string"}})},
		{"browser_get-title", "Get document title", empty},
		{"browser_get-url", "Get current URL", empty},
		{"browser_wait", "Wait for condition", with(map[string]any{"ms": map[string]any{"type": "number"}})},
		{"browser_eval", "Evaluate JavaScript", with(map[string]any{"script": map[string]any{"type": "string"}})},
		{"browser_session-list", "List browser sessions", empty},
	}
	out := make([]*mcp.MCPTool, 0, len(names))
	for _, n := range names {
		out = append(out, &mcp.MCPTool{
			Name:        n.name,
			Description: n.desc,
			InputSchema: n.schema,
			Handler:     toolHandler(browserPlaceholderHandler),
		})
	}
	return out
}

// RegisterBrowserTools 向注册表登记浏览器自动化占位工具（接口齐全、实现为空）。
func RegisterBrowserTools(reg *mcp.ToolRegistry) error {
	for _, t := range browserTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}
