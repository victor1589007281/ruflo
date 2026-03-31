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

type autopilotState struct {
	Enabled   bool               `json:"enabled"`
	Config    map[string]any     `json:"config"`
	Log       []map[string]any   `json:"log"`
	Progress  map[string]float64 `json:"progress"`
	History   []string           `json:"history"`
	Learned   []map[string]any   `json:"learned"`
	UpdatedAt time.Time          `json:"updated_at"`
}

var (
	autopilotMu   sync.Mutex
	autopilotData = &autopilotState{
		Config:   map[string]any{"mode": "assist"},
		Progress: map[string]float64{},
	}
)

func autopilotPath() string {
	return filepath.Join(resolveDataDir(), "autopilot", "state.json")
}

func autopilotLoad() {
	autopilotMu.Lock()
	defer autopilotMu.Unlock()
	b, err := os.ReadFile(autopilotPath())
	if err != nil {
		return
	}
	var s autopilotState
	if json.Unmarshal(b, &s) == nil {
		if s.Config == nil {
			s.Config = map[string]any{}
		}
		if s.Progress == nil {
			s.Progress = map[string]float64{}
		}
		autopilotData = &s
	}
}

func autopilotSave() error {
	autopilotMu.Lock()
	autopilotData.UpdatedAt = now()
	cp := *autopilotData
	if cp.Config == nil {
		cp.Config = map[string]any{}
	}
	if cp.Progress == nil {
		cp.Progress = map[string]float64{}
	}
	cp.Log = append([]map[string]any(nil), autopilotData.Log...)
	cp.History = append([]string(nil), autopilotData.History...)
	cp.Learned = append([]map[string]any(nil), autopilotData.Learned...)
	autopilotMu.Unlock()
	return writeJSONFile(autopilotPath(), cp)
}

func autopilotTools() []*mcp.MCPTool {
	autopilotLoad()
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "autopilot_status", Description: "Autopilot enabled state and summary", InputSchema: obj, Handler: toolHandler(handleAutopilotStatus)},
		{Name: "autopilot_enable", Description: "Enable autopilot", InputSchema: obj, Handler: toolHandler(handleAutopilotEnable)},
		{Name: "autopilot_disable", Description: "Disable autopilot", InputSchema: obj, Handler: toolHandler(handleAutopilotDisable)},
		{Name: "autopilot_config", Description: "Get or merge autopilot config", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"patch": map[string]any{"type": "object"}}}, Handler: toolHandler(handleAutopilotConfig)},
		{Name: "autopilot_reset", Description: "Reset autopilot state", InputSchema: obj, Handler: toolHandler(handleAutopilotReset)},
		{Name: "autopilot_log", Description: "Append or read autopilot log", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}}}, Handler: toolHandler(handleAutopilotLog)},
		{Name: "autopilot_progress", Description: "Set or get progress key", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "number"}}}, Handler: toolHandler(handleAutopilotProgress)},
		{Name: "autopilot_learn", Description: "Record learned fact", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"fact": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAutopilotLearn)},
		{Name: "autopilot_history", Description: "List autopilot history strings", InputSchema: obj, Handler: toolHandler(handleAutopilotHistory)},
		{Name: "autopilot_predict", Description: "Lightweight next-step hint", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"context": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAutopilotPredict)},
	}
}

// RegisterAutopilotTools registers file-backed autopilot tools.
func RegisterAutopilotTools(reg *mcp.ToolRegistry) error {
	for _, t := range autopilotTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleAutopilotStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	en := autopilotData.Enabled
	nlog := len(autopilotData.Log)
	autopilotMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": en, "log_entries": nlog}}
}

func handleAutopilotEnable(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	autopilotData.Enabled = true
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": true}}
}

func handleAutopilotDisable(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	autopilotData.Enabled = false
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": false}}
}

func handleAutopilotConfig(_ context.Context, m map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	if patch, ok := m["patch"].(map[string]any); ok {
		for k, v := range patch {
			autopilotData.Config[k] = v
		}
	}
	cp := make(map[string]any, len(autopilotData.Config))
	for k, v := range autopilotData.Config {
		cp[k] = v
	}
	autopilotMu.Unlock()
	if _, ok := m["patch"].(map[string]any); ok {
		_ = autopilotSave()
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"config": cp}}
}

func handleAutopilotReset(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	autopilotData = &autopilotState{
		Enabled:  false,
		Config:   map[string]any{"mode": "assist"},
		Progress: map[string]float64{},
	}
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"reset": true}}
}

func handleAutopilotLog(_ context.Context, m map[string]any) mcp.MCPToolResult {
	msg := strArg(m, "message")
	autopilotMu.Lock()
	if msg != "" {
		autopilotData.Log = append(autopilotData.Log, map[string]any{"at": now(), "message": msg})
		if len(autopilotData.Log) > 500 {
			autopilotData.Log = autopilotData.Log[len(autopilotData.Log)-500:]
		}
		autopilotData.History = append(autopilotData.History, msg)
		if len(autopilotData.History) > 200 {
			autopilotData.History = autopilotData.History[len(autopilotData.History)-200:]
		}
	}
	limit := 50
	if v, ok := m["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}
	logs := append([]map[string]any(nil), autopilotData.Log...)
	autopilotMu.Unlock()
	if msg != "" {
		_ = autopilotSave()
	}
	if limit > 0 && len(logs) > limit {
		logs = logs[len(logs)-limit:]
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"log": logs}}
}

func handleAutopilotProgress(_ context.Context, m map[string]any) mcp.MCPToolResult {
	key := strArg(m, "key")
	autopilotMu.Lock()
	if v, ok := m["value"]; ok {
		var f float64
		switch t := v.(type) {
		case float64:
			f = t
		case int:
			f = float64(t)
		}
		if key != "" {
			autopilotData.Progress[key] = f
		}
	}
	if key != "" {
		v := autopilotData.Progress[key]
		needSave := false
		if _, ok := m["value"]; ok && key != "" {
			needSave = true
		}
		autopilotMu.Unlock()
		if needSave {
			_ = autopilotSave()
		}
		return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": key, "value": v}}
	}
	cp := make(map[string]float64, len(autopilotData.Progress))
	for k, v := range autopilotData.Progress {
		cp[k] = v
	}
	autopilotMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"progress": cp}}
}

func handleAutopilotLearn(_ context.Context, m map[string]any) mcp.MCPToolResult {
	fact := strArg(m, "fact")
	if fact == "" {
		return mcp.MCPToolResult{OK: false, Error: "fact required"}
	}
	autopilotMu.Lock()
	autopilotData.Learned = append(autopilotData.Learned, map[string]any{"at": now(), "fact": fact})
	if len(autopilotData.Learned) > 200 {
		autopilotData.Learned = autopilotData.Learned[len(autopilotData.Learned)-200:]
	}
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"stored": true}}
}

func handleAutopilotHistory(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	h := append([]string(nil), autopilotData.History...)
	autopilotMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"history": h}}
}

func handleAutopilotPredict(_ context.Context, m map[string]any) mcp.MCPToolResult {
	ctx := strArg(m, "context")
	hint := "continue_with_next_task"
	if ctx != "" {
		hint = "review_" + ctx
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"hint": hint, "confidence": 0.35}}
}
