// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
)

// GoStaticCheck static analysis Tool Skill.
// Automatically detects available static analysis tools in the environment and calls them by priority:
//   staticcheck -f json ./...  (preferred, richest output)
//   golangci-lint run --out-format=json ./...
//   go vet ./...
//   go build ./...  (final fallback)
type GoStaticCheck struct {
	Base
	runtime *Runtime
}

// NewGoStaticCheck creates a new GoStaticCheck.
func NewGoStaticCheck(rt *Runtime) *GoStaticCheck {
	return &GoStaticCheck{
		Base: NewBase(Meta{
			Name:           "go-static-check",
			Description:    "Run static analysis on Go files before compilation. Automatically detects and uses the best available tool (staticcheck > golangci-lint > go vet > go build). Returns structured diagnostics with file, line, category, and severity.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"files":     {"type": "array", "items": {"type": "string"}},
					"strict":    {"type": "boolean", "default": false}
				},
				"required": ["repo_root"]
			}`),
		}),
		runtime: rt,
	}
}

// Execute runs the static check.
func (t *GoStaticCheck) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string   `json:"repo_root"`
		Files    []string `json:"files"`
		Strict   bool     `json:"strict"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	// Try to get static-check adapter (may map to staticcheck / golangci-lint / go vet)
	var result *AdapterResult
	var usedTool string

	if adapter, ok := t.runtime.adapters["static-check"]; ok {
		res, err := adapter.Execute(ctx, AdapterArgs{RepoRoot: in.RepoRoot, Files: in.Files})
		if err == nil && res != nil {
			result = res
			usedTool = adapter.Name()
		}
	}

	// If static-check adapter returned no result, fallback to go build
	if result == nil {
		buildAdapter := t.runtime.adapters["build"]
		res, err := buildAdapter.Execute(ctx, AdapterArgs{RepoRoot: in.RepoRoot})
		if err == nil && res != nil {
			result = res
			usedTool = "go-build"
		}
	}

	if result == nil {
		return nil, fmt.Errorf("no static analysis tool available")
	}

	// Filter diagnostics (strict mode keeps warnings)
	var diagnostics []Diagnostic
	for _, d := range result.Diagnostics {
		if in.Strict || d.Severity == "error" {
			diagnostics = append(diagnostics, d)
		}
	}

	return map[string]any{
		"diagnostics":   diagnostics,
		"error_count":   countBySeverity(diagnostics, "error"),
		"warning_count": countBySeverity(diagnostics, "warning"),
		"tool_used":     usedTool,
		"raw_output":    result.RawOutput,
	}, nil
}

func countBySeverity(d []Diagnostic, sev string) int {
	c := 0
	for _, diag := range d {
		if diag.Severity == sev {
			c++
		}
	}
	return c
}
