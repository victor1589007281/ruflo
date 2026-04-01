package guidance

import (
	"sync"
	"time"
)

// 本文件：多轮会话继续门控。按 sessionID 记录首次出现时间与最近 turnCount，超过 MaxTurns 或 MaxDuration 则 Block。

// ContinueGate 限制会话轮次与总时长，防止无限对话消耗配额。
type ContinueGate struct {
	mu          sync.Mutex               // 保护 sessions
	MaxTurns    int                      // 允许的最大轮次（>0 时生效）
	MaxDuration time.Duration            // 自首次 Evaluate 起最大时长
	sessions    map[string]*sessionState // sessionID -> 状态
}

// sessionState 单会话的起始时间与最近报告的轮次。
type sessionState struct {
	started   time.Time // 首次 Evaluate 时间
	turnCount int       // 最近 turnCount
}

// NewContinueGate 默认 MaxTurns=100、MaxDuration=24h。
func NewContinueGate() *ContinueGate {
	return &ContinueGate{
		MaxTurns:    100,
		MaxDuration: 24 * time.Hour,
		sessions:    make(map[string]*sessionState),
	}
}

// EvaluateContinuation 懒创建 session：更新 turnCount；超限返回 Block，否则 Allow。
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

// Reset 删除某会话的门控状态（用于新会话或人工放行）。
func (c *ContinueGate) Reset(sessionID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.mu.Unlock()
}
