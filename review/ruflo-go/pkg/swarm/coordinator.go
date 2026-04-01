package swarm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/pkg/swarm/consensus"
)

// UnifiedSwarmCoordinator 将拓扑、消息、池化 Worker 与共识合成为单一编排入口。
type UnifiedSwarmCoordinator struct {
	mu sync.RWMutex // 保护 agents/tasks 等内存状态

	cfg CoordinatorConfig // 拓扑类型、共识算法、池上下限、心跳与指标周期

	topology *TopologyManager // 节点与边：路由与领导者选举
	bus      *MessageBus      // 每 Agent 优先级队列 + 订阅投递
	pool     *AgentPool       // 可伸缩执行体槽位（与注册 Agent 概念并行存在）
	engine   consensus.Engine // Raft/Gossip 等共识抽象

	domainConfigs []DomainConfig                 // 默认 15 Agent 域划分模板
	domainAgents  map[api.AgentDomain][]string   // 域 -> 已注册 Agent id 列表
	tasks         map[string]*api.TaskDefinition // 任务 id -> 定义
	assignments   map[string]TaskAssignment      // 任务 id -> 指派记录
	agents        map[string]*api.Agent          // Agent id -> 快照指针

	ctx    context.Context    // 协调器生命周期
	cancel context.CancelFunc // 取消后台循环
	wg     sync.WaitGroup     // 等待 health/metrics 退出

	state   CoordinatorState   // 运行态摘要
	metrics CoordinatorMetrics // 累计 KPI

	heartbeat time.Duration // 健康检查节拍
	paused    bool          // Pause 时拒绝新任务
}

// NewUnifiedSwarmCoordinator 填充 cfg 默认值，创建可取消的根 context，不自动 Initialize。
func NewUnifiedSwarmCoordinator(cfg CoordinatorConfig) *UnifiedSwarmCoordinator {
	if cfg.AgentPoolMin <= 0 {
		cfg.AgentPoolMin = 1
	}
	if cfg.AgentPoolMax <= 0 {
		cfg.AgentPoolMax = 32
	}
	if cfg.HeartbeatMS <= 0 {
		cfg.HeartbeatMS = 100
	}
	if cfg.MetricsInterval <= 0 {
		cfg.MetricsInterval = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &UnifiedSwarmCoordinator{
		cfg:           cfg,
		domainConfigs: defaultFifteenAgentDomains(),
		domainAgents:  make(map[api.AgentDomain][]string),
		tasks:         make(map[string]*api.TaskDefinition),
		assignments:   make(map[string]TaskAssignment),
		agents:        make(map[string]*api.Agent),
		ctx:           ctx,
		cancel:        cancel,
		heartbeat:     time.Duration(cfg.HeartbeatMS) * time.Millisecond,
	}
	return c
}

func defaultFifteenAgentDomains() []DomainConfig {
	return []DomainConfig{
		{Name: api.AgentDomainQueen, AgentNumbers: []int{1}, Priority: 0, Capabilities: []string{"orchestrate", "consensus"}},
		{Name: api.AgentDomainSecurity, AgentNumbers: []int{2, 3, 4}, Priority: 1, Capabilities: []string{"audit", "scan", string(api.TaskTypeSecurity)}},
		{Name: api.AgentDomainCore, AgentNumbers: []int{5, 6, 7, 8, 9}, Priority: 2, Capabilities: []string{string(api.TaskTypeCoding), string(api.TaskTypeTesting), string(api.TaskTypeReview)}},
		{Name: api.AgentDomainIntegration, AgentNumbers: []int{10, 11, 12}, Priority: 3, Capabilities: []string{string(api.TaskTypeDeployment), "bridge"}},
		{Name: api.AgentDomainSupport, AgentNumbers: []int{13, 14, 15}, Priority: 4, Capabilities: []string{string(api.TaskTypeResearch), "docs"}},
	}
}

// Initialize wires subsystems.
func (c *UnifiedSwarmCoordinator) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	topoCfg := TopologyConfig{Type: c.cfg.Topology, MaxMeshDegree: 10, HybridRandomPeers: 3}
	c.topology = NewTopologyManager(topoCfg)

	busCfg := MessageBusConfig{
		QueueCapacityPerAgent: 512,
		ProcessBatchMax:       10,
		ProcessInterval:       10 * time.Millisecond,
		SlidingWindowSecs:     5,
	}
	c.bus = NewMessageBus(busCfg)

	poolCfg := AgentPoolConfig{
		MinSize:          c.cfg.AgentPoolMin,
		MaxSize:          c.cfg.AgentPoolMax,
		HeartbeatTimeout: 30 * time.Second,
		ScaleCooldown:    10 * time.Second,
	}
	c.pool = NewAgentPool(poolCfg)
	if err := c.pool.Initialize(ctx); err != nil {
		return err
	}

	ceCfg := consensus.Config{
		Algorithm:    c.cfg.ConsensusAlgo,
		NodeID:       "coord-1",
		Peers:        nil,
		ByzantineF:   1,
		GossipFanout: 3,
		GossipTTL:    12,
	}
	eng, err := consensus.NewEngine(ceCfg)
	if err != nil {
		return err
	}
	c.engine = eng

	c.state.Status = api.SwarmStatusHealthy
	c.state.Topology = c.cfg.Topology
	c.state.StartedAt = time.Now()

	c.wg.Add(2)
	go c.healthMonitorLoop()
	go c.metricsLoop()

	return nil
}

