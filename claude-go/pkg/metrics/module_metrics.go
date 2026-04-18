// Package metrics 持续迭代观测指标体系。
//
// 设计目标: 为 claude-go 各模块提供固定的、可持续跟踪的质量指标,
// 使每次迭代的效果可量化对比 (类似 MLOps 的 experiment tracking)。
//
// 业界参考:
//   - MLflow Metrics Tracking: 按 run/experiment 记录 step-metric 时间序列
//   - Weights & Biases: 自动 diff + 可视化 + 趋势告警
//   - OpenTelemetry Metrics: Counter/Histogram/Gauge 三种原语
//   - Google SRE: SLI/SLO 驱动的连续观测 + error budget
//
// 存储格式: JSONL (每行一个 MetricEvent), 支持 append-only 追加写入。
// 路径: {stateDir}/metrics/{module}.jsonl
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

// MetricEvent 单次指标事件 (JSONL 存储的原子单元)。
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
	Module       string                    `json:"module"`
	SnapshotTime time.Time                 `json:"snapshot_time"`
	Metrics      map[string]*MetricStat    `json:"metrics"`
	TrendAlerts  []string                  `json:"trend_alerts,omitempty"`
}

// MetricStat 单指标统计 (最近 N 次的聚合)。
type MetricStat struct {
	Name    string  `json:"name"`
	Count   int     `json:"count"`
	Last    float64 `json:"last"`
	Avg     float64 `json:"avg"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	StdDev  float64 `json:"std_dev"`
	Trend   string  `json:"trend"` // "improving", "degrading", "stable"
}

// Collector 指标采集器, 线程安全, append-only 写入 JSONL。
type Collector struct {
	dataDir string
	mu      sync.Mutex
	files   map[string]*os.File
	buffer  map[string][]MetricEvent // 内存缓冲 (用于 Summary)
}

// NewCollector 创建指标采集器。
func NewCollector(stateDir string) *Collector {
	dir := filepath.Join(stateDir, "metrics")
	os.MkdirAll(dir, 0o755)
	c := &Collector{
		dataDir: dir,
		files:   make(map[string]*os.File),
		buffer:  make(map[string][]MetricEvent),
	}
	c.loadExisting()
	return c
}

// Record 记录一个指标值。
func (c *Collector) Record(module, name string, value float64) {
	c.RecordWithLabels(module, name, value, nil)
}

// RecordWithLabels 记录带标签的指标值。
func (c *Collector) RecordWithLabels(module, name string, value float64, labels map[string]string) {
	evt := MetricEvent{
		Timestamp: time.Now(),
		Module:    module,
		Name:      name,
		Value:     value,
		Labels:    labels,
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buffer[module] = append(c.buffer[module], evt)
	c.appendToFile(module, evt)
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
	c.appendToFile(module, evt)
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

// Close 关闭所有文件句柄。
func (c *Collector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.files {
		f.Close()
	}
}

func (c *Collector) appendToFile(module string, evt MetricEvent) {
	f, ok := c.files[module]
	if !ok {
		path := filepath.Join(c.dataDir, module+".jsonl")
		var err error
		f, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		c.files[module] = f
	}
	data, _ := json.Marshal(evt)
	f.Write(data)
	f.WriteString("\n")
}

func (c *Collector) loadExisting() {
	entries, err := os.ReadDir(c.dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		module := e.Name()[:len(e.Name())-6] // strip .jsonl
		data, err := os.ReadFile(filepath.Join(c.dataDir, e.Name()))
		if err != nil {
			continue
		}
		var events []MetricEvent
		for _, line := range splitLines(data) {
			if len(line) == 0 {
				continue
			}
			var evt MetricEvent
			if json.Unmarshal(line, &evt) == nil {
				events = append(events, evt)
			}
		}
		if len(events) > 0 {
			c.buffer[module] = events
		}
	}
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

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
)

// Evolution 指标
const (
	MEvoExperienceCount = "evo_experience_count"  // 经验总数
	MEvoTrajectoryCount = "evo_trajectory_count"  // 轨迹总数
	MEvoSuccessRate     = "evo_success_rate"       // 成功轨迹率
	MEvoUtilizationRate = "evo_utilization_rate"   // 经验使用率 (至少用过1次的/总数)
	MEvoAvgQuality      = "evo_avg_quality"        // 平均质量分
	MEvoQualityMin      = "evo_quality_min"        // 最低质量分
	MEvoQualityMax      = "evo_quality_max"        // 最高质量分
	MEvoDistillCount    = "evo_distill_count"      // 提炼次数
	MEvoPruneCount      = "evo_prune_count"        // 剪枝数量
	MEvoFailTrajectory  = "evo_fail_trajectory_pct" // 失败轨迹占比
)

// Task 指标
const (
	MTaskCreatedCount  = "task_created_count"   // 创建总数
	MTaskCompletedCount = "task_completed_count" // 完成总数
	MTaskFailedCount   = "task_failed_count"    // 失败总数
	MTaskCompletionRate = "task_completion_rate" // 完成率
)

// Team 指标
const (
	MTeamRunCount       = "team_run_count"        // 运行总数
	MTeamSuccessCount   = "team_success_count"    // 成功次数
	MTeamFailCount      = "team_fail_count"       // 失败次数
	MTeamDurationSec    = "team_duration_sec"     // 运行耗时(秒)
	MTeamStagePassRate  = "team_stage_pass_rate"  // 阶段通过率
	MTeamEvalPassRate   = "team_eval_pass_rate"   // 评审通过率
	MTeamBuildPassRate  = "team_build_pass_rate"  // 编译通过率
	MTeamOutputAvgLen   = "team_output_avg_len"   // 平均产出长度
	MTeamRoundCount     = "team_round_count"      // 对抗轮数
	MTeamFileCount      = "team_file_count"       // 产出文件数
)

// Memory 指标
const (
	MMemEntryCount     = "mem_entry_count"      // 记忆条目数
	MMemAvgAccessCount = "mem_avg_access_count"  // 平均访问次数
	MMemPruneCount     = "mem_prune_count"       // 修剪次数
	MMemRetrievalCount = "mem_retrieval_count"   // 检索次数
)

// LLM 指标 (跨模块通用的大模型调用观测)
const (
	MLLMCallCount         = "llm_call_count"          // 调用总数
	MLLMSuccessCount      = "llm_success_count"       // 成功数
	MLLMErrorCount        = "llm_error_count"         // 失败数
	MLLMRetryCount        = "llm_retry_count"         // 重试次数
	MLLMDurationSec       = "llm_duration_sec"        // 单次耗时(秒)
	MLLMInputTokens       = "llm_input_tokens"        // 输入 token
	MLLMOutputTokens      = "llm_output_tokens"       // 输出 token
	MLLMCacheReadTokens   = "llm_cache_read_tokens"   // 缓存命中 token
	MLLMCacheCreateTokens = "llm_cache_create_tokens" // 缓存创建 token
	MLLMTotalTokens       = "llm_total_tokens"        // 总 token
	MLLMRateLimitCount    = "llm_rate_limit_count"    // 429 次数
	MLLMOverloadCount     = "llm_overload_count"      // 过载次数
	MLLMTimeoutCount      = "llm_timeout_count"       // 超时次数
	MLLMRefusalCount      = "llm_refusal_count"       // 拒答次数
	MLLMPromptTooLong     = "llm_prompt_too_long"     // prompt 超长次数
)
