// swarm.go：swarm_* MCP 工具，维护 globalState.swarms 与 swarm-state.json。

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/swarm"
)

var swarmSeq int64

func parseTopologyString(s string) api.TopologyType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "hierarchical", "":
		return api.TopologyHierarchical
	case "mesh":
		return api.TopologyMesh
	case "hierarchical-mesh", "hierarchical_mesh":
		return api.TopologyHierarchicalMesh
	case "ring":
		return api.TopologyRing
	case "star":
		return api.TopologyStar
	case "adaptive":
		return api.TopologyAdaptive
	case "centralized":
		return api.TopologyCentralized
	case "hybrid":
		return api.TopologyHybrid
	default:
		return api.TopologyMesh
	}
}

func parseConsensusStrategy(s string) api.ConsensusAlgorithm {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "byzantine", "bft":
		return api.ConsensusByzantine
	case "gossip":
		return api.ConsensusGossip
	case "crdt":
		return api.ConsensusCRDT
	case "quorum":
		return api.ConsensusQuorum
	default:
		return api.ConsensusRaft
	}
}

func buildSwarmCoordinatorConfig(a swarmInitArgs) swarm.CoordinatorConfig {
	mx := a.MaxAgents
	if mx <= 0 {
		mx = 8
	}
	cfg := swarm.CoordinatorConfig{
		Topology:        parseTopologyString(a.Topology),
		ConsensusAlgo:   parseConsensusStrategy(a.Strategy),
		AgentPoolMin:    2,
		AgentPoolMax:    mx,
		HeartbeatMS:     500,
		MetricsInterval: time.Second,
	}
	if a.V3Mode && cfg.AgentPoolMax < 16 {
		cfg.AgentPoolMax = 16
	}
	return cfg
}

func coordinatorSnapshotForMCP(c *swarm.UnifiedSwarmCoordinator) map[string]any {
	st := c.GetState()
	met := c.GetMetrics()
	agents := c.GetAllAgents()
	agentSummaries := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		agentSummaries = append(agentSummaries, map[string]any{
			"id": a.ID, "name": a.Name, "type": a.Type, "state": a.State, "domain": a.Domain, "status": a.Status,
		})
	}
	return map[string]any{
		"healthy": c.IsHealthy(),
		"state": map[string]any{
			"status": st.Status, "topology": st.Topology, "agent_count": st.AgentCount,
			"pending_tasks": st.PendingTasks, "last_election": st.LastElection, "started_at": st.StartedAt,
		},
		"metrics": map[string]any{
			"tasks_submitted": met.TasksSubmitted, "tasks_assigned": met.TasksAssigned, "tasks_completed": met.TasksCompleted,
			"consensus_proposals": met.ConsensusProposals, "health_recoveries": met.HealthRecoveries,
			"messages_processed": met.MessagesProcessed, "updated_at": met.UpdatedAt,
		},
		"agents": agentSummaries,
	}
}

func swarmTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "swarm_init",
			Description: "Initialize a swarm with topology and capacity",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"topology":   map[string]any{"type": "string"},
					"max_agents": map[string]any{"type": "number"},
					"strategy":   map[string]any{"type": "string"},
					"v3_mode":    map[string]any{"type": "boolean"},
				},
			},
			Handler: handleSwarmInit,
		},
		{
			Name:        "swarm_status",
			Description: "Get swarm status by id (optional, latest if empty)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
			Handler: handleSwarmStatus,
		},
		{
			Name:        "swarm_shutdown",
			Description: "Shutdown a swarm by id (or all if id empty)",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
			},
			Handler: handleSwarmShutdown,
		},
		{
			Name:        "swarm_health",
			Description: "Swarm health overview",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleSwarmHealth,
		},
	}
}

// swarmInitArgs 解析蜂群初始化参数；拓扑/容量/策略有空缺默认值。
type swarmInitArgs struct {
	Topology  string `json:"topology"`
	MaxAgents int    `json:"max_agents"`
	Strategy  string `json:"strategy"`
	V3Mode    bool   `json:"v3_mode"`
}

