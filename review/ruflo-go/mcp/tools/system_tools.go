package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
)

// 本文件：编排子系统与 MCP 运行时的只读/轻量维护工具（状态、健康、指标、路径、任务汇总等）。
//
// 设计思路：多数 handler 读取 globalState 或 runtime.MemStats；system_reset 仅清空 hooks 日志为软重置；
// mcp_status 固定 transport 提示 stdio。

// systemTools 注册 system_status、system_health、system_info、system_metrics、system_reset、mcp_status、task_summary。
func systemTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "system_status",
			Description: "Orchestration subsystem counts",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleSystemStatus,
		},
		{
			Name:        "system_health",
			Description: "Liveness and dependency summary",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleSystemHealth,
		},
		{
			Name:        "system_info",
			Description: "Runtime and paths",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleSystemInfo,
		},
		{
			Name:        "system_metrics",
			Description: "Runtime memory and goroutine counts",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleSystemMetrics,
		},
		{
			Name:        "system_reset",
			Description: "Clear in-process hooks log (soft reset)",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleSystemReset,
		},
		{
			Name:        "mcp_status",
			Description: "MCP subsystem health snapshot",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleMCPStatus,
		},
		{
			Name:        "task_summary",
			Description: "Aggregate task counts by status",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleTaskSummary,
		},
	}
}

// handleSystemStatus 返回 agents/swarms/tasks/sessions/hooks_log 计数及跨 namespace 的 memory 条目总和。
func handleSystemStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	out := map[string]any{
		"agents":    len(globalState.agents),
		"swarms":    len(globalState.swarms),
		"tasks":     len(globalState.tasks),
		"sessions":  len(globalState.sessions),
		"hooks_log": len(globalState.hooksLog),
	}
	mem := 0
	for _, m := range globalState.memory {
		mem += len(m)
	}
	out["memory_map_entries"] = mem
	globalState.mu.RUnlock()
	return jsonOK(out)
}

// handleSystemHealth 检查 hookExec、reasoningBank 非 nil，以及 memory 是否已初始化或有数据，返回 healthy 与原因列表。
func handleSystemHealth(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	ok := true
	reasons := []string{}
	if globalState.hookExec == nil {
		ok = false
		reasons = append(reasons, "hook executor nil")
	}
	if globalState.reasoningBank == nil {
		ok = false
		reasons = append(reasons, "reasoning bank nil")
	}
	globalState.mu.RLock()
	memOk := globalState.memoryInitialized || getUnifiedMemory() != nil || len(globalState.memory) > 0
	globalState.mu.RUnlock()
	if !memOk {
		reasons = append(reasons, "memory not initialized")
	}
	return jsonOK(map[string]any{"healthy": ok, "reasons": reasons, "memory_ready": memOk})
}

// handleSystemMetrics 读取 runtime.MemStats 与 NumGoroutine，返回堆分配与 GC 次数等原始计数。
func handleSystemMetrics(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return jsonOK(map[string]any{
		"goroutines":   runtime.NumGoroutine(),
		"heap_alloc":   ms.HeapAlloc,
		"heap_sys":     ms.HeapSys,
		"total_alloc":  ms.TotalAlloc,
		"gc_cycles":    ms.NumGC,
	})
}

// handleSystemReset 在写锁下清空 globalState.hooksLog（软重置，不影响其他状态）。
func handleSystemReset(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.Lock()
	globalState.hooksLog = nil
	globalState.mu.Unlock()
	return jsonOK(map[string]any{"ok": true, "hooks_log_cleared": true})
}

// handleMCPStatus 返回 MCP 侧快照：Go 版本、data_dir、transport 提示。
func handleMCPStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	return jsonOK(map[string]any{
		"ok":        true,
		"go":        runtime.Version(),
		"data_dir":  resolveDataDir(),
		"transport": "stdio",
	})
}

// handleTaskSummary 按 TaskStatus 聚合 globalState.tasks 数量，返回 by_status 与 total。
func handleTaskSummary(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	by := map[api.TaskStatus]int{}
	for _, t := range globalState.tasks {
		if t != nil {
			by[t.Status]++
		}
	}
	out := map[string]int{}
	for k, v := range by {
		out[string(k)] = v
	}
	return jsonOK(map[string]any{"by_status": out, "total": len(globalState.tasks)})
}

// handleSystemInfo 返回 Go/OS、cwd、data_dir 及 agents/swarms/tasks 存储路径提示与 config_hint。
func handleSystemInfo(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	cwd, _ := os.Getwd()
	return jsonOK(map[string]any{
		"go_version":   runtime.Version(),
		"os_arch":      runtime.GOOS + "/" + runtime.GOARCH,
		"cwd":          cwd,
		"data_dir":     resolveDataDir(),
		"agents_store": agentsStorePath(),
		"swarm_state":  swarmStatePath(),
		"tasks_store":  tasksStorePath(),
		"config_hint":  filepath.Join(cwd, "claude-flow.config.json"),
	})
}
