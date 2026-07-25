package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func readOnce(t *testing.T, rt tool.Tool, tctx *tool.ToolContext, args string) string {
	t.Helper()
	res, err := rt.Call(context.Background(), json.RawMessage(args), tctx)
	if err != nil {
		t.Fatalf("Call(%s) 出错: %v", args, err)
	}
	if res.IsError {
		t.Fatalf("Call(%s) 返回错误: %s", args, res.Content)
	}
	return res.Content
}

// 回归：Read 的去重缓存曾只用 path 作键，忽略 offset/limit。于是"先整读、
// 再读第 2-3 行"会命中去重，返回 "<unchanged since last read>" 而不是那几行
// ——agent 想回看片段只能拿到一句无用提示。
//
// 本测试同时钉住两个方向，防止把 bug 修成"直接关掉去重"：
// 换视图必须返回真内容；同一视图重复读必须仍然去重。
func TestFileRead_去重键含offset与limit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(f, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := NewFileReadTool()
	tctx := &tool.ToolContext{Cwd: dir}

	// ① 整读
	full := readOnce(t, rt, tctx, `{"path":"`+f+`"}`)
	if !strings.Contains(full, "line1") || !strings.Contains(full, "line5") {
		t.Fatalf("整读内容不完整: %q", full)
	}

	// ② 换视图（offset+limit）——内容 hash 与①相同，但这是不同的请求
	slice := readOnce(t, rt, tctx, `{"path":"`+f+`","offset":2,"limit":2}`)
	if strings.Contains(slice, "unchanged since last read") {
		t.Fatalf("换 offset/limit 后被误判为重复读: %q", slice)
	}
	if !strings.Contains(slice, "line2") || !strings.Contains(slice, "line3") {
		t.Errorf("切片未含期望行: %q", slice)
	}
	if strings.Contains(slice, "line1") || strings.Contains(slice, "line4") {
		t.Errorf("切片含了不该有的行: %q", slice)
	}

	// ③ 同一视图重复读——去重必须仍然生效（否则等于把优化关掉了）
	again := readOnce(t, rt, tctx, `{"path":"`+f+`","offset":2,"limit":2}`)
	if !strings.Contains(again, "unchanged since last read") {
		t.Errorf("同一视图重复读应命中去重, 实际返回: %q", again)
	}

	// ④ 回到整读视图：该视图上次读过且内容未变, 也应命中去重
	fullAgain := readOnce(t, rt, tctx, `{"path":"`+f+`"}`)
	if !strings.Contains(fullAgain, "unchanged since last read") {
		t.Errorf("整读视图重复读应命中去重, 实际返回: %q", fullAgain)
	}

	// ⑤ 文件真变了：同一视图必须重新返回内容
	if err := os.WriteFile(f, []byte("line1\nCHANGED\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterChange := readOnce(t, rt, tctx, `{"path":"`+f+`","offset":2,"limit":2}`)
	if strings.Contains(afterChange, "unchanged since last read") {
		t.Errorf("文件已变更, 不应命中去重: %q", afterChange)
	}
	if !strings.Contains(afterChange, "CHANGED") {
		t.Errorf("变更后未返回新内容: %q", afterChange)
	}
}
