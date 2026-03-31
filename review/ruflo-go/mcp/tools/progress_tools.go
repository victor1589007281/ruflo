package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

type progressState struct {
	Watchers []string          `json:"watchers"`
	Summary  map[string]any    `json:"summary"`
	SyncedAt time.Time         `json:"synced_at"`
	Tasks    map[string]string `json:"tasks"`
}

var (
	progressMu   sync.Mutex
	progressData = &progressState{Summary: map[string]any{}, Tasks: map[string]string{}}
)

func progressPath() string {
	return filepath.Join(resolveDataDir(), "progress", "state.json")
}

func progressLoad() {
	progressMu.Lock()
	defer progressMu.Unlock()
	b, err := os.ReadFile(progressPath())
	if err != nil {
		return
	}
	var s progressState
	if json.Unmarshal(b, &s) == nil {
		if s.Summary == nil {
			s.Summary = map[string]any{}
		}
		if s.Tasks == nil {
			s.Tasks = map[string]string{}
		}
		progressData = &s
	}
}

func progressSave() error {
	progressMu.Lock()
	progressData.SyncedAt = now()
	cp := progressState{
		Watchers: append([]string(nil), progressData.Watchers...),
		Summary:  map[string]any{},
		SyncedAt: progressData.SyncedAt,
		Tasks:    map[string]string{},
	}
	for k, v := range progressData.Summary {
		cp.Summary[k] = v
	}
	for k, v := range progressData.Tasks {
		cp.Tasks[k] = v
	}
	progressMu.Unlock()
	return writeJSONFile(progressPath(), cp)
}

func progressTools() []*mcp.MCPTool {
	progressLoad()
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "progress_check", Description: "Check progress snapshot", InputSchema: obj, Handler: toolHandler(handleProgressCheck)},
		{Name: "progress_sync", Description: "Sync progress state to disk", InputSchema: obj, Handler: toolHandler(handleProgressSync)},
		{Name: "progress_summary", Description: "Merge summary fields", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}}}, Handler: toolHandler(handleProgressSummary)},
		{Name: "progress_watch", Description: "Register or list watch targets", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}}}, Handler: toolHandler(handleProgressWatch)},
	}
}

// RegisterProgressTools registers file-backed progress helpers.
func RegisterProgressTools(reg *mcp.ToolRegistry) error {
	for _, t := range progressTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleProgressCheck(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	progressMu.Lock()
	cp := map[string]any{}
	for k, v := range progressData.Summary {
		cp[k] = v
	}
	n := len(progressData.Watchers)
	progressMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"summary": cp, "watchers": n}}
}

func handleProgressSync(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	if err := progressSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"synced": true}}
}

func handleProgressSummary(_ context.Context, m map[string]any) mcp.MCPToolResult {
	k := strArg(m, "key")
	v := strArg(m, "value")
	progressMu.Lock()
	if progressData.Summary == nil {
		progressData.Summary = map[string]any{}
	}
	if k != "" {
		progressData.Summary[k] = v
	}
	cp := map[string]any{}
	for kk, vv := range progressData.Summary {
		cp[kk] = vv
	}
	progressMu.Unlock()
	_ = progressSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"summary": cp}}
}

func handleProgressWatch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	t := strArg(m, "target")
	progressMu.Lock()
	if t != "" {
		progressData.Watchers = append(progressData.Watchers, t)
		if len(progressData.Watchers) > 200 {
			progressData.Watchers = progressData.Watchers[len(progressData.Watchers)-200:]
		}
	}
	w := append([]string(nil), progressData.Watchers...)
	progressMu.Unlock()
	if t != "" {
		_ = progressSave()
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"watchers": w}}
}
