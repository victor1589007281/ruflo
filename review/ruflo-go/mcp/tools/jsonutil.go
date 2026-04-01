// jsonutil.go：MCP 工具共用的 JSON 参数解析与 MCPToolResult 适配辅助函数。

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ruflo/ruflo-go/mcp"
)

// argsAsMap 将 tools/call 的原始 JSON 参数解析为 map；空或 null 视为空对象。
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

// strArg 从 map 中读取字符串参数；支持 string 或其它类型通过 Sprint 转字符串。
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

// toolHandler 将「map 入参 -> MCPToolResult」的处理器适配为 MCP 标准的 RawMessage 入参/出参签名。
func toolHandler(fn func(context.Context, map[string]any) mcp.MCPToolResult) func(context.Context, json.RawMessage) (json.RawMessage, error) {
	return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		args, err := argsAsMap(raw)
		if err != nil {
			return mcp.EncodeToolResult(mcp.MCPToolResult{OK: false, Error: err.Error()})
		}
		return mcp.EncodeToolResult(fn(ctx, args))
	}
}

// jsonOK 将任意可 JSON 序列化的值编码为 RawMessage，供直接作为工具成功响应体。
func jsonOK(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// parseArgs 将 raw 反序列化到 dst；空或 null 时不修改 dst。
func parseArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
