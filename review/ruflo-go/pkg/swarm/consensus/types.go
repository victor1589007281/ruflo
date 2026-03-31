package consensus

import (
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// Config drives engine construction.
type Config struct {
	Algorithm    api.ConsensusAlgorithm
	NodeID       string
	Peers        []string
	ByzantineF   int
	GossipFanout int
	GossipTTL    int
	// QuorumSize is votes required to commit (0 = majority ⌊n/2⌋+1).
	QuorumSize int
}

// Proposal is an in-flight agreement unit.
type Proposal struct {
	ID        string
	Value     []byte
	Proposer  string
	Term      uint64
	View      uint64
	CreatedAt time.Time
}

// Vote records a participant vote.
type Vote struct {
	ProposalID string
	VoterID    string
	Approve    bool
	Term       uint64
	View       uint64
	Timestamp  time.Time
}

// Result is returned from AwaitConsensus.
type Result struct {
	ProposalID string
	Committed  bool
	Value      []byte
	Term       uint64
	Err        error
	FinishedAt time.Time
}
