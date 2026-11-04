// clientdeco_ctx.go — 单次 run 的出站客户端装饰（Moqi R-H2 / R-H17）。
//
// # 为什么是装饰函数而不是新增 Config 字段
//
// 引擎的 *api.Client 是**会话级共享**的装配对象：`WithModel` 每轮都会基于它再造
// 一个浅副本（相位路由 / 升级 / plan 客户端各有自己的分支）。把"本轮缓存计划"
// 或"本轮回退观测器"写到 Config 或 APIClient 上，会让 A 轮的计划滞留在对象上、
// 被 B 轮的请求带走 —— 而 B 轮的 prompt 布局与那个断点毫无关系，表现只是缓存命中
// 率无声漂移，不报错。
//
// ctx 按 run 隔离，天然没有这个问题（与 ephemeral.go / runoverrides.go 同一条
// 理由：同一个引擎会被并发 run 共用）。
//
// # 为什么必须在发送前**最后**应用
//
// 装饰返回的是浅副本。若在上面的 `client.WithModel(...)` / PlanClient 赋值之前
// 应用，后续那一步会基于**原始** client 再造副本，本轮装饰被静默丢弃 —— 这正是
// "建成未通电"的典型形状。所以调用点在 PlanClient 路由之后、StreamMessage 之前。
package engine

import (
	"context"

	"github.com/anthropic/claude-go/pkg/api"
)

// ClientDecorator 接收引擎当轮选定的客户端，返回真正用于发送的客户端。
//
// 返回 nil 表示"不替换"：调用方可以直接 return c。实现应当是**纯装饰**——
// 不得改变 Model / 限流器 / HTTP 客户端等既有语义，只附加本轮需要的字段。
type ClientDecorator func(*api.Client) *api.Client

// clientDecoratorKey 是未导出的 ctx 键：只有本包能写，别的包无法伪造。
type clientDecoratorKey struct{}

// WithClientDecorator 把本轮装饰写进 ctx（多次调用以后者为准）。
//
// nil 装饰等价于不设置：调用方可以无条件调用，不必先判空。
func WithClientDecorator(ctx context.Context, fn ClientDecorator) context.Context {
	if ctx == nil || fn == nil {
		return ctx
	}
	return context.WithValue(ctx, clientDecoratorKey{}, fn)
}

// ClientDecoratorFromContext 取本轮装饰；没有时返回 nil。
func ClientDecoratorFromContext(ctx context.Context) ClientDecorator {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(clientDecoratorKey{}).(ClientDecorator)
	return fn
}