// RegisterAgent adds an agent to topology, domain pools, and local registry.
func (c *UnifiedSwarmCoordinator) RegisterAgent(agent *api.Agent) error {
	if agent == nil || agent.ID == "" {
		return fmt.Errorf("coordinator: invalid agent")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	role := string(agent.Type)
	if agent.Type == api.AgentTypeQueen {
		role = "queen"
	}
	if err := c.topology.AddNode(agent.ID, role); err != nil {
		return err
	}

	domain := agent.Domain
	if domain == "" {
		domain = api.AgentDomainCore
	}
	c.domainAgents[domain] = append(c.domainAgents[domain], agent.ID)
	c.agents[agent.ID] = agent
	c.state.AgentCount = len(c.agents)

	return c.bus.Send(api.Message{
		To:        agent.ID,
		Type:      api.MessageTypeControl,
		Priority:  api.MessagePriorityNormal,
		Timestamp: time.Now(),
		Payload:   map[string]any{"body": "registered"},
	})
}

// RemoveAgent removes tracking and topology node.
func (c *UnifiedSwarmCoordinator) RemoveAgent(agentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.agents, agentID)
	for d, ids := range c.domainAgents {
		c.domainAgents[d] = stringSliceRemoveCopy(ids, agentID)
	}
	c.state.AgentCount = len(c.agents)
	return c.topology.RemoveNode(agentID)
}

// SubmitTask scores agents and assigns the best match.
func (c *UnifiedSwarmCoordinator) SubmitTask(task *api.TaskDefinition) error {
	if task == nil || task.ID == "" {
		return fmt.Errorf("coordinator: invalid task")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return fmt.Errorf("coordinator: paused")
	}

	task.Status = api.TaskStatusPending
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	task.UpdatedAt = time.Now()
	c.tasks[task.ID] = task
	c.state.PendingTasks = len(c.tasks)

	var bestID string
	var bestScore float64 = math.Inf(-1)

	for id, ag := range c.agents {
		sc := c.scoreAgentForTask(ag, task)
		if sc > bestScore {
			bestScore = sc
			bestID = id
		}
	}

	if bestID == "" {
		return fmt.Errorf("coordinator: no agents available")
	}

	task.AgentID = bestID
	task.Status = api.TaskStatusAssigned
	task.Domain = c.agents[bestID].Domain
	c.assignments[task.ID] = TaskAssignment{
		TaskID:   task.ID,
		AgentID:  bestID,
		Domain:   task.Domain,
		Score:    bestScore,
		Assigned: time.Now(),
	}

	c.metrics.mu.Lock()
	c.metrics.TasksSubmitted++
	c.metrics.TasksAssigned++
	c.metrics.mu.Unlock()

	return c.bus.Send(api.Message{
		To:        bestID,
		Type:      api.MessageTypeTask,
		Priority:  taskPriorityToMsg(task.Priority),
		Timestamp: time.Now(),
		Payload:   map[string]any{"task_id": task.ID},
	})
}

func taskPriorityToMsg(p api.TaskPriority) api.MessagePriority {
	if p >= api.TaskPriorityCritical {
		return api.MessagePriorityCritical
	}
	if p >= api.TaskPriorityHigh {
		return api.MessagePriorityHigh
	}
	if p <= api.TaskPriorityLow {
		return api.MessagePriorityBackground
	}
	return api.MessagePriorityNormal
}

