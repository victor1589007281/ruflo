// metrics.go — Code Intelligence 监控指标体系。
//
// 采集维度:
//   1. 构建指标: 耗时、成功率、产出规模
//   2. 查询指标: 延迟、类型分布、缓存命中率
//   3. 降级指标: GitNexus miss → Native / GraphifyNoLLM 兜底次数
//   4. 存储指标: 磁盘读取次数、字节数
//   5. 自动更新指标: 检测次数、触发次数、失败次数
//
// 输出: JSON 文件 + 内存快照，支持 Grafana / Claude Dashboard 消费。
package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// Metrics 数据模型
// ============================================================================

// BuildMetrics 构建阶段指标。
type BuildMetrics struct {
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	DurationMs    int64     `json:"duration_ms"`
	Success       bool      `json:"success"`
	Tool          string    `json:"tool"`          // "gitnexus" | "graphify"
	Phase         string    `json:"phase"`         // "analyze" | "update" | "cluster"
	NodesCount    int       `json:"nodes_count"`
	EdgesCount    int       `json:"edges_count"`
	SymbolsCount  int       `json:"symbols_count,omitempty"`
	ErrorMsg      string    `json:"error_msg,omitempty"`
}

// QueryMetrics 单次查询指标。
type QueryMetrics struct {
	Timestamp    time.Time `json:"timestamp"`
	QueryType    string    `json:"query_type"`
	Engine       string    `json:"engine"`       // "gitnexus" | "graphify" | "native" | "graphify_nollm"
	LatencyMs    int64     `json:"latency_ms"`
	Success      bool      `json:"success"`
	CacheHit     bool      `json:"cache_hit"`    // 仅对查询缓存
	Fallback     bool      `json:"fallback"`     // 是否降级到 Native/NoLLM
	ResultCount  int       `json:"result_count"`
	ErrorMsg     string    `json:"error_msg,omitempty"`
}

// StorageMetrics 存储层指标。
type StorageMetrics struct {
	Timestamp      time.Time `json:"timestamp"`
	DiskReads      int64     `json:"disk_reads"`
	DiskReadBytes  int64     `json:"disk_read_bytes"`
	NodeCacheHits  int64     `json:"node_cache_hits"`
	NodeCacheMiss  int64     `json:"node_cache_misses"`
	ListCacheHits  int64     `json:"list_cache_hits"`
	ListCacheMiss  int64     `json:"list_cache_misses"`
	InCacheHits    int64     `json:"in_cache_hits"`
	InCacheMiss    int64     `json:"in_cache_misses"`
	NodeCacheSize  int       `json:"node_cache_size"`
	ListCacheSize  int       `json:"list_cache_size"`
	InCacheSize    int       `json:"in_cache_size"`
}

// AutoUpdateMetrics 自动更新指标。
type AutoUpdateMetrics struct {
	Timestamp        time.Time `json:"timestamp"`
	CheckCount       int64     `json:"check_count"`
	TriggerCount     int64     `json:"trigger_count"`
	SuccessCount     int64     `json:"success_count"`
	FailCount        int64     `json:"fail_count"`
	ChangedFiles     int       `json:"changed_files"`
	RebuildDurationMs int64    `json:"rebuild_duration_ms"`
}

// MetricsSnapshot 全量指标快照。
type MetricsSnapshot struct {
	CollectedAt     time.Time           `json:"collected_at"`
	RepoPath        string              `json:"repo_path"`
	Builds          []BuildMetrics      `json:"builds,omitempty"`
	Queries         []QueryMetrics      `json:"queries,omitempty"`
	Storage         StorageMetrics      `json:"storage"`
	AutoUpdate      AutoUpdateMetrics   `json:"auto_update"`
	TotalQueries    int64               `json:"total_queries"`
	TotalBuilds     int64               `json:"total_builds"`
	AvgQueryLatency int64               `json:"avg_query_latency_ms"`
	CacheHitRate    float64             `json:"cache_hit_rate"`
	FallbackRate    float64             `json:"fallback_rate"`
}

// ============================================================================
// MetricsCollector 采集器
// ============================================================================

