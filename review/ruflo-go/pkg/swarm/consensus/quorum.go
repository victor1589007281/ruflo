package consensus

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type quorumProposal struct {
	value      []byte
	votes      map[string]bool
	accepts    int
	rejects    int
	quorumNeed int
	committed  bool
	created    time.Time
}

type quorumConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	proposals map[string]*quorumProposal
}

func newQuorumConsensus(cfg Config) Engine {
	nodes := append([]string(nil), cfg.Peers...)
	if !stringSliceContains(nodes, cfg.NodeID) {
		nodes = append(nodes, cfg.NodeID)
	}
	sort.Strings(nodes)
	return &quorumConsensus{
		cfg:       cfg,
		nodes:     nodes,
		proposals: make(map[string]*quorumProposal),
	}
}

func (q *quorumConsensus) quorumThreshold() int {
	n := len(q.nodes)
	if n == 0 {
		return 0
	}
	if q.cfg.QuorumSize > 0 {
		if q.cfg.QuorumSize > n {
			return n
		}
		return q.cfg.QuorumSize
	}
	return n/2 + 1
}

// quorumVote decides accept/reject from local policy (non-empty payload accepts).
func (q *quorumConsensus) quorumVote(value []byte) bool {
	return len(value) > 0
}

func (q *quorumConsensus) AddNode(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !stringSliceContains(q.nodes, id) {
		q.nodes = append(q.nodes, id)
		sort.Strings(q.nodes)
	}
	return nil
}

func (q *quorumConsensus) RemoveNode(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nodes = stringSliceRemove(q.nodes, id)
	return nil
}

func (q *quorumConsensus) Propose(ctx context.Context, value []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.nodes) == 0 {
		return "", ErrNoNodes
	}

	need := q.quorumThreshold()
	propID := fmt.Sprintf("quorum-%s", randomID())
	votes := make(map[string]bool)
	accepts := 0

	// Broadcast: collect each node's vote.
	for _, n := range q.nodes {
		approve := q.quorumVote(value)
		votes[n] = approve
		if approve {
			accepts++
		}
	}

	committed := accepts >= need
	p := &quorumProposal{
		value:      append([]byte(nil), value...),
		votes:      votes,
		accepts:    accepts,
		rejects:    len(q.nodes) - accepts,
		quorumNeed: need,
		committed:  committed,
		created:    time.Now(),
	}
	q.proposals[propID] = p
	if !committed {
		return propID, fmt.Errorf("quorum: insufficient accepts %d/%d (need %d)", accepts, len(q.nodes), need)
	}
	return propID, nil
}

func (q *quorumConsensus) AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error) {
	wait := timeout
	if wait <= 0 {
		wait = 10 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		q.mu.RLock()
		p, ok := q.proposals[proposalID]
		q.mu.RUnlock()
		if ok && p.committed {
			return Result{
				ProposalID: proposalID,
				Committed:  true,
				Value:      append([]byte(nil), p.value...),
				Term:       uint64(p.quorumNeed),
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

func (q *quorumConsensus) NodeID() string { return q.cfg.NodeID }

func (q *quorumConsensus) GetState() string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if len(q.nodes) == 0 {
		return "voter"
	}
	if q.nodes[0] == q.cfg.NodeID {
		return "leader"
	}
	return "follower"
}

func (q *quorumConsensus) IsLeader() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.nodes) > 0 && q.nodes[0] == q.cfg.NodeID
}

func (q *quorumConsensus) GetLeaderID() string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if len(q.nodes) == 0 {
		return ""
	}
	return q.nodes[0]
}

func (q *quorumConsensus) Close() error { return nil }
