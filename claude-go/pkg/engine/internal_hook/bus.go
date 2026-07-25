package internal_hook

// bus.go —— 内置 Hook 链的**观测出口** (design/01 §4.5)。
//
// ---------------------------------------------------------------------------
// 这是什么, 不是什么
// ---------------------------------------------------------------------------
//
// design/01 §4.5 给 internal_hook 的归宿是: "保留在引擎内 (性能敏感、每 turn
// 触发), 但注册进同一总线的 turn|tool 作用域, **事件对图层可见**"。
//
// 所以这里做的是"让图层看得见", 不是"让图层管得着"。三处刻意的设计:
//
//  1. **Observer.Observe 没有返回值**。不是"约定不要阻塞", 是**类型上就说不出
//     阻塞这句话**。要求里那条"新增 hook 触发点不能变成新的阻塞路径"如果靠约定
//     维持, 迟早有人给它加个 error 返回、再有人在调用点 `if err != nil { return }`,
//     于是每 turn 触发的最热路径上多出一条谁都没设计过的失败路径。
//
//  2. **观测者由 ctx 携带, 不是 HookChain 的字段**。HookChain 随引擎/agent
//     构造一次并长期复用, 而总线是**每次团队运行一条** (runGraphSpec 里造)。
//     把它挂成字段就要在每次运行前后改同一个 HookChain 实例的状态 —— 多个团队
//     并发跑同一个 agent 池时会互相串台。ctx 天然是 per-request 的。
//
//  3. **只在 hook 真的介入时发事件** (返回了非 nil HookResult, 或报错)。
//     每 turn 有 11 个 phase, 绝大多数 turn 里绝大多数 hook 什么都不做 ——
//     给"什么都没做"发事件既淹没信号又白烧最热路径的 CPU。图层要看的是
//     "这一轮被 compact 了 / 这次工具调用被 gate 拦了", 不是"链跑过了"。
//
// 无观测者时的开销 = 每次 HookChain.Execute 一次 ctx.Value 查找, 零分配。

import "context"

// Event 一条内置 hook 事件 (映射到总线的 Scope × Phase)。
//
// 不引用 pkg/graph 的类型: pkg/engine 是热路径底层包, 反向依赖 graph/agent
// 那棵重依赖树是倒挂 (与 ConstraintSet 那处同一条边界纪律 —— engine 只定义
// 最小接口, 由上层做结构性适配)。
type Event struct {
	// Scope "turn" 或 "tool"。由 phase 决定, 见 scopeOf。
	Scope string
	// Phase 内置 hook 的 phase 名 (PreToolUse / PostToolUse / PreCompact ...)。
	Phase string
	// Hook 触发这条事件的内置 hook 名 (InternalHook.Name())。
	Hook string
	// Err hook 自身报错时非空 (链会中断, 图层需要看得到是谁断的)。
	Err string
	// TurnCount 第几轮 (HookContext.TurnCount 原样透传), 供图层对齐 turn。
	TurnCount int
}

// Observer 内置 hook 事件的观测者。**没有返回值**: 见文件头第 1 点。
type Observer interface {
	Observe(context.Context, Event)
}

type observerKey struct{}

// WithObserver 把观测者放进 ctx。上层 (pkg/agent) 在起图运行时注入一次,
// ctx 沿 图引擎 → 节点 runner → agent runner → engine.Query → queryLoop
// 一路流到 HookChain, 中间各层无需知道它的存在。
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

// scopeOf phase → 总线作用域。
//
// 只有两档: 工具相关的 phase 归 tool, 其余归 turn。不给每个 phase 单独造一个
// 作用域 —— 作用域是给总线消费方分流用的粗粒度维度, 细分靠 Phase 字段,
// 造 11 个作用域只会让消费方的 switch 变成第二张模式表 (§1.2 记的漂移形态)。
func scopeOf(p InternalHookPhase) string {
	switch p {
	case PhasePreToolUse, PhasePostToolUse:
		return "tool"
	default:
		return "turn"
	}
}

// emit 向 ctx 中的观测者投递一条事件 (观测者为 nil 时是空操作)。
func emit(ctx *HookContext, obs Observer, phase InternalHookPhase, hookName, errText string) {
	if obs == nil {
		return
	}
	obs.Observe(ctx.Ctx, Event{
		Scope:     scopeOf(phase),
		Phase:     phase.String(),
		Hook:      hookName,
		Err:       errText,
		TurnCount: ctx.TurnCount,
	})
}
