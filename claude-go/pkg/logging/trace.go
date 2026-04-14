// trace.go 轻量级分布式追踪: TraceID + SpanID + 结构化事件 + 指标计数器。
// 不引入 OpenTelemetry 依赖, 基于 context 传播和 slog 结构化输出。
// 所有 trace 数据通过 slog JSON 格式写入 trace.log, 可被后续 AI 分析。
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Trace Context ───────────────────────────────────────────────────────────

type ctxKey int

const (
	traceIDKey ctxKey = iota
	spanIDKey
	spanStartKey
	spanNameKey
)

// NewTraceID 生成 16 字节的 trace ID (hex 编码)
func NewTraceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSpanID 生成 8 字节的 span ID
func NewSpanID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// WithTrace 在 context 中注入新的 trace ID, 用于请求入口 (如飞书消息接收)
func WithTrace(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, traceIDKey, NewTraceID())
	return ctx
}

// WithSpan 在当前 trace 下创建子 span, 返回新 context 和结束函数
// 调用方式: ctx, end := logging.WithSpan(ctx, "team.execute"); defer end()
func WithSpan(ctx context.Context, name string) (context.Context, func()) {
	traceID, _ := ctx.Value(traceIDKey).(string)
	if traceID == "" {
		ctx = WithTrace(ctx)
		traceID, _ = ctx.Value(traceIDKey).(string)
	}
	spanID := NewSpanID()
	start := time.Now()

	ctx = context.WithValue(ctx, spanIDKey, spanID)
	ctx = context.WithValue(ctx, spanStartKey, start)
	ctx = context.WithValue(ctx, spanNameKey, name)

	Trace().InfoContext(ctx, "span.start",
		"trace_id", traceID,
		"span_id", spanID,
		"span", name,
	)

	return ctx, func() {
		elapsed := time.Since(start)
		Trace().InfoContext(ctx, "span.end",
			"trace_id", traceID,
			"span_id", spanID,
			"span", name,
			"duration_ms", elapsed.Milliseconds(),
		)
		RecordLatency(name, elapsed)
	}
}

// TraceID 从 context 提取 trace ID
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey).(string)
	return id
}

// SpanID 从 context 提取当前 span ID
func SpanID(ctx context.Context) string {
	id, _ := ctx.Value(spanIDKey).(string)
	return id
}

// Event 记录结构化事件 (带 trace 上下文)
func Event(ctx context.Context, event string, attrs ...any) {
	traceID := TraceID(ctx)
	spanID := SpanID(ctx)
	base := []any{"trace_id", traceID, "span_id", spanID, "event", event}
	Trace().InfoContext(ctx, event, append(base, attrs...)...)
}

// EventError 记录错误事件
func EventError(ctx context.Context, event string, err error, attrs ...any) {
	traceID := TraceID(ctx)
	spanID := SpanID(ctx)
	base := []any{"trace_id", traceID, "span_id", spanID, "event", event, "error", err.Error()}
	Trace().ErrorContext(ctx, event, append(base, attrs...)...)
}

// Trace 返回 trace 专用 logger (写入 trace.log)
func Trace() *slog.Logger {
	return For("trace")
}

// ─── Metrics (轻量级计数器/延迟统计) ──────────────────────────────────────────

// MetricsStore 全局指标存储
var globalMetrics = &MetricsStore{
	counters:   make(map[string]*atomic.Int64),
	latencies:  make(map[string]*LatencyBucket),
}

type MetricsStore struct {
	mu         sync.RWMutex
	counters   map[string]*atomic.Int64
	latencies  map[string]*LatencyBucket
}

// LatencyBucket 延迟分桶统计
type LatencyBucket struct {
	count    atomic.Int64
	totalMs  atomic.Int64
	maxMs    atomic.Int64
}

// IncrCounter 增加计数器
func IncrCounter(name string) {
	globalMetrics.mu.RLock()
	c, ok := globalMetrics.counters[name]
	globalMetrics.mu.RUnlock()
	if !ok {
		globalMetrics.mu.Lock()
		if c, ok = globalMetrics.counters[name]; !ok {
			c = &atomic.Int64{}
			globalMetrics.counters[name] = c
		}
		globalMetrics.mu.Unlock()
	}
	c.Add(1)
}

// RecordLatency 记录延迟
func RecordLatency(name string, d time.Duration) {
	globalMetrics.mu.RLock()
	b, ok := globalMetrics.latencies[name]
	globalMetrics.mu.RUnlock()
	if !ok {
		globalMetrics.mu.Lock()
		if b, ok = globalMetrics.latencies[name]; !ok {
			b = &LatencyBucket{}
			globalMetrics.latencies[name] = b
		}
		globalMetrics.mu.Unlock()
	}
	ms := d.Milliseconds()
	b.count.Add(1)
	b.totalMs.Add(ms)
	for {
		old := b.maxMs.Load()
		if ms <= old || b.maxMs.CompareAndSwap(old, ms) {
			break
		}
	}
}

// MetricSnapshot 单个指标快照
type MetricSnapshot struct {
	Name    string  `json:"name"`
	Count   int64   `json:"count"`
	AvgMs   float64 `json:"avg_ms,omitempty"`
	MaxMs   int64   `json:"max_ms,omitempty"`
}

// Snapshot 导出当前所有指标
func Snapshot() []MetricSnapshot {
	var result []MetricSnapshot

	globalMetrics.mu.RLock()
	defer globalMetrics.mu.RUnlock()

	for name, c := range globalMetrics.counters {
		result = append(result, MetricSnapshot{Name: "counter." + name, Count: c.Load()})
	}
	for name, b := range globalMetrics.latencies {
		count := b.count.Load()
		avg := float64(0)
		if count > 0 {
			avg = float64(b.totalMs.Load()) / float64(count)
		}
		result = append(result, MetricSnapshot{
			Name:  "latency." + name,
			Count: count,
			AvgMs: avg,
			MaxMs: b.maxMs.Load(),
		})
	}
	return result
}

// ─── 团队运行评估 ─────────────────────────────────────────────────────────────

// TeamRunReport 团队运行结构化报告 (用于 AI 分析)
type TeamRunReport struct {
	TeamName    string            `json:"team_name"`
	Workflow    string            `json:"workflow"`
	Objective   string            `json:"objective"`
	StartTime   time.Time         `json:"start_time"`
	EndTime     time.Time         `json:"end_time"`
	DurationSec float64           `json:"duration_sec"`
	Status      string            `json:"status"`
	Stages      []StageReport     `json:"stages"`
	Scores      map[string]float64 `json:"scores,omitempty"`
	Issues      []string          `json:"issues,omitempty"`
}

// StageReport 阶段运行报告
type StageReport struct {
	Name        string  `json:"name"`
	Role        string  `json:"role"`
	DurationSec float64 `json:"duration_sec"`
	Status      string  `json:"status"`
	OutputLen   int     `json:"output_len"`
	Error       string  `json:"error,omitempty"`
}

// LogTeamRun 记录团队运行的结构化日志
func LogTeamRun(ctx context.Context, report TeamRunReport) {
	Trace().InfoContext(ctx, "team.run.complete",
		"trace_id", TraceID(ctx),
		"team", report.TeamName,
		"workflow", report.Workflow,
		"duration_sec", fmt.Sprintf("%.1f", report.DurationSec),
		"status", report.Status,
		"stage_count", len(report.Stages),
		"issues", len(report.Issues),
	)
}
