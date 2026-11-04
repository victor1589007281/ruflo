// hooktrace.go — internal_hook 的 per-request trace 观测面在 engine 包的公开转发层
// (Moqi R-H7)。
//
// # 为什么要有这一层
//
// trace 观测者必须挂在**某一轮请求的 ctx** 上 (见 internal_hook/bus.go): 装在
// HookChain 字段上会让并发 run 串台。宿主 (pkg/harness) 装配 ctx 时需要把观测者
// 放进去, 但它已经依赖 pkg/engine —— 让它再直接依赖 pkg/engine/internal_hook 会把
// "engine 的内部实现包"变成上层契约的一部分, 将来想换实现就得改全部宿主。
//
// 这里只做类型别名 + 两个转发函数, 不新增任何行为: 装配方写
// engine.WithHookTraceObserver(ctx, obs) 即可, obs 实现 engine.HookTraceObserver。
//
// 注意观测者拿到的 HookTrace 只含**类别标签**, 不含用户正文 (见 HookTrace.Result)。
package engine

import (
	"context"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
)

// HookTrace 是单个内置 hook 一次执行/跳过的 trace 记录。
type HookTrace = internal_hook.HookTrace

// HookTraceObserver 接收每次内置 hook 执行的 trace。实现必须并发安全。
type HookTraceObserver = internal_hook.HookTraceObserver

// WithHookTraceObserver 把 trace 观测者放进 ctx (nil 观测者原样返回 ctx)。
//
// 装配方应在把 ctx 交给 engine.Query / SubmitStreamBlocks **之前**调用一次;
// ctx 会一路流到 HookChain.Execute, 中间各层无需知情。
func WithHookTraceObserver(ctx context.Context, obs HookTraceObserver) context.Context {
	return internal_hook.WithHookTraceObserver(ctx, obs)
}

// HookTraceObserverFrom 取出 ctx 中的 trace 观测者 (没有则返回 nil)。
func HookTraceObserverFrom(ctx context.Context) HookTraceObserver {
	return internal_hook.HookTraceObserverFrom(ctx)
}
