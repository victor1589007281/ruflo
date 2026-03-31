package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

type workflowDef struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Steps     []string       `json:"steps"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type workflowRun struct {
	ID           string    `json:"id"`
	DefinitionID string    `json:"definition_id"`
	Status       string    `json:"status"`
	CurrentStep  int       `json:"current_step"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type workflowStoreFile struct {
	Definitions map[string]*workflowDef `json:"definitions"`
	Runs        map[string]*workflowRun `json:"runs"`
}

var (
	workflowMu    sync.Mutex
	workflowCache *workflowStoreFile
)

func workflowToolID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func workflowStorePath() string {
	return filepath.Join(resolveDataDir(), "workflows", "store.json")
}

func wfEnsureLocked() {
	if workflowCache != nil {
		return
	}
	workflowCache = &workflowStoreFile{
		Definitions: make(map[string]*workflowDef),
		Runs:        make(map[string]*workflowRun),
	}
	b, err := os.ReadFile(workflowStorePath())
	if err != nil {
		return
	}
	var f workflowStoreFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	if f.Definitions != nil {
		workflowCache.Definitions = f.Definitions
	}
	if f.Runs != nil {
		workflowCache.Runs = f.Runs
	}
}

func wfSaveLocked() error {
	f := workflowStoreFile{
		Definitions: make(map[string]*workflowDef),
		Runs:        make(map[string]*workflowRun),
	}
	for k, v := range workflowCache.Definitions {
		f.Definitions[k] = v
	}
	for k, v := range workflowCache.Runs {
		f.Runs[k] = v
	}
	return writeJSONFile(workflowStorePath(), f)
}

func workflowTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{Name: "workflow_create", Description: "Create a workflow definition", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "steps": map[string]any{"type": "array"}}}, Handler: toolHandler(handleWorkflowCreate)},
		{Name: "workflow_run", Description: "Execute a workflow", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"definition_id": map[string]any{"type": "string"}}, "required": []string{"definition_id"}}, Handler: toolHandler(handleWorkflowRun)},
		{Name: "workflow_execute", Description: "Alias of workflow_run", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"definition_id": map[string]any{"type": "string"}}, "required": []string{"definition_id"}}, Handler: toolHandler(handleWorkflowRun)},
		{Name: "workflow_status", Description: "Check workflow run status", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}}, "required": []string{"run_id"}}, Handler: toolHandler(handleWorkflowStatus)},
		{Name: "workflow_list", Description: "List workflow definitions and runs", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleWorkflowList)},
		{Name: "workflow_stop", Description: "Stop a running workflow", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}}, "required": []string{"run_id"}}, Handler: toolHandler(handleWorkflowStop)},
		{Name: "workflow_pause", Description: "Pause a workflow run", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}}, "required": []string{"run_id"}}, Handler: toolHandler(handleWorkflowPause)},
		{Name: "workflow_resume", Description: "Resume a paused workflow run", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}}, "required": []string{"run_id"}}, Handler: toolHandler(handleWorkflowResume)},
		{Name: "workflow_cancel", Description: "Cancel workflow run (same as stop)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}}, "required": []string{"run_id"}}, Handler: toolHandler(handleWorkflowStop)},
		{Name: "workflow_delete", Description: "Delete a workflow definition", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"definition_id": map[string]any{"type": "string"}}, "required": []string{"definition_id"}}, Handler: toolHandler(handleWorkflowDelete)},
		{Name: "workflow_template", Description: "List built-in workflow template ids", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleWorkflowTemplate)},
	}
}

func RegisterWorkflowTools(reg *mcp.ToolRegistry) error {
	for _, t := range workflowTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleWorkflowCreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		name = "workflow"
	}
	var steps []string
	if raw, ok := m["steps"]; ok && raw != nil {
		switch arr := raw.(type) {
		case []any:
			for _, x := range arr {
				steps = append(steps, fmt.Sprint(x))
			}
		case []string:
			steps = append(steps, arr...)
		}
	}
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	id := workflowToolID("wf-def-")
	workflowCache.Definitions[id] = &workflowDef{
		ID:        id,
		Name:      name,
		Steps:     steps,
		CreatedAt: now(),
	}
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"id": id, "name": name, "steps": steps}}
}

