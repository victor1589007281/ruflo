package coder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 测试替身 ────────────────────────────────────────────────────────────────

// scriptVerifier 按调用次数返回预置结果, 并记录每次被调用的目录。
// 它是本包最重要的量具: "谁在什么时候验的"必须能被看见。
type scriptVerifier struct {
	results []bool
	outputs []string
	calls   []string
	i       int
}

func (s *scriptVerifier) Verify(_ context.Context, dir string) (bool, string, error) {
	s.calls = append(s.calls, dir)
	idx := s.i
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	s.i++
	out := ""
	if idx < len(s.outputs) {
		out = s.outputs[idx]
	}
	return s.results[idx], out, nil
}

// recordingAgent 记录每次收到的 prompt, 允许按轮次做副作用。
type recordingAgent struct {
	prompts []string
	fn      func(iter int, prompt string) AgentResult
	// artifacts 记录运行时告诉它该往哪落盘 (判"接线接上了没")。
	artifacts [][2]string
}

func (a *recordingAgent) Run(_ context.Context, prompt string) (AgentResult, error) {
	a.prompts = append(a.prompts, prompt)
	iter := len(a.prompts)
	res := AgentResult{ExitCode: 0, Final: "已完成"}
	if a.fn != nil {
		res = a.fn(iter, prompt)
	}
	return res, nil
}

