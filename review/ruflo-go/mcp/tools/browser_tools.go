package tools

import (
	"context"

	"github.com/ruflo/ruflo-go/mcp"
)

const browserUnavailable = "browser automation not available in Go runtime"

func browserPlaceholderHandler(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: false, Error: browserUnavailable}
}

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

// RegisterBrowserTools registers placeholder browser automation tools.
func RegisterBrowserTools(reg *mcp.ToolRegistry) error {
	for _, t := range browserTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}
