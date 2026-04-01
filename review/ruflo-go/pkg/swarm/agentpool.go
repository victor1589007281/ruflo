// Agent 池（swarm 包）：类比连接池，维护 available/inUse 两套槽位与全局 byID 索引；Acquire 优先复用空闲，否则在 MaxSize 内新建。
// CheckScaling 按利用率阈值扩容（≥0.8）或缩容（≤0.2 且高于 MinSize），带 ScaleCooldown；healthLoop 对超时心跳衰减健康并替换不健康 inUse Agent。
package swarm

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// AgentPool 有界池：跟踪空闲列表、占用表与 LRU 顺序用于缩容选牺牲者。
type AgentPool struct {
	mu sync.RWMutex

	cfg AgentPoolConfig

	available []*api.Agent          // 空闲可分配
	inUse     map[string]*api.Agent // 已借出
	byID      map[string]*api.Agent // 全量索引
	lru       []string              // 最近使用顺序（用于 pickLRUIdleUnlocked）

	scaleMu      sync.Mutex
	idMu         sync.Mutex
	lastScale    time.Time
	nextSerial   int64
	healthCtx    context.Context
	healthCancel context.CancelFunc
	healthWg     sync.WaitGroup
}

// NewAgentPool 修正 cfg 并启动 healthLoop。
func NewAgentPool(cfg AgentPoolConfig) *AgentPool {
	if cfg.MinSize < 0 {
		cfg.MinSize = 0
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 16
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 30 * time.Second
	}
	if cfg.ScaleCooldown <= 0 {
		cfg.ScaleCooldown = 10 * time.Second
	}
	if cfg.HealthDecayRate <= 0 {
		cfg.HealthDecayRate = 0.05
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &AgentPool{
		cfg:          cfg,
		inUse:        make(map[string]*api.Agent),
		byID:         make(map[string]*api.Agent),
		healthCtx:    ctx,
		healthCancel: cancel,
	}
	p.healthWg.Add(1)
	go p.healthLoop()
	return p
}

// genID 时间戳+单调序列，避免碰撞。
func (p *AgentPool) genID() string {
	p.idMu.Lock()
	p.nextSerial++
	n := p.nextSerial
	p.idMu.Unlock()
	return fmt.Sprintf("agent-%d-%d", time.Now().UnixNano(), n)
}

// Initialize 创建 MinSize 个默认 Coder/Core Agent 放入 available。
func (p *AgentPool) Initialize(ctx context.Context) error {
	for i := 0; i < p.cfg.MinSize; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		a := p.newAgent(api.AgentTypeCoder, api.AgentDomainCore)
		p.mu.Lock()
		p.available = append(p.available, a)
		p.byID[a.ID] = a
		p.mu.Unlock()
	}
	return nil
}

// newAgent 构造带健康/负载 Extra 指标的初始 Agent。
func (p *AgentPool) newAgent(t api.AgentType, d api.AgentDomain) *api.Agent {
	now := time.Now()
	return &api.Agent{
		ID:        p.genID(),
		Type:      t,
		Domain:    d,
		State:     api.AgentStateIdle,
		Status:    api.AgentStatusHealthy,
		CreatedAt: now,
		UpdatedAt: now,
		Capabilities: api.AgentCapabilities{
			Skills: []string{},
			Tools:  []string{},
		},
		Metrics: api.AgentMetrics{
			LastHeartbeat: now,
			Extra:         map[string]float64{"health": 1, "load": 0},
		},
	}
}

// Acquire 从 available 弹栈或 newAgent，标记 Busy 并登记 inUse；池满返回 nil。
func (p *AgentPool) Acquire() *api.Agent {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.available) > 0 {
		a := p.available[len(p.available)-1]
		p.available = p.available[:len(p.available)-1]
		a.State = api.AgentStateBusy
		a.Metrics.LastHeartbeat = time.Now()
		a.UpdatedAt = time.Now()
		p.inUse[a.ID] = a
		p.touchLRU(a.ID)
		return a
	}

	if len(p.inUse)+len(p.available) >= p.cfg.MaxSize {
		return nil
	}

	a := p.newAgent(api.AgentTypeCoder, api.AgentDomainCore)
	a.State = api.AgentStateBusy
	p.inUse[a.ID] = a
	p.byID[a.ID] = a
	p.touchLRU(a.ID)
	return a
}

// touchLRU 将 id 移到 lru 末尾表示最近使用。
func (p *AgentPool) touchLRU(id string) {
	for i, x := range p.lru {
		if x == id {
			p.lru = append(p.lru[:i], p.lru[i+1:]...)
			break
		}
	}
	p.lru = append(p.lru, id)
}

// Release 将 Busy Agent 置 Idle、负载清零并放回 available。
func (p *AgentPool) Release(agentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.inUse[agentID]
	if !ok {
		return
	}
	delete(p.inUse, agentID)
	a.State = api.AgentStateIdle
	setAgentLoad(a, 0)
	a.Metrics.LastHeartbeat = time.Now()
	a.UpdatedAt = time.Now()
	p.available = append(p.available, a)
}

