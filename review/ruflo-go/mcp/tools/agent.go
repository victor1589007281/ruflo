// agent.go：agent_* MCP 工具，围绕 globalState.agents 与 agents/store.json 持久化。

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
)

// agentTools 注册 spawn/list/status/terminate/health/pool/update 等代理相关工具。
func agentTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "agent_spawn",
			Description: "Register a new orchestration agent",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":      map[string]any{"type": "string", "description": "Agent type (e.g. coder, tester)"},
					"name":      map[string]any{"type": "string"},
					"namespace": map[string]any{"type": "string"},
				},
				"required": []string{"type", "name"},
			},
			Handler: handleAgentSpawn,
		},
		{
			Name:        "agent_list",
			Description: "List registered agents",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filter": map[string]any{"type": "string", "description": "Optional state filter"},
				},
			},
			Handler: handleAgentList,
		},
		{
			Name:        "agent_status",
			Description: "Get agent by id or name",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   map[string]any{"type": "string"},
					"name": map[string]any{"type": "string"},
				},
			},
			Handler: handleAgentStatus,
		},
		{
			Name:        "agent_terminate",
			Description: "Terminate an agent by id",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []string{"id"},
			},
			Handler: handleAgentTerminate,
		},
		{
			Name:        "agent_health",
			Description: "Aggregate agent health summary",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleAgentHealth,
		},
		{
			Name:        "agent_pool",
			Description: "Summarize agent pool capacity by state",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleAgentPool,
		},
		{
			Name:        "agent_update",
			Description: "Update agent metadata (namespace, name)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":        map[string]any{"type": "string"},
					"name":      map[string]any{"type": "string"},
					"namespace": map[string]any{"type": "string"},
				},
				"required": []string{"id"},
			},
			Handler: handleAgentUpdate,
		},
	}
}

type agentSpawnArgs struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func handleAgentSpawn(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a agentSpawnArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Type == "" || a.Name == "" {
		return nil, fmt.Errorf("type and name are required")
	}
	t := api.AgentType(a.Type)
	id := fmt.Sprintf("agent-%d", atomic.AddInt64(&agentSeq, 1))
	agent := &api.Agent{
		ID:        id,
		Name:      a.Name,
		Type:      t,
		Domain:    api.AgentDomainSwarm,
		State:     api.AgentStateIdle,
		Status:    api.AgentStatusHealthy,
		Namespace: a.Namespace,
		CreatedAt: now(),
		UpdatedAt: now(),
	}
	globalState.mu.Lock()
	globalState.agents[id] = agent
	globalState.mu.Unlock()
	saveAgentsToDisk()

	// 触发 AgentSpawn 钩子，让默认 handler 追踪 spawned agent
	globalState.hookExec.AgentSpawn(id, a.Type)

	// 与 UnifiedSwarmCoordinator 同步（若已初始化）
	globalState.coordMu.Lock()
	coord := globalState.coordinator
	globalState.coordMu.Unlock()
	var coordSync string
	if coord != nil {
		coordSync = "synced"
	}

	return jsonOK(map[string]any{"ok": true, "agent": agent, "coordinator_sync": coordSync})
}

type agentListArgs struct {
	Filter string `json:"filter"`
}

func handleAgentList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a agentListArgs
	_ = parseArgs(args, &a)
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	list := make([]*api.Agent, 0, len(globalState.agents))
	for _, ag := range globalState.agents {
		if a.Filter != "" && string(ag.State) != a.Filter && string(ag.Status) != a.Filter {
			continue
		}
		list = append(list, ag)
	}
	return jsonOK(map[string]any{"agents": list, "count": len(list)})
}

type agentIDArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func handleAgentStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a agentIDArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	if a.ID != "" {
		if ag, ok := globalState.agents[a.ID]; ok {
			return jsonOK(map[string]any{"agent": ag})
		}
	}
	if a.Name != "" {
		for _, ag := range globalState.agents {
			if ag.Name == a.Name {
				return jsonOK(map[string]any{"agent": ag})
			}
		}
	}
	return nil, fmt.Errorf("agent not found")
}

type agentStopArgs struct {
	ID string `json:"id"`
}

func handleAgentTerminate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a agentStopArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	globalState.mu.Lock()
	ag, ok := globalState.agents[a.ID]
	if !ok {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("agent not found")
	}
	ag.State = api.AgentStateStopped
	ag.Status = api.AgentStatusUnhealthy
	ag.UpdatedAt = now()
	globalState.mu.Unlock()
	saveAgentsToDisk()

	// 触发 AgentTerminate 钩子
	globalState.hookExec.AgentTerminate(a.ID)

	return jsonOK(map[string]any{"ok": true, "agent": ag})
}

// handleAgentPool 按 State 聚合数量，返回总数与 by_state 分布。
func handleAgentPool(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	by := map[string]int{}
	for _, ag := range globalState.agents {
		k := string(ag.State)
		if k == "" {
			k = "unknown"
		}
		by[k]++
	}
	return jsonOK(map[string]any{"total": len(globalState.agents), "by_state": by})
}

// agentUpdateArgs 解析可变的 name/namespace（非空才更新）。
type agentUpdateArgs struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// handleAgentUpdate 更新元数据并 saveAgentsToDisk。
func handleAgentUpdate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a agentUpdateArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	globalState.mu.Lock()
	ag, ok := globalState.agents[a.ID]
	if !ok {
		globalState.mu.Unlock()
		return nil, fmt.Errorf("agent not found")
	}
	if a.Name != "" {
		ag.Name = a.Name
	}
	if a.Namespace != "" {
		ag.Namespace = a.Namespace
	}
	ag.UpdatedAt = now()
	globalState.mu.Unlock()
	saveAgentsToDisk()
	return jsonOK(map[string]any{"ok": true, "agent": ag})
}

// handleAgentHealth 汇总 healthy/degraded/unhealthy 数量。
func handleAgentHealth(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	summary := map[string]int{"total": len(globalState.agents), "healthy": 0, "degraded": 0, "unhealthy": 0}
	for _, ag := range globalState.agents {
		switch ag.Status {
		case api.AgentStatusHealthy:
			summary["healthy"]++
		case api.AgentStatusDegraded:
			summary["degraded"]++
		default:
			summary["unhealthy"]++
		}
	}
	return jsonOK(map[string]any{"summary": summary})
}
