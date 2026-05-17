package engine

import "github.com/anthropic/claude-go/pkg/engine/internal_hook"

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestExtractXMLToolCalls_MiniMaxWrapperMultipleInvokes(t *testing.T) {
	text := `I'll start by reading the design documents.
<minimax:tool_call>
<invoke name="Read">
<parameter name="file_path">/home/foo/a.md</parameter>
</invoke>
<invoke name="Read">
<parameter name="file_path">/home/foo/b.md</parameter>
</invoke>
</minimax:tool_call>`

	blocks, cleaned := internal_hook.ExtractXMLToolCalls(text, "t0")
	if len(blocks) != 2 {
		t.Fatalf("expected 2 tool_use blocks, got %d", len(blocks))
	}
	if !strings.Contains(cleaned, "reading the design documents") {
		t.Fatalf("expected preface text to survive, got %q", cleaned)
	}
	if strings.Contains(cleaned, "minimax:tool_call") || strings.Contains(cleaned, "<invoke") {
		t.Fatalf("expected XML to be stripped, got %q", cleaned)
	}
	for i, blk := range blocks {
		if blk.Type != types.ContentBlockToolUse {
			t.Fatalf("block %d: expected tool_use, got %v", i, blk.Type)
		}
		if blk.Name != "Read" {
			t.Fatalf("block %d: expected name=Read, got %q", i, blk.Name)
		}
		if blk.ID == "" {
			t.Fatalf("block %d: expected non-empty ID", i)
		}
		var in map[string]any
		if err := json.Unmarshal(blk.Input, &in); err != nil {
			t.Fatalf("block %d: input not valid JSON: %v", i, err)
		}
		fp, _ := in["path"].(string)
		if !strings.HasPrefix(fp, "/home/foo/") {
			t.Fatalf("block %d: expected path /home/foo/* (aliased from file_path), got %q", i, fp)
		}
		if _, hasOld := in["file_path"]; hasOld {
			t.Fatalf("block %d: file_path should be aliased away", i)
		}
	}
}

func TestExtractXMLToolCalls_NameAliases(t *testing.T) {
	text := `<minimax:tool_call>
<invoke name="Edit">
<parameter name="file_path">/x.go</parameter>
<parameter name="old_string">foo</parameter>
<parameter name="new_string">bar</parameter>
</invoke>
<invoke name="Bash">
<parameter name="command">go build ./...</parameter>
</invoke>
</minimax:tool_call>`

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "x")
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Name != "StrReplace" {
		t.Fatalf("expected Edit aliased to StrReplace, got %q", blocks[0].Name)
	}
	if blocks[1].Name != "Shell" {
		t.Fatalf("expected Bash aliased to Shell, got %q", blocks[1].Name)
	}

	var in0, in1 map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in0)
	_ = json.Unmarshal(blocks[1].Input, &in1)
	if in0["old_string"] != "foo" || in0["new_string"] != "bar" || in0["path"] != "/x.go" {
		t.Fatalf("StrReplace input wrong: %v", in0)
	}
	if _, hasOld := in0["file_path"]; hasOld {
		t.Fatalf("StrReplace input should not retain file_path: %v", in0)
	}
	if in1["command"] != "go build ./..." {
		t.Fatalf("Shell input wrong: %v", in1)
	}
}

func TestExtractXMLToolCalls_NoXML(t *testing.T) {
	blocks, cleaned := internal_hook.ExtractXMLToolCalls("just a plain answer", "p")
	if len(blocks) != 0 {
		t.Fatalf("expected no blocks, got %d", len(blocks))
	}
	if cleaned != "just a plain answer" {
		t.Fatalf("expected text unchanged, got %q", cleaned)
	}
}

