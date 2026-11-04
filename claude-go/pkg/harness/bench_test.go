// bench_test.go — Moqi Q-H4（harness 开销）的可重复观测口径。
//
// # 三条目标（design/07 §5）
//
//	harness 开销（不含模型/工具/检索）  p95 < 10ms
//	上下文补丁组装                      p95 < 5ms
//	事件发射                            p95 < 1ms
//
// # 为什么这里没有阈值断言
//
// `testing.B` 报的是 **均值 ns/op**，不是 p95；而且 CI 机器上 ns/op 会随负载
// 波动一个数量级。写 `if ns > 阈值 { b.Fatal() }` 只会得到一个随机红灯，让人
// 学会忽略它 —— 那比没有验收更坏。所以本文件的约定是：benchmark 只负责产出
// 数字，阈值写在注释里作为**人读的观察口径**，由人对照。
//
// 另一点必须说清楚：ns/op 是均值，拿它对照 p95 目标时是**偏乐观**的（p95 ≥ 均值）。
// 因此"均值已超目标"一定意味着不达标；"均值远低于目标"只是达标的一个必要
// 条件，不能单独证明 p95 达标。真正的 p95 需要用 `-benchtime` 多跑几轮看分布，
// 或者上 /benchstat，那是人读数字时的后续动作，不是本文件能替代的。
//
// # 切分原则（为什么只测这些）
//
// Q-H4 的原文是"不含模型/工具/检索"。所以：
//   - 网络、模型生成、工具执行、真实检索**都不进**测量路径；
//   - 但 harness 为这些环节做的**本地固定开销**（身份分配、结果骨架、事件翻译、
//     上下文块去重、事件环形缓冲）要算进来，那正是这条质量目标管的东西。
//
// 每个 benchmark 上方都写明了"测到了什么 / 没测到什么"。
package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

// ============================================================================
// 夹具
// ============================================================================

// benchTransport 是 api.Client 的进程内 RoundTripper。
//
// 为什么不用 httptest.NewServer：httptest 要起真实 TCP 监听，测到的东西里就混进了
// 环回网络的系统调用开销 —— 而 Q-H4 明确把网络排除在外。这里用自定义 Transport
// 直接返回一段固定的 SSE 字节，不经过任何 socket，于是 ns/op 里只剩下
// "harness + engine 本地路径" 的开销。
//
// 它仍然走**真的** api.Client SSE 解析与 engine queryLoop：这正是要测的部分
// （事件翻译、drain、RunTurn 前后处理），只是把模型生成时间和网络时间钉成 0。
type benchTransport struct{ body string }

func (t benchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Request:    req,
	}, nil
}

// newBenchRunner 按生产构造路径（NewQueryEngine）装配一个 Runner，
// 只把上游换成本进程 Transport。
func newBenchRunner(b *testing.B) (*Runner, *engine.QueryEngine) {
	b.Helper()

	reg := tool.NewRegistry()
	reg.Register(noopTool{})

	client := &api.Client{
		BaseURL: "http://bench.invalid", APIKey: "bench", Model: "bench-model",
		Client: &http.Client{Transport: benchTransport{body: runnerSSEText("好")}},
		// 固定为 1：假 Transport 不会失败，重试设置只是为了不落到
		// StreamMessage 里 "<=0 取 4" 的默认分支。
		RetryCount:        1,
		FirstTokenTimeout: 30 * time.Second,
		CallTimeout:       30 * time.Second,
	}
	pm := prompt.NewManager(b.TempDir())
	pm.OverridePrompt = "你是执行者"

	eng := engine.NewQueryEngine(
		&engine.Config{
			Model: "bench-model",
			// MaxRunTokens 显式负值 = 关闭 run 级预算。否则环境里若设了
			// CLAUDE_GO_MAX_RUN_TOKENS，benchmark 会在跑到上限后整轮终止，
			// 数字随环境漂移。
			MaxRunTokens: -1,
			MaxTurns:     3,
		},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, pm)

	return NewRunner(RunnerOptions{Engine: eng}), eng
}

func benchTurnRequest() TurnRequest {
	return TurnRequest{
		SessionID: "bench-session",
		TurnID:    "bench-turn",
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: "你好"}},
	}
}

// ============================================================================
// 1. harness 开销
// ============================================================================

