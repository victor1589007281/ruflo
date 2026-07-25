// Package platformmcp 实现 design/02 §3.5 L5 用户层的 platform-mcp-server:
// 把平台自身的能力 (teams / workflows / skills / tools / TaskService) 以 MCP
// 工具形态对外, 作为"远程 agent 管理"的标准入口之一。
//
// 与 pkg/codeintel 的 MCP server 的分工:
//   - codeintel 暴露的是**代码图谱**能力 (navigate/impact/find_refs...);
//   - 本包暴露的是**平台运行态**能力 (有哪些团队/工作流/技能, 提交与查询任务)。
//
// 两者都是 MCP server, 但一个是"看代码"、一个是"管平台"。
//
// 两种挂载形态:
//   - HTTP: Handler() 挂到 :18080 的 /api/mcp/rpc (由 pkg/dashboard 完成),
//     天然落在 pkg/httpauth 的 /api/ 保护前缀内 —— 新增对外端点必须可鉴权,
//     这也是选 /api/ 前缀而不是 /mcp 的原因 (后者不在白名单里, 会成为绕过口);
//   - stdio: ServeStdio(), 供 `claude-go platform-mcp-server` 子命令使用,
//     Claude Desktop / 其他 agent 按标准 stdio MCP 配置即可接。
//
// 设计取舍:
//   - Backend 用 any 返回值而不是具体 DTO: 平台的 DTO 定义在 pkg/dashboard,
//     本包若引用就成了 dashboard → platformmcp → dashboard 的循环。MCP 的线上
//     形态本来就是 JSON 文本, 让 Backend 直接给可序列化对象最省事。
//   - 只读工具无条件可用; 写工具 (task_submit) 在没有 TaskService 时如实返回
//     "本进程无任务服务", 不谎称已受理 —— 与 /api/actions 的诚实化同一原则。
package platformmcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ProtocolVersion 是本 server 声明的 MCP 协议版本, 与 pkg/codeintel 的 MCP
// server 保持一致, 免得同一二进制里两个 server 对外声明不同版本。
const ProtocolVersion = "2024-11-05"

// ErrUnavailable 表示能力在本进程不可用 (例如只读 dashboard 没有任务服务)。
// 返回它时工具结果是 isError=true 的文本, 而不是 JSON-RPC 协议层错误 ——
// 协议层错误意味着"请求不合法", 而这里请求是合法的、只是环境不具备。
var ErrUnavailable = errors.New("平台能力在本进程不可用")

// TaskSubmit 一次任务提交的入参 (对应 agent.TaskSpec 的最小子集)。
type TaskSubmit struct {
	Team      string `json:"team"`
	Workflow  string `json:"workflow,omitempty"`
	Objective string `json:"objective"`
	Language  string `json:"language,omitempty"`
}

// Backend 是平台能力的提供方, 由 pkg/dashboard 实现 (它持有 Provider 与
// 注入的 TaskService)。
type Backend interface {
	Teams() (any, error)
	Team(name string) (any, error)
	Workflows() (any, error)
	Skills() (any, error)
	Tools() (any, error)
	Tasks() (any, error)
	SubmitTask(spec TaskSubmit) (any, error)
	TaskStatus(id string) (any, error)
}

// Server 平台 MCP server。无状态, 可并发使用。
type Server struct {
	be      Backend
	name    string
	version string
}

