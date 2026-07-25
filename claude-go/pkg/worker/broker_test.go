package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

// 端到端: 控制面 Pick 出远程 runtime → 入队 → 独立 worker 真跑 → 事件回流 →
// 产出经队列回到调用方。这是"执行体是桩"被消灭的直接证据。
func TestBroker_远程执行端到端(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(_ context.Context, role, prompt string) (string, error) {
			return "由 " + role + " 完成: " + prompt, nil
		})
	w, _ := tc.startWorker(t, Options{Name: "w1", Runtime: rt})

	remote := tc.waitRuntime(t, "w1", time.Second)
	if !remote.Capabilities().Bash {
		t.Errorf("worker 上报的 bash 能力未投影进 RuntimeCaps: %+v", remote.Capabilities())
	}

	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "run-1", NodeID: "impl", Role: "coder", UserPrompt: "写个函数",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	evs := drain(t, ch, 5*time.Second)

	// ① 终态必须是 done, 且产出是 worker 真跑出来的 (不是回显 payload)。
	term := terminal(t, evs)
	if term.Kind != agent.NodeEventDone {
		t.Fatalf("期望 done, 实得 %s: %s", term.Kind, term.Err)
	}
	if want := "由 coder 完成: 写个函数"; term.Output != want {
		t.Errorf("产出 = %q, 期望 %q", term.Output, want)
	}
	// ② started 事件必须跨进程回流 (观测流通了)。
	if len(evs) < 2 || evs[0].Kind != agent.NodeEventStarted {
		t.Errorf("期望首个事件是 started, 实得 %v", kinds(evs))
	}
	// ③ 队列里的任务应是 completed, 且 run/node 归因在 (控制面观测靠它)。
	tasks, _ := tc.q.List()
	if len(tasks) != 1 || tasks[0].Status != "completed" {
		t.Fatalf("队列任务状态 = %+v", tasks)
	}
	if tasks[0].RunID != "run-1" || tasks[0].NodeID != "impl" {
		t.Errorf("任务未带 run/node 归因: %+v", tasks[0])
	}
	// ④ worker 侧记账: 一次成功、零失败。
	if done, failed := w.Stats(); done != 1 || failed != 0 {
		t.Errorf("worker 统计 = done:%d failed:%d, 期望 1/0", done, failed)
	}
}

// 执行出错必须变成控制面可见的 failed, 且带上原始错误 —— 不许静默成功。
func TestBroker_执行出错必须真失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) {
			return "", errors.New("编译门禁不通过")
		})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt})
	remote := tc.waitRuntime(t, "w1", time.Second)

	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// CollectRuntimeOutput 是绝大多数调用方的入口: 它必须把失败归约成 error。
	if _, err := agent.CollectRuntimeOutput(ch); err == nil || !strings.Contains(err.Error(), "编译门禁不通过") {
		t.Fatalf("期望归约为含原因的 error, 实得 %v", err)
	}
	tasks, _ := tc.q.List()
	if len(tasks) != 1 || tasks[0].Status != "failed" {
		t.Errorf("队列任务应终态 failed (MaxAttempts=1 不重派), 实得 %+v", tasks)
	}
}

