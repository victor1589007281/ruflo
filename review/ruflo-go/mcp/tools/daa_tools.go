package tools

// 本文件实现 DAA（Dynamic Agent Architecture）相关的文件持久化 MCP 工具（daa_*）。
//
// 设计思路：
//   - 将代理元数据、工作流记录、知识片段与指标聚合存入 daa/state.json，供编排层做轻量审计与学习状态展示。
//   - 执行为追加/更新式记录，非完整多代理运行时；工作流 execute 通过 id 线性扫描更新状态字段。

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

// daaStateFile 为 DAA 持久化文件的 JSON 结构：代理列表、工作流、知识与指标。
type daaStateFile struct {
	Agents    []map[string]any `json:"agents"`
	Workflows []map[string]any `json:"workflows"`
	Knowledge []map[string]any `json:"knowledge"`
	Metrics   map[string]any   `json:"metrics"`
	UpdatedAt time.Time        `json:"updated_at"`
}

var (
	daaMu sync.Mutex
	// daaData 进程内 DAA 状态；Metrics 默认非 nil。
	daaData = &daaStateFile{Metrics: map[string]any{}}
)

// daaPath 返回 DAA 状态文件路径。
func daaPath() string {
	return filepath.Join(resolveDataDir(), "daa", "state.json")
}

// daaLoad 从磁盘加载 DAA 状态；失败时保留内存现状。
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

// daaSave 将 Agents/Workflows/Knowledge/Metrics 快照写入磁盘。
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

// daaTools 构造 daa_* MCP 工具定义。
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

// RegisterDAATools 向注册表登记 DAA 文件持久化 MCP 工具。
func RegisterDAATools(reg *mcp.ToolRegistry) error {
	for _, t := range daaTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleDAAAgentCreate 处理 daa_agent_create：name、role 为代理标识；name 默认 agent，写入 Agents 并保存。
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

// handleDAAAgentAdapt 处理 daa_agent_adapt：记录 agent 与 delta 到 Metrics（最近适应事件）。
func handleDAAAgentAdapt(_ context.Context, m map[string]any) mcp.MCPToolResult {
	daaMu.Lock()
	daaData.Metrics["last_adapt_agent"] = strArg(m, "agent")
	daaData.Metrics["last_adapt_delta"] = strArg(m, "delta")
	daaMu.Unlock()
	_ = daaSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"adapted": true}}
}

// handleDAAWorkflowCreate 处理 daa_workflow_create：name 默认 workflow，生成纳秒级 id，状态 created。
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

// handleDAAWorkflowExecute 处理 daa_workflow_execute：参数 id，将匹配工作流 status 置为 executed 并写 executed_at。
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

// handleDAAKnowledgeShare 处理 daa_knowledge_share：topic、content 写入 Knowledge，超长裁剪至 500 条。
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

// handleDAALearningStatus 处理 daa_learning_status：返回 agents/workflows/knowledge 数量统计。
func handleDAALearningStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	daaMu.Lock()
	nA, nW, nK := len(daaData.Agents), len(daaData.Workflows), len(daaData.Knowledge)
	daaMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agents": nA, "workflows": nW, "knowledge": nK}}
}

// handleDAACognitivePattern 处理 daa_cognitive_pattern：参数 pattern（必填）写入 Metrics.last_cognitive_pattern。
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

// handleDAAPerformanceMetrics 处理 daa_performance_metrics：可选 key/value 写入指标；始终返回 metrics 全量副本；有 key 时保存。
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
