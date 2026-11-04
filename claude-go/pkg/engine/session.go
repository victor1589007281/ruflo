// session.go — session 身份在引擎侧的解析（与 owner.go 同构）。
//
// 优先级与 owner 一致：ctx > Config。理由见 pkg/types/session.go 文件头 ——
// 引擎被多个 session 复用，Config.SessionID 是装配期写死的单值，读它等于让所有
// 会话共用一个身份。
//
// 唯一的差别是**默认值**：owner 缺失归一到 "local"（否则隔离判据在"两个空串"上
// 恒真），session 缺失保持空串（空 session 是合法的"无会话状态"；编一个假
// session 会让两个互不相关的调用共享同一份会话级状态，比空更坏）。
package engine

import (
	"context"

	"github.com/anthropic/claude-go/pkg/types"
)

// sessionFrom 解析本次 run 的 session。
//
// HasSession 而不是 "SessionFromContext != """：调用方可以显式声明空 session
// （"本轮不要会话级状态"），那与"没人告诉我"必须能分开 —— 前者要覆盖 Config，
// 后者要退回 Config。用非空判断会把显式空当成缺失，于是 Config.SessionID
// 又冒出来了，而调用方的意图正好相反。
func (e *QueryEngine) sessionFrom(ctx context.Context) string {
	if types.HasSession(ctx) {
		return types.SessionFromContext(ctx)
	}
	if e != nil {
		return e.Config.SessionID
	}
	return ""
}
