// runlabel_test.go — 驱动传给子进程的 --run-label 必须真的出现在 argv 里。
//
// 背景. 驱动起的每一轮都是一次独立的 `claude-go run`, 各自落一个 trace-run-* 桶。
// 不带标签时, 这些运行在 webapp 的轨迹页上只有一串 session 哈希 (name = session id),
// 于是长程任务跑到第 8 轮时, 没人能把"这 8 个桶"和"我在跑的那个任务"对上。
//
// 判据取在**模型进程实际收到的字节**上, 不是驱动侧的自述 —— 本仓库对这条有明确
// 纪律 (用回显假 agent 看最终 argv): 驱动"认为"自己传了, 与子进程真的收到, 是两件事,
// 而只有后者决定轨迹页上显示什么。
package coder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argvProbe 是一个把最终 argv 原样落盘的假 agent, 然后回一个最小的 json envelope。
// 探针脚本落在 t.TempDir() 里: 测试产物不写进共享的 /tmp, 免得并行跑时互相踩。
const argvProbe = `#!/bin/sh
printf '%s\n' "$@" > "$PROBE_OUT"
printf '{"final":"ok","is_error":false,"session_id":"probe"}\n'
`

func TestClaudeGoAgentPassesRunLabel(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "argv-probe.sh")
	if err := os.WriteFile(bin, []byte(argvProbe), 0o755); err != nil {
		t.Fatalf("写探针失败: %v", err)
	}
	out := filepath.Join(dir, "argv.out")

	a := &ClaudeGoAgent{
		Bin: bin, Cwd: dir, MaxTurns: 3,
		RunLabel: "coder:capstone-toolbox",
	}
	// 探针靠环境变量找落盘点 —— ClaudeGoAgent 会把 os.Environ() 原样传下去,
	// 所以这里设一次就够。
	t.Setenv("PROBE_OUT", out)

	if _, err := a.Run(context.Background(), "跑一轮"); err != nil {
		t.Fatalf("跑探针失败: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("探针没落盘 argv (子进程可能根本没起来): %v", err)
	}
	got := string(b)

	// 判据用带换行的精确串, 而不是 strings.Contains(got, "coder:capstone-toolbox"):
	// 后者在"标签进了别的参数的值里"时也会通过, 那正是这条测试要挡住的形态。
	if !strings.Contains(got, "--run-label\ncoder:capstone-toolbox\n") {
		t.Fatalf("子进程的 argv 里没有 `--run-label coder:capstone-toolbox`。\n"+
			"轨迹页的显示名来自 CLI 侧收到的这个旗标, 没传就等于没标注。\n"+
			"实际收到的 argv:\n%s", got)
	}
}

// TestClaudeGoAgentOmitsEmptyRunLabel 空标签**不传旗标**, 而不是传一个空串。
//
// 两者在 CLI 侧走同一条回落 (用 session id), 但多一个空 argv 会让日志里
// "这一轮没标签"与"标签就是空"长得不一样, 徒增解释成本。
func TestClaudeGoAgentOmitsEmptyRunLabel(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "argv-probe.sh")
	if err := os.WriteFile(bin, []byte(argvProbe), 0o755); err != nil {
		t.Fatalf("写探针失败: %v", err)
	}
	out := filepath.Join(dir, "argv.out")
	t.Setenv("PROBE_OUT", out)

	a := &ClaudeGoAgent{Bin: bin, Cwd: dir}
	if _, err := a.Run(context.Background(), "跑一轮"); err != nil {
		t.Fatalf("跑探针失败: %v", err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "--run-label") {
		t.Fatalf("空标签不该产生 --run-label 旗标, 实际 argv:\n%s", b)
	}
}
