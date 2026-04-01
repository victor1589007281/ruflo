package tools

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"runtime"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	"github.com/ruflo/ruflo-go/pkg/memory"
)

// 本文件：性能基准、画像开关、指标与报告 MCP 工具；状态持久化至 performance/state.json。
//
// 设计思路：benchmark 用简单循环估算 ops/sec 并写入 Metrics；bottleneck 取 Metrics 中数值最大项作为启发式热点；
// report 导出格式化 JSON 到 report.json。

type perfState struct {
	Profiling bool               `json:"profiling"`
	Metrics   map[string]float64 `json:"metrics"`
	LastBench time.Time          `json:"last_bench,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// perfState 描述画像开关、浮点指标表、上次基准时间与更新时间。

var (
	perfMu   sync.Mutex
	perfData = &perfState{Metrics: make(map[string]float64)}
)

// perfStatePath 返回 performance/state.json 路径。
func perfStatePath() string {
	return filepath.Join(resolveDataDir(), "performance", "state.json")
}

// perfLoad 从磁盘恢复 perfState；Metrics 为 nil 时初始化为空 map。
func perfLoad() {
	perfMu.Lock()
	defer perfMu.Unlock()
	b, err := os.ReadFile(perfStatePath())
	if err != nil {
		return
	}
	var s perfState
	if json.Unmarshal(b, &s) == nil {
		if s.Metrics == nil {
			s.Metrics = make(map[string]float64)
		}
		perfData = &s
	}
}

// perfSave 更新 UpdatedAt 后深拷贝 Metrics 并写入 perfStatePath。
func perfSave() error {
	perfMu.Lock()
	perfData.UpdatedAt = now()
	cp := perfState{
		Profiling: perfData.Profiling,
		Metrics:   make(map[string]float64),
		LastBench: perfData.LastBench,
		UpdatedAt: perfData.UpdatedAt,
	}
	for k, v := range perfData.Metrics {
		cp.Metrics[k] = v
	}
	perfMu.Unlock()
	return writeJSONFile(perfStatePath(), cp)
}

// performanceTools 先 perfLoad，再注册 benchmark/profile/metrics/report/bottleneck/optimize。
func performanceTools() []*mcp.MCPTool {
	perfLoad()
	return []*mcp.MCPTool{
		{Name: "performance_benchmark", Description: "Run a performance benchmark", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}, Handler: toolHandler(handlePerformanceBenchmark)},
		{Name: "performance_profile", Description: "Start or stop profiling", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"enable": map[string]any{"type": "boolean"}}}, Handler: toolHandler(handlePerformanceProfile)},
		{Name: "performance_metrics", Description: "Get performance metrics", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handlePerformanceMetrics)},
		{Name: "performance_report", Description: "Generate performance report file", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handlePerformanceReport)},
		{Name: "performance_bottleneck", Description: "Heuristic bottleneck from stored metrics", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handlePerformanceBottleneck)},
		{Name: "performance_optimize", Description: "Mark optimization recommendation applied", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"hint": map[string]any{"type": "string"}}}, Handler: toolHandler(handlePerformanceOptimize)},
	}
}

// RegisterPerformanceTools 注册性能相关 MCP 工具。
func RegisterPerformanceTools(reg *mcp.ToolRegistry) error {
	for _, t := range performanceTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handlePerformanceBenchmark 对 CPU、本地 HNSW、统一 memory、Hook 执行等路径做微基准并写入 Metrics。
func handlePerformanceBenchmark(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		name = "default"
	}
	out := map[string]any{"name": name}

	// CPU baseline
	t0 := time.Now()
	var x int64
	const cpuIters = 100000
	for i := 0; i < cpuIters; i++ {
		x += int64(i)
	}
	cpuSec := time.Since(t0).Seconds()
	_ = x
	cpuOps := float64(cpuIters) / math.Max(cpuSec, 1e-9)

	// HNSW search (ephemeral index)
	idx := memory.NewHNSWIndex(embeddings.HashEmbeddingDim, memory.CosineDistance)
	const nVec = 200
	for i := 0; i < nVec; i++ {
		_ = idx.Insert(uint64(i+1), embeddings.HashEmbed384("perf-bench-"+strconv.Itoa(i)))
	}
	q := embeddings.HashEmbed384("perf-query-" + name)
	t1 := time.Now()
	const searchRepeats = 50
	for i := 0; i < searchRepeats; i++ {
		_ = idx.Search(q, 8, 64)
	}
	hnswSec := time.Since(t1).Seconds()
	hnswQPS := float64(searchRepeats) / math.Max(hnswSec, 1e-9)

	// Memory store/retrieve
	memOps := 0.0
	memSec := 0.0
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		t2 := time.Now()
		const memIters = 30
		for i := 0; i < memIters; i++ {
			k := "perf-bench-" + name + "-" + strconv.Itoa(i)
			_ = u.Store(memory.MemoryEntryInput{Key: k, Value: "v", Namespace: "perf"})
			_, _ = u.Retrieve(k, "perf")
		}
		memSec = time.Since(t2).Seconds()
		memOps = float64(memIters) / math.Max(memSec, 1e-9)
		out["memory_backend"] = "unified"
	} else {
		out["memory_backend"] = "skipped"
	}

	// Hook executor
	t3 := time.Now()
	const hookIters = 40
	for i := 0; i < hookIters; i++ {
		_ = globalState.hookExec.SessionStart("perf-session-" + name + "-" + strconv.Itoa(i))
	}
	hookSec := time.Since(t3).Seconds()
	hookOps := float64(hookIters) / math.Max(hookSec, 1e-9)

	perfMu.Lock()
	perfData.Metrics["last_ops_per_sec"] = cpuOps
	perfData.Metrics["bench_"+name+"_cpu_sec"] = cpuSec
	perfData.Metrics["bench_"+name+"_hnsw_queries_per_sec"] = hnswQPS
	perfData.Metrics["bench_"+name+"_hnsw_sec"] = hnswSec
	perfData.Metrics["bench_"+name+"_memory_roundtrips_per_sec"] = memOps
	perfData.Metrics["bench_"+name+"_memory_sec"] = memSec
	perfData.Metrics["bench_"+name+"_hook_invokes_per_sec"] = hookOps
	perfData.Metrics["bench_"+name+"_hook_sec"] = hookSec
	perfData.LastBench = now()
	perfMu.Unlock()
	if err := perfSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}

	out["cpu_ops_per_sec"] = cpuOps
	out["cpu_elapsed_sec"] = cpuSec
	out["hnsw_queries_per_sec"] = hnswQPS
	out["hnsw_elapsed_sec"] = hnswSec
	out["memory_roundtrips_per_sec"] = memOps
	out["memory_elapsed_sec"] = memSec
	out["hook_invokes_per_sec"] = hookOps
	out["hook_elapsed_sec"] = hookSec
	return mcp.MCPToolResult{OK: true, Data: out}
}

// handlePerformanceProfile 切换 Profiling 标志并采样 runtime.MemStats、协程数与 CPU 数。
func handlePerformanceProfile(_ context.Context, m map[string]any) mcp.MCPToolResult {
	en := true
	if v, ok := m["enable"]; ok {
		if b, ok := v.(bool); ok {
			en = b
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	perfMu.Lock()
	perfData.Profiling = en
	perfMu.Unlock()
	if err := perfSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"profiling":           en,
		"alloc_bytes":         ms.Alloc,
		"total_alloc_bytes":   ms.TotalAlloc,
		"sys_bytes":           ms.Sys,
		"heap_objects":        ms.HeapObjects,
		"num_gc":              ms.NumGC,
		"goroutines":          runtime.NumGoroutine(),
		"gomaxprocs":          runtime.GOMAXPROCS(0),
		"num_cpu":             runtime.NumCPU(),
	}}
}

// handlePerformanceMetrics 聚合持久化指标与当前进程 runtime 快照。
func handlePerformanceMetrics(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	perfMu.Lock()
	cp := make(map[string]float64, len(perfData.Metrics))
	for k, v := range perfData.Metrics {
		cp[k] = v
	}
	pr := perfData.Profiling
	lb := perfData.LastBench
	perfMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"metrics": cp, "profiling": pr, "last_bench": lb,
		"runtime": map[string]any{
			"alloc_bytes": ms.Alloc, "sys_bytes": ms.Sys, "heap_objects": ms.HeapObjects,
			"num_gc": ms.NumGC, "goroutines": runtime.NumGoroutine(), "num_cpu": runtime.NumCPU(),
		},
	}}
}

// handlePerformanceBottleneck 在已存 Metrics 中取数值最大的键作为启发式瓶颈 metric，并附带 profiling_on。
func handlePerformanceBottleneck(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	perfMu.Lock()
	var worst string
	var worstV float64
	for k, v := range perfData.Metrics {
		if v > worstV {
			worstV, worst = v, k
		}
	}
	prof := perfData.Profiling
	perfMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"metric": worst, "value": worstV, "profiling_on": prof}}
}

// handlePerformanceOptimize 将 optimize_<hint> 指标记为 1，表示某条优化建议已采纳（占位语义）。
func handlePerformanceOptimize(_ context.Context, m map[string]any) mcp.MCPToolResult {
	hint := strArg(m, "hint")
	if hint == "" {
		hint = "default"
	}
	perfMu.Lock()
	perfData.Metrics["optimize_"+hint] = 1
	perfMu.Unlock()
	if err := perfSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"hint": hint, "applied": true}}
}

// handlePerformanceReport 将当前 perfState 以缩进 JSON 写入 performance/report.json，返回文件路径。
func handlePerformanceReport(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	perfMu.Lock()
	cp := perfState{
		Profiling: perfData.Profiling,
		Metrics:   make(map[string]float64),
		LastBench: perfData.LastBench,
		UpdatedAt: perfData.UpdatedAt,
	}
	for k, v := range perfData.Metrics {
		cp.Metrics[k] = v
	}
	perfMu.Unlock()
	body, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	p := filepath.Join(resolveDataDir(), "performance", "report.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"path": p}}
}
