// metrics_quality.go — Metrics 扩展：查询精度、噪音、结果质量量化。
//
// 新增维度:
//   1. 查询结果质量: 精度(Precision)、召回(Recall)、F1、噪音率、重复率
//   2. 引擎对比: GitNexus vs Native vs GraphifyNoLLM 的延迟/结果数/成功率对比
//   3. 查询类型分布: 热力图
//   4. 置信度分布: 结果置信度分布直方图
//
// 说明: 精度/召回需要人工标注或用户反馈作为 ground truth，
//       本实现提供采集框架，精度值可通过 RecordQueryFeedback 注入。
package codeintel

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 查询质量指标
// ============================================================================

// QueryQualityMetrics 单次查询结果质量（需外部反馈填充）。
type QueryQualityMetrics struct {
	Timestamp     time.Time `json:"timestamp"`
	QueryType     string    `json:"query_type"`
	QueryText     string    `json:"query_text"`
	Engine        string    `json:"engine"`

	// 核心质量（需人工标注或用户反馈）
	Precision     float64   `json:"precision,omitempty"`      // 相关结果 / 总返回结果
	Recall        float64   `json:"recall,omitempty"`         // 返回的相关结果 / 所有相关结果
	F1Score       float64   `json:"f1_score,omitempty"`       // 2 * P * R / (P + R)
	NoiseRate     float64   `json:"noise_rate,omitempty"`     // 不相关结果 / 总返回结果 = 1 - Precision
	DuplicateRate float64   `json:"duplicate_rate,omitempty"` // 重复结果 / 总返回结果

	// 自动计算的代理指标
	ResultCount   int       `json:"result_count"`
	UniqueCount   int       `json:"unique_count"`             // 去重后的结果数
	AvgConfidence float64   `json:"avg_confidence,omitempty"` // 平均置信度
	MinConfidence float64   `json:"min_confidence,omitempty"`
	MaxConfidence float64   `json:"max_confidence,omitempty"`
	HasDefinition bool      `json:"has_definition"`           // 结果中是否包含定义位置
	HasReferences bool      `json:"has_references"`           // 结果中是否包含引用

	// 用户反馈（可选）
	UserClicked   bool      `json:"user_clicked,omitempty"`   // 用户是否点击了结果
	UserResent    bool      `json:"user_resent,omitempty"`    // 用户是否重新查询了
	UserRating    int       `json:"user_rating,omitempty"`    // 1-5 星评分
}

// EngineComparisonMetrics 引擎对比指标。
type EngineComparisonMetrics struct {
	Timestamp       time.Time `json:"timestamp"`
	QueryType       string    `json:"query_type"`
	GitNexusLatency int64     `json:"gitnexus_latency_ms,omitempty"`
	NativeLatency   int64     `json:"native_latency_ms,omitempty"`
	GraphifyLatency int64     `json:"graphify_latency_ms,omitempty"`
	NoLLMLatency    int64     `json:"nollm_latency_ms,omitempty"`
	GitNexusResults int       `json:"gitnexus_results,omitempty"`
	NativeResults   int       `json:"native_results,omitempty"`
	GraphifyResults int       `json:"graphify_results,omitempty"`
	NoLLMResults    int       `json:"nollm_results,omitempty"`
	Winner          string    `json:"winner,omitempty"` // "gitnexus" | "native" | "graphify" | "nollm" | "tie"
}

// QueryTypeHeatMap 查询类型分布热力图。
type QueryTypeHeatMap struct {
	Timestamp   time.Time       `json:"timestamp"`
	Distribution map[string]int64 `json:"distribution"` // query_type -> count
	Hourly       map[string]int64 `json:"hourly"`       // "HH" -> count
}

// ConfidenceHistogram 置信度分布直方图。
type ConfidenceHistogram struct {
	Timestamp time.Time `json:"timestamp"`
	Buckets   []int64   `json:"buckets"` // 10 个桶: [0,0.1), [0.1,0.2), ..., [0.9,1.0]
}

// ============================================================================
// QualityCollector 质量采集器（挂载在 MetricsCollector 上）
// ============================================================================