func TestExtractXMLToolCalls_FunctionCallsWrapper(t *testing.T) {
	text := `<function_calls>
<invoke name="Write">
<parameter name="file_path">/tmp/out.txt</parameter>
<parameter name="content">hello world</parameter>
</invoke>
</function_calls>`
	blocks, cleaned := internal_hook.ExtractXMLToolCalls(text, "fc")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Name != "Write" {
		t.Fatalf("expected Write, got %q", blocks[0].Name)
	}
	if cleaned != "" {
		t.Fatalf("expected cleaned to be empty after stripping, got %q", cleaned)
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if in["contents"] != "hello world" {
		t.Fatalf("contents wrong (expected alias content -> contents): %v", in)
	}
	if in["path"] != "/tmp/out.txt" {
		t.Fatalf("path wrong (expected alias file_path -> path): %v", in)
	}
}

func TestExtractXMLToolCalls_BareInvoke(t *testing.T) {
	text := `Let me check:
<invoke name="Read">
<parameter name="file_path">/etc/hosts</parameter>
</invoke>
done.`
	blocks, cleaned := internal_hook.ExtractXMLToolCalls(text, "b")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d (text=%q)", len(blocks), text)
	}
	if !strings.Contains(cleaned, "Let me check") {
		t.Fatalf("expected preface to remain: %q", cleaned)
	}
	if strings.Contains(cleaned, "<invoke") {
		t.Fatalf("expected invoke stripped: %q", cleaned)
	}
}

func TestExtractXMLToolCalls_BooleanCoercion(t *testing.T) {
	text := `<minimax:tool_call>
<invoke name="WebFetch">
<parameter name="url">https://example.com</parameter>
<parameter name="follow_redirects">true</parameter>
</invoke>
</minimax:tool_call>`
	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "bc")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block")
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if b, ok := in["follow_redirects"].(bool); !ok || !b {
		t.Fatalf("expected boolean true, got %v (%T)", in["follow_redirects"], in["follow_redirects"])
	}
}

func TestExtractXMLToolCalls_PreservesMultilineContent(t *testing.T) {
	text := "<minimax:tool_call>\n<invoke name=\"Write\">\n<parameter name=\"file_path\">/tmp/x.go</parameter>\n<parameter name=\"content\">package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n</parameter>\n</invoke>\n</minimax:tool_call>"
	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "ml")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	c, _ := in["contents"].(string)
	if !strings.Contains(c, "package main") || !strings.Contains(c, "println(\"hi\")") {
		t.Fatalf("multiline contents lost: %q", c)
	}
	if strings.HasPrefix(c, "\n") || strings.HasSuffix(c, "\n") {
		t.Fatalf("expected exactly one leading/trailing newline trimmed: %q", c)
	}
}

func TestMergeXMLToolCalls_AppendsToolUseAndKeepsText(t *testing.T) {
	original := []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "preamble <minimax:tool_call>\n<invoke name=\"Read\"><parameter name=\"file_path\">/a</parameter></invoke>\n</minimax:tool_call> trailing"},
	}
	updated, tu, n := internal_hook.MergeXMLToolCalls(original, nil, "turn1")
	if n != 1 {
		t.Fatalf("expected 1 delta, got %d", n)
	}
	if len(tu) != 1 || tu[0].Name != "Read" {
		t.Fatalf("tool_use blocks wrong: %+v", tu)
	}
	if len(updated) != 2 {
		t.Fatalf("expected 2 blocks (text+tool_use), got %d", len(updated))
	}
	if updated[0].Type != types.ContentBlockText {
		t.Fatalf("first block should be text, got %v", updated[0].Type)
	}
	if !strings.Contains(updated[0].Text, "preamble") || !strings.Contains(updated[0].Text, "trailing") {
		t.Fatalf("expected text preserved: %q", updated[0].Text)
	}
	if updated[1].Type != types.ContentBlockToolUse {
		t.Fatalf("second block should be tool_use, got %v", updated[1].Type)
	}
}

func TestMergeXMLToolCalls_NoOpWhenNoXML(t *testing.T) {
	original := []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "all good"},
	}
	updated, tu, n := internal_hook.MergeXMLToolCalls(original, nil, "turn1")
	if n != 0 {
		t.Fatalf("expected n=0, got %d", n)
	}
	if len(tu) != 0 {
		t.Fatalf("expected empty tu, got %d", len(tu))
	}
	if len(updated) != 1 || updated[0].Text != "all good" {
		t.Fatalf("expected unchanged blocks, got %+v", updated)
	}
}

