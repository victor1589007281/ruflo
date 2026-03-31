package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

type perfState struct {
	Profiling bool               `json:"profiling"`
	Metrics   map[string]float64 `json:"metrics"`
	LastBench time.Time          `json:"last_bench,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}

var (
	perfMu   sync.Mutex
	perfData = &perfState{Metrics: make(map[string]float64)}
)

func perfStatePath() string {
	return filepath.Join(resolveDataDir(), "performance", "state.json")
}

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

func RegisterPerformanceTools(reg *mcp.ToolRegistry) error {
	for _, t := range performanceTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handlePerformanceBenchmark(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		name = "default"
	}
	start := time.Now()
	var x int64
	for i := 0; i < 100000; i++ {
		x += int64(i)
	}
	elapsed := time.Since(start).Seconds()
	ops := 100000 / elapsed
	if elapsed <= 0 {
		ops = 0
	}
	_ = x
	perfMu.Lock()
	perfData.Metrics["last_ops_per_sec"] = ops
	perfData.Metrics["bench_"+name+"_sec"] = elapsed
	perfData.LastBench = now()
	perfMu.Unlock()
	if err := perfSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name, "ops_per_sec": ops, "elapsed_sec": elapsed}}
}

func handlePerformanceProfile(_ context.Context, m map[string]any) mcp.MCPToolResult {
	en := true
	if v, ok := m["enable"]; ok {
		if b, ok := v.(bool); ok {
			en = b
		}
	}
	perfMu.Lock()
	perfData.Profiling = en
	perfMu.Unlock()
	if err := perfSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"profiling": en}}
}

func handlePerformanceMetrics(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	perfMu.Lock()
	cp := make(map[string]float64, len(perfData.Metrics))
	for k, v := range perfData.Metrics {
		cp[k] = v
	}
	pr := perfData.Profiling
	lb := perfData.LastBench
	perfMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"metrics": cp, "profiling": pr, "last_bench": lb}}
}

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
