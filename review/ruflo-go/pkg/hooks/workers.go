package hooks

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// WorkerPriority orders dispatch.
type WorkerPriority string

const (
	WorkerPriorityCritical WorkerPriority = "critical"
	WorkerPriorityHigh     WorkerPriority = "high"
	WorkerPriorityNormal   WorkerPriority = "normal"
	WorkerPriorityLow      WorkerPriority = "low"
)

// WorkerConfig describes a background worker.
type WorkerConfig struct {
	Name            string
	Priority        WorkerPriority
	TriggerPatterns []*regexp.Regexp
	Handler         func(ctx context.Context, wc WorkerContext) WorkerResult
}

// WorkerContext is passed into worker handlers.
type WorkerContext struct {
	Trigger string
	Args    map[string]any
}

// WorkerResult is the outcome of a worker run.
type WorkerResult struct {
	WorkerID string         `json:"worker_id"`
	OK       bool           `json:"ok"`
	Message  string         `json:"message,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Duration time.Duration  `json:"-"`
}

// workerRuntime tracks status for one worker.
type workerRuntime struct {
	cfg        WorkerConfig
	lastRun    time.Time
	lastResult WorkerResult
	runCount   int64
	errCount   int64
	cancel     context.CancelFunc
	currentJob context.Context
}

// WorkerManager dispatches triggers to registered workers using a bounded goroutine pool.
type WorkerManager struct {
	mu       sync.RWMutex
	workers  map[string]*workerRuntime
	order    []string
	poolSize int
	sem      chan struct{}
}

// NewWorkerManager creates a manager with the 12 default workers and pool size.
func NewWorkerManager() *WorkerManager {
	m := &WorkerManager{
		workers:  make(map[string]*workerRuntime),
		poolSize: 8,
		sem:      make(chan struct{}, 8),
	}
	for _, cfg := range defaultWorkerConfigs() {
		m.workers[cfg.Name] = &workerRuntime{cfg: cfg}
		m.order = append(m.order, cfg.Name)
	}
	sort.Strings(m.order)
	return m
}

// mustRE 编译正则，非法模式会在启动时 panic（仅用于包内常量模式字符串）。
func mustRE(s string) *regexp.Regexp {
	return regexp.MustCompile(s)
}

// defaultWorkerConfigs 返回 12 个预置 Worker 的配置（名称、优先级、触发正则），并附加真实 Handler。
func defaultWorkerConfigs() []WorkerConfig {
	cfgs := []WorkerConfig{
		{Name: "ultralearn", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)ultralearn|learn`)}},
		{Name: "optimize", Priority: WorkerPriorityHigh, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)optim|perf`)}},
		{Name: "consolidate", Priority: WorkerPriorityLow, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)consolidat|merge memory`)}},
		{Name: "predict", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)predict|preload`)}},
		{Name: "audit", Priority: WorkerPriorityCritical, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)audit|security`)}},
		{Name: "map", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)map|codebase`)}},
		{Name: "preload", Priority: WorkerPriorityLow, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)preload|warm`)}},
		{Name: "deepdive", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)deepdive|analyze`)}},
		{Name: "document", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)document|doc`)}},
		{Name: "refactor", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)refactor`)}},
		{Name: "benchmark", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)benchmark|bench`)}},
		{Name: "testgaps", Priority: WorkerPriorityNormal, TriggerPatterns: []*regexp.Regexp{mustRE(`(?i)test gap|coverage gap`)}},
	}
	return attachRealHandlers(cfgs)
}

// Dispatch 根据 trigger 匹配所有 Worker：先按 WorkerPriority 稳定排序，再对每个匹配项调用 runOne。
// 算法：matchesTrigger 对正则列表做 MatchString；无模式时仅当 trigger 去空白后为空则匹配。
func (m *WorkerManager) Dispatch(ctx context.Context, trigger string, wctx WorkerContext) []WorkerResult {
	if wctx.Args == nil {
		wctx.Args = map[string]any{}
	}
	wctx.Trigger = trigger
	m.mu.RLock()
	list := make([]*workerRuntime, 0, len(m.workers))
	for _, id := range m.order {
		list = append(list, m.workers[id])
	}
	m.mu.RUnlock()

	prioRank := map[WorkerPriority]int{WorkerPriorityCritical: 0, WorkerPriorityHigh: 1, WorkerPriorityNormal: 2, WorkerPriorityLow: 3}
	sort.SliceStable(list, func(i, j int) bool {
		return prioRank[list[i].cfg.Priority] < prioRank[list[j].cfg.Priority]
	})

	var results []WorkerResult
	for _, wr := range list {
		if !matchesTrigger(wr.cfg.TriggerPatterns, trigger) {
			continue
		}
		res := m.runOne(ctx, wr, wctx)
		results = append(results, res)
	}
	return results
}

// matchesTrigger 判断触发字符串是否命中任一模式；pats 为空时要求 trigger 空白为空。
func matchesTrigger(pats []*regexp.Regexp, trigger string) bool {
	if len(pats) == 0 {
		return strings.TrimSpace(trigger) == ""
	}
	for _, re := range pats {
		if re != nil && re.MatchString(trigger) {
			return true
		}
	}
	return false
}

// runOne 在信号量限流下执行单个 Worker：为新任务创建可取消子上下文，若同 Worker 已有任务则先 cancel 旧任务；
// 更新 lastRun、runCount、errCount 等统计后返回结果。
func (m *WorkerManager) runOne(parent context.Context, wr *workerRuntime, wctx WorkerContext) WorkerResult {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	cctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	if wr.cancel != nil {
		wr.cancel()
	}
	wr.cancel = cancel
	wr.currentJob = cctx
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		wr.cancel = nil
		wr.currentJob = nil
		m.mu.Unlock()
		cancel()
	}()

	start := time.Now()
	res := wr.cfg.Handler(cctx, wctx)
	res.Duration = time.Since(start)
	res.WorkerID = wr.cfg.Name

	m.mu.Lock()
	wr.lastRun = time.Now()
	wr.lastResult = res
	wr.runCount++
	if !res.OK {
		wr.errCount++
	}
	m.mu.Unlock()
	return res
}

// ListWorkers 按内部 order 顺序返回各 Worker 的 WorkerConfig 副本。
func (m *WorkerManager) ListWorkers() []WorkerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]WorkerConfig, 0, len(m.order))
	for _, name := range m.order {
		if wr, ok := m.workers[name]; ok {
			out = append(out, wr.cfg)
		}
	}
	return out
}

// GetStatus 返回指定 Worker 的运行期统计 map（busy 由 cancel!=nil 推断）。
func (m *WorkerManager) GetStatus(workerID string) (map[string]any, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	wr, ok := m.workers[workerID]
	if !ok {
		return nil, fmt.Errorf("workers: unknown worker %q", workerID)
	}
	busy := wr.cancel != nil
	return map[string]any{
		"name":        wr.cfg.Name,
		"priority":    wr.cfg.Priority,
		"last_run":    wr.lastRun,
		"run_count":   wr.runCount,
		"error_count": wr.errCount,
		"busy":        busy,
		"last_ok":     wr.lastResult.OK,
		"last_msg":    wr.lastResult.Message,
	}, nil
}

// SetPoolSize 更新并发池容量：重建 sem channel，已在执行的 goroutine 不会被强行终止。
func (m *WorkerManager) SetPoolSize(n int) {
	if n <= 0 {
		n = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolSize = n
	m.sem = make(chan struct{}, n)
}
