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

func countPassed(results []CheckResult) int {
	c := 0
	for _, r := range results {
		if r.Passed {
			c++
		}
	}
	return c
}
