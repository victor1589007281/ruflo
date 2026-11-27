package mcp

// client_readonly_test.go —— MCPTool.IsReadOnly 的注解优先 + 启发式兜底测试。
//
// 事故背景 (2026-11-28): webapp 内置浏览器的 browser_open/browser_get_text
// 名字不带读前缀, 纯启发式判成写工具, Plan 模式的只读 agent (行研官) 被整面
// 拦截。服务端 annotations.readOnlyHint 才是正解 (MCP 2025-03 规范)。

import (
	"encoding/json"
	"testing"
)

func toolWith(name, annotations string) *MCPTool {
	raw := `{"name":"` + name + `","description":"d","inputSchema":{"type":"object"}`
	if annotations != "" {
		raw += `,"annotations":` + annotations
	}
	raw += `}`
	var info ToolInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		panic(err)
	}
	return &MCPTool{info: info}
}

func TestIsReadOnlyHonorsAnnotation(t *testing.T) {
	// 名字不带读前缀, 但服务端明说只读 → 放行。
	if !toolWith("browser_open", `{"readOnlyHint":true}`).IsReadOnly(nil) {
		t.Error("readOnlyHint=true 应判只读")
	}
	if !toolWith("browser_get_text", `{"readOnlyHint":true}`).IsReadOnly(nil) {
		t.Error("readOnlyHint=true 应判只读")
	}
}

func TestIsReadOnlyFallsBackToHeuristic(t *testing.T) {
	// 无注解: 名字像读的放行。
	if !toolWith("get_page", "").IsReadOnly(nil) {
		t.Error("get_ 前缀应判只读")
	}
	if !toolWith("docs_search", "").IsReadOnly(nil) {
		t.Error("_search 后缀应判只读")
	}
	// 无注解: 名字不像读的按写处理 (保守默认)。
	if toolWith("browser_click", "").IsReadOnly(nil) {
		t.Error("无注解的 browser_click 应判写")
	}
	// 显式 readOnlyHint=false 不拦截启发式 (与缺席同义)。
	if !toolWith("get_page", `{"readOnlyHint":false}`).IsReadOnly(nil) {
		t.Error("readOnlyHint=false 应回落启发式")
	}
	if toolWith("browser_eval", `{"readOnlyHint":false}`).IsReadOnly(nil) {
		t.Error("readOnlyHint=false + 写名字应判写")
	}
}
