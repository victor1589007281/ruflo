package worker

// gitws_test.go —— cwd 的 git 档位。
//
// 全部用**真的 git** (真裸仓 + 真 clone + 真 push/rebase) 与**真的控制面 + worker
// 循环** (httptest + cluster 端点 + Broker)。不 mock git: 这一档的全部风险都在
// "被拒的 push / 冲突的 rebase / 快进不成立的 merge"这些真实 git 行为上, mock 掉
// 就等于没测。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

const testBranch = "claude-go/ws/t1"

// gitPolicy 一份 git 档策略 (分支模板按团队 → 团队名固定为 t1 ⇒ testBranch)。
func gitPolicy(bare string, maxFile int64) *WorkspacePolicy {
	return &WorkspacePolicy{Mode: WorkspaceModeGit, GitRemote: bare, GitMaxFileBytes: maxFile}
}

// stage 跑完整一个远程阶段: 控制面派任务 → 指定 worker 真跑 → 返回终态事件。
//
// 每个阶段起一个**独立的 worker 进程语义** (独立的本地检出目录), 于是"下一阶段
// 在另一台机器上"这件事是真的被验证的, 而不是同一个目录里的假接力。
func stage(t *testing.T, tc *testControl, workerName, workerDir, ctlDir, node string,
	rt agent.AgentRuntime, timeout time.Duration) agent.NodeEvent {
	t.Helper()
	remote := tc.brk.Runtime(workerName, agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r1", NodeID: node, Role: "coder", UserPrompt: "写代码", Workspace: ctlDir,
		Placement: &agent.Placement{Affinity: "team", AffinityKey: "t1"},
	})
	if err != nil {
		t.Fatalf("派任务失败: %v", err)
	}
	_, cancel := tc.startWorker(t, Options{Name: workerName, Runtime: rt,
		Workspace: workerDir, WorkspaceMode: WorkspaceModeGit})
	defer cancel() // 阶段结束就停掉这个 worker, 免得它抢下一阶段的任务
	return terminal(t, drain(t, ch, timeout))
}

