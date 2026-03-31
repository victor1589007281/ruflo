package swarm

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// FederationSwarm is a registrable swarm participant.
type FederationSwarm struct {
	ID            string
	Capabilities  []string
	Capacity      float64
	LastHeartbeat time.Time
	Bus           *MessageBus
	Coordinator   *UnifiedSwarmCoordinator
}

// EphemeralAgent is short-lived cross-swarm worker.
type EphemeralAgent struct {
	ID        string
	SwarmID   string
	ExpiresAt time.Time
}

// FederationHub coordinates multiple swarms and ephemeral agents.
type FederationHub struct {
	mu sync.RWMutex

	swarms    map[string]*FederationSwarm
	ephemeral map[string]*EphemeralAgent
	messages  map[string][]api.Message

	quorumVotes map[string]map[string]bool
}

// NewFederationHub constructs an empty hub.
func NewFederationHub() *FederationHub {
	return &FederationHub{
		swarms:      make(map[string]*FederationSwarm),
		ephemeral:   make(map[string]*EphemeralAgent),
		messages:    make(map[string][]api.Message),
		quorumVotes: make(map[string]map[string]bool),
	}
}

// RegisterSwarm adds or replaces a swarm entry.
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

// SpawnEphemeralAgent registers a TTL-bound agent id under a swarm.
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

// TerminateAgent removes an ephemeral agent.
func (h *FederationHub) TerminateAgent(agentID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ephemeral, agentID)
	return nil
}

// SelectOptimalSwarm picks swarm by capacity, heartbeat freshness, capability overlap.
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

// SendMessage routes to a swarm bus if present.
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

// Broadcast fans out to all swarms except excluded id.
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

// Propose starts federation-level quorum tracking for a proposal id.
func (h *FederationHub) Propose(proposalID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.quorumVotes[proposalID] == nil {
		h.quorumVotes[proposalID] = make(map[string]bool)
	}
}

// Vote records a swarm vote; returns committed if quorum reached.
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

// PruneEphemeral removes expired agents (call periodically).
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

// TouchHeartbeat updates swarm liveness.
func (h *FederationHub) TouchHeartbeat(swarmID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.swarms[swarmID]; ok {
		s.LastHeartbeat = time.Now()
	}
}
