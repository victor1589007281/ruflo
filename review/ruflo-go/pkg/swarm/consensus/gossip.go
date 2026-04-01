// Gossip（流行病式/流言传播）共识协议实现。
//
// # 算法原理（Demers et al., 1987; Kermarrec et al., 2003）
//
// Gossip 协议受流行病传播启发：每一轮（round），每个节点随机选择 fanout 个邻居传播信息。
// 通过多轮传播，信息最终到达所有节点。收敛时间约 O(log N) 轮（N 为节点数）。
//
// # 传播模型
//
// 本实现采用 Push 模式：
//  1. 发起者(originator)创建提案并自行投票
//  2. 每轮随机选 fanout 个邻居传播（Fisher-Yates 洗牌取前 k 个）
//  3. 收到的节点也投赞成票（模拟诚实多数假设）
//  4. 当 ≥90% 节点参与且 ≥2/3 赞成时，视为已收敛并提交
//  5. TTL 限制最大传播轮数（防止无限传播）
//
// # Anti-Entropy
//
// antiEntropy 字段记录每个节点最后看到的序号，用于检测信息滞后、驱动补偿同步
// （本实现中仅记录，未做主动拉取）。
//
// # 一致性保证
//
// Gossip 只提供最终一致性（eventual consistency），不保证全序；适合对一致性要求较低、
// 但对可用性和分区容错要求高的场景（如状态广播、心跳汇聚）。
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

// gossipProposal 单笔 Gossip 提案的状态。
//   - hops: 已经过的传播轮数
//   - ttl: 最大传播轮数
//   - votes: 各节点的投票结果（true=赞成）
//   - antiEntropy: 各节点最后观察到的传播序号（用于数据修复）
type gossipProposal struct {
	value       []byte
	hops        int
	ttl         int
	votes       map[string]bool
	round       int
	created     time.Time
	committed   bool
	antiEntropy map[string]uint64
}

// gossipConsensus Gossip 引擎：无 Leader，所有节点对等，靠随机传播达成最终一致。
type gossipConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	proposals map[string]*gossipProposal
	rng       *mrand.Rand
}

// newGossipConsensus 构造 Gossip 引擎，默认 TTL=16（即最多 16 轮传播）。
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

// AddNode 加入集群并排序。O(n log n)。
func (g *gossipConsensus) AddNode(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !stringSliceContains(g.nodes, id) {
		g.nodes = append(g.nodes, id)
		sort.Strings(g.nodes)
	}
	return nil
}

// RemoveNode 移除节点。O(n)。
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
	// 发起者自己投赞成票
	p.votes[g.cfg.NodeID] = true
	p.antiEntropy[g.cfg.NodeID] = 1

	total := len(g.nodes)
	// 流行病传播循环：每轮随机选 fanout 个邻居传播，直到 TTL 用尽或收敛
	for hop := 0; hop < p.ttl; hop++ {
		p.hops = hop
		neighbors := g.randomNeighborsUnlocked(g.cfg.NodeID, g.fanout())
		for _, n := range neighbors {
			// 模拟 Gossip 接收：诚实节点收到信息后投赞成票
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

// AwaitConsensus 轮询直至 committed 或超时。O(超时/间隔)。
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
