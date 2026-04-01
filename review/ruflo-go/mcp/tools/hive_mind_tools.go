package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/swarm"
	"github.com/ruflo/ruflo-go/pkg/swarm/consensus"
)

// 本文件：Hive Mind（蜂群心智）协调 MCP 工具，在内存+JSON 中模拟女王、工人、广播与共识记录。
//
// 设计思路：hiveData 为单例状态，hiveLoad/hiveSave 与 hive-mind/state.json 同步；共识 votes 为工人数+1 的占位统计；
// memory 为可选键值共享区；shutdown 仅将 Active 置 false。

type hiveMindState struct {
	QueenID   string            `json:"queen_id"`
	Workers   []string          `json:"workers"`
	Messages  []map[string]any  `json:"messages"`
	Consensus []map[string]any  `json:"consensus"`
	Memory    map[string]string `json:"memory,omitempty"`
	Active    bool              `json:"active"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// hiveMindState 描述女王 ID、工人列表、广播消息、共识事件、可选共享内存与活动标志。

var (
	hiveMu   sync.Mutex
	hiveData = &hiveMindState{}

	hiveRuntimeMu    sync.Mutex
	hiveRuntimeCoord *swarm.UnifiedSwarmCoordinator // Broadcast / RegisterAgent / RemoveAgent
)

func teardownHiveMindRuntime(ctx context.Context) error {
	hiveRuntimeMu.Lock()
	hc := hiveRuntimeCoord
	hiveRuntimeCoord = nil
	hiveRuntimeMu.Unlock()

	var shutdownErr error
	if hc != nil {
		shutdownErr = hc.Shutdown(ctx)
	}

	globalState.mu.Lock()
	q := globalState.queen
	eng := globalState.consensusEngine
	globalState.queen = nil
	globalState.consensusEngine = nil
	globalState.mu.Unlock()

	if q != nil {
		_ = q.Shutdown()
	}
	if eng != nil {
		if err := eng.Close(); shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}

func initHiveMindRuntime(ctx context.Context, queenID string) error {
	q := swarm.NewQueenCoordinator(nil)
	if err := q.Initialize(ctx); err != nil {
		return err
	}
	eng, err := consensus.NewEngine(consensus.Config{
		Algorithm:    api.ConsensusRaft,
		NodeID:       "hive-consensus-" + queenID,
		ByzantineF:   1,
		GossipFanout: 3,
		GossipTTL:    12,
	})
	if err != nil {
		_ = q.Shutdown()
		return err
	}
	c := swarm.NewUnifiedSwarmCoordinator(swarm.CoordinatorConfig{
		Topology:        api.TopologyStar,
		ConsensusAlgo:   api.ConsensusRaft,
		AgentPoolMin:    1,
		AgentPoolMax:    128,
		HeartbeatMS:     500,
		MetricsInterval: time.Second,
	})
	if err = c.Initialize(ctx); err != nil {
		_ = eng.Close()
		_ = q.Shutdown()
		return err
	}
	queenAg := &api.Agent{
		ID: queenID, Name: "queen", Type: api.AgentTypeQueen,
		Domain: api.AgentDomainQueen, State: api.AgentStateIdle, Status: api.AgentStatusHealthy,
		CreatedAt: now(), UpdatedAt: now(),
	}
	q.RegisterAgent(queenAg)
	if err = c.RegisterAgent(queenAg); err != nil {
		_ = c.Shutdown(ctx)
		_ = eng.Close()
		_ = q.Shutdown()
		return err
	}
	globalState.mu.Lock()
	globalState.queen = q
	globalState.consensusEngine = eng
	globalState.mu.Unlock()
	hiveRuntimeMu.Lock()
	hiveRuntimeCoord = c
	hiveRuntimeMu.Unlock()
	return nil
}

func hiveWorkerAgent(id string) *api.Agent {
	return &api.Agent{
		ID: id, Name: id, Type: api.AgentTypeCoder,
		Domain: api.AgentDomainCore, State: api.AgentStateIdle, Status: api.AgentStatusHealthy,
		CreatedAt: now(), UpdatedAt: now(),
	}
}

// hiveStatePath 返回 hive-mind/state.json 路径。
func hiveStatePath() string {
	return filepath.Join(resolveDataDir(), "hive-mind", "state.json")
}

// hiveLoad 从磁盘反序列化覆盖 hiveData，失败则保持默认零值。
func hiveLoad() {
	hiveMu.Lock()
	defer hiveMu.Unlock()
	b, err := os.ReadFile(hiveStatePath())
	if err != nil {
		return
	}
	var s hiveMindState
	if json.Unmarshal(b, &s) == nil {
		hiveData = &s
	}
}

// hiveSave 更新 UpdatedAt 后整结构写入 hiveStatePath。
func hiveSave() error {
	hiveMu.Lock()
	hiveData.UpdatedAt = now()
	cp := *hiveData
	hiveMu.Unlock()
	return writeJSONFile(hiveStatePath(), cp)
}

// hiveTools 先 hiveLoad，再注册 init/spawn/broadcast/status/consensus/join/leave/memory/shutdown。
func hiveTools() []*mcp.MCPTool {
	hiveLoad()
	return []*mcp.MCPTool{
		{Name: "hive-mind_init", Description: "Initialize hive mind with queen", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"queen_id": map[string]any{"type": "string"}}, "required": []string{"queen_id"}}, Handler: toolHandler(handleHiveMindInit)},
		{Name: "hive-mind_spawn", Description: "Spawn hive worker", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker_id": map[string]any{"type": "string"}}, "required": []string{"worker_id"}}, Handler: toolHandler(handleHiveMindSpawn)},
		{Name: "hive-mind_broadcast", Description: "Broadcast message to all workers", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}, "required": []string{"message"}}, Handler: toolHandler(handleHiveMindBroadcast)},
		{Name: "hive-mind_status", Description: "Get hive mind status", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleHiveMindStatus)},
		{Name: "hive-mind_consensus", Description: "Trigger consensus vote", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHiveMindConsensus)},
		{Name: "hive-mind_join", Description: "Join hive as worker", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker_id": map[string]any{"type": "string"}}, "required": []string{"worker_id"}}, Handler: toolHandler(handleHiveMindJoin)},
		{Name: "hive-mind_leave", Description: "Leave hive worker role", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker_id": map[string]any{"type": "string"}}, "required": []string{"worker_id"}}, Handler: toolHandler(handleHiveMindLeave)},
		{Name: "hive-mind_memory", Description: "Store or read hive shared memory key", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHiveMindMemory)},
		{Name: "hive-mind_shutdown", Description: "Shutdown hive mind", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleHiveMindShutdown)},
	}
}

// RegisterHiveMindTools 注册 Hive Mind 相关 MCP 工具。
func RegisterHiveMindTools(reg *mcp.ToolRegistry) error {
	for _, t := range hiveTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleHiveMindInit 设置 queen_id，清空工人/消息/共识列表，Active=true，持久化。
func handleHiveMindInit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	q := strArg(m, "queen_id")
	if q == "" {
		return mcp.MCPToolResult{OK: false, Error: "queen_id required"}
	}
	hiveMu.Lock()
	hiveData.QueenID = q
	hiveData.Workers = nil
	hiveData.Messages = nil
	hiveData.Consensus = nil
	hiveData.Active = true
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"queen_id": q, "active": true}}
}

// handleHiveMindSpawn 将 worker_id 去重加入 Workers，返回当前工人数。
func handleHiveMindSpawn(_ context.Context, m map[string]any) mcp.MCPToolResult {
	w := strArg(m, "worker_id")
	if w == "" {
		return mcp.MCPToolResult{OK: false, Error: "worker_id required"}
	}
	hiveMu.Lock()
	found := false
	for _, x := range hiveData.Workers {
		if x == w {
			found = true
			break
		}
	}
	if !found {
		hiveData.Workers = append(hiveData.Workers, w)
	}
	n := len(hiveData.Workers)
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"worker_id": w, "workers": n}}
}

func handleHiveMindBroadcast(_ context.Context, m map[string]any) mcp.MCPToolResult {
	msg := strArg(m, "message")
	if msg == "" {
		return mcp.MCPToolResult{OK: false, Error: "message required"}
	}
	hiveMu.Lock()
	if hiveData.Messages == nil {
		hiveData.Messages = make([]map[string]any, 0)
	}
	hiveData.Messages = append(hiveData.Messages, map[string]any{"text": msg, "at": now()})
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"broadcast": true, "message": msg}}
}

// handleHiveMindStatus 返回女王、工人副本、计数、Active、消息条数与共识事件数。
func handleHiveMindStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	hiveMu.Lock()
	cp := *hiveData
	w := append([]string(nil), cp.Workers...)
	hiveMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"queen_id": cp.QueenID, "workers": w, "worker_count": len(w),
		"active": cp.Active, "messages": len(cp.Messages), "consensus_events": len(cp.Consensus),
	}}
}

// handleHiveMindConsensus 调用独立 consensus.Engine 的 Propose + AwaitConsensus，并写入 JSON 共识轨迹。
func handleHiveMindConsensus(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	topic := strArg(m, "topic")
	if topic == "" {
		topic = "default"
	}
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	globalState.mu.RLock()
	eng := globalState.consensusEngine
	globalState.mu.RUnlock()
	if eng == nil {
		return mcp.MCPToolResult{OK: false, Error: "hive mind not initialized"}
	}
	pid, err := eng.Propose(rctx, []byte(topic))
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	res, awaitErr := eng.AwaitConsensus(rctx, pid, 2*time.Second)
	rec := map[string]any{
		"topic": topic, "proposal_id": pid, "committed": res.Committed, "term": res.Term,
		"await_error": errString(awaitErr), "consensus_err": errString(res.Err), "at": now(),
	}
	hiveMu.Lock()
	if hiveData.Consensus == nil {
		hiveData.Consensus = make([]map[string]any, 0)
	}
	hiveData.Consensus = append(hiveData.Consensus, rec)
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: rec}
}

// handleHiveMindJoin 语义同 handleHiveMindSpawn，作为「加入蜂群」别名。
func handleHiveMindJoin(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	return handleHiveMindSpawn(ctx, m)
}

// handleHiveMindLeave 从 Workers 中移除指定 worker_id（可不存在），并从 hive 协调器拓扑中 RemoveAgent。
func handleHiveMindLeave(_ context.Context, m map[string]any) mcp.MCPToolResult {
	w := strArg(m, "worker_id")
	hiveRuntimeMu.Lock()
	hc := hiveRuntimeCoord
	hiveRuntimeMu.Unlock()
	if hc != nil && w != "" {
		_ = hc.RemoveAgent(w)
	}
	hiveMu.Lock()
	out := hiveData.Workers[:0]
	for _, x := range hiveData.Workers {
		if x != w {
			out = append(out, x)
		}
	}
	hiveData.Workers = out
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"worker_id": w, "remaining": len(out)}}
}

// handleHiveMindMemory 必填 key；若入参含 value 则写入共享 Memory 并落盘，始终返回当前 key 对应 value。
func handleHiveMindMemory(_ context.Context, m map[string]any) mcp.MCPToolResult {
	k := strArg(m, "key")
	if k == "" {
		return mcp.MCPToolResult{OK: false, Error: "key required"}
	}
	hiveMu.Lock()
	if hiveData.Memory == nil {
		hiveData.Memory = map[string]string{}
	}
	wrote := false
	if _, ok := m["value"]; ok {
		hiveData.Memory[k] = strArg(m, "value")
		wrote = true
	}
	val := hiveData.Memory[k]
	hiveMu.Unlock()
	if wrote {
		if err := hiveSave(); err != nil {
			return mcp.MCPToolResult{OK: false, Error: err.Error()}
		}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": k, "value": val}}
}

// handleHiveMindShutdown 关闭 Queen、共识引擎与 hive 侧协调器，并将 Active 置为 false 后持久化。
func handleHiveMindShutdown(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := teardownHiveMindRuntime(ctx); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	hiveMu.Lock()
	hiveData.Active = false
	hiveMu.Unlock()
	if err := hiveSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"active": false}}
}
