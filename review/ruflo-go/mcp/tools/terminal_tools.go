package tools

// 本文件实现「终端会话」MCP 工具（terminal_*）：在内存中跟踪工作目录与命令历史，通过 sh -c 执行命令。
//
// 设计思路：
//   - 会话仅驻留进程生命周期，重启后丢失；session_id 为随机 hex，避免与真实 shell PTY 绑定以降低实现复杂度。
//   - 执行使用 CombinedOutput，在 history 中记录命令与错误摘要；适合受控环境下的短命令，非交互式长运行进程。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

// termSession 表示一个逻辑终端会话：ID、工作目录、创建时间与命令历史行。
type termSession struct {
	ID        string    `json:"id"`
	CWD       string    `json:"cwd"`
	CreatedAt time.Time `json:"created_at"`
	History   []string  `json:"history"`
}

var (
	termMu       sync.Mutex
	termSessions = make(map[string]*termSession)
)

// termStateDir 返回终端状态目录（当前仅用于 MkdirAll，会话不落盘）。
func termStateDir() string {
	return filepath.Join(resolveDataDir(), "terminal")
}

// newTermID 生成带 term- 前缀的随机会话 ID。
func newTermID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "term-" + hex.EncodeToString(b[:])
}

// terminalTools 构造 terminal_* MCP 工具定义。
func terminalTools() []*mcp.MCPTool {
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "terminal_create", Description: "Create a tracked shell session", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string"}}}, Handler: toolHandler(handleTerminalCreate)},
		{Name: "terminal_execute", Description: "Run command in session (sh -c)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]any{"type": "string"}, "command": map[string]any{"type": "string"}}, "required": []string{"session_id", "command"}}, Handler: toolHandler(handleTerminalExecute)},
		{Name: "terminal_list", Description: "List terminal sessions", InputSchema: obj, Handler: toolHandler(handleTerminalList)},
		{Name: "terminal_close", Description: "Close a session", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]any{"type": "string"}}, "required": []string{"session_id"}}, Handler: toolHandler(handleTerminalClose)},
		{Name: "terminal_history", Description: "Get session command history", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]any{"type": "string"}}, "required": []string{"session_id"}}, Handler: toolHandler(handleTerminalHistory)},
	}
}

// RegisterTerminalTools 向注册表登记本地命令执行类 MCP 工具。
func RegisterTerminalTools(reg *mcp.ToolRegistry) error {
	for _, t := range terminalTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleTerminalCreate 处理 terminal_create：cwd 默认当前工作目录；注册新会话并返回 session_id。
func handleTerminalCreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	cwd := strArg(m, "cwd")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	id := newTermID()
	s := &termSession{ID: id, CWD: cwd, CreatedAt: now(), History: nil}
	termMu.Lock()
	termSessions[id] = s
	termMu.Unlock()
	_ = os.MkdirAll(termStateDir(), 0o755)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"session_id": id, "cwd": cwd}}
}

// handleTerminalExecute 处理 terminal_execute：session_id 与 command 必填；在会话 CWD 下执行 sh -c，返回 stdout 合并输出与 exit_code。
func handleTerminalExecute(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	sid := strArg(m, "session_id")
	cmdStr := strArg(m, "command")
	if sid == "" || cmdStr == "" {
		return mcp.MCPToolResult{OK: false, Error: "session_id and command required"}
	}
	termMu.Lock()
	s, ok := termSessions[sid]
	if !ok {
		termMu.Unlock()
		return mcp.MCPToolResult{OK: false, Error: "session not found"}
	}
	wd := s.CWD
	termMu.Unlock()

	c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	c.Dir = wd
	out, err := c.CombinedOutput()
	line := fmt.Sprintf("%s -> %v", cmdStr, err)
	termMu.Lock()
	if s2, ok2 := termSessions[sid]; ok2 {
		s2.History = append(s2.History, line)
		if len(s2.History) > 200 {
			s2.History = s2.History[len(s2.History)-200:]
		}
	}
	termMu.Unlock()
	exit := 0
	errMsg := ""
	if err != nil {
		exit = 1
		if x, ok := err.(*exec.ExitError); ok {
			exit = x.ExitCode()
		}
		errMsg = err.Error()
	}
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{
		"session_id": sid, "stdout": string(out), "exit_code": exit, "error": errMsg,
	}}
}

// handleTerminalList 处理 terminal_list：返回全部 session_id 与数量。
func handleTerminalList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	termMu.Lock()
	ids := make([]string, 0, len(termSessions))
	for id := range termSessions {
		ids = append(ids, id)
	}
	termMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"sessions": ids, "count": len(ids)}}
}

// handleTerminalClose 处理 terminal_close：按 session_id 删除会话；closed 表示是否曾存在。
func handleTerminalClose(_ context.Context, m map[string]any) mcp.MCPToolResult {
	sid := strArg(m, "session_id")
	termMu.Lock()
	_, ok := termSessions[sid]
	if ok {
		delete(termSessions, sid)
	}
	termMu.Unlock()
	return mcp.MCPToolResult{OK: ok, Data: map[string]any{"session_id": sid, "closed": ok}}
}

// handleTerminalHistory 处理 terminal_history：返回指定会话的命令历史副本。
func handleTerminalHistory(_ context.Context, m map[string]any) mcp.MCPToolResult {
	sid := strArg(m, "session_id")
	termMu.Lock()
	s, ok := termSessions[sid]
	var hist []string
	if ok {
		hist = append([]string(nil), s.History...)
	}
	termMu.Unlock()
	if !ok {
		return mcp.MCPToolResult{OK: false, Error: "session not found"}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"session_id": sid, "history": hist}}
}
