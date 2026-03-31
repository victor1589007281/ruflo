package tools

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

func termStateDir() string {
	return filepath.Join(resolveDataDir(), "terminal")
}

func newTermID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "term-" + hex.EncodeToString(b[:])
}

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

// RegisterTerminalTools registers local command execution tools.
func RegisterTerminalTools(reg *mcp.ToolRegistry) error {
	for _, t := range terminalTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

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

func handleTerminalList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	termMu.Lock()
	ids := make([]string, 0, len(termSessions))
	for id := range termSessions {
		ids = append(ids, id)
	}
	termMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"sessions": ids, "count": len(ids)}}
}

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
