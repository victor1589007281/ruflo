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

// NewRuntime 构造 Tool Skill 运行时: 持有注册表, 并新建工具探测器与一张空的适配器表。
// NewRuntime creates a new Runtime.
func NewRuntime(reg *Registry) *Runtime {
	return &Runtime{
		registry: reg,
		detector: NewToolDetector(),
		adapters: make(map[string]Adapter),
	}
}

// Init 扫描本机可用工具并注册默认适配器; 同一能力后注册者覆盖先注册者, build/format/import-fix 恒有兜底实现。
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
	// 静态检查适配器按探测结果二选一; 两者都没探到时 static-check 能力缺位 (build/format/import-fix 仍可用)。
	// Always register go vet / go build fallback
	rt.adapters["build"] = &GoBuildAdapter{}
	rt.adapters["format"] = &GoFormatAdapter{detector: rt.detector}
	rt.adapters["import-fix"] = &GoImportsAdapter{detector: rt.detector}

	log.Printf("[ToolSkill] Initialized adapters: %v", rt.AdapterNames())
}

// AdapterNames 返回已初始化的适配器能力名 (map 序, 无序)。
// AdapterNames returns all initialized adapter capability names.
func (rt *Runtime) AdapterNames() []string {
	var names []string
	for k := range rt.adapters {
		names = append(names, k)
	}
	return names
}

// Execute 按名执行一个 Tool Skill, 并把 Status/Diagnostics 一并填进 ExecutionResult。
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

// hoistDiagnostics 把 Data["diagnostics"] 提升到结果顶层; 同时兼容进程内直传的类型化
// []Diagnostic 与 JSON 往返后解出的 []any。
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

// deriveStatus 把技能的自由产出收敛成闸门能读的三值 (pass / fail / error)。
//
// 算法解读：
//
//	原理：技能返回的是 (map, error) 二元组, 而闸门要的是"过没过"; 这一层负责把
//	      前者翻译成后者, 避免每个消费方各自解释一遍。
//	关键不变量：优先级固定为 执行错误 > 技能自报 error > 存在 error 级诊断。技能自报的
//	      **非 error** 状态一律不作数 —— 一个报 "completed" 却吐出 error 级诊断的技能
//	      必须判 fail, 否则闸门会被技能的自评绕过。
//	代价与边界：只认 Severity == "error", warning 级诊断不拦。布尔型结论
//	      (performance_regression) 单独兜一条, 因为这类技能不产 diagnostics。
//	设计权衡：状态词表刻意只有三个 —— 多一个档位就要多一处"谁来判"的约定, 而这三档
//	      恰好对应调用方三种完全不同的后续动作 (重试 / 修代码 / 放行)。
//
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

// RegisterDefaults 注册全部内置 Tool Skill。
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
