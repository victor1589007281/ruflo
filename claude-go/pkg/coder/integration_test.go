package coder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeFakeAgent 落一个假 agent 脚本, 它只认 claude-go CLI 的少数几个参数,
// 但**通过真实的 exec 路径**跑 —— 这是本包里唯一一条覆盖了
// ClaudeGoAgent.Run (argv 组装 / prompt 落盘 / envelope 解析 / 日志落盘)
// 与 CommandVerifier (真的起进程、真的看退出码) 的测试。
//
// 它不假装自己测了 LLM。它测的是"驱动侧那一圈接线对不对":
// 接线错了 (参数名写错、prompt 没落到 agent 能读到的地方、envelope 解析挂掉),
// 真模型跑起来会表现成一模一样的"agent 什么都不做"。
func writeFakeAgent(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-claude-go")
	if err := os.WriteFile(p, []byte("#!/bin/bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeGoModule(t *testing.T, ws string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module smoke\n\ngo 1.21\n",
		"greet.go": `package smoke

// 还没实现
`,
		"greet_test.go": `package smoke

import "testing"

func TestGreet(t *testing.T) {
	if Greet() != "hi" {
		t.Fatalf("Greet() = %q", Greet())
	}
}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEndToEndWithRealProcesses 整条链路跑一遍: 真进程、真验证、真落盘、真恢复。
//
// 脚本的行为是**刻意设计**的:
//   - 第 1 轮什么都不做 (真实世界里这是上游 400 被吞掉之后的样子)
//   - 从第 2 轮起把 Greet 写好
//
// 于是它同时压了三件事: 空转连击会涨但没到上限 (不会误杀)、
// 没做完会继续下一轮、做完了会停并判过。
func TestEndToEndWithRealProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("起真进程, -short 跳过")
	}
	ws := t.TempDir()
	writeGoModule(t, ws)

	// 假 agent: 读 prompt.md 确认驱动侧真的把 prompt 落到了它能读到的地方;
	// 只在第 2 轮起写代码。
	bin := writeFakeAgent(t, `
set -e
D="$PWD/.coder/attempts"
mkdir -p "$D"
N=$(( $(ls "$D" 2>/dev/null | wc -l) + 1 ))
: > "$D/$N"
if [ ! -s "$PWD/.coder/prompt.md" ]; then
  echo "驱动侧没有把 prompt 落到 .coder/prompt.md" >&2
  printf '{"final":"","session_id":"x","is_error":true}\n'
  exit 0
fi
if [ "$N" -ge 2 ]; then
  printf 'package smoke\n\nfunc Greet() string { return "hi" }\n' > "$PWD/greet.go"
fi
echo "[fake] 第 $N 轮" >&2
printf '{"final":"第 %s 轮结束","session_id":"fake-%s","is_error":false}\n' "$N" "$N"
`)

	r := &Runner{
		Config: Config{
			Goal: "实现 Greet 函数, 让 go test ./... 通过", Workspace: ws,
			VerifyCmd: []string{"go", "test", "./..."},
			Bin:       bin, AllowedTools: []string{"Read", "Write", "Bash"},
			MaxTurnsPerIteration: 5, MaxIterations: 6, NoProgressLimit: 3,
			IterationTimeout: 2 * time.Minute, VerifyTimeout: 2 * time.Minute,
		},
		Agent: &ClaudeGoAgent{
			Bin: bin, Cwd: ws, MaxTurns: 5,
			AllowedTools: []string{"Read", "Write", "Bash"},
		},
		Verify: CommandVerifier{
			Cmd: []string{"go", "test", "./..."}, Timeout: 2 * time.Minute,
		},
		Logf: func(string, ...any) {},
	}

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("整条链路失败: %v\n报告:\n%s", err, res.Report())
	}
	if !res.Passed {
		t.Fatalf("应当判为通过\n报告:\n%s", res.Report())
	}
	if len(res.State.History) != 2 {
		t.Fatalf("轮数 = %d, 期望 2 (第 1 轮空转, 第 2 轮做好)", len(res.State.History))
	}
	// 第 1 轮必须被如实记成"没动手"。
	if res.State.History[0].FingerprintChanged {
		t.Fatal("第 1 轮什么都没改, 指纹却是变了 —— 指纹算进了不该算的东西")
	}
	// 第 2 轮必须被记成动手了。
	if !res.State.History[1].FingerprintChanged {
		t.Fatal("第 2 轮写了 greet.go, 指纹却没变")
	}
	// 终态验收必须在干净副本里跑过, 且副本里没有 .coder。
	if res.CleanDir == "" {
		t.Fatal("没有做干净副本验收")
	}
	if _, err := os.Stat(filepath.Join(res.CleanDir, RuntimeDir)); !os.IsNotExist(err) {
		t.Fatal("干净副本里混进了 .coder")
	}
	if _, err := os.Stat(filepath.Join(res.CleanDir, "greet.go")); err != nil {
		t.Fatalf("干净副本里没有产物: %v", err)
	}
	// 每轮的日志与 prompt 必须落盘 (取证)。
	if _, err := os.Stat(filepath.Join(ws, RuntimeDir, "logs", "iter-001", "stdout.log")); err != nil {
		t.Fatalf("第 1 轮的 stdout 没落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, RuntimeDir, promptFile)); err != nil {
		t.Fatalf("prompt 没落盘: %v", err)
	}
}

// resumeScript 是"被杀→恢复"那条测试用的假 agent。
//
// 它的行为: 第 1、2 轮动手但写错; 第 3 轮开始**卡住不动** (父进程会在这时候
// 把整个驱动进程 SIGKILL 掉); 第 4 轮起才写对。
//
// 注意它的轮次计数器 N 是自己数的 (靠 .coder/attempts 里的文件数), 与驱动侧的
// iteration 是两套编号 —— 这正是要的: 被杀那一轮在驱动侧没有记录, 但 agent 侧
// 已经推进过, 于是恢复之后两边能对上。
const resumeScript = `
D="$PWD/.coder/attempts"
mkdir -p "$D"
N=$(( $(ls "$D" 2>/dev/null | wc -l) + 1 ))
: > "$D/$N"
if [ "$N" -eq 3 ]; then
  # 告诉父进程"我已经进第 3 轮了", 然后卡住等被杀。
  echo started > "$PWD/.coder/round3-started"
  sleep 300
fi
if [ "$N" -ne 1 ]; then
  printf 'package smoke\n\nfunc Greet() string { return "nope" }\n' > "$PWD/greet.go"
fi
if [ "$N" -ge 4 ]; then
  printf 'package smoke\n\nfunc Greet() string { return "hi" }\n' > "$PWD/greet.go"
fi
printf '{"final":"第 %s 轮结束","session_id":"fake-%s","is_error":false}\n' "$N" "$N"
`

func resumeRunner(ws, bin string) *Runner {
	return &Runner{
		Config: Config{
			Goal: "实现 Greet 函数, 让 go test ./... 通过", Workspace: ws,
			VerifyCmd:            []string{"go", "test", "./..."},
			Bin:                  bin,
			MaxTurnsPerIteration: 5, MaxIterations: 10, NoProgressLimit: 3,
			IterationTimeout: 5 * time.Minute, VerifyTimeout: 2 * time.Minute,
		},
		Agent: &ClaudeGoAgent{Bin: bin, Cwd: ws, MaxTurns: 5},
		Verify: CommandVerifier{
			Cmd: []string{"go", "test", "./..."}, Timeout: 2 * time.Minute,
		},
		Logf: func(string, ...any) {},
	}
}

// TestResumeAfterDriverKill 驱动进程被 SIGKILL 之后, 原样再跑必须**接着**跑。
//
// 这一条杀的是**驱动进程本身** (不是我上一版写错的那种"假 agent 自杀"——
// 那样杀的是 agent 子进程, 驱动会正确地当作单轮失败继续跑, 根本走不到恢复路径)。
//
// 判据落在**轮次编号**上, 而不是"最终通过了": 一个从头重跑的实现照样能通过,
// 只是把前面几轮的进展全丢了 —— 那种实现只有编号能拆穿。
func TestResumeAfterDriverKill(t *testing.T) {
	if testing.Short() {
		t.Skip("起真进程, -short 跳过")
	}
	ws := t.TempDir()
	writeGoModule(t, ws)
	bin := writeFakeAgent(t, resumeScript)

	// ── 第一次: 在**子进程**里跑驱动, 跑到第 3 轮时把它 SIGKILL 掉 ──
	// 必须在子进程里跑: 在测试进程内跑就没法杀, 杀了测试也就死了。
	helper := exec.Command(os.Args[0], "-test.run=^TestCoderDriverHelper$", "-test.v")
	helper.Env = append(os.Environ(),
		"CODER_HELPER=1", "CODER_WS="+ws, "CODER_BIN="+bin)
	helper.Dir = ws
	// 独立进程组: 杀的时候要把卡住的假 agent (sleep 300) 一起带走,
	// 否则它会变成孤儿进程一直挂着。
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var helperOut strings.Builder
	helper.Stdout = &helperOut
	helper.Stderr = &helperOut
	if err := helper.Start(); err != nil {
		t.Fatalf("起 helper 失败: %v", err)
	}

	// 等它真的进到第 3 轮 (卡住的那一刻), 再杀 —— 这样杀的是一个**正在跑**的驱动。
	marker := filepath.Join(ws, RuntimeDir, "round3-started")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-helper.Process.Pid, syscall.SIGKILL)
			t.Fatalf("等第 3 轮开始超时。helper 输出:\n%s", helperOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 再等一下, 确保它正卡在 agent 里 (而不是刚好在写状态), 然后真的杀掉。
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(-helper.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("杀 helper 失败: %v", err)
	}
	_ = helper.Wait() // 被杀 → 必然非零退出, 不检查

	// ── 被杀之后, 落盘状态必须是**真实的**: 只有跑完的那几轮 ──
	st, existed, err := LoadState(ws)
	if err != nil || !existed {
		t.Fatalf("被杀之后没有可恢复的状态: existed=%v err=%v", existed, err)
	}
	if st.Iteration != 2 {
		t.Fatalf("落盘轮次 = %d, 期望 2 —— 第 3 轮没跑完就不该有它的记录 "+
			"(落盘时机太早会记下一轮其实没发生的事)", st.Iteration)
	}
	if len(st.History) != 2 {
		t.Fatalf("落盘历史 = %d 条, 期望 2", len(st.History))
	}

	// ── 第二次: 原样再跑一次, 必须接着跑 ──
	res, err := resumeRunner(ws, bin).Run(context.Background())
	if err != nil {
		t.Fatalf("恢复后应当跑完: %v\n报告:\n%s", err, res.Report())
	}
	if !res.Passed {
		t.Fatalf("恢复后应当判为通过\n报告:\n%s", res.Report())
	}
	// 判据一: 历史是**累积**的 —— 被杀之前那两轮的读数必须原样还在。
	//
	// 注意 History[0].N == 1 是**对的** (它是这个任务的第一轮), 不能拿它判"从头跑了"。
	// 光看编号顺序也拆不开"接着跑"与"从头跑" (两者都会得到 #1,#2,#3) —— 得看
	// 编号是怎么来的。
	if len(res.State.History) != 3 {
		t.Fatalf("恢复后历史 = %d 条, 期望 3\n%s", len(res.State.History), res.Report())
	}
	for i, want := range []int{1, 2, 3} {
		if res.State.History[i].N != want {
			t.Fatalf("第 %d 条记录编号 = %d, 期望 %d\n%s",
				i, res.State.History[i].N, want, res.Report())
		}
	}
	// 判据二: #1/#2 是上一次进程跑出来的 (Resumed=false), #3 才是这次恢复的。
	// 一个从头跑的实现会让三条全是 Resumed=false。
	if res.State.History[0].Resumed || res.State.History[1].Resumed {
		t.Fatal("前两条记录被标成 Resumed —— 它们本来是上一个进程跑的")
	}
	if !res.State.History[2].Resumed {
		t.Fatal("第 3 条记录没有标记 Resumed —— 恢复后跑的那一轮没被认出来")
	}
	// 判据三 (最硬的一条): 恢复进程**只**调了 agent 一次。
	//
	// 假 agent 每次被调都会往 .coder/attempts 落一个文件。被杀时那里有 3 个
	// (第 3 轮进过、只是没跑完)。接着跑 ⇒ 再多 1 个 = 4; 从头跑 ⇒ 再多 3 个 = 6。
	// 这是唯一能把"接着跑"与"从头跑"分开的读数 —— 两者的轮次编号一模一样。
	attempts, err := os.ReadDir(filepath.Join(ws, RuntimeDir, "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 4 {
		t.Fatalf("agent 一共被调了 %d 次, 期望 4 (被杀前 3 次 + 恢复后 1 次)。"+
			"%d 次意味着恢复进程把它从头跑了一遍\n%s",
			len(attempts), len(attempts), res.Report())
	}
}

// TestCoderDriverHelper 不是测试, 是上面那条测试用的**子进程入口**。
//
// 它只在 CODER_HELPER=1 时干活, 平时立刻返回 —— 这样 `go test ./...` 不会
// 把它当成一个真测试跑。
func TestCoderDriverHelper(t *testing.T) {
	if os.Getenv("CODER_HELPER") != "1" {
		t.Skip("子进程入口, 不是测试")
	}
	ws := os.Getenv("CODER_WS")
	bin := os.Getenv("CODER_BIN")
	res, err := resumeRunner(ws, bin).Run(context.Background())
	fmt.Printf("helper 结束: passed=%v reason=%s err=%v\n", res.Passed, res.Reason, err)
	os.Exit(0)
}

// TestAgentCrashDoesNotKillTask agent 进程崩了 (SIGKILL / 非零退出) 只是**一轮**失败,
// 不该让整个任务停下 —— 下一轮会带着原始报错重来。
//
// 这条与上面那条是一对: 上面杀**驱动**, 这里杀**agent**。两者的正确反应完全不同
// (前者要恢复, 后者要继续), 混为一谈的话会写出"agent 一崩就整个任务失败"的运行时。
func TestAgentCrashDoesNotKillTask(t *testing.T) {
	if testing.Short() {
		t.Skip("起真进程, -short 跳过")
	}
	ws := t.TempDir()
	writeGoModule(t, ws)

	bin := writeFakeAgent(t, `
D="$PWD/.coder/attempts"
mkdir -p "$D"
N=$(( $(ls "$D" 2>/dev/null | wc -l) + 1 ))
: > "$D/$N"
# 第 2 轮被杀 (模拟 OOM / 上游拖死超时)。
if [ "$N" -eq 2 ]; then
  kill -9 $$
fi
# 第 3 轮才写对。
if [ "$N" -ge 3 ]; then
  printf 'package smoke\n\nfunc Greet() string { return "hi" }\n' > "$PWD/greet.go"
else
  printf 'package smoke\n\nfunc Greet() string { return "nope" }\n' > "$PWD/greet.go"
fi
printf '{"final":"第 %s 轮结束","session_id":"fake-%s","is_error":false}\n' "$N" "$N"
`)

	r := &Runner{
		Config: Config{
			Goal: "实现 Greet 函数", Workspace: ws,
			VerifyCmd: []string{"go", "test", "./..."}, Bin: bin,
			MaxTurnsPerIteration: 5, MaxIterations: 8, NoProgressLimit: 3,
			IterationTimeout: 2 * time.Minute, VerifyTimeout: 2 * time.Minute,
		},
		Agent: &ClaudeGoAgent{Bin: bin, Cwd: ws, MaxTurns: 5},
		Verify: CommandVerifier{
			Cmd: []string{"go", "test", "./..."}, Timeout: 2 * time.Minute,
		},
		Logf: func(string, ...any) {},
	}

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("agent 崩了一轮不该让整个任务失败: %v\n报告:\n%s", err, res.Report())
	}
	if !res.Passed {
		t.Fatalf("后续轮次修好了, 应当判为通过\n报告:\n%s", res.Report())
	}
	if len(res.State.History) < 3 {
		t.Fatalf("轮数 = %d, 期望至少 3", len(res.State.History))
	}
	// 崩掉的那一轮要被如实记下来 (退出码非零), 而不是被吞掉。
	if crashed := res.State.History[1]; crashed.ExitCode == 0 {
		t.Fatalf("第 2 轮被杀, 退出码却记成 0 —— 单轮的失败被吞了: %+v", crashed)
	}
}

// TestQuietAgentStopsEarly 静默空转在**真进程**上也会被止损。
//
// 这条是端到端版本的止损验证: 单测里用的是测试替身, 这里走的是真 exec —
// 假 agent 正常退出、正常打 envelope、说的话很像样, 但一个字节不改。
func TestQuietAgentStopsEarly(t *testing.T) {
	if testing.Short() {
		t.Skip("起真进程, -short 跳过")
	}
	ws := t.TempDir()
	writeGoModule(t, ws)

	bin := writeFakeAgent(t, `
D="$PWD/.coder/attempts"
mkdir -p "$D"
N=$(( $(ls "$D" 2>/dev/null | wc -l) + 1 ))
: > "$D/$N"
printf '{"final":"我先看一下项目结构和测试文件。","session_id":"fake-%s","is_error":false}\n' "$N"
`)

	r := &Runner{
		Config: Config{
			Goal: "实现 Greet 函数", Workspace: ws,
			VerifyCmd:            []string{"go", "test", "./..."},
			Bin:                  bin,
			MaxTurnsPerIteration: 5, MaxIterations: 8, NoProgressLimit: 3,
			IterationTimeout: 2 * time.Minute, VerifyTimeout: 2 * time.Minute,
		},
		Agent: &ClaudeGoAgent{Bin: bin, Cwd: ws, MaxTurns: 5},
		Verify: CommandVerifier{
			Cmd: []string{"go", "test", "./..."}, Timeout: 2 * time.Minute,
		},
		Logf: func(string, ...any) {},
	}

	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("agent 一直不改代码, 必须止损报错")
	}
	if res.Passed {
		t.Fatal("什么都没做却判通过")
	}
	if res.Reason != "no_progress" {
		t.Fatalf("结束原因 = %q, 期望 no_progress (报告:\n%s)", res.Reason, res.Report())
	}
	if res.State.Iteration != 3 {
		t.Fatalf("跑到第 %d 轮才停, 期望 3 (NoProgressLimit) —— 多跑的是纯烧额度",
			res.State.Iteration)
	}
	if !strings.Contains(err.Error(), "没有真的动手") {
		t.Fatalf("报错要说清是 agent 没动手: %v", err)
	}
}
