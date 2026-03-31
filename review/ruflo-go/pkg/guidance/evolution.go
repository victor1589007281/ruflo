package guidance

import (
	"fmt"
	"sync"
	"time"
)

// RuleEvolutionRecord tracks a rule version transition.
type RuleEvolutionRecord struct {
	RuleID     string    `json:"rule_id"`
	OldVersion string    `json:"old_version"`
	NewVersion string    `json:"new_version"`
	Reason     string    `json:"reason"`
	Score      float64   `json:"score"`
	Timestamp  time.Time `json:"timestamp"`
}

// EvolutionTracker stores per-rule history and supports revert of the last change.
type EvolutionTracker struct {
	mu     sync.Mutex
	byRule map[string][]RuleEvolutionRecord
}

// NewEvolutionTracker creates an empty tracker.
func NewEvolutionTracker() *EvolutionTracker {
	return &EvolutionTracker{byRule: make(map[string][]RuleEvolutionRecord)}
}

// TrackEvolution appends a record for a rule (score left zero until scoring is wired).
func (t *EvolutionTracker) TrackEvolution(ruleID, oldVer, newVer, reason string) {
	if t == nil || ruleID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byRule[ruleID] = append(t.byRule[ruleID], RuleEvolutionRecord{
		RuleID:     ruleID,
		OldVersion: oldVer,
		NewVersion: newVer,
		Reason:     reason,
		Score:      0,
		Timestamp:  time.Now().UTC(),
	})
}

// GetEvolutionHistory returns all records for a rule (oldest first).
func (t *EvolutionTracker) GetEvolutionHistory(ruleID string) []RuleEvolutionRecord {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.byRule[ruleID]
	out := make([]RuleEvolutionRecord, len(h))
	copy(out, h)
	return out
}

// RevertEvolution removes the most recent evolution record for a rule.
func (t *EvolutionTracker) RevertEvolution(ruleID string) error {
	if t == nil {
		return fmt.Errorf("guidance: nil EvolutionTracker")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.byRule[ruleID]
	if !ok || len(h) == 0 {
		return fmt.Errorf("guidance: no evolution for rule %q", ruleID)
	}
	h = h[:len(h)-1]
	if len(h) == 0 {
		delete(t.byRule, ruleID)
	} else {
		t.byRule[ruleID] = h
	}
	return nil
}