// BenchmarkHarnessOverhead_RunTurn 测 RunTurn 的**整条本地路径**：
//
//	emptyResult/身份字段 → resolveModel → checkModalities → EnsureUser(nil sink 跳过)
//	→ enter/leave（session + run 槽位）→ 预算闸构造 → 逐轮上下文接线
//	→ engine.SubmitStreamBlocks（假 Transport 即时返回）→ drain 事件翻译
//	→ 成本归集 → 终态事件 → classifyTerminal。
//
// 测到了什么：一轮完整 RunTurn 里 **harness 与 engine 的本地固定开销**，包括
// 请求 JSON 序列化、SSE 帧解析、StreamEvent → TurnEvent 翻译、Seq 分配。
//
// 没测到什么：
//   - 模型生成时间（Transport 即时返回，等价于 0）；
//   - 真实网络（无 socket，环回也不算）；
//   - 工具执行（noopTool 从未被模型调用，SSE 里没有 tool_use）；
//   - 检索（未装配 ContextProvider）；
//   - 宿主持久化（Sink 为 nil）、trace 落库（Trace 为 nil）、磁盘 I/O。
//
// 为什么这样切分：Q-H4 要的是"不含模型/工具/检索"的 harness 自耗。用进程内
// Transport 而不是 httptest，正是为了不把环回网络糊进这个数字。
//
// 每轮开始前调 ClearMessages：跑 b.N 次时历史会无限增长，不清理的话
// engine 的压缩/遍历成本会随 b.N 线性上升，测出来的就不是"一轮的开销"。
// 这个清理动作本身也在计时区间内（一次加锁 + 置 nil），计入的是真实存在的
// 每轮簿记成本。
//
// 观察口径：ns/op 对应 Q-H4 的 p95 < 10ms。均值远低于 10ms 只是必要条件。
func BenchmarkHarnessOverhead_RunTurn(b *testing.B) {
	ctx := context.Background()

	b.Run("no_emit", func(b *testing.B) {
		r, eng := newBenchRunner(b)
		req := benchTurnRequest()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			eng.ClearMessages()
			if _, err := r.RunTurn(ctx, req, nil); err != nil {
				b.Fatalf("RunTurn 失败: %v", err)
			}
		}
	})

	// 带 emit 的变体：真实宿主都会传 emit，RunTurn 会额外套一层
	// 计数 + Seq 分配 + 互斥的 wrapper。两个子基准的差值就是这层 wrapper
	// 加一次空回调的成本。
	b.Run("noop_emit", func(b *testing.B) {
		r, eng := newBenchRunner(b)
		req := benchTurnRequest()
		emit := func(TurnEvent) {}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			eng.ClearMessages()
			if _, err := r.RunTurn(ctx, req, emit); err != nil {
				b.Fatalf("RunTurn 失败: %v", err)
			}
		}
	})
}

// BenchmarkHarnessOverhead_RunTurnEntry 测 RunTurn 在**未装配 engine** 时的入口
// 固定开销：emptyResult（身份字段 + 默认 Terminal）→ NewError 分类包装 → 返回。
//
// 测到了什么：RunTurn 每次调用无论如何都要付的那部分（结果骨架分配、身份归一、
// 错误分类）。
//
// 没测到什么：enter/leave、预算闸、上下文接线、drain、sink —— 这些在真实轮里
// 也各自有成本，但它们的入口判据是 engine 非 nil，这条路径到不了。所以它只能
// 当作**下界**读，不能当成"harness 开销"的完整答案。
func BenchmarkHarnessOverhead_RunTurnEntry(b *testing.B) {
	r := NewRunner(RunnerOptions{})
	req := benchTurnRequest()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := r.RunTurn(ctx, req, nil)
		if err == nil || res == nil {
			b.Fatal("未装配 engine 时应返回非 nil 结果与错误")
		}
	}
}

