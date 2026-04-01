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

// 本文件：通过 pkg/swarm.UnifiedSwarmCoordinator 暴露拓扑、负载、共识与任务提交的 MCP 桥接工具。
//
// 设计思路：ensureCoordinator 使用 sync.Once 单例初始化网格拓扑+Raft 配置，并注册 bootstrap 代理；各 handler
// 在失败时返回 MCPToolResult 错误字段；共识调用 ProposeConsensus 与带超时的 AwaitConsensus。

var (
	coordOnce    sync.Once
	coordInst    *swarm.UnifiedSwarmCoordinator
	coordInitErr error
)

// ensureCoordinator 懒加载全局协调器：首次调用时构造 CoordinatorConfig、Initialize、注册 coord-bootstrap 代理；
// 返回单例与首次初始化错误（若有）。
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

// coordinationTools 注册 topology、load_balance、sync、node、consensus、orchestrate、metrics 工具。
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

// RegisterCoordinationTools 将 pkg/swarm 协调桥接相关 MCP 工具注册到 ToolRegistry。
func RegisterCoordinationTools(reg *mcp.ToolRegistry) error {
	for _, t := range coordinationTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleCoordinationTopology 确认协调器与 Topology 非 nil，返回固定 mesh 摘要（backend 标识 pkg/swarm）。
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

// handleCoordinationLoadBalance 列举池内代理，累加 Metrics.Extra["load"] 作为 aggregate_load，并返回池大小与代理数。
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

// handleCoordinationSync 确保协调器已初始化，返回占位同步成功与时间戳。
func handleCoordinationSync(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	if _, err := ensureCoordinator(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"synced": true, "at": now()}}
}

// handleCoordinationNode 向拓扑添加节点：agent_id 必填，role 默认 worker，调用 Topology().AddNode。
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

// handleCoordinationConsensus 提交 value（默认 "ok"）的字节提案，等待最多 2s，返回 proposal_id、committed 与 await 错误字符串。
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

// handleCoordinationOrchestrate 构造 TaskDefinition（默认标题 mcp-task）并 SubmitTask，返回 task_id 与初始 status。
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

// handleCoordinationMetrics 返回消息总线 Stats（处理量、每秒消息）与当前池大小。
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
