// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/anthropic/claude-go/pkg/contract"
)

// Runtime integrates Detector + Adapter + Registry.
type Runtime struct {
	registry *Registry
	detector *ToolDetector
	adapters map[string]Adapter // capability -> adapter
}

// NewRuntime creates a new Runtime.
func NewRuntime(reg *Registry) *Runtime {
	return &Runtime{
		registry: reg,
		detector: NewToolDetector(),
		adapters: make(map[string]Adapter),
	}
}

// Init initializes: scans tools, registers default Adapters.
func (rt *Runtime) Init(ctx context.Context) {
	rt.detector.Scan(ctx)

	// Register Adapters (by priority, later ones override earlier ones)
	if cap := rt.detector.Resolve("static-check"); cap != nil {
		switch cap.Name {
		case "staticcheck":
			rt.adapters["static-check"] = &StaticcheckAdapter{cap: cap}
		case "golangci-lint":
			rt.adapters["static-check"] = &GolangCILintAdapter{cap: cap}
		}
	}
	// Always register go vet / go build fallback
	rt.adapters["build"] = &GoBuildAdapter{}
	rt.adapters["format"] = &GoFormatAdapter{detector: rt.detector}
	rt.adapters["import-fix"] = &GoImportsAdapter{detector: rt.detector}

	log.Printf("[ToolSkill] Initialized adapters: %v", rt.AdapterNames())
}

// AdapterNames returns all initialized adapter capability names.
func (rt *Runtime) AdapterNames() []string {
	var names []string
	for k := range rt.adapters {
		names = append(names, k)
	}
	return names
}

// Execute executes a Tool Skill by name.
//
// Status and Diagnostics must be populated here, not left to the caller.
// Skills return a bare (map[string]any, error) pair and put their findings
// under Data["diagnostics"], while consumers (e.g. the validation gate) read
// ExecutionResult.Status and ExecutionResult.Diagnostics. Leaving both unset
// made every stage read as "not passed, with zero blockers" — indistinguishable
// from an unrecoverable failure, so the gate could never report a pass no
// matter how clean the code was.
func (rt *Runtime) Execute(ctx context.Context, name string, input json.RawMessage) ExecutionResult {
	ts, ok := rt.registry.Get(name)
	if !ok {
		return WrapError(fmt.Errorf("tool skill %q not found", name))
	}
	start := time.Now()
	data, err := ts.Execute(ctx, input)
	diags := hoistDiagnostics(data)
	return ExecutionResult{
		Success:     err == nil,
		Status:      deriveStatus(data, diags, err),
		Data:        data,
		Error:       errStr(err),
		Diagnostics: diags,
		Timing:      TimingInfo{Start: start, End: time.Now(), DurationMs: time.Since(start).Milliseconds()},
	}
}

// hoistDiagnostics lifts Data["diagnostics"] to the top level.
//
// The []any branch matters: skills invoked in-process hand back a typed
// []Diagnostic, but the same payload round-tripped through JSON (cached
// results, cross-process workers) decodes to []any of map[string]any.
// Handling only the typed case would silently drop every diagnostic there.
func hoistDiagnostics(data map[string]any) []Diagnostic {
	raw, ok := data["diagnostics"]
	if !ok || raw == nil {
		return nil
	}
	if ds, ok := raw.([]Diagnostic); ok {
		return ds
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]Diagnostic, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		// Re-marshal rather than copy field by field, so Diagnostic can gain
		// fields without this loop silently ignoring them.
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		var d Diagnostic
		if json.Unmarshal(b, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// deriveStatus maps a skill's outcome onto the "pass" | "fail" | "error"
// vocabulary that ExecutionResult.Status documents.
//
//   - error: the skill itself blew up (tool missing, bad input). Distinct from
//     "fail" because it means we learned nothing about the code under test.
//   - fail: the skill ran and found blocking problems.
//   - pass: the skill ran and found nothing blocking.
//
// A skill may also state its own Data["status"]; an explicit "error" there is
// honoured (benchmark_gate uses it to report a missing baseline), but its
// non-error values are advisory — a skill reporting "completed" while emitting
// error-severity diagnostics is a fail.
func deriveStatus(data map[string]any, diags []Diagnostic, err error) string {
	if err != nil {
		return "error"
	}
	if s, ok := data["status"].(string); ok && s == "error" {
		return "error"
	}
	for _, d := range diags {
		if d.Severity == "error" {
			return "fail"
		}
	}
	// Gates that report a boolean verdict instead of diagnostics.
	if reg, ok := data["performance_regression"].(bool); ok && reg {
		return "fail"
	}
	return "pass"
}

// RegisterDefaults registers all built-in Tool Skills.
func (rt *Runtime) RegisterDefaults(store *contract.Store) {
	rt.registry.Register(NewGoContractBuilder(store))
	rt.registry.Register(NewGoStaticCheck(rt))
	rt.registry.Register(NewGoRepairDiagnosis(store))
	rt.registry.Register(NewGoImportResolution(rt))
	rt.registry.Register(NewRepoQualityGate(rt))
	rt.registry.Register(NewGoFormat(rt))
	rt.registry.Register(NewSecurityScan())
	rt.registry.Register(NewBenchmarkGate())
	rt.registry.Register(NewPropertyTest())
}
