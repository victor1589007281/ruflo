package consensus

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

// MergeGCounters returns the pointwise maximum of G-Counter maps (LUB in CRDT join).
func MergeGCounters(a, b map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64)
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if v > out[k] {
			out[k] = v
		}
	}
	return out
}

type crdtProposal struct {
	value     []byte
	merged    map[string]uint64
	views     map[string]map[string]uint64
	committed bool
	rounds    int
	created   time.Time
}

type crdtConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	proposals map[string]*crdtProposal
}

func newCRDTConsensus(cfg Config) Engine {
	nodes := append([]string(nil), cfg.Peers...)
	if !stringSliceContains(nodes, cfg.NodeID) {
		nodes = append(nodes, cfg.NodeID)
	}
	sort.Strings(nodes)
	return &crdtConsensus{
		cfg:       cfg,
		nodes:     nodes,
		proposals: make(map[string]*crdtProposal),
	}
}

func (c *crdtConsensus) AddNode(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !stringSliceContains(c.nodes, id) {
		c.nodes = append(c.nodes, id)
		sort.Strings(c.nodes)
	}
	return nil
}

func (c *crdtConsensus) RemoveNode(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = stringSliceRemove(c.nodes, id)
	return nil
}

func crdtViewsEqual(views map[string]map[string]uint64) bool {
	var first map[string]uint64
	for _, v := range views {
		first = v
		break
	}
	if first == nil {
		return true
	}
	for _, v := range views {
		if !reflect.DeepEqual(first, v) {
			return false
		}
	}
	return true
}

func mergeAllViews(views map[string]map[string]uint64) map[string]uint64 {
	var acc map[string]uint64
	for _, m := range views {
		if acc == nil {
			acc = make(map[string]uint64)
			for k, v := range m {
				acc[k] = v
			}
			continue
		}
		acc = MergeGCounters(acc, m)
	}
	if acc == nil {
		return make(map[string]uint64)
	}
	return acc
}

func (c *crdtConsensus) Propose(ctx context.Context, value []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.nodes) == 0 {
		return "", ErrNoNodes
	}

	propID := fmt.Sprintf("crdt-%s", randomID())
	views := make(map[string]map[string]uint64, len(c.nodes))
	for _, n := range c.nodes {
		views[n] = make(map[string]uint64)
	}
	// Propose: increment local counter for this node (G-Counter increment).
	views[c.cfg.NodeID] = MergeGCounters(views[c.cfg.NodeID], map[string]uint64{c.cfg.NodeID: 1})

	// Broadcast rounds: merge max across replicas until all views match.
	rounds := 0
	maxRounds := len(c.nodes) + 4
	converged := false
	for rounds = 0; rounds < maxRounds; rounds++ {
		canon := mergeAllViews(views)
		for _, n := range c.nodes {
			views[n] = MergeGCounters(views[n], canon)
		}
		if crdtViewsEqual(views) {
			converged = true
			break
		}
	}

	p := &crdtProposal{
		value:     append([]byte(nil), value...),
		merged:    mergeAllViews(views),
		views:     views,
		committed: converged,
		rounds:    rounds,
		created:   time.Now(),
	}
	c.proposals[propID] = p
	if !converged {
		return propID, fmt.Errorf("crdt: convergence not reached")
	}
	return propID, nil
}

func (c *crdtConsensus) AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error) {
	wait := timeout
	if wait <= 0 {
		wait = 10 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		c.mu.RLock()
		p, ok := c.proposals[proposalID]
		c.mu.RUnlock()
		if ok && p.committed {
			return Result{
				ProposalID: proposalID,
				Committed:  true,
				Value:      append([]byte(nil), p.value...),
				Term:       uint64(p.rounds),
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

func (c *crdtConsensus) NodeID() string { return c.cfg.NodeID }

func (c *crdtConsensus) GetState() string { return "replica" }

func (c *crdtConsensus) IsLeader() bool { return false }

func (c *crdtConsensus) GetLeaderID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.nodes) == 0 {
		return ""
	}
	return c.nodes[0]
}

func (c *crdtConsensus) Close() error { return nil }
