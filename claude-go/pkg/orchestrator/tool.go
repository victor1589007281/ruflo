package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ToolAction 定义 LLM 可调用的操作类型。
type ToolAction string

const (
	ActionCreateGraph  ToolAction = "create_graph"   // 创建工作流图
	ActionAddTask      ToolAction = "add_task"        // 添加任务
	ActionAddEdge      ToolAction = "add_edge"        // 添加依赖边
	ActionRunGraph     ToolAction = "run_graph"       // 执行工作流
	ActionGetStatus    ToolAction = "get_status"      // 查询执行状态
	ActionGetTask      ToolAction = "get_task"        // 查询单个任务
	ActionListGraphs   ToolAction = "list_graphs"     // 列出所有图
	ActionCancel       ToolAction = "cancel"          // 取消执行
	ActionReadBB       ToolAction = "read_blackboard" // 读取黑板
	ActionWriteBB      ToolAction = "write_blackboard"// 写入黑板
	ActionGetMetrics   ToolAction = "get_metrics"     // 获取执行指标
	ActionResume       ToolAction = "resume"          // 从检查点恢复
)

// ToolRequest 是 LLM function calling 的输入格式。
//
// 使用方式 (OpenAI function calling / Anthropic tool_use):
//
//	{
//	  "action": "create_graph",
//	  "params": {
//	    "id": "my-workflow",
//	    "name": "数据处理流水线"
//	  }
//	}
type ToolRequest struct {
	Action ToolAction     `json:"action"`
	Params map[string]any `json:"params"`
}

// ToolResponse 是 AI Tool 的统一返回格式。
type ToolResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ToolSchema 返回符合 OpenAI function calling 规范的工具描述。
// LLM 通过此 schema 了解可用操作及其参数。
func ToolSchema() map[string]any {
	return map[string]any{
		"name":        "workflow_engine",
		"description": "通用 Agent 编排与工作流引擎。支持 DAG 任务编排、并发控制、故障恢复、质量门禁等。",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "enum",
					"enum": []string{
						"create_graph", "add_task", "add_edge", "run_graph",
						"get_status", "get_task", "list_graphs", "cancel",
						"read_blackboard", "write_blackboard", "get_metrics", "resume",
					},
					"description": "要执行的操作",
				},
				"params": map[string]any{
					"type":        "object",
					"description": "操作参数, 具体字段取决于 action",
				},
			},
			"required": []string{"action"},
		},
	}
}

// ToolEngine 是面向 AI 的 Tool 接口层。
//
// 设计理念:
//   - 保留底层 Engine 的编程式 API (直接调用)
//   - 在上层封装 JSON 输入/输出, 适配 LLM function calling
//   - 每个 action 映射到底层 Engine 的一组操作
//   - 管理多个 Graph 的生命周期 (类似数据库连接池)
//   - 支持异步执行和状态查询
type ToolEngine struct {
	mu       sync.RWMutex
	engine   *Engine
	graphs   map[string]*Graph            // 已创建的图
	results  map[string]*ExecutionResult   // 已完成的执行结果
	cancels  map[string]context.CancelFunc // 执行中的取消函数
}

// NewToolEngine 创建 AI Tool 引擎实例。
func NewToolEngine(config EngineConfig) *ToolEngine {
	return &ToolEngine{
		engine:  NewEngine(config),
		graphs:  make(map[string]*Graph),
		results: make(map[string]*ExecutionResult),
		cancels: make(map[string]context.CancelFunc),
	}
}

// Engine 返回底层引擎, 供编程式调用。
func (te *ToolEngine) Engine() *Engine { return te.engine }

// HandleRequest 处理 LLM 发来的 Tool 请求。
// 这是 AI Tool 的统一入口, 根据 action 路由到对应的处理函数。
func (te *ToolEngine) HandleRequest(ctx context.Context, req ToolRequest) ToolResponse {
	switch req.Action {
	case ActionCreateGraph:
		return te.handleCreateGraph(req.Params)
	case ActionAddTask:
		return te.handleAddTask(req.Params)
	case ActionAddEdge:
		return te.handleAddEdge(req.Params)
	case ActionRunGraph:
		return te.handleRunGraph(ctx, req.Params)
	case ActionGetStatus:
		return te.handleGetStatus(req.Params)
	case ActionGetTask:
		return te.handleGetTask(req.Params)
	case ActionListGraphs:
		return te.handleListGraphs()
	case ActionCancel:
		return te.handleCancel(req.Params)
	case ActionReadBB:
		return te.handleReadBB(req.Params)
	case ActionWriteBB:
		return te.handleWriteBB(req.Params)
	case ActionGetMetrics:
		return te.handleGetMetrics(req.Params)
	default:
		return ToolResponse{Error: fmt.Sprintf("未知操作: %s", req.Action)}
	}
}