// New 构造 server。be 为 nil 时全部工具返回 ErrUnavailable (而不是 panic):
// 独立 dashboard 也应该能起 HTTP 端点并如实说明能力缺失。
func New(be Backend) *Server {
	return &Server{be: be, name: "claude-go-platform", version: "1.0.0"}
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 报文
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

// Tool 一个 MCP 工具的声明。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// 工具表
// ---------------------------------------------------------------------------

const (
	ToolTeamsList     = "platform_teams_list"
	ToolTeamGet       = "platform_team_get"
	ToolWorkflowsList = "platform_workflows_list"
	ToolSkillsList    = "platform_skills_list"
	ToolToolsList     = "platform_tools_list"
	ToolTasksList     = "platform_tasks_list"
	ToolTaskSubmit    = "platform_task_submit"
	ToolTaskStatus    = "platform_task_status"
)

var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Tools 返回工具清单。导出以便契约测试断言"工具名不漂移"——
// MCP 工具名一旦被下游 agent 写进配置就成了契约, 与 :18080 的路径同等对待。
func (s *Server) Tools() []Tool {
	return []Tool{
		{
			Name:        ToolTeamsList,
			Description: "列出 claude-go 平台上的全部 agent 团队 (名称/工作流/状态/进度)。",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        ToolTeamGet,
			Description: "查看单个团队详情 (成员/阶段/产出报告/时间线)。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"团队名"}},"required":["name"]}`),
		},
		{
			Name:        ToolWorkflowsList,
			Description: "列出可用的编排工作流 (名称/描述/阶段), 供选择 platform_task_submit 的 workflow 参数。",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        ToolSkillsList,
			Description: "列出平台已加载的技能 (SKILL.md) 清单。",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        ToolToolsList,
			Description: "列出平台内置工具 (Read/Write/Bash/...) 清单与入参 schema。",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        ToolTasksList,
			Description: "列出 TaskService 里的任务档案 (状态/团队/目标)。",
			InputSchema: emptyObjectSchema,
		},
		{
			Name: ToolTaskSubmit,
			Description: "向 TaskService 提交一次团队运行任务 (幂等: 同团队同目标的活跃任务会归并到同一个任务 ID)。" +
				"本进程未接入任务服务时返回错误而不是静默排队。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"team":{"type":"string","description":"团队名 (运行身份)"},` +
				`"workflow":{"type":"string","description":"工作流名; 团队已存在时可省"},` +
				`"objective":{"type":"string","description":"目标描述"},` +
				`"language":{"type":"string","description":"编程语言 (影响编译门禁)"}` +
				`},"required":["team","objective"]}`),
		},
		{
			Name:        ToolTaskStatus,
			Description: "按任务 ID 查询 TaskService 里的任务档案。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"任务 ID"}},"required":["id"]}`),
		},
	}
}

// ---------------------------------------------------------------------------
// 分发
// ---------------------------------------------------------------------------

// HandleRPC 处理一条 JSON-RPC 报文。
// 第二个返回值为 false 表示这是通知 (无 id), 不应回响应。
func (s *Server) HandleRPC(raw []byte) ([]byte, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: nil,
			Error: &rpcError{Code: -32700, Message: "Parse error", Data: err.Error()}}), true
	}
	isNotification := req.ID == nil

	switch req.Method {
	case "initialize":
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": s.name, "version": s.version},
		}}), true
	case "notifications/initialized":
		return nil, false
	case "ping":
		if isNotification {
			return nil, false
		}
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}), true
	case "tools/list":
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"tools": s.Tools()}}), true
	case "tools/call":
		if isNotification {
			return nil, false
		}
		return s.handleToolsCall(req), true
	default:
		if isNotification {
			return nil, false
		}
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "Method not found", Data: req.Method}}), true
	}
}

func (s *Server) handleToolsCall(req rpcRequest) []byte {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "Invalid params", Data: err.Error()}})
	}
	data, err := s.execute(params.Name, params.Arguments)
	if errors.Is(err, errToolNotFound) {
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "Tool not found", Data: params.Name}})
	}
	if err != nil {
		// 工具级失败走 isError, 不走协议错误: 调用方 (agent) 需要把失败原因当成
		// 内容读进上下文, 而 JSON-RPC error 在多数客户端里会被当成传输故障。
		return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolResult{
			Content: []toolContent{{Type: "text", Text: err.Error()}}, IsError: true,
		}})
	}
	return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolResult{
		Content: []toolContent{{Type: "text", Text: data}},
	}})
}

var errToolNotFound = errors.New("tool not found")

