package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// workflowToolID 使用 crypto/rand 生成 prefix+16 位十六进制随机后缀，作为定义或运行 ID。
func workflowToolID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// workflowStorePath 返回 workflows/store.json 路径。
func workflowStorePath() string {
	return filepath.Join(resolveDataDir(), "workflows", "store.json")
}

// wfEnsureLocked 在已持有 workflowMu 的前提下懒加载缓存：若 nil 则初始化并从磁盘合并 Definitions/Runs。
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

// wfSaveLocked 在已持有锁时将 workflowCache 深拷贝写入 workflowStorePath。
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

// workflowTools 注册创建、运行、状态、列表、停止/取消、暂停、恢复、删除与模板列举等工具。
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

// RegisterWorkflowTools 注册工作流相关 MCP 工具。
func RegisterWorkflowTools(reg *mcp.ToolRegistry) error {
	for _, t := range workflowTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleWorkflowCreate 解析 name（默认 workflow）与 steps 数组，生成新定义 ID 并落盘。
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

// handleWorkflowRun 按 definition_id 创建 run，逐步将 step 视为 MCP 工具名经 globalToolRegistry 调用并记录结果。
func handleWorkflowRun(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	did := strArg(m, "definition_id")
	workflowMu.Lock()
	wfEnsureLocked()
	def, ok := workflowCache.Definitions[did]
	if !ok || def == nil {
		workflowMu.Unlock()
		return mcp.MCPToolResult{OK: false, Error: "definition not found"}
	}
	steps := append([]string(nil), def.Steps...)
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
		workflowMu.Unlock()
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	workflowMu.Unlock()

	reg := globalToolRegistry
	stepResults := make([]map[string]any, 0, len(steps))
	failed := false
	var failReason string
	for i, step := range steps {
		step = strings.TrimSpace(step)
		t0 := time.Now()
		sr := map[string]any{"index": i, "step": step, "elapsed_sec": 0.0}
		if step == "" {
			sr["ok"] = true
			sr["skipped"] = true
			sr["elapsed_sec"] = time.Since(t0).Seconds()
			stepResults = append(stepResults, sr)
			continue
		}
		if reg == nil {
			failed = true
			failReason = "tool registry not initialized"
			sr["ok"] = false
			sr["error"] = failReason
			sr["elapsed_sec"] = time.Since(t0).Seconds()
			stepResults = append(stepResults, sr)
			break
		}
		raw, err := reg.Call(ctx, step, json.RawMessage("{}"))
		elapsed := time.Since(t0).Seconds()
		sr["elapsed_sec"] = elapsed
		if err != nil {
			failed = true
			failReason = err.Error()
			sr["ok"] = false
			sr["error"] = err.Error()
			stepResults = append(stepResults, sr)
			break
		}
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if okv, has := payload["ok"]; has {
			if ob, okb := okv.(bool); okb && !ob {
				failed = true
				if es, _ := payload["error"].(string); es != "" {
					failReason = es
					sr["error"] = es
				} else {
					failReason = "tool returned ok=false"
					sr["error"] = failReason
				}
				sr["ok"] = false
				stepResults = append(stepResults, sr)
				break
			}
		}
		sr["ok"] = true
		if len(raw) > 800 {
			sr["result_preview"] = string(raw[:800]) + "…"
		} else {
			sr["result_preview"] = string(raw)
		}
		stepResults = append(stepResults, sr)
	}

	workflowMu.Lock()
	defer workflowMu.Unlock()
	wfEnsureLocked()
	r := workflowCache.Runs[rid]
	if r != nil {
		if failed {
			r.Status = "failed"
			for j := range stepResults {
				if stepResults[j]["ok"] == false {
					if idx, ok := stepResults[j]["index"].(int); ok {
						r.CurrentStep = idx
					}
					break
				}
			}
		} else {
			r.Status = "completed"
			r.CurrentStep = len(steps)
		}
		r.UpdatedAt = now()
	}
	if err := wfSaveLocked(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	out := map[string]any{
		"run_id": rid, "steps_total": len(steps), "steps": stepResults,
	}
	if r != nil {
		out["status"] = r.Status
		out["current_step"] = r.CurrentStep
	}
	if failed {
		out["failed"] = true
		if failReason != "" {
			out["error"] = failReason
		}
		return mcp.MCPToolResult{OK: false, Data: out, Error: failReason}
	}
	return mcp.MCPToolResult{OK: true, Data: out}
}

// handleWorkflowStatus 按 run_id 返回运行状态、当前步骤与时间戳字段。
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

// handleWorkflowList 返回定义数、运行数及全部 definition_ids、run_ids 列表。
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

// handleWorkflowPause 将指定 run 标记为 paused。
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

// handleWorkflowResume 将指定 run 标记为 running。
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

// handleWorkflowDelete 按 definition_id 删除定义（若不存在则报错）。
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

// handleWorkflowTemplate 返回内置模板 id 列表（feature/security/refactor/bugfix/docs）。
func handleWorkflowTemplate(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"templates": []string{"feature", "security", "refactor", "bugfix", "docs"},
	}}
}

// handleWorkflowStop 将 run 状态设为 stopped（workflow_cancel 与此共用）。
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
