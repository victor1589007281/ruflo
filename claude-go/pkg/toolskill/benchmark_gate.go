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
