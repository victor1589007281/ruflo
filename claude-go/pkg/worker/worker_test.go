package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

// 构造守卫: 缺控制面/名字/执行体的 worker 不许被造出来 —— 那种 worker 只会安静地
// 什么都不做, 正是改造前的状态。
func TestWorkerNew_构造守卫(t *testing.T) {
	rt := echoRuntime("local-x", agent.RuntimeCaps{}, func(context.Context, string, string) (string, error) { return "", nil })
	cases := []struct {
		name string
		opt  Options
	}{
		{"缺 Control", Options{Name: "w", Runtime: rt}},
		{"缺 Name", Options{Control: "http://x", Runtime: rt}},
		{"缺 Runtime", Options{Control: "http://x", Name: "w"}},
	}
	for _, c := range cases {
		if _, err := New(c.opt); err == nil {
			t.Errorf("%s 时应报错", c.name)
		}
	}
	w, err := New(Options{Control: "http://x", Name: "w", Runtime: rt, ExtraCaps: []string{"mcp:playwright"}})
	if err != nil {
		t.Fatal(err)
	}
	// 上报标签 = runtime 自己声明的能力 + 额外标签 + 钉住自己的合成标签。
	got := strings.Join(w.Caps(), ",")
	if !strings.Contains(got, "mcp:playwright") || !strings.Contains(got, WorkerCap("w")) {
		t.Errorf("能力标签 = %v", w.Caps())
	}
}

