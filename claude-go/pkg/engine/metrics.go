// metrics.go — 引擎级共享指标 (G10)。
//
// 所有新组件共享一个 EngineMetrics 实例, 便于 dashboard/telemetry 统一采集。
// 设计参考:
//   - DeepSeek V3 可观测架构 (分桶计数 + 快照导出)
//   - Anthropic 客户端 telemetry (cached_tokens/input_tokens 分项暴露)
//
// 全部字段使用 atomic.Int64 或 sync.Map, 允许 lock-free 读写。
package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// EngineMetrics 引擎级指标集合。
// 所有计数器 lock-free, 无读写竞争。
type EngineMetrics struct {
	// 轮次统计
	TurnsTotal   atomic.Int64
	TurnsSuccess atomic.Int64
	TurnsAborted atomic.Int64
	TurnsError   atomic.Int64

	// 工具统计
	ToolCallsTotal       atomic.Int64
	ToolLoopsDetected    atomic.Int64
	ToolLoopsSuppressed  atomic.Int64
	ProgressLoopsDetected atomic.Int64
	JSONRepairsApplied   atomic.Int64
	JSONRepairsFailed    atomic.Int64

	// Prompt Cache
	CacheHits   atomic.Int64
	CacheMisses atomic.Int64

	// 预算分级
	budgetDegradations sync.Map // BudgetLevel → *atomic.Int64

	// 错误族
	errorsByFamily sync.Map // ErrorFamily → *atomic.Int64

	// 停止信号
	StopSuggestionsEmitted atomic.Int64
	StopSuggestionsHonored atomic.Int64

	// 延迟 (单调累加, 平均需除以 TurnsTotal)
	TotalTurnLatencyMs atomic.Int64

	// 最近一次 turn 时间戳
	LastTurnAt atomic.Int64 // unix nano

	// Trajectory
	TrajSuccessRecorded atomic.Int64
	TrajFailRecorded    atomic.Int64
}

// NewEngineMetrics 构造默认指标对象。
func NewEngineMetrics() *EngineMetrics {
	return &EngineMetrics{}
}

// RecordCache 记录 cache 命中/未命中。
func (m *EngineMetrics) RecordCache(hit bool) {
	if m == nil {
		return
	}
	if hit {
		m.CacheHits.Add(1)
	} else {
		m.CacheMisses.Add(1)
	}
}

// RecordBudgetDegrade 记录某个预算等级的降级动作。
func (m *EngineMetrics) RecordBudgetDegrade(level int) {
	if m == nil {
		return
	}
	v, _ := m.budgetDegradations.LoadOrStore(level, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// RecordError 记录某个错误族次数。
func (m *EngineMetrics) RecordError(family int) {
	if m == nil {
		return
	}
	v, _ := m.errorsByFamily.LoadOrStore(family, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// RecordTurnLatency 累计单轮延迟 (ms)。
func (m *EngineMetrics) RecordTurnLatency(d time.Duration) {
	if m == nil {
		return
	}
	m.TotalTurnLatencyMs.Add(d.Milliseconds())
	m.LastTurnAt.Store(time.Now().UnixNano())
}

// CacheHitRate 返回 cache 命中率 (0-1)。
func (m *EngineMetrics) CacheHitRate() float64 {
	if m == nil {
		return 0
	}
	hits := m.CacheHits.Load()
	miss := m.CacheMisses.Load()
	total := hits + miss
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// AvgTurnLatencyMs 返回平均每 turn 延迟 (ms)。
func (m *EngineMetrics) AvgTurnLatencyMs() int64 {
	if m == nil {
		return 0
	}
	total := m.TurnsTotal.Load()
	if total == 0 {
		return 0
	}
	return m.TotalTurnLatencyMs.Load() / total
}

// Snapshot 导出一个 map 形式的只读快照, 供 dashboard 采集。
// 保证对并发写入是安全的 (atomic load), 但非原子快照 (各字段间可能短暂不一致)。
func (m *EngineMetrics) Snapshot() map[string]any {
	if m == nil {
		return map[string]any{}
	}

	budget := map[int]int64{}
	m.budgetDegradations.Range(func(k, v any) bool {
		budget[k.(int)] = v.(*atomic.Int64).Load()
		return true
	})

	errs := map[int]int64{}
	m.errorsByFamily.Range(func(k, v any) bool {
		errs[k.(int)] = v.(*atomic.Int64).Load()
		return true
	})

	return map[string]any{
		"turns_total":               m.TurnsTotal.Load(),
		"turns_success":             m.TurnsSuccess.Load(),
		"turns_aborted":             m.TurnsAborted.Load(),
		"turns_error":               m.TurnsError.Load(),
		"tool_calls_total":          m.ToolCallsTotal.Load(),
		"tool_loops_detected":       m.ToolLoopsDetected.Load(),
		"tool_loops_suppressed":     m.ToolLoopsSuppressed.Load(),
		"progress_loops_detected":   m.ProgressLoopsDetected.Load(),
		"json_repairs_applied":      m.JSONRepairsApplied.Load(),
		"json_repairs_failed":       m.JSONRepairsFailed.Load(),
		"cache_hits":                m.CacheHits.Load(),
		"cache_misses":              m.CacheMisses.Load(),
		"cache_hit_rate":            m.CacheHitRate(),
		"stop_suggestions_emitted":  m.StopSuggestionsEmitted.Load(),
		"stop_suggestions_honored":  m.StopSuggestionsHonored.Load(),
		"traj_success":              m.TrajSuccessRecorded.Load(),
		"traj_fail":                 m.TrajFailRecorded.Load(),
		"avg_turn_latency_ms":       m.AvgTurnLatencyMs(),
		"budget_degradations":       budget,
		"errors_by_family":          errs,
		"last_turn_at":              m.LastTurnAt.Load(),
	}
}