func (a *recordingAgent) SetArtifacts(promptFile, logDir string) {
	a.artifacts = append(a.artifacts, [2]string{promptFile, logDir})
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func newTestRunner(t *testing.T, ws string, ag Agent, vf Verifier, mut func(*Config)) (*Runner, Config) {
	t.Helper()
	cfg := Config{
		Goal:                 "让 go test ./... 通过",
		Workspace:            ws,
		VerifyCmd:            []string{"go", "test", "./..."},
		MaxIterations:        5,
		MaxTurnsPerIteration: 4,
		NoProgressLimit:      3,
		IterationTimeout:     time.Minute,
		VerifyTimeout:        time.Minute,
	}
	if mut != nil {
		mut(&cfg)
	}
	return &Runner{
		Config: cfg, Agent: ag, Verify: vf,
		Now:   fixedClock(),
		Logf:  func(string, ...any) {},
		Sleep: func(time.Duration) {},
	}, cfg
}

// ── 1. 断点恢复 ─────────────────────────────────────────────────────────────

// TestResumeKeepsCounting 恢复必须是**接着**跑, 不是从头跑。
//
// 从头跑的表现: 轮次计数从 1 重新开始, 且新进程收到的 prompt 里没有上一轮的
// 失败输出 —— agent 于是失去了它唯一能拿到的反馈, 恢复变成"再试一次同样的运气"。
func TestResumeKeepsCounting(t *testing.T) {
	ws := t.TempDir()
	const rawFail = "FAIL\tsmoke\t[match failed] 上一轮留下的原始报错"

	// 造一份"跑到第 3 轮、未完成"的落盘状态。
	seed := State{
		Goal: "让 go test ./... 通过", Workspace: ws,
		VerifyCmd: []string{"go", "test", "./..."},
		Phase:     PhaseCoding, Iteration: 3,
		LastVerifyOutput: rawFail,
		StartedAt:        time.Now(),
	}
	if err := saveState(ws, seed); err != nil {
		t.Fatalf("落盘种子状态失败: %v", err)
	}

	vf := &scriptVerifier{
		results: []bool{false, false, true},
		outputs: []string{rawFail, rawFail, "ok"},
	}
	ag := &recordingAgent{}
	r, _ := newTestRunner(t, ws, ag, vf, nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("跑失败: %v", err)
	}
	if !res.Passed {
		t.Fatal("应判为通过")
	}
	// 恢复后第一轮的编号必须是 4, 不是 1。
	if res.State.History[0].N != 4 {
		t.Fatalf("恢复后首轮编号 = %d, 期望 4 —— 说明它是从头跑的, 前面的进展被丢了",
			res.State.History[0].N)
	}
	// 第一轮喂进去的 prompt 必须带着上一轮的原始报错。
	if len(ag.prompts) == 0 {
		t.Fatal("agent 一次都没被调用")
	}
	if !strings.Contains(ag.prompts[0], "上一轮留下的原始报错") {
		t.Fatalf("恢复后首轮的 prompt 里没有上一轮的原始报错 —— 断点恢复退化成了再试一次\n%s",
			truncate(ag.prompts[0], 1200))
	}
}

// TestResumeMeasuresFirst 恢复的正确性来自"先验一次", 而不是来自相信落盘记录。
//
// 场景: 上一轮其实已经做完了, 但进程在落"完成"标记前被杀。恢复时若不先验,
// 会白烧一整轮; 更糟的是如果那一轮把代码改坏了, 任务会从"完成"退回"未完成"
// 而没人发现。
func TestResumeMeasuresFirst(t *testing.T) {
	ws := t.TempDir()
	seed := State{
		Goal: "让 go test ./... 通过", Workspace: ws,
		VerifyCmd: []string{"go", "test", "./..."},
		Phase:     PhaseCoding, Iteration: 3, // 上一轮被杀, 没来得及记完成
		StartedAt: time.Now(),
	}
	if err := saveState(ws, seed); err != nil {
		t.Fatal(err)
	}
	vf := &scriptVerifier{results: []bool{true}, outputs: []string{"ok"}}
	ag := &recordingAgent{fn: func(int, string) AgentResult {
		// 不能用 t.Fatal: recordingAgent.Run 跑在别的 goroutine 上,
		// 从非测试 goroutine 调 Fatal 是错的。改成返回一个明显的哨兵,
		// 由下面的长度断言来判。
		return AgentResult{Final: "不该被调用"}
	}}
	r, _ := newTestRunner(t, ws, ag, vf, nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("跑失败: %v", err)
	}
	if !res.Passed {
		t.Fatal("应先验一次就发现已完成")
	}
	if len(ag.prompts) != 0 {
		t.Fatalf("起了 %d 轮 agent, 期望 0 —— 没有先测量世界", len(ag.prompts))
	}
	// 第一次验证必须发生在**工作区**上 (先测量世界), 而不是别处。
	if len(vf.calls) == 0 {
		t.Fatal("验证一次都没跑")
	}
	if filepath.Clean(vf.calls[0]) != filepath.Clean(ws) {
		t.Fatalf("第一次验证跑在 %q, 期望工作区 %q", vf.calls[0], ws)
	}
}

// ── 2. 静默空转必须被止损 ───────────────────────────────────────────────────

// TestNoProgressStopsIdleRun 这是本次实测事故的回归测试。
//
// 事故形态 (2026-09-24, opencode:space-bunny-free): 上游对多工具调用回 400,
// 引擎吞掉之后以 exit=0 / is_error=false / final="我先看看项目结构和测试文件。"
// 收尾 —— **一行代码没改, 而所有"成功"信号都是真的**。
//
// 一个只看退出码/自述的运行时会把这种 run 记成"这轮做完了", 一路烧到迭代额度,
// 最后只报一句"没成功", 完全看不出"它其实什么都没做"。
func TestNoProgressStopsIdleRun(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	vf := &scriptVerifier{
		results: []bool{false},
		outputs: []string{"main.go:5: undefined: Greet"},
	}
	// agent: 正常退出、is_error=false、说的话很像样 —— 但什么都不改。
	ag := &recordingAgent{fn: func(int, string) AgentResult {
		return AgentResult{
			ExitCode: 0, IsError: false,
			Final: "我先看一下项目结构和测试文件。", SessionID: "s",
		}
	}}
	r, _ := newTestRunner(t, ws, ag, vf, nil)

	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("整个 run 什么都不做, 必须报错收场")
	}
	if res.Passed {
		t.Fatal("agent 自称完成、但验证没过 —— 判成通过就是把自己报告当判据了")
	}
	if !strings.Contains(err.Error(), "没有真的动手") {
		t.Fatalf("报错必须指名「agent 没有真的动手」, 实得: %v", err)
	}
	// 必须在 NoProgressLimit 处停, 不能烧满 MaxIterations (5)。
	if len(ag.prompts) != 3 {
		t.Fatalf("起了 %d 轮, 期望 3 (NoProgressLimit) —— 多起来的就是纯烧额度",
			len(ag.prompts))
	}
	if res.Reason != "no_progress" {
		t.Fatalf("结束原因 = %q, 期望 no_progress", res.Reason)
	}
}

