package worker

// harness_test.go —— 测试脚手架: 一个真的控制面 (httptest + cluster 端点 + broker)
// 与一个真的 worker (agent.NewLocalRuntime 收编的执行体)。
//
// 刻意不 mock HTTP: 这个包的全部风险都在跨进程边界上 (状态码、协议形状、
// 事件与终态的时序), mock 掉就等于没测。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/httpauth"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// testControl 一套进程内控制面。
type testControl struct {
	q   *cluster.Queue
	reg *cluster.Registry
	brk *Broker
	srv *httptest.Server
	rr  agent.RuntimeRegistry
}

type controlOpts struct {
	taskLease   time.Duration    // 队列租约, 0 → 5s
	workerLease time.Duration    // worker 注册表租约, 0 → 5s
	grace       time.Duration    // 掉线宽限期, 0 → 5s
	poll        time.Duration    // 控制面轮询队列间隔, 0 → 5ms
	authSecret  string           // 非空 → 控制面按 httpauth 保护 /cluster/*
	ws          *WorkspacePolicy // cwd 档位策略, nil = 不启用 (默认)
}

func newTestControl(t *testing.T, o controlOpts) *testControl {
	t.Helper()
	if o.taskLease == 0 {
		o.taskLease = 5 * time.Second
	}
	if o.workerLease == 0 {
		o.workerLease = 5 * time.Second
	}
	if o.grace == 0 {
		o.grace = 5 * time.Second
	}
	if o.poll == 0 {
		o.poll = 5 * time.Millisecond
	}
	ss := statestore.NewMemStore()
	q := cluster.NewQueue(ss, o.taskLease)
	reg := cluster.NewRegistry(ss, o.workerLease)
	brk, err := NewBroker(BrokerOptions{
		Queue: q, Registry: reg, Workspace: o.ws,
		PollInterval: o.poll, DeadWorkerGrace: o.grace,
		Logf: func(f string, a ...any) { t.Logf("[broker] "+f, a...) },
	})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	mux := http.NewServeMux()
	cluster.Mount(mux, q, reg)
	brk.Mount(mux)
	var h http.Handler = mux
	if o.authSecret != "" {
		// 与生产同一把闸: httpauth 默认保护 /cluster/ 前缀。
		h = httpauth.Middleware(httpauth.Config{Secret: o.authSecret})(mux)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &testControl{q: q, reg: reg, brk: brk, srv: srv, rr: agent.NewRuntimeRegistry()}
}

// startWorker 启一个后台 worker, 返回停止函数。
func (tc *testControl) startWorker(t *testing.T, opt Options) (*Worker, context.CancelFunc) {
	t.Helper()
	if opt.Control == "" {
		opt.Control = tc.srv.URL
	}
	if opt.PollInterval == 0 {
		opt.PollInterval = 5 * time.Millisecond
	}
	if opt.HeartbeatInterval == 0 {
		opt.HeartbeatInterval = 50 * time.Millisecond
	}
	if opt.KeepaliveInterval == 0 {
		opt.KeepaliveInterval = 10 * time.Millisecond
	}
	if opt.Logf == nil {
		opt.Logf = func(f string, a ...any) { t.Logf("[worker] "+f, a...) }
	}
	w, err := New(opt)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker.Run 未在 3s 内退出 (goroutine 泄漏)")
		}
	})
	return w, cancel
}

// syncOnce 同步一次注册表, 返回本轮 runtime 名。
func (tc *testControl) syncOnce(t *testing.T, lease time.Duration) []string {
	t.Helper()
	names, err := tc.brk.Sync(tc.rr, lease)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return names
}

