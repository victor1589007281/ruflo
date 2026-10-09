package compact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

func bigResult(id, body string) types.ContentBlock {
	return types.ContentBlock{
		Type:      types.ContentBlockToolResult,
		ToolUseID: id,
		Content:   body + strings.Repeat("x", MicroCompactThreshold+1),
	}
}

func userMsg(blocks ...types.ContentBlock) types.Message {
	return types.Message{Type: types.MessageTypeUser, Content: blocks}
}

// 核心契约: 引用名是**内容的纯函数**。
//
// 这条一破, 同一条 tool_result 在不同轮次就折出不同字节, 从该位置起的前缀缓存
// 全废 —— 正是这套折叠机制存在的理由被自己打掉。
func TestToolResultRefName_IsContentAddressed(t *testing.T) {
	a := ToolResultRefName("同样的内容")
	b := ToolResultRefName("同样的内容")
	if a != b {
		t.Fatalf("同内容应得同名, got %q vs %q", a, b)
	}
	if ToolResultRefName("别的内容") == a {
		t.Error("不同内容应得不同名")
	}
	if !strings.HasSuffix(a, ".txt") {
		t.Errorf("名字应带 .txt 后缀, got %q", a)
	}
	// 不含调用 id / 时间戳 —— 名字里除了十六进制和小数点不该有别的东西。
	for _, r := range strings.TrimSuffix(a, ".txt") {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("名字应只由十六进制构成 (否则掺进了非内容信息), got %q", a)
		}
	}
}

// 折叠占位必须是纯函数: 同一条 tool_result 折两次, 字节完全相同。
func TestToolResultStubWithRef_Idempotent(t *testing.T) {
	store := NewToolResultRefStore(t.TempDir())
	blk := bigResult("call_1", "正文开头\n")

	first := ToolResultStubWithRef(blk, store)
	second := ToolResultStubWithRef(blk, store)
	if first != second {
		t.Fatalf("同一 block 两次折叠产出不同字节:\n第一次=%q\n第二次=%q", first, second)
	}

	// 落盘后**再折一次**仍须相同 —— 已存在的文件不能让占位变形。
	if _, err := store.Put(blk.Content); err != nil {
		t.Fatal(err)
	}
	if third := ToolResultStubWithRef(blk, store); third != first {
		t.Fatalf("落盘后重折产出变了:\n前=%q\n后=%q", first, third)
	}
}

// 落盘的原文必须能逐字节读回 —— 这是"文件引用"相对"截断"的全部价值。
func TestToolResultRefStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStore(dir)
	content := "完整原文\n" + strings.Repeat("内容", 5000)

	name, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("读回的原文与写入不一致 (len %d vs %d)", len(got), len(content))
	}
}

// 内容寻址顺带带来去重: 同一内容写两次只有一份文件。
func TestToolResultRefStore_DedupesIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStore(dir)
	content := strings.Repeat("y", 3000)

	for i := 0; i < 3; i++ {
		if _, err := store.Put(content); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("同内容应只落一份, got %d 份", len(entries))
	}
}

// 折叠后的占位里要给出可回读的路径, 且原文确实在盘上。
func TestStubToolResultsWithRef_WritesAndReferences(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStore(dir)
	blk := bigResult("call_9", "大文件内容\n")

	out := StubToolResultsWithRef([]types.Message{userMsg(blk)}, 0, -1, store)
	got := out[0].Content[0].Content

	if !strings.Contains(got, "call_id=call_9") {
		t.Error("占位必须保留 call_id (tool_use 按 id 配对)")
	}
	if !strings.Contains(got, store.Path(ToolResultRefName(blk.Content))) {
		t.Errorf("占位里应给出回读路径, got %q", got[:min(200, len(got))])
	}
	// 路径不能是空话: 按占位里给的路径必须真能读到完整原文。
	name := ToolResultRefName(blk.Content)
	back, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("占位声称的原文读不到: %v", err)
	}
	if string(back) != blk.Content {
		t.Error("盘上原文与折叠前不一致")
	}
}

// nil 存储 = 不落盘, 行为必须与改造前的纯截断逐字节相同, 且不 panic。
func TestStubToolResultsWithRef_NilStoreIsPureTruncation(t *testing.T) {
	blk := bigResult("call_nil", "正文\n")
	withNil := StubToolResultsWithRef([]types.Message{userMsg(blk)}, 0, -1, nil)
	got := withNil[0].Content[0].Content

	if strings.Contains(got, "完整原文见") {
		t.Error("nil 存储不该给出引用路径")
	}
	if got != ToolResultStub(blk) {
		t.Error("nil 存储的产出应与 ToolResultStub 逐字节相同")
	}
	// nil 上的方法调用不能 panic。
	var s *ToolResultRefStore
	if s.Dir() != "" || s.Path("x") != "" {
		t.Error("nil 存储的 Dir/Path 应返回空串")
	}
	if _, err := s.Put("x"); err == nil {
		t.Error("nil 存储 Put 应报错而不是静默丢弃") // 不应触碰磁盘
	}
}