// git 档第一阶段: 远端是空裸仓 (真实的起步状态), 产物必须落到约定位置, 并被控制面
// 同步进团队 cwd —— 门禁就在那个目录里跑。
func TestGit_首阶段在空裸仓起步并推回控制面(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	rt := &fileWriterRuntime{files: map[string]string{
		"go.mod":  "module demo\n\ngo 1.22\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}, out: "第一阶段完成"}

	term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", rt, 60*time.Second)
	if term.Kind != agent.NodeEventDone {
		t.Fatalf("git 档首阶段应成功, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
	if got := gitLsTree(t, bare, testBranch); len(got) != 2 {
		t.Fatalf("裸仓 %s 上的文件 = %v", testBranch, got)
	}
	// 控制面侧: 文件真的到了团队 cwd (这一步以前是断的 —— 远程写的文件门禁看不见)。
	if b, err := os.ReadFile(filepath.Join(ctlDir, "main.go")); err != nil {
		t.Fatalf("控制面同步后应能读到 main.go: %v", err)
	} else if !strings.Contains(string(b), "func main") {
		t.Fatalf("控制面 main.go 内容 = %q", string(b))
	}
	if br := gitOut(t, ctlDir, "rev-parse", "--abbrev-ref", "HEAD"); br != testBranch {
		t.Errorf("控制面应停在工作区分支上, 实得 %q", br)
	}
}

// ★ 本轮要补的那条断链: 下一阶段 (在另一台 worker 上) 必须看得到上一阶段的代码,
// 且控制面的**编译门禁 (go build ./... on <team.Cwd>) 真的能编过**。
func TestGit_下一阶段拉到上一阶段的代码且门禁看得见(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})

	rt1 := &fileWriterRuntime{files: map[string]string{
		"go.mod": "module demo\n\ngo 1.22\n",
		"add.go": "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
	}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", rt1, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("阶段1 失败: %s %q", term.Kind, term.Err)
	}

	// 阶段2 在**另一个 worker、另一个本地目录**上跑: 它必须能读到阶段1 的 add.go,
	// 否则 check 让阶段失败 —— 与生产症状完全一致。
	rt2 := &fileWriterRuntime{
		check: func(dir string) error {
			b, err := os.ReadFile(filepath.Join(dir, "add.go"))
			if err != nil {
				return fmt.Errorf("看不到上一阶段的 add.go: %w", err)
			}
			if !strings.Contains(string(b), "func Add") {
				return fmt.Errorf("add.go 内容不对: %q", string(b))
			}
			return nil
		},
		files: map[string]string{
			"add_test_helper.go": "package demo\n\nfunc Triple(x int) int { return Add(Add(x, x), x) }\n",
		},
	}
	if term := stage(t, tc, "w2", emptyDir(t, "w2"), ctlDir, "review", rt2, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("阶段2 失败 (跨机接力断了): %s %q", term.Kind, term.Err)
	}
	if got := gitLsTree(t, bare, testBranch); len(got) != 3 {
		t.Fatalf("裸仓上应有 3 个文件, 实得 %v", got)
	}

	// 编译门禁: 与 pkg/agent/teams.go:1105 一字不差的命令与目录。
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = ctlDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("控制面编译门禁应通过 (这正是跨机产码要的东西): %v\n%s", err, out)
	}
	t.Logf("控制面 %s 上 go build ./... 通过, 文件 = %v", ctlDir, gitLsTree(t, bare, testBranch))
}

// ★ 真机验证抓到的坑之三: 同一个约定 git 位置上跑**第二个团队**时, 远端已经有
// 别的团队的分支, 而 origin/HEAD 在 init+fetch 流程里从来不会被设置 (那是 clone 才
// 写的)。此时新分支必须能从空树起步 (不删任何东西), 而不是被"远端有内容却猜不出
// 起点"挡死 —— 那一版直接把第二个团队卡成 6 次重试全失败。
func TestGit_第二个团队在已有他人分支的远端上起空树(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir1 := emptyDir(t, "control1")
	tc1 := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	// **同一个 worker 目录**先后服务两个团队 (生产里就是这样: 一个 worker 进程一个 cwd)。
	wdir := emptyDir(t, "w-shared")
	first := &fileWriterRuntime{files: map[string]string{"team1.go": "package t1\n"}}
	if term := stage(t, tc1, "w1", wdir, ctlDir1, "impl", first, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("团队1 失败: %s %q", term.Kind, term.Err)
	}

	// 第二个团队: 换一个 AffinityKey ⇒ 换一条分支。这里直接用另一个策略实例, 分支
	// 模板同款, 团队名不同。
	ctlDir2 := emptyDir(t, "control2")
	tc2 := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModeGit,
		GitRemote: bare, GitBranch: "claude-go/ws/t2"}})
	second := &fileWriterRuntime{
		// 团队1 的文件绝不能出现在团队2 的工作区里 (跨团队串味)。
		check: func(dir string) error {
			if _, err := os.Stat(filepath.Join(dir, "team1.go")); err == nil {
				return fmt.Errorf("工作区里还留着上一个团队的 team1.go")
			}
			return nil
		},
		files: map[string]string{"team2.go": "package t2\n"},
	}
	remote := tc2.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r2", NodeID: "impl", Role: "coder", UserPrompt: "x", Workspace: ctlDir2,
		Placement: &agent.Placement{Affinity: "team", AffinityKey: "t2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := tc2.startWorker(t, Options{Name: "w1", Runtime: second,
		Workspace: wdir, WorkspaceMode: WorkspaceModeGit})
	defer cancel()
	if term := terminal(t, drain(t, ch, 60*time.Second)); term.Kind != agent.NodeEventDone {
		t.Fatalf("第二个团队应能在同一个远端上起步, 实得 %s %q", term.Kind, term.Err)
	}
	// 两条分支各自独立, 谁都没被删。
	if got := gitLsTree(t, bare, "claude-go/ws/t1"); strings.Join(got, ",") != "team1.go" {
		t.Errorf("团队1 的分支被动了: %v", got)
	}
	if got := gitLsTree(t, bare, "claude-go/ws/t2"); strings.Join(got, ",") != "team2.go" {
		t.Errorf("团队2 的分支内容 = %v", got)
	}
}

