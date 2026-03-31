package guidance

import (
	"sync"
	"time"
)

// ContinueGate limits session length and duration.
type ContinueGate struct {
	mu         sync.Mutex
	MaxTurns   int
	MaxDuration time.Duration
	sessions   map[string]*sessionState
}

type sessionState struct {
	started   time.Time
	turnCount int
}

// NewContinueGate returns a gate with defaults (100 turns, 24h).
func NewContinueGate() *ContinueGate {
	return &ContinueGate{
		MaxTurns:    100,
		MaxDuration: 24 * time.Hour,
		sessions:    make(map[string]*sessionState),
	}
}

// EvaluateContinuation returns whether another turn is allowed.
func (c *ContinueGate) EvaluateContinuation(sessionID string, turnCount int) GateResult {
	if c == nil || sessionID == "" {
		return GateResult{Decision: GateAllow}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.sessions[sessionID]
	if !ok {
		st = &sessionState{started: time.Now().UTC()}
		c.sessions[sessionID] = st
	}
	st.turnCount = turnCount
	if c.MaxTurns > 0 && turnCount > c.MaxTurns {
		return GateResult{
			RuleID:    "G-CONT-TURNS",
			Decision:  GateBlock,
			Reason:    "max turns exceeded",
			RiskClass: RiskMedium,
		}
	}
	if c.MaxDuration > 0 && time.Since(st.started) > c.MaxDuration {
		return GateResult{
			RuleID:    "G-CONT-DURATION",
			Decision:  GateBlock,
			Reason:    "max session duration exceeded",
			RiskClass: RiskMedium,
		}
	}
	return GateResult{Decision: GateAllow}
}

// Reset clears continuation state for a session.
func (c *ContinueGate) Reset(sessionID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.mu.Unlock()
}
