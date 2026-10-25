package search

// metrics.go —— 进程内的最小指标集合 (counter / gauge / histogram)。
//
// 不引 prometheus: 数据面嵌在 CLI 与网关里, 指标只需要被 dashboard 或日志
// 周期拉取。标签组合按字符串拼 key, 读快照时再解析回来; 所有方法并发安全。
import (
	"sort"
	"strings"
	"sync"
)

// Metrics 是进程内的轻量指标登记器。
//
// 不引入 prometheus 依赖: 检索服务嵌入在 CLI/网关里, 指标只需被本地
// 观测面板或日志周期拉取, 用 counter/gauge/histogram 三种最小集合即可。
// 所有方法并发安全。
type Metrics struct {
	mu         sync.Mutex
	counters   map[string]int64
	gauges     map[string]float64
	histograms map[string]*histogram
}

// histogram 保存一组观测值; 快照时计算 count/sum/min/max/p50/p95。
type histogram struct {
	values []float64
}

// NewMetrics 创建空的指标登记器。
func NewMetrics() *Metrics {
	return &Metrics{
		counters:   make(map[string]int64),
		gauges:     make(map[string]float64),
		histograms: make(map[string]*histogram),
	}
}

// metricKey 把指标名与标签拼成稳定 key, 标签按名排序后拼接。
//
// 固定排序是为了让同一组标签在不同调用路径下生成同一个 key,
// 避免因 map 遍历顺序导致快照里出现两份数据。
func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

// Inc 给计数器加 1。
func (m *Metrics) Inc(name string, labels map[string]string) {
	m.Add(name, 1, labels)
}

// Add 给计数器加 delta。
func (m *Metrics) Add(name string, delta int64, labels map[string]string) {
	if m == nil {
		return
	}
	key := metricKey(name, labels)
	m.mu.Lock()
	m.counters[key] += delta
	m.mu.Unlock()
}

// Set 设置 gauge 的当前值。
func (m *Metrics) Set(name string, value float64, labels map[string]string) {
	if m == nil {
		return
	}
	key := metricKey(name, labels)
	m.mu.Lock()
	m.gauges[key] = value
	m.mu.Unlock()
}

// Observe 记录一次直方图观测。
func (m *Metrics) Observe(name string, value float64, labels map[string]string) {
	if m == nil {
		return
	}
	key := metricKey(name, labels)
	m.mu.Lock()
	h := m.histograms[key]
	if h == nil {
		h = &histogram{}
		m.histograms[key] = h
	}
	h.values = append(h.values, value)
	m.mu.Unlock()
}

// Snapshot 返回指标副本。
//
// counter 与 gauge 的值分别是 int64 与 float64; histogram 的值是
// map[string]any{count,sum,min,max,p50,p95}, 便于直接序列化成 JSON。
func (m *Metrics) Snapshot() map[string]any {
	out := make(map[string]any)
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for k, v := range m.counters {
		out[k] = v
	}
	for k, g := range m.gauges {
		out[k] = g
	}
	for k, h := range m.histograms {
		out[k] = h.snapshot()
	}
	return out
}

// snapshot 计算直方图的分位摘要。调用方已持有 Metrics.mu。
func (h *histogram) snapshot() map[string]any {
	n := len(h.values)
	if n == 0 {
		return map[string]any{
			"count": 0, "sum": float64(0), "min": float64(0),
			"max": float64(0), "p50": float64(0), "p95": float64(0),
		}
	}
	vals := make([]float64, n)
	copy(vals, h.values)
	sort.Float64s(vals)

	var sum float64
	for _, v := range vals {
		sum += v
	}
	return map[string]any{
		"count": n,
		"sum":   sum,
		"min":   vals[0],
		"max":   vals[n-1],
		"p50":   percentile(vals, 0.50),
		"p95":   percentile(vals, 0.95),
	}
}

// percentile 返回已排序切片的分位值 (最近秩法)。
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
