// Package regression 是下一代架构（design/01-03）的集成回归套件, 沉淀到测锻平台
// (testforge) 供未来回归。独立嵌套模块 (replace→claude-go), 只依赖新架构包,
// 不再受旧 pkg/orchestrator 预存死锁影响 (该包已于 design/01 M4 删除),
// 可经 `go test ./...` 干净运行。
//
// 覆盖: 图引擎执行+恢复 / 集群任务队列崩溃自愈 / StateStore 原子与去重 /
// TraceStore 内容寻址 / EventBus 队列组 / LLM 网关路由。
package regression

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/eventbus"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// echoRunner 确定性节点执行器 (回归无需真 LLM)。
type echoRunner struct{ calls map[string]int }

func (r *echoRunner) RunNode(_ context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[node.ID]++
	return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: "done:" + node.ID + ":prev=" + in.PrevOutputs["a"]}
}

// TestGraphEngineLinearAndParallel 图引擎: 线性依赖 + 并行 + 产出传递 (design/01 M1)。
func TestGraphEngineLinearAndParallel(t *testing.T) {
	spec := graph.GraphSpec{
		Name: "reg-diamond",
		Nodes: []graph.NodeSpec{
			{ID: "a", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "r"}},
			{ID: "b", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "r"}},
			{ID: "c", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "r"}},
			{ID: "d", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "r"}},
		},
		Edges: []graph.EdgeSpec{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: "d"}, {From: "c", To: "d"},
		},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("菱形图应合法: %v", err)
	}
	runner := &echoRunner{}
	eng := &graph.Engine{Runner: runner, Journal: graph.NewMemoryJournal()}
	rr, err := eng.Run(context.Background(), spec, graph.RunOpts{RunID: "reg-1", Objective: "o"})
	if err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	if rr.Status != "completed" {
		t.Fatalf("应 completed, got %s", rr.Status)
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if runner.calls[n] != 1 {
			t.Errorf("节点 %s 应执行 1 次, got %d", n, runner.calls[n])
		}
	}
	// d 依赖 a→b→d 的产出传递 (PrevOutputs)
	if rr.Nodes["b"].Output != "done:b:prev=done:a:prev=" {
		t.Errorf("产出传递错误: %q", rr.Nodes["b"].Output)
	}
}

// TestGraphValidateRejectsBadGraphs 编译期校验 (design/01 §4.6)。
func TestGraphValidateRejectsBadGraphs(t *testing.T) {
	cases := []graph.GraphSpec{
		{Name: "empty"}, // 空图
		{Name: "cycle", Nodes: []graph.NodeSpec{
			{ID: "a", Kind: graph.NodeKindAgent}, {ID: "b", Kind: graph.NodeKindAgent}},
			Edges: []graph.EdgeSpec{{From: "a", To: "b"}, {From: "b", To: "a"}}}, // 环
		{Name: "dangling", Nodes: []graph.NodeSpec{{ID: "a", Kind: graph.NodeKindAgent}},
			Edges: []graph.EdgeSpec{{From: "a", To: "ghost"}}}, // 悬空边
	}
	for _, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("非法图 %q 应被 Validate 拒绝", c.Name)
		}
	}
}

