// hooks.go：hooks_* 核心 MCP 工具，串联 HookExecutor、ReasoningBank、WorkerManager 与 LLMHookBundle；
// hooksMoreTools 的扩展定义在 hooks_more.go。

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

// hooksTools 构建 pre/post task、route、session、worker、model-route、metrics 等工具，并拼接 hooksMoreTools。
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

// logHook 将一次钩子调用追加到 globalState.hooksLog，超过 500 条时保留尾部窗口。
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

// preTaskArgs 解析任务开始前钩子上下文。
type preTaskArgs struct {
	Description string `json:"description"`
	SessionID   string `json:"session_id"`
	AgentID     string `json:"agent_id"`
}

// handleHooksPreTask 执行 HookEventPreTask 并记录日志。
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

// postTaskArgs 解析任务结束后钩子参数。
type postTaskArgs struct {
	TaskID  string `json:"task_id"`
	Success bool   `json:"success"`
	Train   bool   `json:"train"`
}

// handleHooksPostTask 执行 HookEventPostTask，可选附带 Task 引用。
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

// routeArgs 描述待路由任务文本与可选复杂度，用于 ReasoningBank 与 tier 推断。
type routeArgs struct {
	Task        string  `json:"task"`
	Description string  `json:"description"`
	Complexity  float64 `json:"complexity"`
}

// handleHooksRouteTool：对描述做 HashEmbed384，调用 ReasoningBank.RouteTask；再跑 PreRoute/PostRoute，
// 并按 complexity 写入 tier（1/2/3）到 pre_route.Data。
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

// sessHookArgs 会话起止钩子共用参数。
type sessHookArgs struct {
	SessionID     string `json:"session_id"`
	ExportMetrics bool   `json:"export_metrics"`
}

// handleHooksSessionStart 触发 HookEventSessionStart。
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

// handleHooksSessionEnd 触发 HookEventSessionEnd，并传入 export_metrics。
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

// handleHooksWorkerList 枚举 WorkerManager 中已注册 Worker 的摘要信息。
func handleHooksWorkerList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	ws := globalState.workerMgr.ListWorkers()
	type workerInfo struct {
		Name     string `json:"name"`
		Priority string `json:"priority"`
		Triggers int    `json:"trigger_count"`
	}
	out := make([]workerInfo, len(ws))
	for i, w := range ws {
		out[i] = workerInfo{
			Name:     w.Name,
			Priority: string(w.Priority),
			Triggers: len(w.TriggerPatterns),
		}
	}
	return jsonOK(map[string]any{"workers": out, "count": len(out)})
}

// dispatchArgs 指定 Worker 派发触发器字符串。
type dispatchArgs struct {
	Trigger string `json:"trigger"`
}

// handleHooksWorkerDispatch 调用 WorkerManager.Dispatch 并包装为 HookResult 记录日志。
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

// handleHooksWorkerStatus 逐个 Worker 查询 GetStatus，跳过查询失败的项。
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

// modelRouteArgs 解析 LLM 预钩子所需的请求轮廓。
type modelRouteArgs struct {
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Messages []api.LLMMessage `json:"messages"`
}

// handleHooksModelRoute 调用 PreLLMCallHook，返回缓存命中与指标快照。
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

// handleHooksMetrics 返回 hooksLog 条数与 Worker 数量等粗粒度指标。
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