func (s *Server) execute(name string, args json.RawMessage) (string, error) {
	if s.be == nil {
		switch name {
		case ToolTeamsList, ToolTeamGet, ToolWorkflowsList, ToolSkillsList,
			ToolToolsList, ToolTasksList, ToolTaskSubmit, ToolTaskStatus:
			return "", fmt.Errorf("%w: 未注入 Backend", ErrUnavailable)
		default:
			return "", errToolNotFound
		}
	}
	switch name {
	case ToolTeamsList:
		return jsonOf(s.be.Teams())
	case ToolTeamGet:
		var in struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return "", err
		}
		if strings.TrimSpace(in.Name) == "" {
			return "", errors.New("name 必填")
		}
		return jsonOf(s.be.Team(in.Name))
	case ToolWorkflowsList:
		return jsonOf(s.be.Workflows())
	case ToolSkillsList:
		return jsonOf(s.be.Skills())
	case ToolToolsList:
		return jsonOf(s.be.Tools())
	case ToolTasksList:
		return jsonOf(s.be.Tasks())
	case ToolTaskSubmit:
		var in TaskSubmit
		if err := decodeArgs(args, &in); err != nil {
			return "", err
		}
		if strings.TrimSpace(in.Objective) == "" {
			return "", errors.New("objective 必填")
		}
		if strings.TrimSpace(in.Team) == "" {
			return "", errors.New("team 必填 (团队名 = 运行身份)")
		}
		return jsonOf(s.be.SubmitTask(in))
	case ToolTaskStatus:
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return "", err
		}
		if strings.TrimSpace(in.ID) == "" {
			return "", errors.New("id 必填")
		}
		return jsonOf(s.be.TaskStatus(in.ID))
	default:
		return "", errToolNotFound
	}
}

func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	return nil
}

func jsonOf(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	b, mErr := json.MarshalIndent(v, "", "  ")
	if mErr != nil {
		return "", fmt.Errorf("结果序列化失败: %w", mErr)
	}
	return string(b), nil
}

func marshalResponse(resp rpcResponse) []byte {
	b, err := json.Marshal(resp)
	if err != nil {
		// 兜底: 连响应都序列化不了时给一条合法的 JSON-RPC 错误, 不能返回空字节。
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
	}
	return b
}

// ---------------------------------------------------------------------------
// HTTP 传输
// ---------------------------------------------------------------------------

// Handler 返回单端点 JSON-RPC handler (POST)。
// 支持单条报文与批量数组两种 body 形态 (JSON-RPC 2.0 批处理)。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.ServeHTTP)
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONStatus(w, http.StatusMethodNotAllowed,
			json.RawMessage(`{"error":"POST only (JSON-RPC 2.0)"}`))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, json.RawMessage(`{"error":"read body failed"}`))
		return
	}
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSONStatus(w, http.StatusOK, marshalResponse(rpcResponse{JSONRPC: "2.0", ID: nil,
				Error: &rpcError{Code: -32700, Message: "Parse error", Data: err.Error()}}))
			return
		}
		out := make([]json.RawMessage, 0, len(batch))
		for _, one := range batch {
			if resp, ok := s.HandleRPC(one); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 { // 全是通知: 按 JSON-RPC 规范不回 body
			w.WriteHeader(http.StatusAccepted)
			return
		}
		b, _ := json.Marshal(out)
		writeJSONStatus(w, http.StatusOK, b)
		return
	}
	resp, ok := s.HandleRPC(body)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSONStatus(w, http.StatusOK, resp)
}

func writeJSONStatus(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// stdio 传输
// ---------------------------------------------------------------------------

// ServeStdio 按行读 JSON-RPC 报文、按行写响应 (Claude Desktop 等的默认形态)。
// 直到 in 读到 EOF 返回。注意: stdout 必须只有 JSON-RPC, 日志一律走 stderr。
func (s *Server) ServeStdio(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	// MCP 单条报文可以很长 (tools/list 的 schema、team 详情), 默认 64KB 不够。
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		resp, ok := s.HandleRPC([]byte(line))
		if !ok {
			continue
		}
		if _, err := out.Write(append(resp, '\n')); err != nil {
			return err
		}
	}
	return sc.Err()
}