func TestMergeXMLToolCalls_DropsEmptyTextBlock(t *testing.T) {
	original := []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "<minimax:tool_call><invoke name=\"Read\"><parameter name=\"file_path\">/x</parameter></invoke></minimax:tool_call>"},
	}
	updated, tu, n := internal_hook.MergeXMLToolCalls(original, nil, "turn1")
	if n != 1 {
		t.Fatalf("expected 1 delta, got %d", n)
	}
	if len(tu) != 1 {
		t.Fatalf("expected 1 tu, got %d", len(tu))
	}
	if len(updated) != 1 {
		t.Fatalf("expected 1 block (tool_use only, text dropped), got %d: %+v", len(updated), updated)
	}
	if updated[0].Type != types.ContentBlockToolUse {
		t.Fatalf("expected tool_use, got %v", updated[0].Type)
	}
}

// Testinternal_hook.MergeXMLToolCalls_StripsWrapperWithoutInvoke 回归: 当模型只输出 <minimax:tool_call>
// 包裹但里面没有 <invoke> 子元素 (MiniMax 偶尔会这么干, 例如把 wrapper 留作
// 占位符), 我们应该: 提取出 0 个 tool_use, 但仍然把包裹标签从文本里抹掉,
// 这样上层 architect / researcher 的 "伪工具调用" 校验器不会把残留 XML 当成
// 真的工具调用.
func TestMergeXMLToolCalls_StripsWrapperWithoutInvoke(t *testing.T) {
	original := []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "前置说明.\n\n<minimax:tool_call>just narrative, no invoke</minimax:tool_call>\n\n后置正文."},
	}
	updated, tu, n := internal_hook.MergeXMLToolCalls(original, nil, "turn1")
	if n != 0 {
		t.Fatalf("expected 0 delta (no invoke inside), got %d", n)
	}
	if len(tu) != 0 {
		t.Fatalf("expected empty tu, got %d", len(tu))
	}
	if len(updated) != 1 {
		t.Fatalf("expected 1 block, got %d: %+v", len(updated), updated)
	}
	if strings.Contains(updated[0].Text, "minimax:tool_call") {
		t.Fatalf("wrapper should be stripped from cleaned text, got %q", updated[0].Text)
	}
	if !strings.Contains(updated[0].Text, "前置说明") || !strings.Contains(updated[0].Text, "后置正文") {
		t.Fatalf("surrounding text should be preserved: %q", updated[0].Text)
	}
}

func TestExtractXMLToolCalls_ParamAlias_ReadAndWrite(t *testing.T) {
	text := `<minimax:tool_call>
<invoke name="Read">
<parameter name="file_path">/abs/r.txt</parameter>
</invoke>
<invoke name="Write">
<parameter name="file_path">/abs/w.txt</parameter>
<parameter name="content">hello</parameter>
</invoke>
<invoke name="Edit">
<parameter name="file_path">/abs/e.txt</parameter>
<parameter name="old_string">foo</parameter>
<parameter name="new_string">bar</parameter>
</invoke>
</minimax:tool_call>`
	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "p")
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	var in0, in1, in2 map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in0)
	_ = json.Unmarshal(blocks[1].Input, &in1)
	_ = json.Unmarshal(blocks[2].Input, &in2)

	// Read: file_path -> path
	if blocks[0].Name != "Read" {
		t.Fatalf("[0] expected Read, got %q", blocks[0].Name)
	}
	if in0["path"] != "/abs/r.txt" || in0["file_path"] != nil {
		t.Fatalf("[0] expected path=/abs/r.txt and no file_path, got %v", in0)
	}

	// Write: file_path -> path, content -> contents
	if blocks[1].Name != "Write" {
		t.Fatalf("[1] expected Write, got %q", blocks[1].Name)
	}
	if in1["path"] != "/abs/w.txt" || in1["contents"] != "hello" {
		t.Fatalf("[1] expected path=/abs/w.txt and contents=hello, got %v", in1)
	}
	if in1["file_path"] != nil || in1["content"] != nil {
		t.Fatalf("[1] expected file_path/content removed, got %v", in1)
	}

	// Edit -> StrReplace, file_path -> path; old/new strings stay.
	if blocks[2].Name != "StrReplace" {
		t.Fatalf("[2] expected StrReplace, got %q", blocks[2].Name)
	}
	if in2["path"] != "/abs/e.txt" || in2["old_string"] != "foo" || in2["new_string"] != "bar" {
		t.Fatalf("[2] expected path/old_string/new_string normalized, got %v", in2)
	}
}

