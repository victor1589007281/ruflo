package hooks

import (
	"context"
	"errors"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// 本文件：Hook 执行器（责任链）。从 Registry 按事件取已排序列表顺序执行，合并 Warnings/Data；
// 遇 Abort/Error/Success=false 短路；单钩子 goroutine + context 超时，避免整条链被卡死。

// HookExecutor 钩子执行器，持有注册表与默认超时，对外提供按事件或便捷方法（PreEdit 等）的入口。
type HookExecutor struct {
	Registry *HookRegistry // 钩子注册中心；nil 时 Execute 系列会短路为成功
	// DefaultTimeout 当 ExecuteWithTimeout 的 dur<=0 时使用的单钩子超时时间。
	DefaultTimeout time.Duration
}

// NewExecutor 构造执行器；reg 为 nil 时会新建空 Registry。
func NewExecutor(reg *HookRegistry) *HookExecutor {
	if reg == nil {
		reg = NewRegistry()
	}
	return &HookExecutor{
		Registry:       reg,
		DefaultTimeout: 30 * time.Second,
	}
}

// Execute 使用 Background 上下文与默认超时执行指定事件下的全部钩子。
func (e *HookExecutor) Execute(ev HookEvent, hc HookContext) HookResult {
	return e.ExecuteWithTimeout(context.Background(), ev, hc, 0)
}

// ExecuteWithTimeout 对链上每个钩子使用相同 dur 作为单次调用的超时（dur<=0 时用 DefaultTimeout）。
// 聚合规则：顺序执行；Data 做浅层合并；遇 Abort 立即返回并带上该钩子结果；遇 Error 或非 Success 同样短路。
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

// runOne 在子 goroutine 中执行单个 HookHandler，通过 channel 回传结果；主 goroutine 与 context 超时竞态。
// panic 时向 done 发送失败结果；若超时则返回 Abort+timeout，防止悬挂 goroutine 永久阻塞（select 丢弃向 done 的发送）。
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

// PreToolUse 构造 HookContext（Args 含 tool、raw_args）并执行 HookEventPreToolUse。
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

// PostToolUse 执行 HookEventPostToolUse，传入工具名与结果文本。
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

// PreEdit 对即将编辑的文件路径执行 HookEventPreEdit。
func (e *HookExecutor) PreEdit(file string) HookResult {
	hc := HookContext{File: file, Session: map[string]any{}}
	return e.Execute(HookEventPreEdit, hc)
}

// PostEdit 在编辑完成后执行 HookEventPostEdit，Args 与 Session 均携带 diff 便于钩子读取。
func (e *HookExecutor) PostEdit(file, diff string) HookResult {
	hc := HookContext{
		File:    file,
		Args:    map[string]any{"diff": diff},
		Session: map[string]any{"diff": diff},
	}
	return e.Execute(HookEventPostEdit, hc)
}

// PreCommand 执行 HookEventPreCommand，Command 字段承载待执行命令行。
func (e *HookExecutor) PreCommand(cmd string) HookResult {
	hc := HookContext{Command: cmd, Session: map[string]any{}}
	return e.Execute(HookEventPreCommand, hc)
}

// PostCommand 执行 HookEventPostCommand，Args 中带 exit_code。
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

// SessionStart 执行 HookEventSessionStart，Session 与 Agent 均绑定 sessionID。
func (e *HookExecutor) SessionStart(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return e.Execute(HookEventSessionStart, hc)
}

// SessionEnd 执行 HookEventSessionEnd。
func (e *HookExecutor) SessionEnd(sessionID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"session_id": sessionID},
		Agent:   &api.Agent{ID: sessionID, Name: sessionID},
	}
	return e.Execute(HookEventSessionEnd, hc)
}

// AgentSpawn 执行 HookEventAgentSpawn，构造 Agent 类型与 Session 元数据。
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

// AgentTerminate 执行 HookEventAgentTerminate。
func (e *HookExecutor) AgentTerminate(agentID string) HookResult {
	hc := HookContext{
		Session: map[string]any{"agent_id": agentID},
		Agent:   &api.Agent{ID: agentID, Name: agentID},
	}
	return e.Execute(HookEventAgentTerminate, hc)
}
