package agent

// graph_external_bridge.go —— design/01 §4.5 归宿表**第 1 行**的 `ExternalHook`
// 适配器: 把 `pkg/hooks` (外部 shell/HTTP/gRPC/OPA/MCP/plugin/function hook) 的事件
// 按"事件名 → Scope×Phase"映射后注册进**同一条** `graph.HookBus`。
//
// ---------------------------------------------------------------------------
// 先说清这一步**不是**在做什么
// ---------------------------------------------------------------------------
//
// 读者容易从 §4.5 的"三套 hook 并存"推断出「外部 hook 在图模式下不生效」——
// **不是这样**。逐跳核实过调用链: 图路径 `stageNodeRunner.RunNode` →
// `ExecuteSingleStage` → `we.factory` → `SessionManager.CreateAgentRunner` →
// `sessionAgentRunner.Execute`, 而后者会触发 `ExecuteSubagentStartHooks` /
// `RunPreToolUseHooks` / `ExecuteSubagentStopHooks`。用户配置的 hook 照样触发,
// 因为它们挂在 runner 内部而图引擎调的就是这个 runner。
//
// 所以这一步补的是**可见性**, 与 `graph_internal_bridge.go` 完全同构:
// 改造前一个节点里的 agent 被用户的 PreToolUse hook 拦掉 5 次工具调用、
// SubagentStart hook 报错 3 次, 图层看到的只有该节点的最终产出。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的两种做法
// ---------------------------------------------------------------------------
//
//  1. **不让 `pkg/hooks` 直接持有 `graph.HookBus`**。总线是**每次团队运行一条**
//     (`runGraphSpec` 里造), 而 hook 配置随 `SessionManager` 长期复用 —— 挂成字段
//     就要在每次运行前后改同一个实例的状态, 多个团队并发时互相串台。这与
//     `internal_hook/bus.go` 文件头第 2 点、`subagent_span.go` 第 2 条是同一条理由,
//     所以同样走 ctx 通道。
//
//  2. **不复用 `internal_hook.Observer` 这一个通道**(那样只要一个桥)。两套事件的
//     词表完全不同: internal_hook 的 `Event` 只有 `Hook/Phase/Err/TurnCount`,
//     而外部 hook 事件必须带 `Decision`(deny/block/approve, §4.5 明写"决策语义保留")
//     与 `Matched`(配了几条 hook)。硬塞进去要么给 internal_hook 的热路径结构体加两个
//     它永远不用的字段, 要么把这两个量编码进字符串再解回来。更要紧的是: 混在同一个
//     通道后, "这次工具调用是内置 ToolGate 拦的, 还是用户策略拦的" 就分不出来了 ——
//     这两件事的处置完全不同 (前者调参, 后者是用户自己写的治理规则)。
//
// ---------------------------------------------------------------------------
// 三条硬边界
// ---------------------------------------------------------------------------
//
//  1. **绝不阻塞、绝不改变外部 hook 的决策**。`hooks.Observer.Observe` 无返回值
//     (类型层面就阻塞不了), 这里再把 `bus.Emit` 的决策显式丢弃 —— 双保险。
//     外部 hook 自己的 deny/block/approve 仍**原样生效**(它本来就有这个能力,
//     执行仍在 `pkg/hooks` 各调用方手里), 图层只是**看见**它做了什么决策。
//     这与 `teamGraphHooks` "绝不返回 deny" 是同一条纪律。
//
//  2. **绝不影响既有日志形态**。外部 hook 事件在 `teamGraphHooks` 里与 turn|tool
//     一样只做**计数聚合**, 在节点收尾时随既有的 `graph.node.post` 一起打出,
//     且**为空时一个字段都不加**。绝大多数部署一条外部 hook 都没配 ⇒
//     `findHooks` 恒空 ⇒ 一条事件都不发 ⇒ 日志逐字节与改造前一致。
//
//  3. **不进 StageResult / journal**。那两处是用户可感知的产出形态与恢复真源,
//     往里加字段属行为变更; 观测数据走日志与总线即可。

import (
	"context"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/trace"
)

// externalHookBridge 实现 hooks.Observer, 把外部 hook 事件转成 graph.HookEvent
// 打进同一条总线。
type externalHookBridge struct {
	bus   graph.HookBus
	runID string
}

// Observe 把一条外部 hook 事件投递到总线。
//
// NodeID/TurnID 从 ctx 的 trace 四元组取 (图引擎在 execNode 里注入过 NodeID),
// 于是这条事件天然带着"发生在哪个节点里" —— 没有它, 一个团队几十个节点的外部
// hook 事件会糊成一团。
func (b *externalHookBridge) Observe(ctx context.Context, ev hooks.Event) {
	if b == nil || b.bus == nil {
		return
	}
	ids := trace.From(ctx)
	runID := ids.RunID
	if runID == "" {
		runID = b.runID
	}
	payload := map[string]any{
		"hook": ev.Hook,
		// source 必须有: 总线上 tool|PostToolUse 既可能来自内置 ToolGate 也可能来自
		// 用户的 PostToolUse hook, 少了它两者不可分辨 (见 hooks/bus.go SourceExternal)。
		"source":  hooks.SourceExternal,
		"matched": ev.Matched,
	}
	// 以下三项**只在非空时才放进载荷**: 恒放会让"这个事件没有角色"与"角色是空串"
	// 不可区分, 而 payloadStr 对两者返回同一个值。
	if ev.Decision != "" {
		payload["decision"] = ev.Decision
	}
	if ev.Tool != "" {
		payload["tool"] = ev.Tool
	}
	if ev.Role != "" {
		payload["role"] = ev.Role
	}
	if ev.Err != "" {
		payload["error"] = ev.Err
	}
	// 决策**刻意丢弃**: 见文件头边界 1。
	_ = b.bus.Emit(ctx, graph.HookEvent{
		Scope:   graph.HookScope(ev.Scope),
		Phase:   ev.Phase,
		RunID:   runID,
		NodeID:  ids.NodeID,
		TurnID:  ids.TurnID,
		Payload: payload,
	})
}

// withExternalHookBridge 把桥接观测者挂进 ctx。bus 为空总线时不挂 ——
// NopBus 会把每一条事件都扔掉, 挂上去只是白付一次 ctx.Value + Emit 的开销。
func withExternalHookBridge(ctx context.Context, bus graph.HookBus, runID string) context.Context {
	if bus == nil {
		return ctx
	}
	if _, isNop := bus.(graph.NopBus); isNop {
		return ctx
	}
	return hooks.WithObserver(ctx, &externalHookBridge{bus: bus, runID: runID})
}

// ---------------------------------------------------------------------------
// teamGraphHooks 侧的消费
// ---------------------------------------------------------------------------
//
// 刻意**不写新的消费函数**: 外部 hook 事件与 internal_hook 事件在图层的用法完全
// 相同 (按节点 × hook 名计数, 节点收尾时汇进一条日志), 所以直接复用
// `noteInternalHook`。hook 名靠 `ext:` 前缀区分两类来源 (hooks/bus.go 的
// hookNamePrefix), 于是同一份摘要里能同时读到
// `AutoCompact=3,ToolGate=1,ext:PreToolUse=5` —— 内置与用户策略的干预并列可比。
//
// 为什么不给外部事件单开一张计数表: 那要在 StageResult/日志里多一个字段, 且
// "这个阶段一共被干预了多少次"要靠调用方把两张表相加 —— 相加这件事一定有人漏做。
