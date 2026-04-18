// Package metrics — 指标中英文目录 (MetricCatalog)
//
// 用途:
//   1. 所有被记录的 metric 在 dashboard 展示时同时显示英文 name + 中文描述
//   2. 支撑 /api/metrics/catalog 接口, 让前端做全量指标审计: 每个指标是否有数据、是否被可视化
//   3. 统一单位 / 采集类型 (counter/gauge/histogram/rate), 避免前端做猜测
//
// 设计参考: Prometheus metric_metadata + OpenTelemetry Instrument 类型
package metrics

import "sort"

// MetricKind 指标的采集语义, 前端按此选图表类型:
//   - counter   累加计数 → 时间序列折线 (cumulative / delta)
//   - gauge     瞬时值 → 时间序列折线 / 仪表盘
//   - histogram 分布 → 直方图 / 分位线
//   - rate      速率 → 时间序列 (单位 per-sec/min)
type MetricKind string

const (
	KindCounter   MetricKind = "counter"
	KindGauge     MetricKind = "gauge"
	KindHistogram MetricKind = "histogram"
	KindRate      MetricKind = "rate"
)

// MetricDesc 单个 metric 的元数据, 通过 /api/metrics/catalog 返回给前端。
type MetricDesc struct {
	Module string     `json:"module"` // 所属模块 (llm / team / evolution / dreaming / cron / memory / task)
	Name   string     `json:"name"`   // metric 英文名 (JSONL 里存储的 name 字段)
	ZH     string     `json:"zh"`     // 中文描述
	EN     string     `json:"en"`     // 英文描述
	Unit   string     `json:"unit"`   // 单位 (sec / count / tokens / ratio / bytes / "")
	Kind   MetricKind `json:"kind"`   // 采集语义
	// Panel 建议的 dashboard 展示位置 (overview / team / llm / dreaming / evolution / cron / memory / task)
	Panel string `json:"panel"`
}

