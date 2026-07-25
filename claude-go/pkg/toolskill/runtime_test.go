package toolskill

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// stubSkill lets a test dictate exactly what a skill hands back.
type stubSkill struct {
	name string
	data map[string]any
	err  error
}

func (s *stubSkill) Name() string        { return s.name }
func (s *stubSkill) Description() string { return "stub" }
func (s *stubSkill) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (s *stubSkill) OutputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (s *stubSkill) IsReadOnly() bool        { return true }
func (s *stubSkill) IsConcurrencySafe() bool { return true }
func (s *stubSkill) Execute(context.Context, json.RawMessage) (map[string]any, error) {
	return s.data, s.err
}

func runStub(t *testing.T, data map[string]any, err error) ExecutionResult {
	t.Helper()
	reg := NewRegistry()
	reg.Register(&stubSkill{name: "stub", data: data, err: err})
	rt := NewRuntime(reg)
	return rt.Execute(context.Background(), "stub", json.RawMessage(`{}`))
}

// 回归：Execute 曾只设 Success/Data/Error/Timing，从不设 Status 与 Diagnostics。
// 消费方（validation gate）读的正是这两个字段，于是"干净的代码"与"跑挂了"
// 无法区分——门禁永远报不出 pass。
func TestExecute_设置Status与Diagnostics(t *testing.T) {
	res := runStub(t, map[string]any{"diagnostics": []Diagnostic{}}, nil)
	if res.Status != "pass" {
		t.Errorf("无诊断应为 pass, 得 %q", res.Status)
	}
	if !res.Success {
		t.Error("无错误应 Success=true")
	}
}

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		err  error
		want string
	}{
		{"skill报错→error", nil, errors.New("boom"), "error"},
		{"无诊断→pass", map[string]any{}, nil, "pass"},
		{"仅warning→pass", map[string]any{
			"diagnostics": []Diagnostic{{Severity: "warning", Message: "w"}},
		}, nil, "pass"},
		{"含error严重度→fail", map[string]any{
			"diagnostics": []Diagnostic{{Severity: "error", Message: "e"}},
		}, nil, "fail"},
		{"warning与error混合→fail", map[string]any{
			"diagnostics": []Diagnostic{{Severity: "warning"}, {Severity: "error"}},
		}, nil, "fail"},
		{"skill自报status=error→error", map[string]any{"status": "error"}, nil, "error"},
		// benchmark_gate 报 completed 但有性能回归：布尔判决也要变 fail
		{"性能回归→fail", map[string]any{
			"status": "completed", "performance_regression": true,
		}, nil, "fail"},
		{"无回归→pass", map[string]any{
			"status": "completed", "performance_regression": false,
		}, nil, "pass"},
		// skill 自报的非 error 值只是建议：有 error 诊断仍应 fail
		{"自报completed但有error诊断→fail", map[string]any{
			"status":      "completed",
			"diagnostics": []Diagnostic{{Severity: "error"}},
		}, nil, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runStub(t, tc.data, tc.err).Status; got != tc.want {
				t.Errorf("Status = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestHoistDiagnostics_类型化切片(t *testing.T) {
	res := runStub(t, map[string]any{
		"diagnostics": []Diagnostic{
			{File: "a.go", Line: 3, Severity: "error", Message: "bad", Tool: "staticcheck"},
			{File: "b.go", Line: 9, Severity: "warning", Message: "meh"},
		},
	}, nil)
	if len(res.Diagnostics) != 2 {
		t.Fatalf("诊断数 = %d, 期望 2", len(res.Diagnostics))
	}
	if res.Diagnostics[0].File != "a.go" || res.Diagnostics[0].Line != 3 {
		t.Errorf("首条诊断错: %+v", res.Diagnostics[0])
	}
	if res.Status != "fail" {
		t.Errorf("含 error 诊断应 fail, 得 %q", res.Status)
	}
}

// JSON 往返（缓存结果 / 跨进程 worker）会把 []Diagnostic 解成
// []any of map[string]any。只处理类型化分支会静默丢掉全部诊断。
func TestHoistDiagnostics_JSON往返(t *testing.T) {
	orig := map[string]any{"diagnostics": []Diagnostic{
		{File: "x.go", Line: 7, Column: 2, Severity: "error", Message: "type error",
			Category: "type_error", Code: "SA4000", Tool: "staticcheck", Fixable: true},
	}}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, isAny := decoded["diagnostics"].([]any); !isAny {
		t.Fatal("前置条件不成立: 往返后应为 []any")
	}

	res := runStub(t, decoded, nil)
	if len(res.Diagnostics) != 1 {
		t.Fatalf("往返后诊断数 = %d, 期望 1 (说明 []any 分支没生效)", len(res.Diagnostics))
	}
	d := res.Diagnostics[0]
	if d.File != "x.go" || d.Line != 7 || d.Column != 2 || d.Severity != "error" ||
		d.Code != "SA4000" || d.Tool != "staticcheck" || !d.Fixable {
		t.Errorf("往返后字段丢失: %+v", d)
	}
	if res.Status != "fail" {
		t.Errorf("往返后仍应 fail, 得 %q", res.Status)
	}
}

func TestHoistDiagnostics_缺失与异常形态(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
	}{
		{"无 diagnostics 键", map[string]any{"tool_used": "go build"}},
		{"显式 nil", map[string]any{"diagnostics": nil}},
		{"类型不对(字符串)", map[string]any{"diagnostics": "oops"}},
		{"元素不是对象", map[string]any{"diagnostics": []any{"a", 1}}},
		{"data 为 nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runStub(t, tc.data, nil)
			if len(res.Diagnostics) != 0 {
				t.Errorf("应无诊断, 得 %d 条", len(res.Diagnostics))
			}
			if res.Status != "pass" {
				t.Errorf("无可解析诊断应 pass, 得 %q", res.Status)
			}
		})
	}
}

func TestExecute_未注册的skill(t *testing.T) {
	rt := NewRuntime(NewRegistry())
	res := rt.Execute(context.Background(), "nope", json.RawMessage(`{}`))
	if res.Success {
		t.Error("未注册的 skill 不应 Success")
	}
	if res.Status != "error" {
		t.Errorf("Status = %q, 期望 error", res.Status)
	}
	if res.Error == "" {
		t.Error("应带错误信息")
	}
}

func TestExecute_记录耗时(t *testing.T) {
	res := runStub(t, map[string]any{}, nil)
	if res.Timing.Start.IsZero() || res.Timing.End.IsZero() {
		t.Error("Timing 起止时间未设置")
	}
	if res.Timing.End.Before(res.Timing.Start) {
		t.Error("End 早于 Start")
	}
}
