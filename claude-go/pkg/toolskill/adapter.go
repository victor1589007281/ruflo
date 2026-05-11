// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Adapter is the native tool adapter interface.
// Each Adapter wraps a native tool: responsible for calling, parsing output, and converting to structured JSON.
type Adapter interface {
	// Name returns the adapter name
	Name() string
	// Capabilities declares the capabilities this Adapter provides
	Capabilities() []string
	// Execute executes the native tool and returns structured results
	Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error)
}

// AdapterArgs generic invocation arguments.
type AdapterArgs struct {
	RepoRoot string         `json:"repo_root"`
	Files    []string       `json:"files,omitempty"`
	Extra    map[string]any `json:"extra,omitempty"` // tool-specific parameters
}

// AdapterResult generic execution result.
type AdapterResult struct {
	RawOutput   string         `json:"raw_output"`
	Diagnostics []Diagnostic   `json:"diagnostics,omitempty"`
	CheckPassed bool           `json:"check_passed,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	DurationMs  int64          `json:"duration_ms"`
}

// Diagnostic structured diagnostic information (unified output format for all Adapters).
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Message  string `json:"message"`
	Category string `json:"category"`  // syntax_error, type_error, import_error, style, warning, info
	Severity string `json:"severity"`  // error, warning, info
	Code     string `json:"code,omitempty"`     // rule code (e.g. SA4000)
	Tool     string `json:"tool,omitempty"`     // source tool name
	Fixable  bool   `json:"fixable,omitempty"`  // whether auto-fixable
}

// BaseAdapter generic adapter base class.
type BaseAdapter struct {
	name      string
	cmd       string
	args      []string
	outputFmt string
	parser    OutputParser
}

// OutputParser output parser interface.
type OutputParser interface {
	Parse(raw []byte, repoRoot string) (*AdapterResult, error)
}

// ExecAndParse executes a command and parses the output.
func (a *BaseAdapter) ExecAndParse(ctx context.Context, args []string, repoRoot string, parser OutputParser) (*AdapterResult, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, a.cmd, args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	duration := time.Since(start).Milliseconds()

	result, parseErr := parser.Parse(out, repoRoot)
	if parseErr != nil {
		// Return raw output when parsing fails
		return &AdapterResult{
			RawOutput:   string(out),
			DurationMs:  duration,
			CheckPassed: err == nil,
		}, fmt.Errorf("parse failed: %w", parseErr)
	}

	result.RawOutput = string(out)
	result.DurationMs = duration
	result.CheckPassed = err == nil && len(result.Diagnostics) == 0
	return result, nil
}
