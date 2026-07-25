package worker

// workspace_test.go —— cwd 三档位的 local/pvc 两档 + 档位路由 + 默认行为不变。
// git 档在 gitws_test.go。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/trace"
)

// 未配置档位策略时, 任务里**不能出现任何档位字段**, 也不能多出 ws:* 硬要求 ——
// 否则现有部署 (worker 没声明工作区) 会突然全线拒活。这条是本轮的兼容性底线。
func TestWorkspace_未配置档位时任务声明与行为都不变(t *testing.T) {
	tc := newTestControl(t, controlOpts{}) // ws = nil
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := firstTaskID(t, tc)
	task, err := tc.q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range task.RequireCaps {
		if strings.HasPrefix(c, CapWorkspacePrefix) || strings.HasPrefix(c, CapVolumePrefix) {
			t.Errorf("未配置策略却要求了工作区能力 %q (会让现有 worker 拉不到任务)", c)
		}
	}
	st, err := DecodeStageTask(task.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if st.WorkspaceMode != WorkspaceModeUnset || st.Workspace != "" || st.Git != nil || st.WorkspaceHandshake != "" {
		t.Fatalf("未配置策略时载荷不该带档位声明: %+v", st)
	}
	// 一个既没声明工作区也没声明档位的 worker 必须照常干活 (= 改造前行为)。
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) { return "ok", nil })
	tc.startWorker(t, Options{Name: "w1", Runtime: rt})
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventDone || term.Output != "ok" {
		t.Fatalf("期望照常成功, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

// 档位靠既有的能力标签过滤路由: local 档的 worker 绝不能拉到 pvc 档的任务。
// (这是第一道闸; worker 侧的模式校验是第二道。)
func TestWorkspace_档位标签路由靠队列过滤(t *testing.T) {
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "teams"}})
	payload, err := EncodeStageTask(StageTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	req := tc.brk.ws.RequireCaps()
	if len(req) != 2 || req[0] != "ws:pvc" || req[1] != "wsvol:teams" {
		t.Fatalf("pvc 策略的硬要求 = %v, 期望 [ws:pvc wsvol:teams]", req)
	}
	if _, err := tc.q.Enqueue(clusterTask("t1", payload, MergeCaps(req, []string{WorkerCap("w1")}))); err != nil {
		t.Fatal(err)
	}
	// local 档 worker 的标签集: 拉不到。
	localCaps := workspaceCaps(Options{})
	got, _, err := tc.q.PullFor("w1", nil, MergeCaps(localCaps, []string{CapBash, WorkerCap("w1")}))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("local 档 worker 不该拉到 pvc 档任务 (caps=%v)", localCaps)
	}
	// 卷名不同的 pvc worker: 也拉不到。
	otherVol := workspaceCaps(Options{WorkspaceMode: WorkspaceModePVC, WorkspaceVolume: "other"})
	if got, _, _ := tc.q.PullFor("w1", nil, MergeCaps(otherVol, []string{CapBash, WorkerCap("w1")})); got != nil {
		t.Fatalf("卷名不同的 worker 不该拉到任务 (caps=%v)", otherVol)
	}
	// 同档同卷: 拉得到。
	right := workspaceCaps(Options{WorkspaceMode: WorkspaceModePVC, WorkspaceVolume: "TEAMS"})
	got, _, err = tc.q.PullFor("w1", nil, MergeCaps(right, []string{CapBash, WorkerCap("w1")}))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("同档同卷的 worker 应该拉到任务 (caps=%v)", right)
	}
}

// 纵深防御: 队列过滤失灵 (注册表不同步/有人手工入队) 时, worker 也必须按档位拒绝,
// 而不是拿另一个档位的语义硬跑。
func TestWorkspace_档位不匹配拒绝执行(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{opt: Options{Name: "w", Workspace: dir}} // 未声明 = local
	if _, err := w.prepareWorkspace(StageTask{WorkspaceMode: WorkspaceModeGit,
		Git: &GitWorkspace{Remote: "x", Branch: "y"}}); err == nil {
		t.Error("local 档 worker 收到 git 档任务应拒绝")
	}
	wg := &Worker{opt: Options{Name: "wg", Workspace: dir, WorkspaceMode: WorkspaceModeGit}}
	if _, err := wg.prepareWorkspace(StageTask{WorkspaceMode: WorkspaceModePVC, Workspace: dir}); err == nil {
		t.Error("git 档 worker 收到 pvc 档任务应拒绝")
	}
	// 未知档位不许静默降级成 local。
	if _, err := ParseWorkspaceMode("pvcc"); err == nil {
		t.Error("未知档位名应报错")
	}
	if WorkspaceModeUnset.Effective() != WorkspaceModeLocal {
		t.Error("未声明档位应等价于 local")
	}
}

