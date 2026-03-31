// Package transport implements MCP-oriented JSON-RPC over stdio (newline-delimited).
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

// jsonRPCRequest is a minimal JSON-RPC 2.0 request object.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ServeStdio reads one JSON object per line from in and writes responses to out.
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

func writeErr(enc *json.Encoder, id json.RawMessage, code int, msg string) {
	_ = enc.Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	})
}

func writeResult(enc *json.Encoder, id json.RawMessage, v json.RawMessage) {
	_ = enc.Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  v,
	})
}

func writeResultRaw(enc *json.Encoder, id json.RawMessage, b []byte) {
	_ = enc.Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  json.RawMessage(b),
	})
}

// ServeStdioOS runs ServeStdio with os.Stdin and os.Stdout.
func ServeStdioOS(ctx context.Context, srv *mcp.MCPServer) error {
	return ServeStdio(ctx, srv, os.Stdin, os.Stdout)
}