// TestOnlyRuntimeDirChangesIsNotWork 指纹必须排除运行时自己会写的那些目录。
//
// 每一条都对应一个**实测见过**的"agent 什么都没干, 但工作区变了":
//   - `.coder` 是本运行时自己写的 (state.json / prompt.md / logs);
//   - `.claude-go` 是 **agent 进程自己**写的 (sandboxes/<id>/stdout.log) ——
//     实测跑一次只创建了个 hello.txt 的 run, 工作区里就多出了这个目录。
//
// 任何一条漏掉, "工作区动没动"就恒为真, 空转止损**永远不会触发**。
func TestOnlyRuntimeDirChangesIsNotWork(t *testing.T) {
	for _, rt := range []string{RuntimeDir, ".claude-go"} {
		t.Run(rt, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			vf := &scriptVerifier{results: []bool{false}, outputs: []string{"FAIL"}}
			ag := &recordingAgent{fn: func(iter int, _ string) AgentResult {
				// 只动运行时目录里的东西, 代码一行不改。
				d := filepath.Join(ws, rt, "sub", fmt.Sprint(iter))
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "stdout.log"),
					[]byte(fmt.Sprintf("第 %d 轮\n", iter)), 0o644); err != nil {
					t.Fatal(err)
				}
				return AgentResult{ExitCode: 0, Final: "跑了但没改代码"}
			}}
			r, _ := newTestRunner(t, ws, ag, vf, nil)

			if _, err := r.Run(context.Background()); err == nil {
				t.Fatalf("只改 %s 不算动手, 必须止损", rt)
			}
			if len(ag.prompts) != 3 {
				t.Fatalf("起了 %d 轮, 期望 3 —— 只写 %s 就把指纹改了, 空转止损失效",
					len(ag.prompts), rt)
			}
		})
	}
}

// TestRealEditsAreNotIdle 反面: 动手了但还没成功, 不该被判成空转。
//
// 这条挡的是"止损过猛": 一个只看验证输出的实现会把"连续几轮都失败但一直在收敛"
// 误杀。判据必须是"验证过了 或 工作区变了"。
func TestRealEditsAreNotIdle(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 一直失败, 但每轮都改代码。
	vf := &scriptVerifier{results: []bool{false}, outputs: []string{"FAIL 同样的报错"}}
	ag := &recordingAgent{fn: func(iter int, _ string) AgentResult {
		body := strings.Repeat("// 改动\n", iter) + "package main\n"
		if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return AgentResult{ExitCode: 0, Final: "继续修"}
	}}
	r, cfg := newTestRunner(t, ws, ag, vf, func(c *Config) { c.MaxIterations = 4 })

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("4 轮都没过, 应当以额度用尽收场")
	}
	if !strings.Contains(err.Error(), "迭代额度用尽") {
		t.Fatalf("应判为迭代额度用尽, 而不是被判成空转: %v", err)
	}
	// 必须跑满 4 轮 (每轮都在动), 不能提前止损。
	if len(ag.prompts) != cfg.MaxIterations {
		t.Fatalf("只跑了 %d 轮, 期望满 %d 轮 —— 一直在改代码却被误判成空转",
			len(ag.prompts), cfg.MaxIterations)
	}
}

// TestTimingNoiseIsNotProgress 归一化必须抹掉每轮都不同的耗时。
//
// 注意这条只测了**帮手**, 没测**主循环用没用它** —— 后者由
// TestStopGapSurvivesTimingNoise 覆盖。两者缺一: 只测帮手会得到一条
// "函数没错, 但没人调用它"的假绿。
func TestTimingNoiseIsNotProgress(t *testing.T) {
	a := normalizeOutput("--- FAIL: TestX (0.00s)\nFAIL\tsmoke\t0.002s\n")
	b := normalizeOutput("--- FAIL: TestX (0.13s)\nFAIL\tsmoke\t1.447s\n")
	if a != b {
		t.Fatalf("只有耗时不同却被判成两份不同的输出:\n a=%q\n b=%q", a, b)
	}
	// 反面: 真的不同的输出不能被抹平。
	c := normalizeOutput("--- FAIL: TestY (0.00s)\n")
	if c == a {
		t.Fatal("不同的失败被归一化成同一份 —— 归一化抹过头了")
	}
}

// timingNoiseVerifier 每次调用返回同一份失败, 只有耗时不同 —— 真实 go test 的样子。
type timingNoiseVerifier struct{ n int }

