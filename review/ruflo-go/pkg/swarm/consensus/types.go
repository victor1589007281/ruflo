// 共识子包公共类型定义。
package consensus

import (
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// Config 共识引擎构造配置。
//   - Algorithm: 选用的共识算法（raft/byzantine/gossip/crdt/quorum）
//   - NodeID: 本节点唯一标识
//   - Peers: 参与共识的所有节点 ID（含或不含自身，各引擎内部会补齐）
//   - ByzantineF: 拜占庭容错上限 f（n ≥ 3f+1）
//   - GossipFanout: 每轮传播的目标邻居数（默认 3）
//   - GossipTTL: 最大传播轮数（默认 16）
//   - QuorumSize: 提交所需票数（0 = 自动使用多数派 ⌊n/2⌋+1）
type Config struct {
	Algorithm    api.ConsensusAlgorithm
	NodeID       string
	Peers        []string
	ByzantineF   int
	GossipFanout int
	GossipTTL    int
	QuorumSize   int
}

// Proposal 进行中的共识提案。
//   - Term: Raft 任期号 / Byzantine 视图号
//   - View: 拜占庭协议视图编号
type Proposal struct {
	ID        string
	Value     []byte
	Proposer  string
	Term      uint64
	View      uint64
	CreatedAt time.Time
}

// Vote 参与者投票记录。
type Vote struct {
	ProposalID string
	VoterID    string
	Approve    bool
	Term       uint64
	View       uint64
	Timestamp  time.Time
}

// Result 共识结果，由 AwaitConsensus 返回。
//   - Committed: 是否已达成共识并提交
//   - Term: 提交时的任期/轮次
//   - Err: 错误信息（超时、未达法定人数等）
type Result struct {
	ProposalID string
	Committed  bool
	Value      []byte
	Term       uint64
	Err        error
	FinishedAt time.Time
}
