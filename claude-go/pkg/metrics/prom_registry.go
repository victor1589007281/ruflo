// Package metrics — Prometheus 原生指标注册表。
//
// 使用 prometheus/client_golang 定义所有指标, 替代手动拼装文本格式的 /metrics 端点。
// Counter 单调递增, Gauge 瞬时值, Histogram 分布桶 — Prometheus 原生支持 rate()/histogram_quantile()。
package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	promOnce sync.Once
	promReg  *prometheus.Registry

	// LLM Counters
	llmCallCount      *prometheus.HistogramVec
	llmSuccessCount   *prometheus.CounterVec
	llmErrorCount     *prometheus.CounterVec
	llmRetryCount     *prometheus.HistogramVec
	llmRateLimitCount *prometheus.CounterVec
	llmOverloadCount  *prometheus.CounterVec
	llmTimeoutCount   *prometheus.CounterVec
	llmRefusalCount   *prometheus.CounterVec
	llmPromptTooLong  *prometheus.CounterVec

	// LLM Histograms
	llmDurationSec       *prometheus.HistogramVec
	llmInputTokens       *prometheus.HistogramVec
	llmOutputTokens      *prometheus.HistogramVec
	llmCacheReadTokens   *prometheus.HistogramVec
	llmCacheCreateTokens *prometheus.HistogramVec
	llmTotalTokens       *prometheus.HistogramVec

	// LLM Guard / Circuit
	llmGuardInFlight     *prometheus.GaugeVec
	llmGuardMaxParallel  *prometheus.GaugeVec
	llmGuardRPMTokens    *prometheus.GaugeVec
	llmGuardPauseSec     *prometheus.GaugeVec
	llmGuardWaitSec      *prometheus.HistogramVec
	llmCircuitTrips      *prometheus.CounterVec
	llmCircuitOpenGauge  *prometheus.GaugeVec
	llmCircuitFailStreak *prometheus.GaugeVec

	// Team
	teamRunCount          *prometheus.CounterVec
	teamSuccessCount      *prometheus.CounterVec
	teamFailCount         *prometheus.CounterVec
	teamStageCount        *prometheus.CounterVec
	teamStageSuccessCount *prometheus.CounterVec
	teamStageFailCount    *prometheus.CounterVec
	teamStageRetryCount   *prometheus.HistogramVec
	teamRoundCount        *prometheus.GaugeVec
	teamAgentRunCount     *prometheus.CounterVec

	teamDurationSec      *prometheus.HistogramVec
	teamStageDurationSec *prometheus.HistogramVec
	teamAgentDurationSec *prometheus.HistogramVec

	teamStagePassRate  *prometheus.GaugeVec
	teamEvalPassRate   *prometheus.GaugeVec
	teamBuildPassRate  *prometheus.GaugeVec
	teamOutputAvgLen   *prometheus.GaugeVec
	teamFileCount      *prometheus.GaugeVec
	teamStageOutputLen *prometheus.GaugeVec

	// Dreaming
	dreamCount        *prometheus.CounterVec
	dreamErrorCount   *prometheus.CounterVec
	dreamTriggerSource *prometheus.CounterVec
	dreamDurationSec  *prometheus.HistogramVec
	dreamSessionsInput *prometheus.HistogramVec

	dreamCompressionRatio       *prometheus.GaugeVec
	dreamOutputSize             *prometheus.GaugeVec
	dreamSessionsPending        *prometheus.GaugeVec
	dreamHoursSinceLast         *prometheus.GaugeVec
	dreamGateBlockSessionsLow   *prometheus.CounterVec
	dreamGateBlockTimeShort     *prometheus.CounterVec
	dreamGateBlockDreaming      *prometheus.CounterVec
	dreamGateBlockLockHeld      *prometheus.CounterVec
	dreamGateBlockScanThrottle  *prometheus.CounterVec
	dreamConsolidatorFacts      *prometheus.GaugeVec
	dreamConsolidatorContra     *prometheus.GaugeVec
	dreamConsolidatorPatterns   *prometheus.GaugeVec

	// Memory
	memEntryCount     *prometheus.GaugeVec
	memAvgAccessCount *prometheus.GaugeVec
	memPruneCount     *prometheus.CounterVec
	memRetrievalCount *prometheus.CounterVec

	factTotalCount         *prometheus.GaugeVec
	factActiveCount        *prometheus.GaugeVec
	factArchivedCount      *prometheus.GaugeVec
	factEvergreenCount     *prometheus.GaugeVec
	factAvgRetention       *prometheus.GaugeVec
	factIngestCount        *prometheus.CounterVec
	factDecayArchivedCount *prometheus.CounterVec
	factConnectionCount    *prometheus.GaugeVec
	amnesiaRiskScore       *prometheus.GaugeVec
	precompactFactsSaved   *prometheus.CounterVec

	// Evolution
	evoExperienceCount *prometheus.GaugeVec
	evoTrajectoryCount *prometheus.GaugeVec
	evoSuccessRate     *prometheus.GaugeVec
	evoUtilizationRate *prometheus.GaugeVec
	evoAvgQuality      *prometheus.GaugeVec
	evoQualityMin      *prometheus.GaugeVec
	evoQualityMax      *prometheus.GaugeVec
	evoDistillCount    *prometheus.CounterVec
	evoPruneCount      *prometheus.CounterVec
	evoFailTrajectory  *prometheus.GaugeVec

	// Task
	taskCreatedCount   *prometheus.CounterVec
	taskCompletedCount *prometheus.CounterVec
	taskFailedCount    *prometheus.CounterVec
	taskCompletionRate *prometheus.GaugeVec

	// Swarm Intel
	swarmRunCount       *prometheus.CounterVec
	swarmSuccessCount   *prometheus.CounterVec
	swarmConsensus      *prometheus.GaugeVec
	swarmBrierScore     *prometheus.GaugeVec
	swarmDiversity      *prometheus.GaugeVec
	swarmLatencyMs      *prometheus.HistogramVec
	swarmDebateSkipRate *prometheus.GaugeVec
	swarmLLMCalls       *prometheus.GaugeVec

	// Cron
	cronRunCount      *prometheus.CounterVec
	cronSuccessCount  *prometheus.CounterVec
	cronFailCount     *prometheus.CounterVec
	cronDurationSec   *prometheus.HistogramVec
	cronEnabledGauge  *prometheus.GaugeVec

	// Build info
	buildInfo prometheus.Gauge
)