// local 档显式化之后, 路径不一致仍然照旧拒绝 (既有 fail-closed 判定原样保留)。
func TestWorkspaceLocal_路径不一致仍拒绝执行(t *testing.T) {
	ctlDir := t.TempDir()
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModeLocal}})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x", Workspace: ctlDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := &fileWriterRuntime{files: map[string]string{"a.go": "package a"}}
	tc.startWorker(t, Options{Name: "w1", Runtime: rt,
		Workspace: t.TempDir(), WorkspaceMode: WorkspaceModeLocal}) // 另一个目录
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "工作区") {
		t.Fatalf("期望因工作区不一致失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
	if rt.called() != 0 {
		t.Error("拒绝执行时不该真调执行体")
	}
}

// pvc 档: 读不到控制面写在共享卷上的握手文件 = 那个路径不是同一份数据, 必须拒绝。
// 这是"两个 Pod 各挂一个 emptyDir 到 /workspace"这类误配置的唯一检出手段 ——
// 光比路径字符串它们是"一致"的。
func TestWorkspacePVC_握手缺失拒绝执行(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{opt: Options{Name: "w", Workspace: dir,
		WorkspaceMode: WorkspaceModePVC, WorkspaceVolume: "teams"}}
	_, err := w.prepareWorkspace(StageTask{
		WorkspaceMode: WorkspaceModePVC, Workspace: dir,
		WorkspaceVolume: "teams", WorkspaceHandshake: "task-x",
	})
	if err == nil {
		t.Fatal("没有控制面握手文件时必须拒绝执行")
	}
	if !strings.Contains(err.Error(), "共享卷") {
		t.Errorf("失败原因应点明共享卷不一致: %v", err)
	}
	// 缺令牌 (老控制面/协议漂移) 也必须拒绝, 不许当"没要求"放过。
	if _, err := w.prepareWorkspace(StageTask{WorkspaceMode: WorkspaceModePVC, Workspace: dir,
		WorkspaceVolume: "teams"}); err == nil {
		t.Error("缺握手令牌应拒绝执行")
	}
	// 卷名不一致: 拒绝。
	if err := writeHandshake(dir, "task-y", "control", handshakeNote{Volume: "teams", TaskID: "task-y"}); err != nil {
		t.Fatal(err)
	}
	w2 := &Worker{opt: Options{Name: "w2", Workspace: dir,
		WorkspaceMode: WorkspaceModePVC, WorkspaceVolume: "teams"}}
	if _, err := w2.prepareWorkspace(StageTask{WorkspaceMode: WorkspaceModePVC, Workspace: dir,
		WorkspaceVolume: "other", WorkspaceHandshake: "task-y"}); err == nil {
		t.Error("卷名不一致应拒绝执行")
	}
}

// pvc 档正路: 控制面写握手 → worker 读到才跑 → worker 写回执 → 控制面读到才认成功。
// 顺带断言产物真的落在共享目录里 (门禁就在那里跑)。
func TestWorkspacePVC_双向握手通过且产物落在共享目录(t *testing.T) {
	shared := t.TempDir()
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "teams"}})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "写代码", Workspace: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 控制面在派任务时就已经把握手文件写进共享卷了 (Execute 是同步做这件事的)。
	id := firstTaskID(t, tc)
	if _, err := os.Stat(handshakePath(shared, id, "control")); err != nil {
		t.Fatalf("控制面握手文件未落地: %v", err)
	}
	rt := &fileWriterRuntime{files: map[string]string{"pkg/a.go": "package a\n"}, out: "写好了"}
	tc.startWorker(t, Options{Name: "w1", Runtime: rt,
		Workspace: shared, WorkspaceMode: WorkspaceModePVC, WorkspaceVolume: "teams"})

	term := terminal(t, drain(t, ch, 10*time.Second))
	if term.Kind != agent.NodeEventDone {
		t.Fatalf("pvc 档正路应成功, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
	if b, err := os.ReadFile(filepath.Join(shared, "pkg", "a.go")); err != nil || string(b) != "package a\n" {
		t.Fatalf("产物没落在共享目录: %v %q", err, string(b))
	}
	// 任务结束后握手文件应被清掉 (共享卷上不留垃圾)。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(handshakePath(shared, id, "control")); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("任务结束后握手文件未被清理")
}

// worker 说完成了, 但控制面在共享卷上看不到它的回执 ⇒ 两边不是同一份数据,
// 于是 worker 写的代码门禁也看不见。必须判失败, 不许当成功。
func TestWorkspacePVC_控制面读不到回执必须判失败(t *testing.T) {
	shared := t.TempDir()
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "teams"}})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x", Workspace: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := firstTaskID(t, tc)
	// 模拟"协议漂移的 worker": 走队列正常完成, 但没写回执 (等价于它写到了自己的卷上)。
	if _, _, err := tc.q.PullFor("w1", nil, []string{WorkerCap("w1"), "ws:pvc", "wsvol:teams"}); err != nil {
		t.Fatal(err)
	}
	if err := tc.q.Complete(id, "w1", mustJSON(t, NewStageResult("w1", "我干完了", 1))); err != nil {
		t.Fatal(err)
	}
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("回执缺失必须判失败, 实得 kind=%s output=%q", term.Kind, term.Output)
	}
	if !strings.Contains(term.Err, "回执") {
		t.Errorf("失败原因应点明回执缺失: %q", term.Err)
	}
}

