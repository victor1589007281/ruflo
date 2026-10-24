// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// RepoQualityGate runs comprehensive quality gate checks.
// Directly invokes Go toolchain commands, no external dependencies (go itself must be installed).
type RepoQualityGate struct {
	Base
	runtime *Runtime
}

// NewRepoQualityGate 构造 repo-quality-gate 技能 (只读、不可并发): 串行跑 build/test/vet/format。
// NewRepoQualityGate creates a new RepoQualityGate.
func NewRepoQualityGate(rt *Runtime) *RepoQualityGate {
	return &RepoQualityGate{
		Base: NewBase(Meta{
			Name:           "repo-quality-gate",
			Description:    "Run comprehensive quality gate: go build, go test, go vet, gofmt. Directly invokes Go toolchain commands. This is the final validation step before marking a task complete.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: false,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root":    {"type": "string"},
					"checks":       {"type": "array", "items": {"type": "string", "enum": ["build", "test", "vet", "format"]}, "default": ["build", "test", "vet"]},
					"test_timeout": {"type": "string", "default": "60s"},
					"test_flags":   {"type": "array", "items": {"type": "string"}, "default": ["-count=1"]}
				},
				"required": ["repo_root"]
			}`),
		}),
		runtime: rt,
	}
}

// CheckResult represents the result of a single check.
type CheckResult struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Duration int64  `json:"duration_ms"`
	Output   string `json:"output,omitempty"`
	Errors   string `json:"errors,omitempty"`
}

// Execute 执行质量门禁: 按 checks 列表**逐项串行**跑 Go 工具链, 全部通过才算 passed。
//
// 算法解读：
//
//	原理：把每项检查映射成一条 exec.CommandContext (go build / go test / go vet /
//	      gofmt -l), 以退出码判定通过与否, 收集输出与耗时后汇总。
//	关键不变量：passed = **所有**检查都通过 (allPassed 一旦置 false 不会再翻回);
//	      未知的 check 名不匹配任何 case, 会产出一条 Passed=false 的空结果 ——
//	      即"写错检查名"等于"该项永远失败", 而不是被忽略。
//	代价与边界：串行执行, 总耗时是各项之和 (test 默认 60s 超时, 由 ctx 传递);
//	      任一项超时会以非零退出码落进 Passed=false, 与"真有错"不可区分。
//	设计权衡：不并行 —— build/vet/test 共享同一份构建缓存与工作目录,
//	      并发跑既抢 CPU 又让失败输出互相穿插, 反而更难定位。
//
// Execute runs the quality gate.
func (t *RepoQualityGate) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot    string   `json:"repo_root"`
		Checks      []string `json:"checks"`
		TestTimeout string   `json:"test_timeout"`
		TestFlags   []string `json:"test_flags"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	if len(in.Checks) == 0 {
		in.Checks = []string{"build", "test", "vet"}
	}
	if in.TestTimeout == "" {
		in.TestTimeout = "60s"
	}
	if len(in.TestFlags) == 0 {
		in.TestFlags = []string{"-count=1"}
	}

	var results []CheckResult
	allPassed := true

	for _, check := range in.Checks {
		var result CheckResult
		result.Name = check

		switch check {
		case "build":
			cmd := exec.CommandContext(ctx, "go", "build", "./...")
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = err == nil

		case "test":
			args := append([]string{"test", "-timeout=" + in.TestTimeout}, in.TestFlags...)
			args = append(args, "./...")
			cmd := exec.CommandContext(ctx, "go", args...)
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = err == nil

		case "vet":
			cmd := exec.CommandContext(ctx, "go", "vet", "./...")
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = err == nil

		case "format":
			// Use gofmt to check for unformatted files
			cmd := exec.CommandContext(ctx, "gofmt", "-l", ".")
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = strings.TrimSpace(string(out)) == "" && err == nil
		}

		// 未知的 check 名不命中任何 case, 会留下 Passed=false 的空结果 ——
		// 也就是说拼错检查名等价于"该项永远失败", 而不是被静默忽略。
		if !result.Passed {
			allPassed = false
		}
		results = append(results, result)
	}

	return map[string]any{
		"passed":       allPassed,
		"results":      results,
		"check_count":  len(results),
		"failed_count": len(results) - countPassed(results),
	}, nil
}

// countPassed 数出通过项个数 (供 failed_count = 总数 - 通过数 使用)。
func countPassed(results []CheckResult) int {
	c := 0
	for _, r := range results {
		if r.Passed {
			c++
		}
	}
	return c
}