// 显式基线 (--workspace-git-base) 必须被采纳: 新团队从产品主干起步。
func TestGit_显式基线起点被采纳(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	// 先在远端造一条"产品主干" main。
	seed := emptyDir(t, "seed")
	gitOut(t, seed, "init", "-q")
	gitOut(t, seed, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(seed, "product.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, seed, "checkout", "-q", "-B", "main")
	gitOut(t, seed, "add", "-A")
	gitOut(t, seed, "commit", "-q", "-m", "产品主干")
	gitOut(t, seed, "push", "-q", "origin", "main")

	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModeGit,
		GitRemote: bare, GitBaseRef: "main"}})
	rt := &fileWriterRuntime{
		check: func(dir string) error {
			if _, err := os.Stat(filepath.Join(dir, "product.go")); err != nil {
				return fmt.Errorf("应以 main 为基线起步, 但看不到 product.go: %w", err)
			}
			return nil
		},
		files: map[string]string{"feature.go": "package p\n\nvar F = 1\n"},
	}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", rt, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("以显式基线起步的阶段应成功: %s %q", term.Kind, term.Err)
	}
	got := gitLsTree(t, bare, testBranch)
	if len(got) != 2 {
		t.Fatalf("工作区分支应同时含基线与新文件, 实得 %v", got)
	}
}

// 并发分片改**不同文件**: push 被拒后自动变基合入, 两边的提交都在, 谁都没丢。
func TestGit_并发分片不同文件变基后都保留(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	// 先有一个基线提交, 两个分片都从它出发。
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w0", emptyDir(t, "w0"), ctlDir, "base", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}

	// 分片 A 卡在执行中间, 分片 B 完整跑完并推送 → A 再推就必然被拒。
	gate := make(chan struct{})
	startedA := make(chan struct{})
	rtA := &fileWriterRuntime{files: map[string]string{"a.go": "package demo\n\nvar A = 1\n"},
		gate: gate, started: startedA}
	rtB := &fileWriterRuntime{files: map[string]string{"b.go": "package demo\n\nvar B = 2\n"}}

	remoteA := tc.brk.Runtime("wa", agent.RuntimeCaps{Bash: true})
	chA, err := remoteA.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r1", NodeID: "shard-a", Role: "coder", UserPrompt: "A", Workspace: ctlDir,
		Placement: &agent.Placement{Affinity: "team", AffinityKey: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, cancelA := tc.startWorker(t, Options{Name: "wa", Runtime: rtA,
		Workspace: emptyDir(t, "wa"), WorkspaceMode: WorkspaceModeGit})
	defer cancelA()
	select {
	case <-startedA:
	case <-time.After(30 * time.Second):
		t.Fatal("分片 A 未开始执行")
	}

	if term := stage(t, tc, "wb", emptyDir(t, "wb"), ctlDir, "shard-b", rtB, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("分片 B 失败: %s %q", term.Kind, term.Err)
	}
	close(gate) // 放 A 走: 它的 push 会被拒 → fetch + rebase → 再推

	termA := terminal(t, drain(t, chA, 60*time.Second))
	if termA.Kind != agent.NodeEventDone {
		t.Fatalf("分片 A 应在变基后成功, 实得 %s %q", termA.Kind, termA.Err)
	}
	files := gitLsTree(t, bare, testBranch)
	if len(files) != 3 {
		t.Fatalf("变基后分支上应同时有 a.go 与 b.go, 实得 %v", files)
	}
	t.Logf("并发变基后 %s 上的文件 = %v", testBranch, files)
}

// 并发分片改**同一文件同一处**: 不许静默覆盖也不许静默丢弃 —— 阶段必须失败并指出
// 冲突文件, 而且先到的提交还在分支上。
func TestGit_并发分片同文件冲突必须失败且不覆盖(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	base := &fileWriterRuntime{files: map[string]string{"shared.go": "package demo\n\nvar V = 0\n"}}
	if term := stage(t, tc, "w0", emptyDir(t, "w0"), ctlDir, "base", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}
	baseHead := gitOut(t, bare, "rev-parse", testBranch)

	gate := make(chan struct{})
	startedA := make(chan struct{})
	rtA := &fileWriterRuntime{files: map[string]string{"shared.go": "package demo\n\nvar V = 1 // A\n"},
		gate: gate, started: startedA}
	rtB := &fileWriterRuntime{files: map[string]string{"shared.go": "package demo\n\nvar V = 2 // B\n"}}

	remoteA := tc.brk.Runtime("wa", agent.RuntimeCaps{Bash: true})
	chA, err := remoteA.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r1", NodeID: "shard-a", Role: "coder", UserPrompt: "A", Workspace: ctlDir,
		Placement: &agent.Placement{Affinity: "team", AffinityKey: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, cancelA := tc.startWorker(t, Options{Name: "wa", Runtime: rtA,
		Workspace: emptyDir(t, "wa"), WorkspaceMode: WorkspaceModeGit})
	defer cancelA()
	select {
	case <-startedA:
	case <-time.After(30 * time.Second):
		t.Fatal("分片 A 未开始执行")
	}
	if term := stage(t, tc, "wb", emptyDir(t, "wb"), ctlDir, "shard-b", rtB, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("分片 B 失败: %s %q", term.Kind, term.Err)
	}
	bHead := gitOut(t, bare, "rev-parse", testBranch)
	close(gate)

	termA := terminal(t, drain(t, chA, 60*time.Second))
	if termA.Kind != agent.NodeEventFailed {
		t.Fatalf("同文件冲突必须让阶段失败 (不许静默覆盖), 实得 %s", termA.Kind)
	}
	if !strings.Contains(termA.Err, "冲突") || !strings.Contains(termA.Err, "shared.go") {
		t.Errorf("失败原因必须点明冲突文件, 实得 %q", termA.Err)
	}
	// 先到者的提交还在, 且没有被 A 强推覆盖。
	if now := gitOut(t, bare, "rev-parse", testBranch); now != bHead {
		t.Errorf("分支被动过: 期望停在 B 的提交 %s, 实得 %s", shortSHA(bHead), shortSHA(now))
	}
	if bHead == baseHead {
		t.Error("B 的提交没上去, 这个测试就没验到冲突")
	}
	if b := gitOut(t, ctlDir, "show", testBranch+":shared.go"); !strings.Contains(b, "// B") {
		t.Errorf("远端内容应是 B 的版本, 实得 %q", b)
	}
}

// 拉取失败必须真失败, 且**根本不执行** —— 否则就是在一个没有上游代码的目录里
// "成功"跑完, 正是本包首要要挡的故障。
func TestGit_拉取失败必须真失败且不执行(t *testing.T) {
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{
		ws: gitPolicy(filepath.Join(t.TempDir(), "does-not-exist.git"), 0)})
	rt := &fileWriterRuntime{files: map[string]string{"a.go": "package a"}}
	term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", rt, 60*time.Second)
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("远端不可达时必须失败, 实得 %s", term.Kind)
	}
	if !strings.Contains(term.Err, "拉取失败") {
		t.Errorf("失败原因应点明拉取失败: %q", term.Err)
	}
	if rt.called() != 0 {
		t.Error("拉取失败时不该调执行体 (那会在没有上游代码的目录里产码)")
	}
}

