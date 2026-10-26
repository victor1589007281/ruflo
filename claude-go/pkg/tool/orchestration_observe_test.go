package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

type observedSleepTool struct {
	delay   time.Duration
	content string
	isError bool
}

func (t observedSleepTool) Name() string                           { return t.content }
func (t observedSleepTool) Description() string                    { return "sleep test tool" }
func (t observedSleepTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (t observedSleepTool) IsReadOnly(json.RawMessage) bool        { return true }
func (t observedSleepTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (t observedSleepTool) CheckPermissions(json.RawMessage, *ToolContext) *types.PermissionResult {
	return nil
}
func (t observedSleepTool) Call(ctx context.Context, _ json.RawMessage, _ *ToolContext) (*ToolResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(t.delay):
	}
	return &ToolResult{Content: t.content, IsError: t.isError}, nil
}

func TestRunToolsObservesEachCallDurationAndStatus(t *testing.T) {
	reg := NewRegistry()
	reg.Register(observedSleepTool{delay: 20 * time.Millisecond, content: "fast"})
	reg.Register(observedSleepTool{delay: 80 * time.Millisecond, content: "slow"})
	reg.Register(observedSleepTool{delay: 20 * time.Millisecond, content: "failed", isError: true})
	observations := make(chan ToolCallObservation, 3)
	tctx := &ToolContext{ObserveToolCall: func(obs ToolCallObservation) { observations <- obs }}
	blocks := []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "1", Name: "fast", Input: json.RawMessage(`{}`)},
		{Type: types.ContentBlockToolUse, ID: "2", Name: "slow", Input: json.RawMessage(`{}`)},
		{Type: types.ContentBlockToolUse, ID: "3", Name: "failed", Input: json.RawMessage(`{}`)},
	}
	results := RunTools(context.Background(), blocks, reg, tctx, nil)
	close(observations)
	if len(results) != 3 {
		t.Fatalf("unexpected result count: %d", len(results))
	}
	byName := map[string]ToolCallObservation{}
	for obs := range observations {
		byName[obs.ToolName] = obs
	}
	if len(byName) != 3 {
		t.Fatalf("every concurrent call must be observed: %+v", byName)
	}
	if byName["fast"].Duration < 20*time.Millisecond || byName["slow"].Duration < 80*time.Millisecond {
		t.Fatalf("durations do not include actual execution: fast=%v slow=%v", byName["fast"].Duration, byName["slow"].Duration)
	}
	if byName["slow"].Duration-byName["fast"].Duration < 30*time.Millisecond {
		t.Fatalf("batch wall time was attributed uniformly instead of per call: fast=%v slow=%v", byName["fast"].Duration, byName["slow"].Duration)
	}
	if byName["fast"].IsError || !byName["failed"].IsError || byName["failed"].Output != "failed" {
		t.Fatalf("observation lost result status/content: %+v", byName)
	}
	if byName["slow"].StartedAt.IsZero() || byName["slow"].ToolUseID != "2" {
		t.Fatalf("observation lost identity/start time: %+v", byName["slow"])
	}
}
