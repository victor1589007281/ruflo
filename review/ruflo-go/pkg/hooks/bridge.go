package hooks

import (
	"context"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// 本文件：Claude Code 官方钩子事件到 Ruflo HookExecutor 的桥接层。
// 将 OnPreToolUse/OnPostEdit 等调用转换为对应 HookEvent 与 HookContext，保持与 executor 便捷方法一致的字段约定。

// ClaudeCodeEvent 描述来自 Claude Code 侧钩子的原始事件载荷（可选用于扩展，当前桥接主要用专用 On* 方法）。
type ClaudeCodeEvent struct {
	Type      string    // 事件类型标识
	Tool      string    // 工具名
	File      string    // 文件路径
	Command   string    // 命令行
	SessionID string    // 会话 ID
	Timestamp time.Time // 事件发生时间
}

// OfficialHooksBridge 持有 HookRegistry 与 HookExecutor，将 Claude Code 生命周期映射为 Ruflo 钩子执行。
type OfficialHooksBridge struct {
	registry *HookRegistry // 可选暴露给外部注册；构造时若从 nil 创建则内部持有
	executor *HookExecutor // 实际执行钩子链
}

// NewOfficialHooksBridge 若 exec 非 nil 直接使用；否则在 reg 为 nil 时 NewRegistry，再 NewExecutor(reg)。
func NewOfficialHooksBridge(reg *HookRegistry, exec *HookExecutor) *OfficialHooksBridge {
	if exec == nil {
		if reg == nil {
			reg = NewRegistry()
		}
		exec = NewExecutor(reg)
	}
	return &OfficialHooksBridge{registry: reg, executor: exec}
}

// exec 在桥接非空时委托 executor.Execute；否则短路成功。
func (b *OfficialHooksBridge) exec(ev HookEvent, hc HookContext) HookResult {
	if b == nil || b.executor == nil {
		return HookResult{Success: true}
	}
	return b.executor.Execute(ev, hc)
}

// Registry 返回构造时传入的注册表指针（可能为 nil）。
func (b *OfficialHooksBridge) Registry() *HookRegistry {
	if b == nil {
		return nil
	}
	return b.registry
}

// OnPreToolUse 触发 HookEventPreToolUse（Args: tool, raw_args）。
func (b *OfficialHooksBridge) OnPreToolUse(tool, args string) HookResult {
	hc := HookContext{
		Session: map[string]any{},
		Args: map[string]any{
			"tool":     tool,
			"raw_args": args,
		},
	}
	return b.exec(HookEventPreToolUse, hc)
}

// OnPostToolUse 触发 HookEventPostToolUse。
func (b *OfficialHooksBridge) OnPostToolUse(tool, result string) HookResult {
	hc := HookContext{
		Session: map[string]any{},
		Args: map[string]any{
			"tool":   tool,
			"result": result,
		},
	}
	return b.exec(HookEventPostToolUse, hc)
}

// OnPreEdit 触发 HookEventPreEdit。
func (b *OfficialHooksBridge) OnPreEdit(file string) HookResult {
	hc := HookContext{File: file, Session: map[string]any{}}
	return b.exec(HookEventPreEdit, hc)
}

// OnPostEdit 触发 HookEventPostEdit。
func (b *OfficialHooksBridge) OnPostEdit(file, diff string) HookResult {
	hc := HookContext{
		File: file,
		Args: map[string]any{"diff": diff},
		Session: map[string]any{
			"diff": diff,
		},
	}
	return b.exec(HookEventPostEdit, hc)
}

// OnPreCommand 触发 HookEventPreCommand。
func (b *OfficialHooksBridge) OnPreCommand(cmd string) HookResult {
	hc := HookContext{Command: cmd, Session: map[string]any{}}
	return b.exec(HookEventPreCommand, hc)
}

// OnPostCommand 触发 HookEventPostCommand。
func (b *OfficialHooksBridge) OnPostCommand(cmd string, exitCode int) HookResult {
	hc := HookContext{
		Command: cmd,
		Args: map[string]any{
			"exit_code": exitCode,
		},
		Session: map[string]any{},
	}
	return b.exec(HookEventPostCommand, hc)
}

// OnSessionStart 触发 HookEventSessionStart。
func (b *OfficialHooksBridge) OnSessionStart(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return b.exec(HookEventSessionStart, hc)
}

// OnSessionEnd 触发 HookEventSessionEnd。
func (b *OfficialHooksBridge) OnSessionEnd(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return b.exec(HookEventSessionEnd, hc)
}

// PreEdit 同 OnPreEdit（别名便于与 Executor 命名对齐）。
func (b *OfficialHooksBridge) PreEdit(file string) HookResult {
	return b.OnPreEdit(file)
}

// SessionStart 同 OnSessionStart。
func (b *OfficialHooksBridge) SessionStart(sessionID string) HookResult {
	return b.OnSessionStart(sessionID)
}

// SessionEnd 同 OnSessionEnd。
func (b *OfficialHooksBridge) SessionEnd(sessionID string) HookResult {
	return b.OnSessionEnd(sessionID)
}

// SessionLifecycle 顺序调用 SessionStart 与 SessionEnd（ctx 预留，当前未传入执行器超时链）。
func (b *OfficialHooksBridge) SessionLifecycle(ctx context.Context, sessionID string) (HookResult, HookResult) {
	_ = ctx
	return b.SessionStart(sessionID), b.SessionEnd(sessionID)
}