// HandleJSON 处理 JSON 字符串输入, 返回 JSON 字符串。
// 适用于不需要 Go 结构体的集成场景 (如 HTTP API / MCP)。
func (te *ToolEngine) HandleJSON(ctx context.Context, input string) string {
	var req ToolRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		resp := ToolResponse{Error: fmt.Sprintf("JSON 解析失败: %s", err)}
		data, _ := json.Marshal(resp)
		return string(data)
	}
	resp := te.HandleRequest(ctx, req)
	data, _ := json.Marshal(resp)
	return string(data)
}

// ---- 各 action 的处理函数 ----

func (te *ToolEngine) handleCreateGraph(params map[string]any) ToolResponse {
	id, _ := params["id"].(string)
	name, _ := params["name"].(string)
	if id == "" {
		return ToolResponse{Error: "缺少参数: id"}
	}
	if name == "" {
		name = id
	}

	te.mu.Lock()
	defer te.mu.Unlock()
	if _, exists := te.graphs[id]; exists {
		return ToolResponse{Error: fmt.Sprintf("图已存在: %s", id)}
	}
	te.graphs[id] = NewGraph(id, name)
	return ToolResponse{Success: true, Data: map[string]any{
		"id": id, "name": name, "message": "图创建成功",
	}}
}

func (te *ToolEngine) handleAddTask(params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	taskID, _ := params["task_id"].(string)
	taskName, _ := params["name"].(string)
	runner, _ := params["runner"].(string)

	if graphID == "" || taskID == "" {
		return ToolResponse{Error: "缺少参数: graph_id, task_id"}
	}
	if runner == "" {
		runner = "noop"
	}

	te.mu.RLock()
	g, ok := te.graphs[graphID]
	te.mu.RUnlock()
	if !ok {
		return ToolResponse{Error: fmt.Sprintf("图不存在: %s", graphID)}
	}

	task := &Task{
		ID:     taskID,
		Name:   taskName,
		Runner: runner,
	}

	// 可选参数
	if p, ok := params["priority"].(float64); ok {
		task.Priority = int(p)
	}
	if t, ok := params["timeout_seconds"].(float64); ok {
		task.Timeout = time.Duration(t) * time.Second
	}
	if r, ok := params["max_retries"].(float64); ok {
		task.MaxRetries = int(r)
	}
	if labels, ok := params["labels"].(map[string]any); ok {
		task.Labels = make(map[string]string)
		for k, v := range labels {
			task.Labels[k] = fmt.Sprintf("%v", v)
		}
	}
	if input, ok := params["input"]; ok {
		task.Input = input
	}

	if err := g.AddTask(task); err != nil {
		return ToolResponse{Error: err.Error()}
	}
	return ToolResponse{Success: true, Data: map[string]any{
		"task_id": taskID, "graph_id": graphID, "message": "任务添加成功",
	}}
}

func (te *ToolEngine) handleAddEdge(params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	from, _ := params["from"].(string)
	to, _ := params["to"].(string)

	if graphID == "" || from == "" || to == "" {
		return ToolResponse{Error: "缺少参数: graph_id, from, to"}
	}

	te.mu.RLock()
	g, ok := te.graphs[graphID]
	te.mu.RUnlock()
	if !ok {
		return ToolResponse{Error: fmt.Sprintf("图不存在: %s", graphID)}
	}

	if err := g.AddEdge(Edge{From: from, To: to, Kind: EdgeDependency}); err != nil {
		return ToolResponse{Error: err.Error()}
	}
	return ToolResponse{Success: true, Data: map[string]any{
		"from": from, "to": to, "message": "依赖边添加成功",
	}}
}

func (te *ToolEngine) handleRunGraph(ctx context.Context, params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	if graphID == "" {
		return ToolResponse{Error: "缺少参数: graph_id"}
	}

	te.mu.RLock()
	g, ok := te.graphs[graphID]
	te.mu.RUnlock()
	if !ok {
		return ToolResponse{Error: fmt.Sprintf("图不存在: %s", graphID)}
	}

	async, _ := params["async"].(bool)

	if async {
		// 异步执行: 立即返回, 后台运行
		execCtx, cancel := context.WithCancel(ctx)
		te.mu.Lock()
		te.cancels[graphID] = cancel
		te.mu.Unlock()

		go func() {
			result, _ := te.engine.Run(execCtx, g)
			te.mu.Lock()
			te.results[graphID] = result
			delete(te.cancels, graphID)
			te.mu.Unlock()
		}()

		return ToolResponse{Success: true, Data: map[string]any{
			"graph_id": graphID,
			"status":   "running",
			"message":  "工作流已异步启动, 使用 get_status 查询进度",
		}}
	}

	// 同步执行: 阻塞直到完成
	result, err := te.engine.Run(ctx, g)
	if err != nil {
		return ToolResponse{Error: err.Error()}
	}

	te.mu.Lock()
	te.results[graphID] = result
	te.mu.Unlock()

	return ToolResponse{Success: result.Success, Data: map[string]any{
		"exec_id":   result.ExecID,
		"success":   result.Success,
		"metrics":   result.Metrics,
		"completed": result.Metrics.CompletedTasks,
		"failed":    result.Metrics.FailedTasks,
		"elapsed":   result.Metrics.Elapsed.String(),
	}}
}

