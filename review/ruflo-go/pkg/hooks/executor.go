package hooks

import (
	"context"
	"errors"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// HookExecutor runs registered hooks with optional per-hook timeouts.
type HookExecutor struct {
	Registry *HookRegistry
	// DefaultTimeout applies when ExecuteWithTimeout is called with zero timeout.
	DefaultTimeout time.Duration
}

// NewExecutor builds an executor with the given registry.
func NewExecutor(reg *HookRegistry) *HookExecutor {
	if reg == nil {
		reg = NewRegistry()
	}
	return &HookExecutor{
		Registry:       reg,
		DefaultTimeout: 30 * time.Second,
	}
}

// Execute runs hooks for the given event sequentially; aggregates warnings; stops on first blocking result.
func (e *HookExecutor) Execute(ev HookEvent, hc HookContext) HookResult {
	return e.ExecuteWithTimeout(context.Background(), ev, hc, 0)
}

// ExecuteWithTimeout runs each hook with a per-hook timeout (or DefaultTimeout if dur==0).
func (e *HookExecutor) ExecuteWithTimeout(ctx context.Context, ev HookEvent, hc HookContext, dur time.Duration) HookResult {
	if e == nil || e.Registry == nil {
		return HookResult{Success: true}
	}
	if dur <= 0 {
		dur = e.DefaultTimeout
	}
	regs := e.Registry.GetForEvent(ev)
	agg := HookResult{Success: true, Data: make(map[string]any)}
	for _, reg := range regs {
		res := runOne(ctx, reg.Handler, hc, dur)
		agg.Warnings = append(agg.Warnings, res.Warnings...)
		if res.Data != nil {
			for k, v := range res.Data {
				agg.Data[k] = v
			}
		}
		if res.Abort {
			agg.Abort = true
			agg.Success = res.Success
			agg.Error = res.Error
			if res.Message != "" {
				agg.Message = res.Message
			}
			return agg
		}
		if res.Error != "" {
			agg.Success = false
			agg.Error = res.Error
			agg.Message = res.Message
			return agg
		}
		if !res.Success {
			agg.Success = false
			agg.Message = res.Message
			return agg
		}
		if res.Message != "" && agg.Message == "" {
			agg.Message = res.Message
		}
	}
	return agg
}

func runOne(ctx context.Context, h HookHandler, hc HookContext, dur time.Duration) HookResult {
	if h == nil {
		return HookResult{Success: true}
	}
	cctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	done := make(chan HookResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				select {
				case done <- HookResult{Success: false, Error: "panic in hook"}:
				case <-cctx.Done():
				}
			}
		}()
		r := h(cctx, hc)
		// Avoid blocking forever if the caller already returned on timeout.
		select {
		case done <- r:
		case <-cctx.Done():
		}
	}()
	select {
	case r := <-done:
		return r
	case <-cctx.Done():
		return HookResult{
			Success: false,
			Error:   errors.New("hooks: timeout").Error(),
			Abort:   true,
		}
	}
}

// PreToolUse runs PreToolUse hooks with tool and raw argument payload.
func (e *HookExecutor) PreToolUse(tool, args string) HookResult {
	hc := HookContext{
		Session: map[string]any{},
		Args: map[string]any{
			"tool":     tool,
			"raw_args": args,
		},
	}
	return e.Execute(HookEventPreToolUse, hc)
}

// PostToolUse runs PostToolUse hooks with tool and result text.
func (e *HookExecutor) PostToolUse(tool, result string) HookResult {
	hc := HookContext{
		Session: map[string]any{},
		Args: map[string]any{
			"tool":   tool,
			"result": result,
		},
	}
	return e.Execute(HookEventPostToolUse, hc)
}

// PreEdit runs PreEdit hooks for a file path.
func (e *HookExecutor) PreEdit(file string) HookResult {
	hc := HookContext{File: file, Session: map[string]any{}}
	return e.Execute(HookEventPreEdit, hc)
}

// PostEdit runs PostEdit hooks with file and diff text.
func (e *HookExecutor) PostEdit(file, diff string) HookResult {
	hc := HookContext{
		File:    file,
		Args:    map[string]any{"diff": diff},
		Session: map[string]any{"diff": diff},
	}
	return e.Execute(HookEventPostEdit, hc)
}

// PreCommand runs PreCommand hooks.
func (e *HookExecutor) PreCommand(cmd string) HookResult {
	hc := HookContext{Command: cmd, Session: map[string]any{}}
	return e.Execute(HookEventPreCommand, hc)
}

// PostCommand runs PostCommand hooks with exit status.
func (e *HookExecutor) PostCommand(cmd string, exitCode int) HookResult {
	hc := HookContext{
		Command: cmd,
		Args: map[string]any{
			"exit_code": exitCode,
		},
		Session: map[string]any{},
	}
	return e.Execute(HookEventPostCommand, hc)
}

// SessionStart runs session start hooks.
func (e *HookExecutor) SessionStart(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return e.Execute(HookEventSessionStart, hc)
}

// SessionEnd runs session end hooks.
func (e *HookExecutor) SessionEnd(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return e.Execute(HookEventSessionEnd, hc)
}

// AgentSpawn runs agent spawn hooks.
func (e *HookExecutor) AgentSpawn(agentID, agentType string) HookResult {
	hc := HookContext{
		Session: map[string]any{
			"agent_id":   agentID,
			"agent_type": agentType,
		},
		Agent: &api.Agent{
			ID:   agentID,
			Name: agentID,
			Type: api.AgentType(agentType),
		},
	}
	return e.Execute(HookEventAgentSpawn, hc)
}

// AgentTerminate runs agent termination hooks.
func (e *HookExecutor) AgentTerminate(agentID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"agent_id": agentID},
		Agent:   &api.Agent{ID: agentID, Name: agentID},
	}
	return e.Execute(HookEventAgentTerminate, hc)
}