// 没有文件改动就不造提交 (评审/分析类阶段的正常情况), 也不许报"推送了"。
func TestGit_无改动不做空提交(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}
	head := gitOut(t, bare, "rev-parse", testBranch)

	review := &fileWriterRuntime{out: "评审意见: 没问题"} // 不写任何文件
	term := stage(t, tc, "w2", emptyDir(t, "w2"), ctlDir, "review", review, 60*time.Second)
	if term.Kind != agent.NodeEventDone || term.Output != "评审意见: 没问题" {
		t.Fatalf("无产出的阶段应正常成功, 实得 %s %q", term.Kind, term.Err)
	}
	if now := gitOut(t, bare, "rev-parse", testBranch); now != head {
		t.Errorf("无改动的阶段不该产生提交: %s → %s", shortSHA(head), shortSHA(now))
	}
	if n := gitOut(t, bare, "rev-list", "--count", testBranch); n != "1" {
		t.Errorf("分支上应仍只有 1 个提交, 实得 %s", n)
	}
}

// 大文件超限: 阶段**失败**并列出路径, 而不是悄悄跳过 (跳过 = 下一阶段找不到它)。
func TestGit_超限大文件拒绝提交且阶段失败(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 4096)}) // 4 KiB 上限
	rt := &fileWriterRuntime{files: map[string]string{
		"small.go": "package demo\n",
		"big.bin":  strings.Repeat("x", 8192),
	}}
	term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", rt, 60*time.Second)
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("超限大文件必须让阶段失败, 实得 %s", term.Kind)
	}
	if !strings.Contains(term.Err, "big.bin") || !strings.Contains(term.Err, "上限") {
		t.Errorf("失败原因应列出超限文件: %q", term.Err)
	}
	// 什么都不该被推上去 (否则就是"半个阶段的产物进了主线")。
	if out, err := gitCmd(context.Background(), bare, 10*time.Second,
		"rev-parse", "--verify", "-q", testBranch); err == nil {
		t.Errorf("大文件被拒时分支不该被创建, 实得 %s", out)
	}
}

