package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ToolRegistry 是 MCP 工具注册中心：按名称索引元数据与 Handler，
// 供 tools/list 枚举、tools/call 按名路由到具体处理函数。
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]*MCPTool // 工具名 -> 工具定义（含 InputSchema 与 Handler）
}

// NewToolRegistry 返回空的工具表，可逐步 Register 填充。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]*MCPTool)}
}

// Register 注册或覆盖同名工具；Name 非空且 Handler 非 nil，否则返回错误。
func (r *ToolRegistry) Register(tool *MCPTool) error {
	if tool == nil || tool.Name == "" {
		return fmt.Errorf("mcp: invalid tool")
	}
	if tool.Handler == nil {
		return fmt.Errorf("mcp: tool %q has nil handler", tool.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
	return nil
}

// Get 按名称查找工具，第二个返回值为是否命中。
func (r *ToolRegistry) Get(name string) (*MCPTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 返回当前已注册全部工具的切片副本；按 Name 字典序排序，保证 tools/list 输出稳定。
func (r *ToolRegistry) List() []*MCPTool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*MCPTool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	// deterministic order
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name < out[i].Name {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Call 根据工具名调用对应 Handler，args 通常为 JSON 对象或 null（由传输层规范化为 {}）。
func (r *ToolRegistry) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("mcp: unknown tool %q", name)
	}
	return t.Handler(ctx, args)
}
