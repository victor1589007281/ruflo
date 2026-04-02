package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/hooks"
)

// 本文件：任务管理 MCP 工具（创建、查询、列表、完成、分配、取消、更新）。
//
// 设计思路：在进程内 globalState.tasks 中维护任务定义，与编排运行时共享状态；任务 ID 由原子递增
// taskSeq 生成（task-<n>），创建/变更后调用 saveTasksToDisk 持久化。各工具通过 JSON Schema 描述入参，
// 处理器统一解析 json.RawMessage，读写全局状态并返回 jsonOK 封装结果。

// taskSeq 为内存中任务 ID 的原子递增序号源，与 fmt.Sprintf("task-%d", ...) 组合生成唯一 ID。
var taskSeq int64

// taskTools 注册任务相关 MCP 工具：创建、查询、列表、完成、分配、取消、更新；各工具的 InputSchema 描述 JSON 入参字段。
func taskTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "task_create",
			Description: "Create a task",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":         map[string]any{"type": "string"},
					"title":        map[string]any{"type": "string"},
					"description":  map[string]any{"type": "string"},
					"priority":     map[string]any{"type": "number"},
					"depends_on":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"title"},
			},
			Handler: handleTaskCreate,
		},
		{
			Name:        "task_status",
			Description: "Get task by id",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []string{"id"},
			},
			Handler: handleTaskStatus,
		},
		{
			Name:        "task_list",
			Description: "List tasks",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleTaskList,
		},
		{
			Name:        "task_complete",
			Description: "Mark task succeeded",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []string{"id"},
			},
			Handler: handleTaskComplete,
		},
		{
			Name:        "task_assign",
			Description: "Assign task to agent",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "string"},
					"agent_id": map[string]any{"type": "string"},
				},
				"required": []string{"id", "agent_id"},
			},
			Handler: handleTaskAssign,
		},
		{
			Name:        "task_cancel",
			Description: "Cancel a task",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []string{"id"},
			},
			Handler: handleTaskCancel,
		},
		{
			Name:        "task_update",
			Description: "Update task title, description, priority, or status",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          map[string]any{"type": "string"},
					"title":       map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"priority":    map[string]any{"type": "number"},
					"status":      map[string]any{"type": "string"},
					"progress":    map[string]any{"type": "number", "description": "0-100"},
				},
				"required": []string{"id"},
			},
			Handler: handleTaskUpdate,
		},
	}
}

// handleTaskCreate 解析 type/title/description/priority，校验 title 非空；默认类型为实现类任务、默认优先级 Normal；
// 生成新任务并置为 Pending，写入 globalState 后落盘，返回 ok 与完整 TaskDefinition。
func handleTaskCreate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Priority    int      `json:"priority"`
		DependsOn   []string `json:"depends_on"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, fmt.Errorf("title required")
	}
	tt := api.TaskTypeImplementation
	if in.Type != "" {
		tt = api.TaskType(in.Type)
	}
	pr := api.TaskPriorityNormal
	if in.Priority > 0 {
		pr = api.TaskPriority(in.Priority)
	}
	id := fmt.Sprintf("task-%d", atomic.AddInt64(&taskSeq, 1))
	t := now()
	deps := append([]string(nil), in.DependsOn...)
	td := &api.TaskDefinition{
		ID:          id,
		Type:        tt,
		Status:      api.TaskStatusPending,
		Priority:    pr,
		Title:       in.Title,
		Description: in.Description,
		DependsOn:   deps,
		Progress:    0,
		CreatedAt:   t,
		UpdatedAt:   t,
	}
	globalState.mu.Lock()
	globalState.tasks[id] = td
	globalState.mu.Unlock()
	saveTasksToDisk()

	// 触发 PreTask 钩子：分析任务、推荐 Agent、评估复杂度
	preTaskHC := hooks.HookContext{
		Task:    td,
		Command: in.Title + " " + in.Description,
		Args:    map[string]any{"description": in.Title + " " + in.Description},
	}
	preResult := globalState.hookExec.Execute(hooks.HookEventPreTask, preTaskHC)

	return jsonOK(map[string]any{"ok": true, "task": td, "analysis": preResult.Data})
}

// handleTaskStatus 根据 id 在 globalState.tasks 中只读查找，存在则返回 task 对象，否则报错 task not found。
func handleTaskStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		ID string `json:"id"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	globalState.mu.RLock()
	td, ok := globalState.tasks[in.ID]
	globalState.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("task not found")
	}
	return jsonOK(map[string]any{"task": td})
}

