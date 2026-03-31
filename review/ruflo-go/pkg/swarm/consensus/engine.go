package consensus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// Engine is the consensus abstraction.
type Engine interface {
	AddNode(id string) error
	RemoveNode(id string) error
	Propose(ctx context.Context, value []byte) (string, error)
	AwaitConsensus(ctx context.Context, proposalID string, timeout time.Duration) (Result, error)
	NodeID() string
	Close() error
	// GetState returns a role label: raft "leader"|"follower"|"candidate"; byzantine "primary"|"replica"; others are engine-specific.
	GetState() string
	IsLeader() bool
	GetLeaderID() string
}

var (
	ErrUnknownAlgorithm = errors.New("consensus: unknown algorithm")
	ErrNoNodes          = errors.New("consensus: no nodes registered")
)

// NewEngine creates Raft, Byzantine, Gossip, CRDT, or Quorum based on config.
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