func TestExtractXMLToolCalls_ParamAlias_DoesNotClobber(t *testing.T) {
	text := `<minimax:tool_call>
<invoke name="Read">
<parameter name="path">/real/path</parameter>
<parameter name="file_path">/alias/path</parameter>
</invoke>
</minimax:tool_call>`
	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "p")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if in["path"] != "/real/path" {
		t.Fatalf("expected path=/real/path (real key wins), got %v", in)
	}
	if _, exists := in["file_path"]; exists {
		t.Fatalf("expected file_path removed, got %v", in)
	}
}

// ============================================================
// 方言 B: [TOOL_CALL] hash (MiniMax M2.x 训练语料)
// ============================================================

func TestExtractBracketToolCalls_SingleRead(t *testing.T) {
	text := "Let me first read the design doc.\n" +
		"[TOOL_CALL]\n" +
		"{tool => \"Read\", args => {\n" +
		"  --path \"/home/foo/design.md\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, cleaned := internal_hook.ExtractXMLToolCalls(text, "bk")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d (cleaned=%q)", len(blocks), cleaned)
	}
	if blocks[0].Type != types.ContentBlockToolUse {
		t.Fatalf("expected tool_use block, got %v", blocks[0].Type)
	}
	if blocks[0].Name != "Read" {
		t.Fatalf("expected Read, got %q", blocks[0].Name)
	}
	var in map[string]any
	if err := json.Unmarshal(blocks[0].Input, &in); err != nil {
		t.Fatalf("input not valid JSON: %v", err)
	}
	if in["path"] != "/home/foo/design.md" {
		t.Fatalf("expected path=/home/foo/design.md, got %v", in)
	}
	if !strings.Contains(cleaned, "read the design doc") {
		t.Fatalf("expected preface preserved, got %q", cleaned)
	}
	if strings.Contains(cleaned, "TOOL_CALL") {
		t.Fatalf("expected TOOL_CALL wrapper stripped, got %q", cleaned)
	}
}

func TestExtractBracketToolCalls_BashWithMultipleArgs(t *testing.T) {
	text := "[TOOL_CALL]\n" +
		"{tool => \"Bash\", args => {\n" +
		"  --description \"List Go files\"\n" +
		"  --command \"find . -name '*.go' | head -50\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "bk")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	// Bash should be aliased to Shell
	if blocks[0].Name != "Shell" {
		t.Fatalf("expected Bash aliased to Shell, got %q", blocks[0].Name)
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if in["description"] != "List Go files" {
		t.Fatalf("expected description=\"List Go files\", got %v", in["description"])
	}
	if cmd, _ := in["command"].(string); !strings.Contains(cmd, "find") || !strings.Contains(cmd, "head -50") {
		t.Fatalf("expected command preserved, got %v", in["command"])
	}
}

func TestExtractBracketToolCalls_EscapedQuotes(t *testing.T) {
	// The args.command value contains escaped \" which must survive JSON decoding.
	text := "[TOOL_CALL]\n" +
		"{tool => \"Bash\", args => {\n" +
		"  --command \"find /tmp -type f -name \\\"*.go\\\" 2>/dev/null\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "bk")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	cmd, _ := in["command"].(string)
	if !strings.Contains(cmd, `"*.go"`) {
		t.Fatalf("expected escaped quotes preserved as literal \" in command, got %q", cmd)
	}
}

func TestExtractBracketToolCalls_MultipleCalls(t *testing.T) {
	text := "step1\n" +
		"[TOOL_CALL]\n" +
		"{tool => \"Read\", args => {\n" +
		"  --path \"/a\"\n" +
		"}}\n" +
		"[/TOOL_CALL]\n" +
		"step2\n" +
		"[TOOL_CALL]\n" +
		"{tool => \"Read\", args => {\n" +
		"  --path \"/b\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, cleaned := internal_hook.ExtractXMLToolCalls(text, "bk")
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d (cleaned=%q)", len(blocks), cleaned)
	}
	var in0, in1 map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in0)
	_ = json.Unmarshal(blocks[1].Input, &in1)
	if in0["path"] != "/a" || in1["path"] != "/b" {
		t.Fatalf("expected /a and /b, got %v / %v", in0, in1)
	}
	if !strings.Contains(cleaned, "step1") || !strings.Contains(cleaned, "step2") {
		t.Fatalf("expected narrative text preserved, got %q", cleaned)
	}
}

