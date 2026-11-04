// qh_p95_test.go — Q-H4 的**实测 p95**（不是 testing.B 的均值 ns/op）。
//
// # 为什么不用 benchmark
//
// `testing.B` 报 ns/op 是均值；需求写的是 p95。均值达标不能推出 p95 达标
// （p95 >= 均值），而且均值会被少数极快样本稀释。这里逐次用 time.Now()
// 取样，迭代结束后按 nearest-rank 取第 95 百分位，阈值写死；失败时打印
// 实测 p95 与样本数。
//
// # 精心保持"不含模型/工具/检索"
//
//   - RunTurn：进程内 benchTransport（无 socket）+ 固定 SSE 字节，模型生成
//     时间为 0；不装 sink/trace/context provider，工具从不被调用。
//   - 上下文补丁组装：provider 是常量补丁，直接调用
//     adaptContextProviderWithBudget 返回的闭包，测的是补丁到手之后的加工。
//   - 事件发射：Emitter 装 no-op sink；sink 耗时不计（目标的前提是非阻塞 sink）。
package harness

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	qh4RunTurnSamples  = 500
	qh4RunTurnP95Limit = 10 * time.Millisecond
	qh4ContextSamples  = 1000
	qh4ContextP95Limit = 5 * time.Millisecond
	qh4EmitSamples     = 5000
	qh4EmitP95Limit    = 1 * time.Millisecond
)

// qh4NearestRankP95 返回 nearest-rank p95：升序第 ceil(0.95*N) 个样本。
// 用整数运算避免浮点边界（N=20 时 ceil(19.0)=19，不是 18）。
func qh4NearestRankP95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// ceil(95*n/100) - 1 = floor((95*n + 99)/100) - 1
	idx := (95*len(sorted)+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// qh4AssertP95 断言实测 p95 低于阈值，失败时打印实测值与样本数。
func qh4AssertP95(t *testing.T, name string, samples []time.Duration, limit time.Duration) {
	t.Helper()
	p95 := qh4NearestRankP95(samples)
	var max time.Duration
	for _, s := range samples {
		if s > max {
			max = s
		}
	}
	t.Logf("Q-H4 %s: p95=%s (阈值 %s), max=%s, n=%d", name, p95, limit, max, len(samples))
	if p95 >= limit {
		t.Fatalf("Q-H4 %s 未达标: 实测 p95=%s >= 阈值 %s (n=%d, max=%s)",
			name, p95, limit, len(samples), max)
	}
}

// qh4NewRunner 与 bench_test.go 的 newBenchRunner 同构，但用 *testing.T
// 而不是 *testing.B。复用其 benchTransport 与 runnerSSEText（同包测试文件）。
func qh4NewRunner(t *testing.T) (*Runner, *engine.QueryEngine) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(noopTool{})

	client := &api.Client{
		BaseURL: "http://p95.invalid", APIKey: "p95", Model: "p95-model",
		Client:            &http.Client{Transport: benchTransport{body: runnerSSEText("好")}},
		RetryCount:        1,
		FirstTokenTimeout: 30 * time.Second,
		CallTimeout:       30 * time.Second,
	}
	pm := prompt.NewManager(t.TempDir())
	pm.OverridePrompt = "你是执行者"

	eng := engine.NewQueryEngine(
		&engine.Config{Model: "p95-model", MaxTurns: 3, MaxRunTokens: -1},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, pm)

	return NewRunner(RunnerOptions{Engine: eng}), eng
}