func (v *timingNoiseVerifier) Verify(context.Context, string) (bool, string, error) {
	v.n++
	return false, fmt.Sprintf(
		"--- FAIL: TestGreet (0.%02ds)\nFAIL\tsmoke\t0.%03ds\nmain.go:5: undefined: Greet",
		v.n, v.n), nil
}

// TestStopGapSurvivesTimingNoise 主循环必须**真的**用上归一化。
//
// 这条是冲着一个很容易漏的坑去的: normalizeOutput 单测全绿, 但主循环里如果
// 拿原始字符串比较, 每轮的输出都会因为耗时不同而"看起来变了" —— 空转连击
// 永远归零, 止损完全失效。判据必须落在**跑完整个 run 之后**收敛到哪个分支上。
func TestStopGapSurvivesTimingNoise(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := newTestRunner(t, ws, &recordingAgent{}, &timingNoiseVerifier{}, nil)

	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("agent 一个字节没改, 必须止损")
	}
	if res.Reason != "no_progress" {
		t.Fatalf("结束原因 = %q, 期望 no_progress —— 耗时噪声把空转连击清零了, 止损失效",
			res.Reason)
	}
}

// TestOutputChangeIsProgressOutsideFingerprint 指纹看不见的地方改了, 也算动手。
//
// 这是"空转判据必须同时看指纹与输出"的理由: 指纹**刻意**跳过 vendor/node_modules
// (那是依赖目录, 里面通常有几万个文件)。但依赖被改是真真切切的动手 —— 一条
// 只看指纹的判据会把这种 agent 判成空转并**提前掐死**, 而且掐的时候它正在收敛。
func TestOutputChangeIsProgressOutsideFingerprint(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "vendor", "dep.go"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 验证输出直接由 vendor 里的内容决定: 内容变 → 输出变。
	vf := fileEchoVerifier{rel: filepath.Join("vendor", "dep.go")}
	ag := AgentFunc(func(_ context.Context, _ string) (AgentResult, error) {
		// 只改 vendor 里的东西 (指纹跳过它), 但验证输出确实变了。
		return AgentResult{ExitCode: 0, Final: "更新了依赖"}, nil
	})
	// 让 agent 每轮改 vendor: 用 recordingAgent 的副作用更直观。
	a := &recordingAgent{fn: func(iter int, _ string) AgentResult {
		body := fmt.Sprintf("v%d\n", iter)
		if err := os.WriteFile(filepath.Join(ws, "vendor", "dep.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return AgentResult{ExitCode: 0, Final: "更新了依赖"}
	}}
	_ = ag

	r, cfg := newTestRunner(t, ws, a, vf, func(c *Config) { c.MaxIterations = 4 })
	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("4 轮都没过, 应当以额度用尽收场")
	}
	if !strings.Contains(err.Error(), "迭代额度用尽") {
		t.Fatalf("agent 一直在改 vendor (指纹看不见但输出变了), 却被判成空转提前掐死: %v", err)
	}
	if len(a.prompts) != cfg.MaxIterations {
		t.Fatalf("只跑了 %d 轮, 期望满 %d 轮", len(a.prompts), cfg.MaxIterations)
	}
}

// fileEchoVerifier 把工作区里某个文件的内容当验证输出回吐; 永远判失败。
// 用它来把"工作区变了"与"验证输出变了"这两件事**解耦**开测。
type fileEchoVerifier struct{ rel string }

func (v fileEchoVerifier) Verify(_ context.Context, dir string) (bool, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, v.rel))
	if err != nil {
		return false, "", nil
	}
	return false, "FAIL: " + string(b), nil
}

// ── 3. 自述绝不算判据 ───────────────────────────────────────────────────────

// TestSelfReportIsNotEvidence agent 说"做完了"不算数。
func TestSelfReportIsNotEvidence(t *testing.T) {
	ws := t.TempDir()
	vf := &scriptVerifier{results: []bool{false}, outputs: []string{"FAIL"}}
	ag := &recordingAgent{fn: func(int, string) AgentResult {
		return AgentResult{
			ExitCode: 0, IsError: false,
			Final: "已完成。**测试结果**: `ok smoke 0.002s`, `go test ./...` 通过 ✅",
		}
	}}
	r, _ := newTestRunner(t, ws, ag, vf, nil)
	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("验证一直没过, 必须报错收场")
	}
	if res.Passed {
		t.Fatal("agent 自称测试通过, 而驱动侧验证是失败的 —— 判成通过就是把自我报告当判据")
	}
}

