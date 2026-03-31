package hooks

import (
	"context"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// ClaudeCodeEvent describes an event originating from Claude Code hook wiring.
type ClaudeCodeEvent struct {
	Type      string
	Tool      string
	File      string
	Command   string
	SessionID string
	Timestamp time.Time
}

// OfficialHooksBridge maps Claude Code-style lifecycle events to Ruflo hook execution.
type OfficialHooksBridge struct {
	registry *HookRegistry
	executor *HookExecutor
}

// NewOfficialHooksBridge wires registry and executor. If executor is nil, reg defaults to NewRegistry() when nil, then NewExecutor(reg) is used.
func NewOfficialHooksBridge(reg *HookRegistry, exec *HookExecutor) *OfficialHooksBridge {
	if exec == nil {
		if reg == nil {
			reg = NewRegistry()
		}
		exec = NewExecutor(reg)
	}
	return &OfficialHooksBridge{registry: reg, executor: exec}
}

func (b *OfficialHooksBridge) exec(ev HookEvent, hc HookContext) HookResult {
	if b == nil || b.executor == nil {
		return HookResult{Success: true}
	}
	return b.executor.Execute(ev, hc)
}

// Registry returns the configured registry when the bridge was constructed with a non-nil registry.
func (b *OfficialHooksBridge) Registry() *HookRegistry {
	if b == nil {
		return nil
	}
	return b.registry
}

// OnPreToolUse fires HookEventPreToolUse.
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

// OnPostToolUse fires HookEventPostToolUse.
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

// OnPreEdit fires HookEventPreEdit.
func (b *OfficialHooksBridge) OnPreEdit(file string) HookResult {
	hc := HookContext{File: file, Session: map[string]any{}}
	return b.exec(HookEventPreEdit, hc)
}

// OnPostEdit fires HookEventPostEdit.
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

// OnPreCommand fires HookEventPreCommand.
func (b *OfficialHooksBridge) OnPreCommand(cmd string) HookResult {
	hc := HookContext{Command: cmd, Session: map[string]any{}}
	return b.exec(HookEventPreCommand, hc)
}

// OnPostCommand fires HookEventPostCommand.
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

// OnSessionStart fires HookEventSessionStart.
func (b *OfficialHooksBridge) OnSessionStart(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return b.exec(HookEventSessionStart, hc)
}

// OnSessionEnd fires HookEventSessionEnd.
func (b *OfficialHooksBridge) OnSessionEnd(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return b.exec(HookEventSessionEnd, hc)
}

// PreEdit runs PreEdit hooks.
func (b *OfficialHooksBridge) PreEdit(file string) HookResult {
	return b.OnPreEdit(file)
}

// SessionStart runs SessionStart hooks.
func (b *OfficialHooksBridge) SessionStart(sessionID string) HookResult {
	return b.OnSessionStart(sessionID)
}

// SessionEnd runs SessionEnd hooks.
func (b *OfficialHooksBridge) SessionEnd(sessionID string) HookResult {
	return b.OnSessionEnd(sessionID)
}

// SessionLifecycle runs SessionStart then SessionEnd for the same id.
func (b *OfficialHooksBridge) SessionLifecycle(ctx context.Context, sessionID string) (HookResult, HookResult) {
	_ = ctx
	return b.SessionStart(sessionID), b.SessionEnd(sessionID)
}
