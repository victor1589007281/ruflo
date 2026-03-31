package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

type daaStateFile struct {
	Agents    []map[string]any `json:"agents"`
	Workflows []map[string]any `json:"workflows"`
	Knowledge []map[string]any `json:"knowledge"`
	Metrics   map[string]any   `json:"metrics"`
	UpdatedAt time.Time        `json:"updated_at"`
}

var (
	daaMu   sync.Mutex
	daaData = &daaStateFile{Metrics: map[string]any{}}
)

func daaPath() string {
	return filepath.Join(resolveDataDir(), "daa", "state.json")
}

func daaLoad() {
	daaMu.Lock()
	defer daaMu.Unlock()
	b, err := os.ReadFile(daaPath())
	if err != nil {
		return
	}
	var s daaStateFile
	if json.Unmarshal(b, &s) == nil {
		if s.Metrics == nil {
			s.Metrics = map[string]any{}
		}
		daaData = &s
	}
}

func daaSave() error {
	daaMu.Lock()
	daaData.UpdatedAt = now()
	cp := daaStateFile{
		Agents:    append([]map[string]any(nil), daaData.Agents...),
		Workflows: append([]map[string]any(nil), daaData.Workflows...),
		Knowledge: append([]map[string]any(nil), daaData.Knowledge...),
		Metrics:   map[string]any{},
		UpdatedAt: daaData.UpdatedAt,
	}
	for k, v := range daaData.Metrics {
		cp.Metrics[k] = v
	}
	daaMu.Unlock()
	return writeJSONFile(daaPath(), cp)
}

func daaTools() []*mcp.MCPTool {
	daaLoad()
	return []*mcp.MCPTool{
		{Name: "daa_agent_create", Description: "Register DAA agent metadata", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "role": map[string]any{"type": "string"}}}, Handler: toolHandler(handleDAAAgentCreate)},
		{Name: "daa_agent_adapt", Description: "Record adaptation event", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string"}, "delta": map[string]any{"type": "string"}}}, Handler: toolHandler(handleDAAAgentAdapt)},
		{Name: "daa_workflow_create", Description: "Create DAA workflow record", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}, Handler: toolHandler(handleDAAWorkflowCreate)},
		{Name: "daa_workflow_execute", Description: "Mark workflow execution", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}, Handler: toolHandler(handleDAAWorkflowExecute)},
		{Name: "daa_knowledge_share", Description: "Share knowledge fragment", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}, Handler: toolHandler(handleDAAKnowledgeShare)},
		{Name: "daa_learning_status", Description: "DAA learning counters", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleDAALearningStatus)},
		{Name: "daa_cognitive_pattern", Description: "Store cognitive pattern tag", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}}, Handler: toolHandler(handleDAACognitivePattern)},
		{Name: "daa_performance_metrics", Description: "Update or read DAA metrics", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "number"}}}, Handler: toolHandler(handleDAAPerformanceMetrics)},
	}
}

// RegisterDAATools registers Dynamic Agent Architecture file-backed tools.
func RegisterDAATools(reg *mcp.ToolRegistry) error {
	for _, t := range daaTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleDAAAgentCreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		name = "agent"
	}
	rec := map[string]any{"name": name, "role": strArg(m, "role"), "at": now()}
	daaMu.Lock()
	daaData.Agents = append(daaData.Agents, rec)
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agent": rec}}
}

func handleDAAAgentAdapt(_ context.Context, m map[string]any) mcp.MCPToolResult {
	daaMu.Lock()
	daaData.Metrics["last_adapt_agent"] = strArg(m, "agent")
	daaData.Metrics["last_adapt_delta"] = strArg(m, "delta")
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"adapted": true}}
}

func handleDAAWorkflowCreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		name = "workflow"
	}
	id := fmt.Sprintf("daa-wf-%d", time.Now().UnixNano())
	rec := map[string]any{"id": id, "name": name, "status": "created", "at": now()}
	daaMu.Lock()
	daaData.Workflows = append(daaData.Workflows, rec)
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"workflow": rec}}
}

func handleDAAWorkflowExecute(_ context.Context, m map[string]any) mcp.MCPToolResult {
	id := strArg(m, "id")
	daaMu.Lock()
	for _, w := range daaData.Workflows {
		if w["id"] == id {
			w["status"] = "executed"
			w["executed_at"] = now()
		}
	}
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"id": id, "executed": true}}
}

func handleDAAKnowledgeShare(_ context.Context, m map[string]any) mcp.MCPToolResult {
	rec := map[string]any{"topic": strArg(m, "topic"), "content": strArg(m, "content"), "at": now()}
	daaMu.Lock()
	daaData.Knowledge = append(daaData.Knowledge, rec)
	if len(daaData.Knowledge) > 500 {
		daaData.Knowledge = daaData.Knowledge[len(daaData.Knowledge)-500:]
	}
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"shared": true}}
}

func handleDAALearningStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	daaMu.Lock()
	nA, nW, nK := len(daaData.Agents), len(daaData.Workflows), len(daaData.Knowledge)
	daaMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agents": nA, "workflows": nW, "knowledge": nK}}
}

func handleDAACognitivePattern(_ context.Context, m map[string]any) mcp.MCPToolResult {
	p := strArg(m, "pattern")
	if p == "" {
		return mcp.MCPToolResult{OK: false, Error: "pattern required"}
	}
	daaMu.Lock()
	if daaData.Metrics == nil {
		daaData.Metrics = map[string]any{}
	}
	daaData.Metrics["last_cognitive_pattern"] = p
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"pattern": p}}
}

func handleDAAPerformanceMetrics(_ context.Context, m map[string]any) mcp.MCPToolResult {
	key := strArg(m, "key")
	daaMu.Lock()
	if key != "" {
		if daaData.Metrics == nil {
			daaData.Metrics = map[string]any{}
		}
		if v, ok := m["value"]; ok {
			daaData.Metrics[key] = v
		}
	}
	cp := map[string]any{}
	for k, v := range daaData.Metrics {
		cp[k] = v
	}
	daaMu.Unlock()
	if key != "" {
		_ = daaSave()
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"metrics": cp}}
}
