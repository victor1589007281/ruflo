// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BenchmarkGate compares benchmark results against a baseline.
type BenchmarkGate struct{ Base }

// NewBenchmarkGate 构造 benchmark-gate 技能 (只读、不可并发): 跑基准并与基线对比, 检出性能回归。
// NewBenchmarkGate creates a new BenchmarkGate skill.
func NewBenchmarkGate() *BenchmarkGate {
	return &BenchmarkGate{
		Base: NewBase(Meta{
			Name:           "benchmark-gate",
			Description:    "Compare benchmark results against a baseline to detect performance regressions.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: false,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"packages":  {"type": "array", "items": {"type": "string"}}
				},
				"required": ["repo_root"]
			}`),
		}),
	}
}

// BaselineEntry stores baseline benchmark metrics.
type BaselineEntry struct {
	Name        string  `json:"name"`
	NsPerOp     float64 `json:"ns_per_op"`
	AllocsPerOp float64 `json:"allocs_per_op"`
}

// Execute 执行基准门禁: 读基线 → 跑基准 → 比对, 返回结果与回归清单。
//
// 注意：基线读不到或基准执行失败都**不以 error 返回**, 而是包成 status="error" 的
// 正常结果 —— 调用方必须读 status 字段, 只看 error 会把"没跑成"当成"没有回归"。
// Execute runs the benchmark gate.
func (b *BenchmarkGate) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string   `json:"repo_root"`
		Packages []string `json:"packages,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	baseline, err := b.loadBaseline(in.RepoRoot)
	if err != nil {
		return map[string]any{
			"status":  "error",
			"message": fmt.Sprintf("Failed to load baseline: %v", err),
		}, nil
	}

	results, regressions, err := b.runBenchmarks(ctx, in.RepoRoot, in.Packages, baseline)
	if err != nil {
		return map[string]any{
			"status":  "error",
			"message": fmt.Sprintf("Benchmark execution failed: %v", err),
		}, nil
	}

	return map[string]any{
		"status":                 "completed",
		"performance_regression": len(regressions) > 0,
		"regressions":            regressions,
		"results":                results,
	}, nil
}

// loadBaseline 从 repoRoot/benchmark_baseline.json 读基线, 按基准名索引成 map。
//
// 文件名固定, 不提供覆盖入口; 文件不存在/不是合法 JSON 时返回错误 (由 Execute 转成 status=error)。
func (b *BenchmarkGate) loadBaseline(repoRoot string) (map[string]BaselineEntry, error) {
	path := filepath.Join(repoRoot, "benchmark_baseline.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []BaselineEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	m := make(map[string]BaselineEntry)
	for _, e := range entries {
		m[e.Name] = e
	}
	return m, nil
}

// runBenchmarks 逐个包跑 `go test -bench=. -benchmem -count=1`, 汇总各基准的结果与回归。
//
// 注意：单个包执行失败 (编译不过/测试 panic) 会被 **continue 静默跳过** ——
// 那个包里的基准既不进 results 也不进 regressions, 门禁仍可能报"无回归"。
// 另外 -count=1 意味着结果不取多次中位数, 单次运行的噪声会直接进入判定。
func (b *BenchmarkGate) runBenchmarks(ctx context.Context, repoRoot string, packages []string, baseline map[string]BaselineEntry) ([]map[string]any, []map[string]any, error) {
	pkgs := packages
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}

	var results []map[string]any
	var regressions []map[string]any

	for _, pkg := range pkgs {
		cmd := exec.CommandContext(ctx, "go", "test", "-bench=.", "-benchmem", "-count=1", pkg)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			continue
		}
		res, regs := b.parseOutput(string(out), baseline)
		results = append(results, res...)
		regressions = append(regressions, regs...)
	}

	return results, regressions, nil
}

// parseOutput 逐行解析 go test -bench 的输出, 对照基线挑出 ns/op 或 allocs/op 的回归。
//
// 算法解读：
//
//	原理：用一条正则从每个 Benchmark 行里抽出名字、ns/op、allocs/op, 与基线中同名
//	      条目比较; 任一指标超过基线的 1.2 倍即记一条 regression。
//	关键不变量：正则**要求 B/op 与 allocs/op 都在行内** —— 少跑 -benchmem、
//	      或基准自身用 b.ReportAllocs() 之外的输出形态时整行不匹配, 该基准会被
//	      静默忽略 (既不算通过也不算失败), 而门禁不会因此报错。
//	代价与边界：1.2 是硬编码阈值, 且只比单次运行值 —— 基准抖动 20% 以上就误报,
//	      真回归 15% 就漏报; 基线里没有同名条目的基准只进 results 不做判定。
//	设计权衡：用正则而非结构化输出 (benchstat/JSON) —— 前者零依赖且能容忍
//	      混在输出里的编译日志; 代价是格式一变就整块失效, 且失效是无声的。
func (b *BenchmarkGate) parseOutput(output string, baseline map[string]BaselineEntry) ([]map[string]any, []map[string]any) {
	var results []map[string]any
	var regressions []map[string]any

	re := regexp.MustCompile(`Benchmark(\S+)\s+\d+\s+(\d+(?:\.\d+)?)\s+ns/op\s+(?:\d+)\s+B/op\s+(\d+)\s+allocs/op`)
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		nsPerOp, _ := strconv.ParseFloat(m[2], 64)
		allocsPerOp, _ := strconv.ParseFloat(m[3], 64)

		entry := map[string]any{
			"name":          name,
			"ns_per_op":     nsPerOp,
			"allocs_per_op": allocsPerOp,
		}
		results = append(results, entry)

		if base, ok := baseline[name]; ok {
			// 阈值硬编码 1.2 倍: 低于 20% 的真回归看不见, 而单次运行的抖动
			// 超过 20% 就会误报 (-count=1 不取中位数)。
			if nsPerOp > base.NsPerOp*1.20 {
				regressions = append(regressions, map[string]any{
					"name":        name,
					"metric":      "ns_per_op",
					"baseline":    base.NsPerOp,
					"current":     nsPerOp,
					"message":     fmt.Sprintf("ns/op regressed by >20%%: %.2f vs baseline %.2f", nsPerOp, base.NsPerOp),
				})
			}
			if allocsPerOp > base.AllocsPerOp*1.20 {
				regressions = append(regressions, map[string]any{
					"name":        name,
					"metric":      "allocs_per_op",
					"baseline":    base.AllocsPerOp,
					"current":     allocsPerOp,
					"message":     fmt.Sprintf("allocs/op regressed by >20%%: %.2f vs baseline %.2f", allocsPerOp, base.AllocsPerOp),
				})
			}
		}
	}

	return results, regressions
}
