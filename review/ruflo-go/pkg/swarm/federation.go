// 联邦中心（swarm 包）：跨多个子 Swarm 注册表、消息路由与临时 Agent（TTL）；SelectOptimalSwarm 按容量、心跳新鲜度与能力重叠打分；
// Vote/Propose 维护 proposalID -> swarmID -> 票的映射实现联邦级法定人数；无 Bus 时消息暂存 messages 内存队列。
package swarm

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// FederationSwarm 联邦中的一个蜂群参与者，可挂接本地总线与协调器。
type FederationSwarm struct {
	ID            string                   // 蜂群唯一 id
	Capabilities  []string                 // 能力标签供路由打分
	Capacity      float64                  // 容量权重（≤0 时按 1 处理）
	LastHeartbeat time.Time                // 最后存活时间
	Bus           *MessageBus              // 可选：直连投递
	Coordinator   *UnifiedSwarmCoordinator // 可选：与单群协调器绑定
}

// EphemeralAgent 跨群短期工人，到期应由 PruneEphemeral 清理。
type EphemeralAgent struct {
	ID        string    // 全局临时 id
	SwarmID   string    // 所属蜂群
	ExpiresAt time.Time // 绝对过期时间
}

// FederationHub 进程内联邦注册表与投票状态机。
type FederationHub struct {
	mu sync.RWMutex

	swarms    map[string]*FederationSwarm
	ephemeral map[string]*EphemeralAgent
	messages  map[string][]api.Message // Bus 为空时的退避队列

	quorumVotes map[string]map[string]bool // proposalID -> swarmID -> 赞成与否
}

// NewFederationHub 创建空 Hub。
func NewFederationHub() *FederationHub {
	return &FederationHub{
		swarms:      make(map[string]*FederationSwarm),
		ephemeral:   make(map[string]*EphemeralAgent),
		messages:    make(map[string][]api.Message),
		quorumVotes: make(map[string]map[string]bool),
	}
}

// RegisterSwarm 写入/覆盖 swarms 并刷新 LastHeartbeat。
func (h *FederationHub) RegisterSwarm(s *FederationSwarm) error {
	if s == nil || s.ID == "" {
		return fmt.Errorf("federation: invalid swarm")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s.LastHeartbeat = time.Now()
	h.swarms[s.ID] = s
	return nil
}

// SpawnEphemeralAgent 校验 swarm 存在后生成 eph-{swarm}-{nano} 并登记过期时间。
func (h *FederationHub) SpawnEphemeralAgent(swarmID string, ttl time.Duration) (*EphemeralAgent, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.swarms[swarmID]; !ok {
		return nil, fmt.Errorf("federation: unknown swarm %s", swarmID)
	}
	id := fmt.Sprintf("eph-%s-%d", swarmID, time.Now().UnixNano())
	e := &EphemeralAgent{ID: id, SwarmID: swarmID, ExpiresAt: time.Now().Add(ttl)}
	h.ephemeral[id] = e
	return e, nil
}

// TerminateAgent 主动删除临时 Agent。
func (h *FederationHub) TerminateAgent(agentID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ephemeral, agentID)
	return nil
}

// SelectOptimalSwarm 线性组合 score=0.5*Capacity+0.35*exp(-Δt/30)+0.15*overlap，取最高分。
func (h *FederationHub) SelectOptimalSwarm(required []string) (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	now := time.Now()
	type cand struct {
		id    string
		score float64
	}
	var list []cand
	for sid, s := range h.swarms {
		fresh := math.Exp(-now.Sub(s.LastHeartbeat).Seconds() / 30.0)
		capW := s.Capacity
		if capW <= 0 {
			capW = 1
		}
		overlap := capabilityOverlap(s.Capabilities, required)
		sc := capW*0.5 + fresh*0.35 + overlap*0.15
		list = append(list, cand{id: sid, score: sc})
	}
	if len(list) == 0 {
		return "", fmt.Errorf("federation: no swarms")
	}
	sort.Slice(list, func(i, j int) bool { return list[i].score > list[j].score })
	return list[0].id, nil
}

// capabilityOverlap need 为空视为完全匹配；否则为交集比例。
func capabilityOverlap(have, need []string) float64 {
	if len(need) == 0 {
		return 1
	}
	set := make(map[string]struct{}, len(have))
	for _, c := range have {
		set[c] = struct{}{}
	}
	n := 0
	for _, c := range need {
		if _, ok := set[c]; ok {
			n++
		}
	}
	return float64(n) / float64(len(need))
}

// SendMessage 优先 FederationSwarm.Bus.Send；否则追加到 messages[swarmID]。
func (h *FederationHub) SendMessage(swarmID string, msg api.Message) error {
	h.mu.RLock()
	s := h.swarms[swarmID]
	h.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("federation: unknown swarm %s", swarmID)
	}
	if s.Bus != nil {
		return s.Bus.Send(msg)
	}
	h.mu.Lock()
	h.messages[swarmID] = append(h.messages[swarmID], msg)
	h.mu.Unlock()
	return nil
}

// Broadcast 对每个已注册 swarm 调用 SendMessage。
func (h *FederationHub) Broadcast(msg api.Message, excludeSwarmID string) {
	h.mu.RLock()
	ids := make([]string, 0, len(h.swarms))
	for id := range h.swarms {
		ids = append(ids, id)
	}
	h.mu.RUnlock()
	for _, id := range ids {
		if id == excludeSwarmID {
			continue
		}
		cp := msg
		_ = h.SendMessage(id, cp)
	}
}

// Propose 懒创建 quorumVotes[proposalID] map。
func (h *FederationHub) Propose(proposalID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.quorumVotes[proposalID] == nil {
		h.quorumVotes[proposalID] = make(map[string]bool)
	}
}

// Vote 记录 swarmID 的票；quorum 默认 len(swarms)/2+1，yes>=quorum 返回 true。
func (h *FederationHub) Vote(proposalID, swarmID string, approve bool, quorum int) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.quorumVotes[proposalID]
	if !ok {
		m = make(map[string]bool)
		h.quorumVotes[proposalID] = m
	}
	m[swarmID] = approve
	if quorum <= 0 {
		quorum = len(h.swarms)/2 + 1
	}
	yes := 0
	for _, a := range m {
		if a {
			yes++
		}
	}
	return yes >= quorum, nil
}

// PruneEphemeral 扫描 ephemeral 删除过期项，返回清除数量。
func (h *FederationHub) PruneEphemeral() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	n := 0
	for id, e := range h.ephemeral {
		if now.After(e.ExpiresAt) {
			delete(h.ephemeral, id)
			n++
		}
	}
	return n
}

// TouchHeartbeat 更新指定 FederationSwarm 的 LastHeartbeat。
func (h *FederationHub) TouchHeartbeat(swarmID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.swarms[swarmID]; ok {
		s.LastHeartbeat = time.Now()
	}
}
