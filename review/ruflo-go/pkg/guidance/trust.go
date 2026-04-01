package guidance

// 本文件：Agent 信任评分。ComputeTrust 用加权成功率与历史长度衰减项线性组合为 0..1 分数；IsTrusted 与阈值比较。

// TrustScore 聚合某 Agent 的信任分及分解因子（便于解释）。
type TrustScore struct {
	AgentID string             `json:"agent_id"`          // Agent 标识
	Score   float64            `json:"score"`             // 综合分 0..1
	Factors map[string]float64 `json:"factors,omitempty"` // 如 success_rate、recency
}

// AgentHistoryEvent 信任计算用的最小历史事件（成功与否与权重）。
type AgentHistoryEvent struct {
	Success bool    // 该事件是否成功
	Weight  float64 // 权重，<=0 时按 1 处理
}

// ComputeTrust：无历史时返回 0.5；否则 rate = 加权成功/加权总数；recency 在 history>10 时为 0.9 否则 1；
// Score = rate*0.85 + recency*0.15，并裁剪到 [0,1]。
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

// IsTrusted：minScore<=0 恒 true；否则 ComputeTrust.Score >= minScore。
func IsTrusted(agentID string, minScore float64, history []AgentHistoryEvent) bool {
	if minScore <= 0 {
		return true
	}
	return ComputeTrust(agentID, history).Score >= minScore
}
