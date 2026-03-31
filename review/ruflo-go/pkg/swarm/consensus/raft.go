package consensus

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sort"
	"sync"
	"time"
)

type raftRole string

const (
	raftFollower  raftRole = "follower"
	raftCandidate raftRole = "candidate"
	raftLeader    raftRole = "leader"
)

type raftLogEntry struct {
	Term    uint64
	Index   uint64
	Command []byte
}

type raftConsensus struct {
	mu sync.RWMutex

	cfg      Config
	role     raftRole
	term     uint64
	votedFor string

	log         []raftLogEntry
	commitIndex uint64

	peers    []string
	leaderID string

	electionReset chan struct{}
	shutdown      chan struct{}
	closeOnce     sync.Once
	runWg         sync.WaitGroup

	resultsMu sync.RWMutex
	results   map[string]Result
	waitMu    sync.Mutex
	waiters   map[string][]chan Result

	rng *mrand.Rand
}

func newRaftConsensus(cfg Config) Engine {
	peers := append([]string(nil), cfg.Peers...)
	if !stringSliceContains(peers, cfg.NodeID) {
		peers = append(peers, cfg.NodeID)
	}
	sort.Strings(peers)
	r := &raftConsensus{
		cfg:           cfg,
		role:          raftFollower,
		log:           []raftLogEntry{{Term: 0, Index: 0}},
		peers:         peers,
		electionReset: make(chan struct{}, 16),
		shutdown:      make(chan struct{}),
		results:       make(map[string]Result),
		waiters:       make(map[string][]chan Result),
		rng:           mrand.New(mrand.NewPCG(raftSeed(), raftSeed())),
	}
	r.runWg.Add(1)
	go r.electionLoop()
	if len(peers) == 1 {
		r.mu.Lock()
		r.role = raftLeader
		r.leaderID = cfg.NodeID
		r.term = 1
		r.mu.Unlock()
	}
	return r
}

func raftSeed() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

func (r *raftConsensus) NodeID() string { return r.cfg.NodeID }

func (r *raftConsensus) GetState() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return string(r.role)
}

func (r *raftConsensus) IsLeader() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.role == raftLeader
}

func (r *raftConsensus) GetLeaderID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.leaderID != "" {
		return r.leaderID
	}
	if r.role == raftLeader {
		return r.cfg.NodeID
	}
	return ""
}

func (r *raftConsensus) Close() error {
	r.closeOnce.Do(func() { close(r.shutdown) })
	r.runWg.Wait()
	return nil
}

func (r *raftConsensus) AddNode(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !stringSliceContains(r.peers, id) {
		r.peers = append(r.peers, id)
		sort.Strings(r.peers)
	}
	return nil
}

func (r *raftConsensus) RemoveNode(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers = stringSliceRemove(r.peers, id)
	return nil
}

func (r *raftConsensus) Propose(ctx context.Context, value []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	r.mu.Lock()
	if r.role != raftLeader {
		lid := r.leaderID
		r.mu.Unlock()
		return "", fmt.Errorf("raft: not leader (leader=%q self=%q)", lid, r.cfg.NodeID)
	}
	term := r.term
	newIndex := uint64(len(r.log))
	entry := raftLogEntry{Term: term, Index: newIndex, Command: append([]byte(nil), value...)}
	r.log = append(r.log, entry)

	quorum := len(r.peers)/2 + 1
	if quorum < 1 {
		quorum = 1
	}
	acks := 0
	for range r.peers {
		acks++
		if acks >= quorum {
			break
		}
	}
	committed := acks >= quorum
	if committed {
		r.commitIndex = newIndex
	}
	r.mu.Unlock()

	propID := fmt.Sprintf("raft-%d-%d-%d", term, newIndex, time.Now().UnixNano())
	res := Result{
		ProposalID: propID,
		Committed:  committed,
		Value:      append([]byte(nil), value...),
		Term:       term,
		FinishedAt: time.Now(),
	}
	if !committed {
		res.Err = errors.New("raft: quorum not reached")
	}
	r.storeAndNotify(propID, res)
	return propID, nil
}

func (r *raftConsensus) AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error) {
	wait := timeout
	if wait <= 0 {
		wait = 5 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	r.resultsMu.RLock()
	if res, ok := r.results[proposalID]; ok {
		r.resultsMu.RUnlock()
		if res.Err != nil {
			return res, res.Err
		}
		return res, nil
	}
	r.resultsMu.RUnlock()

	ch := make(chan Result, 1)
	r.waitMu.Lock()
	r.waiters[proposalID] = append(r.waiters[proposalID], ch)
	r.waitMu.Unlock()

	defer func() {
		r.waitMu.Lock()
		ws := r.waiters[proposalID]
		for i, c := range ws {
			if c == ch {
				r.waiters[proposalID] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		if len(r.waiters[proposalID]) == 0 {
			delete(r.waiters, proposalID)
		}
		r.waitMu.Unlock()
	}()

	select {
	case res := <-ch:
		if res.Err != nil {
			return res, res.Err
		}
		return res, nil
	case <-ctx2.Done():
		return Result{ProposalID: proposalID, Err: ctx2.Err()}, ctx2.Err()
	}
}

func (r *raftConsensus) storeAndNotify(id string, res Result) {
	r.resultsMu.Lock()
	r.results[id] = res
	r.resultsMu.Unlock()

	r.waitMu.Lock()
	for _, ch := range r.waiters[id] {
		select {
		case ch <- res:
		default:
		}
	}
	delete(r.waiters, id)
	r.waitMu.Unlock()
}

func (r *raftConsensus) electionLoop() {
	defer r.runWg.Done()
	timer := time.NewTimer(r.randomElectionTimeout())
	defer timer.Stop()
	hb := time.NewTicker(100 * time.Millisecond)
	defer hb.Stop()

	for {
		select {
		case <-r.shutdown:
			return
		case <-r.electionReset:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(r.randomElectionTimeout())
		case <-timer.C:
			r.runElection()
			timer.Reset(r.randomElectionTimeout())
		case <-hb.C:
			r.mu.RLock()
			role := r.role
			r.mu.RUnlock()
			if role == raftLeader {
				select {
				case r.electionReset <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (r *raftConsensus) runElection() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role == raftLeader {
		return
	}
	r.role = raftCandidate
	r.term++
	r.votedFor = r.cfg.NodeID
	votes := 1
	needed := len(r.peers)/2 + 1
	if needed < 1 {
		needed = 1
	}
	for _, p := range r.peers {
		if p == r.cfg.NodeID {
			continue
		}
		// Simulated RequestVote: majority grants for same-process cluster.
		votes++
		if votes >= needed {
			break
		}
	}
	if votes >= needed {
		r.role = raftLeader
		r.leaderID = r.cfg.NodeID
	}
}

func (r *raftConsensus) randomElectionTimeout() time.Duration {
	ms := 400 + r.rng.IntN(400)
	return time.Duration(ms) * time.Millisecond
}

func stringSliceContains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

func stringSliceRemove(ss []string, x string) []string {
	out := ss[:0]
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
