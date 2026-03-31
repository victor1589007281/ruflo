package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ToolRegistry stores MCP tools by name and dispatches calls.
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]*MCPTool
}

// NewToolRegistry returns an empty registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]*MCPTool)}
}

// Register adds or replaces a tool. Name must be non-empty.
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

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (*MCPTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns all registered tools (copy of slice, stable order by name sort would be nice - we sort).
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

// Call invokes a tool by name with JSON arguments (object or null).
func (r *ToolRegistry) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("mcp: unknown tool %q", name)
	}
	return t.Handler(ctx, args)
}
