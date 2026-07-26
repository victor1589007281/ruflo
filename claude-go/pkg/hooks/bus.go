package hooks

// bus.go —— 外部 Hook (shell/HTTP/gRPC/OPA/MCP/plugin/function) 的**观测出口**
// (design/01 §4.5 归宿表第 1 行的 `ExternalHook` 适配器)。
//
// ---------------------------------------------------------------------------
// 这一步补的是哪块缺口 (不是"让外部 hook 生效")
// ---------------------------------------------------------------------------
//
// 容易误判的一点先写清楚: 用户配置的外部 hook 在**图模式下本来就会触发** ——
// 它们挂在 runner 内部, 而图引擎调的就是这个 runner:
//
//	graph stageNodeRunner.RunNode → ExecuteSingleStage → we.factory
//	  → SessionManager.CreateAgentRunner → sessionAgentRunner.Execute
//	  → ExecuteSubagentStartHooks / RunPreToolUseHooks / ExecuteSubagentStopHooks
//
// 所以缺口不在"生效", 在**可见**: 改造前一个图节点里的 agent 可以被用户的
// PreToolUse hook 拦掉 5 次工具调用、被 SubagentStart hook 注入过上下文, 图这一层
// 看到的只有该节点的最终产出 —— "这个阶段为什么慢/为什么产出短/为什么那个工具没调"
// 在事后完全不可解释。
//
// design/01 §4.5 对这套的处置是"配置格式不变, **事件名映射到 Scope×Phase**,
// 决策语义 deny/block/approve 保留"。本文件做映射与投递; 决策语义**一个字没动**
// (仍由 hooks.go 里各调用方原样解释), 见下面第 1 点。
//
// ---------------------------------------------------------------------------
// 四条硬边界 (照 pkg/engine/internal_hook/bus.go 的同一套纪律)
// ---------------------------------------------------------------------------
//
//  1. **绝不新增阻塞路径, 且是类型层面保证的**。`Observer.Observe` 没有返回值 ——
//     不是"约定不要阻塞", 是**类型上就说不出阻塞这句话**。靠约定维持的话, 迟早有人
//     给它加个 error 返回、再有人在调用点 `if err != nil { return }`, 于是每次工具
//     调用的最热路径上多出一条谁都没设计过的失败路径。
//     外部 hook 自己的 deny/block/approve **照旧生效**(那是它本来就有的能力),
//     只是把"它做了什么决策"作为**载荷**告诉图层, 图层不据此改变执行。
//
//  2. **观测者由 ctx 携带, 不是 Runner 的长期字段**。总线是**每次团队运行一条**
//     (runGraphSpec 里造), 而 hook 配置随 SessionManager 长期复用。挂成长期字段就要
//     在每次运行前后改同一个实例的状态 —— 多个团队并发跑时互相串台。
//     Runner 只在**构造时**收一个 ctx (NewRunnerWithContext), 而生产里那个真正跑在
//     图节点内的 Runner 恰好是 per-Execute 构造的 (feishu/session.go 的
//     sessionAgentRunner.Execute), 于是"per-run" 天然成立。
//
//  3. **只在真的有 hook 配置时发事件**。每个事件的 findHooks 为空就直接返回 ——
//     绝大多数部署一条外部 hook 都没配, 那时本文件的全部开销 = 每个事件一次
//     `len(hooks)==0` 判断 (那个判断改造前就有), 零 ctx 查找、零分配、零事件。
//     "hook 链跑过了" 不是信号, "hook 真的介入了" 才是。
//
//  4. **一次事件只发一条**, 即使该事件配了 3 个 hook。图层对这类事件只做计数
//     (teamGraphHooks), 发 3 条会让 `ext:PreToolUse=3` 实际只是一次工具调用 ——
//     那种计数没人敢用。与 subagent_span.go 的"只在结束时发一条"同一条理由。
//
// ---------------------------------------------------------------------------
// 为什么 Phase 用事件名原文, 而不是压成 pre|post|failure
// ---------------------------------------------------------------------------
//
// design/01 §4.5 的相位词表是 `pre|post|failure|idle|stalled|...`, 直觉做法是把
// 26 个事件名压到这四档。**但那是不可逆的信息丢失**: PreTurn / PreCompact /
// PreRequest 会全部塌成 `turn|pre`, 消费方想还原只能去 Payload 里反查 —— 等于把
// 维度信息塞进载荷再取出来。`pkg/engine/internal_hook` 走的就是"Phase = phase 名原文"
// (`graph.HookEvent.Phase` 的注释已写明这一口径), 这里沿用同一口径, 于是总线上
// turn|tool 作用域的相位命名保持一致, 消费方不需要认识第二套词表。
//
// 内部/外部两套在总线上靠 `Payload["source"]` 与 hook 名前缀 (`ext:`) 区分。

import (
	"context"

	"github.com/anthropic/claude-go/pkg/types"
)

// 总线作用域取值 (与 pkg/graph.HookScope 的字符串一致)。
//
// 这里用裸字符串而不引用 `pkg/graph.HookScope`: pkg/hooks 是被 pkg/engine 依赖的
// 底层包, 反向依赖 graph/agent 那棵树是倒挂 —— 与 internal_hook/bus.go 同一条
// 边界纪律 (底层只定义最小结构, 由上层做结构性适配)。
const (
	ScopeTool    = "tool"
	ScopeTurn    = "turn"
	ScopeSession = "session"
	ScopeNode    = "node"
)

