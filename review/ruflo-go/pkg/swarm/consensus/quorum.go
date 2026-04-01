// 本文件实现基于法定人数（Quorum）投票的共识演示：提案需获得足够多的节点接受方可提交。
//
// # Quorum 与读写一致性（经典多数派）
//
// 设副本数为 n，常见写法定人数 W 与读法定人数 R 满足 W + R > n 且 W,R ≥ ⌈(n+1)/2⌉ 时，任意读与写副本集合相交，
// 从而可保证读到最新已提交写入（在同步更新假设下）。本实现默认 need = ⌊n/2⌋+1，即严格多数派。
package consensus

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// quorumProposal 单笔提案的投票统计：votes 记录各节点是否接受；accepts/rejects 计数；quorumNeed 为通过阈值。
type quorumProposal struct {
	value      []byte
	votes      map[string]bool
	accepts    int
	rejects    int
	quorumNeed int
	committed  bool
	created    time.Time
}

// quorumConsensus Quorum 引擎：nodes 为选民集合；可配置 QuorumSize 覆盖默认多数。
type quorumConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	proposals map[string]*quorumProposal
}

// newQuorumConsensus 构造 Quorum 引擎。
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

// quorumThreshold 返回通过所需接受票数：默认 ⌊n/2⌋+1 = ⌈(n+1)/2⌉；若配置 QuorumSize 则裁剪到 [1,n]。O(1)。
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

// quorumVote 本地策略：非空 payload 视为接受（演示用）。O(1)。
func (q *quorumConsensus) quorumVote(value []byte) bool {
	return len(value) > 0
}

// AddNode 加入选民。O(n log n)。
func (q *quorumConsensus) AddNode(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !stringSliceContains(q.nodes, id) {
		q.nodes = append(q.nodes, id)
		sort.Strings(q.nodes)
	}
	return nil
}

// RemoveNode 移除选民。O(n)。
func (q *quorumConsensus) RemoveNode(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nodes = stringSliceRemove(q.nodes, id)
	return nil
}

// Propose 向每个节点征询 quorumVote，accepts ≥ quorumThreshold 则 committed。O(n)；空间 O(n)。
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

// AwaitConsensus 轮询直至 committed 或超时。O(超时/间隔)。
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

// NodeID 本节点。O(1)。
func (q *quorumConsensus) NodeID() string { return q.cfg.NodeID }

// GetState 排序后首节点视为 leader 占位，其余 follower。O(1)。
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

// IsLeader 当本节点为排序后首节点时为 true（演示 leader 标签）。O(1)。
func (q *quorumConsensus) IsLeader() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.nodes) > 0 && q.nodes[0] == q.cfg.NodeID
}

// GetLeaderID 返回排序后首个节点 id。O(1)。
func (q *quorumConsensus) GetLeaderID() string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if len(q.nodes) == 0 {
		return ""
	}
	return q.nodes[0]
}

// Close 无资源。O(1)。
func (q *quorumConsensus) Close() error { return nil }