// BenchmarkHarnessOverhead_DrainStreamEvents 单独测 drain：把一段**已经产出的**
// StreamEvent 流翻译成 TurnEvent 并填充 TurnResult。
//
// 测到了什么：每个事件的翻译成本（delta 累积、usage/terminal 归并、Seq 分配、
// emit 回调）—— 这正是 RunTurn 里随事件数线性增长的那部分。
//
// 没测到什么：模型产出事件的时间（夹具预先把事件填进 channel）、工具执行、
// engine 侧的 SSE 解析（那部分在 RunTurn 基准里，这段只测 harness 的 drain）。
//
// 为什么单独切出来：RunTurn 的整体数字里，drain 的占比会随回复长度变化；
// 单测它才能回答"事件多了以后 harness 自己会不会成为瓶颈"。
//
// 夹具成本（每次迭代新建并填充 channel）计入 ns/op，因此这个数字是 drain
// 开销的**上界**，不是精确值。
func BenchmarkHarnessOverhead_DrainStreamEvents(b *testing.B) {
	const deltas = 64
	events := make([]types.StreamEvent, 0, deltas+1)
	for i := 0; i < deltas; i++ {
		events = append(events, types.StreamEvent{
			Kind: types.StreamEventDelta, DeltaText: "片段", BlockIndex: 0,
		})
	}
	events = append(events, types.StreamEvent{
		Kind:    types.StreamEventMessageDone,
		Message: &types.Message{Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "完"}}},
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch := make(chan types.StreamEvent, len(events))
		for _, ev := range events {
			ch <- ev
		}
		close(ch)

		res := &TurnResult{}
		// 复刻 runTurn 里的 emitEvent wrapper（互斥 + Seq 分配）：它是 harness
		// 每个事件都要付的成本，不是夹具噪音。
		var emitMu sync.Mutex
		var seq int64
		drain(context.Background(), ch, func(ev TurnEvent) {
			emitMu.Lock()
			seq++
			ev.Seq = seq
			emitMu.Unlock()
		}, res, nil, nil, nil, "", "", "")

		if seq == 0 {
			b.Fatal("drain 未发出任何事件")
		}
	}
}

// ============================================================================
// 2. 上下文补丁组装
// ============================================================================

// benchContextProvider 返回固定补丁，不检索、不访问磁盘。
type benchContextProvider struct{ patch ContextPatch }

func (p benchContextProvider) ProvideTurnContext(_ context.Context, _ TurnContext) (ContextPatch, error) {
	return p.patch, nil
}

// benchContextPatch 造 20 个块、其中 5 个重复 ID：
//
//	id b00..b14 各一次（15 个唯一），随后 b00..b04 再各来一次（5 个重复）。
//
// 这样去重后有 15 个块，且 adaptContextProvider 会把这次去重标成
// "context_duplicate_id_dropped" 降级 —— 两个行为都走到。
func benchContextPatch() ContextPatch {
	blocks := make([]ContextBlock, 0, 20)
	for i := 0; i < 15; i++ {
		blocks = append(blocks, ContextBlock{
			ID:       fmt.Sprintf("b%02d", i),
			Text:     strings.Repeat("一条证据。", 20),
			Priority: i,
		})
	}
	for i := 0; i < 5; i++ {
		blocks = append(blocks, ContextBlock{
			ID:   fmt.Sprintf("b%02d", i),
			Text: "重复出现但内容不同的同一 ID",
		})
	}
	return ContextPatch{Blocks: blocks, Trace: "retrieval: 20 hits"}
}

func benchHistory() []types.Message {
	history := make([]types.Message, 0, 8)
	for i := 0; i < 8; i++ {
		history = append(history, types.Message{
			UUID: fmt.Sprintf("m%d", i),
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{{
				Type: types.ContentBlockText,
				Text: fmt.Sprintf("第 %d 轮用户输入", i),
			}},
		})
	}
	return history
}

// BenchmarkContextPatchAssembly 测 adaptContextProvider + dedupeBlocks 的完整
// 组装路径：
//
//	TurnContext 组装（含 recentTurns 从 History 抽摘要）
//	→ provider.ProvideTurnContext（固定补丁，无检索）
//	→ dedupeBlocks（按 ID 保留第一个 + 丢空块 + 转 types.ContentBlock）
//	→ 降级判定（20 入 / 15 出 ⇒ context_duplicate_id_dropped）
//	→ sink 记录块 ID。
//
// 测到了什么：harness 自己把一个 ContextPatch 变成引擎内容块的全部本地成本。
//
// 没测到什么：**真实检索**（provider 是固定返回，Q-H4 明确排除检索）、
// 模型调用、token 裁剪（harness 不做）、引擎侧 appendEphemeralBlocks
// （那在 engine 的请求组装里，见 RunTurn 基准）。
//
// 为什么这样切分：Q-H4 的"上下文补丁组装 p95 < 5ms"管的是**补丁到手之后**
// 的加工，不是检索本身。所以把 provider 钉成常量，测出来的就是纯组装。
//
// sink 传一个空函数而不是 nil：生产装配一定带 sink（它把块 ID 写进 trace），
// 传 nil 会让这条路径少一次调用，数字偏乐观。
//
// 观察口径：ns/op 对应 p95 < 5ms。
func BenchmarkContextPatchAssembly(b *testing.B) {
	req := benchTurnRequest()
	p := adaptContextProvider(
		benchContextProvider{patch: benchContextPatch()},
		req,
		func(ids []string, trace string) {},
	)
	view := engine.EphemeralView{
		SentUpTo: 0,
		History:  benchHistory(),
		UserText: "你好",
	}
	ctx := context.Background()

	// 夹具自检（不是性能断言）：确认 20 入 15 出、降级标签正确，
	// 防止将来改夹具后 benchmark 名不副实。
	blocks, degraded, err := p(ctx, view)
	if err != nil {
		b.Fatalf("provider 调用失败: %v", err)
	}
	if len(blocks) != 15 {
		b.Fatalf("去重后应剩 15 块, 实际 %d", len(blocks))
	}
	if degraded != "context_duplicate_id_dropped" {
		b.Fatalf("降级标签应为 context_duplicate_id_dropped, 实际 %q", degraded)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := p(ctx, view); err != nil {
			b.Fatalf("provider 调用失败: %v", err)
		}
	}
}