// waitRuntime 等到某个 runtime 出现在 RuntimeRegistry 里 (周期 Sync)。
func (tc *testControl) waitRuntime(t *testing.T, name string, lease time.Duration) agent.AgentRuntime {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tc.syncOnce(t, lease)
		for _, n := range tc.rr.List() {
			if n == name {
				rt, err := tc.rr.Pick(&agent.Placement{Prefer: "remote:" + name})
				if err != nil {
					t.Fatalf("Pick: %v", err)
				}
				return rt
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime %q 未在 3s 内注册; 当前 = %v", name, tc.rr.List())
	return nil
}

// runnerFunc 把函数适配成 agent.AgentRunner。
type runnerFunc func(ctx context.Context, prompt string) (string, error)

func (f runnerFunc) Execute(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// echoRuntime 用**真的** localRuntime 收编一个回显 runner (证明 worker 走的是既有
// 本地执行路径, 而不是测试专用的旁路)。
func echoRuntime(name string, caps agent.RuntimeCaps, fn func(ctx context.Context, role, prompt string) (string, error)) agent.AgentRuntime {
	return agent.NewLocalRuntime(name, caps, func(_ context.Context, role, _ string) (agent.AgentRunner, error) {
		return runnerFunc(func(ctx context.Context, prompt string) (string, error) {
			return fn(ctx, role, prompt)
		}), nil
	})
}

// drain 读完事件通道, 返回事件序列 (通道必须被关闭, 否则 range 不结束)。
func drain(t *testing.T, ch <-chan agent.NodeEvent, timeout time.Duration) []agent.NodeEvent {
	t.Helper()
	var evs []agent.NodeEvent
	tmr := time.NewTimer(timeout)
	defer tmr.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-tmr.C:
			t.Fatalf("事件通道 %s 内未关闭, 已收到 %d 条: %+v", timeout, len(evs), evs)
			return evs
		}
	}
}

func kinds(evs []agent.NodeEvent) []agent.NodeEventKind {
	out := make([]agent.NodeEventKind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func terminal(t *testing.T, evs []agent.NodeEvent) agent.NodeEvent {
	t.Helper()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == agent.NodeEventDone || evs[i].Kind == agent.NodeEventFailed {
			return evs[i]
		}
	}
	t.Fatalf("事件流里没有终态事件 (静默成功的典型症状): %+v", kinds(evs))
	return agent.NodeEvent{}
}

// firstTaskID 返回队列里唯一任务的 ID。
func firstTaskID(t *testing.T, tc *testControl) string {
	t.Helper()
	tasks, err := tc.q.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("期望队列里恰好 1 个任务, 实得 %d", len(tasks))
	}
	return tasks[0].ID
}

// clusterTask 造一条 stage 任务 (MaxAttempts=1, 与 Broker 默认一致)。
func clusterTask(id string, payload json.RawMessage, require []string) cluster.Task {
	return cluster.Task{ID: id, Kind: TaskKindStage, Payload: payload, RequireCaps: require, MaxAttempts: 1}
}

// clusterWorker 造一条 worker 注册信息 (默认声明接受 stage)。
func clusterWorker(name string, caps []string) cluster.WorkerInfo {
	return clusterWorkerKinds(name, caps, []string{TaskKindStage})
}

func clusterWorkerKinds(name string, caps, kinds []string) cluster.WorkerInfo {
	return cluster.WorkerInfo{Name: name, Caps: caps, Kinds: kinds}
}

// fileWriterRuntime 一个"会产码"的执行体: 把 files 写进**任务声明的工作目录**。
//
// 为什么直接读 task.Workspace 而不像 echoRuntime 那样走 localRuntime: 三档位要验的
// 恰恰是"worker 把执行放在了哪个目录"。生产里这个目录由 worker 进程级 cwd 决定
// (工具的执行根), 测试里用 task.Workspace 断言 worker 算出来的目录是对的 ——
// 若 worker 把控制面的路径而不是本机检出目录传下来, 这些测试会立刻红。
type fileWriterRuntime struct {
	files   map[string]string
	out     string
	calls   int32
	failMsg string
	// check 写文件之前对工作目录的断言 (如"上一阶段的代码在不在")。
	// 返回错误 ⇒ 阶段失败 —— 与生产症状一致: 看不到上游代码的阶段本就该失败。
	check func(dir string) error
	// after 写完文件之后的动作 (模拟真 Agent 用 Bash 干的事: 自己 git commit、切分支…)。
	after func(dir string) error
	// gate 执行到一半时的同步点 (并发冲突测试用): 非 nil 则在写完文件后等它。
	gate chan struct{}
	// started 收到任务时关闭 (并发编排用)。
	started chan struct{}
}

func (f *fileWriterRuntime) Name() string                    { return "local-writer" }
func (f *fileWriterRuntime) Capabilities() agent.RuntimeCaps { return agent.RuntimeCaps{Bash: true} }
func (f *fileWriterRuntime) Cancel(string, string) error     { return nil }
func (f *fileWriterRuntime) Execute(_ context.Context, task agent.RuntimeNodeTask) (<-chan agent.NodeEvent, error) {
	atomic.AddInt32(&f.calls, 1)
	ch := make(chan agent.NodeEvent, 3)
	go func() {
		defer close(ch)
		ch <- agent.NodeEvent{Kind: agent.NodeEventStarted, RunID: task.RunID, NodeID: task.NodeID}
		if f.started != nil {
			close(f.started)
		}
		if f.failMsg != "" {
			ch <- agent.NodeEvent{Kind: agent.NodeEventFailed, RunID: task.RunID, NodeID: task.NodeID, Err: f.failMsg}
			return
		}
		if f.check != nil {
			if err := f.check(task.Workspace); err != nil {
				ch <- agent.NodeEvent{Kind: agent.NodeEventFailed, RunID: task.RunID, NodeID: task.NodeID,
					Err: "工作区自检失败: " + err.Error()}
				return
			}
		}
		for name, body := range f.files {
			p := filepath.Join(task.Workspace, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				ch <- agent.NodeEvent{Kind: agent.NodeEventFailed, Err: err.Error()}
				return
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				ch <- agent.NodeEvent{Kind: agent.NodeEventFailed, Err: err.Error()}
				return
			}
		}
		if f.after != nil {
			if err := f.after(task.Workspace); err != nil {
				ch <- agent.NodeEvent{Kind: agent.NodeEventFailed, RunID: task.RunID, NodeID: task.NodeID,
					Err: "after 钩子失败: " + err.Error()}
				return
			}
		}
		if f.gate != nil {
			<-f.gate // 卡在这里, 让另一个 worker 先把它的提交推上去
		}
		out := f.out
		if out == "" {
			out = "已写入 " + strconv.Itoa(len(f.files)) + " 个文件"
		}
		ch <- agent.NodeEvent{Kind: agent.NodeEventDone, RunID: task.RunID, NodeID: task.NodeID, Output: out}
	}()
	return ch, nil
}

func (f *fileWriterRuntime) called() int { return int(atomic.LoadInt32(&f.calls)) }

// git 测试脚手架 ─────────────────────────────────────────────────────────────

// newBareRepo 造一个真的裸仓当"约定 git 位置"。
func newBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := gitCmd(context.Background(), "", 30*time.Second, "init", "--bare", "-q", dir); err != nil {
		t.Fatalf("git init --bare: %v %s", err, out)
	}
	return dir
}

// gitOut 在 dir 里跑一条 git 命令并返回输出 (断言用)。
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(context.Background(), dir, 60*time.Second, args...)
	if err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

// gitLsTree 裸仓某分支上的文件清单。
func gitLsTree(t *testing.T, bare, branch string) []string {
	t.Helper()
	out := gitOut(t, bare, "ls-tree", "-r", "--name-only", branch)
	return splitLines(out)
}

// emptyDir t.TempDir() 下的一个空子目录 (git 档要求 worker 工作区为空或已是仓库)。
func emptyDir(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