// worker 的工作区被指到一个非空的非仓目录: 拒绝在其上初始化 (会与既有文件撞车)。
func TestGit_目录非空且非仓库拒绝初始化(t *testing.T) {
	dir := emptyDir(t, "dirty")
	if err := os.WriteFile(filepath.Join(dir, "someone-elses.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &Worker{opt: Options{Name: "w", Workspace: dir, WorkspaceMode: WorkspaceModeGit,
		Logf: func(string, ...any) {}}}
	_, err := w.prepareWorkspace(StageTask{WorkspaceMode: WorkspaceModeGit,
		Git: &GitWorkspace{Remote: newBareRepo(t), Branch: testBranch}})
	if err == nil {
		t.Fatal("非空非仓目录应拒绝初始化")
	}
	if !strings.Contains(err.Error(), "非空") {
		t.Errorf("失败原因应点明目录非空: %v", err)
	}
	// origin 指向别的仓时也不许静默改指向。
	repo := emptyDir(t, "repo")
	other := newBareRepo(t)
	if err := gitEnsureRepo(context.Background(), repo, other, false); err != nil {
		t.Fatal(err)
	}
	if err := gitEnsureRepo(context.Background(), repo, newBareRepo(t), false); err == nil {
		t.Error("origin 指向不同仓库时应拒绝")
	}
}

// 控制面工作区有本地提交 (人工介入/本地阶段的产物) 时: ff-only 不成立必须诚实失败,
// 不许 reset --hard 把人的东西毁掉。
func TestGit_控制面非快进拒绝静默重置(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}
	// 控制面侧造一个本地提交 (与远端分叉)。
	if err := os.WriteFile(filepath.Join(ctlDir, "human.txt"), []byte("我手改的\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, ctlDir, "add", "-A")
	gitOut(t, ctlDir, "commit", "-q", "-m", "人工介入")
	localHead := gitOut(t, ctlDir, "rev-parse", "HEAD")

	next := &fileWriterRuntime{files: map[string]string{"more.go": "package demo\n\nvar X = 1\n"}}
	term := stage(t, tc, "w2", emptyDir(t, "w2"), ctlDir, "impl2", next, 60*time.Second)
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("控制面无法快进时应判失败, 实得 %s", term.Kind)
	}
	if !strings.Contains(term.Err, "快进") {
		t.Errorf("失败原因应点明快进不成立: %q", term.Err)
	}
	if now := gitOut(t, ctlDir, "rev-parse", "HEAD"); now != localHead {
		t.Errorf("控制面本地提交被动了: %s → %s", shortSHA(localHead), shortSHA(now))
	}
	if _, err := os.Stat(filepath.Join(ctlDir, "human.txt")); err != nil {
		t.Errorf("人工改的文件不该被毁: %v", err)
	}
}