func defaultBuckets() []float64 {
	return []float64{0.1, 0.5, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600}
}

func defaultLLMBuckets() []float64 {
	return []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 15, 30, 60, 120}
}

// initPrometheusMetrics 初始化所有 Prometheus 指标。线程安全 (sync.Once)。
func initPrometheusMetrics() {
	promReg = prometheus.NewRegistry()

	llmLabels := []string{"model", "status", "source", "purpose", "stop_reason", "error_kind", "http_status", "stream"}
	teamLabels := []string{"team", "stage", "role", "workflow"}
	dreamLabels := []string{"trigger_source", "consolidator_result"}
	memLabels := []string{"memory_type"}
	evoLabels := []string{"evolution_stage"}
	taskLabels := []string{"task_type"}

	// ── LLM ──
	llmCallCount = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_call", Help: "LLM 调用延迟分布 (秒)", Buckets: defaultLLMBuckets(),
	}, llmLabels)
	llmSuccessCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_success", Help: "LLM 成功调用次数",
	}, llmLabels)
	llmErrorCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_error", Help: "LLM 失败调用次数",
	}, llmLabels)
	llmRetryCount = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_retry", Help: "LLM 重试次数", Buckets: defaultBuckets(),
	}, llmLabels)
	llmRateLimitCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_rate_limit", Help: "限流 (429) 次数",
	}, llmLabels)
	llmOverloadCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_overload", Help: "过载 (503/529) 次数",
	}, llmLabels)
	llmTimeoutCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_timeout", Help: "超时次数",
	}, llmLabels)
	llmRefusalCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_refusal", Help: "拒答次数",
	}, llmLabels)
	llmPromptTooLong = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_prompt_too_long", Help: "Prompt 超长次数",
	}, llmLabels)

	llmDurationSec = llmCallCount
	llmInputTokens = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_input_tokens", Help: "输入 token 分布", Buckets: defaultLLMBuckets(),
	}, llmLabels)
	llmOutputTokens = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_output_tokens", Help: "输出 token 分布", Buckets: defaultLLMBuckets(),
	}, llmLabels)
	llmCacheReadTokens = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_cache_read_tokens", Help: "缓存读取 token", Buckets: defaultLLMBuckets(),
	}, llmLabels)
	llmCacheCreateTokens = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_cache_create_tokens", Help: "缓存创建 token", Buckets: defaultLLMBuckets(),
	}, llmLabels)
	llmTotalTokens = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_total_tokens", Help: "总 token 分布", Buckets: defaultLLMBuckets(),
	}, llmLabels)

	llmGuardInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "llm_guard_in_flight", Help: "在途请求数",
	}, []string{"module"})
	llmGuardMaxParallel = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "llm_guard_max_parallel", Help: "最大并发数 (AIMD)",
	}, []string{"module"})
	llmGuardRPMTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "llm_guard_rpm_tokens", Help: "RPM 令牌剩余",
	}, []string{"module"})
	llmGuardPauseSec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "llm_guard_pause_sec", Help: "退避剩余秒数",
	}, []string{"module"})
	llmGuardWaitSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "llm_guard_wait_sec", Help: "准入等待时间", Buckets: defaultBuckets(),
	}, llmLabels)
	llmCircuitTrips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "llm_circuit_trips", Help: "熔断触发次数",
	}, llmLabels)
	llmCircuitOpenGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "llm_circuit_open", Help: "熔断状态 (0/1)",
	}, []string{"module", "blocked"})
	llmCircuitFailStreak = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "llm_circuit_fail_streak", Help: "连续失败数",
	}, []string{"module"})

	// ── Team ──
	teamRunCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_run", Help: "团队运行次数",
	}, teamLabels)
	teamSuccessCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_success", Help: "团队成功次数",
	}, teamLabels)
	teamFailCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_fail", Help: "团队失败次数",
	}, teamLabels)
	teamStageCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_stage", Help: "阶段执行次数",
	}, teamLabels)
	teamStageSuccessCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_stage_success", Help: "阶段成功次数",
	}, teamLabels)
	teamStageFailCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_stage_fail", Help: "阶段失败次数",
	}, teamLabels)
	teamStageRetryCount = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "team_stage_retry", Help: "阶段重试次数", Buckets: defaultBuckets(),
	}, teamLabels)
	teamRoundCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_round", Help: "对抗轮数",
	}, teamLabels)
	teamAgentRunCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "team_agent_run", Help: "Agent 触发次数",
	}, teamLabels)

	teamDurationSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "team_duration", Help: "团队运行耗时 (秒)", Buckets: defaultBuckets(),
	}, teamLabels)
	teamStageDurationSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "team_stage_duration", Help: "阶段耗时 (秒)", Buckets: defaultBuckets(),
	}, teamLabels)
	teamAgentDurationSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "team_agent_duration", Help: "Agent 耗时 (秒)", Buckets: defaultBuckets(),
	}, teamLabels)

	teamStagePassRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_stage_pass_rate", Help: "阶段通过率",
	}, teamLabels)
	teamEvalPassRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_eval_pass_rate", Help: "评审通过率",
	}, teamLabels)
	teamBuildPassRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_build_pass_rate", Help: "编译通过率",
	}, teamLabels)
	teamOutputAvgLen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_output_avg_len", Help: "平均产出长度",
	}, teamLabels)
	teamFileCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_file_count", Help: "产出文件数",
	}, teamLabels)
	teamStageOutputLen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "team_stage_output_len", Help: "阶段产出长度",
	}, teamLabels)

	// ── Dreaming ──
	dreamCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_count", Help: "Dreaming 整理次数",
	}, dreamLabels)
	dreamErrorCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_error", Help: "Dreaming 失败次数",
	}, dreamLabels)
	dreamTriggerSource = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_trigger", Help: "触发来源",
	}, []string{"source"})
	dreamDurationSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "dream_duration", Help: "Dreaming 耗时 (秒)", Buckets: defaultBuckets(),
	}, dreamLabels)
	dreamSessionsInput = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "dream_sessions_input", Help: "输入会话数", Buckets: defaultBuckets(),
	}, dreamLabels)

	dreamCompressionRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_compression_ratio", Help: "压缩率",
	}, dreamLabels)
	dreamOutputSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_output_size", Help: "输出大小 (字符)",
	}, dreamLabels)
	dreamSessionsPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_sessions_pending", Help: "待处理会话数",
	}, dreamLabels)
	dreamHoursSinceLast = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_hours_since_last", Help: "距上次整理小时数",
	}, dreamLabels)
	dreamGateBlockSessionsLow = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_gate_sessions_low", Help: "门控: 会话不足",
	}, dreamLabels)
	dreamGateBlockTimeShort = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_gate_time_short", Help: "门控: 时间不足",
	}, dreamLabels)
	dreamGateBlockDreaming = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_gate_dreaming", Help: "门控: 已在执行",
	}, dreamLabels)
	dreamGateBlockLockHeld = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_gate_lock_held", Help: "门控: 锁被持有",
	}, dreamLabels)
	dreamGateBlockScanThrottle = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "dream_gate_scan_throttle", Help: "门控: 扫描节流",
	}, dreamLabels)
	dreamConsolidatorFacts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_consolidator_facts", Help: "Consolidator 事实产出",
	}, dreamLabels)
	dreamConsolidatorContra = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_consolidator_contradictions", Help: "矛盾检测数",
	}, dreamLabels)
	dreamConsolidatorPatterns = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "dream_consolidator_patterns", Help: "模式发现数",
	}, dreamLabels)

	// ── Memory ──
	memEntryCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "mem_entry_count", Help: "记忆条目数",
	}, memLabels)
	memAvgAccessCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "mem_avg_access_count", Help: "平均访问次数",
	}, memLabels)
	memPruneCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "mem_prune_count", Help: "记忆修剪次数",
	}, memLabels)
	memRetrievalCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "mem_retrieval_count", Help: "记忆检索次数",
	}, memLabels)

	factTotalCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "fact_total_count", Help: "事实总数 (L2)",
	}, memLabels)
	factActiveCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "fact_active_count", Help: "活跃事实数",
	}, memLabels)
	factArchivedCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "fact_archived_count", Help: "归档事实数",
	}, memLabels)
	factEvergreenCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "fact_evergreen_count", Help: "永久豁免事实",
	}, memLabels)
	factAvgRetention = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "fact_avg_retention", Help: "平均保留率",
	}, memLabels)
	factIngestCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "fact_ingest_count", Help: "新事实摄入数",
	}, memLabels)
	factDecayArchivedCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "fact_decay_archived_count", Help: "衰减归档数",
	}, memLabels)
	factConnectionCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "fact_connection_count", Help: "事实间关联数",
	}, memLabels)
	amnesiaRiskScore = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "amnesia_risk_score", Help: "失忆风险评分 (0-100)",
	}, memLabels)
	precompactFactsSaved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "precompact_facts_saved", Help: "PreCompact 抢救事实数",
	}, memLabels)

	// ── Evolution ──
	evoExperienceCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_experience_count", Help: "经验库条目数",
	}, evoLabels)
	evoTrajectoryCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_trajectory_count", Help: "轨迹总数",
	}, evoLabels)
	evoSuccessRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_success_rate", Help: "成功轨迹率",
	}, evoLabels)
	evoUtilizationRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_utilization_rate", Help: "经验使用率",
	}, evoLabels)
	evoAvgQuality = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_avg_quality", Help: "平均质量分",
	}, evoLabels)
	evoQualityMin = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_quality_min", Help: "最低质量分",
	}, evoLabels)
	evoQualityMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_quality_max", Help: "最高质量分",
	}, evoLabels)
	evoDistillCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "evo_distill_count", Help: "经验提炼次数",
	}, evoLabels)
	evoPruneCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "evo_prune_count", Help: "经验剪枝数",
	}, evoLabels)
	evoFailTrajectory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "evo_fail_trajectory_pct", Help: "失败轨迹占比",
	}, evoLabels)

	// ── Task ──
	taskCreatedCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "task_created", Help: "任务创建数",
	}, taskLabels)
	taskCompletedCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "task_completed", Help: "任务完成数",
	}, taskLabels)
	taskFailedCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "task_failed", Help: "任务失败数",
	}, taskLabels)
	taskCompletionRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "task_completion_rate", Help: "任务完成率",
	}, taskLabels)

	// ── Swarm Intel ──
	swarmLabels := []string{"run_type"} // predict / simulate
	swarmRunCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "swarm_runs", Help: "群体智能预测/模拟次数",
	}, swarmLabels)
	swarmSuccessCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "swarm_success", Help: "群体智能成功次数",
	}, swarmLabels)
	swarmConsensus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "swarm_consensus", Help: "共识度 (0-1)",
	}, swarmLabels)
	swarmBrierScore = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "swarm_brier", Help: "Brier 校准分数",
	}, swarmLabels)
	swarmDiversity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "swarm_diversity", Help: "多样性分数 (0-1)",
	}, swarmLabels)
	swarmLatencyMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "swarm_latency_ms", Help: "延迟分布 (ms)", Buckets: []float64{100, 500, 1000, 2000, 5000, 10000, 30000, 60000},
	}, swarmLabels)
	swarmDebateSkipRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "swarm_debate_skip_rate", Help: "辩论跳过率",
	}, swarmLabels)
	swarmLLMCalls = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "swarm_llm_calls", Help: "单次 LLM 调用数",
	}, swarmLabels)

	// ── Cron ──
	cronLabels := []string{"job_name", "job_type"}
	cronRunCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "cron_run_count", Help: "Cron 触发次数",
	}, cronLabels)
	cronSuccessCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "cron_success_count", Help: "Cron 成功次数",
	}, cronLabels)
	cronFailCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "claude_go", Name: "cron_fail_count", Help: "Cron 失败次数",
	}, cronLabels)
	cronDurationSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "claude_go", Name: "cron_duration_bucket", Help: "Cron 耗时分布 (秒)", Buckets: defaultBuckets(),
	}, cronLabels)
	cronEnabledGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "cron_enabled", Help: "启用的定时任务数",
	}, []string{})

	// ── Build Info ──
	buildInfo = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "claude_go", Name: "build_info", Help: "Build 信息",
	})

	// 注册所有指标到自定义 registry
	for _, c := range []prometheus.Collector{
		llmCallCount, llmSuccessCount, llmErrorCount, llmRetryCount,
		llmRateLimitCount, llmOverloadCount, llmTimeoutCount, llmRefusalCount, llmPromptTooLong,
		llmInputTokens, llmOutputTokens, llmCacheReadTokens, llmCacheCreateTokens, llmTotalTokens,
		llmGuardInFlight, llmGuardMaxParallel, llmGuardRPMTokens, llmGuardPauseSec, llmGuardWaitSec,
		llmCircuitTrips, llmCircuitOpenGauge, llmCircuitFailStreak,
		teamRunCount, teamSuccessCount, teamFailCount, teamStageCount,
		teamStageSuccessCount, teamStageFailCount, teamStageRetryCount, teamRoundCount, teamAgentRunCount,
		teamDurationSec, teamStageDurationSec, teamAgentDurationSec,
		teamStagePassRate, teamEvalPassRate, teamBuildPassRate, teamOutputAvgLen, teamFileCount, teamStageOutputLen,
		dreamCount, dreamErrorCount, dreamTriggerSource, dreamDurationSec, dreamSessionsInput,
		dreamCompressionRatio, dreamOutputSize, dreamSessionsPending, dreamHoursSinceLast,
		dreamGateBlockSessionsLow, dreamGateBlockTimeShort, dreamGateBlockDreaming,
		dreamGateBlockLockHeld, dreamGateBlockScanThrottle,
		dreamConsolidatorFacts, dreamConsolidatorContra, dreamConsolidatorPatterns,
		memEntryCount, memAvgAccessCount, memPruneCount, memRetrievalCount,
		factTotalCount, factActiveCount, factArchivedCount, factEvergreenCount,
		factAvgRetention, factIngestCount, factDecayArchivedCount, factConnectionCount,
		amnesiaRiskScore, precompactFactsSaved,
		evoExperienceCount, evoTrajectoryCount, evoSuccessRate, evoUtilizationRate,
		evoAvgQuality, evoQualityMin, evoQualityMax, evoDistillCount, evoPruneCount, evoFailTrajectory,
		taskCreatedCount, taskCompletedCount, taskFailedCount, taskCompletionRate,
		swarmRunCount, swarmSuccessCount, swarmConsensus, swarmBrierScore,
		swarmDiversity, swarmLatencyMs, swarmDebateSkipRate, swarmLLMCalls,
		cronRunCount, cronSuccessCount, cronFailCount, cronDurationSec, cronEnabledGauge,
		buildInfo,
	} {
		promReg.MustRegister(c)
	}
}

