package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ruflo/ruflo-go/mcp"
)

func argsAsMap(raw json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func strArg(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// toolHandler adapts map-based handlers to MCP JSON args/results.
func toolHandler(fn func(context.Context, map[string]any) mcp.MCPToolResult) func(context.Context, json.RawMessage) (json.RawMessage, error) {
	return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		args, err := argsAsMap(raw)
		if err != nil {
			return mcp.EncodeToolResult(mcp.MCPToolResult{OK: false, Error: err.Error()})
		}
		return mcp.EncodeToolResult(fn(ctx, args))
	}
}

func jsonOK(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func parseArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
