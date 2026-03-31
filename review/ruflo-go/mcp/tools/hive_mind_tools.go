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

type hiveMindState struct {
	QueenID   string            `json:"queen_id"`
	Workers   []string          `json:"workers"`
	Messages  []map[string]any  `json:"messages"`
	Consensus []map[string]any  `json:"consensus"`
	Memory    map[string]string `json:"memory,omitempty"`
	Active    bool              `json:"active"`
	UpdatedAt time.Time         `json:"updated_at"`
}

var (
	hiveMu   sync.Mutex
	hiveData = &hiveMindState{}
)

func hiveStatePath() string {
	return filepath.Join(resolveDataDir(), "hive-mind", "state.json")
}

func hiveLoad() {
	hiveMu.Lock()
	defer hiveMu.Unlock()
	b, err := os.ReadFile(hiveStatePath())
	if err != nil {
		return
	}
	var s hiveMindState
	if json.Unmarshal(b, &s) == nil {
		hiveData = &s
	}
}

func hiveSave() error {
	hiveMu.Lock()
	hiveData.UpdatedAt = now()
	cp := *hiveData
	hiveMu.Unlock()
	return writeJSONFile(hiveStatePath(), cp)
}

func hiveTools() []*mcp.MCPTool {
	hiveLoad()
	return []*mcp.MCPTool{
		{Name: "hive-mind_init", Description: "Initialize hive mind with queen", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"queen_id": map[string]any{"type": "string"}}, "required": []string{"queen_id"}}, Handler: toolHandler(handleHiveMindInit)},
		{Name: "hive-mind_spawn", Description: "Spawn hive worker", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker_id": map[string]any{"type": "string"}}, "required": []string{"worker_id"}}, Handler: toolHandler(handleHiveMindSpawn)},
		{Name: "hive-mind_broadcast", Description: "Broadcast message to all workers", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}, "required": []string{"message"}}, Handler: toolHandler(handleHiveMindBroadcast)},
		{Name: "hive-mind_status", Description: "Get hive mind status", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleHiveMindStatus)},
		{Name: "hive-mind_consensus", Description: "Trigger consensus vote", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHiveMindConsensus)},
		{Name: "hive-mind_join", Description: "Join hive as worker", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker_id": map[string]any{"type": "string"}}, "required": []string{"worker_id"}}, Handler: toolHandler(handleHiveMindJoin)},
		{Name: "hive-mind_leave", Description: "Leave hive worker role", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker_id": map[string]any{"type": "string"}}, "required": []string{"worker_id"}}, Handler: toolHandler(handleHiveMindLeave)},
		{Name: "hive-mind_memory", Description: "Store or read hive shared memory key", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHiveMindMemory)},
		{Name: "hive-mind_shutdown", Description: "Shutdown hive mind", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleHiveMindShutdown)},
	}
}

func RegisterHiveMindTools(reg *mcp.ToolRegistry) error {
	for _, t := range hiveTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleHiveMindInit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	q := strArg(m, "queen_id")
	if q == "" {
		return mcp.MCPToolResult{OK: false, Error: "queen_id required"}
	}
	hiveMu.Lock()
	hiveData.QueenID = q
	hiveData.Workers = nil
	hiveData.Messages = nil
	hiveData.Consensus = nil
	hiveData.Active = true
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"queen_id": q, "active": true}}
}

func handleHiveMindSpawn(_ context.Context, m map[string]any) mcp.MCPToolResult {
	w := strArg(m, "worker_id")
	if w == "" {
		return mcp.MCPToolResult{OK: false, Error: "worker_id required"}
	}
	hiveMu.Lock()
	found := false
	for _, x := range hiveData.Workers {
		if x == w {
			found = true
			break
		}
	}
	if !found {
		hiveData.Workers = append(hiveData.Workers, w)
	}
	n := len(hiveData.Workers)
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"worker_id": w, "workers": n}}
}

func handleHiveMindBroadcast(_ context.Context, m map[string]any) mcp.MCPToolResult {
	msg := strArg(m, "message")
	if msg == "" {
		return mcp.MCPToolResult{OK: false, Error: "message required"}
	}
	hiveMu.Lock()
	if hiveData.Messages == nil {
		hiveData.Messages = make([]map[string]any, 0)
	}
	hiveData.Messages = append(hiveData.Messages, map[string]any{"text": msg, "at": now()})
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"broadcast": true, "message": msg}}
}

func handleHiveMindStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	hiveMu.Lock()
	cp := *hiveData
	w := append([]string(nil), cp.Workers...)
	hiveMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"queen_id": cp.QueenID, "workers": w, "worker_count": len(w),
		"active": cp.Active, "messages": len(cp.Messages), "consensus_events": len(cp.Consensus),
	}}
}

func handleHiveMindConsensus(_ context.Context, m map[string]any) mcp.MCPToolResult {
	topic := strArg(m, "topic")
	if topic == "" {
		topic = "default"
	}
	hiveMu.Lock()
	if hiveData.Consensus == nil {
		hiveData.Consensus = make([]map[string]any, 0)
	}
	rec := map[string]any{"topic": topic, "votes": len(hiveData.Workers) + 1, "at": now()}
	hiveData.Consensus = append(hiveData.Consensus, rec)
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: rec}
}

func handleHiveMindJoin(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	return handleHiveMindSpawn(ctx, m)
}

func handleHiveMindLeave(_ context.Context, m map[string]any) mcp.MCPToolResult {
	w := strArg(m, "worker_id")
	hiveMu.Lock()
	out := hiveData.Workers[:0]
	for _, x := range hiveData.Workers {
		if x != w {
			out = append(out, x)
		}
	}
	hiveData.Workers = out
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"worker_id": w, "remaining": len(out)}}
}

func handleHiveMindMemory(_ context.Context, m map[string]any) mcp.MCPToolResult {
	k := strArg(m, "key")
	if k == "" {
		return mcp.MCPToolResult{OK: false, Error: "key required"}
	}
	hiveMu.Lock()
	if hiveData.Memory == nil {
		hiveData.Memory = map[string]string{}
	}
	wrote := false
	if _, ok := m["value"]; ok {
		hiveData.Memory[k] = strArg(m, "value")
		wrote = true
	}
	val := hiveData.Memory[k]
	hiveMu.Unlock()
	if wrote {
		if err := hiveSave(); err != nil {
			return mcp.MCPToolResult{OK: false, Error: err.Error()}
		}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": k, "value": val}}
}

func handleHiveMindShutdown(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	hiveMu.Lock()
	hiveData.Active = false
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"active": false}}
}
