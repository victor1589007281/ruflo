// Raft 共识算法实现（Ongaro & Ousterhout, USENIX ATC 2014）。
//
// # 核心思想
//
// Raft 将分布式共识分解为三个子问题：
//  1. Leader 选举：使用随机化选举超时防止活锁；Follower 超时后变为 Candidate 发起投票；
//     获得多数票的 Candidate 成为 Leader。
//  2. 日志复制：Leader 接收客户端请求，追加到本地日志后通过 AppendEntries RPC 复制到所有 Follower；
//     当多数派确认后标记为已提交（committed）。
//  3. 安全性：任何已提交的日志条目不会被后续 Leader 覆盖（通过 term + 日志完整性约束）。
//
// # 任期（Term）机制
//
// 每次选举递增 term；节点发现更高 term 自动转为 Follower 并更新本地 term。
// Term 相当于逻辑时钟，保证旧 Leader 无法覆盖新 Leader 的决策。
//
// # 心跳
//
// Leader 周期性发送心跳（空 AppendEntries），重置 Follower 选举计时器，防止不必要的选举。
//
// # 本文件为同进程模拟
//
// 实际 Raft 需要网络 RPC（RequestVote、AppendEntries）；本实现在同一进程内用 goroutine
// 模拟选举循环，通过计数 peer 数量来模拟多数派确认。适用于本地多 Agent 编排的共识场景。
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

// raftRole 节点角色：follower（跟随者）、candidate（候选人）、leader（领导者）。
type raftRole string

const (
	raftFollower  raftRole = "follower"  // 默认角色，接收并转发请求
	raftCandidate raftRole = "candidate" // 选举中，正在拉票
	raftLeader    raftRole = "leader"    // 当选领导者，负责日志复制
)

// raftLogEntry Raft 日志条目：Term 为创建时的任期；Index 为全局递增序号；Command 为用户提案数据。
type raftLogEntry struct {
	Term    uint64
	Index   uint64
	Command []byte
}

// raftConsensus Raft 引擎状态。
// 字段说明：
//   - role: 当前角色（follower/candidate/leader）
//   - term: 当前任期号（单调递增，每次选举 +1）
//   - votedFor: 本任期已投票给的 NodeID（防止重复投票）
//   - log: 日志数组，index 0 为哨兵空条目
//   - commitIndex: 已提交的最高日志索引
//   - peers: 参与共识的所有节点 ID（含自身），保持排序
//   - electionReset: 收到心跳/投票授予时重置选举定时器的信号通道
//   - shutdown: 关闭信号
//   - results/waiters: 提案结果存储与异步等待机制
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

// newRaftConsensus 构造 Raft 引擎：初始化日志（含哨兵）、启动选举循环协程；
// 若集群仅 1 节点则直接自选为 Leader（单节点模式）。
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
	go r.electionLoop() // 启动后台选举循环 goroutine
	if len(peers) == 1 {
		// 单节点集群：无需选举，直接成为 Leader
		r.mu.Lock()
		r.role = raftLeader
		r.leaderID = cfg.NodeID
		r.term = 1
		r.mu.Unlock()
	}
	return r
}

// raftSeed 使用 crypto/rand 生成密码学安全的 PCG 随机种子，避免选举超时可预测。
func raftSeed() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

// NodeID 返回本节点标识。O(1)。
func (r *raftConsensus) NodeID() string { return r.cfg.NodeID }

// GetState 返回角色字符串 follower/candidate/leader。O(1)。
func (r *raftConsensus) GetState() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return string(r.role)
}

// IsLeader 当前是否为 Leader。O(1)。
func (r *raftConsensus) IsLeader() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.role == raftLeader
}

// GetLeaderID 返回已知 Leader 的 NodeID；若本机为 Leader 则可为自身。O(1)。
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

// Close 关闭 shutdown 通道并等待 electionLoop 退出。O(1) 触发。
func (r *raftConsensus) Close() error {
	r.closeOnce.Do(func() { close(r.shutdown) })
	r.runWg.Wait()
	return nil
}

// AddNode 将节点加入 peers 并保持排序。O(n)。
func (r *raftConsensus) AddNode(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !stringSliceContains(r.peers, id) {
		r.peers = append(r.peers, id)
		sort.Strings(r.peers)
	}
	return nil
}

// RemoveNode 从 peers 移除节点。O(n)。
func (r *raftConsensus) RemoveNode(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers = stringSliceRemove(r.peers, id)
	return nil
}

// Propose 仅在 Leader 上接受：追加日志条目，用本地 peer 计数模拟达到 quorum（⌈n/2⌉）即提交，并通知等待者。
// 时间复杂度 O(n)（模拟 ack）；空间 O(|value|) 复制命令。
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

// AwaitConsensus 阻塞直到结果入 results 或超时；通过 waiters 注册 channel。O(1) 注册 + 等待。
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

// storeAndNotify 写入 results 并向所有等待 channel 非阻塞发送。
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

// electionLoop 事件循环：shutdown 退出；electionReset 重置随机选举定时器；超时触发 runElection；
// Leader 心跳 tick 向 electionReset 发脉冲模拟 AppendEntries。O(1) 每事件。
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
	// 转变为 Candidate，递增任期，给自己投票
	r.role = raftCandidate
	r.term++
	r.votedFor = r.cfg.NodeID
	votes := 1
	needed := len(r.peers)/2 + 1 // 多数派阈值 = ⌊n/2⌋+1
	if needed < 1 {
		needed = 1
	}
	for _, p := range r.peers {
		if p == r.cfg.NodeID {
			continue
		}
		// 同进程模拟：默认所有 peer 都投赞成票（实际 Raft 需 RequestVote RPC）
		votes++
		if votes >= needed {
			break
		}
	}
	if votes >= needed {
		// 赢得选举，成为 Leader
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