// SourceExternal 写进 `Payload["source"]` 的标记: 这条事件来自 pkg/hooks 这一套。
//
// 必须有它: 总线上 `turn|PostToolUse` 既可能来自 internal_hook 的 ToolGate,
// 也可能来自用户配置的 PostToolUse shell hook。少了这个字段, 事后看到一条被拦的
// 工具调用无法判断"是我们内置的闸拦的还是用户策略拦的" —— 这两件事的处置完全不同。
const SourceExternal = "pkg/hooks"

// hookNamePrefix 事件在总线上的 hook 名前缀 (`ext:SubagentStart`)。
//
// 带前缀而不是裸事件名: teamGraphHooks 按 hook 名聚合计数并打进节点收尾日志,
// 裸事件名会与 internal_hook 的 hook 名 (AutoCompact/ToolGate/...) 混在同一个
// 命名空间里 —— 将来某个内置 hook 若叫 `Stop`, 两类干预的计数会静默相加。
const hookNamePrefix = "ext:"

// Event 一条外部 hook 事件 (映射到总线的 Scope × Phase)。
type Event struct {
	// Scope "tool"|"turn"|"session"|"node", 由事件名决定, 见 ScopeOf。
	Scope string
	// Phase 事件名原文 (PreToolUse / SubagentStart / TeammateIdle ...)。
	// 用原文的理由见文件头最后一节。
	Phase string
	// Hook 总线上的 hook 名 (`ext:` + 事件名)。
	Hook string
	// Matched 本次事件匹配到的 hook 配置条数 (≥1, 否则不会发事件)。
	// 有它才能区分"配了 3 个 hook 都放行"与"只配了 1 个"。
	Matched int
	// Decision hook 返回的决策原文 (deny|block|approve|""), 空 = 无 hook 表态。
	// **纯载荷**: 决策的执行仍在 hooks.go 各调用方手里, 图层不解释它 (边界 1)。
	Decision string
	// Tool 工具名 (仅 tool 作用域的三个事件有)。
	Tool string
	// Role 角色名 (SubagentStart/SubagentStop/TeammateIdle 有)。
	Role string
	// Err hook 自身执行报错时非空 (外部 hook 是子进程/网络调用, 会失败;
	// 改造前这类失败被 `continue` 静默吞掉, 图层永远看不到)。
	Err string
}

// Observer 外部 hook 事件的观测者。**没有返回值**: 见文件头边界 1。
type Observer interface {
	Observe(context.Context, Event)
}

type observerKey struct{}

// WithObserver 把观测者放进 ctx。上层 (pkg/agent) 在起图运行时注入一次。
func WithObserver(ctx context.Context, obs Observer) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, observerKey{}, obs)
}

// ObserverFrom 取出 ctx 中的观测者 (没有则返回 nil)。
func ObserverFrom(ctx context.Context) Observer {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(observerKey{}).(Observer)
	return obs
}

// ScopeOf 事件名 → 总线作用域。
//
// 四档而不是给每个事件单造一个: 作用域是给总线消费方**分流**用的粗粒度维度,
// 细分靠 Phase (事件名原文)。造 26 个作用域只会让消费方的 switch 变成第二张事件表。
//
// ⚠️ TeammateIdle / TaskCompleted 映到 node 但**当前无产生方**: 它们由
// ProductionTeamManager 那个**长期复用**的 Runner 触发 (teams.go 的 ptm.hookRunner),
// 而长期实例不能携带 per-run 的观测 ctx (边界 2 的串台问题)。映射先写对, 等这两个
// 事件有了 per-run 的触发点再通电 —— 不映射的话将来接线的人得先来这里补一遍。
func ScopeOf(ev types.HookEvent) string {
	switch ev {
	case types.HookEventPreToolUse, types.HookEventPostToolUse, types.HookEventPostToolUseFailure:
		return ScopeTool
	case types.HookEventSessionStart, types.HookEventSessionEnd,
		types.HookEventSubagentStart, types.HookEventSubagentStop:
		return ScopeSession
	case types.HookEventTeammateIdle, types.HookEventTaskCompleted:
		return ScopeNode
	default:
		// 其余全是每轮/每请求触发的引擎内事件 (PreTurn/PreCompact/PreRequest/
		// Stop/OnError/OnChunk/...), 归 turn —— 与 internal_hook.scopeOf 同口径。
		return ScopeTurn
	}
}

// observe 向 ctx 中的观测者投递一条事件 (观测者为 nil 时是空操作)。
//
// 挂在 Runner 上而不是包级函数: 观测 ctx 是 Runner 的构造参数 (边界 2),
// 各调用点没有 ctx 可传。
func (r *Runner) observe(ev Event) {
	if r == nil || r.obsCtx == nil {
		return
	}
	obs := ObserverFrom(r.obsCtx)
	if obs == nil {
		return
	}
	if ev.Hook == "" {
		ev.Hook = hookNamePrefix + ev.Phase
	}
	if ev.Scope == "" {
		ev.Scope = ScopeOf(types.HookEvent(ev.Phase))
	}
	obs.Observe(r.obsCtx, ev)
}

// decisionOf 从 HookOutput 里取"这个 hook 表了什么态"。
//
// 两个字段都看: PreToolUse 那条路径用 `Decision`, Stop 那条用 `ContinueDecision`
// (hooks.go 的 executeStopLikeHooks)。只看一个的后果是另一条路径的决策在总线上
// 恒为空 —— 那正是本仓反复吃过的"看着有数据其实半边是死的"形态。
func decisionOf(out *types.HookOutput) string {
	if out == nil {
		return ""
	}
	if out.Decision != "" {
		return out.Decision
	}
	return out.ContinueDecision
}
