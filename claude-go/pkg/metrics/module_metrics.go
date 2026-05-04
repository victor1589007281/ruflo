// Package metrics 持续迭代观测指标体系。
//
// 设计目标: 为 claude-go 各模块提供固定的、可持续跟踪的质量指标,
// 使每次迭代的效果可量化对比 (类似 MLOps 的 experiment tracking)。
//
// 存储: 全部通过 Prometheus 采集, 本地仅保留内存缓冲用于即时查询。
//
// 使用方式:
//
//	collector := metrics.NewCollector(stateDir)
//	collector.Record("dreaming", "dream_compression_ratio", 3.5)
//	collector.RecordWithLabels("team", "stage_pass_rate", 0.85, map[string]string{"workflow": "development"})
//	summary := collector.Summary("dreaming")
package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MetricEvent 单次指标事件 (内存中的原子单元)。
type MetricEvent struct {
	Timestamp time.Time         `json:"ts"`
	Module    string            `json:"module"`
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	RunID     string            `json:"run_id,omitempty"`
}

// ModuleSummary 单模块指标摘要 (供 AI 分析)。
type ModuleSummary struct {
	Module       string                 `json:"module"`
	SnapshotTime time.Time              `json:"snapshot_time"`
	Metrics      map[string]*MetricStat `json:"metrics"`
	TrendAlerts  []string               `json:"trend_alerts,omitempty"`
}