// 改造前的桩 worker 回报的是 {"worker":...,"echo":...} 形状。它能被宽松解码成
// 空产出 —— 必须凭协议标记判失败, 否则"什么都没干"会变成"成功但没话说"。
func TestBroker_桩worker的回显结果判失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})

	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id := firstTaskID(t, tc)
	// 模拟旧桩: 拉走 + 回显式 Complete。
	if _, ok, err := tc.q.PullFor("w1", []string{TaskKindStage}, []string{WorkerCap("w1"), CapBash}); err != nil || !ok {
		t.Fatalf("PullFor: ok=%v err=%v", ok, err)
	}
	legacy := mustJSON(t, map[string]any{"worker": "w1", "task_id": id, "echo": "{}", "status": "completed"})
	if err := tc.q.Complete(id, "w1", legacy); err != nil {
		t.Fatal(err)
	}
	term := terminal(t, drain(t, ch, 3*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "协议标记不符") {
		t.Fatalf("期望因协议标记不符而失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

// worker 掉线 (从注册表消失) 且任务无人可拉 → 超过宽限期必须失败,
// 不能让调用方一直挂着等到节点超时。
func TestBroker_worker掉线后任务无人可拉判失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{grace: 50 * time.Millisecond, workerLease: 60 * time.Millisecond})
	// 让 worker 心跳一次后就"死掉"(不再心跳), 注册表租约到期即消失。
	if err := tc.reg.Heartbeat(clusterWorker("w1", []string{CapBash, WorkerCap("w1")})); err != nil {
		t.Fatal(err)
	}
	// runtime 租约刻意取小: worker 停止心跳后, RuntimeRegistry 必须凭既有的
	// 租约过期机制把它剔除 (Sync 只给存活 worker 续租, 不做显式注销)。
	remote := tc.waitRuntime(t, "w1", 100*time.Millisecond)

	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "已掉线") {
		t.Fatalf("期望因 worker 掉线失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
	// 同时: worker 从 cluster 注册表消失后, Sync 不再续租 → 租约到期即剔除。
	time.Sleep(150 * time.Millisecond)
	tc.syncOnce(t, 100*time.Millisecond)
	if names := tc.rr.List(); len(names) != 0 {
		t.Errorf("掉线 worker 应被租约剔除, 实得 %v", names)
	}
	if _, err := tc.rr.Pick(&agent.Placement{Require: []string{CapBash}}); !errors.Is(err, agent.ErrNoRuntime) {
		t.Errorf("剔除后 Pick 应返回 ErrNoRuntime, 实得 %v", err)
	}
}

// worker 拉走任务后进程被 kill: 租约过期必须被回收判失败。
// 关键是 Queue.Get **不做**回收 (只有 Pull/List 做), 集群里没有别的 worker 在
// Pull 时, 若控制面不主动触发回收, 任务会永远停在 leased = 静默挂死。
func TestBroker_租约过期任务被回收判失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{taskLease: 40 * time.Millisecond})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// 拉走但永不回报 (等价于 worker 被 SIGKILL)。
	if _, ok, err := tc.q.PullFor("w1", []string{TaskKindStage}, []string{WorkerCap("w1"), CapBash}); err != nil || !ok {
		t.Fatalf("PullFor: ok=%v err=%v", ok, err)
	}
	term := terminal(t, drain(t, ch, 5*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "租约过期") {
		t.Fatalf("期望因租约过期失败, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
}

// 钉住: 派给 w1 的任务, w2 必须拉不到 (团队亲和的落地机制)。
func TestBroker_任务钉住指定worker(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	r1 := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	if _, err := r1.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"}); err != nil {
		t.Fatal(err)
	}
	// w2 带自己的全部标签来拉: 拉不到 (缺 worker:w1)。
	if task, ok, err := tc.q.PullFor("w2", []string{TaskKindStage}, []string{CapBash, WorkerCap("w2")}); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("w2 不该拉到钉给 w1 的任务: %+v", task)
	}
	// w1 能拉到。
	if _, ok, err := tc.q.PullFor("w1", []string{TaskKindStage}, []string{CapBash, WorkerCap("w1")}); err != nil || !ok {
		t.Fatalf("w1 应能拉到自己的任务: ok=%v err=%v", ok, err)
	}
}

// Sync: 首次注册 → 心跳续租 → 能力变化重新注册; 未声明 stage 的 worker 不入选。
func TestBroker_Sync注册与续租(t *testing.T) {
	tc := newTestControl(t, controlOpts{workerLease: time.Second})
	_ = tc.reg.Heartbeat(clusterWorker("w1", []string{CapBash}))
	_ = tc.reg.Heartbeat(clusterWorkerKinds("w2", []string{CapBash}, []string{"other"}))

	names := tc.syncOnce(t, 200*time.Millisecond)
	if len(names) != 1 || names[0] != "w1" {
		t.Fatalf("只应同步声明了 stage 的 worker, 实得 %v", names)
	}
	// 心跳续租: 200ms 租约下持续 Sync, 400ms 后仍应存活。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = tc.reg.Heartbeat(clusterWorker("w1", []string{CapBash}))
		tc.syncOnce(t, 200*time.Millisecond)
		time.Sleep(20 * time.Millisecond)
	}
	if names := tc.rr.List(); len(names) != 1 {
		t.Fatalf("持续心跳应续租成功, 实得 %v", names)
	}
	// 能力变化: 重新注册, Capabilities 必须更新 (否则需要 browser 的节点会误派)。
	_ = tc.reg.Heartbeat(clusterWorker("w1", []string{CapBash, CapBrowser}))
	tc.syncOnce(t, time.Second)
	rt, err := tc.rr.Pick(&agent.Placement{Require: []string{CapBrowser}})
	if err != nil {
		t.Fatalf("能力更新后应能按 browser 选中: %v", err)
	}
	if rt.Name() != "w1" || !rt.Capabilities().Browser {
		t.Errorf("能力未更新: %s %+v", rt.Name(), rt.Capabilities())
	}
}

