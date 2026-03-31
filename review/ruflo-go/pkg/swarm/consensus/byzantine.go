package consensus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// pbftInstance tracks PBFT phases for one proposal.
type pbftInstance struct {
	view       uint64
	sequence   uint64
	value      []byte
	prePrepare map[string]struct{}
	prepare    map[string]struct{}
	commit     map[string]struct{}
	committed  bool
	created    time.Time
	primaryID  string
}

type byzantineConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	view      uint64
	sequence  uint64
	f         int
	proposals map[string]*pbftInstance
}

func newByzantineConsensus(cfg Config) Engine {
	nodes := append([]string(nil), cfg.Peers...)
	if !stringSliceContains(nodes, cfg.NodeID) {
		nodes = append(nodes, cfg.NodeID)
	}
	sort.Strings(nodes)
	n := len(nodes)
	f := cfg.ByzantineF
	if f < 0 {
		f = 0
	}
	maxF := (n - 1) / 3
	if f > maxF {
		f = maxF
	}
	if n > 0 && f*3+1 > n {
		f = maxF
	}
	return &byzantineConsensus{
		cfg:       cfg,
		nodes:     nodes,
		proposals: make(map[string]*pbftInstance),
		f:         f,
	}
}

func (b *byzantineConsensus) primaryForView(view uint64) string {
	if len(b.nodes) == 0 {
		return ""
	}
	idx := int(view % uint64(len(b.nodes)))
	return b.nodes[idx]
}

func (b *byzantineConsensus) threshold() int {
	return 2*b.f + 1
}

func (b *byzantineConsensus) AddNode(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !stringSliceContains(b.nodes, id) {
		b.nodes = append(b.nodes, id)
		sort.Strings(b.nodes)
		n := len(b.nodes)
		maxF := (n - 1) / 3
		if b.f > maxF {
			b.f = maxF
		}
	}
	return nil
}

func (b *byzantineConsensus) RemoveNode(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nodes = stringSliceRemove(b.nodes, id)
	n := len(b.nodes)
	maxF := 0
	if n > 0 {
		maxF = (n - 1) / 3
	}
	if b.f > maxF {
		b.f = maxF
	}
	return nil
}

func (b *byzantineConsensus) Propose(ctx context.Context, value []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.nodes) == 0 {
		return "", ErrNoNodes
	}
	t := b.threshold()
	if t > len(b.nodes) {
		return "", fmt.Errorf("pbft: need 3f+1 nodes; have %d f=%d", len(b.nodes), b.f)
	}

	primary := b.primaryForView(b.view)
	if primary != b.cfg.NodeID {
		return "", fmt.Errorf("pbft: only primary %q may propose in view %d", primary, b.view)
	}

	b.sequence++
	seq := b.sequence
	propID := fmt.Sprintf("pbft-%d-%d-%s", b.view, seq, randomID())

	inst := &pbftInstance{
		view:       b.view,
		sequence:   seq,
		value:      append([]byte(nil), value...),
		prePrepare: make(map[string]struct{}),
		prepare:    make(map[string]struct{}),
		commit:     make(map[string]struct{}),
		created:    time.Now(),
		primaryID:  primary,
	}

	for _, nid := range b.nodes {
		inst.prePrepare[nid] = struct{}{}
	}
	if len(inst.prePrepare) < t {
		return "", errors.New("pbft: pre-prepare quorum not met")
	}

	for i := 0; i < t && i < len(b.nodes); i++ {
		inst.prepare[b.nodes[i]] = struct{}{}
	}
	for i := 0; i < t && i < len(b.nodes); i++ {
		inst.commit[b.nodes[i]] = struct{}{}
	}

	if len(inst.prepare) >= t && len(inst.commit) >= t {
		inst.committed = true
	}

	b.proposals[propID] = inst
	return propID, nil
}

func (b *byzantineConsensus) AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error) {
	wait := timeout
	if wait <= 0 {
		wait = 10 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	for {
		b.mu.RLock()
		inst, ok := b.proposals[proposalID]
		b.mu.RUnlock()
		if ok && inst != nil && inst.committed {
			return Result{
				ProposalID: proposalID,
				Committed:  true,
				Value:      append([]byte(nil), inst.value...),
				Term:       inst.view,
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

func (b *byzantineConsensus) NodeID() string { return b.cfg.NodeID }

func (b *byzantineConsensus) GetState() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.nodes) == 0 {
		return "replica"
	}
	if b.primaryForView(b.view) == b.cfg.NodeID {
		return "primary"
	}
	return "replica"
}

func (b *byzantineConsensus) IsLeader() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.nodes) > 0 && b.primaryForView(b.view) == b.cfg.NodeID
}

func (b *byzantineConsensus) GetLeaderID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.primaryForView(b.view)
}

func (b *byzantineConsensus) Close() error { return nil }
