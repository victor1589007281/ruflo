package guidance

// TrustScore aggregates trust for an agent.
type TrustScore struct {
	AgentID string             `json:"agent_id"`
	Score   float64            `json:"score"`
	Factors map[string]float64 `json:"factors,omitempty"`
}

// AgentHistoryEvent is a minimal audit record for trust computation.
type AgentHistoryEvent struct {
	Success bool
	Weight  float64
}

// ComputeTrust derives a 0..1 score from recent outcomes.
func ComputeTrust(agentID string, history []AgentHistoryEvent) TrustScore {
	ts := TrustScore{AgentID: agentID, Factors: make(map[string]float64)}
	if len(history) == 0 {
		ts.Score = 0.5
		ts.Factors["default"] = 0.5
		return ts
	}
	var num, den float64
	for i, ev := range history {
		w := ev.Weight
		if w <= 0 {
			w = 1
		}
		if ev.Success {
			num += w
		}
		den += w
		_ = i
	}
	rate := 0.5
	if den > 0 {
		rate = num / den
	}
	ts.Factors["success_rate"] = rate
	recency := 1.0
	if len(history) > 10 {
		recency = 0.9
	}
	ts.Factors["recency"] = recency
	ts.Score = rate*0.85 + recency*0.15
	if ts.Score > 1 {
		ts.Score = 1
	}
	if ts.Score < 0 {
		ts.Score = 0
	}
	return ts
}

// IsTrusted returns true when score meets the minimum threshold.
func IsTrusted(agentID string, minScore float64, history []AgentHistoryEvent) bool {
	if minScore <= 0 {
		return true
	}
	return ComputeTrust(agentID, history).Score >= minScore
}
