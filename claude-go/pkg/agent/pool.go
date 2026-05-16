// Agent Pool — 动态 Agent 池, 支持并发限制、生命周期追踪和弹性扩缩。
//
// 设计参考:
//   - ruflo v3 Agent Pool (进程内 goroutine 池)
//   - Kimi K2.5 Sub-Agent 实例化与回收 (PARL)
//   - 业界 Worker Pool 模式 (bounded concurrency + lifecycle)
//
// 核心能力:
//   1. 信号量控制最大并发数 (防止资源耗尽)
//   2. 动态扩缩: 根据负载调整池大小
//   3. Agent 生命周期追踪: 创建、执行、完成、回收
//   4. 统计信息: 活跃数、已完成数、历史延迟
//
//	┌─────────────────────────────────────────────┐
//	│ AgentPool                                   │
//	│  semaphore chan (控制并发上限)               │
//	│  agents map (追踪所有活跃 agent)            │
//	│  factory CreateAgentFunc (创建执行器)        │
//	│  Scale() → 动态调整池大小                   │
//	│  Acquire() → 获取执行槽位 + 创建 Runner     │
//	│  Release() → 归还槽位 + 记录统计            │
//	└─────────────────────────────────────────────┘
package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
)

// PooledAgent 池化 Agent 实例, 追踪生命周期。
type PooledAgent struct {
	ID        string
	Role      string
	Runner    AgentRunner
	CreatedAt time.Time
	LastUsed  time.Time
	TasksDone int
	Status    string // "busy", "idle", "failed"
}

// PoolStats Agent 池统计信息。
type PoolStats struct {
	MaxSize     int   `json:"maxSize"`
	ActiveCount int   `json:"activeCount"`
	TotalSpawns int64 `json:"totalSpawns"`
	TotalDone   int64 `json:"totalDone"`
	TotalFailed int64 `json:"totalFailed"`
}

// RoleQuota 单角色的并发配额 (参考: K8s ResourceQuota per-namespace)
type RoleQuota struct {
	Max     int `json:"max"`
	Current int `json:"current"`
}

// AgentPool 动态 Agent 池, 支持全局限流 + 按角色配额。
// 参考: Kubernetes HPA (Horizontal Pod Autoscaler) 按 deployment 独立扩缩
type AgentPool struct {
	factory   CreateAgentFunc
	semaphore chan struct{}
	agents    map[string]*PooledAgent
	mu        sync.Mutex
	maxSize   int
	nextID    int64

	roleQuotas map[string]*RoleQuota // 按角色的动态配额

	totalSpawns int64
	totalDone   int64
	totalFailed int64

	// AutoScale 防震荡: 记录上次扩缩时间，避免频繁波动
	lastScaleAt   time.Time
	scaleCooldown time.Duration // 最小扩缩间隔

	// OnRelease Agent 释放回调 (用于触发 TeammateIdle Hook 等)。
	OnRelease func(*PooledAgent)
}

// poolMaxCap 池信号量的固定容量上限 — 预分配后不再替换 channel，消除 Scale 竞态。
const poolMaxCap = 32

// NewAgentPool 创建 Agent 池。
// maxSize 控制最大并发 Agent 数 (推荐 4-16)。
func NewAgentPool(factory CreateAgentFunc, maxSize int) *AgentPool {
	if maxSize <= 0 {
		maxSize = 8
	}
	if maxSize > poolMaxCap {
		maxSize = poolMaxCap
	}
	return &AgentPool{
		factory:       factory,
		semaphore:     make(chan struct{}, poolMaxCap),
		agents:        make(map[string]*PooledAgent),
		roleQuotas:    make(map[string]*RoleQuota),
		maxSize:       maxSize,
		scaleCooldown: 30 * time.Second, // 最小扩缩间隔, 防止频繁震荡
	}
}

