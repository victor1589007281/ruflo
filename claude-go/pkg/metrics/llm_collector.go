// Package metrics 中的 LLM 指标采集器。
//
// 对于 claude-go 各处 (主进程 / feishu bot / dashboard 内部诊断) 发起的 LLM 调用,
// 通过 api.SetGlobalLLMMetricsHook 把每次调用的 token / 延迟 / 错误码 / 重试等
// 数据以 MetricEvent 形式 append 到 {stateDir}/metrics/llm.jsonl, 方便 dashboard
// 统一展示:
//   - llm_call_count / llm_success_count / llm_error_count
//   - llm_duration_sec (单次耗时), llm_*tokens, llm_retry_count
//   - 按错误原因细分的计数: llm_rate_limit_count / llm_overload_count / ...
package metrics

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

var (
	llmGlobalMu sync.Mutex
	llmGlobal   *Collector
)

// InitGlobalLLMCollector 幂等地在指定 stateDir 上启动全局 LLM 指标采集。
// 多次调用使用同一个采集器实例。返回底层 Collector, 方便其它模块复用。
func InitGlobalLLMCollector(stateDir string) *Collector {
	llmGlobalMu.Lock()
	defer llmGlobalMu.Unlock()
	if llmGlobal != nil {
		return llmGlobal
	}
	c := NewCollector(stateDir)
	llmGlobal = c
	llmPath := filepath.Join(stateDir, "metrics", "llm.jsonl")
	log.Printf("[metrics] LLM 指标采集已初始化: %s", llmPath)
	api.SetGlobalLLMMetricsHook(func(rec api.LLMCallRecord) {
		recordLLMCall(c, rec)
	})
	return c
}

// GlobalLLMCollector 返回已初始化的全局采集器 (可能为 nil)。
func GlobalLLMCollector() *Collector {
	llmGlobalMu.Lock()
	defer llmGlobalMu.Unlock()
	return llmGlobal
}

func recordLLMCall(c *Collector, rec api.LLMCallRecord) {
	labels := map[string]string{
		"model":   firstNonEmptyStr(rec.Model, "unknown"),
		"status":  rec.Status,
		"source":  firstNonEmptyStr(rec.Source, "unknown"),
		"request": firstNonEmptyStr(rec.Request, "messages"),
	}
	if rec.Purpose != "" {
		labels["purpose"] = rec.Purpose
	}
	if rec.StopReason != "" {
		labels["stop_reason"] = rec.StopReason
	}
	if rec.ErrorKind != "" {
		labels["error_kind"] = rec.ErrorKind
	}
	if rec.Stream {
		labels["stream"] = "1"
	}
	if rec.HTTPStatus != 0 {
		labels["http_status"] = intStr(rec.HTTPStatus)
	}

	// 同一次 LLM 调用的所有指标共享同一个时间戳,
	// 确保 dashboard 的 handleLLMStats 能正确按 (ts, model) 分组聚合。
	ts := rec.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	c.RecordAtTime("llm", MLLMCallCount, 1, labels, ts)
	c.RecordAtTime("llm", MLLMDurationSec, rec.DurationSec, labels, ts)
	if rec.InputTokens > 0 {
		c.RecordAtTime("llm", MLLMInputTokens, float64(rec.InputTokens), labels, ts)
	}
	if rec.OutputTokens > 0 {
		c.RecordAtTime("llm", MLLMOutputTokens, float64(rec.OutputTokens), labels, ts)
	}
	if rec.CacheReadTokens > 0 {
		c.RecordAtTime("llm", MLLMCacheReadTokens, float64(rec.CacheReadTokens), labels, ts)
	}
	if rec.CacheCreationTokens > 0 {
		c.RecordAtTime("llm", MLLMCacheCreateTokens, float64(rec.CacheCreationTokens), labels, ts)
	}
	if rec.TotalTokens > 0 {
		c.RecordAtTime("llm", MLLMTotalTokens, float64(rec.TotalTokens), labels, ts)
	}
	if rec.Retries > 0 {
		c.RecordAtTime("llm", MLLMRetryCount, float64(rec.Retries), labels, ts)
	}

	switch rec.Status {
	case "success", "retry_success":
		c.RecordAtTime("llm", MLLMSuccessCount, 1, labels, ts)
	case "error":
		c.RecordAtTime("llm", MLLMErrorCount, 1, labels, ts)
	}

	switch rec.ErrorKind {
	case "rate_limit":
		c.RecordAtTime("llm", MLLMRateLimitCount, 1, labels, ts)
	case "overloaded":
		c.RecordAtTime("llm", MLLMOverloadCount, 1, labels, ts)
	case "timeout":
		c.RecordAtTime("llm", MLLMTimeoutCount, 1, labels, ts)
	case "refusal":
		c.RecordAtTime("llm", MLLMRefusalCount, 1, labels, ts)
	case "prompt_too_long":
		c.RecordAtTime("llm", MLLMPromptTooLong, 1, labels, ts)
	}

	// 限流 / 熔断器事件
	if rec.GuardWaitSec > 0 {
		c.RecordAtTime("llm", MLLMGuardWaitSec, rec.GuardWaitSec, labels, ts)
	}
	if rec.CircuitOpened {
		c.RecordAtTime("llm", MLLMCircuitTrips, 1, labels, ts)
	}
	if rec.CircuitBlocked {
		blockedLabels := copyLabels(labels)
		blockedLabels["blocked"] = "1"
		c.RecordAtTime("llm", MLLMCircuitOpenGauge, 1, blockedLabels, ts)
	}
}

// GuardSample RateLimitGuard 的瞬时采样 (由调用方从 api.GuardSnapshot 映射)。
// 避免 metrics 反向依赖 api 包。
type GuardSample struct {
	InFlight         float64
	MaxParallel      float64
	RPMAvailable     float64
	PauseSecondsLeft float64
}

// CircuitSample 熔断器瞬时采样。
type CircuitSample struct {
	Open             bool
	ConsecutiveFails float64
}

// SampleGuardSnapshot 周期性写入 RateLimitGuard + 熔断器的 gauge 快照。
// dashboard / daemon 应每 5-10s 调一次, 让时序图能展现实时并发 / RPM 剩余 / 熔断状态。
// 两个参数任意一个为 nil 时对应字段跳过。
func SampleGuardSnapshot(module string, g *GuardSample, cb *CircuitSample) {
	c := GlobalLLMCollector()
	if c == nil {
		return
	}
	labels := map[string]string{"module": firstNonEmptyStr(module, "unknown"), "kind": "sample"}
	if g != nil {
		c.RecordWithLabels("llm", MLLMGuardInFlight, g.InFlight, labels)
		c.RecordWithLabels("llm", MLLMGuardMaxParallel, g.MaxParallel, labels)
		c.RecordWithLabels("llm", MLLMGuardRPMTokens, g.RPMAvailable, labels)
		c.RecordWithLabels("llm", MLLMGuardPauseSec, g.PauseSecondsLeft, labels)
	}
	if cb != nil {
		open := 0.0
		if cb.Open {
			open = 1
		}
		c.RecordWithLabels("llm", MLLMCircuitOpenGauge, open, labels)
		c.RecordWithLabels("llm", MLLMCircuitFailStreak, cb.ConsecutiveFails, labels)
	}
}

func copyLabels(src map[string]string) map[string]string {
	out := make(map[string]string, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func intStr(v int) string {
	// 避免引入 strconv 包 (已经间接依赖但保持简洁)。
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := [20]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
