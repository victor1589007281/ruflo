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

// UnifiedSwarmCoordinator consolidates topology, bus, pool, and consensus.
type UnifiedSwarmCoordinator struct {
	mu sync.RWMutex

	cfg CoordinatorConfig

	topology *TopologyManager
	bus      *MessageBus
	pool     *AgentPool
	engine   consensus.Engine

	domainConfigs []DomainConfig
	domainAgents  map[api.AgentDomain][]string
	tasks         map[string]*api.TaskDefinition
	assignments   map[string]TaskAssignment
	agents        map[string]*api.Agent

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	state   CoordinatorState
	metrics CoordinatorMetrics

	heartbeat time.Duration
	paused    bool
}

// NewUnifiedSwarmCoordinator builds a coordinator with defaults.
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

// ExecuteParallel runs multiple task definitions concurrently.
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

// AwaitConsensus waits for a proposal outcome.
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

// DefaultDomainConfigs returns the 15-agent layout.
func (c *UnifiedSwarmCoordinator) DefaultDomainConfigs() []DomainConfig {
	return c.domainConfigs
}

// Shutdown stops background work and closes resources.
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

// Topology exposes the topology manager.
func (c *UnifiedSwarmCoordinator) Topology() *TopologyManager {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.topology
}

// Bus exposes the message bus.
func (c *UnifiedSwarmCoordinator) Bus() *MessageBus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bus
}

// Pool exposes the agent pool.
func (c *UnifiedSwarmCoordinator) Pool() *AgentPool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pool
}

// Pause stops new task submissions until Resume.
func (c *UnifiedSwarmCoordinator) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
	c.state.Status = api.SwarmStatusDraining
}

// Resume allows task submissions again.
func (c *UnifiedSwarmCoordinator) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
	if c.state.Status == api.SwarmStatusDraining {
		c.state.Status = api.SwarmStatusHealthy
	}
}

// GetAgent returns a registered agent by id.
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

// GetAllAgents returns a snapshot of all registered agents.
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

// GetAgentsByType returns agents matching the given role type.
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

// GetAvailableAgents returns agents in idle (assignable) state.
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

// CancelTask marks a task cancelled and clears its assignment.
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

// GetTask returns a task definition by id.
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

// BroadcastMessage delivers a copy of msg to every registered agent queue.
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

// GetState returns a snapshot of coordinator state (mutex field is unused in the copy).
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

// GetMetrics returns a point-in-time metrics snapshot.
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

// IsHealthy reports whether the swarm is considered operational.
func (c *UnifiedSwarmCoordinator) IsHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.paused {
		return false
	}
	return c.state.Status == api.SwarmStatusHealthy || c.state.Status == api.SwarmStatusActive
}

// GetAgentsByDomain returns agents in the given domain.
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