// runtime 名冲突守卫: 叫 local-xxx 的远程 worker 必须改名, 否则会被
// Prefer:"local" 当成本地 runtime 加分 —— 把跨机执行伪装成本机执行。
func TestBroker_远程worker不得冒充本地(t *testing.T) {
	if got := RuntimeName("local-sneaky"); got != "remote-local-sneaky" {
		t.Errorf("RuntimeName(local-sneaky) = %q", got)
	}
	if got := RuntimeName("w1"); got != "w1" {
		t.Errorf("RuntimeName(w1) = %q", got)
	}
	tc := newTestControl(t, controlOpts{})
	_ = tc.reg.Heartbeat(clusterWorker("local-sneaky", []string{CapBash}))
	names := tc.syncOnce(t, time.Second)
	if len(names) != 1 || names[0] != "remote-local-sneaky" {
		t.Fatalf("同步后的 runtime 名 = %v", names)
	}
	// 本地 runtime 与它同时在场时, Prefer:"local" 必须选真本地的那个。
	tc.rr.Register(echoRuntime("local-real", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) { return "", nil }), 0)
	rt, err := tc.rr.Pick(&agent.Placement{Require: []string{CapBash}, Prefer: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "local-real" {
		t.Errorf("Prefer=local 选中了 %q", rt.Name())
	}
}

// Cancel: 未知节点报错 (与 localRuntime 一致); 已知节点 → 事件通道以 failed 收尾,
// 且 worker 下一次上报事件时能取回取消指令。
func TestBroker_Cancel语义(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	if err := remote.Cancel("nope", "nope"); err == nil {
		t.Error("取消不存在的节点应报错")
	}
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id := firstTaskID(t, tc)
	if err := remote.Cancel("r", "n"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	term := terminal(t, drain(t, ch, 3*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "取消") {
		t.Fatalf("取消后期望 failed, 实得 kind=%s err=%q", term.Kind, term.Err)
	}
	// worker 侧的取消投递: 上报事件时 ack.Cancel=true。
	ack := tc.brk.ingest(EventBatch{Worker: "w1", TaskID: id})
	if !ack.Cancel {
		t.Errorf("取消指令未能经事件应答投递给 worker: %+v", ack)
	}
	// 取消后再次 Cancel 应报错 (在途表已清)。
	if err := remote.Cancel("r", "n"); err == nil {
		t.Error("在途表清空后 Cancel 应报错")
	}
}

// 调用方 ctx 取消 (节点超时/图被取消): 必须收到 failed 并关闭通道, 不许挂死。
func TestBroker_调用方取消立即失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := remote.Execute(ctx, agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	evs := drain(t, ch, 3*time.Second)
	if len(evs) == 0 {
		// ctx 已取消时终态送不出去是允许的 (调用方已放弃), 但通道必须关闭。
		return
	}
	if term := evs[len(evs)-1]; term.Kind != agent.NodeEventFailed {
		t.Fatalf("ctx 取消后期望 failed, 实得 %s", term.Kind)
	}
}

// 入队失败必须同步报错, 而不是给一个永远不出事件的通道。
func TestBroker_入队失败同步报错(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := tc.brk.Runtime("w1", agent.RuntimeCaps{})
	// Kind 由本包写死为 "stage", 正常路径不会失败; 这里直接验证 Broker 的构造守卫,
	// 它保证"没有队列的 broker"不会被造出来 (那种 broker 会静默无所作为)。
	if _, err := NewBroker(BrokerOptions{}); err == nil {
		t.Error("Queue 为空时 NewBroker 应报错")
	}
	if rt == nil {
		t.Fatal("Runtime 返回 nil")
	}
}

// 控制面开了鉴权 (wiki.apiSecret) 时: 不带 token 的 worker 必须**看得见** 401
// (而不是静默什么都拉不到), 带 token 的 worker 正常工作。
//
// 这条很关键: httpauth 默认保护 /cluster/ 前缀 (httpauth.go:36), 而既有的
// cluster.Client 不发鉴权头且不检查状态码 —— 那种组合下 worker 会安静地永远拉不到
// 任务, 看起来像"队列里没活"。
func TestWorker_控制面鉴权(t *testing.T) {
	tc := newTestControl(t, controlOpts{authSecret: "s3cret"})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(_ context.Context, role, prompt string) (string, error) { return "带票进场:" + prompt, nil })

	noToken, err := New(Options{Control: tc.srv.URL, Name: "w1", Runtime: rt,
		Logf: func(f string, a ...any) { t.Logf("[worker] "+f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noToken.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("无 token 时应拿到可见的 401, 实得 %v", err)
	}

	withToken, err := New(Options{Control: tc.srv.URL, Name: "w1", Runtime: rt, AuthToken: "s3cret",
		Logf: func(f string, a ...any) { t.Logf("[worker] "+f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	remote := tc.brk.Runtime("w1", agent.RuntimeCaps{Bash: true})
	ch, err := remote.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n", Role: "coder", UserPrompt: "干活"})
	if err != nil {
		t.Fatal(err)
	}
	ran, err := withToken.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("带 token 应能拉到并执行: ran=%v err=%v", ran, err)
	}
	term := terminal(t, drain(t, ch, 3*time.Second))
	if term.Kind != agent.NodeEventDone || term.Output != "带票进场:干活" {
		t.Fatalf("kind=%s out=%q err=%q", term.Kind, term.Output, term.Err)
	}
}