// 控制面侧看不到工作区目录时 (卷没挂上): Execute 必须**同步**报错, 让调用方立刻
// 失败, 而不是派一个注定被拒的任务再等一轮。
func TestWorkspacePVC_控制面缺工作区目录同步报错(t *testing.T) {
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "teams"}})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	_, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x",
		Workspace: filepath.Join(t.TempDir(), "not-mounted"),
	})
	if err == nil {
		t.Fatal("控制面侧工作区不存在时 Execute 应直接报错")
	}
	if tasks, _ := tc.q.List(); len(tasks) != 0 {
		t.Errorf("失败时不该留下任务: %d", len(tasks))
	}
}

// 策略自洽性: 缺卷名/缺 remote 必须在构造时就报错 —— 否则所有任务会派成永远
// 没人能拉的 pending, 症状是"团队卡住不动"。
func TestWorkspacePolicy_校验与分支模板(t *testing.T) {
	if err := (&WorkspacePolicy{Mode: WorkspaceModePVC}).Validate(); err == nil {
		t.Error("pvc 缺卷名应报错")
	}
	if err := (&WorkspacePolicy{Mode: WorkspaceModeGit}).Validate(); err == nil {
		t.Error("git 缺 remote 应报错")
	}
	var nilPolicy *WorkspacePolicy
	if err := nilPolicy.Validate(); err != nil || nilPolicy.Enabled() {
		t.Error("nil 策略应视为未启用且校验通过")
	}
	if _, err := NewBroker(BrokerOptions{Queue: newTestControl(t, controlOpts{}).q,
		Workspace: &WorkspacePolicy{Mode: WorkspaceModeGit}}); err == nil {
		t.Error("Broker 应拒绝不自洽的策略")
	}
	p := &WorkspacePolicy{Mode: WorkspaceModeGit, GitRemote: "/tmp/x.git"}
	if got := p.resolveBranch("织叙 team/1", "run-9"); got != "claude-go/ws/-team/1" {
		t.Errorf("分支模板展开 = %q", got)
	}
	if got := p.resolveBranch("", "run-9"); got != "claude-go/ws/run-9" {
		t.Errorf("无团队名时应退回 run: %q", got)
	}
	p2 := &WorkspacePolicy{Mode: WorkspaceModeGit, GitRemote: "x", GitBranch: "wip/{team}/{run}"}
	if got := p2.resolveBranch("t1", "r1"); got != "wip/t1/r1" {
		t.Errorf("自定义模板 = %q", got)
	}
}

