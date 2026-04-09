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
	"log"
	"sync"
	"sync/atomic"
	"time"
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

// AgentPool 动态 Agent 池。
type AgentPool struct {
	factory   CreateAgentFunc
	semaphore chan struct{}
	agents    map[string]*PooledAgent
	mu        sync.Mutex
	maxSize   int
	nextID    int64

	totalSpawns int64
	totalDone   int64
	totalFailed int64
}

// NewAgentPool 创建 Agent 池。
// maxSize 控制最大并发 Agent 数 (推荐 4-12)。
func NewAgentPool(factory CreateAgentFunc, maxSize int) *AgentPool {
	if maxSize <= 0 {
		maxSize = 8
	}
	return &AgentPool{
		factory:   factory,
		semaphore: make(chan struct{}, maxSize),
		agents:    make(map[string]*PooledAgent),
		maxSize:   maxSize,
	}
}

// Acquire 从池中获取一个执行槽位并创建 Agent。
// 如果池已满则阻塞等待, 或在 ctx 取消时返回错误。
func (p *AgentPool) Acquire(ctx context.Context, role, systemPrompt string) (*PooledAgent, error) {
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

// Scale 动态调整池大小。
// 新大小立即生效: 缩小时等待多余 Agent 自然完成, 扩大时立即可用。
func (p *AgentPool) Scale(newMax int) {
	if newMax <= 0 || newMax == p.maxSize {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	oldSem := p.semaphore
	p.semaphore = make(chan struct{}, newMax)

	// 迁移已占用的槽位
	activeCount := len(p.agents)
	for i := 0; i < activeCount && i < newMax; i++ {
		p.semaphore <- struct{}{}
	}

	p.maxSize = newMax

	// 释放旧信号量中的空闲槽位
	for i := 0; i < cap(oldSem)-activeCount; i++ {
		select {
		case <-oldSem:
		default:
		}
	}
}

// ActiveCount 返回当前活跃 Agent 数。
func (p *AgentPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.agents)
}

// AutoScale 根据待执行任务数自动调整池大小。
// 策略: 池大小 = clamp(pendingTasks + 2, minSize, maxCap)
// 确保有足够并发槽位，同时不过度分配。
func (p *AgentPool) AutoScale(pendingTasks int) {
	p.mu.Lock()
	currentActive := len(p.agents)
	currentMax := p.maxSize
	p.mu.Unlock()

	const minSize = 4
	const maxCap = 16

	desired := pendingTasks + 2 // 额外预留 2 个缓冲槽位
	if desired < minSize {
		desired = minSize
	}
	if desired > maxCap {
		desired = maxCap
	}

	if desired != currentMax {
		log.Printf("[AgentPool] AutoScale: active=%d, pending=%d, %d → %d",
			currentActive, pendingTasks, currentMax, desired)
		p.Scale(desired)
	}
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
