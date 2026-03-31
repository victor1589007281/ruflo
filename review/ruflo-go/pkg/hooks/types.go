package hooks

import (
	"context"

	"github.com/ruflo/ruflo-go/api"
)

// HookEvent classifies orchestration hook invocations.
type HookEvent string

const (
	HookEventPreEdit             HookEvent = "PreEdit"
	HookEventPostEdit            HookEvent = "PostEdit"
	HookEventPreCommand          HookEvent = "PreCommand"
	HookEventPostCommand         HookEvent = "PostCommand"
	HookEventPreTask             HookEvent = "PreTask"
	HookEventPostTask            HookEvent = "PostTask"
	HookEventSessionStart        HookEvent = "SessionStart"
	HookEventSessionEnd          HookEvent = "SessionEnd"
	HookEventSessionRestore      HookEvent = "SessionRestore"
	HookEventPreRoute            HookEvent = "PreRoute"
	HookEventPostRoute           HookEvent = "PostRoute"
	HookEventPatternLearned      HookEvent = "PatternLearned"
	HookEventPatternConsolidated HookEvent = "PatternConsolidated"
	HookEventPreToolUse          HookEvent = "PreToolUse"
	HookEventPostToolUse         HookEvent = "PostToolUse"
	HookEventPreRead             HookEvent = "PreRead"
	HookEventPostRead            HookEvent = "PostRead"
	HookEventTaskProgress        HookEvent = "TaskProgress"
	HookEventAgentSpawn          HookEvent = "AgentSpawn"
	HookEventAgentTerminate      HookEvent = "AgentTerminate"
)

// HookHandler processes a hook; ctx is canceled when the executor times out.
type HookHandler func(ctx context.Context, hc HookContext) HookResult

// HookRegistration binds a handler to an event with ordering metadata.
type HookRegistration struct {
	Event    HookEvent
	Handler  HookHandler
	Priority int
	Name     string
	Enabled  bool
}

// HookContext carries task/session/file/command context into hooks.
type HookContext struct {
	Task    *api.TaskDefinition `json:"task,omitempty"`
	Session map[string]any      `json:"session,omitempty"`
	Agent   *api.Agent          `json:"agent,omitempty"`
	File    string              `json:"file,omitempty"`
	Command string              `json:"command,omitempty"`
	Args    map[string]any      `json:"args,omitempty"`
	Env     map[string]string   `json:"env,omitempty"`
}

// HookResult is the outcome of one hook; Abort stops the chain when blocking.
type HookResult struct {
	Success  bool           `json:"success"`
	Abort    bool           `json:"abort"`
	Error    string         `json:"error,omitempty"`
	Message  string         `json:"message,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}
