package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
)

func TestToolEngine_CreateAndRunWorkflow(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	// 注册测试执行器
	te.Engine().Runners().Register(NewFuncRunner("echo", func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
		return "ok:" + task.ID, nil
	}))

	// 1. 创建图
	resp := te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{
		"id": "test-flow", "name": "测试流水线",
	}})
	if !resp.Success {
		t.Fatalf("创建图失败: %s", resp.Error)
	}

	// 2. 添加任务
	for _, id := range []string{"A", "B", "C"} {
		resp = te.HandleRequest(ctx, ToolRequest{Action: ActionAddTask, Params: map[string]any{
			"graph_id": "test-flow", "task_id": id, "name": "任务" + id, "runner": "echo",
		}})
		if !resp.Success {
			t.Fatalf("添加任务 %s 失败: %s", id, resp.Error)
		}
	}

	// 3. 添加依赖: A → B → C
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionAddEdge, Params: map[string]any{
		"graph_id": "test-flow", "from": "A", "to": "B",
	}})
	if !resp.Success {
		t.Fatalf("添加边失败: %s", resp.Error)
	}
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionAddEdge, Params: map[string]any{
		"graph_id": "test-flow", "from": "B", "to": "C",
	}})
	if !resp.Success {
		t.Fatalf("添加边失败: %s", resp.Error)
	}

	// 4. 同步执行
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionRunGraph, Params: map[string]any{
		"graph_id": "test-flow",
	}})
	if !resp.Success {
		t.Fatalf("执行失败: %s", resp.Error)
	}

	// 5. 查询状态
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionGetStatus, Params: map[string]any{
		"graph_id": "test-flow",
	}})
	if !resp.Success {
		t.Fatalf("查询状态失败: %s", resp.Error)
	}

	data := resp.Data.(map[string]any)
	if data["status"] != "completed" {
		t.Errorf("期望 status=completed, 实际 %s", data["status"])
	}
}

func TestToolEngine_HandleJSON(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	input := `{"action":"create_graph","params":{"id":"json-test","name":"JSON测试"}}`
	output := te.HandleJSON(ctx, input)

	var resp ToolResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		t.Fatalf("JSON 解析失败: %s", err)
	}
	if !resp.Success {
		t.Errorf("JSON 接口调用失败: %s", resp.Error)
	}
}

func TestToolEngine_ListGraphs(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{
		"id": "g1", "name": "图1",
	}})
	te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{
		"id": "g2", "name": "图2",
	}})

	resp := te.HandleRequest(ctx, ToolRequest{Action: ActionListGraphs})
	if !resp.Success {
		t.Fatalf("列出图失败: %s", resp.Error)
	}
	graphs := resp.Data.([]map[string]any)
	if len(graphs) != 2 {
		t.Errorf("期望 2 个图, 实际 %d", len(graphs))
	}
}

func TestToolEngine_ReadWriteBlackboard(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	// 写入
	resp := te.HandleRequest(ctx, ToolRequest{Action: ActionWriteBB, Params: map[string]any{
		"key": "test/result", "value": "hello", "author": "tester",
	}})
	if !resp.Success {
		t.Fatalf("写入黑板失败: %s", resp.Error)
	}

	// 读取
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionReadBB, Params: map[string]any{
		"key": "test/result",
	}})
	if !resp.Success {
		t.Fatalf("读取黑板失败: %s", resp.Error)
	}
	data := resp.Data.(map[string]any)
	if data["value"] != "hello" {
		t.Errorf("期望 value=hello, 实际 %v", data["value"])
	}
}

func TestToolEngine_GetTask(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{
		"id": "tg", "name": "task graph",
	}})
	te.HandleRequest(ctx, ToolRequest{Action: ActionAddTask, Params: map[string]any{
		"graph_id": "tg", "task_id": "t1", "name": "测试任务", "priority": float64(10),
	}})

	resp := te.HandleRequest(ctx, ToolRequest{Action: ActionGetTask, Params: map[string]any{
		"graph_id": "tg", "task_id": "t1",
	}})
	if !resp.Success {
		t.Fatalf("查询任务失败: %s", resp.Error)
	}
	data := resp.Data.(map[string]any)
	if data["name"] != "测试任务" {
		t.Errorf("任务名称不匹配: %v", data["name"])
	}
}

func TestToolEngine_ErrorCases(t *testing.T) {
	te := NewToolEngine(testConfig())
	ctx := context.Background()

	// 未知 action
	resp := te.HandleRequest(ctx, ToolRequest{Action: "unknown"})
	if resp.Success || resp.Error == "" {
		t.Error("未知 action 应该返回错误")
	}

	// 缺少参数
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{}})
	if resp.Success {
		t.Error("缺少 id 应该返回错误")
	}

	// 重复创建
	te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{"id": "dup"}})
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionCreateGraph, Params: map[string]any{"id": "dup"}})
	if resp.Success {
		t.Error("重复创建应该返回错误")
	}

	// 不存在的图
	resp = te.HandleRequest(ctx, ToolRequest{Action: ActionRunGraph, Params: map[string]any{"graph_id": "noexist"}})
	if resp.Success {
		t.Error("不存在的图应该返回错误")
	}
}

func TestToolSchema(t *testing.T) {
	schema := ToolSchema()
	if schema["name"] != "workflow_engine" {
		t.Error("schema name 不匹配")
	}
}
