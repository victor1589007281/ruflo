// metrics.go — 引擎级共享指标 (G10)。
//
// 所有新组件共享一个 EngineMetrics 实例, 便于 dashboard/telemetry 统一采集。
// 设计参考:
//   - DeepSeek V3 可观测架构 (分桶计数 + 快照导出)
//   - Anthropic 客户端 telemetry (cached_tokens/input_tokens 分项暴露)
//
// 全部字段使用 atomic.Int64 或 sync.Map, 允许 lock-free 读写。
package internal_hook

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

	// Advisor push 模式主动咨询次数
	AdvisorCheckpoints atomic.Int64

	// 延迟 (单调累加, 平均需除以 TurnsTotal)
	TotalTurnLatencyMs atomic.Int64

	// 首 token 延迟 (按模型分桶, 单调累加 ms, 平均需除以对应 counts)
	firstTokenLatencies sync.Map // modelName → *atomic.Int64
	firstTokenCounts    sync.Map // modelName → *atomic.Int64

	// 最近一次 turn 时间戳
	LastTurnAt atomic.Int64 // unix nano

	// Trajectory
	TrajSuccessRecorded atomic.Int64
	TrajFailRecorded    atomic.Int64

	// =========================================================================
	// 消息链指标 (由 MessageMetricsHook 在 PhasePreRequest 阶段写入)
	// =========================================================================
	// MsgChainTotalRecorded 记录的消息链分析次数（每轮 API 请求前计数一次）
	MsgChainTotalRecorded atomic.Int64
	// MsgChainTotalTokens 所有消息链的估算 token 总数（字符数/4 累加）
	MsgChainTotalTokens atomic.Int64
	// MsgChainMaxLength 历史最大消息链长度（消息条数）
	MsgChainMaxLength atomic.Int64
	// MsgChainMaxTokens 历史最大消息链估算 token 数
	MsgChainMaxTokens atomic.Int64

	// =========================================================================
	// ToolResult 指标 (由 MessageMetricsHook 在 PhasePreRequest 阶段写入)
	// =========================================================================
	// ToolResultsTotal 累计 tool_result 块总数
	ToolResultsTotal atomic.Int64
	// ToolResultsSuccess 累计成功 tool_result 数（IsError=false）
	ToolResultsSuccess atomic.Int64
	// ToolResultsError 累计错误 tool_result 数（IsError=true）
	ToolResultsError atomic.Int64
	// ToolResultsTotalChars 累计 tool_result 字符总数（用于计算平均值）
	ToolResultsTotalChars atomic.Int64

	// =========================================================================
	// 压缩/过滤效果指标 (由 MessageMetricsHook 在 PhasePreRequest 阶段写入)
	// =========================================================================
	// FilterDeletedUnits FilterPureToolUseUnits 累计删除的原子单元数
	FilterDeletedUnits atomic.Int64
	// CompressTotalBeforeChars CompressMessageContent 压缩前总字符数
	CompressTotalBeforeChars atomic.Int64
	// CompressTotalAfterChars CompressMessageContent 压缩后总字符数
	CompressTotalAfterChars atomic.Int64

	// =========================================================================
	// MemGPT 预留指标 (待 MemGPT 实施后填充)
	// =========================================================================
	// MemGPTWorkingTokens Working Memory 累计 token 数
	MemGPTWorkingTokens atomic.Int64
	// MemGPTArchivalTokens Archival Memory 累计 token 数
	MemGPTArchivalTokens atomic.Int64
	// MemGPTRecallHits Recall Memory 向量检索命中次数
	MemGPTRecallHits atomic.Int64
	// MemGPTRecallMisses Recall Memory 向量检索未命中次数
	MemGPTRecallMisses atomic.Int64
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

// RecordFirstTokenLatency 记录某个模型的首 token 延迟 (ms)。
// 每次 API 流式调用在收到第一个 content_block_delta 时调用一次。
func (m *EngineMetrics) RecordFirstTokenLatency(model string, d time.Duration) {
	if m == nil || model == "" {
		return
	}
	v, _ := m.firstTokenLatencies.LoadOrStore(model, new(atomic.Int64))
	v.(*atomic.Int64).Add(d.Milliseconds())
	c, _ := m.firstTokenCounts.LoadOrStore(model, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
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

	// 首 token 延迟快照: model → {avg_ms, count}
	firstToken := map[string]map[string]int64{}
	m.firstTokenCounts.Range(func(k, v any) bool {
		model := k.(string)
		count := v.(*atomic.Int64).Load()
		var total int64
		if tv, ok := m.firstTokenLatencies.Load(model); ok {
			total = tv.(*atomic.Int64).Load()
		}
		avg := int64(0)
		if count > 0 {
			avg = total / count
		}
		firstToken[model] = map[string]int64{"avg_ms": avg, "count": count}
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
		"first_token_latency_ms":    firstToken,
		"budget_degradations":       budget,
		"errors_by_family":          errs,
		"last_turn_at":              m.LastTurnAt.Load(),
	}
}