func handleTaskList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	list := make([]*api.TaskDefinition, 0, len(globalState.tasks))
	for _, t := range globalState.tasks {
		list = append(list, t)
	}
	globalState.mu.RUnlock()
	return jsonOK(map[string]any{"tasks": list, "count": len(list)})
}

func handleTaskComplete(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		ID string `json:"id"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	globalState.mu.Lock()
	td, ok := globalState.tasks[in.ID]
	if !ok {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("task not found")
	}
	td.Status = api.TaskStatusSucceeded
	td.UpdatedAt = now()
	if td.AgentID != "" {
		if ag, ok := globalState.agents[td.AgentID]; ok && ag != nil {
			ag.State = api.AgentStateIdle
			ag.UpdatedAt = now()
		}
	}
	globalState.mu.Unlock()
	saveTasksToDisk()
	saveAgentsToDisk()

	// 触发 PostTask 钩子：学习记录
	postHC := hooks.HookContext{
		Task: td,
		Args: map[string]any{"success": true, "task_id": in.ID},
	}
	postResult := globalState.hookExec.Execute(hooks.HookEventPostTask, postHC)

	return jsonOK(map[string]any{"ok": true, "task": td, "learning": postResult.Data})
}

// handleTaskAssign 为任务设置 AgentID，状态改为 Queued，更新 UpdatedAt 并持久化。
func handleTaskAssign(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		ID      string `json:"id"`
		AgentID string `json:"agent_id"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	globalState.mu.Lock()
	td, ok := globalState.tasks[in.ID]
	if !ok {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("task not found")
	}
	var pendingDeps []string
	var missingDeps []string
	for _, depID := range td.DependsOn {
		if depID == "" {
			continue
		}
		dep, depOk := globalState.tasks[depID]
		if !depOk {
			missingDeps = append(missingDeps, depID)
			continue
		}
		if dep.Status != api.TaskStatusSucceeded {
			pendingDeps = append(pendingDeps, depID)
		}
	}
	if len(missingDeps) > 0 || len(pendingDeps) > 0 {
		globalState.mu.Unlock()
		return jsonOK(map[string]any{
			"ok":                 false,
			"blocked":            true,
			"message":            "dependencies not satisfied",
			"pending_task_ids":   pendingDeps,
			"missing_task_ids":   missingDeps,
			"task":               td,
		})
	}
	// 验证 agent 存在并更新状态
	ag, agOk := globalState.agents[in.AgentID]
	if !agOk {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("agent not found: %s", in.AgentID)
	}
	ag.State = api.AgentStateBusy
	ag.UpdatedAt = now()
	td.AgentID = in.AgentID
	td.Status = api.TaskStatusQueued
	td.UpdatedAt = now()
	globalState.mu.Unlock()
	saveTasksToDisk()
	saveAgentsToDisk()
	return jsonOK(map[string]any{"ok": true, "task": td, "agent_state": string(ag.State)})
}

func handleTaskUpdate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Priority    int      `json:"priority"`
		Status      string   `json:"status"`
		Progress    *float64 `json:"progress"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	if in.ID == "" {
		return nil, fmt.Errorf("id required")
	}
	globalState.mu.Lock()
	td, ok := globalState.tasks[in.ID]
	if !ok {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("task not found")
	}
	if in.Title != "" {
		td.Title = in.Title
	}
	if in.Description != "" {
		td.Description = in.Description
	}
	if in.Priority > 0 {
		td.Priority = api.TaskPriority(in.Priority)
	}
	if in.Status != "" {
		td.Status = api.TaskStatus(in.Status)
	}
	if in.Progress != nil {
		td.Progress = clampTaskProgress(*in.Progress)
	}
	td.UpdatedAt = now()
	globalState.mu.Unlock()
	saveTasksToDisk()
	return jsonOK(map[string]any{"ok": true, "task": td})
}

func handleTaskCancel(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		ID string `json:"id"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	globalState.mu.Lock()
	td, ok := globalState.tasks[in.ID]
	if !ok {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("task not found")
	}
	td.Status = api.TaskStatusCancelled
	td.UpdatedAt = now()
	if td.AgentID != "" {
		if ag, ok := globalState.agents[td.AgentID]; ok && ag != nil {
			ag.State = api.AgentStateIdle
			ag.UpdatedAt = now()
		}
	}
	globalState.mu.Unlock()
	saveTasksToDisk()
	saveAgentsToDisk()
	return jsonOK(map[string]any{"ok": true, "task": td})
}

func clampTaskProgress(p float64) float64 {
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return 0
	}
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