// catalog 内置中英文目录。顺序: 按模块分组, 模块内按可视化重要性排列。
// 维护约定: 新增 metric 常量时, 必须同时在这里补一条, 否则 dashboard 全量审计会标记为 "missing_desc"。
var catalog = []MetricDesc{
	// ── LLM 指标 (跨模块通用) ────────────────────────────────
	{Module: "llm", Name: MLLMCallCount, ZH: "LLM 调用总次数", EN: "Total LLM calls", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMSuccessCount, ZH: "LLM 调用成功数", EN: "Successful LLM calls", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMErrorCount, ZH: "LLM 调用失败数", EN: "Failed LLM calls", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMRetryCount, ZH: "LLM 重试次数", EN: "LLM retries", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMDurationSec, ZH: "单次调用耗时 (秒)", EN: "Per-call latency", Unit: "sec", Kind: KindHistogram, Panel: "llm"},
	{Module: "llm", Name: MLLMInputTokens, ZH: "输入 token 数", EN: "Input tokens", Unit: "tokens", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMOutputTokens, ZH: "输出 token 数", EN: "Output tokens", Unit: "tokens", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMCacheReadTokens, ZH: "缓存命中 token", EN: "Cache hit tokens", Unit: "tokens", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMCacheCreateTokens, ZH: "缓存创建 token", EN: "Cache create tokens", Unit: "tokens", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMTotalTokens, ZH: "总 token 消耗", EN: "Total tokens", Unit: "tokens", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMRateLimitCount, ZH: "限流 (429) 次数", EN: "Rate-limited (429) count", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMOverloadCount, ZH: "过载 (503/529) 次数", EN: "Overloaded (503/529) count", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMTimeoutCount, ZH: "超时次数", EN: "Timeout count", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMRefusalCount, ZH: "拒答次数", EN: "Refusal count", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMPromptTooLong, ZH: "Prompt 超长次数", EN: "Prompt-too-long count", Unit: "count", Kind: KindCounter, Panel: "llm"},

	// LLM 限流 / 熔断
	{Module: "llm", Name: MLLMGuardInFlight, ZH: "当前在途并发数", EN: "In-flight requests", Unit: "count", Kind: KindGauge, Panel: "llm"},
	{Module: "llm", Name: MLLMGuardMaxParallel, ZH: "AIMD 允许最大并发", EN: "AIMD max parallel", Unit: "count", Kind: KindGauge, Panel: "llm"},
	{Module: "llm", Name: MLLMGuardRPMTokens, ZH: "RPM 令牌桶剩余", EN: "RPM tokens available", Unit: "count", Kind: KindGauge, Panel: "llm"},
	{Module: "llm", Name: MLLMGuardPauseSec, ZH: "全局退避剩余秒数", EN: "Global pause seconds left", Unit: "sec", Kind: KindGauge, Panel: "llm"},
	{Module: "llm", Name: MLLMGuardWaitSec, ZH: "准入等待时间", EN: "Guard acquire wait", Unit: "sec", Kind: KindHistogram, Panel: "llm"},
	{Module: "llm", Name: MLLMGuardAIMDCut, ZH: "AIMD 降并发事件", EN: "AIMD concurrency cuts", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMCircuitTrips, ZH: "熔断触发次数", EN: "Circuit breaker trips", Unit: "count", Kind: KindCounter, Panel: "llm"},
	{Module: "llm", Name: MLLMCircuitOpenGauge, ZH: "熔断状态 (0=关/1=开)", EN: "Circuit open (0/1)", Unit: "", Kind: KindGauge, Panel: "llm"},
	{Module: "llm", Name: MLLMCircuitFailStreak, ZH: "连续失败计数", EN: "Consecutive failure streak", Unit: "count", Kind: KindGauge, Panel: "llm"},

	// ── Dreaming ──────────────────────────────────────────
	{Module: "dreaming", Name: MDreamCount, ZH: "Dreaming 整理次数", EN: "Dreaming runs", Unit: "count", Kind: KindCounter, Panel: "dreaming"},
	{Module: "dreaming", Name: MDreamSessionsInput, ZH: "输入会话数", EN: "Input sessions", Unit: "count", Kind: KindGauge, Panel: "dreaming"},
	{Module: "dreaming", Name: MDreamCompressionRatio, ZH: "压缩率 (输入/输出)", EN: "Compression ratio", Unit: "ratio", Kind: KindGauge, Panel: "dreaming"},
	{Module: "dreaming", Name: MDreamDurationSec, ZH: "整理耗时", EN: "Dreaming duration", Unit: "sec", Kind: KindHistogram, Panel: "dreaming"},
	{Module: "dreaming", Name: MDreamErrorCount, ZH: "失败次数", EN: "Dreaming errors", Unit: "count", Kind: KindCounter, Panel: "dreaming"},
	{Module: "dreaming", Name: MDreamOutputSize, ZH: "输出大小 (字符)", EN: "Output size", Unit: "bytes", Kind: KindGauge, Panel: "dreaming"},

	// ── Evolution ──────────────────────────────────────────
	{Module: "evolution", Name: MEvoExperienceCount, ZH: "经验库条目数", EN: "Experience count", Unit: "count", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoTrajectoryCount, ZH: "轨迹总数", EN: "Trajectories", Unit: "count", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoSuccessRate, ZH: "成功轨迹率", EN: "Trajectory success rate", Unit: "ratio", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoUtilizationRate, ZH: "经验使用率", EN: "Utilization rate", Unit: "ratio", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoAvgQuality, ZH: "平均质量分", EN: "Avg quality", Unit: "score", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoQualityMin, ZH: "最低质量分", EN: "Min quality", Unit: "score", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoQualityMax, ZH: "最高质量分", EN: "Max quality", Unit: "score", Kind: KindGauge, Panel: "evolution"},
	{Module: "evolution", Name: MEvoDistillCount, ZH: "经验提炼次数", EN: "Distillations", Unit: "count", Kind: KindCounter, Panel: "evolution"},
	{Module: "evolution", Name: MEvoPruneCount, ZH: "经验剪枝数", EN: "Pruned experiences", Unit: "count", Kind: KindCounter, Panel: "evolution"},
	{Module: "evolution", Name: MEvoFailTrajectory, ZH: "失败轨迹占比", EN: "Failed trajectory %", Unit: "ratio", Kind: KindGauge, Panel: "evolution"},

	// ── Task ──────────────────────────────────────────────
	{Module: "task", Name: MTaskCreatedCount, ZH: "任务创建总数", EN: "Tasks created", Unit: "count", Kind: KindCounter, Panel: "task"},
	{Module: "task", Name: MTaskCompletedCount, ZH: "任务完成总数", EN: "Tasks completed", Unit: "count", Kind: KindCounter, Panel: "task"},
	{Module: "task", Name: MTaskFailedCount, ZH: "任务失败总数", EN: "Tasks failed", Unit: "count", Kind: KindCounter, Panel: "task"},
	{Module: "task", Name: MTaskCompletionRate, ZH: "任务完成率", EN: "Completion rate", Unit: "ratio", Kind: KindGauge, Panel: "task"},

	// ── Team (整团队级) ───────────────────────────────────
	{Module: "team", Name: MTeamRunCount, ZH: "团队运行次数", EN: "Team runs", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamSuccessCount, ZH: "团队成功次数", EN: "Team successes", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamFailCount, ZH: "团队失败次数", EN: "Team failures", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamDurationSec, ZH: "团队整次运行耗时", EN: "Team run duration", Unit: "sec", Kind: KindHistogram, Panel: "team"},
	{Module: "team", Name: MTeamStagePassRate, ZH: "阶段通过率", EN: "Stage pass rate", Unit: "ratio", Kind: KindGauge, Panel: "team"},
	{Module: "team", Name: MTeamEvalPassRate, ZH: "评审通过率", EN: "Eval pass rate", Unit: "ratio", Kind: KindGauge, Panel: "team"},
	{Module: "team", Name: MTeamBuildPassRate, ZH: "编译通过率", EN: "Build pass rate", Unit: "ratio", Kind: KindGauge, Panel: "team"},
	{Module: "team", Name: MTeamOutputAvgLen, ZH: "平均产出长度", EN: "Avg output length", Unit: "bytes", Kind: KindGauge, Panel: "team"},
	{Module: "team", Name: MTeamRoundCount, ZH: "对抗轮数", EN: "Adversarial rounds", Unit: "count", Kind: KindGauge, Panel: "team"},
	{Module: "team", Name: MTeamFileCount, ZH: "产出文件数", EN: "Produced files", Unit: "count", Kind: KindGauge, Panel: "team"},

	// Team Stage 级
	{Module: "team", Name: MTeamStageCount, ZH: "阶段执行次数", EN: "Stage executions", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamStageDurationSec, ZH: "单阶段耗时", EN: "Stage duration", Unit: "sec", Kind: KindHistogram, Panel: "team"},
	{Module: "team", Name: MTeamStageSuccessCount, ZH: "阶段成功次数", EN: "Stage successes", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamStageFailCount, ZH: "阶段失败次数", EN: "Stage failures", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamStageRetryCount, ZH: "阶段重试次数", EN: "Stage retries", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamStageOutputLen, ZH: "阶段产出长度", EN: "Stage output length", Unit: "bytes", Kind: KindGauge, Panel: "team"},
	{Module: "team", Name: MTeamStageToolCalls, ZH: "阶段工具调用数", EN: "Stage tool calls", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamAgentRunCount, ZH: "Agent 触发次数", EN: "Agent runs", Unit: "count", Kind: KindCounter, Panel: "team"},
	{Module: "team", Name: MTeamAgentDurationSec, ZH: "Agent 单次耗时", EN: "Agent duration", Unit: "sec", Kind: KindHistogram, Panel: "team"},
	{Module: "team", Name: MTeamBlackboardWrites, ZH: "黑板写入次数", EN: "Blackboard writes", Unit: "count", Kind: KindCounter, Panel: "team"},

	// ── Cron ──────────────────────────────────────────────
	{Module: "cron", Name: MCronRunCount, ZH: "Cron 触发总数", EN: "Cron runs", Unit: "count", Kind: KindCounter, Panel: "cron"},
	{Module: "cron", Name: MCronSuccessCount, ZH: "Cron 成功次数", EN: "Cron successes", Unit: "count", Kind: KindCounter, Panel: "cron"},
	{Module: "cron", Name: MCronFailCount, ZH: "Cron 失败次数", EN: "Cron failures", Unit: "count", Kind: KindCounter, Panel: "cron"},
	{Module: "cron", Name: MCronDurationSec, ZH: "Cron 运行耗时", EN: "Cron duration", Unit: "sec", Kind: KindHistogram, Panel: "cron"},

	// ── Memory ────────────────────────────────────────────
	{Module: "memory", Name: MMemEntryCount, ZH: "记忆条目数", EN: "Memory entries", Unit: "count", Kind: KindGauge, Panel: "memory"},
	{Module: "memory", Name: MMemAvgAccessCount, ZH: "平均访问次数", EN: "Avg access count", Unit: "count", Kind: KindGauge, Panel: "memory"},
	{Module: "memory", Name: MMemPruneCount, ZH: "记忆修剪次数", EN: "Memory prunes", Unit: "count", Kind: KindCounter, Panel: "memory"},
	{Module: "memory", Name: MMemRetrievalCount, ZH: "记忆检索次数", EN: "Retrievals", Unit: "count", Kind: KindCounter, Panel: "memory"},
}

// Catalog 返回内置的指标目录 (按模块+名称排序, 便于 diff)。
func Catalog() []MetricDesc {
	out := make([]MetricDesc, len(catalog))
	copy(out, catalog)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LookupMetric 查找 (module, name) 对应的描述, 找不到返回 nil。
func LookupMetric(module, name string) *MetricDesc {
	for i := range catalog {
		if catalog[i].Module == module && catalog[i].Name == name {
			return &catalog[i]
		}
	}
	// 宽松匹配: 很多模块把 llm_* 指标写到 llm.jsonl, 但前端查询其它模块时也应返回中文
	for i := range catalog {
		if catalog[i].Name == name {
			return &catalog[i]
		}
	}
	return nil
}

// CatalogByModule 按模块分组返回目录, 供前端全量审计。
func CatalogByModule() map[string][]MetricDesc {
	out := map[string][]MetricDesc{}
	for i := range catalog {
		d := catalog[i]
		out[d.Module] = append(out[d.Module], d)
	}
	for m := range out {
		sort.Slice(out[m], func(i, j int) bool { return out[m][i].Name < out[m][j].Name })
	}
	return out
}