// MetricsCollector 线程安全的指标采集器。
type MetricsCollector struct {
	repoPath string
	mu       sync.RWMutex

	// 计数器（原子操作）
	totalQueries   int64
	totalBuilds    int64
	totalFallbacks int64

	// 环形缓冲区（保留最近 N 条）
	buildRing  *ringBuffer[BuildMetrics]
	queryRing  *ringBuffer[QueryMetrics]

	// 存储指标（原子计数器，由 DiskGraphIndex 通过回调更新）
	storage atomicStorage

	// 自动更新指标
	autoUpdate atomicAutoUpdate
}

// NewMetricsCollector 创建采集器。
func NewMetricsCollector(repoPath string) *MetricsCollector {
	return &MetricsCollector{
		repoPath:  repoPath,
		buildRing: newRingBuffer[BuildMetrics](50),
		queryRing: newRingBuffer[QueryMetrics](200),
	}
}

// RecordBuild 记录构建指标。
func (m *MetricsCollector) RecordBuild(b BuildMetrics) {
	atomic.AddInt64(&m.totalBuilds, 1)
	m.buildRing.push(b)
}

// RecordQuery 记录查询指标。
func (m *MetricsCollector) RecordQuery(q QueryMetrics) {
	atomic.AddInt64(&m.totalQueries, 1)
	if q.Fallback {
		atomic.AddInt64(&m.totalFallbacks, 1)
	}
	m.queryRing.push(q)
}

// RecordDiskRead 记录磁盘读取（由 DiskGraphIndex 调用）。
func (m *MetricsCollector) RecordDiskRead(bytes int64) {
	m.storage.addRead(bytes)
}

// RecordCacheHit 记录缓存命中（由 DiskGraphIndex 调用）。
func (m *MetricsCollector) RecordCacheHit(cacheType string) {
	m.storage.addHit(cacheType)
}

// RecordCacheMiss 记录缓存未命中（由 DiskGraphIndex 调用）。
func (m *MetricsCollector) RecordCacheMiss(cacheType string) {
	m.storage.addMiss(cacheType)
}

// RecordAutoUpdateCheck 记录自动更新检测。
func (m *MetricsCollector) RecordAutoUpdateCheck(changedFiles int) {
	m.autoUpdate.recordCheck(changedFiles)
}

// RecordAutoUpdateTrigger 记录自动更新触发。
func (m *MetricsCollector) RecordAutoUpdateTrigger(success bool, durationMs int64) {
	m.autoUpdate.recordTrigger(success, durationMs)
}

// Snapshot 生成全量快照。
func (m *MetricsCollector) Snapshot() *MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tq := atomic.LoadInt64(&m.totalQueries)
	tb := atomic.LoadInt64(&m.totalBuilds)
	tf := atomic.LoadInt64(&m.totalFallbacks)

	// 计算平均延迟（最近 50 条查询）
	var avgLat int64
	recentQueries := m.queryRing.slice()
	if len(recentQueries) > 0 {
		var sum int64
		for _, q := range recentQueries {
			sum += q.LatencyMs
		}
		avgLat = sum / int64(len(recentQueries))
	}

	// 缓存命中率
	st := m.storage.snapshot()
	var cacheHitRate float64
	totalCache := st.NodeCacheHits + st.NodeCacheMiss +
		st.ListCacheHits + st.ListCacheMiss +
		st.InCacheHits + st.InCacheMiss
	if totalCache > 0 {
		cacheHitRate = float64(st.NodeCacheHits+st.ListCacheHits+st.InCacheHits) / float64(totalCache) * 100
	}

	// 降级率
	var fallbackRate float64
	if tq > 0 {
		fallbackRate = float64(tf) / float64(tq) * 100
	}

	return &MetricsSnapshot{
		CollectedAt:     time.Now(),
		RepoPath:        m.repoPath,
		Builds:          m.buildRing.slice(),
		Queries:         recentQueries,
		Storage:         st,
		AutoUpdate:      m.autoUpdate.snapshot(),
		TotalQueries:    tq,
		TotalBuilds:     tb,
		AvgQueryLatency: avgLat,
		CacheHitRate:    cacheHitRate,
		FallbackRate:    fallbackRate,
	}
}

