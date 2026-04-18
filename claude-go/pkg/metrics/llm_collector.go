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
	"sync"

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
		"model":  firstNonEmptyStr(rec.Model, "unknown"),
		"status": rec.Status,
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

	c.RecordWithLabels("llm", MLLMCallCount, 1, labels)
	c.RecordWithLabels("llm", MLLMDurationSec, rec.DurationSec, labels)
	if rec.InputTokens > 0 {
		c.RecordWithLabels("llm", MLLMInputTokens, float64(rec.InputTokens), labels)
	}
	if rec.OutputTokens > 0 {
		c.RecordWithLabels("llm", MLLMOutputTokens, float64(rec.OutputTokens), labels)
	}
	if rec.CacheReadTokens > 0 {
		c.RecordWithLabels("llm", MLLMCacheReadTokens, float64(rec.CacheReadTokens), labels)
	}
	if rec.CacheCreationTokens > 0 {
		c.RecordWithLabels("llm", MLLMCacheCreateTokens, float64(rec.CacheCreationTokens), labels)
	}
	if rec.TotalTokens > 0 {
		c.RecordWithLabels("llm", MLLMTotalTokens, float64(rec.TotalTokens), labels)
	}
	if rec.Retries > 0 {
		c.RecordWithLabels("llm", MLLMRetryCount, float64(rec.Retries), labels)
	}

	switch rec.Status {
	case "success", "retry_success":
		c.RecordWithLabels("llm", MLLMSuccessCount, 1, labels)
	case "error":
		c.RecordWithLabels("llm", MLLMErrorCount, 1, labels)
	}

	switch rec.ErrorKind {
	case "rate_limit":
		c.RecordWithLabels("llm", MLLMRateLimitCount, 1, labels)
	case "overloaded":
		c.RecordWithLabels("llm", MLLMOverloadCount, 1, labels)
	case "timeout":
		c.RecordWithLabels("llm", MLLMTimeoutCount, 1, labels)
	case "refusal":
		c.RecordWithLabels("llm", MLLMRefusalCount, 1, labels)
	case "prompt_too_long":
		c.RecordWithLabels("llm", MLLMPromptTooLong, 1, labels)
	}
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