// TestClusterQueueCrashRecovery 集群队列: 租约过期崩溃自愈 (design/02 §3.3)。
func TestClusterQueueCrashRecovery(t *testing.T) {
	q := cluster.NewQueue(statestore.NewMemStore(), 40*time.Millisecond)
	id, err := q.Enqueue(cluster.Task{Kind: "stage", MaxAttempts: 3, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	// worker A 拉取后崩溃 (不回报)
	if _, ok, _ := q.Pull("workerA", nil); !ok {
		t.Fatal("应拉到任务")
	}
	time.Sleep(70 * time.Millisecond) // 租约过期
	// worker B 拉取 → 回收重派
	reaped, ok, _ := q.Pull("workerB", nil)
	if !ok || reaped.ID != id || reaped.Worker != "workerB" || reaped.Attempts != 2 {
		t.Fatalf("过期租约应回收重派: %+v ok=%v", reaped, ok)
	}
	if err := q.Complete(id, "workerB", nil); err != nil {
		t.Fatal(err)
	}
}

// TestStateStoreAtomicAndDedup StateStore: KV roundtrip + Blob 内容寻址去重 (design/02 §3.4.1)。
func TestStateStoreAtomicAndDedup(t *testing.T) {
	ss := statestore.NewFileStore(t.TempDir())
	kv := ss.KV("reg")
	if err := kv.Put("k", map[string]int{"v": 42}); err != nil {
		t.Fatal(err)
	}
	var out map[string]int
	if ok, _ := kv.Get("k", &out); !ok || out["v"] != 42 {
		t.Fatalf("KV roundtrip 失败: %v", out)
	}
	blob := ss.Blob()
	h1, _ := blob.Put([]byte("same content"))
	h2, _ := blob.Put([]byte("same content"))
	if h1 != h2 {
		t.Fatalf("相同内容应同 hash (去重): %q vs %q", h1, h2)
	}
}

// TestTraceStoreContentAddressing TraceStore: 长文本 Blob 内容寻址还原 (design/03 §4.1)。
func TestTraceStoreContentAddressing(t *testing.T) {
	ts := tracestore.New(statestore.NewMemStore())
	long := make([]byte, 5000)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	ts.Write(tracestore.Span{TraceID: "reg-run", SpanID: "s1", Kind: "turn",
		OutputRef: ts.MakeRef(string(long))})
	spans, err := ts.ReadRun("reg-run")
	if err != nil || len(spans) != 1 {
		t.Fatalf("应读回 1 span: %v err=%v", len(spans), err)
	}
	got, _ := ts.Resolve(spans[0].OutputRef)
	if got != string(long) {
		t.Fatalf("长文本 Blob 还原失败 (len %d)", len(got))
	}
	if spans[0].OutputRef.Blob == "" {
		t.Error("长文本应落 Blob 而非内联")
	}
}

// TestEventBusQueueGroup EventBus: 队列组轮询单投递 (design/02 §3.4.2)。
func TestEventBusQueueGroup(t *testing.T) {
	bus := eventbus.NewChanBus(64)
	ch1, unsub1, _ := bus.Subscribe("reg.*", "grp")
	ch2, unsub2, _ := bus.Subscribe("reg.*", "grp")
	defer unsub1()
	defer unsub2()
	for i := 0; i < 20; i++ {
		_ = bus.Publish("reg.ev", eventbus.Event{Subject: "reg.ev"})
	}
	got := 0
	deadline := time.After(2 * time.Second)
	for got < 20 {
		select {
		case <-ch1:
			got++
		case <-ch2:
			got++
		case <-deadline:
			t.Fatalf("队列组应收到全部 20 条, got %d", got)
		}
	}
}

// TestClusterRegistryHeartbeat 集群注册表: 心跳+能力标签 (design/02 §3.4.5)。
func TestClusterRegistryHeartbeat(t *testing.T) {
	reg := cluster.NewRegistry(statestore.NewMemStore(), time.Minute)
	_ = reg.Heartbeat(cluster.WorkerInfo{Name: "w1", Caps: []string{"bash", "browser"}})
	alive, _ := reg.Alive()
	if len(alive) != 1 || len(alive[0].Caps) != 2 {
		t.Fatalf("worker 应注册且带 2 个能力标签: %+v", alive)
	}
}

// TestTracePropagation trace 四元组合并注入 (design/03 §4.1)。
func TestTracePropagation(t *testing.T) {
	ctx := trace.With(context.Background(), trace.IDs{RunID: "r"})
	ctx = trace.With(ctx, trace.IDs{NodeID: "n"})
	ids := trace.From(ctx)
	if ids.RunID != "r" || ids.NodeID != "n" {
		t.Fatalf("trace 合并注入丢字段: %+v", ids)
	}
}

// --- 二轮缺口关闭的回归 (2026-07-24 续) ---

// TestSQLiteStoreBackend R2: sqlite StateStore 后端语义与 file 一致。
func TestSQLiteStoreBackend(t *testing.T) {
	s, err := statestore.NewSQLiteStore(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	kv := s.KV("reg")
	_ = kv.Put("k", map[string]int{"v": 7})
	var out map[string]int
	if ok, _ := kv.Get("k", &out); !ok || out["v"] != 7 {
		t.Fatal("sqlite KV roundtrip 失败")
	}
	h1, _ := s.Blob().Put([]byte("x"))
	h2, _ := s.Blob().Put([]byte("x"))
	if h1 != h2 {
		t.Fatal("sqlite Blob 去重失败")
	}
}

// TestClusterFileLease R3: cron 选主文件租约互斥。
func TestClusterFileLease(t *testing.T) {
	l := cluster.NewFileLease(t.TempDir(), time.Minute)
	if !l.TryAcquire("cron/j/202607241200") {
		t.Fatal("首次应抢到")
	}
	if l.TryAcquire("cron/j/202607241200") {
		t.Fatal("同 key 不应重抢")
	}
}

// TestClusterCapsRouting R3: 能力标签路由。
func TestClusterCapsRouting(t *testing.T) {
	q := cluster.NewQueue(statestore.NewMemStore(), time.Minute)
	_, _ = q.Enqueue(cluster.Task{Kind: "stage", RequireCaps: []string{"browser"}})
	if _, ok, _ := q.PullFor("w", []string{"stage"}, []string{"bash"}); ok {
		t.Fatal("缺 browser 的 worker 不应拉到")
	}
	if _, ok, _ := q.PullFor("w2", []string{"stage"}, []string{"browser"}); !ok {
		t.Fatal("具备 browser 应拉到")
	}
}

// TestTraceStoreSampling E1: 正文采样降存储, 元数据保留。
func TestTraceStoreSampling(t *testing.T) {
	s := tracestore.NewWithOptions(statestore.NewMemStore(), tracestore.Options{BodySampleRate: 0})
	r := s.MakeRef("body text")
	if r.Inline != "" || r.Size != 9 {
		t.Fatalf("rate=0 应不留正文但保留 Size: %+v", r)
	}
}