func (c *UnifiedSwarmCoordinator) scoreAgentForTask(ag *api.Agent, task *api.TaskDefinition) float64 {
	typeMatch := 0.0
	ts := string(task.Type)
	for _, cap := range agentCapabilities(ag) {
		if cap == ts {
			typeMatch = 2
			break
		}
	}
	if typeMatch == 0 {
		switch task.Type {
		case api.TaskTypeCoding, api.TaskTypeTesting, api.TaskTypeReview:
			if ag.Domain == api.AgentDomainCore {
				typeMatch = 1
			}
		case api.TaskTypeSecurity:
			if ag.Domain == api.AgentDomainSecurity {
				typeMatch = 1
			}
		default:
			typeMatch = 0.5
		}
	}

	workload := agentLoad(ag)
	age := time.Since(ag.CreatedAt).Seconds()
	normTime := math.Min(1.0, age/3600.0)

	total := float64(ag.Metrics.TasksCompleted + ag.Metrics.TasksFailed)
	successRate := 0.5
	if total > 0 {
		successRate = float64(ag.Metrics.TasksCompleted) / total
	}

	raw := typeMatch - workload - normTime*0.3 + successRate*0.5
	return raw * agentHealth(ag)
}

// AssignTaskToDomain pins a task to a domain pool agent (least loaded).
func (c *UnifiedSwarmCoordinator) AssignTaskToDomain(taskID string, domain api.AgentDomain) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return fmt.Errorf("coordinator: paused")
	}
	task, ok := c.tasks[taskID]
	if !ok {
		return fmt.Errorf("coordinator: unknown task %s", taskID)
	}
	ids := c.domainAgents[domain]
	if len(ids) == 0 {
		return fmt.Errorf("coordinator: no agents in domain %s", domain)
	}
	sort.Strings(ids)
	var best string
	var bestLoad float64 = math.MaxFloat64
	for _, id := range ids {
		ag := c.agents[id]
		if ag == nil {
			continue
		}
		if l := agentLoad(ag); l < bestLoad {
			bestLoad = l
			best = id
		}
	}
	if best == "" {
		return fmt.Errorf("coordinator: could not pick agent in domain %s", domain)
	}
	task.AgentID = best
	task.Domain = domain
	task.Status = api.TaskStatusAssigned
	task.UpdatedAt = time.Now()
	c.assignments[task.ID] = TaskAssignment{
		TaskID:   task.ID,
		AgentID:  best,
		Domain:   domain,
		Assigned: time.Now(),
	}
	c.metrics.mu.Lock()
	c.metrics.TasksAssigned++
	c.metrics.mu.Unlock()
	return c.bus.Send(api.Message{
		To:        best,
		Type:      api.MessageTypeTask,
		Priority:  api.MessagePriorityNormal,
		Timestamp: time.Now(),
		Payload:   map[string]any{"task_id": task.ID},
	})
}

// ExecuteParallel 对每个任务启 goroutine 调用 SubmitTask，WaitGroup 收敛后返回各任务结果片段。
func (c *UnifiedSwarmCoordinator) ExecuteParallel(tasks []*api.TaskDefinition) []ParallelExecutionResult {
	results := make([]ParallelExecutionResult, len(tasks))
	var wg sync.WaitGroup
	for i, tsk := range tasks {
		wg.Add(1)
		go func(idx int, t *api.TaskDefinition) {
			defer wg.Done()
			start := time.Now()
			err := c.SubmitTask(t)
			results[idx] = ParallelExecutionResult{
				TaskID:    t.ID,
				AgentID:   t.AgentID,
				Success:   err == nil,
				Err:       err,
				Duration:  time.Since(start),
				StartedAt: start,
			}
		}(i, tsk)
	}
	wg.Wait()
	return results
}

// healthMonitorLoop 周期 tickHealth：心跳超时则衰减健康并在过低时尝试“恢复”计数。
func (c *UnifiedSwarmCoordinator) healthMonitorLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.tickHealth()
		}
	}
}