// TestNonZeroExitDoesNotFailTheTask agent 进程崩了但工作其实已经做好, 应当判过。
//
// 判据取在**产物**上而不是进程上: 进程的死法 (被杀/超时/非零退出) 与"代码写没写好"
// 是两件事。只看退出码的实现会在这里误报失败。
func TestNonZeroExitDoesNotFailTheTask(t *testing.T) {
	ws := t.TempDir()
	vf := &scriptVerifier{results: []bool{true}, outputs: []string{"ok smoke"}}
	ag := &recordingAgent{fn: func(int, string) AgentResult {
		// 进程被 SIGKILL, 但工作区里的代码是对的。
		return AgentResult{ExitCode: 137, IsError: true, Final: ""}
	}}
	r, _ := newTestRunner(t, ws, ag, vf, nil)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("工作其实已经做好(验证通过), 不该报错: %v", err)
	}
	if !res.Passed {
		t.Fatal("验证已通过却判失败 —— 判据跑到进程上去了")
	}
}

// ── 4. 环境问题与"还没写好"要分开 ──────────────────────────────────────────

// failingVerifier 模拟"验证命令根本跑不起来" (go 不在 PATH / 工作区没有 go.mod)。
type failingVerifier struct{ err error }

func (f failingVerifier) Verify(context.Context, string) (bool, string, error) {
	return false, "", f.err
}

// TestVerifyErrorStopsInsteadOfIterating 环境问题要停下, 而不是继续迭代。
//
// 把"验证命令起不来"当成 passed=false 会让运行时把这同一个环境错误**重复 N 遍**,
// 每遍都烧一整轮 agent; 最后报一句"没通过", 而真正的故障在环境上。
func TestVerifyErrorStopsInsteadOfIterating(t *testing.T) {
	ws := t.TempDir()
	ag := &recordingAgent{}
	vf := failingVerifier{err: errors.New(`exec: "go": executable file not found in $PATH`)}
	r, _ := newTestRunner(t, ws, ag, vf, nil)

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("验证命令跑不起来必须报错")
	}
	if !strings.Contains(err.Error(), "环境问题") {
		t.Fatalf("报错要指出这是环境问题而不是代码问题: %v", err)
	}
	if len(ag.prompts) != 0 {
		t.Fatalf("验证都跑不起来还起了 %d 轮 agent —— 在把同一个环境错误重复烧额度",
			len(ag.prompts))
	}
}

// TestCommandVerifierSeparatesEnvFromCode 用**真的** CommandVerifier 打这一枪。
//
// 上面那条用的是测试替身, 只证明了 Runner 处理 err 的方式 —— 它**不**证明
// CommandVerifier 在"命令根本不存在"时真的会返回 err。把那一行改成返回 nil,
// 上面那条照样绿 (变异测试实测存活)。所以判据必须落在真身上。
func TestCommandVerifierSeparatesEnvFromCode(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// (1) 命令不存在 ⇒ 必须是 err (环境问题), 不能只是 passed=false。
	v := CommandVerifier{Cmd: []string{"definitely-not-a-real-binary-xyz"}}
	passed, _, err := v.Verify(ctx, dir)
	if err == nil {
		t.Fatal("命令不存在却只报 passed=false —— 环境问题会被当成「代码还没写好」重复烧额度")
	}
	if passed {
		t.Fatal("命令起不来却报 passed=true")
	}

	// (2) 命令跑得起来但退出码非零 ⇒ passed=false 且 err=nil (正常路径)。
	v2 := CommandVerifier{Cmd: []string{"sh", "-c", "echo 'boom'; exit 3"}}
	passed2, out2, err2 := v2.Verify(ctx, dir)
	if err2 != nil {
		t.Fatalf("非零退出是正常路径, 不该报 err: %v", err2)
	}
	if passed2 {
		t.Fatal("退出码 3 却判通过")
	}
	if !strings.Contains(out2, "boom") {
		t.Fatalf("验证输出没被带回来: %q —— 下一轮就没有反馈可给模型", out2)
	}

	// (3) 退出码 0 ⇒ 通过。
	v3 := CommandVerifier{Cmd: []string{"sh", "-c", "echo ok"}}
	passed3, _, err3 := v3.Verify(ctx, dir)
	if err3 != nil || !passed3 {
		t.Fatalf("退出码 0 却判失败: passed=%v err=%v", passed3, err3)
	}

	// (4) 命令在**指定目录**里跑 (不是当前进程的目录)。
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	v4 := CommandVerifier{Cmd: []string{"sh", "-c", "cat marker"}}
	passed4, out4, err4 := v4.Verify(ctx, dir)
	if err4 != nil || !passed4 || !strings.Contains(out4, "x") {
		t.Fatalf("验证没有在指定目录里跑: passed=%v out=%q err=%v", passed4, out4, err4)
	}
}

