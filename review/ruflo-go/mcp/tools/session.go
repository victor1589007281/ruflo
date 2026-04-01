// session.go：session_* MCP 工具，管理 globalState.sessions 与会话 store.json 持久化。

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/mcp"
)

// sessionSeq 在 session_id 省略时用于生成 sess-{N}，重启后由 persist 恢复。
var sessionSeq int64

// sessionTools 注册 save/restore/list/delete/info 会话工具。
func sessionTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "session_save",
			Description: "Persist session snapshot",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
					"data":       map[string]any{"type": "object"},
				},
			},
			Handler: handleSessionSave,
		},
		{
			Name:        "session_restore",
			Description: "Restore session snapshot",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
				},
				"required": []string{"session_id"},
			},
			Handler: handleSessionRestore,
		},
		{
			Name:        "session_list",
			Description: "List stored sessions",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleSessionList,
		},
		{
			Name:        "session_delete",
			Description: "Delete a session",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"session_id": map[string]any{"type": "string"}},
				"required":   []string{"session_id"},
			},
			Handler: handleSessionDelete,
		},
		{
			Name:        "session_info",
			Description: "Summary of stored sessions",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleSessionInfo,
		},
	}
}

type sessionSaveArgs struct {
	SessionID string         `json:"session_id"`
	Data      map[string]any `json:"data"`
}

func handleSessionSave(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a sessionSaveArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	id := a.SessionID
	if id == "" {
		id = fmt.Sprintf("sess-%d", atomic.AddInt64(&sessionSeq, 1))
	}
	t := now()
	rec := &sessionRecord{
		ID:        id,
		Data:      a.Data,
		CreatedAt: t,
		UpdatedAt: t,
	}
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	globalState.mu.Lock()
	if old, ok := globalState.sessions[id]; ok {
		rec.CreatedAt = old.CreatedAt
	}
	globalState.sessions[id] = rec
	globalState.mu.Unlock()
	saveSessionsToDisk()
	return jsonOK(map[string]any{"ok": true, "session": rec})
}

type sessionIDArg struct {
	SessionID string `json:"session_id"`
}

func handleSessionRestore(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a sessionIDArg
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	globalState.mu.RLock()
	rec, ok := globalState.sessions[a.SessionID]
	globalState.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	return jsonOK(map[string]any{"session": rec})
}

func handleSessionList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	ids := make([]string, 0, len(globalState.sessions))
	for id := range globalState.sessions {
		ids = append(ids, id)
	}
	globalState.mu.RUnlock()
	sort.Strings(ids)
	return jsonOK(map[string]any{"sessions": ids, "count": len(ids)})
}

func handleSessionInfo(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var oldest, newest string
	var ot, nt int64
	first := true
	for id, rec := range globalState.sessions {
		if rec == nil {
			continue
		}
		u := rec.UpdatedAt.UnixNano()
		if first {
			oldest, newest, ot, nt = id, id, u, u
			first = false
			continue
		}
		if u < ot {
			oldest, ot = id, u
		}
		if u > nt {
			newest, nt = id, u
		}
	}
	return jsonOK(map[string]any{
		"count": len(globalState.sessions), "oldest_id": oldest, "newest_id": newest,
	})
}

func handleSessionDelete(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a sessionIDArg
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	globalState.mu.Lock()
	_, ok := globalState.sessions[a.SessionID]
	if ok {
		delete(globalState.sessions, a.SessionID)
	}
	globalState.mu.Unlock()
	if ok {
		saveSessionsToDisk()
	}
	return jsonOK(map[string]any{"ok": ok, "deleted": ok})
}
