package guidance

import (
	"fmt"
	"sync"
	"time"
)

// 本文件：规则演化追踪。按 ruleID 追加版本迁移记录；RevertEvolution 弹出最近一条（栈语义）。

// RuleEvolutionRecord 单条规则从 old 到 new 的版本变迁及原因、可选评分、时间戳。
type RuleEvolutionRecord struct {
	RuleID     string    `json:"rule_id"`     // 规则 ID
	OldVersion string    `json:"old_version"` // 旧版本标识
	NewVersion string    `json:"new_version"` // 新版本标识
	Reason     string    `json:"reason"`      // 变更原因说明
	Score      float64   `json:"score"`       // 预留：与优化器评分联动
	Timestamp  time.Time `json:"timestamp"`   // UTC 记录时间
}

// EvolutionTracker 线程安全地按规则 ID 存储演化历史切片。
type EvolutionTracker struct {
	mu     sync.Mutex                       // 保护 byRule
	byRule map[string][]RuleEvolutionRecord // ruleID -> 时间有序记录
}

// NewEvolutionTracker 创建空追踪器。
func NewEvolutionTracker() *EvolutionTracker {
	return &EvolutionTracker{byRule: make(map[string][]RuleEvolutionRecord)}
}

// TrackEvolution 追加一条记录（Score 暂固定 0，待与评分管线对接）。
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

// GetEvolutionHistory 返回某规则历史副本（从旧到新顺序）。
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

// RevertEvolution 删除该规则最近一条记录；若为空则返回错误；删至空时移除 map 键。
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
