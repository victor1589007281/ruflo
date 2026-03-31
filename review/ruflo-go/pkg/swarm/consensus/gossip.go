package consensus

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sort"
	"sync"
	"time"
)

type gossipProposal struct {
	value       []byte
	hops        int
	ttl         int
	votes       map[string]bool
	round       int
	created     time.Time
	committed   bool
	antiEntropy map[string]uint64 // node -> last seq seen (anti-entropy)
}

type gossipConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	proposals map[string]*gossipProposal
	rng       *mrand.Rand
}

func newGossipConsensus(cfg Config) Engine {
	nodes := append([]string(nil), cfg.Peers...)
	if !stringSliceContains(nodes, cfg.NodeID) {
		nodes = append(nodes, cfg.NodeID)
	}
	sort.Strings(nodes)
	ttl := cfg.GossipTTL
	if ttl <= 0 {
		ttl = 16
	}
	_ = ttl // stored per proposal
	return &gossipConsensus{
		cfg:       cfg,
		nodes:     nodes,
		proposals: make(map[string]*gossipProposal),
		rng:       mrand.New(mrand.NewPCG(gossipSeed(), gossipSeed())),
	}
}

func gossipSeed() uint64 {
	return raftSeed()
}

func (g *gossipConsensus) fanout() int {
	f := g.cfg.GossipFanout
	if f <= 0 {
		f = 3
	}
	return f
}

func (g *gossipConsensus) AddNode(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !stringSliceContains(g.nodes, id) {
		g.nodes = append(g.nodes, id)
		sort.Strings(g.nodes)
	}
	return nil
}

func (g *gossipConsensus) RemoveNode(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes = stringSliceRemove(g.nodes, id)
	return nil
}

func (g *gossipConsensus) Propose(ctx context.Context, value []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.nodes) == 0 {
		return "", ErrNoNodes
	}

	propID := fmt.Sprintf("gossip-%s", randomID())
	ttl := g.cfg.GossipTTL
	if ttl <= 0 {
		ttl = 16
	}
	p := &gossipProposal{
		value:       append([]byte(nil), value...),
		ttl:         ttl,
		votes:       make(map[string]bool),
		created:     time.Now(),
		antiEntropy: make(map[string]uint64),
	}
	// Originator approves.
	p.votes[g.cfg.NodeID] = true
	p.antiEntropy[g.cfg.NodeID] = 1

	total := len(g.nodes)
	// Epidemic rounds until TTL exhausted or convergence.
	for hop := 0; hop < p.ttl; hop++ {
		p.hops = hop
		neighbors := g.randomNeighborsUnlocked(g.cfg.NodeID, g.fanout())
		for _, n := range neighbors {
			// Gossip receive: node adopts and votes approve (honest majority).
			p.votes[n] = true
			p.antiEntropy[n] = uint64(hop + 1)
		}
		votes := len(p.votes)
		approve := 0
		for _, ok := range p.votes {
			if ok {
				approve++
			}
		}
		convVotes := float64(votes) / float64(total)
		var approveRatio float64
		if votes > 0 {
			approveRatio = float64(approve) / float64(votes)
		}
		threshold := 2.0 / 3.0
		if convVotes >= 0.9 && approveRatio >= threshold {
			p.committed = true
			break
		}
		p.round++
	}

	if !p.committed && len(p.votes)*100 >= total*90 {
		approve := 0
		for _, ok := range p.votes {
			if ok {
				approve++
			}
		}
		if len(p.votes) > 0 && float64(approve)/float64(len(p.votes)) >= 2.0/3.0 {
			p.committed = true
		}
	}

	g.proposals[propID] = p
	if !p.committed {
		return propID, errors.New("gossip: convergence not reached")
	}
	return propID, nil
}

func (g *gossipConsensus) randomNeighborsUnlocked(exclude string, k int) []string {
	if len(g.nodes) <= 1 {
		return nil
	}
	candidates := make([]string, 0, len(g.nodes))
	for _, n := range g.nodes {
		if n != exclude {
			candidates = append(candidates, n)
		}
	}
	if k > len(candidates) {
		k = len(candidates)
	}
	if k == 0 {
		return nil
	}
	g.rng.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	return append([]string(nil), candidates[:k]...)
}

func (g *gossipConsensus) AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error) {
	wait := timeout
	if wait <= 0 {
		wait = 10 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	for {
		g.mu.RLock()
		p, ok := g.proposals[proposalID]
		g.mu.RUnlock()
		if ok && p.committed {
			return Result{
				ProposalID: proposalID,
				Committed:  true,
				Value:      append([]byte(nil), p.value...),
				Term:       uint64(p.round),
				FinishedAt: time.Now(),
			}, nil
		}
		select {
		case <-ctx2.Done():
			return Result{ProposalID: proposalID, Err: ctx2.Err(), FinishedAt: time.Now()}, ctx2.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (g *gossipConsensus) NodeID() string { return g.cfg.NodeID }

func (g *gossipConsensus) GetState() string { return "follower" }

func (g *gossipConsensus) IsLeader() bool { return false }

func (g *gossipConsensus) GetLeaderID() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.nodes) == 0 {
		return ""
	}
	return g.nodes[0]
}

func (g *gossipConsensus) Close() error { return nil }