// TestCommandVerifierTimeoutIsEnvProblem 超时也是环境问题, 不是"代码没写好"。
func TestCommandVerifierTimeoutIsEnvProblem(t *testing.T) {
	v := CommandVerifier{Cmd: []string{"sleep", "5"}, Timeout: 150 * time.Millisecond}
	start := time.Now()
	passed, _, err := v.Verify(context.Background(), t.TempDir())
	if passed {
		t.Fatal("超时却判通过")
	}
	if err == nil {
		t.Fatal("超时必须报 err —— 当成 passed=false 会把死循环的验证命令重复烧 N 轮")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("超时没生效, 跑了 %v", d)
	}
}

// ── 5. 状态落盘 ─────────────────────────────────────────────────────────────

// TestCorruptStateIsNotFatal 状态是可重建的缓存, 不是唯一真相。
//
// 直接 WriteFile 写状态会在"写到一半断电"时留下截断的 JSON。若把"读不出来"
// 当成致命错误, 用户会遇到"任务跑不了了, 而且原因是一个他看不懂的 JSON 错误";
// 正确行为是从头开始 (工作区保留, 验证会重新测量世界)。
func TestCorruptStateIsNotFatal(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, RuntimeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个被截断的状态文件。
	if err := os.WriteFile(statePath(ws), []byte(`{"goal":"让 go test`), 0o644); err != nil {
		t.Fatal(err)
	}

	vf := &scriptVerifier{results: []bool{true}, outputs: []string{"ok"}}
	r, _ := newTestRunner(t, ws, &recordingAgent{}, vf, nil)

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("坏状态不该让任务跑不起来: %v", err)
	}
	if !res.Passed {
		t.Fatal("应从零开始并把已经通过的任务判为通过")
	}
}

// TestStateSavedAtomically 状态文件写完之后必须没有残留的 .tmp。
func TestStateSavedAtomically(t *testing.T) {
	ws := t.TempDir()
	st := State{Goal: "g", Workspace: ws, Phase: PhaseCoding}
	if err := saveState(ws, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath(ws) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("落盘后残留了 .tmp 文件 —— rename 没被用上")
	}
	got, existed, err := LoadState(ws)
	if err != nil || !existed {
		t.Fatalf("落盘的状态读不回来: existed=%v err=%v", existed, err)
	}
	if got.Goal != "g" {
		t.Fatalf("读回来的 goal = %q", got.Goal)
	}
}

// TestMissingStateIsColdStart 没有状态文件是正常冷启动, 不是错误。
func TestMissingStateIsColdStart(t *testing.T) {
	ws := t.TempDir()
	_, existed, err := LoadState(ws)
	if err != nil {
		t.Fatalf("没有状态文件应当不是错误: %v", err)
	}
	if existed {
		t.Fatal("没有状态文件却报 existed=true")
	}
}

// ── 6. 终态验收 ─────────────────────────────────────────────────────────────

// cleanAwareVerifier 在原目录里判过, 在别处判不过 —— 模拟"依赖了运行时目录"。
type cleanAwareVerifier struct{ ws string }

func (c *cleanAwareVerifier) Verify(_ context.Context, dir string) (bool, string, error) {
	if filepath.Clean(dir) == filepath.Clean(c.ws) {
		return true, "ok (原目录)", nil
	}
	return false, "FAIL: 找不到 .coder/plan.md", nil
}

