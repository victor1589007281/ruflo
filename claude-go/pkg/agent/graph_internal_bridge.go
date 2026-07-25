package agent

// graph_internal_bridge.go —— 把引擎内 `internal_hook` 的事件注册进**同一条**
// graph.HookBus (design/01 §4.5 表的第 2 行)。
//
// ---------------------------------------------------------------------------
// 这一步补的是哪块缺口
// ---------------------------------------------------------------------------
//
// §4.5 现在只剩两套 hook: `pkg/hooks` (外部 shell/HTTP/gRPC/OPA) 与
// `pkg/engine/internal_hook` (AutoCompact/MemoryInject/ToolGate/LoopDetector/
// CircuitBreaker...)。设计对后者的处置是"**保留在引擎内**, 但注册进同一总线的
// turn|tool 作用域, 事件对图层可见" —— 不是把它搬到图层来。
//
// 缺口是"可见": 改造前一个节点跑了 40 轮 turn、被 compact 过 3 次、有 2 次工具
// 调用被 ToolGate 拦下, 图层能看到的只有节点的最终产出。于是"这个阶段为什么慢/
// 为什么产出短"在事后完全不可解释 —— 那些干预全发生在图层看不见的地方。
//
// ---------------------------------------------------------------------------
// 三条硬边界 (都不是随手定的)
// ---------------------------------------------------------------------------
//
//  1. **绝不阻塞**。`internal_hook.Observer.Observe` 无返回值 (类型层面就阻塞不了),
//     这里再把 `bus.Emit` 的决策显式丢弃 —— 双保险。灰度期的观测桥不该新增阻塞
//     路径, 这与 `teamGraphHooks` "绝不返回 deny" 是同一条纪律。
//
//  2. **绝不影响既有日志形态**。turn|tool 事件在 `teamGraphHooks` 里只做**计数
//     聚合**, 不逐条打日志: 一次团队运行有成千上万次 turn/tool 干预, 逐条 INFO
//     会把日志淹掉 (而日志是本仓排障的主要入口)。计数在节点收尾时随既有的
//     `graph.node.post` 一起打出, 且**为空时一个字段都不加** —— 没有内置 hook
//     介入的运行, 日志逐字节与改造前一致。
//
//  3. **不进 StageResult / journal**。那两处是用户可感知的产出形态与恢复真源,
//     往里加字段属行为变更; 观测数据走日志与总线即可。
//
// 注入点在 `runGraphSpec` (每次团队运行一处), 而不是各 runner 里 —— ctx 从
// `eng.Run` 一路流到 `sessionAgentRunner.Execute` → `engine.Query` → queryLoop,
// 中间各层无需知情。走 ctx 而不是给 HookChain 加字段的理由见 bus.go 文件头第 2 点。

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/trace"
)

// internalHookBridge 实现 internal_hook.Observer, 把事件转成 graph.HookEvent
// 打进同一条总线。
type internalHookBridge struct {
	bus   graph.HookBus
	runID string
}

// Observe 把一条内置 hook 事件投递到总线。
//
// NodeID/TurnID 从 ctx 的 trace 四元组取: 图引擎在 execNode 里注入过 NodeID
// (`trace.With(ctx, trace.IDs{NodeID: evID})`), 于是这条 turn 事件天然带着
// "发生在哪个节点里"。没有它, 一个团队几十个节点的 turn 事件会糊成一团。
func (b *internalHookBridge) Observe(ctx context.Context, ev internal_hook.Event) {
	if b == nil || b.bus == nil {
		return
	}
	ids := trace.From(ctx)
	runID := ids.RunID
	if runID == "" {
		runID = b.runID
	}
	// 决策**刻意丢弃**: 见文件头边界 1。
	_ = b.bus.Emit(ctx, graph.HookEvent{
		Scope:  graph.HookScope(ev.Scope),
		Phase:  ev.Phase,
		RunID:  runID,
		NodeID: ids.NodeID,
		TurnID: ids.TurnID,
		Payload: map[string]any{
			"hook":  ev.Hook,
			"turn":  ev.TurnCount,
			"error": ev.Err,
		},
	})
}

// withInternalHookBridge 把桥接观测者挂进 ctx。bus 为空总线时不挂 ——
// NopBus 会把每一条事件都扔掉, 挂上去只是白付一次 ctx.Value + Emit 的开销。
func withInternalHookBridge(ctx context.Context, bus graph.HookBus, runID string) context.Context {
	if bus == nil {
		return ctx
	}
	if _, isNop := bus.(graph.NopBus); isNop {
		return ctx
	}
	return internal_hook.WithObserver(ctx, &internalHookBridge{bus: bus, runID: runID})
}

// ---------------------------------------------------------------------------
// teamGraphHooks 侧的消费: 按节点聚合计数
// ---------------------------------------------------------------------------

// noteInternalHook 记一次内置 hook 干预 (按节点 × hook 名计数)。
//
// 为什么是计数而不是明细: 明细的价值在单次排障, 而单次排障已经有 llm.jsonl 与
// 结构化日志; 图层需要的是"这个阶段被干预了多少次、被谁干预" 这个可对比的量,
// 它能直接回答"为什么这一轮比上一轮慢/短"。存明细则要在内存里挂一个随 turn 数
// 线性增长的切片, 长跑团队会把它变成一个静默的内存增长点。
func (h *teamGraphHooks) noteInternalHook(ev graph.HookEvent) {
	nodeID := ev.NodeID
	if nodeID == "" {
		// 没有 NodeID 的 turn 事件 (节点外的 agent 调用) 归到一个哑桶, 不丢 ——
		// 丢掉会让"总数对不上"这种最容易发现的异常也看不出来。
		nodeID = "(no-node)"
	}
	name, _ := ev.Payload["hook"].(string)
	if name == "" {
		name = ev.Phase
	}
	h.mu.Lock()
	if h.internalHits == nil {
		h.internalHits = map[string]map[string]int{}
	}
	if h.internalHits[nodeID] == nil {
		h.internalHits[nodeID] = map[string]int{}
	}
	h.internalHits[nodeID][name]++
	h.mu.Unlock()
}

// internalHookSummary 取某节点的干预计数摘要 (形如 "AutoCompact=3,ToolGate=1")。
// 无干预时返回空串 —— 调用方据此**完全不加字段**, 保证既有日志形态不变。
func (h *teamGraphHooks) internalHookSummary(nodeID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return formatHitCounts(h.internalHits[nodeID])
}

// formatHitCounts 把计数表格式化成稳定序 (按 hook 名字典序) 的摘要串。
// 字典序是刻意的: map 迭代序随机, 日志里同一份内容每次不同形会让 diff 排障失效。
func formatHitCounts(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+strconv.Itoa(m[n]))
	}
	return strings.Join(parts, ",")
}