// 构造守卫: 档位声明不全的 worker 不许被造出来 (启动就失败, 而不是运行期才发现)。
func TestWorkspace_worker构造守卫(t *testing.T) {
	rt := &fileWriterRuntime{}
	cases := []struct {
		name string
		opt  Options
	}{
		{"pvc 缺工作区", Options{Control: "http://x", Name: "w", Runtime: rt, WorkspaceMode: WorkspaceModePVC, WorkspaceVolume: "v"}},
		{"pvc 缺卷名", Options{Control: "http://x", Name: "w", Runtime: rt, WorkspaceMode: WorkspaceModePVC, Workspace: "/tmp"}},
		{"git 缺工作区", Options{Control: "http://x", Name: "w", Runtime: rt, WorkspaceMode: WorkspaceModeGit}},
		{"git 并发>1", Options{Control: "http://x", Name: "w", Runtime: rt, WorkspaceMode: WorkspaceModeGit,
			Workspace: "/tmp", MaxParallel: 2}},
		{"未知档位", Options{Control: "http://x", Name: "w", Runtime: rt, WorkspaceMode: WorkspaceMode("nfs")}},
	}
	for _, c := range cases {
		if _, err := New(c.opt); err == nil {
			t.Errorf("%s 时应报错", c.name)
		} else {
			t.Logf("%s → %v", c.name, err)
		}
	}
	// 上报的能力标签里必须有档位 (控制面靠它路由)。
	w, err := New(Options{Control: "http://x", Name: "w", Runtime: rt,
		WorkspaceMode: WorkspaceModePVC, Workspace: "/tmp", WorkspaceVolume: "teams"})
	if err != nil {
		t.Fatal(err)
	}
	caps := strings.Join(w.Caps(), ",")
	if !strings.Contains(caps, "ws:pvc") || !strings.Contains(caps, "wsvol:teams") {
		t.Errorf("能力标签缺档位声明: %v", w.Caps())
	}
}

// 控制面的执行工厂必须把**团队 cwd** 声明出来 (RunMetadata.Cwd → 任务的工作区):
// 改造前这个字段从来没人填, 于是远程 worker 一律在自己的目录里产码, 而编译门禁
// 跑在控制面的 <team.Cwd>/go.mod 上 —— 这正是"跨机产码不可用"的根。
func TestRuntimeFactory_档位启用时声明团队cwd(t *testing.T) {
	rec := &recordRuntime{name: "remote-w1", caps: agent.RuntimeCaps{Bash: true}, out: "ok"}
	reg := agent.NewRuntimeRegistry()
	reg.Register(rec, 0)
	ctx := agent.WithRunMetadata(context.Background(), agent.RunMetadata{Team: "t1", Cwd: "/srv/teams/t1"})
	ctx = trace.With(ctx, trace.IDs{RunID: "run-1", NodeID: "impl"})

	// 未启用策略: 工作区字段留空 (行为与改造前一致)。
	r1, err := RuntimeFactory(reg, &agent.Placement{Prefer: "remote:remote-w1"}, nil)(ctx, "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Execute(ctx, "写"); err != nil {
		t.Fatal(err)
	}
	if got := rec.tasks[0].Workspace; got != "" {
		t.Errorf("未启用档位时不该声明工作区, 实得 %q", got)
	}

	// 启用后: 团队 cwd 必须出现在任务里。
	pol := &WorkspacePolicy{Mode: WorkspaceModeLocal}
	r2, err := RuntimeFactory(reg, &agent.Placement{Prefer: "remote:remote-w1"}, pol)(ctx, "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Execute(ctx, "写"); err != nil {
		t.Fatal(err)
	}
	if got := rec.tasks[1].Workspace; got != "/srv/teams/t1" {
		t.Errorf("启用档位后应声明团队 cwd, 实得 %q", got)
	}
	// 策略不自洽时工厂就该报错, 不许造出一个跑起来才失败的 runner。
	if _, err := RuntimeFactory(reg, nil, &WorkspacePolicy{Mode: WorkspaceModePVC})(ctx, "coder", ""); err == nil {
		t.Error("不自洽的策略应在造 runner 时就报错")
	}
}
