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
func (rt *Runtime) Execute(ctx context.Context, name string, input json.RawMessage) ExecutionResult {
	ts, ok := rt.registry.Get(name)
	if !ok {
		return WrapError(fmt.Errorf("tool skill %q not found", name))
	}
	start := time.Now()
	data, err := ts.Execute(ctx, input)
	return ExecutionResult{
		Success: err == nil,
		Data:    data,
		Error:   errStr(err),
		Timing:  TimingInfo{Start: start, End: time.Now(), DurationMs: time.Since(start).Milliseconds()},
	}
}

// RegisterDefaults registers all built-in Tool Skills.
func (rt *Runtime) RegisterDefaults(store *contract.Store) {
	rt.registry.Register(NewGoContractBuilder(store))
	rt.registry.Register(NewGoStaticCheck(rt))
	rt.registry.Register(NewGoRepairDiagnosis(store))
	rt.registry.Register(NewGoImportResolution(rt))
	rt.registry.Register(NewRepoQualityGate(rt))
	rt.registry.Register(NewGoFormat(rt))
}
