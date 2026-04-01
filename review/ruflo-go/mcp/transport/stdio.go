// Package transport 实现基于标准输入/输出的 MCP 导向 JSON-RPC 2.0 传输：
// 每行一个 JSON 请求对象，响应同样以单行 JSON 写出，适合宿主通过管道与子进程通信。
package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ruflo/ruflo-go/mcp"
)

// jsonRPCRequest 为 JSON-RPC 2.0 请求的最小字段集（与 MCP 宿主对齐）。
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"` // 必须为 "2.0"
	ID      json.RawMessage `json:"id"`      // 请求关联 id，原样抄回响应
	Method  string          `json:"method"`  // 如 initialize、tools/list、tools/call
	Params  json.RawMessage `json:"params"`  // 方法参数原始 JSON
}

// jsonRPCResponse 为成功或失败时的统一响应外壳。
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"` // 成功时填充
	Error   *jsonRPCError   `json:"error,omitempty"`  // 失败时填充
}

// jsonRPCError 表示 JSON-RPC 错误对象（code/message/可选 data）。
type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// toolsCallParams 解析 tools/call 的 params：工具名与参数字节。
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ServeStdio 从 in 逐行读取 JSON-RPC 请求，将每条请求的响应写入 out（每行一个 JSON）。
// 支持 initialize、tools/list、tools/call；解析失败时返回 parse error 且 id 可能为 null。
// 受 ctx 取消时退出循环并返回 ctx.Err()。
func ServeStdio(ctx context.Context, srv *mcp.MCPServer, in io.Reader, out io.Writer) error {
	if srv == nil || srv.Registry == nil {
		return errors.New("transport: nil server or registry")
	}
	sc := bufio.NewScanner(in)
	// allow long lines
	const maxBuf = 16 * 1024 * 1024
	sc.Buffer(make([]byte, 0, 64*1024), maxBuf)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &jsonRPCError{Code: -32700, Message: "parse error"},
			})
			continue
		}
		if req.JSONRPC != "2.0" {
			writeErr(enc, req.ID, -32600, "invalid request")
			continue
		}
		switch req.Method {
		case "initialize":
			res, err := handleInitialize(srv, req.Params)
			if err != nil {
				writeErr(enc, req.ID, -32603, err.Error())
				continue
			}
			writeResult(enc, req.ID, res)
		case "tools/list":
			res := map[string]any{"tools": srv.ToolsListSchema()}
			b, _ := json.Marshal(res)
			writeResultRaw(enc, req.ID, b)
		case "tools/call":
			var p toolsCallParams
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, &p)
			}
			if p.Name == "" {
				writeErr(enc, req.ID, -32602, "invalid params: name required")
				continue
			}
			args := p.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			raw, err := srv.Registry.Call(ctx, p.Name, args)
			if err != nil {
				writeErr(enc, req.ID, -32000, err.Error())
				continue
			}
			wrap := map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(raw)},
				},
			}
			b, _ := json.Marshal(wrap)
			writeResultRaw(enc, req.ID, b)
		default:
			writeErr(enc, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
		}
	}
	return sc.Err()
}

// handleInitialize 构造 MCP initialize 结果：协议版本、tools 能力、serverInfo（名称与版本）。
func handleInitialize(srv *mcp.MCPServer, params json.RawMessage) (json.RawMessage, error) {
	_ = params
	res := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    srv.Name,
			"version": srv.Version,
		},
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// writeErr 写出 JSON-RPC error 响应（单行）。
func writeErr(enc *json.Encoder, id json.RawMessage, code int, msg string) {
	_ = enc.Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	})
}

// writeResult 写出带 result 字段的成功响应，result 已为合法 JSON 片段。
func writeResult(enc *json.Encoder, id json.RawMessage, v json.RawMessage) {
	_ = enc.Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  v,
	})
}

// writeResultRaw 与 writeResult 类似，但 result 由 []byte 包装为 RawMessage。
func writeResultRaw(enc *json.Encoder, id json.RawMessage, b []byte) {
	_ = enc.Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  json.RawMessage(b),
	})
}

// ServeStdioOS 使用 os.Stdin/os.Stdout 调用 ServeStdio，便于 CLI 直接作为 MCP 子进程挂接。
func ServeStdioOS(ctx context.Context, srv *mcp.MCPServer) error {
	return ServeStdio(ctx, srv, os.Stdin, os.Stdout)
}
