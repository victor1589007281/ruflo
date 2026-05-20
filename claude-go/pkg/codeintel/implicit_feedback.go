// implicit_feedback.go — 隐式反馈信号采集。
//
// 设计:
//   - 查询返回后启动超时定时器（默认 30s）
//   - 超时未重查 → 隐式好评（用户接受了结果）
//   - 短期内重查相似内容 → 隐式差评（结果未满足需求）
//   - 自动调用 VectorCache.RecordFeedback 参与阈值校准
package codeintel

import (
	"sync"
	"time"
)

const (
	defaultImplicitTimeout   = 30 * time.Second
	defaultImplicitInterval  = 5 * time.Second
	implicitSimilarityThreshold = 0.85 // 判定为"重查"的相似度阈值
)

// pendingQuery 待观察的查询记录。
type pendingQuery struct {
	queryText string
	hit       bool
	timestamp time.Time
	features  map[string]float64
}

// ImplicitFeedbackCollector 隐式反馈采集器。
type ImplicitFeedbackCollector struct {
	mu             sync.RWMutex
	pending        map[string]*pendingQuery // queryKey -> pendingQuery
	vectorCache    *VectorCache
	timeout        time.Duration
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// NewImplicitFeedbackCollector 创建隐式反馈采集器。
func NewImplicitFeedbackCollector(vc *VectorCache) *ImplicitFeedbackCollector {
	return &ImplicitFeedbackCollector{
		pending:     make(map[string]*pendingQuery),
		vectorCache: vc,
		timeout:     defaultImplicitTimeout,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动后台超时扫描 goroutine。
func (c *ImplicitFeedbackCollector) Start() {
	c.wg.Add(1)
	go c.timeoutWorker()
}

// Stop 停止后台 goroutine。
func (c *ImplicitFeedbackCollector) Stop() {
	close(c.stopCh)
	c.wg.Wait()
}

// TrackQuery 记录一次查询，开始隐式观察。
func (c *ImplicitFeedbackCollector) TrackQuery(queryText string, hit bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := normalizeQuery(queryText)
	c.pending[key] = &pendingQuery{
		queryText: queryText,
		hit:       hit,
		timestamp: time.Now(),
		features:  ExtractFeatures(queryText),
	}
}

// ObserveQuery 当新查询到达时调用，检查是否与 pending 中的查询相似。
// 如果相似，则标记原查询为 Resent（差评）。
func (c *ImplicitFeedbackCollector) ObserveQuery(queryText string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.pending) == 0 {
		return
	}

	newFeatures := ExtractFeatures(queryText)
	now := time.Now()

	for key, pq := range c.pending {
		if now.Sub(pq.timestamp) < time.Second {
			// 忽略 1 秒内的重复（可能是同一查询的重试）
			continue
		}
		sim := CosineSimilarity(newFeatures, pq.features)
		if sim >= implicitSimilarityThreshold {
			// 发现重查：提交差评反馈，移除 pending
			if c.vectorCache != nil {
				c.vectorCache.RecordFeedback(pq.queryText, pq.hit, 0, false, true)
			}
			delete(c.pending, key)
		}
	}
}

// timeoutWorker 后台扫描 pendingQueries，超时的标记为好评。
func (c *ImplicitFeedbackCollector) timeoutWorker() {
	defer c.wg.Done()
	ticker := time.NewTicker(defaultImplicitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.flushTimeouts()
		}
	}
}

// flushTimeouts 将超时的 pending 查询标记为好评并移除。
func (c *ImplicitFeedbackCollector) flushTimeouts() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, pq := range c.pending {
		if now.Sub(pq.timestamp) >= c.timeout {
			// 超时未重查：提交好评反馈
			if c.vectorCache != nil {
				c.vectorCache.RecordFeedback(pq.queryText, pq.hit, 0, true, false)
			}
			delete(c.pending, key)
		}
	}
}

// PendingCount 返回当前待观察的查询数量（用于监控）。
func (c *ImplicitFeedbackCollector) PendingCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pending)
}

// SetTimeout 设置隐式反馈超时时间。
func (c *ImplicitFeedbackCollector) SetTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.timeout = d
	}
}