func (te *ToolEngine) handleGetStatus(params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	if graphID == "" {
		return ToolResponse{Error: "缺少参数: graph_id"}
	}

	te.mu.RLock()
	g, gOK := te.graphs[graphID]
	result, rOK := te.results[graphID]
	_, running := te.cancels[graphID]
	te.mu.RUnlock()

	if !gOK {
		return ToolResponse{Error: fmt.Sprintf("图不存在: %s", graphID)}
	}

	status := "created"
	if running {
		status = "running"
	} else if rOK {
		if result.Success {
			status = "completed"
		} else {
			status = "failed"
		}
	}

	stats := g.Stats()
	data := map[string]any{
		"graph_id": graphID,
		"status":   status,
		"stats":    formatStats(stats),
	}
	if rOK {
		data["exec_id"] = result.ExecID
		data["success"] = result.Success
		data["metrics"] = result.Metrics
	}

	return ToolResponse{Success: true, Data: data}
}

func (te *ToolEngine) handleGetTask(params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	taskID, _ := params["task_id"].(string)
	if graphID == "" || taskID == "" {
		return ToolResponse{Error: "缺少参数: graph_id, task_id"}
	}

	te.mu.RLock()
	g, ok := te.graphs[graphID]
	te.mu.RUnlock()
	if !ok {
		return ToolResponse{Error: fmt.Sprintf("图不存在: %s", graphID)}
	}

	t, ok := g.Tasks[taskID]
	if !ok {
		return ToolResponse{Error: fmt.Sprintf("任务不存在: %s", taskID)}
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	return ToolResponse{Success: true, Data: map[string]any{
		"task_id":  t.ID,
		"name":    t.Name,
		"state":   t.state.String(),
		"retries": t.retries,
		"error":   t.err,
		"output":  t.output,
		"runner":  t.Runner,
	}}
}

func (te *ToolEngine) handleListGraphs() ToolResponse {
	te.mu.RLock()
	defer te.mu.RUnlock()

	var graphs []map[string]any
	for id, g := range te.graphs {
		_, running := te.cancels[id]
		status := "created"
		if running {
			status = "running"
		} else if _, ok := te.results[id]; ok {
			status = "finished"
		}
		graphs = append(graphs, map[string]any{
			"id":     id,
			"name":   g.Name,
			"tasks":  g.TaskCount(),
			"status": status,
		})
	}
	return ToolResponse{Success: true, Data: graphs}
}

func (te *ToolEngine) handleCancel(params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	if graphID == "" {
		return ToolResponse{Error: "缺少参数: graph_id"}
	}

	te.mu.Lock()
	cancel, ok := te.cancels[graphID]
	te.mu.Unlock()

	if !ok {
		return ToolResponse{Error: fmt.Sprintf("图 %s 未在运行中", graphID)}
	}

	cancel()
	return ToolResponse{Success: true, Data: map[string]any{
		"graph_id": graphID, "message": "取消信号已发送",
	}}
}

func (te *ToolEngine) handleReadBB(params map[string]any) ToolResponse {
	key, _ := params["key"].(string)
	if key == "" {
		// 返回全部快照
		snap := te.engine.BB().Snapshot()
		return ToolResponse{Success: true, Data: snap}
	}

	val, meta, err := te.engine.BB().Read(key)
	if err != nil {
		return ToolResponse{Error: err.Error()}
	}
	return ToolResponse{Success: true, Data: map[string]any{
		"key":     key,
		"value":   val,
		"version": meta.Version,
		"author":  meta.Author,
	}}
}

func (te *ToolEngine) handleWriteBB(params map[string]any) ToolResponse {
	key, _ := params["key"].(string)
	value, hasVal := params["value"]
	if key == "" || !hasVal {
		return ToolResponse{Error: "缺少参数: key, value"}
	}

	author, _ := params["author"].(string)
	category, _ := params["category"].(string)

	ver, err := te.engine.BB().Write(key, value, WriteMeta{Author: author, Category: category})
	if err != nil {
		return ToolResponse{Error: err.Error()}
	}
	return ToolResponse{Success: true, Data: map[string]any{
		"key": key, "version": ver,
	}}
}

func (te *ToolEngine) handleGetMetrics(params map[string]any) ToolResponse {
	graphID, _ := params["graph_id"].(string)
	if graphID == "" {
		return ToolResponse{Error: "缺少参数: graph_id"}
	}

	te.mu.RLock()
	result, ok := te.results[graphID]
	te.mu.RUnlock()

	if !ok {
		return ToolResponse{Error: fmt.Sprintf("无执行结果: %s", graphID)}
	}
	return ToolResponse{Success: true, Data: result.Metrics}
}

func formatStats(stats map[TaskState]int) map[string]int {
	m := make(map[string]int)
	for s, c := range stats {
		m[s.String()] = c
	}
	return m
}