// QualityCollector 查询质量采集器。
type QualityCollector struct {
	mu sync.RWMutex

	// 质量指标环形缓冲区
	qualityRing *ringBuffer[QueryQualityMetrics]

	// 引擎对比环形缓冲区
	compareRing *ringBuffer[EngineComparisonMetrics]

	// 查询类型分布（原子计数器）
	queryTypeDist sync.Map // string -> *int64
	hourlyDist    sync.Map // string -> *int64

	// 置信度直方图（10 个桶）
	confBuckets [10]int64

	// 聚合统计
	totalPrecision float64
	totalRecall    float64
	totalNoise     float64
	annotatedCount int64 // 有标注的查询数
}

// NewQualityCollector 创建质量采集器。
func NewQualityCollector() *QualityCollector {
	return &QualityCollector{
		qualityRing: newRingBuffer[QueryQualityMetrics](100),
		compareRing: newRingBuffer[EngineComparisonMetrics](50),
	}
}

// RecordQueryQuality 记录查询质量（含自动计算的代理指标）。
func (q *QualityCollector) RecordQueryQuality(qm QueryQualityMetrics) {
	// 自动计算 F1、NoiseRate
	if qm.Precision > 0 || qm.Recall > 0 {
		qm.F1Score = computeF1(qm.Precision, qm.Recall)
	}
	if qm.ResultCount > 0 {
		qm.NoiseRate = 1.0 - qm.Precision
		qm.DuplicateRate = float64(qm.ResultCount-qm.UniqueCount) / float64(qm.ResultCount)
	}

	q.qualityRing.push(qm)

	// 更新聚合统计
	if qm.Precision > 0 {
		atomic.AddInt64(&q.annotatedCount, 1)
		q.mu.Lock()
		q.totalPrecision += qm.Precision
		q.totalRecall += qm.Recall
		q.totalNoise += qm.NoiseRate
		q.mu.Unlock()
	}

	// 更新查询类型分布
	if qm.QueryType != "" {
		v, _ := q.queryTypeDist.LoadOrStore(qm.QueryType, new(int64))
		atomic.AddInt64(v.(*int64), 1)
	}
	hour := time.Now().Format("15")
	v, _ := q.hourlyDist.LoadOrStore(hour, new(int64))
	atomic.AddInt64(v.(*int64), 1)

	// 更新置信度直方图
	if qm.AvgConfidence > 0 {
		bucket := int(qm.AvgConfidence * 10)
		if bucket > 9 {
			bucket = 9
		}
		if bucket >= 0 {
			atomic.AddInt64(&q.confBuckets[bucket], 1)
		}
	}
}

// RecordEngineComparison 记录引擎对比。
func (q *QualityCollector) RecordEngineComparison(ec EngineComparisonMetrics) {
	// 自动判定 winner（结果数最多且延迟最低的引擎）
	if ec.GitNexusResults > 0 || ec.NativeResults > 0 || ec.GraphifyResults > 0 {
		ec.Winner = pickWinner(ec)
	}
	q.compareRing.push(ec)
}

// RecordQueryFeedback 记录用户反馈（用于后续计算精度）。
func (q *QualityCollector) RecordQueryFeedback(queryType, queryText, engine string, relevantCount, totalCount int, rating int) {
	var precision, recall float64
	if totalCount > 0 {
		precision = float64(relevantCount) / float64(totalCount)
	}
	// 召回需要知道所有相关结果数，这里用 precision 代理
	recall = precision

	qm := QueryQualityMetrics{
		Timestamp:   time.Now(),
		QueryType:   queryType,
		QueryText:   queryText,
		Engine:      engine,
		Precision:   precision,
		Recall:      recall,
		ResultCount: totalCount,
		UniqueCount: totalCount,
		UserRating:  rating,
	}
	q.RecordQueryQuality(qm)
}