// worker 回报"推了某个提交"但控制面同步后找不到它 (推到了别处/协议漂移):
// 产物对门禁不可见, 不许当成功。
func TestGit_报了推送但控制面找不到该提交必须失败(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}

	// 造一个"假 worker": 走队列正常完成, 回报一个不存在的提交。
	remote := tc.brk.Runtime("w9", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r1", NodeID: "fake", Role: "coder", UserPrompt: "x", Workspace: ctlDir,
		Placement: &agent.Placement{Affinity: "team", AffinityKey: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := tc.q.List()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, tk := range tasks {
		if tk.Status == "pending" {
			id = tk.ID
		}
	}
	if id == "" {
		t.Fatal("没找到待执行的任务")
	}
	if _, _, err := tc.q.PullFor("w9", nil, []string{WorkerCap("w9"), "ws:git"}); err != nil {
		t.Fatal(err)
	}
	res := NewStageResult("w9", "我推了", 1)
	res.Workspace = &WorkspaceReport{Mode: WorkspaceModeGit, Changed: true, Pushed: true,
		Commit: "0123456789012345678901234567890123456789"}
	if err := tc.q.Complete(id, "w9", mustJSON(t, res)); err != nil {
		t.Fatal(err)
	}
	term := terminal(t, drain(t, ch, 30*time.Second))
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("找不到回报的提交时必须判失败, 实得 %s output=%q", term.Kind, term.Output)
	}
	if !strings.Contains(term.Err, "找不到") {
		t.Errorf("失败原因应点明提交不可达: %q", term.Err)
	}
}

// worker 说有改动却没推上去: 产物对下一阶段与门禁都不可见, 不许当成功。
func TestGit_报有改动但未推送必须失败(t *testing.T) {
	bare := newBareRepo(t)
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	remote := tc.brk.Runtime("w9", agent.RuntimeCaps{Bash: true})
	// 控制面无团队 cwd: 此时不同步、也无从校验提交, 但"有改动却没推"这条仍必须挡住。
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r1", NodeID: "n", Role: "coder", UserPrompt: "x",
		Placement: &agent.Placement{Affinity: "team", AffinityKey: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := firstTaskID(t, tc)
	if _, _, err := tc.q.PullFor("w9", nil, []string{WorkerCap("w9"), "ws:git"}); err != nil {
		t.Fatal(err)
	}
	res := NewStageResult("w9", "写完了", 1)
	res.Workspace = &WorkspaceReport{Mode: WorkspaceModeGit, Changed: true, Files: 3, Pushed: false}
	if err := tc.q.Complete(id, "w9", mustJSON(t, res)); err != nil {
		t.Fatal(err)
	}
	term := terminal(t, drain(t, ch, 30*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "没推上去") {
		t.Fatalf("期望因未推送而失败, 实得 %s err=%q", term.Kind, term.Err)
	}
}

// ★ 真机验证抓到的坑之一: 执行体 (Agent 有 Bash) 常自己 `git add && git commit`。
// 那时 `git add -A` 什么都暂存不到, 若据此判"无改动不推送", 它的提交就永远留在
// worker 本地 —— 下一阶段与门禁都看不见, 正是本包要挡的静默丢失。
func TestGit_执行体自行提交也必须推上去(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}
	head := gitOut(t, bare, "rev-parse", testBranch)

	// 这个"执行体"像真 Agent 一样自己提交, 什么都不留在暂存区/工作树。
	selfCommit := &fileWriterRuntime{
		files: map[string]string{"self.go": "package demo\n\nvar Self = true\n"},
		check: func(string) error { return nil },
	}
	wdir := emptyDir(t, "w2")
	selfCommit.after = func(dir string) error {
		if _, err := gitCmd(context.Background(), dir, 30*time.Second, "add", "-A"); err != nil {
			return err
		}
		_, err := gitCmd(context.Background(), dir, 30*time.Second, "commit", "-q", "-m", "执行体自己提交的")
		return err
	}
	term := stage(t, tc, "w2", wdir, ctlDir, "impl2", selfCommit, 60*time.Second)
	if term.Kind != agent.NodeEventDone {
		t.Fatalf("阶段应成功, 实得 %s %q", term.Kind, term.Err)
	}
	if now := gitOut(t, bare, "rev-parse", testBranch); now == head {
		t.Fatal("执行体自行创建的提交没被推上去 (下一阶段将看不到它)")
	}
	if got := gitLsTree(t, bare, testBranch); !strings.Contains(strings.Join(got, ","), "self.go") {
		t.Fatalf("远端应包含执行体提交的文件, 实得 %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(ctlDir, "self.go")); err != nil || !strings.Contains(string(b), "Self") {
		t.Fatalf("控制面同步后应能看到它: %v", err)
	}
}

// 执行体把工作区切到别的分支/游离 HEAD: 拒绝推送不确定的内容 (也不丢弃本地改动)。
func TestGit_执行体切走分支拒绝推送(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}
	head := gitOut(t, bare, "rev-parse", testBranch)

	rogue := &fileWriterRuntime{files: map[string]string{"x.go": "package demo\n"}}
	rogue.after = func(dir string) error {
		_, err := gitCmd(context.Background(), dir, 30*time.Second, "checkout", "-q", "-b", "agent-side-branch")
		return err
	}
	term := stage(t, tc, "w2", emptyDir(t, "w2"), ctlDir, "impl2", rogue, 60*time.Second)
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("执行体切走分支时应判失败, 实得 %s", term.Kind)
	}
	if !strings.Contains(term.Err, "切到") {
		t.Errorf("失败原因应点明分支被切走: %q", term.Err)
	}
	if now := gitOut(t, bare, "rev-parse", testBranch); now != head {
		t.Errorf("远端分支不该被动: %s → %s", shortSHA(head), shortSHA(now))
	}
}