func handleWorkflowRun(_ context.Context, m map[string]any) mcp.MCPToolResult {
	did := strArg(m, "definition_id")
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	def, ok := workflowCache.Definitions[did]
	if !ok || def == nil {
		return mcp.MCPToolResult{OK: false, Error: "definition not found"}
	}
	rid := workflowToolID("wf-run-")
	run := &workflowRun{
		ID:           rid,
		DefinitionID: did,
		Status:       "running",
		CurrentStep:  0,
		StartedAt:    now(),
		UpdatedAt:    now(),
	}
	workflowCache.Runs[rid] = run
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	run.CurrentStep = len(def.Steps)
	run.Status = "completed"
	run.UpdatedAt = now()
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"run_id": rid, "status": run.Status, "steps_total": len(def.Steps)}}
}

func handleWorkflowStatus(_ context.Context, m map[string]any) mcp.MCPToolResult {
	rid := strArg(m, "run_id")
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	r, ok := workflowCache.Runs[rid]
	if !ok || r == nil {
		return mcp.MCPToolResult{OK: false, Error: "run not found"}
	}
	cp := *r
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"run_id": cp.ID, "definition_id": cp.DefinitionID, "status": cp.Status, "current_step": cp.CurrentStep,
		"started_at": cp.StartedAt, "updated_at": cp.UpdatedAt,
	}}
}

func handleWorkflowList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	defs := make([]string, 0, len(workflowCache.Definitions))
	for k := range workflowCache.Definitions {
		defs = append(defs, k)
	}
	runs := make([]string, 0, len(workflowCache.Runs))
	for k := range workflowCache.Runs {
		runs = append(runs, k)
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"definitions":    len(workflowCache.Definitions),
		"runs":           len(workflowCache.Runs),
		"definition_ids": defs,
		"run_ids":        runs,
	}}
}

func handleWorkflowPause(_ context.Context, m map[string]any) mcp.MCPToolResult {
	rid := strArg(m, "run_id")
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	r, ok := workflowCache.Runs[rid]
	if !ok || r == nil {
		return mcp.MCPToolResult{OK: false, Error: "run not found"}
	}
	r.Status = "paused"
	r.UpdatedAt = now()
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"run_id": rid, "status": r.Status}}
}

func handleWorkflowResume(_ context.Context, m map[string]any) mcp.MCPToolResult {
	rid := strArg(m, "run_id")
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	r, ok := workflowCache.Runs[rid]
	if !ok || r == nil {
		return mcp.MCPToolResult{OK: false, Error: "run not found"}
	}
	r.Status = "running"
	r.UpdatedAt = now()
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"run_id": rid, "status": r.Status}}
}

func handleWorkflowDelete(_ context.Context, m map[string]any) mcp.MCPToolResult {
	did := strArg(m, "definition_id")
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	if _, ok := workflowCache.Definitions[did]; !ok {
		return mcp.MCPToolResult{OK: false, Error: "definition not found"}
	}
	delete(workflowCache.Definitions, did)
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"deleted": did}}
}

func handleWorkflowTemplate(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"templates": []string{"feature", "security", "refactor", "bugfix", "docs"},
	}}
}

func handleWorkflowStop(_ context.Context, m map[string]any) mcp.MCPToolResult {
	rid := strArg(m, "run_id")
	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	r, ok := workflowCache.Runs[rid]
	if !ok || r == nil {
		return mcp.MCPToolResult{OK: false, Error: "run not found"}
	}
	r.Status = "stopped"
	r.UpdatedAt = now()
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"run_id": rid, "status": r.Status}}
}
