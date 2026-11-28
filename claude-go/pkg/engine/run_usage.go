// run_usage.go — run 级 LLM 用量累加器 (P1: ndjson result 事件的 usage 段)。
//
// # 为什么不用 CumulativeUsage
//
// CumulativeUsage (engine.go) 有两个不适用之处:
//  1. 只在 queryLoop 累加 —— RunIsolated (agent/团队/stage 路径) 不经过它,
//     而评测与 deck 工单走的恰恰是 RunIsolated;
//  2. 它被 MaxRunTokens 预算闸与压缩阈值消费 —— 往里面塞新口径会动到既有判定。
//
// 本累加器挂在 writeLLMCallSpan 里 (三个调用点: queryLoop 流式 / RunIsolated /
// handoff 交接的唯一收口), 口径 = 每次 LLM 调用上报的 usage 四项求和。
//
// # 与 llm.jsonl 的关系
//
// llm.jsonl (metrics 采集器) 是**进程外**账本, 本累加器是**进程内**账本。
// P1 验收要求两者对账 (差异 >5% 告警) —— 它们必须独立采集才有对账价值,
// 所以这里刻意不复用 metrics 采集器的任何中间状态。
//
// # 并发
//
// RunIsolated 可被 8 个并发 stage 共用一个引擎 (见 cliAgentRunner 的论证),
// writeLLMCallSpan 因此可能并发进入 —— 累加器必须持锁, 不能学 CumulativeUsage
// 的裸 += (那是 queryLoop 单 goroutine 前提下的写法)。
package engine

import (
	"github.com/anthropic/claude-go/pkg/types"
)

// RunUsage 是一次 run 的 LLM 用量聚合快照。
type RunUsage struct {
	// Calls 是 LLM 调用次数 (含失败调用 —— 失败也是 run 成本的一部分,
	// 与 llm_call Span "失败也落" 同一纪律)。
	Calls int `json:"calls"`
	// UsageReported 是上游**上报了** usage 的调用次数。Calls-UsageReported =
	// 用量缺失的调用数 (流式失败天然无 usage), 对账时必须知道分母。
	UsageReported            int `json:"usage_reported"`
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// noteRunUsage 把一次 LLM 调用的用量并入 run 级聚合。writeLLMCallSpan 专用,
// 必须在 TraceStore 守卫**之前**调用 (没有轨迹底座的 run 也要出 usage 段)。
func (e *QueryEngine) noteRunUsage(u *types.Usage) {
	if e == nil {
		return
	}
	e.runUsageMu.Lock()
	defer e.runUsageMu.Unlock()
	e.runUsage.Calls++
	if u == nil {
		return
	}
	e.runUsage.UsageReported++
	e.runUsage.InputTokens += u.InputTokens
	e.runUsage.OutputTokens += u.OutputTokens
	e.runUsage.CacheReadInputTokens += u.CacheReadInputTokens
	e.runUsage.CacheCreationInputTokens += u.CacheCreationInputTokens
}

// RunUsageSnapshot 返回 run 级用量聚合的拷贝。ok=false 表示一次调用都没记到
// (此时调用方应整段省略 usage 字段, 而不是发一个全零段冒充"没花钱")。
func (e *QueryEngine) RunUsageSnapshot() (u RunUsage, ok bool) {
	if e == nil {
		return RunUsage{}, false
	}
	e.runUsageMu.Lock()
	defer e.runUsageMu.Unlock()
	return e.runUsage, e.runUsage.Calls > 0
}

// runUsageMu/runUsage 的零值即可用 —— 字段声明在 QueryEngine 上 (engine.go),
// 这里只放方法与类型, 与 trace_llm.go 收 writeLLMCallSpan 同一布局惯例。