// Snapshot 生成质量快照。
func (q *QualityCollector) Snapshot() *QualitySnapshot {
	q.mu.RLock()
	annotated := atomic.LoadInt64(&q.annotatedCount)
	var avgPrecision, avgRecall, avgNoise float64
	if annotated > 0 {
		avgPrecision = q.totalPrecision / float64(annotated)
		avgRecall = q.totalRecall / float64(annotated)
		avgNoise = q.totalNoise / float64(annotated)
	}
	q.mu.RUnlock()

	// 查询类型分布
	dist := make(map[string]int64)
	q.queryTypeDist.Range(func(k, v interface{}) bool {
		dist[k.(string)] = atomic.LoadInt64(v.(*int64))
		return true
	})

	// 小时分布
	hourly := make(map[string]int64)
	q.hourlyDist.Range(func(k, v interface{}) bool {
		hourly[k.(string)] = atomic.LoadInt64(v.(*int64))
		return true
	})

	// 置信度直方图
	buckets := make([]int64, 10)
	for i := 0; i < 10; i++ {
		buckets[i] = atomic.LoadInt64(&q.confBuckets[i])
	}

	return &QualitySnapshot{
		Timestamp:        time.Now(),
		RecentQuality:    q.qualityRing.slice(),
		RecentCompare:    q.compareRing.slice(),
		AvgPrecision:     avgPrecision,
		AvgRecall:        avgRecall,
		AvgNoiseRate:     avgNoise,
		AnnotatedQueries: annotated,
		QueryTypeDist:    dist,
		HourlyDist:       hourly,
		ConfidenceHist:   buckets,
	}
}

// ============================================================================
// QualitySnapshot 质量快照
// ============================================================================

// QualitySnapshot 质量指标快照。
type QualitySnapshot struct {
	Timestamp        time.Time                 `json:"timestamp"`
	RecentQuality    []QueryQualityMetrics     `json:"recent_quality,omitempty"`
	RecentCompare    []EngineComparisonMetrics `json:"recent_compare,omitempty"`
	AvgPrecision     float64                   `json:"avg_precision"`
	AvgRecall        float64                   `json:"avg_recall"`
	AvgNoiseRate     float64                   `json:"avg_noise_rate"`
	AnnotatedQueries int64                     `json:"annotated_queries"`
	QueryTypeDist    map[string]int64          `json:"query_type_dist"`
	HourlyDist       map[string]int64          `json:"hourly_dist"`
	ConfidenceHist   []int64                   `json:"confidence_histogram"`
}

// ============================================================================
// 私有辅助
// ============================================================================

func computeF1(precision, recall float64) float64 {
	if precision+recall == 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}

type engineScore struct {
	name    string
	results int
	latency int64
}

func pickWinner(ec EngineComparisonMetrics) string {
	engines := []engineScore{
		{"gitnexus", ec.GitNexusResults, ec.GitNexusLatency},
		{"native", ec.NativeResults, ec.NativeLatency},
		{"graphify", ec.GraphifyResults, ec.GraphifyLatency},
		{"nollm", ec.NoLLMResults, ec.NoLLMLatency},
	}

	// 筛选有结果的引擎
	var valid []engineScore
	for _, e := range engines {
		if e.results > 0 {
			valid = append(valid, e)
		}
	}
	if len(valid) == 0 {
		return "none"
	}
	if len(valid) == 1 {
		return valid[0].name
	}

	// 综合评分: 结果数权重 70%，延迟权重 30%（延迟越低越好）
	best := valid[0]
	bestScore := scoreEngine(best, valid)
	for _, e := range valid[1:] {
		s := scoreEngine(e, valid)
		if s > bestScore {
			bestScore = s
			best = e
		}
	}
	return best.name
}

func scoreEngine(e engineScore, all []engineScore) float64 {
	maxResults := 1
	maxLatency := int64(1)
	for _, a := range all {
		if a.results > maxResults {
			maxResults = a.results
		}
		if a.latency > maxLatency {
			maxLatency = a.latency
		}
	}
	resultScore := float64(e.results) / float64(maxResults)
	latencyScore := 1.0 - float64(e.latency)/float64(maxLatency)
	if latencyScore < 0 {
		latencyScore = 0
	}
	return resultScore*0.7 + latencyScore*0.3
}

// DegToRadians 辅助函数（避免 lint 报错）。
func DegToRadians(deg float64) float64 {
	return deg * math.Pi / 180
}