// tickHealth 扫描所有 Agent 上次心跳，触发池 CheckScaling。
func (c *UnifiedSwarmCoordinator) tickHealth() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, ag := range c.agents {
		if now.Sub(ag.Metrics.LastHeartbeat) > 5*c.heartbeat {
			h := agentHealth(ag)
			if h > 0 {
				h -= 0.02
				setAgentHealth(ag, h)
			}
			if agentHealth(ag) < 0.4 {
				ag.Status = api.AgentStatusUnhealthy
				c.metrics.mu.Lock()
				c.metrics.HealthRecoveries++
				c.metrics.mu.Unlock()
				setAgentHealth(ag, 1)
				ag.Status = api.AgentStatusHealthy
				ag.State = api.AgentStateIdle
				ag.Metrics.LastHeartbeat = now
				ag.UpdatedAt = now
			}
		}
	}
	if c.pool != nil {
		c.pool.CheckScaling()
	}
}

func (c *UnifiedSwarmCoordinator) metricsLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.MetricsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.metrics.mu.Lock()
			c.metrics.UpdatedAt = time.Now()
			c.metrics.mu.Unlock()
		}
	}
}

// ProposeConsensus submits a value to the consensus engine.
func (c *UnifiedSwarmCoordinator) ProposeConsensus(ctx context.Context, value []byte) (string, error) {
	c.mu.RLock()
	eng := c.engine
	c.mu.RUnlock()
	if eng == nil {
		return "", fmt.Errorf("coordinator: not initialized")
	}
	id, err := eng.Propose(ctx, value)
	if err != nil {
		return "", err
	}
	c.metrics.mu.Lock()
	c.metrics.ConsensusProposals++
	c.metrics.mu.Unlock()
	return id, nil
}

// AwaitConsensus 阻塞等待提案提交结果并映射为 Coordinator 使用的 ConsensusResult。
func (c *UnifiedSwarmCoordinator) AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (ConsensusResult, error) {
	c.mu.RLock()
	eng := c.engine
	c.mu.RUnlock()
	if eng == nil {
		return ConsensusResult{}, fmt.Errorf("coordinator: not initialized")
	}
	r, err := eng.AwaitConsensus(ctx, proposalID, timeout)
	return ConsensusResult{
		ProposalID: r.ProposalID,
		Committed:  r.Committed,
		Value:      r.Value,
		Term:       r.Term,
		Err:        r.Err,
		FinishedAt: r.FinishedAt,
	}, err
}

// DefaultDomainConfigs 返回内置 15 Agent 域切片副本引用（与内部 domainConfigs 同源）。
func (c *UnifiedSwarmCoordinator) DefaultDomainConfigs() []DomainConfig {
	return c.domainConfigs
}

// Shutdown cancel 上下文、等待协程、关闭总线/池/共识引擎。
func (c *UnifiedSwarmCoordinator) Shutdown(ctx context.Context) error {
	c.cancel()
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bus != nil {
		c.bus.Shutdown()
	}
	if c.pool != nil {
		c.pool.Shutdown()
	}
	if c.engine != nil {
		_ = c.engine.Close()
	}
	c.state.Status = api.SwarmStatusShuttingDown
	return nil
}

// Topology 返回拓扑管理器指针（调用方勿并发修改内部状态，应通过其方法访问）。
func (c *UnifiedSwarmCoordinator) Topology() *TopologyManager {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.topology
}

// Bus 返回消息总线实例。
func (c *UnifiedSwarmCoordinator) Bus() *MessageBus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bus
}

// Pool 返回 Agent 池实例。
func (c *UnifiedSwarmCoordinator) Pool() *AgentPool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pool
}

// Pause 设置 draining 状态并阻止 SubmitTask/AssignTaskToDomain。
func (c *UnifiedSwarmCoordinator) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
	c.state.Status = api.SwarmStatusDraining
}

// Resume 清除暂停标志并在原 draining 时恢复健康状态枚举。
func (c *UnifiedSwarmCoordinator) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
	if c.state.Status == api.SwarmStatusDraining {
		c.state.Status = api.SwarmStatusHealthy
	}
}

// GetAgent 返回 Agent 值拷贝，避免外部长期持有内部指针。
func (c *UnifiedSwarmCoordinator) GetAgent(id string) (*api.Agent, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.agents[id]
	if !ok || a == nil {
		return nil, false
	}
	cp := *a
	return &cp, true
}

