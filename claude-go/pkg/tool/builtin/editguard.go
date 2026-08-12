// editguard.go — edit/write 写入前语法护栏 (方案三 L2 护栏半件, 手册 13.3.3)。
//
// SWE-agent ACI 原则 (arXiv:2405.15793): 坏 diff 直接拒绝并把错误作为反馈返回,
// 而不是落盘一个编译不过的文件让模型在后面十几轮里补救。对弱模型这既是
// "零成本确定性 critic"——把语法错误从"延迟爆炸"变成"即时可恢复反馈"。
//
// 覆盖: .go (go/parser 完整解析) / .json (json.Valid) / .py (python3 -m py_compile,
// 子进程 5s 超时, 无 python3 则放行)。其余扩展名放行 (不误伤)。
// 仅在 ToolContext.WeakEditGuard=true (弱模型 Profile.SchemaRetry 开启) 时生效。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
)

// guardWriteSyntax 写入前语法校验。返回非 nil 的 ToolResult 表示应拒绝写入。
// content 是**写入后的完整内容** (edit 场景为替换后的新内容)。
func guardWriteSyntax(path, content string, tctx *tool.ToolContext) *tool.ToolResult {
	if tctx == nil || !tctx.WeakEditGuard {
		return nil
	}
	var errMsg string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		fset := token.NewFileSet()
		if _, err := parser.ParseFile(fset, path, content, parser.AllErrors); err != nil {
			errMsg = err.Error()
		}
	case ".json":
		if !json.Valid([]byte(content)) {
			errMsg = "json.Valid 校验失败 (非法 JSON)"
		}
	case ".py":
		errMsg = pyCompileCheck(content)
	}
	if errMsg == "" {
		return nil
	}
	if len(errMsg) > 600 {
		errMsg = errMsg[:600] + "…"
	}
	return &tool.ToolResult{
		Content: fmt.Sprintf("[edit_guard] 已拒绝写入 %s: 写入后内容语法错误, 文件未被修改。错误: %s。请修正后重新调用 (注意括号/缩进/引号配对)。", path, errMsg),
		IsError: true,
	}
}

// pyCompileCheck 用 python3 -m py_compile 校验。python3 不可用或超时则放行 (返回空)。
func pyCompileCheck(content string) string {
	if _, err := exec.LookPath("python3"); err != nil {
		return ""
	}
	f, err := os.CreateTemp("", "editguard-*.py")
	if err != nil {
		return ""
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return ""
	}
	f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-m", "py_compile", f.Name()).CombinedOutput()
	if ctx.Err() != nil || err == nil {
		return "" // 超时按放行处理; 编译通过 errMsg 为空
	}
	return strings.TrimSpace(string(out))
}