// 未超阈值的 tool_result 不折、不落盘 —— 别为了省 4 个字节把盘写满。
func TestStubToolResultsWithRef_SmallResultsUntouched(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStore(dir)
	small := types.ContentBlock{
		Type: types.ContentBlockToolResult, ToolUseID: "c", Content: "很短",
	}
	out := StubToolResultsWithRef([]types.Message{userMsg(small)}, 0, -1, store)
	if out[0].Content[0].Content != "很短" {
		t.Error("小结果不该被折叠")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("小结果不该落盘, got %d 个文件", len(entries))
	}
}

// 落盘目录为空 = 关闭该功能 (纯测试 / 只读挂载场景)。
func TestNewToolResultRefStore_EmptyDirDisables(t *testing.T) {
	if NewToolResultRefStore("") != nil {
		t.Error("空目录应返回 nil (关闭落盘)")
	}
	if NewToolResultRefStore("   ") != nil {
		t.Error("纯空白目录应返回 nil")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 折叠必须认得"本轮有人在读引用目录", 并据此放弃落盘。
//
// 这条守的是一个**真实发生过的坏循环**: 引用被 Read 回来后, 内容比原文更大 (Read 每行
// 加 `<行号>|` 前缀), 再次超过阈值 -> 又折一次 -> 给出指向引用的引用。实测 20770 字节
// 的引用被读回后折成 21511 字节的新引用, 完整原文永远到不了模型眼前。
//
// 判据用**内容字节**而不是"有没有落盘": 落盘与否是手段, 原文能不能进上下文才是目的。
// 只断言目录为空的话, 把保护写成"永不给引用"也能过。
func TestRefFoldDoesNotFoldTheReadBackOfARef(t *testing.T) {
	// 用生产构造器: 自指保护要同时认折叠引用目录与 pkg/tool 的溢出目录。
	// 只造折叠目录的话, 真实场景里模型读的是**溢出 artifact**, 保护不生效而测试全绿。
	workdir := t.TempDir()
	dir := filepath.Join(workdir, ToolResultRefDir())
	store := NewToolResultRefStoreForCwd(workdir)
	if store == nil {
		t.Fatal("生产构造器返回 nil, 落盘等于关闭")
	}

	// 第 1 轮: 一条大 tool_result, 正常折叠并给出引用。
	big := "BIG-" + strings.Repeat("Z", MicroCompactThreshold+3000)
	msgs := []types.Message{
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "c1", Name: "Bash"},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c1", Content: big},
		}},
	}
	out := MicroCompactWithRef(msgs, 0, store)
	refPath := ""
	for _, line := range strings.Split(out[1].Content[0].Content, "\n") {
		if p, ok := strings.CutPrefix(line, "full_artifact: "); ok {
			refPath = p
		}
	}
	if refPath == "" {
		t.Fatal("第 1 轮没给出引用路径 —— 前置条件不成立, 后面的断言没有意义")
	}
	if _, err := os.Stat(refPath); err != nil {
		t.Fatalf("引用路径读不到: %v", err)
	}

	// 第 2 轮: 模型 Read 那个引用, 读回的正文**比原文更大** (Read 加了行号)。
	// 这就是真实形态 —— 内容不同、体量更大, 所以不能靠"内容相同就不折"来挡。
	var sb strings.Builder
	for i, line := range strings.Split(big, "\n") {
		sb.WriteString(fmt.Sprintf("%5d|%s\n", i+1, line))
	}
	readBack := sb.String()
	if len(readBack) <= len(big) {
		t.Fatalf("构造的读回内容 (%d) 没有比原文 (%d) 大, 复现不出真实场景", len(readBack), len(big))
	}

	round2 := append(append([]types.Message{}, msgs...), []types.Message{
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "c2", Name: "Read",
				Input: json.RawMessage(`{"path":"` + refPath + `"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c2", Content: readBack},
		}},
	}...)

	// 只折本轮新增 (protectFrom=2), 与引擎里的 SentUpTo 语义一致。
	got := MicroCompactWithRef(round2, 2, store)

	// 读回的正文必须**完整留在上下文里**。
	if !strings.Contains(got[3].Content[0].Content, readBack) {
		t.Fatalf("读回引用后的正文被折叠了 —— 原文永远进不了上下文。\n实际: %s",
			truncateForTest2(got[3].Content[0].Content, 300))
	}
	// 且不得再冒出第二个引用文件 (落盘也被跳过了)。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("引用目录里有 %d 个文件, 期望 1 (第二次折叠不该落盘)", len(entries))
	}
}

// 对照: 同样一条大 tool_result, 只要**不是**在读引用目录, 就该照常折叠 + 落盘。
// 缺了这条, 把保护写成"永不折叠"也能让上面那条测过。
func TestRefFoldStillFoldsUnrelatedBigResults(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStore(dir)
	msgs := []types.Message{
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "c1", Name: "Read",
				Input: json.RawMessage(`{"path":"/workspace/src/main.go"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c1",
				Content: "SRC-" + strings.Repeat("Q", MicroCompactThreshold+3000)},
		}},
	}
	out := MicroCompactWithRef(msgs, 0, store)
	if !strings.Contains(out[1].Content[0].Content, "已折叠") {
		t.Fatal("普通大结果没被折叠 —— 保护把折叠整个关死了")
	}
	if !strings.Contains(out[1].Content[0].Content, "full_artifact: ") {
		t.Fatal("普通大结果折叠后没给引用")
	}
}

