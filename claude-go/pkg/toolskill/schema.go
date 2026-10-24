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

// NewBase 由 Meta 构造一个 Base —— 供各 skill 内嵌, 提供 Name/Description/Schema 等默认实现。
// NewBase creates a Base from Meta.
func NewBase(meta Meta) Base { return Base{meta: meta} }

// Name 返回技能名 (注册表的键)。
func (b *Base) Name() string                  { return b.meta.Name }
// Description 返回给模型看的技能描述。
func (b *Base) Description() string           { return b.meta.Description }
// InputSchema 返回输入 JSON Schema。
func (b *Base) InputSchema() json.RawMessage  { return b.meta.InputSchema }
// OutputSchema 返回输出 JSON Schema (可为空)。
func (b *Base) OutputSchema() json.RawMessage { return b.meta.OutputSchema }
// IsReadOnly 报告本技能是否只读 (不产生副作用)。
func (b *Base) IsReadOnly() bool              { return b.meta.ReadOnly }
// IsConcurrencySafe 报告本技能能否与其他调用并发执行。
func (b *Base) IsConcurrencySafe() bool       { return b.meta.SafeConcurrent }

// Registry stores registered ToolSkills.
type Registry struct {
	skills map[string]ToolSkill
}

// NewRegistry 创建一个空的技能注册表。
// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{skills: make(map[string]ToolSkill)}
}

// Register 注册一个 ToolSkill; 同名会直接覆盖 (不做冲突检测)。
// Register adds a ToolSkill. Existing skill with same name is overwritten.
func (r *Registry) Register(ts ToolSkill) { r.skills[ts.Name()] = ts }

// Get 按名取 ToolSkill, 第二个返回值为是否存在。
// Get retrieves a ToolSkill by name.
func (r *Registry) Get(name string) (ToolSkill, bool) {
	ts, ok := r.skills[name]
	return ts, ok
}

// All 返回全部已注册的 ToolSkill (map 遍历, 顺序不定)。
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

// WrapError 把 error 包成失败的 ExecutionResult (Success=false, Status="error")。
// WrapError wraps an error into an ExecutionResult.
func WrapError(err error) ExecutionResult {
	return ExecutionResult{
		Success: false,
		Status:  "error",
		Error:   err.Error(),
	}
}

// ToResult 把 (data, err) 对转成 ExecutionResult: err 非空走 WrapError, 否则记为 pass。
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

// errStr 返回 err.Error(); err 为 nil 时返回空串。
// errStr returns empty string for nil, otherwise err.Error().
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