func TestExtractBracketToolCalls_WriteWithMultilineContent(t *testing.T) {
	text := "[TOOL_CALL]\n" +
		"{tool => \"Write\", args => {\n" +
		"  --path \"/tmp/hello.go\"\n" +
		"  --content \"package main\\n\\nfunc main() {\\n\\tprintln(\\\"hi\\\")\\n}\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "bk")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Name != "Write" {
		t.Fatalf("expected Write, got %q", blocks[0].Name)
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	// content -> contents alias
	contents, _ := in["contents"].(string)
	if !strings.Contains(contents, "package main") {
		t.Fatalf("expected contents to include 'package main', got %q", contents)
	}
	if !strings.Contains(contents, "println(\"hi\")") {
		t.Fatalf("expected contents to preserve escaped quotes, got %q", contents)
	}
}

func TestExtractBracketToolCalls_NameAlias_EditToStrReplace(t *testing.T) {
	text := "[TOOL_CALL]\n" +
		"{tool => \"Edit\", args => {\n" +
		"  --path \"/x.go\"\n" +
		"  --old_string \"foo\"\n" +
		"  --new_string \"bar\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "bk")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Name != "StrReplace" {
		t.Fatalf("expected Edit aliased to StrReplace, got %q", blocks[0].Name)
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if in["path"] != "/x.go" || in["old_string"] != "foo" || in["new_string"] != "bar" {
		t.Fatalf("StrReplace input wrong: %v", in)
	}
}

func TestExtractBracketToolCalls_MixedWithXML(t *testing.T) {
	// Same response uses both dialects (defensive against models switching mid-stream).
	text := "<function_calls>\n" +
		"<invoke name=\"Read\">\n" +
		"<parameter name=\"file_path\">/from/xml</parameter>\n" +
		"</invoke>\n" +
		"</function_calls>\n" +
		"and then\n" +
		"[TOOL_CALL]\n" +
		"{tool => \"Read\", args => {\n" +
		"  --path \"/from/bracket\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "mix")
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	paths := []string{}
	for _, b := range blocks {
		var in map[string]any
		_ = json.Unmarshal(b.Input, &in)
		if p, ok := in["path"].(string); ok {
			paths = append(paths, p)
		}
	}
	// Order: bracket parsed first, then XML.
	gotBracket, gotXML := false, false
	for _, p := range paths {
		if p == "/from/bracket" {
			gotBracket = true
		}
		if p == "/from/xml" {
			gotXML = true
		}
	}
	if !gotBracket || !gotXML {
		t.Fatalf("expected both bracket and XML parsed, got paths=%v", paths)
	}
}

func TestExtractBracketToolCalls_SingleQuotedToolName(t *testing.T) {
	// Some models emit single quotes around the tool name key.
	text := "[TOOL_CALL]\n" +
		"{'tool' => 'Read', 'args' => {\n" +
		"  --path \"/single/quoted\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "sq")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Name != "Read" {
		t.Fatalf("expected Read, got %q", blocks[0].Name)
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if in["path"] != "/single/quoted" {
		t.Fatalf("expected path preserved, got %v", in)
	}
}

func TestExtractBracketToolCalls_BooleanCoercion(t *testing.T) {
	text := "[TOOL_CALL]\n" +
		"{tool => \"WebFetch\", args => {\n" +
		"  --url \"https://x\"\n" +
		"  --follow_redirects \"true\"\n" +
		"}}\n" +
		"[/TOOL_CALL]"

	blocks, _ := internal_hook.ExtractXMLToolCalls(text, "bc")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	var in map[string]any
	_ = json.Unmarshal(blocks[0].Input, &in)
	if b, ok := in["follow_redirects"].(bool); !ok || !b {
		t.Fatalf("expected boolean true, got %v (%T)", in["follow_redirects"], in["follow_redirects"])
	}
}
