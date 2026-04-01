// 本文件实现 PBFT（Practical Byzantine Fault Tolerance，Castro & Liskov 1999）风格的提案提交流程简化模拟。
//
// # 拜占庭容错
//
// 在至多 f 个节点任意故障（含恶意）时，需 n ≥ 3f+1。三阶段广播：Pre-Prepare（主节点序列化提案）→ Prepare → Commit，
// 各阶段需收集至少 2f+1 条一致投票方可推进。视图切换（View Change）在主节点失效时轮换 primary，本代码仅固定 view 与简化计数模拟。
//
// Propose 仅允许当前 view 的 primary 发起；内部用阈值 t=2f+1 填充各阶段集合以演示提交条件。
package consensus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// pbftInstance 跟踪单笔提案在各阶段的副本确认集合与是否已提交。
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

// byzantineConsensus PBFT 引擎状态：nodes 排序；view 当前视图；sequence 单调序号；f 为容错数上界；(n-1)/3。
type byzantineConsensus struct {
	mu sync.RWMutex

	cfg       Config
	nodes     []string
	view      uint64
	sequence  uint64
	f         int
	proposals map[string]*pbftInstance
}

// newByzantineConsensus 构造 PBFT 引擎并裁剪 f 至 max ⌊(n-1)/3⌋。
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

// primaryForView 按 view mod n 轮转主节点。O(1)。
func (b *byzantineConsensus) primaryForView(view uint64) string {
	if len(b.nodes) == 0 {
		return ""
	}
	idx := int(view % uint64(len(b.nodes)))
	return b.nodes[idx]
}

// threshold 返回 PBFT 阶段法定人数 2f+1。O(1)。
func (b *byzantineConsensus) threshold() int {
	return 2*b.f + 1
}

// AddNode 加入节点并重新约束 f。O(n log n) 排序。
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

// RemoveNode 移除节点并调整 f。O(n)。
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

// Propose 仅 primary 可调用：分配序号，构造 pre-prepare/prepare/commit 集合（本实现为同步模拟填充至阈值），满足则 committed。
// 时间复杂度 O(n)；空间 O(|value|)。
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

// AwaitConsensus 轮询直至提案 committed 或超时。O(等待时间/轮询间隔)。
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

// NodeID 返回本副本标识。O(1)。
func (b *byzantineConsensus) NodeID() string { return b.cfg.NodeID }

// GetState 若本机为当前 view 的 primary 返回 "primary"，否则 "replica"。O(1)。
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

// IsLeader 语义对应 PBFT primary。O(1)。
func (b *byzantineConsensus) IsLeader() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.nodes) > 0 && b.primaryForView(b.view) == b.cfg.NodeID
}

// GetLeaderID 返回当前 view 的主节点 id。O(1)。
func (b *byzantineConsensus) GetLeaderID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.primaryForView(b.view)
}

// Close 无后台资源。O(1)。
func (b *byzantineConsensus) Close() error { return nil }
