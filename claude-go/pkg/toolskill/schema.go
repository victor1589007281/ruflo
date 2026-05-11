// Package toolskill implements the Tool Skill Runtime layer.
// It wraps native Go ecosystem tools (staticcheck, go vet, goimports, etc.)
// and exposes them as LLM-callable skills with structured JSON output.
package toolskill

import (
	"context"
	"encoding/json"
	"time"
)

// ToolSkill is the interface for executable tool skills (LLM-facing).
type ToolSkill interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	OutputSchema() json.RawMessage
	IsReadOnly() bool
	IsConcurrencySafe() bool
	Execute(ctx context.Context, input json.RawMessage) (map[string]any, error)
}

// Meta holds ToolSkill metadata.
type Meta struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Version        string          `json:"version"`
	InputSchema    json.RawMessage `json:"input_schema"`
	OutputSchema   json.RawMessage `json:"output_schema,omitempty"`
	ReadOnly       bool            `json:"read_only"`
	SafeConcurrent bool            `json:"safe_concurrent"`
	Timeout        time.Duration   `json:"timeout,omitempty"`
}

// Base provides common method implementations for ToolSkill.
type Base struct{ meta Meta }

// NewBase creates a Base from Meta.
func NewBase(meta Meta) Base { return Base{meta: meta} }

func (b *Base) Name() string                  { return b.meta.Name }
func (b *Base) Description() string           { return b.meta.Description }
func (b *Base) InputSchema() json.RawMessage  { return b.meta.InputSchema }
func (b *Base) OutputSchema() json.RawMessage { return b.meta.OutputSchema }
func (b *Base) IsReadOnly() bool              { return b.meta.ReadOnly }
func (b *Base) IsConcurrencySafe() bool       { return b.meta.SafeConcurrent }

// Registry stores registered ToolSkills.
type Registry struct {
	skills map[string]ToolSkill
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{skills: make(map[string]ToolSkill)}
}

// Register adds a ToolSkill. Existing skill with same name is overwritten.
func (r *Registry) Register(ts ToolSkill) { r.skills[ts.Name()] = ts }

// Get retrieves a ToolSkill by name.
func (r *Registry) Get(name string) (ToolSkill, bool) {
	ts, ok := r.skills[name]
	return ts, ok
}

// All returns all registered ToolSkills.
func (r *Registry) All() []ToolSkill {
	var list []ToolSkill
	for _, ts := range r.skills {
		list = append(list, ts)
	}
	return list
}

// ExecutionResult is the unified result from executing a ToolSkill.
type ExecutionResult struct {
	Success    bool           `json:"success"`
	Status     string         `json:"status,omitempty"` // "pass" | "fail" | "error"
	Data       map[string]any `json:"data,omitempty"`
	Error      string         `json:"error,omitempty"`
	Timing     TimingInfo     `json:"timing"`
	Diagnostics []Diagnostic  `json:"diagnostics,omitempty"`
}

// TimingInfo records execution timing.
type TimingInfo struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMs int64     `json:"duration_ms"`
}

// SkillRequest is the standard input for tool skill execution.
type SkillRequest struct {
	RepoRoot     string         `json:"repo_root"`
	ChangedFiles []string       `json:"changed_files,omitempty"`
	Files        []string       `json:"files,omitempty"`
	Scope        string         `json:"scope"` // "package", "file", "repo"
	Language     string         `json:"language"`
	Extra        map[string]any `json:"extra,omitempty"`
}

// WrapError wraps an error into an ExecutionResult.
func WrapError(err error) ExecutionResult {
	return ExecutionResult{
		Success: false,
		Status:  "error",
		Error:   err.Error(),
	}
}

// ToResult converts a (data, error) pair into an ExecutionResult.
func ToResult(data map[string]any, err error) ExecutionResult {
	if err != nil {
		return WrapError(err)
	}
	return ExecutionResult{
		Success: true,
		Status:  "pass",
		Data:    data,
	}
}

// errStr returns empty string for nil, otherwise err.Error().
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
