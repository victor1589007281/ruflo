package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件钉住 R-H3 的验收点：未知 content block / 未知 metadata 往返后字节不变，
// 同时既有序列化行为（空 thinking、tool_result.content 数组、Go 构造块）一字不变。

func roundtrip(t *testing.T, in string) (ContentBlock, []byte) {
	t.Helper()
	var cb ContentBlock
	if err := json.Unmarshal([]byte(in), &cb); err != nil {
		t.Fatalf("unmarshal %s: %v", in, err)
	}
	out, err := json.Marshal(cb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return cb, out
}

// 未知块类型 + 未知 metadata：整块逐字节往返。
func TestContentBlockRoundtrip_UnknownBlockType(t *testing.T) {
	cases := []string{
		`{"type":"server_tool_use","id":"toolu_1","name":"web_search","input":{"q":"go"},"cache_control":{"type":"ephemeral"}}`,
		`{"type":"future_block_type","id":"b1","payload":{"nested":[1,2,3]},"meta":"m"}`,
		`{"type":"future_block_type","custom":{"deep":{"er":"value"}}}`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, out := roundtrip(t, in)
			if string(out) != in {
				t.Fatalf("未知块往返后字节变了:\n in=%s\nout=%s", in, out)
			}
		})
	}
}

// 已知块未修改：往返逐字节相等（raw 快照生效，未知 metadata 不丢）。
func TestContentBlockRoundtrip_KnownBlockUnmodified(t *testing.T) {
	cases := []string{
		`{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}`,
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/x"},"cache_control":{"type":"ephemeral"}}`,
		`{"type":"thinking","thinking":"推理中","signature":"sig-abc"}`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, out := roundtrip(t, in)
			if string(out) != in {
				t.Fatalf("已知块未修改却改变了字节:\n in=%s\nout=%s", in, out)
			}
		})
	}
}

// MarshalJSON 本身必须返回原始字节（json.Marshal 外层还会 compact，
// 这里直接调方法，确认连空白也原样保留）。
func TestContentBlockRoundtrip_MarshalJSONReturnsRaw(t *testing.T) {
	in := `{ "type" : "future_block_type" , "x" : 1 }`
	var cb ContentBlock
	if err := json.Unmarshal([]byte(in), &cb); err != nil {
		t.Fatal(err)
	}
	out, err := cb.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("MarshalJSON 未返回原始字节:\n in=%s\nout=%s", in, out)
	}
}

// 修改已知字段后：新值优先，未知 metadata 仍在。
func TestContentBlockRoundtrip_ModifiedKeepsUnknown(t *testing.T) {
	in := `{"type":"text","text":"original","cache_control":{"type":"ephemeral"}}`
	cb, _ := roundtrip(t, in)
	cb.Text = "changed"
	out, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON: %s", out)
	}
	var text string
	if err := json.Unmarshal(m["text"], &text); err != nil || text != "changed" {
		t.Fatalf("修改后的 Text 未生效: %s", out)
	}
	if _, ok := m["cache_control"]; !ok {
		t.Fatalf("修改已知字段后未知 metadata 被丢弃: %s", out)
	}
	if strings.Contains(string(out), "original") {
		t.Fatalf("仍然输出了旧值: %s", out)
	}
}

// 修改 thinking 后：新值优先，signature（未知字段）仍在。
func TestContentBlockRoundtrip_ModifiedThinkingKeepsSignature(t *testing.T) {
	in := `{"type":"thinking","thinking":"old","signature":"sig-abc"}`
	cb, _ := roundtrip(t, in)
	cb.Thinking = "new"
	out, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON: %s", out)
	}
	if m["thinking"] != "new" {
		t.Fatalf("thinking 新值未生效: %s", out)
	}
	if m["signature"] != "sig-abc" {
		t.Fatalf("signature 未保留: %s", out)
	}
}

// 未知块类型修改已知字段后：新值优先，未知字段仍在。
func TestContentBlockRoundtrip_ModifiedUnknownTypeKeepsFields(t *testing.T) {
	in := `{"type":"server_tool_use","id":"x","name":"old","custom":{"k":1}}`
	cb, _ := roundtrip(t, in)
	cb.Name = "web_search"
	out, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON: %s", out)
	}
	var name string
	_ = json.Unmarshal(m["name"], &name)
	if name != "web_search" {
		t.Fatalf("name 新值未生效: %s", out)
	}
	if _, ok := m["custom"]; !ok {
		t.Fatalf("未知字段 custom 被丢弃: %s", out)
	}
}

// 空 thinking 特例：Go 构造（无 raw）必须输出 {"type":"thinking","thinking":""}。
func TestContentBlockMarshal_EmptyThinking(t *testing.T) {
	out, err := json.Marshal(ContentBlock{Type: ContentBlockThinking})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"type":"thinking","thinking":""}` {
		t.Fatalf("空 thinking 输出变了: %s", out)
	}
	// 带非空 thinking 也保持历史形状（仅 type + thinking）。
	out, err = json.Marshal(ContentBlock{Type: ContentBlockThinking, Thinking: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"type":"thinking","thinking":"hi"}` {
		t.Fatalf("thinking 输出变了: %s", out)
	}
}

// tool_result.content 数组特例：未修改往返逐字节保持数组形态；
// 同时确认展平语义（Content = "a\nb"）仍然生效。
func TestContentBlockRoundtrip_ToolResultContentArray(t *testing.T) {
	in := `{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`
	cb, out := roundtrip(t, in)
	if cb.Content != "a\nb" {
		t.Fatalf("tool_result.content 展平语义变了: %q", cb.Content)
	}
	if string(out) != in {
		t.Fatalf("未修改的数组形态往返后字节变了:\n in=%s\nout=%s", in, out)
	}
}

// Go 构造的已知块（无 raw）输出必须与历史逐字节相同。
func TestContentBlockMarshal_GoConstructedUnchanged(t *testing.T) {
	cases := []struct {
		name string
		in   ContentBlock
		want string
	}{
		{
			name: "text",
			in:   ContentBlock{Type: ContentBlockText, Text: "hi"},
			want: `{"type":"text","text":"hi"}`,
		},
		{
			name: "tool_use",
			in:   ContentBlock{Type: ContentBlockToolUse, ID: "t", Name: "Read", Input: json.RawMessage(`{"a":1}`)},
			want: `{"type":"tool_use","id":"t","name":"Read","input":{"a":1}}`,
		},
		{
			name: "tool_result",
			in:   ContentBlock{Type: ContentBlockToolResult, ToolUseID: "t", Content: "ok", IsError: true},
			want: `{"type":"tool_result","tool_use_id":"t","content":"ok","is_error":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("Go 构造块输出变了:\n want=%s\n  got=%s", tc.want, out)
			}
		})
	}
}