// TestFinalizeRejectsRuntimeDependency 产物依赖 .coder ⇒ 这个程序不能独立运行。
//
// 用户要的是"最后的程序可以正常运行"。在原目录里跑验证是**不足以**证明这件事的:
// 那个目录里有 agent 的会话、日志、临时产物, 验证可能悄悄依赖了它们。
func TestFinalizeRejectsRuntimeDependency(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 第一次 (原目录) 过; 之后在干净副本里跑时过不了。
	vf := &cleanAwareVerifier{ws: ws}
	r, _ := newTestRunner(t, ws, &recordingAgent{}, vf, nil)

	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("干净副本里没通过, 必须报错")
	}
	if res.Passed {
		t.Fatal("原目录过、干净副本不过 ⇒ 不能判为「程序可以正常运行」")
	}
	if !strings.Contains(err.Error(), "干净副本") {
		t.Fatalf("报错必须指名干净副本: %v", err)
	}
	if res.Reason != "clean_verify_failed" {
		t.Fatalf("结束原因 = %q, 期望 clean_verify_failed", res.Reason)
	}
}

// TestExportCleanExcludesRuntimeDir 导出副本里不能有 .coder / .git。
func TestExportCleanExcludesRuntimeDir(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{RuntimeDir, ".git"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, d, "x"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := t.TempDir()
	if err := ExportClean(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.go", "go.mod"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Fatalf("副本里缺少 %s: %v", f, err)
		}
	}
	for _, d := range []string{RuntimeDir, ".git"} {
		if _, err := os.Stat(filepath.Join(dst, d)); !os.IsNotExist(err) {
			t.Fatalf("副本里不该有 %s", d)
		}
	}
}

// ── 7. prompt 组装 ──────────────────────────────────────────────────────────

// TestPromptPutsVaryingTextLast 前缀稳定是提示缓存能命中的前提。
//
// 判据是**首个不同字节的位置**: 它必须落在最后那一段 (上一轮的原始输出) 里。
// 只要变化段出现在中间, 它后面的一切每轮都会被重算, 长程任务里这个代价随轮数
// 线性累积。
func TestPromptPutsVaryingTextLast(t *testing.T) {
	cfg := Config{Goal: "目标文本", VerifyCmd: []string{"go", "test", "./..."}}
	p1 := buildPrompt(cfg, State{}, "FAIL 第一轮的报错", false)
	p2 := buildPrompt(cfg, State{}, "FAIL 第二轮的报错", false)

	i := 0
	for i < len(p1) && i < len(p2) && p1[i] == p2[i] {
		i++
	}
	// 判据: 首个不同字节的位置必须落在"变化段标题"**之后**。
	// 换成"变化段的标题必须还在稳定前缀里"更好量 —— 标题属于每一轮都一样的那部分,
	// 它后面才允许出现逐轮变化的正文。
	const head = "# 上一轮验证命令的原始输出"
	idxHead := strings.Index(p1, head)
	if idxHead < 0 {
		t.Fatalf("prompt 里找不到变化段的标题 %q:\n%s", head, truncate(p1, 800))
	}
	if i <= idxHead+len(head) {
		t.Fatalf("首个不同字节出现在第 %d 字节, 而变化段的标题在第 %d 字节 —— "+
			"变化段被放到了中间, 它后面的一切每轮都会重算, 前缀缓存命中不了",
			i, idxHead)
	}
	// 稳定前缀必须已经包含最后一个稳定段落 (工作方式)。
	if !strings.Contains(p1[:i], "# 工作方式") {
		t.Fatalf("首个不同字节出现在第 %d 字节, 但它之前还没走完稳定段落", i)
	}
	// 目标必须逐字出现在最前面。
	if !strings.HasPrefix(p1, "# 任务\n\n目标文本") {
		t.Fatalf("目标没有放在最前面:\n%s", truncate(p1, 300))
	}
	// 判据命令必须出现在 prompt 里 —— 模型得知道什么算完成。
	if !strings.Contains(p1, "go test ./...") {
		t.Fatal("prompt 里没有完成判据命令")
	}
}

// TestPromptFirstRunSaysNoHistory 首轮说"上一轮失败了"会让模型去找不存在的历史。
func TestPromptFirstRunSaysNoHistory(t *testing.T) {
	cfg := Config{Goal: "g", VerifyCmd: []string{"true"}}
	p := buildPrompt(cfg, State{}, "", true)
	if !strings.Contains(p, "还没有历史") {
		t.Fatalf("首轮 prompt 应当明说没有历史:\n%s", truncate(p, 800))
	}
}