// GetAllAgents 返回当前注册 Agent 的浅拷贝切片。
func (c *UnifiedSwarmCoordinator) GetAllAgents() []*api.Agent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*api.Agent, 0, len(c.agents))
	for _, a := range c.agents {
		if a == nil {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// GetAgentsByType 过滤 Type 相等的 Agent。
func (c *UnifiedSwarmCoordinator) GetAgentsByType(agentType api.AgentType) []*api.Agent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*api.Agent
	for _, a := range c.agents {
		if a == nil || a.Type != agentType {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// GetAvailableAgents 仅 State==Idle 的 Agent。
func (c *UnifiedSwarmCoordinator) GetAvailableAgents() []*api.Agent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*api.Agent
	for _, a := range c.agents {
		if a == nil || a.State != api.AgentStateIdle {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// CancelTask 将任务标为取消并删除 assignments 项。
func (c *UnifiedSwarmCoordinator) CancelTask(taskID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	task, ok := c.tasks[taskID]
	if !ok {
		return fmt.Errorf("coordinator: unknown task %s", taskID)
	}
	task.Status = api.TaskStatusCancelled
	task.UpdatedAt = time.Now()
	delete(c.assignments, taskID)
	c.state.PendingTasks = len(c.tasks)
	return nil
}

// GetTask 返回值拷贝的任务定义。
func (c *UnifiedSwarmCoordinator) GetTask(taskID string) (*api.TaskDefinition, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tasks[taskID]
	if !ok || t == nil {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// BroadcastMessage 为每个已注册 Agent 克隆消息（新 ID）并 bus.Send，id 排序保证顺序稳定。
func (c *UnifiedSwarmCoordinator) BroadcastMessage(msg *api.Message) error {
	if msg == nil {
		return fmt.Errorf("coordinator: nil message")
	}
	c.mu.RLock()
	ids := make([]string, 0, len(c.agents))
	for id := range c.agents {
		ids = append(ids, id)
	}
	bus := c.bus
	c.mu.RUnlock()
	if bus == nil {
		return fmt.Errorf("coordinator: not initialized")
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := *msg
		m.To = id
		m.ID = fmt.Sprintf("bc-%s-%d", id, time.Now().UnixNano())
		if m.Timestamp.IsZero() {
			m.Timestamp = time.Now()
		}
		if err := bus.Send(m); err != nil {
			return err
		}
	}
	return nil
}

// GetState 拷贝轻量状态字段（不含内部互斥量语义）。
func (c *UnifiedSwarmCoordinator) GetState() CoordinatorState {
	c.mu.RLock()
	st := c.state.Status
	top := c.state.Topology
	ac := c.state.AgentCount
	pt := c.state.PendingTasks
	le := c.state.LastElection
	sa := c.state.StartedAt
	c.mu.RUnlock()
	return CoordinatorState{
		Status:       st,
		Topology:     top,
		AgentCount:   ac,
		PendingTasks: pt,
		LastElection: le,
		StartedAt:    sa,
	}
}

// GetMetrics 在锁内复制计数器快照。
func (c *UnifiedSwarmCoordinator) GetMetrics() CoordinatorMetrics {
	c.metrics.mu.Lock()
	defer c.metrics.mu.Unlock()
	return CoordinatorMetrics{
		TasksSubmitted:     c.metrics.TasksSubmitted,
		TasksAssigned:      c.metrics.TasksAssigned,
		TasksCompleted:     c.metrics.TasksCompleted,
		ConsensusProposals: c.metrics.ConsensusProposals,
		HealthRecoveries:   c.metrics.HealthRecoveries,
		MessagesProcessed:  c.metrics.MessagesProcessed,
		UpdatedAt:          c.metrics.UpdatedAt,
	}
}

// IsHealthy 非 Pause 且状态为 Healthy 或 Active 时视为可用。
func (c *UnifiedSwarmCoordinator) IsHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.paused {
		return false
	}
	return c.state.Status == api.SwarmStatusHealthy || c.state.Status == api.SwarmStatusActive
}

// GetAgentsByDomain 过滤 Domain 字段。
func (c *UnifiedSwarmCoordinator) GetAgentsByDomain(domain api.AgentDomain) []*api.Agent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*api.Agent
	for _, a := range c.agents {
		if a == nil || a.Domain != domain {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// stringSliceRemoveCopy 返回去除 x 后的新切片，全删尽则 nil。
func stringSliceRemoveCopy(ss []string, x string) []string {
	var out []string
	for _, s := range ss {
		if s != x {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
