package tools

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/swarm"
)

var (
	coordOnce    sync.Once
	coordInst    *swarm.UnifiedSwarmCoordinator
	coordInitErr error
)

func ensureCoordinator() (*swarm.UnifiedSwarmCoordinator, error) {
	coordOnce.Do(func() {
		cfg := swarm.CoordinatorConfig{
			Topology:        api.TopologyMesh,
			ConsensusAlgo:   api.ConsensusRaft,
			AgentPoolMin:    2,
			AgentPoolMax:    16,
			HeartbeatMS:     500,
			MetricsInterval: time.Second,
		}
		coordInst = swarm.NewUnifiedSwarmCoordinator(cfg)
		coordInitErr = coordInst.Initialize(context.Background())
		if coordInitErr == nil {
			boot := &api.Agent{
				ID: "coord-bootstrap", Name: "bootstrap", Type: api.AgentTypeCoder,
				Domain: api.AgentDomainCore, State: api.AgentStateIdle, Status: api.AgentStatusHealthy,
				CreatedAt: now(), UpdatedAt: now(),
			}
			if err := coordInst.RegisterAgent(boot); err != nil {
				coordInitErr = err
			}
		}
	})
	return coordInst, coordInitErr
}

func coordinationTools() []*mcp.MCPTool {
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "coordination_topology", Description: "Report swarm topology summary via pkg/swarm", InputSchema: obj, Handler: toolHandler(handleCoordinationTopology)},
		{Name: "coordination_load_balance", Description: "Pool size and coarse load snapshot", InputSchema: obj, Handler: toolHandler(handleCoordinationLoadBalance)},
		{Name: "coordination_sync", Description: "Coordination sync checkpoint", InputSchema: obj, Handler: toolHandler(handleCoordinationSync)},
		{Name: "coordination_node", Description: "Add topology node (agent)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "role": map[string]any{"type": "string"}}, "required": []string{"agent_id"}}, Handler: toolHandler(handleCoordinationNode)},
		{Name: "coordination_consensus", Description: "Propose binary consensus value", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}, Handler: toolHandler(handleCoordinationConsensus)},
		{Name: "coordination_orchestrate", Description: "Submit a task to the coordinator", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}}, Handler: toolHandler(handleCoordinationOrchestrate)},
		{Name: "coordination_metrics", Description: "Message bus and pool metrics", InputSchema: obj, Handler: toolHandler(handleCoordinationMetrics)},
	}
}

// RegisterCoordinationTools registers pkg/swarm coordination bridges.
func RegisterCoordinationTools(reg *mcp.ToolRegistry) error {
	for _, t := range coordinationTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleCoordinationTopology(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	c, err := ensureCoordinator()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if c.Topology() == nil {
		return mcp.MCPToolResult{OK: false, Error: "topology nil"}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"backend": "pkg/swarm", "topology": "mesh"}}
}

func handleCoordinationLoadBalance(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	c, err := ensureCoordinator()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	p := c.Pool()
	if p == nil {
		return mcp.MCPToolResult{OK: false, Error: "pool nil"}
	}
	agents := p.List()
	load := 0.0
	for _, a := range agents {
		if a != nil && a.Metrics.Extra != nil {
			load += a.Metrics.Extra["load"]
		}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"pool_size": p.Size(), "agents": len(agents), "aggregate_load": load}}
}

func handleCoordinationSync(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	if _, err := ensureCoordinator(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"synced": true, "at": now()}}
}

func handleCoordinationNode(_ context.Context, m map[string]any) mcp.MCPToolResult {
	aid := strArg(m, "agent_id")
	if aid == "" {
		return mcp.MCPToolResult{OK: false, Error: "agent_id required"}
	}
	role := strArg(m, "role")
	if role == "" {
		role = "worker"
	}
	c, err := ensureCoordinator()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if err := c.Topology().AddNode(aid, role); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agent_id": aid, "role": role}}
}

func handleCoordinationConsensus(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	v := strArg(m, "value")
	if v == "" {
		v = "ok"
	}
	c, err := ensureCoordinator()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	pid, err := c.ProposeConsensus(ctx, []byte(v))
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	res, err := c.AwaitConsensus(ctx, pid, 2*time.Second)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"proposal_id": pid, "committed": res.Committed, "error": errString(err),
	}}
}

func handleCoordinationOrchestrate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	title := strArg(m, "title")
	if title == "" {
		title = "mcp-task"
	}
	c, err := ensureCoordinator()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	t := &api.TaskDefinition{
		ID:          fmt.Sprintf("coord-%d", now().UnixNano()),
		Type:        api.TaskTypeImplementation,
		Status:      api.TaskStatusPending,
		Priority:    api.TaskPriorityNormal,
		Title:       title,
		Description: "submitted via coordination_orchestrate",
		CreatedAt:   now(),
		UpdatedAt:   now(),
	}
	if err := c.SubmitTask(t); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"task_id": t.ID, "status": t.Status}}
}

func handleCoordinationMetrics(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	c, err := ensureCoordinator()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	st := c.Bus().Stats()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"messages_processed": st.MessagesProcessed,
		"messages_per_sec":   st.MessagesPerSec,
		"pool_size":          c.Pool().Size(),
	}}
}