// handleSwarmInit 创建 Active 状态蜂群记录并 saveSwarmToDisk。
func handleSwarmInit(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a swarmInitArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Topology == "" {
		a.Topology = "hierarchical"
	}
	if a.MaxAgents <= 0 {
		a.MaxAgents = 8
	}
	if a.Strategy == "" {
		a.Strategy = "specialized"
	}
	id := fmt.Sprintf("swarm-%d", atomic.AddInt64(&swarmSeq, 1))
	rec := &swarmRecord{
		ID:        id,
		Topology:  a.Topology,
		MaxAgents: a.MaxAgents,
		Strategy:  a.Strategy,
		V3Mode:    a.V3Mode,
		Status:    api.SwarmStatusActive,
		CreatedAt: now(),
		UpdatedAt: now(),
	}
	globalState.mu.Lock()
	globalState.swarms[id] = rec
	globalState.mu.Unlock()

	cfg := buildSwarmCoordinatorConfig(a)
	globalState.coordMu.Lock()
	globalState.pendingCoordCfg = &cfg
	globalState.coordMu.Unlock()
	if _, err := ensureCoordinator(); err != nil {
		return nil, fmt.Errorf("swarm coordinator: %w", err)
	}

	saveSwarmToDisk()
	return jsonOK(map[string]any{"ok": true, "swarm": rec})
}

// swarmIDArgs 可选蜂群 id；空表示取最近更新的一个。
type swarmIDArgs struct {
	ID string `json:"id"`
}

// handleSwarmStatus 按 id 精确查询，或返回 UpdatedAt 最新的一条；无数据时 swarm 为 nil。
func handleSwarmStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a swarmIDArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	if a.ID != "" {
		if s, ok := globalState.swarms[a.ID]; ok {
			out := map[string]any{"swarm": s}
			globalState.coordMu.Lock()
			c := globalState.coordinator
			globalState.coordMu.Unlock()
			if c != nil {
				out["coordinator"] = coordinatorSnapshotForMCP(c)
			}
			return jsonOK(out)
		}
		return nil, fmt.Errorf("swarm not found")
	}
	var latest *swarmRecord
	for _, s := range globalState.swarms {
		if latest == nil || s.UpdatedAt.After(latest.UpdatedAt) {
			latest = s
		}
	}
	if latest == nil {
		return jsonOK(map[string]any{"swarm": nil, "message": "no swarms"})
	}
	out := map[string]any{"swarm": latest}
	globalState.coordMu.Lock()
	c := globalState.coordinator
	globalState.coordMu.Unlock()
	if c != nil {
		out["coordinator"] = coordinatorSnapshotForMCP(c)
	}
	return jsonOK(out)
}

func handleSwarmShutdown(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a swarmIDArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	globalState.mu.Lock()
	if a.ID == "" {
		for id, s := range globalState.swarms {
			s.Status = api.SwarmStatusStopped
			s.UpdatedAt = now()
			globalState.swarms[id] = s
		}
	} else {
		s, ok := globalState.swarms[a.ID]
		if !ok {
			globalState.mu.Unlock()
			return nil, fmt.Errorf("swarm not found")
		}
		s.Status = api.SwarmStatusStopped
		s.UpdatedAt = now()
		globalState.swarms[a.ID] = s
	}
	remainingActive := 0
	for _, s := range globalState.swarms {
		if s.Status == api.SwarmStatusActive {
			remainingActive++
		}
	}
	globalState.mu.Unlock()
	saveSwarmToDisk()
	if remainingActive == 0 {
		shutdownSwarmCoordinator()
	}
	if a.ID == "" {
		return jsonOK(map[string]any{"ok": true, "stopped": "all"})
	}
	globalState.mu.RLock()
	s := globalState.swarms[a.ID]
	globalState.mu.RUnlock()
	return jsonOK(map[string]any{"ok": true, "swarm": s})
}

// handleSwarmHealth 返回蜂群总数与 Active 数量。
func handleSwarmHealth(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	active := 0
	for _, s := range globalState.swarms {
		if s.Status == api.SwarmStatusActive {
			active++
		}
	}
	out := map[string]any{
		"total":  len(globalState.swarms),
		"active": active,
	}
	globalState.coordMu.Lock()
	c := globalState.coordinator
	globalState.coordMu.Unlock()
	if c != nil {
		out["coordinator_healthy"] = c.IsHealthy()
	}
	return jsonOK(out)
}
