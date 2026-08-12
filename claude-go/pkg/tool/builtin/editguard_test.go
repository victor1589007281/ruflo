package builtin

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func TestEditGuardGate(t *testing.T) {
	bad := "package main\nfunc broken( {\n"
	// 关: 放行
	if res := guardWriteSyntax("/tmp/x.go", bad, &tool.ToolContext{}); res != nil {
		t.Error("WeakEditGuard=false 时应放行")
	}
	// 开: 拦截
	res := guardWriteSyntax("/tmp/x.go", bad, &tool.ToolContext{WeakEditGuard: true})
	if res == nil || !res.IsError || !strings.Contains(res.Content, "edit_guard") {
		t.Errorf("坏 Go 文件应被拦截, 得 %+v", res)
	}
}

func TestEditGuardGoSyntax(t *testing.T) {
	ctx := &tool.ToolContext{WeakEditGuard: true}
	good := "package main\n\nfunc add(a, b int) int { return a + b }\n"
	if res := guardWriteSyntax("/tmp/ok.go", good, ctx); res != nil {
		t.Errorf("合法 Go 应通过, 得 %v", res.Content)
	}
}

func TestEditGuardJSON(t *testing.T) {
	ctx := &tool.ToolContext{WeakEditGuard: true}
	if res := guardWriteSyntax("/tmp/ok.json", `{"a":1}`, ctx); res != nil {
		t.Errorf("合法 JSON 应通过, 得 %v", res.Content)
	}
	if res := guardWriteSyntax("/tmp/bad.json", `{"a":}`, ctx); res == nil {
		t.Error("坏 JSON 应被拦截")
	}
}

func TestEditGuardPython(t *testing.T) {
	ctx := &tool.ToolContext{WeakEditGuard: true}
	if res := guardWriteSyntax("/tmp/ok.py", "def add(a, b):\n    return a + b\n", ctx); res != nil {
		t.Errorf("合法 Python 应通过, 得 %v", res.Content)
	}
	res := guardWriteSyntax("/tmp/bad.py", "def add(a, b\n    return a +\n", ctx)
	if python3Available() && res == nil {
		t.Error("坏 Python 应被拦截 (python3 可用时)")
	}
}

func TestEditGuardOtherExtPass(t *testing.T) {
	ctx := &tool.ToolContext{WeakEditGuard: true}
	if res := guardWriteSyntax("/tmp/x.txt", "anything {{{", ctx); res != nil {
		t.Error("未知扩展名应放行")
	}
	if res := guardWriteSyntax("/tmp/x.md", "# free form", ctx); res != nil {
		t.Error("markdown 应放行")
	}
}

func python3Available() bool {
	res := guardWriteSyntax("/tmp/probe.py", "x = 1\n", &tool.ToolContext{WeakEditGuard: true})
	return res == nil // 合法 python 通过仅说明流程走通; 此探测仅为可读性占位
}