// ★ 真机验证抓到的坑之二: worker 自己在 cwd 下的运行期垃圾 (沙箱 stdout/stderr)
// 会被 git add -A 扫进提交。用 .git/info/exclude 排除 —— 本地克隆级, 不动仓库文件。
func TestGit_worker运行期垃圾不进提交(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	rt := &fileWriterRuntime{files: map[string]string{
		"go.mod":                                "module demo\n\ngo 1.22\n",
		".claude-go/sandboxes/sbx-1/stdout.log": "一堆沙箱输出\n",
		".claude-go-ws/some.control":            "{}",
	}}
	if term := stage(t, tc, "w1", emptyDir(t, "w1"), ctlDir, "impl", rt, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("阶段失败: %s %q", term.Kind, term.Err)
	}
	files := strings.Join(gitLsTree(t, bare, testBranch), ",")
	if strings.Contains(files, ".claude-go/") || strings.Contains(files, ".claude-go-ws/") {
		t.Errorf("worker 运行期垃圾不该进仓库: %s", files)
	}
	if !strings.Contains(files, "go.mod") {
		t.Errorf("真产物必须进仓库: %s", files)
	}
}

// 上一阶段失败留下的脏树: 下一次准备时导出成 patch 再清干净 —— 不静默丢弃,
// 也不让 worker 永久卡在脏状态上。
func TestGit_脏树先抢救成patch再清理(t *testing.T) {
	bare := newBareRepo(t)
	ctlDir := emptyDir(t, "control")
	tc := newTestControl(t, controlOpts{ws: gitPolicy(bare, 0)})
	wdir := emptyDir(t, "w1")
	base := &fileWriterRuntime{files: map[string]string{"go.mod": "module demo\n\ngo 1.22\n"}}
	if term := stage(t, tc, "w1", wdir, ctlDir, "impl", base, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("基线阶段失败: %s %q", term.Kind, term.Err)
	}
	// 模拟"上一阶段失败但已经写了文件"。
	if err := os.WriteFile(filepath.Join(wdir, "half-done.go"), []byte("package demo // 半成品\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := &fileWriterRuntime{files: map[string]string{"ok.go": "package demo\n\nvar OK = true\n"}}
	if term := stage(t, tc, "w1", wdir, ctlDir, "impl2", next, 60*time.Second); term.Kind != agent.NodeEventDone {
		t.Fatalf("脏树抢救后阶段应成功: %s %q", term.Kind, term.Err)
	}
	// 半成品既没进提交, 也没被无痕丢弃。
	if got := gitLsTree(t, bare, testBranch); strings.Contains(strings.Join(got, ","), "half-done") {
		t.Errorf("上一阶段的残留不该被本阶段带进提交: %v", got)
	}
	patches, _ := filepath.Glob(filepath.Join(wdir, ".git", "claude-go-salvage", "dirty-*.patch"))
	if len(patches) == 0 {
		t.Error("脏树应被导出成 patch (不许静默丢弃)")
	} else {
		b, _ := os.ReadFile(patches[0])
		if !strings.Contains(string(b), "half-done.go") {
			t.Errorf("patch 里应包含被清理的文件: %q", string(b))
		}
	}
}
