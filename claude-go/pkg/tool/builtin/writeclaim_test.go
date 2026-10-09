// writeclaim_test.go —— H3-4 落在**工具层**的那一半验收 (占用语义在 pkg/tool)。
//
// pkg/tool 那边判的是占用表本身; 这里判的是**工具真的用了它**。两件事必须分开验,
// 因为"占用表写得很对"与"Write 调它"之间隔着一次接线, 而那一次接线正是最容易缺的:
// 表可以完全正确, 工具里那一句 ClaimWrite 可以根本没写 —— 装配态看不出来, 只有
// 拿真工具去写才暴露。
//
// 判据落在磁盘上: 被拒的那一次**文件内容必须没变**。只看 "res.IsError == true" 是不够的
// —— 一个"先写再报错"的守卫同样满足它, 而覆盖已经发生了。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func callWrite(t *testing.T, claim *tool.WriteClaimant, path, contents string) *tool.ToolResult {
	t.Helper()
	res, err := (&FileWriteTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"contents":%q}`, path, contents)),
		&tool.ToolContext{Cwd: "/", WriteClaim: claim})
	if err != nil {
		t.Fatalf("Write 调用出错: %v", err)
	}
	return res
}

func callEdit(t *testing.T, claim *tool.WriteClaimant, path, oldStr, newStr string) *tool.ToolResult {
	t.Helper()
	res, err := (&FileEditTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":%q,"new_string":%q}`, path, oldStr, newStr)),
		&tool.ToolContext{Cwd: "/", WriteClaim: claim})
	if err != nil {
		t.Fatalf("Edit 调用出错: %v", err)
	}
	return res
}

// mustReadStr 与 filehistory_test.go 的 mustRead 区分开 (那边返回 []byte)。
func mustReadStr(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	return string(b)
}

// TestWriteTool_并发写同一文件被拒且不覆盖 验收原文的直接落点。
//
// 两条判据一起: 第二个写者拿到错误, **且磁盘上还是第一个写者的内容**。
func TestWriteTool_并发写同一文件被拒且不覆盖(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.go")

	a := tool.BeginWriter("stage=a")
	defer a.Release()
	b := tool.BeginWriter("stage=b")
	defer b.Release()

	// 第一个写者落盘。
	if res := callWrite(t, a, path, "package shared // A 的版本\n"); res.IsError {
		t.Fatalf("第一个写者不该被拒: %s", res.Content)
	}
	if got := mustReadStr(t, path); got != "package shared // A 的版本\n" {
		t.Fatalf("第一个写者的内容没落盘: %q", got)
	}

	// 第二个写者: 必须被明确拒绝, 且**磁盘内容一个字节都不变**。
	res := callWrite(t, b, path, "package shared // B 的版本\n")
	if !res.IsError {
		t.Fatal("第二个写者写同一文件必须被拒 —— 放行就是静默覆盖")
	}
	if !tool.IsWriteConflict(fmt.Errorf("%s", res.Content)) && !strings.Contains(res.Content, "占用") {
		t.Fatalf("拒绝理由应说明是并发写占用, 实得: %s", res.Content)
	}
	if got := mustReadStr(t, path); got != "package shared // A 的版本\n" {
		t.Fatalf("被拒的写必须不落盘, 磁盘上应是 A 的版本, 实得: %q", got)
	}
}

// TestEditTool_并发写同一文件被拒.
//
// Edit 与 Write 是两条独立的代码路径 (一个基于读到的旧内容算新内容)。只给 Write
// 接线是一种很常见的漏 —— 而 Edit 恰恰是"读到旧内容、基于它算新内容、写回"的
// 典型覆盖场景, 漏掉它等于漏掉了最该挡的那一半。
func TestEditTool_并发写同一文件被拒(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.go")
	if err := os.WriteFile(path, []byte("var x = 1\n"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	a := tool.BeginWriter("stage=a")
	defer a.Release()
	if res := callEdit(t, a, path, "var x = 1", "var x = 2"); res.IsError {
		t.Fatalf("第一个写者不该被拒: %s", res.Content)
	}

	b := tool.BeginWriter("stage=b")
	defer b.Release()
	res := callEdit(t, b, path, "var x = 2", "var x = 999")
	if !res.IsError {
		t.Fatal("第二个写者 Edit 同一文件必须被拒")
	}
	if got := mustReadStr(t, path); got != "var x = 2\n" {
		t.Fatalf("被拒的 Edit 必须不落盘, 实得: %q", got)
	}
}

// TestWriteTool_同一写者反复写放行 反面臂。
//
// 一次 run 里反复改同一个文件是常态。若这里被拒, 长程任务会被自己的护栏打死,
// 且症状是"模型每次编辑都失败", 看起来像模型的问题。
func TestWriteTool_同一写者反复写放行(t *testing.T) {
	path := filepath.Join(t.TempDir(), "own.go")
	a := tool.BeginWriter("stage=a")
	defer a.Release()

	for i := 0; i < 5; i++ {
		content := fmt.Sprintf("package own // 第 %d 版\n", i)
		if res := callWrite(t, a, path, content); res.IsError {
			t.Fatalf("同一写者第 %d 次写自己的文件不该被拒: %s", i+1, res.Content)
		}
		if got := mustReadStr(t, path); got != content {
			t.Fatalf("第 %d 次写的内容没落盘: %q", i+1, got)
		}
	}
}

// TestWriteTool_未装配占用照常写 回归臂 —— 这是**改造前的全部行为**。
//
// 主会话循环不登记写者, 工具被单独调用时也没有引擎装配它。那条路上的写入必须与
// 改造前逐字节相同; 否则这个改动就从"给并行子代理加保护"变成"给所有人改了默认语义",
// 而后者是没人会预期到的。
func TestWriteTool_未装配占用照常写(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.go")

	if res := callWrite(t, nil, path, "package plain\n"); res.IsError {
		t.Fatalf("未装配占用时写入必须照常成功, 实得: %s", res.Content)
	}
	if got := mustReadStr(t, path); got != "package plain\n" {
		t.Fatalf("内容不对: %q", got)
	}
	// 反复写也照常 (没有占用表, 也不该有任何限流)。
	for i := 0; i < 3; i++ {
		if res := callWrite(t, nil, path, fmt.Sprintf("// v%d\n", i)); res.IsError {
			t.Fatalf("未装配占用时第 %d 次写被拒: %s", i+1, res.Content)
		}
	}
}

// TestWriteTool_占用先于语法守卫.
//
// 顺序是有意的: 占用是"谁有权动这个文件", 语法是"内容合不合法"。反过来的话,
// 一个语法**合法**的覆盖会被放行 —— 而覆盖这件事本身才是要挡的, 与内容长什么样无关。
// 这条用一份语法完全正确的覆盖内容来判: 它必须仍然被拒。
func TestWriteTool_占用先于语法守卫(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "order.go")

	a := tool.BeginWriter("stage=a")
	defer a.Release()
	if res := callWrite(t, a, path, "package order\n"); res.IsError {
		t.Fatalf("第一个写者失败: %s", res.Content)
	}

	b := tool.BeginWriter("stage=b")
	defer b.Release()
	// 完全合法 Go 源码 —— 语法护栏在这里一个字都说不出。
	res := callWrite(t, b, path, "package order // 完全合法的覆盖\n")
	if !res.IsError {
		t.Fatal("语法合法的覆盖同样是数据丢失, 必须被占用闸拦下")
	}
	if got := mustReadStr(t, path); got != "package order\n" {
		t.Fatalf("被拒的写不该落盘: %q", got)
	}
}
