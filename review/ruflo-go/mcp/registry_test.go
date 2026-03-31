package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegisterAndCall(t *testing.T) {
	t.Parallel()
	r := NewToolRegistry()
	tool := &MCPTool{
		Name:        "echo",
		Description: "echo",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			return args, nil
		},
	}
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	out, err := r.Call(context.Background(), "echo", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1}` {
		t.Fatalf("got %s", out)
	}
}

func TestListTools(t *testing.T) {
	t.Parallel()
	r := NewToolRegistry()
	_ = r.Register(&MCPTool{
		Name:    "z",
		Handler: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	})
	_ = r.Register(&MCPTool{
		Name:    "a",
		Handler: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	})
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].Name != "a" || list[1].Name != "z" {
		t.Fatalf("order: %v, %v", list[0].Name, list[1].Name)
	}
}

func TestCallUnknown(t *testing.T) {
	t.Parallel()
	r := NewToolRegistry()
	_, err := r.Call(context.Background(), "nope", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
}
