package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	"github.com/ruflo/ruflo-go/pkg/hooks"
)

func hooksTools() []*mcp.MCPTool {
	base := []*mcp.MCPTool{
		{
			Name:        "hooks_pre-task",
			Description: "Invoke pre-task hook via hook executor",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"description": map[string]any{"type": "string"},
					"session_id":  map[string]any{"type": "string"},
					"agent_id":    map[string]any{"type": "string"},
				},
			},
			Handler: handleHooksPreTask,
		},
		{
			Name:        "hooks_post-task",
			Description: "Invoke post-task hook",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string"},
					"success": map[string]any{"type": "boolean"},
					"train":   map[string]any{"type": "boolean"},
				},
			},
			Handler: handleHooksPostTask,
		},
		{
			Name:        "hooks_route",
			Description: "Route task via ReasoningBank and hook executor",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"description": map[string]any{"type": "string"},
					"task":        map[string]any{"type": "string"},
					"complexity":  map[string]any{"type": "number"},
				},
			},
			Handler: handleHooksRouteTool,
		},
		{
			Name:        "hooks_session-start",
			Description: "Session start hook",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
				},
			},
			Handler: handleHooksSessionStart,
		},
		{
			Name:        "hooks_session-end",
			Description: "Session end hook",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id":     map[string]any{"type": "string"},
					"export_metrics": map[string]any{"type": "boolean"},
				},
			},
			Handler: handleHooksSessionEnd,
		},
		{
			Name:        "hooks_worker-list",
			Description: "List registered background workers",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleHooksWorkerList,
		},
		{
			Name:        "hooks_worker-dispatch",
			Description: "Dispatch workers for a trigger",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"trigger": map[string]any{"type": "string"},
				},
				"required": []string{"trigger"},
			},
			Handler: handleHooksWorkerDispatch,
		},
		{
			Name:        "hooks_worker-status",
			Description: "Per-worker runtime status",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleHooksWorkerStatus,
		},
		{
			Name:        "hooks_model-route",
			Description: "LLM pre-hook: cache check, provider defaults, metrics",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{"type": "string"},
					"model":    map[string]any{"type": "string"},
					"messages": map[string]any{"type": "array"},
				},
			},
			Handler: handleHooksModelRoute,
		},
		{
			Name:        "hooks_metrics",
			Description: "Hook invocation metrics",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleHooksMetrics,
		},
	}
	return append(base, hooksMoreTools()...)
}

func logHook(name string, args map[string]any, res hooks.HookResult) {
	globalState.mu.Lock()
	globalState.hooksLog = append(globalState.hooksLog, hookInvocation{
		Name:      name,
		Args:      args,
		Result:    res,
		Timestamp: now(),
	})
	if len(globalState.hooksLog) > 500 {
		globalState.hooksLog = globalState.hooksLog[len(globalState.hooksLog)-500:]
	}
	globalState.mu.Unlock()
}

type preTaskArgs struct {
	Description string `json:"description"`
	SessionID   string `json:"session_id"`
	AgentID     string `json:"agent_id"`
}

func handleHooksPreTask(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a preTaskArgs
	_ = parseArgs(args, &a)
	sess := map[string]any{"session_id": a.SessionID, "agent_id": a.AgentID}
	hc := hooks.HookContext{
		Session: sess,
		Args: map[string]any{
			"description": a.Description,
		},
		Command: a.Description,
	}
	res := globalState.hookExec.Execute(hooks.HookEventPreTask, hc)
	logHook("hooks_pre-task", hc.Args, res)
	return jsonOK(map[string]any{"result": res})
}

type postTaskArgs struct {
	TaskID  string `json:"task_id"`
	Success bool   `json:"success"`
	Train   bool   `json:"train"`
}

func handleHooksPostTask(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a postTaskArgs
	_ = parseArgs(args, &a)
	hc := hooks.HookContext{
		Args: map[string]any{
			"success": a.Success,
			"train":   a.Train,
			"task_id": a.TaskID,
		},
	}
	if a.TaskID != "" {
		hc.Task = &api.TaskDefinition{ID: a.TaskID}
	}
	res := globalState.hookExec.Execute(hooks.HookEventPostTask, hc)
	logHook("hooks_post-task", hc.Args, res)
	return jsonOK(map[string]any{"result": res})
}