// ============================================================================
// 3. 事件发射
// ============================================================================

// BenchmarkEventEmit 测 Emitter.Emit 的单次发射：
//
//	Clock 取时 → 加锁 → 终态/TurnID 校验 → Seq 分配 → 环形缓冲写入
//	→ 解锁 → Sink 调用（这里没有 sink）。
//
// 测到了什么：Q-H4 "事件发射 p95 < 1ms" 直接对应的那条路径，包含 Seq 分配
// 与环形缓冲的两种写入（未满填充 / 满后淘汰）。BeginTurn 只在计时开始前调一次，
// 循环内累积的写入会跨过 1024 容量进入淘汰分支 —— 这是真实长流的稳态。
//
// 没测到什么：宿主 Sink 的扇出成本（见 WithSlowSink 那条，它专门划这个边界）、
// 网络/SSE 序列化（那是 sessionapi 出口层的事）、Replay。
//
// 为什么用默认缓冲：1024 是 EmitterOptions 的默认值，也是宿主不显式配置时
// 实际拿到的容量。换容量只改变淘汰点的位置，不改变单次发射的复杂度。
//
// 观察口径：ns/op 对应 p95 < 1ms，且**只在不装慢 sink 的前提下成立**。
func BenchmarkEventEmit(b *testing.B) {
	em := NewEmitter(EmitterOptions{})
	em.BeginTurn("bench-turn")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "片段"}); err != nil {
			b.Fatalf("Emit 失败: %v", err)
		}
	}
}

// BenchmarkEventEmit_WithSlowSink 是前面三条数字的**边界条件**：Sink 每次
// sleep 1ms，证明发射耗时会被慢 sink 直接拉爆。
//
// 为什么必须要有这一条：Emitter.Emit 是**同步**调用 Sink 的（锁外，但不异步）。
// 一个只看 BenchmarkEventEmit 的人会得出"发射 <1ms，达标"的结论，然后把
// 一个写磁盘/写同步 channel 的 Sink 装上去。那时数字就不再由 harness 决定，
// 而是由 Sink 决定 —— 这条基准把这个前提显式钉出来：**1ms 目标的成立
// 依赖于 Sink 非阻塞**（EmitterOptions.Sink 的注释也要求如此）。
//
// 测到了什么：Emit 的端到端延迟 = 自身开销 + Sink 耗时，且二者是串行的。
//
// 没测到什么：并发 Emit 下多个慢 Sink 互相排队的情形（那需要多 goroutine
// 夹具，且 Sink 顺序本就不保证与 Seq 一致）。
//
// 预期观察值：≈ 1ms/op（1000000+ ns/op），即超过 1ms 目标 —— 这不是 harness
// 不达标，而是这条基准在演示"目标的前提被打破时会发生什么"。
func BenchmarkEventEmit_WithSlowSink(b *testing.B) {
	slow := func(TurnEvent) { time.Sleep(time.Millisecond) }
	em := NewEmitter(EmitterOptions{Sink: slow})
	em.BeginTurn("bench-turn")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := em.Emit(TurnEvent{Kind: EventDelta, Delta: "片段"}); err != nil {
			b.Fatalf("Emit 失败: %v", err)
		}
	}
}
