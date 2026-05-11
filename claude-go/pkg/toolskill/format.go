// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// GoFormat wraps gofmt as a Tool Skill.
type GoFormat struct {
	Base
	runtime *Runtime
}

// NewGoFormat creates a new GoFormat.
func NewGoFormat(rt *Runtime) *GoFormat {
	return &GoFormat{
		Base: NewBase(Meta{
			Name:           "go-format",
			Description:    "Format Go files using gofmt. Returns a list of unformatted files or applies formatting in-place.",
			Version:        "1.0.0",
			ReadOnly:       false,
			SafeConcurrent: false,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"files":     {"type": "array", "items": {"type": "string"}},
					"write":     {"type": "boolean", "default": false}
				},
				"required": ["repo_root"]
			}`),
		}),
		runtime: rt,
	}
}

// Execute runs the format check or fix.
func (t *GoFormat) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string   `json:"repo_root"`
		Files    []string `json:"files"`
		Write    bool     `json:"write"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	args := []string{"-l"}
	if in.Write {
		args = []string{"-w"}
	}
	if len(in.Files) > 0 {
		args = append(args, in.Files...)
	} else {
		args = append(args, ".")
	}

	cmd := exec.CommandContext(ctx, "gofmt", args...)
	cmd.Dir = in.RepoRoot
	out, err := cmd.CombinedOutput()

	output := strings.TrimSpace(string(out))
	unformatted := []string{}
	if output != "" {
		unformatted = strings.Split(output, "\n")
	}

	return map[string]any{
		"formatted":      err == nil && output == "",
		"unformatted":    unformatted,
		"raw_output":     output,
		"write_mode":     in.Write,
	}, nil
}
