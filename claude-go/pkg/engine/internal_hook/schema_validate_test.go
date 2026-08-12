package internal_hook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

const testSchema = `{
  "type": "object",
  "properties": {
    "command": {"type": "string"},
    "timeout": {"type": "integer"},
    "tags": {"type": "array", "items": {"type": "string"}},
    "mode": {"type": "string", "enum": ["fast", "slow"]},
    "opts": {"type": "object", "properties": {"force": {"type": "boolean"}}, "required": ["force"]}
  },
  "required": ["command"]
}`

func TestValidateAgainstSchema(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string // 空 = 应通过
	}{
		{"合法全字段", `{"command":"ls","timeout":5,"tags":["a"],"mode":"fast","opts":{"force":true}}`, ""},
		{"合法仅必填", `{"command":"ls"}`, ""},
		{"缺必填", `{"timeout":5}`, "command"},
		{"类型错 string", `{"command":123}`, "类型错误"},
		{"类型错 integer 带小数", `{"command":"ls","timeout":1.5}`, "类型错误"},
		{"integer 合法整数值", `{"command":"ls","timeout":3}`, ""},
		{"enum 越界", `{"command":"ls","mode":"turbo"}`, "enum"},
		{"enum 命中", `{"command":"ls","mode":"slow"}`, ""},
		{"数组元素类型错", `{"command":"ls","tags":["a",2]}`, "tags[1]"},
		{"嵌套对象缺必填", `{"command":"ls","opts":{}}`, "force"},
		{"未知额外字段放行", `{"command":"ls","whatever":1}`, ""},
		{"非法 JSON", `{bad`, "不是合法 JSON"},
	}
	for _, c := range cases {
		err := ValidateAgainstSchema(json.RawMessage(c.input), json.RawMessage(testSchema))
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: 应通过, 得到 %v", c.name, err)
			}
		} else {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: 期望含 %q 的错误, 得到 %v", c.name, c.wantErr, err)
			}
		}
	}
	// schema 不可解析 → 放行 (容错)
	if err := ValidateAgainstSchema(json.RawMessage(`{"a":1}`), json.RawMessage(`{bad`)); err != nil {
		t.Errorf("坏 schema 应放行, 得到 %v", err)
	}
	// 空 schema → 放行
	if err := ValidateAgainstSchema(json.RawMessage(`{"a":1}`), nil); err != nil {
		t.Errorf("空 schema 应放行, 得到 %v", err)
	}
}

func schemaTestBlocks() []types.ContentBlock {
	return []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "t1", Name: "Shell", Input: json.RawMessage(`{"command":"ls","timeout":"abc"}`)}, // 类型错 → 拦
		{Type: types.ContentBlockToolUse, ID: "t2", Name: "Shell", Input: json.RawMessage(`{"command":"ls"}`)}, // 合法 → 过
		{Type: types.ContentBlockToolUse, ID: "t3", Name: "NoSchema", Input: json.RawMessage(`{"x":1}`)},       // 无 schema → 过
	}
}

func TestSchemaValidateHookBlocksBadCalls(t *testing.T) {
	schemas := map[string]json.RawMessage{"Shell": json.RawMessage(testSchema)}
	h := NewSchemaValidateHook(schemas, nil, 2)
	if h == nil {
		t.Fatal("hook 不应为 nil")
	}
	if h.Priority() != 75 {
		t.Errorf("优先级应为 75, 得 %d", h.Priority())
	}

	res, err := h.Execute(&HookContext{ToolUseBlocks: schemaTestBlocks()})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("应有拦截结果")
	}
	if len(res.ToolUseBlocks) != 2 { // t1 被拦, t2/t3 放行
		t.Errorf("放行块数应为 2, 得 %d", len(res.ToolUseBlocks))
	}
	if len(res.AppendMsgs) != 1 {
		t.Fatalf("应回注 1 条错误结果, 得 %d", len(res.AppendMsgs))
	}
	msg := res.AppendMsgs[0]
	tr := msg.Content[0]
	if !tr.IsError || tr.ToolUseID != "t1" {
		t.Errorf("回注应为 t1 的错误 tool_result, 得 %+v", tr)
	}
	if !strings.Contains(tr.Content, "schema_validate") || !strings.Contains(tr.Content, "类型错误") {
		t.Errorf("回注内容应含错误详情, 得 %s", tr.Content)
	}
	if !strings.Contains(tr.Content, "command*") {
		t.Errorf("回注应含参数形状提示, 得 %s", tr.Content)
	}
	if res.InjectContinue {
		t.Error("仍有放行块时不应 InjectContinue")
	}
}

func TestSchemaValidateHookRetryThenTerminal(t *testing.T) {
	schemas := map[string]json.RawMessage{"Shell": json.RawMessage(testSchema)}
	h := NewSchemaValidateHook(schemas, nil, 2)
	bad := []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "x1", Name: "Shell", Input: json.RawMessage(`{"timeout":"abc"}`)},
	}
	// 第 1、2 次: 重填提示; 第 3 次同签名: 终止
	for i, want := range []string{"重新调用", "重新调用", "被终止"} {
		res, _ := h.Execute(&HookContext{ToolUseBlocks: bad})
		if res == nil || len(res.AppendMsgs) != 1 {
			t.Fatalf("第 %d 次应有回注", i+1)
		}
		got := res.AppendMsgs[0].Content[0].Content
		if !strings.Contains(got, want) {
			t.Errorf("第 %d 次应含 %q, 得 %s", i+1, want, got)
		}
		if len(res.ToolUseBlocks) != 0 || !res.InjectContinue {
			t.Errorf("第 %d 次全拦应 InjectContinue", i+1)
		}
	}
}

func TestSchemaValidateHookAllPassReturnsNil(t *testing.T) {
	schemas := map[string]json.RawMessage{"Shell": json.RawMessage(testSchema)}
	h := NewSchemaValidateHook(schemas, nil, 2)
	good := []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "g1", Name: "Shell", Input: json.RawMessage(`{"command":"ls"}`)},
	}
	res, err := h.Execute(&HookContext{ToolUseBlocks: good})
	if err != nil || res != nil {
		t.Errorf("全部合法时应返回 nil 结果, 得 res=%v err=%v", res, err)
	}
}

func TestNewSchemaValidateHookGuards(t *testing.T) {
	if h := NewSchemaValidateHook(nil, nil, 2); h != nil {
		t.Error("空 schemas 应返回 nil")
	}
	if h := NewSchemaValidateHook(map[string]json.RawMessage{"a": nil}, nil, 0); h != nil {
		t.Error("maxRetries<1 应返回 nil")
	}
}