// SaveToDisk 将快照持久化到磁盘。
func (m *MetricsCollector) SaveToDisk() error {
	snap := m.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.repoPath, ".claude-code-intel", "metrics.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// String 返回人类可读的指标摘要。
func (m *MetricsCollector) String() string {
	snap := m.Snapshot()
	return fmt.Sprintf(
		"[codeintel metrics] queries=%d builds=%d fallback=%.1f%% avg_latency=%dms cache_hit=%.1f%% disk_reads=%d",
		snap.TotalQueries, snap.TotalBuilds, snap.FallbackRate,
		snap.AvgQueryLatency, snap.CacheHitRate, snap.Storage.DiskReads,
	)
}

// ============================================================================
// 原子存储计数器
// ============================================================================

type atomicStorage struct {
	diskReads     int64
	diskReadBytes int64
	nodeHits      int64
	nodeMisses    int64
	listHits      int64
	listMisses    int64
	inHits        int64
	inMisses      int64
}

func (s *atomicStorage) addRead(bytes int64) {
	atomic.AddInt64(&s.diskReads, 1)
	atomic.AddInt64(&s.diskReadBytes, bytes)
}

func (s *atomicStorage) addHit(cacheType string) {
	switch cacheType {
	case "node":
		atomic.AddInt64(&s.nodeHits, 1)
	case "list":
		atomic.AddInt64(&s.listHits, 1)
	case "in":
		atomic.AddInt64(&s.inHits, 1)
	}
}

func (s *atomicStorage) addMiss(cacheType string) {
	switch cacheType {
	case "node":
		atomic.AddInt64(&s.nodeMisses, 1)
	case "list":
		atomic.AddInt64(&s.listMisses, 1)
	case "in":
		atomic.AddInt64(&s.inMisses, 1)
	}
}

func (s *atomicStorage) snapshot() StorageMetrics {
	return StorageMetrics{
		Timestamp:     time.Now(),
		DiskReads:     atomic.LoadInt64(&s.diskReads),
		DiskReadBytes: atomic.LoadInt64(&s.diskReadBytes),
		NodeCacheHits: atomic.LoadInt64(&s.nodeHits),
		NodeCacheMiss: atomic.LoadInt64(&s.nodeMisses),
		ListCacheHits: atomic.LoadInt64(&s.listHits),
		ListCacheMiss: atomic.LoadInt64(&s.listMisses),
		InCacheHits:   atomic.LoadInt64(&s.inHits),
		InCacheMiss:   atomic.LoadInt64(&s.inMisses),
	}
}

// ============================================================================
// 原子自动更新计数器
// ============================================================================

type atomicAutoUpdate struct {
	checks      int64
	triggers    int64
	successes   int64
	failures    int64
	changedFiles int64
	rebuildMs   int64
}

func (a *atomicAutoUpdate) recordCheck(changedFiles int) {
	atomic.AddInt64(&a.checks, 1)
	if changedFiles > 0 {
		atomic.AddInt64(&a.triggers, 1)
	}
	atomic.AddInt64(&a.changedFiles, int64(changedFiles))
}

func (a *atomicAutoUpdate) recordTrigger(success bool, durationMs int64) {
	if success {
		atomic.AddInt64(&a.successes, 1)
	} else {
		atomic.AddInt64(&a.failures, 1)
	}
	atomic.AddInt64(&a.rebuildMs, durationMs)
}

func (a *atomicAutoUpdate) snapshot() AutoUpdateMetrics {
	return AutoUpdateMetrics{
		Timestamp:         time.Now(),
		CheckCount:        atomic.LoadInt64(&a.checks),
		TriggerCount:      atomic.LoadInt64(&a.triggers),
		SuccessCount:      atomic.LoadInt64(&a.successes),
		FailCount:         atomic.LoadInt64(&a.failures),
		ChangedFiles:      int(atomic.LoadInt64(&a.changedFiles)),
		RebuildDurationMs: atomic.LoadInt64(&a.rebuildMs),
	}
}

// ============================================================================
// 环形缓冲区（线程安全）
// ============================================================================

type ringBuffer[T any] struct {
	mu       sync.RWMutex
	data     []T
	capacity int
	head     int
	size     int
}

func newRingBuffer[T any](capacity int) *ringBuffer[T] {
	return &ringBuffer[T]{
		data:     make([]T, capacity),
		capacity: capacity,
	}
}

func (r *ringBuffer[T]) push(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[r.head] = v
	r.head = (r.head + 1) % r.capacity
	if r.size < r.capacity {
		r.size++
	}
}

func (r *ringBuffer[T]) slice() []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.size == 0 {
		return nil
	}
	out := make([]T, r.size)
	for i := 0; i < r.size; i++ {
		idx := (r.head - r.size + i + r.capacity) % r.capacity
		out[i] = r.data[idx]
	}
	return out
}
