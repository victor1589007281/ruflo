package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

// TestBashToolUsesMergeGuard 钉住闸门确实接在 BashTool.Call 的执行路径上。
//
// 只测 BashMergeGuard 本身不够: 把 CheckAndRecord 写在 Call 之外 (或写在一个
// 永远不进的 if 里), 单元测试照样全绿, 而生产上该跑的命令照跑。判据因此落在
// **BashTool 的返回内容**上 —— 且不能用"exit code 非零"代替 (文件不存在时
// cat 自己也会非零), 必须认闸门文案。
func TestBashToolUsesMergeGuard(t *testing.T) {
	cwd := t.TempDir()
	g := tool.NewBashMergeGuard()
	if reason := g.CheckAndRecord(cwd, "curl -sL https://example.com/report.pdf -o /tmp/report.pdf"); reason != "" {
		t.Fatalf("首次下载不应被拒: %s", reason)
	}

	res, err := NewBashTool().Call(context.Background(),
		json.RawMessage(`{"command":"cat /tmp/report.pdf"}`),
		&tool.ToolContext{Cwd: cwd, BashMerge: g})
	if err != nil {
		t.Fatalf("闸门拒绝应走 ToolResult(IsError), 不是 Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("重复单独访问应返回 IsError, got %+v", res)
	}
	if !strings.Contains(res.Content, "Bash 合并闸") {
		t.Fatalf("返回内容不是闸门文案 —— 命令可能已经真的执行了: %q", res.Content)
	}
}
