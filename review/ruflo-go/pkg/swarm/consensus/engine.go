// 本文件定义分布式共识引擎的统一接口 Engine，以及工厂函数 NewEngine 与启发式算法选择 SelectOptimalAlgorithm。
//
// 支持的算法实现分布在 raft.go、byzantine.go、gossip.go、crdt.go、quorum.go，均实现 propose / await 语义供上层 swarm 调用。
package consensus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// Engine 分布式共识引擎统一接口。所有实现（Raft/Byzantine/Gossip/CRDT/Quorum）均满足此接口：
//   - AddNode/RemoveNode: 动态变更集群成员
//   - Propose: 提交提案（同步或异步），返回提案 ID
//   - AwaitConsensus: 阻塞等待提案结果（已提交/超时/错误）
//   - GetState: 返回角色标签（如 Raft 返回 "leader"/"follower"/"candidate"）
//   - IsLeader/GetLeaderID: Leader 状态查询（无 Leader 的算法如 Gossip/CRDT 总返回 false/""）
type Engine interface {
	AddNode(id string) error
	RemoveNode(id string) error
	Propose(ctx context.Context, value []byte) (string, error)
	AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error)
	NodeID() string
	Close() error
	GetState() string
	IsLeader() bool
	GetLeaderID() string
}

var (
	ErrUnknownAlgorithm = errors.New("consensus: unknown algorithm")
	ErrNoNodes          = errors.New("consensus: no nodes registered")
)

// NewEngine 工厂函数：根据 Config.Algorithm 创建对应的共识引擎实例。
func NewEngine(cfg Config) (Engine, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("consensus: NodeID required")
	}
	switch cfg.Algorithm {
	case api.ConsensusRaft:
		return newRaftConsensus(cfg), nil
	case api.ConsensusByzantine:
		return newByzantineConsensus(cfg), nil
	case api.ConsensusGossip:
		return newGossipConsensus(cfg), nil
	case api.ConsensusCRDT:
		return newCRDTConsensus(cfg), nil
	case api.ConsensusQuorum:
		return newQuorumConsensus(cfg), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownAlgorithm, cfg.Algorithm)
	}
}

// SelectOptimalAlgorithm picks an algorithm from heuristics (raft, byzantine, gossip, crdt, quorum).
func SelectOptimalAlgorithm(byzantineNeeded bool, largeScale bool, nodeCount int) api.ConsensusAlgorithm {
	if byzantineNeeded && nodeCount >= 4 {
		return api.ConsensusByzantine
	}
	if largeScale || nodeCount > 32 {
		return api.ConsensusGossip
	}
	if nodeCount >= 3 && nodeCount <= 12 {
		return api.ConsensusQuorum
	}
	if nodeCount > 12 && nodeCount <= 32 {
		return api.ConsensusCRDT
	}
	return api.ConsensusRaft
}