// TestP95_HarnessOverhead 对应 Q-H4 (a)：RunTurn 全链路本地开销 p95 < 10ms。
//
// 测量的是 harness + engine 本地路径（请求序列化、SSE 解析、事件翻译、
// Seq 分配、结果骨架、enter/leave）。模型生成与网络为 0；无 sink、无 trace、
// 无检索、工具从不执行。
func TestP95_HarnessOverhead(t *testing.T) {
	r, eng := qh4NewRunner(t)
	req := TurnRequest{
		SessionID: "p95-session",
		TurnID:    "p95-turn",
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: "你好"}},
	}
	emit := func(TurnEvent) {}
	ctx := context.Background()

	// 预热：首次调用包含 engine 装配后的惰性初始化，不计入样本。
	for i := 0; i < 20; i++ {
		eng.ClearMessages()
		if _, err := r.RunTurn(ctx, req, emit); err != nil {
			t.Fatalf("预热 RunTurn 失败: %v", err)
		}
	}

	samples := make([]time.Duration, 0, qh4RunTurnSamples)
	for i := 0; i < qh4RunTurnSamples; i++ {
		// 历史必须每轮清空：不清的话 engine 的消息遍历/压缩成本会随
		// 迭代线性增长，测到的就不是"一轮的开销"。清理在计时之外。
		eng.ClearMessages()
		start := time.Now()
		_, err := r.RunTurn(ctx, req, emit)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("第 %d 次 RunTurn 失败: %v", i, err)
		}
		samples = append(samples, elapsed)
	}

	qh4AssertP95(t, "RunTurn harness 开销", samples, qh4RunTurnP95Limit)
}

// TestP95_ContextPatchAssembly 对应 Q-H4 (b)：上下文补丁组装 p95 < 5ms。
//
// 直接调用 adaptContextProviderWithBudget 返回的 provider 闭包：20 块（含
// 5 个重复 ID）→ 按 ID 去重 → 转 types.ContentBlock → 降级判定 → sink 回报。
// provider 是常量补丁，不含真实检索。
func TestP95_ContextPatchAssembly(t *testing.T) {
	req := TurnRequest{
		SessionID: "p95-session",
		TurnID:    "p95-context",
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: "你好"}},
	}
	provider := adaptContextProviderWithBudget(
		benchContextProvider{patch: benchContextPatch()},
		req,
		0, // 0 = 不限预算：与生产里未配预算时的路径一致
		func(ContextSelection, string) {},
	)
	view := engine.EphemeralView{
		SentUpTo: 0,
		History:  benchHistory(),
		UserText: "你好",
	}
	ctx := context.Background()

	// 夹具自检：确认组装路径真的走了，而不是变成空操作。
	blocks, degraded, err := provider(ctx, view)
	if err != nil {
		t.Fatalf("provider 失败: %v", err)
	}
	if len(blocks) != 15 || degraded != "context_duplicate_id_dropped" {
		t.Fatalf("夹具失效: blocks=%d degraded=%q（期望 15 / context_duplicate_id_dropped）",
			len(blocks), degraded)
	}

	for i := 0; i < 20; i++ {
		if _, _, err := provider(ctx, view); err != nil {
			t.Fatalf("预热 provider 失败: %v", err)
		}
	}

	samples := make([]time.Duration, 0, qh4ContextSamples)
	for i := 0; i < qh4ContextSamples; i++ {
		start := time.Now()
		_, _, err := provider(ctx, view)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("第 %d 次 provider 失败: %v", i, err)
		}
		samples = append(samples, elapsed)
	}

	qh4AssertP95(t, "上下文补丁组装", samples, qh4ContextP95Limit)
}

// TestP95_EventEmit 对应 Q-H4 (c)：事件发射（到 no-op sink）p95 < 1ms。
//
// 每轮 BeginTurn 只调一次；循环内累积写入会跨过默认 1024 容量进入淘汰分支，
// 这是真实长流的稳态。sink 是非阻塞空函数 —— 1ms 目标的前提就在这里。
func TestP95_EventEmit(t *testing.T) {
	em := NewEmitter(EmitterOptions{Sink: func(TurnEvent) {}})
	em.BeginTurn("p95-turn")

	for i := 0; i < 100; i++ {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "片"}); err != nil {
			t.Fatalf("预热 Emit 失败: %v", err)
		}
	}

	samples := make([]time.Duration, 0, qh4EmitSamples)
	for i := 0; i < qh4EmitSamples; i++ {
		start := time.Now()
		err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "片段"})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("第 %d 次 Emit 失败: %v", i, err)
		}
		samples = append(samples, elapsed)
	}

	qh4AssertP95(t, "事件发射", samples, qh4EmitP95Limit)
}