// CheckScaling adjusts pool size based on utilization.
func (p *AgentPool) CheckScaling() {
	p.scaleMu.Lock()
	if time.Since(p.lastScale) < p.cfg.ScaleCooldown {
		p.scaleMu.Unlock()
		return
	}
	p.scaleMu.Unlock()

	p.mu.Lock()
	inUseN := len(p.inUse)
	total := len(p.inUse) + len(p.available)
	var util float64
	if total > 0 {
		util = float64(inUseN) / float64(total)
	}
	p.mu.Unlock()

	if total == 0 {
		return
	}

	if util >= 0.8 {
		p.scaleMu.Lock()
		if time.Since(p.lastScale) < p.cfg.ScaleCooldown {
			p.scaleMu.Unlock()
			return
		}
		p.mu.Lock()
		if len(p.inUse)+len(p.available) < p.cfg.MaxSize {
			a := p.newAgent(api.AgentTypeCoder, api.AgentDomainCore)
			p.available = append(p.available, a)
			p.byID[a.ID] = a
			p.lastScale = time.Now()
		}
		p.mu.Unlock()
		p.scaleMu.Unlock()
		return
	}

	if util <= 0.2 && total > p.cfg.MinSize {
		p.scaleMu.Lock()
		if time.Since(p.lastScale) < p.cfg.ScaleCooldown {
			p.scaleMu.Unlock()
			return
		}
		p.mu.Lock()
		victim := p.pickLRUIdleUnlocked()
		if victim != "" {
			p.evictIdleUnlocked(victim)
			p.lastScale = time.Now()
		}
		p.mu.Unlock()
		p.scaleMu.Unlock()
	}
}

func (p *AgentPool) pickLRUIdleUnlocked() string {
	for _, id := range p.lru {
		for _, a := range p.available {
			if a.ID == id {
				return id
			}
		}
	}
	if len(p.available) > 0 {
		return p.available[0].ID
	}
	return ""
}

func (p *AgentPool) evictIdleUnlocked(id string) {
	for i, a := range p.available {
		if a.ID == id {
			p.available = append(p.available[:i], p.available[i+1:]...)
			delete(p.byID, id)
			p.removeLRU(id)
			return
		}
	}
}

// removeLRU 从 lru 切片删除 id。
func (p *AgentPool) removeLRU(id string) {
	for i, x := range p.lru {
		if x == id {
			p.lru = append(p.lru[:i], p.lru[i+1:]...)
			return
		}
	}
}

// healthLoop 每秒 runHealthTick。
func (p *AgentPool) healthLoop() {
	defer p.healthWg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.healthCtx.Done():
			return
		case <-ticker.C:
			p.runHealthTick()
		}
	}
}

// runHealthTick 对 inUse 中超时未心跳者衰减健康，过低则从池中移除并 replaceUnhealthyUnlocked。
func (p *AgentPool) runHealthTick() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	timeout := p.cfg.HeartbeatTimeout
	for id, a := range p.inUse {
		if now.Sub(a.Metrics.LastHeartbeat) > timeout {
			h := agentHealth(a)
			h -= p.cfg.HealthDecayRate
			if h < 0 {
				h = 0
			}
			setAgentHealth(a, h)
			if h < 0.3 {
				delete(p.inUse, id)
				rep := p.replaceUnhealthyUnlocked(a)
				if rep != nil {
					p.inUse[rep.ID] = rep
					p.byID[rep.ID] = rep
				}
			}
		}
	}
}

// replaceUnhealthyUnlocked 优先从 available 取替身继承类型/域，否则在未满时新建。
func (p *AgentPool) replaceUnhealthyUnlocked(old *api.Agent) *api.Agent {
	delete(p.byID, old.ID)
	p.removeLRU(old.ID)
	if len(p.available) > 0 {
		a := p.available[len(p.available)-1]
		p.available = p.available[:len(p.available)-1]
		a.Type = old.Type
		a.Domain = old.Domain
		a.State = api.AgentStateBusy
		setAgentHealth(a, 1)
		setAgentLoad(a, 0)
		a.Metrics.LastHeartbeat = time.Now()
		a.UpdatedAt = time.Now()
		p.touchLRU(a.ID)
		return a
	}
	if len(p.inUse)+len(p.available) < p.cfg.MaxSize {
		a := p.newAgent(old.Type, old.Domain)
		a.State = api.AgentStateBusy
		p.byID[a.ID] = a
		p.touchLRU(a.ID)
		return a
	}
	return nil
}

// Shutdown 取消 healthCtx 并等待 healthLoop 退出。
func (p *AgentPool) Shutdown() {
	p.healthCancel()
	p.healthWg.Wait()
}

// Size 返回 inUse 与 available 总和。
func (p *AgentPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.inUse) + len(p.available)
}

// Get 从 byID 查找（不拷贝）。
func (p *AgentPool) Get(agentID string) (*api.Agent, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a, ok := p.byID[agentID]
	if !ok {
		return nil, fmt.Errorf("agentpool: unknown %s", agentID)
	}
	return a, nil
}

// List 返回 byID 中所有指针，按 ID 排序。
func (p *AgentPool) List() []*api.Agent {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*api.Agent, 0, len(p.byID))
	for _, a := range p.byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