// Acquire 从池中获取一个执行槽位并创建 Agent。
// 通过自旋检测 maxSize 限制并发，不替换 channel。
func (p *AgentPool) Acquire(ctx context.Context, role, systemPrompt string) (*PooledAgent, error) {
	for {
		p.mu.Lock()
		active := len(p.agents)
		limit := p.maxSize
		p.mu.Unlock()
		if active < limit {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("agent pool: 等待槽位超时 (%v)", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	select {
	case p.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("agent pool: 等待槽位超时 (%v)", ctx.Err())
	}

	runner, err := p.factory(ctx, role, systemPrompt)
	if err != nil {
		<-p.semaphore
		atomic.AddInt64(&p.totalFailed, 1)
		return nil, fmt.Errorf("agent pool: 创建 %s agent 失败: %w", role, err)
	}

	id := fmt.Sprintf("agent-%d-%d", atomic.AddInt64(&p.nextID, 1), time.Now().UnixMilli()%10000)
	agent := &PooledAgent{
		ID:        id,
		Role:      role,
		Runner:    runner,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
		Status:    "busy",
	}

	p.mu.Lock()
	p.agents[id] = agent
	p.mu.Unlock()
	atomic.AddInt64(&p.totalSpawns, 1)

	return agent, nil
}

// Release 归还 Agent 到池中, 释放槽位。
func (p *AgentPool) Release(agent *PooledAgent) {
	if agent == nil {
		return
	}

	p.mu.Lock()
	agent.Status = "idle"
	agent.LastUsed = time.Now()
	agent.TasksDone++
	delete(p.agents, agent.ID)
	p.mu.Unlock()

	if p.OnRelease != nil {
		p.OnRelease(agent)
	}

	atomic.AddInt64(&p.totalDone, 1)
	<-p.semaphore
}

// MarkFailed 标记 Agent 失败并释放槽位。
func (p *AgentPool) MarkFailed(agent *PooledAgent) {
	if agent == nil {
		return
	}
	p.mu.Lock()
	agent.Status = "failed"
	delete(p.agents, agent.ID)
	p.mu.Unlock()

	atomic.AddInt64(&p.totalFailed, 1)
	<-p.semaphore
}

// Scale 动态调整逻辑池大小 (不替换 channel，避免竞态)。
// 缩容时已有 agent 自然完成后受新限制约束。
func (p *AgentPool) Scale(newMax int) {
	if newMax <= 0 || newMax == p.maxSize {
		return
	}
	if newMax > poolMaxCap {
		newMax = poolMaxCap
	}
	p.mu.Lock()
	p.maxSize = newMax
	p.mu.Unlock()
}

// ActiveCount 返回当前活跃 Agent 数。
func (p *AgentPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.agents)
}

// AutoScale 根据待执行任务数自动调整池大小。
// 策略: 池大小 = clamp(pendingTasks + 2, minSize, maxCap)
// 防震荡: 30s 冷却期 + 渐进式调整 (每次最多 ±50%)
func (p *AgentPool) AutoScale(pendingTasks int) {
	p.mu.Lock()
	currentActive := len(p.agents)
	currentMax := p.maxSize
	// 冷却期检查: 如果距上次扩缩不足 30s，跳过本次调整
	if time.Since(p.lastScaleAt) < p.scaleCooldown && p.lastScaleAt.IsZero() == false {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	const minSize = 4
	const maxCap = 32

	desired := pendingTasks + 2
	if desired < minSize {
		desired = minSize
	}
	if desired > maxCap {
		desired = maxCap
	}

	// 渐进式调整: 每次最多变化 50%，避免骤增骤降
	maxStep := currentMax / 2
	if maxStep < 2 {
		maxStep = 2
	}
	diff := desired - currentMax
	if diff > maxStep {
		diff = maxStep
	} else if diff < -maxStep {
		diff = -maxStep
	}
	adjusted := currentMax + diff

	if adjusted != currentMax {
		p.mu.Lock()
		p.lastScaleAt = time.Now()
		p.mu.Unlock()
		logging.For("agent-pool").Info("AutoScale", "active", currentActive, "pending", pendingTasks, "old", currentMax, "new", adjusted)
		p.Scale(adjusted)
	}
}

// AutoScaleByRoles 按角色的任务需求动态扩缩池。
// 参考: K8s HPA 按 Deployment 独立扩缩。每个角色根据其复杂度系数获得配额。
// roleNeeds: map[role]taskCount, complexity: 0=simple, 1=moderate, 2=complex
// 防震荡: 30s 冷却期 + 渐进式调整
func (p *AgentPool) AutoScaleByRoles(roleNeeds map[string]int, complexity int) {
	p.mu.Lock()
	// 冷却期检查
	if time.Since(p.lastScaleAt) < p.scaleCooldown && !p.lastScaleAt.IsZero() {
		p.mu.Unlock()
		return
	}
	defer p.mu.Unlock()

	// 复杂度乘数: 简单任务1倍, 中等2倍, 复杂3倍
	multiplier := 1
	switch {
	case complexity >= 2:
		multiplier = 3
	case complexity >= 1:
		multiplier = 2
	}

	totalNeeded := 0
	for role, count := range roleNeeds {
		// 每个角色的配额 = 任务数 × 复杂度乘数, 至少1
		quota := count * multiplier
		if quota < 1 {
			quota = 1
		}
		if quota > 8 {
			quota = 8
		}
		if p.roleQuotas[role] == nil {
			p.roleQuotas[role] = &RoleQuota{}
		}
		p.roleQuotas[role].Max = quota
		totalNeeded += quota
	}

	// 全局池大小 = 所有角色配额之和, 但不超过硬上限
	if totalNeeded < 4 {
		totalNeeded = 4
	}
	if totalNeeded > poolMaxCap {
		totalNeeded = poolMaxCap
	}

	if totalNeeded != p.maxSize {
		// 渐进式调整: 每次最多变化 50%
		currentMax := p.maxSize
		maxStep := currentMax / 2
		if maxStep < 3 {
			maxStep = 3
		}
		diff := totalNeeded - currentMax
		if diff > maxStep {
			diff = maxStep
		} else if diff < -maxStep {
			diff = -maxStep
		}
		adjusted := currentMax + diff

		p.lastScaleAt = time.Now()
		logging.For("agent-pool").Info("AutoScaleByRoles",
			"roles", len(roleNeeds), "complexity", complexity,
			"multiplier", multiplier, "old", p.maxSize, "desired", totalNeeded, "new", adjusted)
		p.maxSize = adjusted
	}
}

// RoleActiveCount 返回指定角色当前的活跃 agent 数。
func (p *AgentPool) RoleActiveCount(role string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, ag := range p.agents {
		if ag.Role == role {
			count++
		}
	}
	return count
}

// RoleQuotas 返回当前各角色的配额快照。
func (p *AgentPool) RoleQuotas() map[string]RoleQuota {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make(map[string]RoleQuota, len(p.roleQuotas))
	for role, q := range p.roleQuotas {
		current := 0
		for _, ag := range p.agents {
			if ag.Role == role {
				current++
			}
		}
		result[role] = RoleQuota{Max: q.Max, Current: current}
	}
	return result
}

// Stats 返回池统计。
func (p *AgentPool) Stats() PoolStats {
	return PoolStats{
		MaxSize:     p.maxSize,
		ActiveCount: p.ActiveCount(),
		TotalSpawns: atomic.LoadInt64(&p.totalSpawns),
		TotalDone:   atomic.LoadInt64(&p.totalDone),
		TotalFailed: atomic.LoadInt64(&p.totalFailed),
	}
}
