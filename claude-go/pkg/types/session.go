// session.go — session 身份在引擎侧的载体（保 harness → engine → tool 单向依赖）。
//
// # 为什么 session 也要走 ctx
//
// `engine.Config.SessionID` 是**部署级**字段，而引擎是会话级复用的：harness
// Runner 用同一个 *QueryEngine 服务任意多个 session。把 session 写进 Config，
// 多会话部署下 `ToolContext.SessionID` 会恒等于最后装配的那一个值 ——
// EnterPlanMode/ExitPlanMode 的会话级 plan flag、按 session 命名的计划文件目录，
// 全都会**静默**串到别的会话上。这类串号不会报错：plan mode 照样工作，
// 只是工作在了别人的会话里。
//
// 与 owner 同一个理由、同一个位置（pkg/types 是叶子包，engine 与 tool 都能引用，
// 而两者都不能 import harness）。两个 carrier 分文件而不是合一个：它们的读取方
// 与默认值语义不同 —— owner 缺失要归一到 "local"（隔离判据不能恒真），
// session 缺失只能保持空串（空 session 是合法的"无会话状态"，
// 编一个假 session 会让两个无关的调用共享 plan flag）。
package types

import "context"

type sessionCtxKey struct{}

// WithSession 把 session 身份写进 ctx。空串会被写入（见文件头：空 session 合法），
// 由 HasSession 区分"显式空"与"没写过"。
func WithSession(ctx context.Context, session string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, sessionCtxKey{}, session)
}

// SessionFromContext 读取 ctx 中的 session；没有时返回空串。
func SessionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(sessionCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// HasSession 报告 ctx 里是否**显式**写过 session。
//
// 引擎用它区分"本轮声明了空 session"与"没有人告诉过我 session"：前者应当
// 覆盖 Config.SessionID（调用方明确说不要会话状态），后者应当退回 Config。
func HasSession(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(sessionCtxKey{}).(string)
	return ok
}