// PrometheusRegistry 返回原生 Prometheus Registry。
func PrometheusRegistry() *prometheus.Registry {
	promOnce.Do(initPrometheusMetrics)
	return promReg
}

// PrometheusHandler 返回标准 Prometheus HTTP handler。
func PrometheusHandler() http.Handler {
	promOnce.Do(initPrometheusMetrics)
	return promhttp.HandlerFor(promReg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// ─── recordPromMetric 将 (module, name, value, labels) 路由到对应 Prometheus 指标 ───
// 这是 Collector.RecordAtTime/RecordRun 的桥接层, 保持向后兼容的 name 常量。

func recordPromMetric(module, name string, value float64, labels map[string]string) {
	promOnce.Do(initPrometheusMetrics)

	// 填充缺失的 label 为空字符串, 避免 cardinality panic
	fillLabels := func(need []string, extra map[string]string) prometheus.Labels {
		pl := prometheus.Labels{}
		for _, k := range need {
			pl[k] = ""
			if extra != nil {
				if v, ok := extra[k]; ok {
					pl[k] = v
				}
			}
		}
		return pl
	}

	llmNeed := []string{"model", "status", "source", "purpose", "stop_reason", "error_kind", "http_status", "stream"}
	teamNeed := []string{"team", "stage", "role", "workflow"}
	dreamNeed := []string{"trigger_source", "consolidator_result"}
	memNeed := []string{"memory_type"}
	evoNeed := []string{"evolution_stage"}
	taskNeed := []string{"task_type"}

	switch name {
	// LLM counters
	case MLLMCallCount:
		llmCallCount.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMSuccessCount:
		llmSuccessCount.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMErrorCount:
		llmErrorCount.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMRetryCount:
		llmRetryCount.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMRateLimitCount:
		llmRateLimitCount.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMOverloadCount:
		llmOverloadCount.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMTimeoutCount:
		llmTimeoutCount.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMRefusalCount:
		llmRefusalCount.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMPromptTooLong:
		llmPromptTooLong.With(fillLabels(llmNeed, labels)).Add(value)
	case MLLMCircuitTrips:
		llmCircuitTrips.With(fillLabels(llmNeed, labels)).Add(value)

	// LLM histograms
	case MLLMDurationSec:
		llmDurationSec.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMInputTokens:
		llmInputTokens.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMOutputTokens:
		llmOutputTokens.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMCacheReadTokens:
		llmCacheReadTokens.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMCacheCreateTokens:
		llmCacheCreateTokens.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMTotalTokens:
		llmTotalTokens.With(fillLabels(llmNeed, labels)).Observe(value)
	case MLLMGuardWaitSec:
		llmGuardWaitSec.With(fillLabels(llmNeed, labels)).Observe(value)

	// LLM gauges
	case MLLMGuardInFlight:
		llmGuardInFlight.With(prometheus.Labels{"module": module}).Set(value)
	case MLLMGuardMaxParallel:
		llmGuardMaxParallel.With(prometheus.Labels{"module": module}).Set(value)
	case MLLMGuardRPMTokens:
		llmGuardRPMTokens.With(prometheus.Labels{"module": module}).Set(value)
	case MLLMGuardPauseSec:
		llmGuardPauseSec.With(prometheus.Labels{"module": module}).Set(value)
	case MLLMCircuitOpenGauge:
		llmCircuitOpenGauge.With(prometheus.Labels{"module": module}).Set(value)
	case MLLMCircuitFailStreak:
		llmCircuitFailStreak.With(prometheus.Labels{"module": module}).Set(value)

	// Team counters
	case MTeamRunCount:
		teamRunCount.With(fillLabels(teamNeed, labels)).Add(value)
	case MTeamSuccessCount:
		teamSuccessCount.With(fillLabels(teamNeed, labels)).Add(value)
	case MTeamFailCount:
		teamFailCount.With(fillLabels(teamNeed, labels)).Add(value)
	case MTeamStageCount:
		teamStageCount.With(fillLabels(teamNeed, labels)).Add(value)
	case MTeamStageSuccessCount:
		teamStageSuccessCount.With(fillLabels(teamNeed, labels)).Add(value)
	case MTeamStageFailCount:
		teamStageFailCount.With(fillLabels(teamNeed, labels)).Add(value)
	case MTeamStageRetryCount:
		teamStageRetryCount.With(fillLabels(teamNeed, labels)).Observe(value)
	case MTeamRoundCount:
		teamRoundCount.With(fillLabels(teamNeed, labels)).Set(value)
	case MTeamAgentRunCount:
		teamAgentRunCount.With(fillLabels(teamNeed, labels)).Add(value)

	// Team histograms
	case MTeamDurationSec:
		teamDurationSec.With(fillLabels(teamNeed, labels)).Observe(value)
	case MTeamStageDurationSec:
		teamStageDurationSec.With(fillLabels(teamNeed, labels)).Observe(value)
	case MTeamAgentDurationSec:
		teamAgentDurationSec.With(fillLabels(teamNeed, labels)).Observe(value)

	// Team gauges
	case MTeamStagePassRate:
		teamStagePassRate.With(fillLabels(teamNeed, labels)).Set(value)
	case MTeamEvalPassRate:
		teamEvalPassRate.With(fillLabels(teamNeed, labels)).Set(value)
	case MTeamBuildPassRate:
		teamBuildPassRate.With(fillLabels(teamNeed, labels)).Set(value)
	case MTeamOutputAvgLen:
		teamOutputAvgLen.With(fillLabels(teamNeed, labels)).Set(value)
	case MTeamFileCount:
		teamFileCount.With(fillLabels(teamNeed, labels)).Set(value)
	case MTeamStageOutputLen:
		teamStageOutputLen.With(fillLabels(teamNeed, labels)).Set(value)

	// Dreaming counters
	case MDreamCount:
		dreamCount.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamErrorCount:
		dreamErrorCount.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamTriggerSource:
		src := ""
		if labels != nil {
			src = labels["source"]
		}
		if src != "" {
			dreamTriggerSource.With(prometheus.Labels{"source": src}).Add(value)
		} else {
			dreamTriggerSource.With(prometheus.Labels{"source": ""}).Add(value)
		}

	// Dreaming histograms
	case MDreamDurationSec:
		dreamDurationSec.With(fillLabels(dreamNeed, labels)).Observe(value)
	case MDreamSessionsInput:
		dreamSessionsInput.With(fillLabels(dreamNeed, labels)).Observe(value)

	// Dreaming gauges
	case MDreamCompressionRatio:
		dreamCompressionRatio.With(fillLabels(dreamNeed, labels)).Set(value)
	case MDreamOutputSize:
		dreamOutputSize.With(fillLabels(dreamNeed, labels)).Set(value)
	case MDreamSessionsPending:
		dreamSessionsPending.With(fillLabels(dreamNeed, labels)).Set(value)
	case MDreamHoursSinceLast:
		dreamHoursSinceLast.With(fillLabels(dreamNeed, labels)).Set(value)
	case MDreamGateBlockSessionsLow:
		dreamGateBlockSessionsLow.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamGateBlockTimeShort:
		dreamGateBlockTimeShort.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamGateBlockDreaming:
		dreamGateBlockDreaming.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamGateBlockLockHeld:
		dreamGateBlockLockHeld.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamGateBlockScanThrottle:
		dreamGateBlockScanThrottle.With(fillLabels(dreamNeed, labels)).Add(value)
	case MDreamConsolidatorFacts:
		dreamConsolidatorFacts.With(fillLabels(dreamNeed, labels)).Set(value)
	case MDreamConsolidatorContra:
		dreamConsolidatorContra.With(fillLabels(dreamNeed, labels)).Set(value)
	case MDreamConsolidatorPatterns:
		dreamConsolidatorPatterns.With(fillLabels(dreamNeed, labels)).Set(value)

	// Memory gauges
	case MMemEntryCount:
		memEntryCount.With(fillLabels(memNeed, labels)).Set(value)
	case MMemAvgAccessCount:
		memAvgAccessCount.With(fillLabels(memNeed, labels)).Set(value)
	case MFactTotalCount:
		factTotalCount.With(fillLabels(memNeed, labels)).Set(value)
	case MFactActiveCount:
		factActiveCount.With(fillLabels(memNeed, labels)).Set(value)
	case MFactArchivedCount:
		factArchivedCount.With(fillLabels(memNeed, labels)).Set(value)
	case MFactEvergreenCount:
		factEvergreenCount.With(fillLabels(memNeed, labels)).Set(value)
	case MFactAvgRetention:
		factAvgRetention.With(fillLabels(memNeed, labels)).Set(value)
	case MFactConnectionCount:
		factConnectionCount.With(fillLabels(memNeed, labels)).Set(value)
	case MAmnesiaRiskScore:
		amnesiaRiskScore.With(fillLabels(memNeed, labels)).Set(value)

	// Memory counters
	case MMemPruneCount:
		memPruneCount.With(fillLabels(memNeed, labels)).Add(value)
	case MMemRetrievalCount:
		memRetrievalCount.With(fillLabels(memNeed, labels)).Add(value)
	case MFactIngestCount:
		factIngestCount.With(fillLabels(memNeed, labels)).Add(value)
	case MFactDecayArchivedCount:
		factDecayArchivedCount.With(fillLabels(memNeed, labels)).Add(value)
	case MPrecompactFactsSaved:
		precompactFactsSaved.With(fillLabels(memNeed, labels)).Add(value)

	// Evolution gauges
	case MEvoExperienceCount:
		evoExperienceCount.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoTrajectoryCount:
		evoTrajectoryCount.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoSuccessRate:
		evoSuccessRate.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoUtilizationRate:
		evoUtilizationRate.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoAvgQuality:
		evoAvgQuality.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoQualityMin:
		evoQualityMin.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoQualityMax:
		evoQualityMax.With(fillLabels(evoNeed, labels)).Set(value)
	case MEvoFailTrajectory:
		evoFailTrajectory.With(fillLabels(evoNeed, labels)).Set(value)

	// Evolution counters
	case MEvoDistillCount:
		evoDistillCount.With(fillLabels(evoNeed, labels)).Add(value)
	case MEvoPruneCount:
		evoPruneCount.With(fillLabels(evoNeed, labels)).Add(value)

	// Task counters
	case MTaskCreatedCount:
		taskCreatedCount.With(fillLabels(taskNeed, labels)).Add(value)
	case MTaskCompletedCount:
		taskCompletedCount.With(fillLabels(taskNeed, labels)).Add(value)
	case MTaskFailedCount:
		taskFailedCount.With(fillLabels(taskNeed, labels)).Add(value)

	// Task gauges
	case MTaskCompletionRate:
		taskCompletionRate.With(fillLabels(taskNeed, labels)).Set(value)

	// Swarm Intel counters
	case MSwarmRunCount:
		swarmRunCount.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Add(value)
	case MSwarmSuccessCount:
		swarmSuccessCount.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Add(value)

	// Swarm Intel gauges
	case MSwarmConsensus:
		swarmConsensus.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Set(value)
	case MSwarmBrierScore:
		swarmBrierScore.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Set(value)
	case MSwarmDiversity:
		swarmDiversity.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Set(value)
	case MSwarmLatencyMs:
		swarmLatencyMs.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Observe(value)
	case MSwarmDebateSkipRate:
		swarmDebateSkipRate.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Set(value)
	case MSwarmLLMCalls:
		swarmLLMCalls.With(prometheus.Labels{"run_type": labelsOrEmpty(labels, "run_type")}).Set(value)

	// Cron counters
	case MCronRunCount:
		cronRunCount.With(prometheus.Labels{"job_name": labelsOrEmpty(labels, "job_name"), "job_type": labelsOrEmpty(labels, "job_type")}).Add(value)
	case MCronSuccessCount:
		cronSuccessCount.With(prometheus.Labels{"job_name": labelsOrEmpty(labels, "job_name"), "job_type": labelsOrEmpty(labels, "job_type")}).Add(value)
	case MCronFailCount:
		cronFailCount.With(prometheus.Labels{"job_name": labelsOrEmpty(labels, "job_name"), "job_type": labelsOrEmpty(labels, "job_type")}).Add(value)
	case MCronDurationSec:
		cronDurationSec.With(prometheus.Labels{"job_name": labelsOrEmpty(labels, "job_name"), "job_type": labelsOrEmpty(labels, "job_type")}).Observe(value)
	}
}

// labelsOrEmpty 从 labels 取指定 key 的值, 不存在时返回空字符串。
func labelsOrEmpty(labels map[string]string, key string) string {
	if labels == nil {
		return ""
	}
	return labels[key]
}

// RecordPromMetric 将 (module, name, value, labels) 路由到对应 Prometheus 指标。
// 供外部调用 (如 dashboard 从 JSON 数据源导出 Swarm/Cron 指标)。
func RecordPromMetric(module, name string, value float64, labels map[string]string) {
	recordPromMetric(module, name, value, labels)
}

// RestorePromFromJSONL 从 JSONL 历史文件回放事件到 Prometheus,
// 使服务重启后 counter/histogram 保留历史累计值。
// 仅回放最近 N 条事件 (默认 10000), 避免启动过慢。
func RestorePromFromJSONL(stateDir string, maxEvents int) error {
	if maxEvents <= 0 {
		maxEvents = 10000
	}

	dir := filepath.Join(stateDir, "metrics")
	entries, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("扫描 %s 失败: %w", dir, err)
	}

	total := 0
	for _, path := range entries {
		n, err := replayJSONL(path, maxEvents)
		if err != nil {
			log.Printf("[metrics] 回放 %s 失败: %v", path, err)
			continue
		}
		log.Printf("[metrics] 已从 %s 回放 %d 条事件到 Prometheus", filepath.Base(path), n)
		total += n
	}

	if total > 0 {
		log.Printf("[metrics] 共回放 %d 条历史事件", total)
	}
	return nil
}

func replayJSONL(path string, maxEvents int) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	type jsonlEvent struct {
		Timestamp string            `json:"ts"`
		Module    string            `json:"module"`
		Name      string            `json:"name"`
		Value     float64           `json:"value"`
		Labels    map[string]string `json:"labels"`
	}

	// 读取最后 maxEvents 行, 避免回放太慢
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // 1MB buffer for long JSONL lines
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > maxEvents+100 {
			lines = lines[len(lines)-maxEvents:]
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("读取 %s: %w", path, err)
	}

	restored := 0
	skipped := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		var evt jsonlEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		// 回放可能因旧 JSONL 标签格式不匹配而 panic, 跳过即可。
		ok := func() bool {
			defer func() {
				if r := recover(); r != nil {
					skipped++
				}
			}()
			recordPromMetric(evt.Module, evt.Name, evt.Value, evt.Labels)
			return true
		}()
		if ok {
			restored++
		}
	}

	if skipped > 0 {
		log.Printf("[metrics] 回放 %s: 跳过 %d 条不兼容的事件", filepath.Base(path), skipped)
	}

	return restored, nil
}