type routeArgs struct {
	Task        string  `json:"task"`
	Description string  `json:"description"`
	Complexity  float64 `json:"complexity"`
}

func handleHooksRouteTool(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a routeArgs
	_ = parseArgs(args, &a)
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		desc = strings.TrimSpace(a.Task)
	}
	var emb []float32
	if desc != "" {
		emb = embeddings.HashEmbed384(desc)
	}
	rt := globalState.reasoningBank.RouteTask(desc, emb)
	hc := hooks.HookContext{
		Args: map[string]any{
			"task": a.Task, "description": desc, "complexity": a.Complexity,
		},
		Command: desc,
	}
	pre := globalState.hookExec.Execute(hooks.HookEventPreRoute, hc)
	post := globalState.hookExec.Execute(hooks.HookEventPostRoute, hc)
	if pre.Data == nil {
		pre.Data = map[string]any{}
	}
	tier := 2
	if a.Complexity > 0.7 {
		tier = 3
	} else if a.Complexity < 0.2 && desc != "" {
		tier = 1
	}
	pre.Data["tier"] = tier
	logHook("hooks_route", hc.Args, pre)
	return jsonOK(map[string]any{
		"routing":    rt,
		"pre_route":  pre,
		"post_route": post,
	})
}

type sessHookArgs struct {
	SessionID     string `json:"session_id"`
	ExportMetrics bool   `json:"export_metrics"`
}

func handleHooksSessionStart(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a sessHookArgs
	_ = parseArgs(args, &a)
	hc := hooks.HookContext{
		Session: map[string]any{"session_id": a.SessionID, "phase": "start"},
		Args:    map[string]any{"phase": "start"},
	}
	res := globalState.hookExec.Execute(hooks.HookEventSessionStart, hc)
	logHook("hooks_session-start", hc.Args, res)
	return jsonOK(map[string]any{"result": res})
}

func handleHooksSessionEnd(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a sessHookArgs
	_ = parseArgs(args, &a)
	hc := hooks.HookContext{
		Session: map[string]any{
			"session_id": a.SessionID, "phase": "end", "export_metrics": a.ExportMetrics,
		},
		Args: map[string]any{
			"phase": "end", "export_metrics": a.ExportMetrics,
		},
	}
	res := globalState.hookExec.Execute(hooks.HookEventSessionEnd, hc)
	logHook("hooks_session-end", hc.Args, res)
	return jsonOK(map[string]any{"result": res})
}

func handleHooksWorkerList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	ws := globalState.workerMgr.ListWorkers()
	return jsonOK(map[string]any{"workers": ws, "count": len(ws)})
}

type dispatchArgs struct {
	Trigger string `json:"trigger"`
}

func handleHooksWorkerDispatch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a dispatchArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Trigger == "" {
		return nil, fmt.Errorf("trigger is required")
	}
	wctx := hooks.WorkerContext{Trigger: a.Trigger}
	results := globalState.workerMgr.Dispatch(ctx, a.Trigger, wctx)
	res := hooks.HookResult{Success: true, Data: map[string]any{"results": results}}
	logHook("hooks_worker-dispatch", map[string]any{"trigger": a.Trigger}, res)
	return jsonOK(map[string]any{"result": res})
}

func handleHooksWorkerStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	st := map[string]any{}
	for _, w := range globalState.workerMgr.ListWorkers() {
		s, err := globalState.workerMgr.GetStatus(w.Name)
		if err != nil {
			continue
		}
		st[w.Name] = s
	}
	return jsonOK(map[string]any{"workers": st})
}

type modelRouteArgs struct {
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Messages []api.LLMMessage `json:"messages"`
}

func handleHooksModelRoute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a modelRouteArgs
	_ = parseArgs(args, &a)
	req := &api.LLMRequest{Provider: api.LLMProvider(a.Provider), Model: a.Model, Messages: a.Messages}
	_, cached, hit := globalState.llmHooks.PreLLMCallHook(req)
	return jsonOK(map[string]any{
		"cache_hit": hit,
		"cached":    cached,
		"request":   req,
		"metrics":   globalState.llmHooks.MetricsSnapshot(),
	})
}

func handleHooksMetrics(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	n := len(globalState.hooksLog)
	globalState.mu.RUnlock()
	return jsonOK(map[string]any{
		"invocations": n,
		"workers":     len(globalState.workerMgr.ListWorkers()),
	})
}
