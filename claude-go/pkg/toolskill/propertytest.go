// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
)

// PropertyTest generates property-based tests for pure functions.
// For now this is a skeleton; full LLM-driven property generation can be added later.
type PropertyTest struct{ Base }

// PropertyCase represents a generated property test case.
type PropertyCase struct {
	Name      string `json:"name"`
	Function  string `json:"function"`
	Property  string `json:"property"`   // e.g., "round-trip", "idempotent", "sort-preserves-length"
	InputGen  string `json:"input_gen"`  // Go code to generate inputs
	CheckCode string `json:"check_code"` // Go code to verify property
}

// NewPropertyTest 创建 property-test 技能 (只读、可并发, 入参 repo_root + function)。
// NewPropertyTest creates a new PropertyTest skill.
func NewPropertyTest() *PropertyTest {
	return &PropertyTest{
		Base: NewBase(Meta{
			Name:           "property-test",
			Description:    "Generate property-based tests for pure functions. Skeleton implementation — LLM-driven generation to be added later.",
			Version:        "0.1.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"function":  {"type": "string"},
					"package":   {"type": "string"}
				},
				"required": ["repo_root", "function"]
			}`),
		}),
	}
}

// Execute 解析入参并返回空的属性测试用例列表。
//
// 注意：这是**桩实现** —— 它只回显 repo_root/function/package 并把 cases 固定为空,
// 并不真的生成属性测试 (也不校验函数是否存在)。method 字段回 "skeleton" 就是为了让
// 消费方能分辨"没生成"与"生成了 0 条", 不要把它当成已完成的能力。
// Execute runs the property test generator (skeleton).
func (p *PropertyTest) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string `json:"repo_root"`
		Function string `json:"function"`
		Package  string `json:"package"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	// Skeleton: return empty result with the structure in place
	return map[string]any{
		"status":     "completed",
		"method":     "skeleton",
		"cases":      []PropertyCase{},
		"repo_root":  in.RepoRoot,
		"function":   in.Function,
		"package":    in.Package,
	}, nil
}