func truncateForTest2(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(截断)"
}

// 真实生产场景: 模型读的是 pkg/tool 的**执行期溢出 artifact**, 不是折叠引用目录。
//
// 这条是线上实测暴露出来的那一半 —— 只用折叠目录做测试时它会漏过去, 因为模型回读
// 时拿到的路径恰好来自溢出目录 (执行期先折了一次, 占位里给的是 artifact 路径)。
// 保护漏掉这个目录, 读回的正文照样被再折一次, 原文照样进不了上下文。
func TestRefFoldSkipsReadBackOfSpillArtifact(t *testing.T) {
	workdir := t.TempDir()
	spillDir := tool.ToolResultArtifactDir(workdir)
	if spillDir == "" {
		t.Fatal("溢出目录为空, 前置条件不成立")
	}
	store := NewToolResultRefStoreForCwd(workdir)

	// 模型接到的路径来自溢出目录 (执行期溢出的形态)。
	spillPath := filepath.Join(spillDir, "Bash-abc123.txt")
	original := strings.Repeat("S", MicroCompactThreshold+3000)

	msgs := []types.Message{
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "r1", Name: "Read",
				Input: json.RawMessage(`{"path":"` + spillPath + `"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "r1", Content: original},
		}},
	}
	got := MicroCompactWithRef(msgs, 0, store)
	if got[1].Content[0].Content != original {
		t.Fatalf("读回溢出 artifact 的正文被折叠了 —— 原文进不了上下文。\n实际: %s",
			truncateForTest2(got[1].Content[0].Content, 200))
	}
	// 也不该为它落盘。
	entries, err := os.ReadDir(filepath.Join(workdir, ToolResultRefDir()))
	if err == nil && len(entries) != 0 {
		t.Fatalf("跳过的那条不该落盘, 却写了 %d 个文件", len(entries))
	}
}

// 对照: 同一批里**其它**大结果照常折叠并给出引用。
//
// 保护是按 tool_use→tool_result 配对精确命中的, 不是"整批关掉"。缺了这条, 把保护
// 实现成"看到任何一次回读就全批不折"也能让上面两条测过 —— 代价是别的大结果白白
// 失去回读能力。
func TestRefFoldStillFoldsSiblingsOfARefReadBack(t *testing.T) {
	workdir := t.TempDir()
	store := NewToolResultRefStoreForCwd(workdir)
	spillPath := filepath.Join(tool.ToolResultArtifactDir(workdir), "Bash-x.txt")

	sibling := "SIBLING-" + strings.Repeat("W", MicroCompactThreshold+3000)
	msgs := []types.Message{
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "r1", Name: "Read",
				Input: json.RawMessage(`{"path":"` + spillPath + `"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "r1", Content: strings.Repeat("S", MicroCompactThreshold+3000)},
		}},
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "r2", Name: "Bash",
				Input: json.RawMessage(`{"command":"cat big"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "r2", Content: sibling},
		}},
	}
	got := MicroCompactWithRef(msgs, 0, store)

	if got[1].Content[0].Content == "" || !strings.Contains(got[1].Content[0].Content, "SSSS") {
		t.Fatal("读回的那条应原样保留")
	}
	if !strings.Contains(got[3].Content[0].Content, "已折叠") {
		t.Fatal("同批的兄弟大结果应照常折叠 —— 保护不该整批关掉落盘")
	}
	if !strings.Contains(got[3].Content[0].Content, "full_artifact: ") {
		t.Fatal("兄弟大结果折叠后应给出引用")
	}
}