// 载荷坏了必须失败: 拿空 prompt 跑出来的产出是"成功的垃圾", 比失败更难查。
func TestWorker_载荷解码失败必须上报失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id := firstTaskID(t, tc)
	// 直接篡改任务载荷为非法 JSON, 再让真 worker 去拉。
	task, err := tc.q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	// 合法 JSON 但不是 StageTask 的形状 (statestore 只接受合法 JSON, 所以用数组
	// 而不是乱码 —— 这也更贴近真实的协议漂移场景)。
	task.Payload = []byte(`[1,2,3]`)
	if _, err := tc.q.Enqueue(*task); err != nil { // 同 ID 覆盖写回 (状态回 pending)
		t.Fatal(err)
	}
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) {
			t.Error("载荷非法时不该真去执行")
			return "", nil
		})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt})

	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "载荷") {
		t.Fatalf("期望载荷错误导致失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

// 工作区无法提供必须失败: 静默在错误目录里产码会得到"阶段成功但下一阶段找不到
// 上一阶段的代码", 这类故障极难归因。
func TestWorker_工作区无法提供必须失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x", Workspace: "/srv/teams/foo",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) {
			t.Error("工作区不匹配时不该真去执行")
			return "", nil
		})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt, Workspace: t.TempDir()})

	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "工作区") {
		t.Fatalf("期望因工作区不匹配失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

func TestWorker_工作区检查规则(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{opt: Options{Name: "w", Workspace: dir}}
	if err := w.checkWorkspace(""); err != nil {
		t.Errorf("未指定工作区应放行: %v", err)
	}
	if err := w.checkWorkspace(dir + "/"); err != nil {
		t.Errorf("尾斜杠差异不该判失败: %v", err)
	}
	if err := w.checkWorkspace("/nope"); err == nil {
		t.Error("工作区不一致应报错")
	}
	// 未声明工作区的 worker 拿到指定工作区的任务: 必须拒 (fail-closed)。
	w2 := &Worker{opt: Options{Name: "w2"}}
	if err := w2.checkWorkspace("/srv/x"); err == nil {
		t.Error("未声明工作区却收到指定工作区的任务, 应拒绝")
	}
	// 声明了但目录不存在: 也必须拒。
	w3 := &Worker{opt: Options{Name: "w3", Workspace: "/definitely/not/here"}}
	if err := w3.checkWorkspace("/definitely/not/here"); err == nil {
		t.Error("声明的工作区不存在时应拒绝")
	}
}

// 能力自检 (纵深防御): 控制面若因注册表不同步派来干不了的任务, 必须诚实失败,
// 而不是硬跑出"没有浏览器却声称调研完了"的假产出。
func TestWorker_能力不匹配拒绝执行(t *testing.T) {
	if miss := MissingCaps([]string{"bash", "GPU", ""}, []string{"bash"}); len(miss) != 1 || miss[0] != "gpu" {
		t.Fatalf("MissingCaps = %v", miss)
	}
	tc := newTestControl(t, controlOpts{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) {
			t.Error("缺能力时不该真去执行")
			return "", nil
		})
	w, err := New(Options{Control: tc.srv.URL, Name: "w1", Runtime: rt,
		Logf: func(f string, a ...any) { t.Logf("[worker] "+f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	// 直接把一个要求 gpu 的任务塞给 execute (队列的 caps 过滤正常情况下会挡住它,
	// 这里测的就是"过滤失灵时"的第二道闸)。
	payload, err := EncodeStageTask(StageTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := tc.q.Enqueue(clusterTask("t-gpu", payload, []string{CapGPU}))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := tc.q.PullFor("w1", nil, []string{CapGPU, CapBash, WorkerCap("w1")})
	if err != nil || task == nil {
		t.Fatalf("PullFor: %v", err)
	}
	w.execute(context.Background(), task)
	got, err := tc.q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || !strings.Contains(got.Err, "缺少必需能力") {
		t.Fatalf("期望因缺能力失败, 实得 status=%s err=%q", got.Status, got.Err)
	}
}

// 执行体关了通道却没给终态 (被强杀/实现有 bug): 不许当成功。
func TestWorker_执行体未给终态判失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	tc.startWorker(t, Options{Name: "w1", Runtime: silentRuntime{}})
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "未回报终态") {
		t.Fatalf("期望因未回报终态而失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

// Execute 直接报错 (工厂造不出 runner) 也必须上报失败。
func TestWorker_执行体启动失败上报失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// 真 localRuntime + 报错的工厂: 复用既有 localRuntime 的错误路径。
	rt := agent.NewLocalRuntime("local-w1", agent.RuntimeCaps{}, func(context.Context, string, string) (agent.AgentRunner, error) {
		return nil, errors.New("模型未配置")
	})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt})
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "模型未配置") {
		t.Fatalf("期望启动失败被上报, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

// worker 关停时在途任务必须立刻上报失败, 不留悬挂租约让控制面等到超时。
func TestWorker_关停时在途任务立即失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{taskLease: time.Minute})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{}, func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done() // 长任务: 一直跑到被取消
		return "", ctx.Err()
	})
	_, cancel := tc.startWorker(t, Options{Name: "w1", Runtime: rt})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker 未在 3s 内开始执行")
	}
	cancel() // 关停

	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("关停时在途任务应失败, 实得 kind=%s", term.Kind)
	}
	if !strings.Contains(term.Err, "cancel") && !strings.Contains(term.Err, "取消") && !strings.Contains(term.Err, "关停") {
		t.Errorf("失败原因应指明取消/关停, 实得 %q", term.Err)
	}
}

// 长任务续租: keepalive 必须把租约往后推, 否则 5 分钟以上的编码阶段会被队列
// 误判为 worker 崩溃而回收。
func TestWorker_长任务续租防误回收(t *testing.T) {
	tc := newTestControl(t, controlOpts{taskLease: 300 * time.Millisecond})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{})
	release := make(chan struct{})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	rt := echoRuntime("local-w1", agent.RuntimeCaps{}, func(ctx context.Context, _, _ string) (string, error) {
		select {
		case <-release:
			return "熬过了两个租约周期", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt, KeepaliveInterval: 50 * time.Millisecond})
	time.Sleep(700 * time.Millisecond) // > 2 个租约周期
	close(release)

	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventDone {
		t.Fatalf("续租应让长任务活下来, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
	if term.Output != "熬过了两个租约周期" {
		t.Errorf("产出 = %q", term.Output)
	}
}

// 跨进程取消: 控制面 Cancel → worker 在下一次 keepalive 收到指令并真的停下来。
func TestWorker_跨进程取消真的停下来(t *testing.T) {
	tc := newTestControl(t, controlOpts{taskLease: time.Minute})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{}, func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return "", ctx.Err()
	})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt, KeepaliveInterval: 20 * time.Millisecond})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker 未在 3s 内开始执行")
	}
	if err := remote.Cancel("r", "n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("取消指令未在 3s 内经 keepalive 投递到 worker (跨进程 Cancel 断链)")
	}
	_ = drain(t, ch, 3*time.Second)
}

// silentRuntime 关通道但不给终态事件的坏执行体。
type silentRuntime struct{}

func (silentRuntime) Name() string                    { return "silent" }
func (silentRuntime) Capabilities() agent.RuntimeCaps { return agent.RuntimeCaps{} }
func (silentRuntime) Cancel(string, string) error     { return nil }
func (silentRuntime) Execute(context.Context, agent.RuntimeNodeTask) (<-chan agent.NodeEvent, error) {
	ch := make(chan agent.NodeEvent)
	close(ch)
	return ch, nil
}
