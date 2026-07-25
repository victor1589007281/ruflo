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
	taskLease   time.Duration // 队列租约, 0 → 5s
	workerLease time.Duration // worker 注册表租约, 0 → 5s
	grace       time.Duration // 掉线宽限期, 0 → 5s
	poll        time.Duration // 控制面轮询队列间隔, 0 → 5ms
	authSecret  string        // 非空 → 控制面按 httpauth 保护 /cluster/*
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
		Queue: q, Registry: reg,
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

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
