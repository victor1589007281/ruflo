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

// NewGoStaticCheck 构造静态检查 Tool Skill (只读、可并发)。
//
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

// Execute 跑一次静态检查: 优先用 static-check 适配器, 拿不到结果时退化成 go build。
//
// 算法解读：
//
//	原理：两级降级 —— 先试"最好的工具"(staticcheck / golangci-lint / go vet, 由
//		Runtime 按环境探测后绑到同一个 "static-check" 键上), 只有当它**没产出结果**
//		时才退回 "build" 适配器。降级判据是 result == nil 而非 err != nil。
//	关键不变量：diagnostics 的过滤在**降级之后**才做, 且 strict=false 时只留
//		Severity=="error" —— 也就是说非严格模式下 warning 被静默丢弃, 而
//		warning_count 统计的是**过滤后**的集合, 恒为 0。要拿真实 warning 数必须开 strict。
//	代价与边界：adapters 里没有 "static-check" 时直接跳过第一级; 两级都拿不到结果
//		才返回错误。注意 "build" 那次查表**没有用逗号-ok 形式**, 缺失时会得到 nil
//		接口并在此处 panic (实际不可达, 见 runtime.go 构造时必定写入 "build")。
//	设计权衡：不做工具探测的缓存 —— 探测开销由 Runtime 初始化时承担一次, 这里只查表。
//
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

// countBySeverity 统计给定严重级别的诊断条数 (精确匹配 severity 字符串)。
func countBySeverity(d []Diagnostic, sev string) int {
	c := 0
	for _, diag := range d {
		if diag.Severity == sev {
			c++
		}
	}
	return c
}
