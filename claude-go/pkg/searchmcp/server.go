// Package searchmcp 把搜索与行情数据面暴露为 MCP server。
//
// 与 pkg/platformmcp 的分工: 那里暴露平台运行态 (团队/工作流/任务), 这里只暴露
// 联网检索与结构化行情, 供 Claude Desktop、其它 agent 或 IDE 直接挂载。
//
// 工具实现直接复用 pkg/tool/builtin 的注册结果 —— 同一份 schema、同一套错误
// 语义。另写一套 MCP 专用实现迟早会分叉成"agent 里能用、MCP 里行为不同"。
package searchmcp

// server.go —— MCP 传输与 JSON-RPC 分发。
//
// 同时支持 stdio 与 HTTP: stdio 一帧一行 JSON, stdout 只放协议消息, 日志走
// stderr; HTTP 用同一套 handler, 便于 sidecar 以 29235 端口被集群内其它服务
// 挂载。方法集只覆盖 MCP 客户端实际会发的 initialize / tools/list / tools/call
// 与 server/discover, 工具本身直接复用 pkg/tool/builtin 的注册结果。
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// ProtocolVersion 与 platformmcp 保持一致。
const ProtocolVersion = "2024-11-05"

// Server 可并发处理请求。传输层会话只承载身份与 max_uses，不改变工具数据面。
type Server struct {
	reg      *tool.Registry
	names    []string
	sessions sessionBudget
}

// New 用进程级数据面构造 server; data 为 nil 时返回 nil (调用方应报配置错误)。
//
// 只暴露数据面工具, 不暴露文件/Shell 工具: MCP 客户端挂载本服务时拿到的
// 应当只是"查资料"的能力。
func New(data *builtin.DataStack) *Server {
	s, _ := NewWithMaxUses(data, 0)
	return s
}

// NewWithMaxUses 构造带服务端机械调用预算的 server。maxUses=0 保持原行为；
// 该值不进入工具 schema，模型不能在请求里修改。
func NewWithMaxUses(data *builtin.DataStack, maxUses int) (*Server, error) {
	if maxUses < 0 {
		return nil, fmt.Errorf("max_uses must not be negative: %d", maxUses)
	}
	if data == nil || data.Search == nil || data.Market == nil {
		return nil, nil
	}
	reg := tool.NewRegistry()
	reg.Register(builtin.NewWebSearchTool(data.Search))
	reg.Register(builtin.NewWebFetchTool())
	builtin.RegisterMarketTools(reg, data)
	names := make([]string, 0, len(reg.All()))
	for _, t := range reg.All() {
		names = append(names, t.Name())
	}
	return &Server{reg: reg, names: names, sessions: newSessionBudget(maxUses)}, nil
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// HandleRPC 处理一条 JSON-RPC 报文; 返回 nil 表示这是通知, 不应回包。
func (s *Server) HandleRPC(ctx context.Context, raw []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return marshalResponse(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
	}
	if len(req.ID) == 0 {
		// notification (initialized / cancelled): 不回包。
		return nil
	}

	switch req.Method {
	case "initialize":
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.handshake()})
	case "server/discover":
		// claude-go 自己的 MCP 客户端 (pkg/mcp) 走 2026-07-28 无状态协商, 第一步就是
		// server/discover; 只实现 initialize 的服务会在这一步被判连接失败, 且失败
		// 归因是 "method not found" 而不是 "协议不兼容" —— 能力中心里只会显示
		// 这个 MCP 服务器压根没连上。discover 与 initialize 返回同一份能力即可。
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.handshake()})
	case "ping":
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.names))
		for _, name := range s.names {
			t, ok := s.reg.Get(name)
			if !ok {
				continue
			}
			tools = append(tools, map[string]any{
				"name":        t.Name(),
				"description": t.Description(),
				"inputSchema": t.InputSchema(),
			})
		}
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}})
	case "tools/call":
		var p rpcParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}})
		}
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.callTool(ctx, p)})
	default:
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
	}
}

// handshake 是 initialize 与 server/discover 共用的能力声明。
//
// supportedVersions 只列本服务真正实现的版本: 客户端会从中挑一个再发后续请求,
// 多列一个没实现的版本等于把失败推迟到 tools/list。
func (s *Server) handshake() map[string]any {
	return map[string]any{
		"protocolVersion":   ProtocolVersion,
		"supportedVersions": []string{ProtocolVersion},
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"serverInfo":        map[string]any{"name": "claude-go-search", "version": "1.0.0"},
	}
}

// callTool 执行工具并把结果转成 MCP content 数组。
func (s *Server) callTool(ctx context.Context, p rpcParams) map[string]any {
	t, ok := s.reg.Get(p.Name)
	if !ok {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "unknown tool: " + p.Name}},
			"isError": true,
		}
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	sessionID := sessionIDFromContext(ctx)
	if p.Name == builtin.WebSearchToolName || p.Name == builtin.WebFetchToolName {
		if err := s.sessions.consume(sessionID); err != nil {
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
		}
	}
	res, err := t.Call(ctx, args, &tool.ToolContext{SessionID: sessionID})
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": res.Content}},
		"isError": res.IsError,
	}
}

// ServeStdio 以换行分隔的 JSON 服务 stdin/stdout, 直到 ctx 取消或 EOF。
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	sessionID, err := s.sessions.create()
	if err != nil {
		return err
	}
	defer s.sessions.remove(sessionID)
	ctx = withSessionID(ctx, sessionID)
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriter(out)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := s.HandleRPC(ctx, bytes.TrimSpace(line)); resp != nil {
				if _, werr := w.Write(append(resp, '\n')); werr != nil {
					return werr
				}
				if werr := w.Flush(); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Handler 返回 HTTP JSON-RPC 入口 (POST)。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		var envelope struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			switch envelope.Method {
			case "initialize", "server/discover":
				sessionID, serr := s.sessions.create()
				if serr != nil {
					http.Error(w, "session creation failed", http.StatusInternalServerError)
					return
				}
				ctx = withSessionID(ctx, sessionID)
				w.Header().Set(SessionHeader, sessionID)
			default:
				sessionID := r.Header.Get(SessionHeader)
				if s.sessions.enabled() && sessionID == "" {
					http.Error(w, errSessionRequired.Error(), http.StatusBadRequest)
					return
				}
				if sessionID != "" {
					if !s.sessions.valid(sessionID) {
						if s.sessions.enabled() {
							http.Error(w, errSessionInvalid.Error(), http.StatusNotFound)
							return
						}
					} else {
						ctx = withSessionID(ctx, sessionID)
					}
				}
			}
		}
		resp := s.HandleRPC(ctx, body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	})
}

// ServeHTTP 在 addr 上监听并服务, 直到 ctx 取消。
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func marshalResponse(resp rpcResponse) []byte {
	b, err := json.Marshal(resp)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal error"}}`)
	}
	return b
}
