// CRDT（Conflict-free Replicated Data Type，无冲突复制数据类型）共识实现。
//
// # 理论基础（Shapiro et al., INRIA 2011）
//
// CRDT 利用数学性质——交换律、结合律、幂等性——保证任何副本在任意顺序接收更新后，
// 最终收敛到相同状态（强最终一致性），无需协调协议。
//
// # G-Counter（只增计数器）
//
// 每个节点维护一个 map[nodeID]uint64，只允许对自己的槽位递增。
// 全局计数 = Σ map[node]。Merge 操作取各槽位的 max（即 LUB，最小上界）。
// 数学证明：max 满足交换律+结合律+幂等性 → 强最终一致。
//
// # 本文件的 Propose 流程
//
//  1. 为每个节点初始化空 G-Counter 视图
//  2. 本节点对自己的槽位 +1（G-Counter 递增）
//  3. 广播合并轮次：每轮取所有视图的全局 merge，然后广播回每个节点
//  4. 当所有节点视图相等时收敛（committed）
//  5. 最多 n+4 轮（保证收敛）
//
// # 其他 CRDT 类型（未在本文件实现但与此框架兼容）
//
//   - PN-Counter: 两个 G-Counter 相减，支持增减
//   - LWW-Register: Last-Writer-Wins 寄存器，按时间戳取最新值
//   - OR-Set: Observed-Remove 集合，支持并发添加和删除
package consensus

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

// MergeGCounters 计算两个 G-Counter 的合并结果（逐键取 max），即 CRDT join-semilattice 的 LUB（最小上界）。
// 时间复杂度 O(|a| + |b|)。
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

// crdtProposal 单笔 CRDT 提案的状态。
//   - merged: 所有副本 G-Counter 合并后的全局视图
//   - views: 各节点各自的 G-Counter 本地视图
//   - rounds: 合并到收敛所用的轮数
type crdtProposal struct {
	value     []byte
	merged    map[string]uint64
	views     map[string]map[string]uint64
	committed bool
	rounds    int
	created   time.Time
}

// crdtConsensus CRDT 引擎：无 Leader，所有节点对等，通过 G-Counter 合并达成最终一致。
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

// crdtViewsEqual 判断所有副本视图是否两两相同（DeepEqual）。O(n·k) k 为 map 大小。
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

// mergeAllViews 对所有节点视图做 G-Counter 累积 merge。O(n·k)。
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
	// G-Counter 递增：本节点对自己的槽位 +1
	views[c.cfg.NodeID] = MergeGCounters(views[c.cfg.NodeID], map[string]uint64{c.cfg.NodeID: 1})

	// 广播合并轮次：每轮计算全局 merge 并广播到所有副本，直到所有视图一致（收敛）
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