// ── 8. envelope 解析 ────────────────────────────────────────────────────────

// TestParseEnvelopeWithPrefixLines stdout 里混着非 JSON 行是常态 (实测有
// "[computer-use] 已注册 1 个工具")。整段 Unmarshal 会在这里失败。
func TestParseEnvelopeWithPrefixLines(t *testing.T) {
	out := "[computer-use] 已注册 1 个工具 (平台: linux, 截图: , 输入: )\n" +
		`{"final":"我做完了","session_id":"abc123","is_error":false}` + "\n"
	env, ok := parseEnvelope(out)
	if !ok {
		t.Fatal("有前缀行就解析不出 envelope —— 那会把每次 run 都判成没有读数")
	}
	if env.SessionID != "abc123" || env.IsError {
		t.Fatalf("解析结果不对: %+v", env)
	}
}

// TestParseEnvelopeIsErrorOnly 模型输出里的花括号不算 envelope; is_error 单独出现
// 也要能认出来。
func TestParseEnvelopeIsErrorOnly(t *testing.T) {
	out := "模型说: {\"a\": 1}\n" +
		`{"final":"","session_id":"","is_error":true}` + "\n"
	env, ok := parseEnvelope(out)
	if !ok {
		t.Fatal("应当认出真正的 envelope")
	}
	if !env.IsError {
		t.Fatal("is_error=true 没被读出来 —— 判据不能只认 session_id/final")
	}
}

// TestParseEnvelopeAbsent 没有 envelope 时要如实报 false, 不能伪造一个空的。
func TestParseEnvelopeAbsent(t *testing.T) {
	if _, ok := parseEnvelope("只有普通输出\n没有 JSON 行\n"); ok {
		t.Fatal("没有 envelope 却报 ok=true")
	}
}

// ── 9. 报告是交付物 ─────────────────────────────────────────────────────────

// TestReportShowsPerRoundReadings 报告要能回答"它到底动没动手"。
func TestReportShowsPerRoundReadings(t *testing.T) {
	res := Result{
		State: State{
			Goal: "g", VerifyCmd: []string{"go", "test", "./..."}, Iteration: 2,
			History: []IterationRecord{
				{N: 1, VerifyPassed: false, FingerprintChanged: false, ExitCode: 0},
				{N: 2, VerifyPassed: true, FingerprintChanged: true, ExitCode: 0},
			},
		},
		Passed: true, Reason: "done",
	}
	rep := res.Report()
	for _, want := range []string{"逐轮读数", "#1", "#2", "空转"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("报告里缺少 %q:\n%s", want, rep)
		}
	}
}

// TestDidWorkPredicate 这条判据同时被止损与报告使用。
func TestDidWorkPredicate(t *testing.T) {
	cases := []struct {
		rec  IterationRecord
		want bool
	}{
		{IterationRecord{VerifyPassed: true, FingerprintChanged: false}, true},
		{IterationRecord{VerifyPassed: false, FingerprintChanged: true}, true},
		{IterationRecord{VerifyPassed: true, FingerprintChanged: true}, true},
		{IterationRecord{VerifyPassed: false, FingerprintChanged: false}, false},
	}
	for _, c := range cases {
		if got := c.rec.DidWork(); got != c.want {
			t.Fatalf("DidWork(%+v) = %v, 期望 %v", c.rec, got, c.want)
		}
	}
}

// TestStateRoundTrip 落盘/读回必须不丢字段 (history 尤其, 它是报告的数据源)。
func TestStateRoundTrip(t *testing.T) {
	ws := t.TempDir()
	st := State{
		Goal: "g", Workspace: ws, VerifyCmd: []string{"a", "b"},
		Phase: PhaseCoding, Iteration: 5, NoProgressStreak: 2,
		History: []IterationRecord{{N: 1, ExitCode: 0, VerifyPassed: false}},
	}
	if err := saveState(ws, st); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadState(ws)
	if err != nil || !ok {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Iteration != 5 || got.NoProgressStreak != 2 || len(got.History) != 1 {
		b, _ := json.Marshal(got)
		t.Fatalf("字段丢失: %s", b)
	}
	if len(got.VerifyCmd) != 2 {
		t.Fatalf("VerifyCmd 丢了: %v", got.VerifyCmd)
	}
}