// MetricStat 单指标统计 (最近 N 次的聚合)。
type MetricStat struct {
	Name   string  `json:"name"`
	Count  int     `json:"count"`
	Last   float64 `json:"last"`
	Avg    float64 `json:"avg"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	StdDev float64 `json:"std_dev"`
	Trend  string  `json:"trend"` // "improving", "degrading", "stable"
}

// MetricCumulative 单指标累积统计 (用于 Prometheus counter)。
type MetricCumulative struct {
	Name  string  `json:"name"`
	Total float64 `json:"total"` // 自进程启动以来的累计值
	Count int     `json:"count"` // 事件数
}

// MetricHistogram 单指标直方图统计 (用于 Prometheus histogram)。
type MetricHistogram struct {
	Name   string         `json:"name"`
	Sum    float64        `json:"sum"`
	Count  int            `json:"count"`
	Bucket map[string]int `json:"bucket"` // bucket_upper_bound -> count
}

// Collector 指标采集器, 线程安全, 写入 Prometheus + JSONL 持久化 + 内存缓冲。
type Collector struct {
	mu       sync.Mutex
	buffer   map[string][]MetricEvent // 内存缓冲 (用于 Summary)
	jsonlDir string                   // JSONL 持久化目录, 供 dashboard 按 scrape 节奏读取
}

// NewCollector 创建指标采集器, 在 stateDir/metrics/ 下持久化 JSONL。
func NewCollector(stateDir string) *Collector {
	// 确保 Prometheus 注册已初始化
	PrometheusRegistry()
	jsonlDir := filepath.Join(stateDir, "metrics")
	_ = os.MkdirAll(jsonlDir, 0o755)
	c := &Collector{
		buffer:   make(map[string][]MetricEvent),
		jsonlDir: jsonlDir,
	}
	return c
}

// Record 记录一个指标值。
func (c *Collector) Record(module, name string, value float64) {
	c.RecordWithLabels(module, name, value, nil)
}

// RecordWithLabels 记录带标签的指标值。
func (c *Collector) RecordWithLabels(module, name string, value float64, labels map[string]string) {
	c.RecordAtTime(module, name, value, labels, time.Now())
}

// RecordAtTime 记录指定时间戳的指标值。
// 同一批指标共享时间戳, 确保 dashboard 能正确分组聚合。
// 同时写入 Prometheus 原生指标 (通过 prom_registry.go)。
func (c *Collector) RecordAtTime(module, name string, value float64, labels map[string]string, ts time.Time) {
	evt := MetricEvent{
		Timestamp: ts,
		Module:    module,
		Name:      name,
		Value:     value,
		Labels:    labels,
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buffer[module] = append(c.buffer[module], evt)

	// 同时写入 Prometheus 原生指标
	recordPromMetric(module, name, value, labels)

	// 持久化到 JSONL (供 dashboard scrape)
	c.appendJSONL(module, evt)
}

// RecordRun 记录一个带 RunID 的指标值 (用于团队/任务级别追踪)。
func (c *Collector) RecordRun(module, name string, value float64, runID string, labels map[string]string) {
	evt := MetricEvent{
		Timestamp: time.Now(),
		Module:    module,
		Name:      name,
		Value:     value,
		Labels:    labels,
		RunID:     runID,
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buffer[module] = append(c.buffer[module], evt)

	// 同时写入 Prometheus 原生指标
	recordPromMetric(module, name, value, labels)

	// 持久化到 JSONL (供 dashboard scrape)
	c.appendJSONL(module, evt)
}

// appendJSONL 将事件以 JSONL 格式追加到对应模块的文件。
func (c *Collector) appendJSONL(module string, evt MetricEvent) {
	if c.jsonlDir == "" {
		return
	}
	path := filepath.Join(c.jsonlDir, module+".jsonl")
	line, err := json.Marshal(evt)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
}

// Summary 生成单模块的指标摘要 (基于最近 100 条事件)。
func (c *Collector) Summary(module string) *ModuleSummary {
	c.mu.Lock()
	events := make([]MetricEvent, len(c.buffer[module]))
	copy(events, c.buffer[module])
	c.mu.Unlock()

	summary := &ModuleSummary{
		Module:       module,
		SnapshotTime: time.Now(),
		Metrics:      make(map[string]*MetricStat),
	}

	grouped := make(map[string][]float64)
	for _, e := range events {
		grouped[e.Name] = append(grouped[e.Name], e.Value)
	}

	for name, values := range grouped {
		recent := values
		if len(recent) > 100 {
			recent = recent[len(recent)-100:]
		}
		stat := computeStat(name, recent)
		summary.Metrics[name] = stat

		if alert := detectTrend(name, recent); alert != "" {
			summary.TrendAlerts = append(summary.TrendAlerts, alert)
		}
	}

	return summary
}

// RecentEvents 返回某模块最近 N 条事件 (用于 /metrics Prometheus 端点)。
func (c *Collector) RecentEvents(module string, n int) []MetricEvent {
	c.mu.Lock()
	events := make([]MetricEvent, len(c.buffer[module]))
	copy(events, c.buffer[module])
	c.mu.Unlock()
	if len(events) > n {
		return events[len(events)-n:]
	}
	return events
}

// Cumulative 返回某模块所有指标的累积总和 (用于 Prometheus counter)。
func (c *Collector) Cumulative(module string) map[string]*MetricCumulative {
	c.mu.Lock()
	events := make([]MetricEvent, len(c.buffer[module]))
	copy(events, c.buffer[module])
	c.mu.Unlock()

	grouped := make(map[string]*MetricCumulative)
	for _, e := range events {
		mc, ok := grouped[e.Name]
		if !ok {
			mc = &MetricCumulative{Name: e.Name}
			grouped[e.Name] = mc
		}
		mc.Total += e.Value
		mc.Count++
	}
	return grouped
}

// Histogram 返回某模块指定指标的直方图统计 (用于 Prometheus histogram)。
func (c *Collector) Histogram(module, name string, buckets []float64) *MetricHistogram {
	c.mu.Lock()
	events := make([]MetricEvent, len(c.buffer[module]))
	copy(events, c.buffer[module])
	c.mu.Unlock()

	h := &MetricHistogram{
		Name:   name,
		Bucket: make(map[string]int),
	}
	for _, b := range buckets {
		h.Bucket[fmt.Sprintf("%.6g", b)] = 0
	}
	h.Bucket["+Inf"] = 0

	for _, e := range events {
		if e.Name != name {
			continue
		}
		h.Sum += e.Value
		h.Count++
		for _, b := range buckets {
			if e.Value <= b {
				h.Bucket[fmt.Sprintf("%.6g", b)]++
			}
		}
		h.Bucket["+Inf"]++
	}
	return h
}

// HistogramDefaults 返回默认直方图桶 (适合耗时秒数分布)。
func HistogramDefaults() []float64 {
	return []float64{0.1, 0.5, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600}
}

// AllSummaries 生成所有模块的摘要。
func (c *Collector) AllSummaries() []*ModuleSummary {
	c.mu.Lock()
	modules := make([]string, 0, len(c.buffer))
	for m := range c.buffer {
		modules = append(modules, m)
	}
	c.mu.Unlock()

	sort.Strings(modules)
	var result []*ModuleSummary
	for _, m := range modules {
		result = append(result, c.Summary(m))
	}
	return result
}

// Close no-op (no longer manages file handles).
func (c *Collector) Close() {}

func computeStat(name string, values []float64) *MetricStat {
	n := len(values)
	if n == 0 {
		return &MetricStat{Name: name}
	}

	sum := 0.0
	minV, maxV := values[0], values[0]
	for _, v := range values {
		sum += v
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	avg := sum / float64(n)

	// 标准差
	variance := 0.0
	for _, v := range values {
		variance += (v - avg) * (v - avg)
	}
	stddev := math.Sqrt(variance / float64(n))

	// 趋势: 比较前半 vs 后半平均
	trend := "stable"
	if n >= 4 {
		mid := n / 2
		firstHalf := avg2(values[:mid])
		secondHalf := avg2(values[mid:])
		delta := (secondHalf - firstHalf) / (math.Abs(firstHalf) + 1e-10)
		if delta > 0.1 {
			trend = "improving"
		} else if delta < -0.1 {
			trend = "degrading"
		}
	}

	return &MetricStat{
		Name:   name,
		Count:  n,
		Last:   values[n-1],
		Avg:    avg,
		Min:    minV,
		Max:    maxV,
		StdDev: stddev,
		Trend:  trend,
	}
}

func avg2(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func detectTrend(name string, values []float64) string {
	if len(values) < 6 {
		return ""
	}
	mid := len(values) / 2
	firstAvg := avg2(values[:mid])
	secondAvg := avg2(values[mid:])

	if firstAvg == 0 && secondAvg == 0 {
		return ""
	}

	delta := (secondAvg - firstAvg) / (math.Abs(firstAvg) + 1e-10)
	if delta < -0.2 {
		return fmt.Sprintf("⚠️ %s 显著下降 (%.1f%%), 前半均值=%.2f → 后半均值=%.2f", name, delta*100, firstAvg, secondAvg)
	}
	return ""
}

// ─── 各模块固定指标定义 (作为文档 + 常量) ─────────────────────────────────────

// Dreaming 指标
const (
	MDreamCount            = "dream_count"             // 总整理次数
	MDreamSessionsInput    = "dream_sessions_input"    // 输入会话数
	MDreamCompressionRatio = "dream_compression_ratio" // 压缩率 (输入字符/输出字符)
	MDreamDurationSec      = "dream_duration_sec"      // 整理耗时(秒)
	MDreamErrorCount       = "dream_error_count"       // 失败次数
	MDreamOutputSize       = "dream_output_size"       // 输出大小(字符)

	// V3: Dreaming 健康度指标
	MDreamSessionsPending       = "dream_sessions_pending"            // 待处理会话数
	MDreamHoursSinceLast        = "dream_hours_since_last"            // 距上次 dream 小时数
	MDreamGateBlockSessionsLow  = "dream_gate_block_sessions_low"     // 门控: 会话不足
	MDreamGateBlockTimeShort    = "dream_gate_block_time_short"       // 门控: 时间不足
	MDreamGateBlockDreaming     = "dream_gate_block_already_dreaming" // 门控: 已在执行
	MDreamGateBlockLockHeld     = "dream_gate_block_lock_held"        // 门控: 锁被持有
	MDreamGateBlockScanThrottle = "dream_gate_block_scan_throttle"    // 门控: 扫描节流
	MDreamTriggerSource         = "dream_trigger_source"              // 触发来源
	MDreamConsolidatorFacts     = "dream_consolidator_facts"          // Consolidator 产出事实数
	MDreamConsolidatorContra    = "dream_consolidator_contradictions" // 矛盾检测数
	MDreamConsolidatorPatterns  = "dream_consolidator_patterns"       // 模式发现数
)

// Evolution 指标
const (
	MEvoExperienceCount = "evo_experience_count"    // 经验总数
	MEvoTrajectoryCount = "evo_trajectory_count"    // 轨迹总数
	MEvoSuccessRate     = "evo_success_rate"        // 成功轨迹率
	MEvoUtilizationRate = "evo_utilization_rate"    // 经验使用率 (至少用过1次的/总数)
	MEvoAvgQuality      = "evo_avg_quality"         // 平均质量分
	MEvoQualityMin      = "evo_quality_min"         // 最低质量分
	MEvoQualityMax      = "evo_quality_max"         // 最高质量分
	MEvoDistillCount    = "evo_distill_count"       // 提炼次数
	MEvoPruneCount      = "evo_prune_count"         // 剪枝数量
	MEvoFailTrajectory  = "evo_fail_trajectory_pct" // 失败轨迹占比
)

// Task 指标
const (
	MTaskCreatedCount   = "task_created_count"   // 创建总数
	MTaskCompletedCount = "task_completed_count" // 完成总数
	MTaskFailedCount    = "task_failed_count"    // 失败总数
	MTaskCompletionRate = "task_completion_rate" // 完成率
)

// Team 指标 — 整团队级
const (
	MTeamRunCount      = "team_run_count"       // 运行总数
	MTeamSuccessCount  = "team_success_count"   // 成功次数
	MTeamFailCount     = "team_fail_count"      // 失败次数
	MTeamDurationSec   = "team_duration_sec"    // 运行耗时(秒)
	MTeamStagePassRate = "team_stage_pass_rate" // 阶段通过率
	MTeamEvalPassRate  = "team_eval_pass_rate"  // 评审通过率
	MTeamBuildPassRate = "team_build_pass_rate" // 编译通过率
	MTeamTestPassRate  = "team_test_pass_rate"  // 测试通过率
	MTeamOutputAvgLen  = "team_output_avg_len"  // 平均产出长度
	MTeamRoundCount    = "team_round_count"     // 对抗轮数
	MTeamFileCount     = "team_file_count"      // 产出文件数
)

// Team 指标 — Stage 级 (每次 stage 完成即上报, 可绘制阶段耗时/成功率分布)
const (
	MTeamStageCount        = "team_stage_count"         // 阶段执行次数
	MTeamStageDurationSec  = "team_stage_duration_sec"  // 单阶段耗时
	MTeamStageSuccessCount = "team_stage_success_count" // 单阶段成功次数
	MTeamStageFailCount    = "team_stage_fail_count"    // 单阶段失败次数
	MTeamStageRetryCount   = "team_stage_retry_count"   // 单阶段重试次数
	MTeamStageOutputLen    = "team_stage_output_len"    // 单阶段产出长度
	MTeamStageToolCalls    = "team_stage_tool_calls"    // 单阶段工具调用数
	MTeamAgentRunCount     = "team_agent_run_count"     // Agent 触发次数
	MTeamAgentDurationSec  = "team_agent_duration_sec"  // Agent 单次耗时
	MTeamBlackboardWrites  = "team_blackboard_writes"   // 黑板写入次数
)

// Cron 指标 (从 cron_jobs.json 的历史执行统计)
const (
	MCronRunCount     = "cron_run_count"     // 触发次数
	MCronSuccessCount = "cron_success_count" // 成功次数
	MCronFailCount    = "cron_fail_count"    // 失败次数
	MCronDurationSec  = "cron_duration_sec"  // 运行耗时
)

// Swarm Intel 指标 (群体智能预测/模拟)
const (
	MSwarmRunCount       = "swarm_run_count"        // 预测/模拟总次数
	MSwarmSuccessCount   = "swarm_success_count"    // 成功次数
	MSwarmConsensus      = "swarm_consensus"        // 共识度 (0-1)
	MSwarmBrierScore     = "swarm_brier_score"      // Brier 校准分数
	MSwarmDiversity      = "swarm_diversity"        // 多样性分数 (0-1)
	MSwarmLatencyMs      = "swarm_latency_ms"       // 延迟 (ms)
	MSwarmDebateSkipRate = "swarm_debate_skip_rate" // 辩论跳过率
	MSwarmLLMCalls       = "swarm_llm_calls"        // 单次 LLM 调用数
)

// Memory 指标
const (
	MMemEntryCount     = "mem_entry_count"      // 记忆条目数
	MMemAvgAccessCount = "mem_avg_access_count" // 平均访问次数
	MMemPruneCount     = "mem_prune_count"      // 修剪次数
	MMemRetrievalCount = "mem_retrieval_count"  // 检索次数

	// V3: L2 FactStore 指标
	MFactTotalCount         = "fact_total_count"          // L2 事实总数
	MFactActiveCount        = "fact_active_count"         // 活跃事实数
	MFactArchivedCount      = "fact_archived_count"       // 已归档事实数
	MFactEvergreenCount     = "fact_evergreen_count"      // 永久豁免事实数
	MFactAvgRetention       = "fact_avg_retention"        // 平均保留率
	MFactIngestCount        = "fact_ingest_count"         // 新事实摄入数
	MFactDecayArchivedCount = "fact_decay_archived_count" // 衰减归档数
	MFactConnectionCount    = "fact_connection_count"     // 事实间关联数

	// V3: 失忆风险指标
	MAmnesiaRiskScore     = "amnesia_risk_score"     // 综合失忆风险评分 (0-100)
	MPrecompactFactsSaved = "precompact_facts_saved" // PreCompact 抢救事实数
)

// LLM 指标 (跨模块通用的大模型调用观测)
const (
	MLLMCallCount             = "llm_call_count"              // 调用总数
	MLLMSuccessCount          = "llm_success_count"           // 成功数
	MLLMErrorCount            = "llm_error_count"             // 失败数
	MLLMRetryCount            = "llm_retry_count"             // 重试次数
	MLLMDurationSec           = "llm_duration_sec"            // 单次耗时(秒)
	MLLMInputTokens           = "llm_input_tokens"            // 输入 token
	MLLMOutputTokens          = "llm_output_tokens"           // 输出 token
	MLLMCacheReadTokens       = "llm_cache_read_tokens"       // 缓存命中 token
	MLLMCacheCreateTokens     = "llm_cache_create_tokens"     // 缓存创建 token
	MLLMTotalTokens           = "llm_total_tokens"            // 总 token
	MLLMRateLimitCount        = "llm_rate_limit_count"        // 429 次数
	MLLMOverloadCount         = "llm_overload_count"          // 过载次数
	MLLMTimeoutCount          = "llm_timeout_count"           // 超时次数
	MLLMRefusalCount          = "llm_refusal_count"           // 拒答次数
	MLLMPromptTooLong         = "llm_prompt_too_long"         // prompt 超长次数
	MLLMPromptComponentChars  = "llm_prompt_component_chars"  // prompt 组件字符数
	MLLMPromptComponentTokens = "llm_prompt_component_tokens" // prompt 组件估算 token 数
)

// LLM 限流/熔断 指标 (与 RateLimitGuard / Client 熔断器状态绑定, 周期性采样)
const (
	// RateLimitGuard (每次 LLM 调用或周期采样记录)
	MLLMGuardInFlight    = "llm_guard_in_flight"    // 当前在途请求数 (gauge)
	MLLMGuardMaxParallel = "llm_guard_max_parallel" // 当前允许最大并发 (gauge, AIMD)
	MLLMGuardRPMTokens   = "llm_guard_rpm_tokens"   // 令牌桶剩余 (gauge)
	MLLMGuardPauseSec    = "llm_guard_pause_sec"    // 全局退避剩余秒数 (gauge)
	MLLMGuardWaitSec     = "llm_guard_wait_sec"     // 单次准入等待时间 (histogram)
	MLLMGuardAIMDCut     = "llm_guard_aimd_cut"     // AIMD 降并发事件 (counter)

	// 熔断器 (连续失败触发)
	MLLMCircuitTrips      = "llm_circuit_trips"       // 熔断触发事件 (counter)
	MLLMCircuitOpenGauge  = "llm_circuit_open"        // 当前是否熔断 (gauge 0/1)
	MLLMCircuitFailStreak = "llm_circuit_fail_streak" // 连续失败计数 (gauge)
)

// ─── Prometheus 指标类型分类 ──────────────────────────────────────────────

const KindPromHistogram = "histogram"

// MetricType 定义每个指标的 Prometheus 类型 (仅列出常量中定义的指标)。
var MetricType = map[string]MetricKind{
	// LLM 计数器 (单调递增)
	MLLMCallCount:      "counter",
	MLLMSuccessCount:   "counter",
	MLLMErrorCount:     "counter",
	MLLMRetryCount:     "counter",
	MLLMRateLimitCount: "counter",
	MLLMOverloadCount:  "counter",
	MLLMTimeoutCount:   "counter",
	MLLMRefusalCount:   "counter",
	MLLMPromptTooLong:  "counter",
	MLLMGuardAIMDCut:   "counter",
	MLLMCircuitTrips:   "counter",

	// LLM 状态 (gauge)
	MLLMGuardInFlight:     "gauge",
	MLLMGuardMaxParallel:  "gauge",
	MLLMGuardRPMTokens:    "gauge",
	MLLMGuardPauseSec:     "gauge",
	MLLMGuardWaitSec:      "gauge",
	MLLMCircuitOpenGauge:  "gauge",
	MLLMCircuitFailStreak: "gauge",

	// LLM 分布 (histogram)
	MLLMDurationSec:           "histogram",
	MLLMInputTokens:           "histogram",
	MLLMOutputTokens:          "histogram",
	MLLMCacheReadTokens:       "histogram",
	MLLMCacheCreateTokens:     "histogram",
	MLLMTotalTokens:           "histogram",
	MLLMPromptComponentChars:  "histogram",
	MLLMPromptComponentTokens: "histogram",

	// Team 计数器
	MTeamRunCount:          "counter",
	MTeamSuccessCount:      "counter",
	MTeamFailCount:         "counter",
	MTeamStageCount:        "counter",
	MTeamStageSuccessCount: "counter",
	MTeamStageFailCount:    "counter",
	MTeamStageRetryCount:   "counter",
	MTeamRoundCount:        "counter",
	MTeamAgentRunCount:     "counter",

	// Team 状态/比率 (gauge)
	MTeamStagePassRate: "gauge",
	MTeamEvalPassRate:  "gauge",
	MTeamBuildPassRate: "gauge",
	MTeamTestPassRate:  "gauge",
	MTeamOutputAvgLen:  "gauge",
	MTeamFileCount:     "gauge",

	// Team 分布 (histogram)
	MTeamDurationSec:      "histogram",
	MTeamStageDurationSec: "histogram",
	MTeamStageOutputLen:   "histogram",
	MTeamAgentDurationSec: "histogram",

	// Dreaming 计数器
	MDreamCount:         "counter",
	MDreamErrorCount:    "counter",
	MDreamTriggerSource: "counter",

	// Dreaming 状态 (gauge)
	MDreamCompressionRatio:      "gauge",
	MDreamOutputSize:            "gauge",
	MDreamSessionsPending:       "gauge",
	MDreamHoursSinceLast:        "gauge",
	MDreamGateBlockSessionsLow:  "gauge",
	MDreamGateBlockTimeShort:    "gauge",
	MDreamGateBlockDreaming:     "gauge",
	MDreamGateBlockLockHeld:     "gauge",
	MDreamGateBlockScanThrottle: "gauge",
	MDreamConsolidatorFacts:     "gauge",
	MDreamConsolidatorContra:    "gauge",
	MDreamConsolidatorPatterns:  "gauge",

	// Dreaming 分布 (histogram)
	MDreamDurationSec:   "histogram",
	MDreamSessionsInput: "histogram",

	// Memory 计数器
	MMemPruneCount:          "counter",
	MMemRetrievalCount:      "counter",
	MFactIngestCount:        "counter",
	MFactDecayArchivedCount: "counter",
	MPrecompactFactsSaved:   "counter",

	// Memory 状态 (gauge)
	MMemEntryCount:       "gauge",
	MMemAvgAccessCount:   "gauge",
	MFactTotalCount:      "gauge",
	MFactActiveCount:     "gauge",
	MFactArchivedCount:   "gauge",
	MFactEvergreenCount:  "gauge",
	MFactAvgRetention:    "gauge",
	MFactConnectionCount: "gauge",
	MAmnesiaRiskScore:    "gauge",

	// Evolution 计数器
	MEvoDistillCount: "counter",
	MEvoPruneCount:   "counter",

	// Evolution 状态 (gauge)
	MEvoExperienceCount: "gauge",
	MEvoTrajectoryCount: "gauge",
	MEvoSuccessRate:     "gauge",
	MEvoUtilizationRate: "gauge",
	MEvoAvgQuality:      "gauge",
	MEvoQualityMin:      "gauge",
	MEvoQualityMax:      "gauge",
	MEvoFailTrajectory:  "gauge",

	// Cron 计数器
	MCronRunCount:     "counter",
	MCronSuccessCount: "counter",
	MCronFailCount:    "counter",

	// Cron 分布 (histogram)
	MCronDurationSec: "histogram",

	// Task 计数器
	MTaskCreatedCount:   "counter",
	MTaskCompletedCount: "counter",
	MTaskFailedCount:    "counter",

	// Task 状态 (gauge)
	MTaskCompletionRate: "gauge",
}
