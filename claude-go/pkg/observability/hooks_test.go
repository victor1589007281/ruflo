package observability

import (
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

func TestHookRegistryRegister(t *testing.T) {
	reg := &HookRegistry{}
	reg.RegisterLLM(NoopLLMHook{})
	reg.RegisterStage(NoopStageHook{})
	reg.RegisterTask(NoopTaskHook{})
	reg.RegisterTeam(NoopTeamHook{})
	reg.RegisterCollaboration(NoopCollaborationHook{})
	reg.RegisterPrompt(NoopPromptHook{})

	if len(reg.LLM) != 1 || len(reg.Stage) != 1 || len(reg.Task) != 1 {
		t.Fatalf("注册失败")
	}
	if len(reg.Team) != 1 || len(reg.Collaboration) != 1 || len(reg.Prompt) != 1 {
		t.Fatalf("注册失败")
	}
}

func TestNoopHooks(t *testing.T) {
	// 确保所有 NoopHook 不会 panic
	llm := NoopLLMHook{}
	llm.OnLLMCallStart(LLMCallEvent{})
	llm.OnLLMCallComplete(LLMCallEvent{})
	llm.OnLLMCallError(LLMCallEvent{})
	llm.OnLLMCallFallback("a", "b", LLMCallEvent{})
	llm.OnLLMCacheHit("m", "c", 1)
	llm.OnLLMCacheMiss("m", "c", 1)

	stage := NoopStageHook{}
	stage.OnStageStart(StageEvent{})
	stage.OnStageComplete(StageEvent{})
	stage.OnStageFail(StageEvent{})
	stage.OnStageRetry(StageEvent{}, 1, time.Second)

	task := NoopTaskHook{}
	task.OnTaskReady(TaskEvent{})
	task.OnTaskStart(TaskEvent{})
	task.OnTaskComplete(TaskEvent{})
	task.OnTaskFail(TaskEvent{})
	task.OnTaskRetry(TaskEvent{}, 1, time.Second)

	team := NoopTeamHook{}
	team.OnTeamStart(TeamEvent{})
	team.OnTeamComplete(TeamEvent{})
	team.OnTeamFail(TeamEvent{})
	team.OnTeamStageTransition("a", "b", TeamEvent{})

	collab := NoopCollaborationHook{}
	collab.OnCollabMessage(CollaborationEvent{})
	collab.OnCollabReview(CollaborationEvent{})
	collab.OnCollabConsensus(CollaborationEvent{})
	collab.OnCollabHandoff("a", "b", "s", CollaborationEvent{})

	prompt := NoopPromptHook{}
	prompt.OnPromptRender(PromptEvent{})
	prompt.OnPromptVersion("id", "v", "h", nil)
	prompt.OnPromptCompare("a", "b", nil)
	prompt.OnPromptABStart("t", "a", "b", PromptEvent{})
	prompt.OnPromptABResult(PromptABResult{})
}

func TestBusSubscriberRouting(t *testing.T) {
	reg := &HookRegistry{}
	var llmCalled, stageCalled, taskCalled bool
	reg.RegisterLLM(&testLLMHook{onComplete: func(ev LLMCallEvent) { llmCalled = true }})
	reg.RegisterStage(&testStageHook{onComplete: func(ev StageEvent) { stageCalled = true }})
	reg.RegisterTask(&testTaskHook{onComplete: func(ev TaskEvent) { taskCalled = true }})

	sub := NewBusSubscriber(reg)

	// LLM complete
	sub.OnEvent(Event{Type: EvtLLMCallComplete, Payload: map[string]interface{}{"status": "success"}})
	if !llmCalled {
		t.Fatalf("LLM complete hook 应被调用")
	}

	// Stage complete
	sub.OnEvent(Event{Type: EvtStageComplete, Payload: map[string]interface{}{"stage_name": "design"}})
	if !stageCalled {
		t.Fatalf("Stage complete hook 应被调用")
	}

	// Task complete
	sub.OnEvent(Event{Type: EvtTaskComplete, Payload: map[string]interface{}{"task_name": "t1"}})
	if !taskCalled {
		t.Fatalf("Task complete hook 应被调用")
	}
}

func TestBusSubscriberPayloadExtraction(t *testing.T) {
	reg := &HookRegistry{}
	var ev StageEvent
	reg.RegisterStage(&testStageHook{onStart: func(e StageEvent) { ev = e }})

	sub := NewBusSubscriber(reg)
	sub.OnEvent(Event{
		Type: EvtStageStart,
		Payload: map[string]interface{}{
			"team_id":      "team1",
			"workflow":     "dev",
			"stage_name":   "design",
			"stage_index":  1,
			"agent_role":   "architect",
			"model_alias":  "dashscope:qwen",
			"duration_sec": 5.5,
			"success":      true,
			"error":        "",
			"retry_count":  0,
			"output_len":   100,
		},
	})

	if ev.TeamID != "team1" || ev.Workflow != "dev" || ev.StageName != "design" {
		t.Fatalf("StageEvent 字段解析失败")
	}
	if ev.StageIndex != 1 || ev.AgentRole != "architect" {
		t.Fatalf("StageEvent 索引/角色解析失败")
	}
	if ev.DurationSec != 5.5 {
		t.Fatalf("StageEvent duration 解析失败")
	}
	if !ev.Success {
		t.Fatalf("StageEvent success 应为 true")
	}
}

func TestBusSubscriberToLLMEvent(t *testing.T) {
	reg := &HookRegistry{}
	var ev LLMCallEvent
	reg.RegisterLLM(&testLLMHook{onComplete: func(e LLMCallEvent) { ev = e }})

	sub := NewBusSubscriber(reg)
	rec := api.LLMCallRecord{Model: "gpt-4", Status: "success", DurationSec: 1.2, InputTokens: 100}
	sub.OnEvent(Event{
		Type: EvtLLMCallComplete,
		Payload: map[string]interface{}{
			"record":       rec,
			"team_id":      "t1",
			"stage_name":   "s1",
			"agent_role":   "coder",
			"prompt_hash":  "abc",
			"prompt_len":   50,
			"response_len": 30,
		},
	})

	if ev.Record.Model != "gpt-4" || ev.Record.Status != "success" {
		t.Fatalf("LLMCallEvent record 解析失败")
	}
	if ev.TeamID != "t1" || ev.StageName != "s1" || ev.AgentRole != "coder" {
		t.Fatalf("LLMCallEvent 上下文解析失败")
	}
	if ev.PromptHash != "abc" || ev.PromptLen != 50 || ev.ResponseLen != 30 {
		t.Fatalf("LLMCallEvent payload 解析失败")
	}
}

func TestGlobalRegistry(t *testing.T) {
	old := globalRegistry
	defer func() { globalRegistry = old }()

	SetGlobalRegistry(&HookRegistry{})
	if GlobalRegistry() == nil {
		t.Fatalf("GlobalRegistry 不应为 nil")
	}
}

// ─── test helpers ──────────────────────────────────────────────────────────

type testLLMHook struct {
	onStart    func(ev LLMCallEvent)
	onComplete func(ev LLMCallEvent)
	onError    func(ev LLMCallEvent)
}

func (h *testLLMHook) OnLLMCallStart(ev LLMCallEvent)     { if h.onStart != nil { h.onStart(ev) } }
func (h *testLLMHook) OnLLMCallComplete(ev LLMCallEvent)  { if h.onComplete != nil { h.onComplete(ev) } }
func (h *testLLMHook) OnLLMCallError(ev LLMCallEvent)     { if h.onError != nil { h.onError(ev) } }
func (h *testLLMHook) OnLLMCallFallback(_, _ string, _ LLMCallEvent) {}
func (h *testLLMHook) OnLLMCacheHit(_, _ string, _ int)   {}
func (h *testLLMHook) OnLLMCacheMiss(_, _ string, _ int)  {}

type testStageHook struct {
	onStart    func(ev StageEvent)
	onComplete func(ev StageEvent)
	onFail     func(ev StageEvent)
}

func (h *testStageHook) OnStageStart(ev StageEvent)                      { if h.onStart != nil { h.onStart(ev) } }
func (h *testStageHook) OnStageComplete(ev StageEvent)                   { if h.onComplete != nil { h.onComplete(ev) } }
func (h *testStageHook) OnStageFail(ev StageEvent)                       { if h.onFail != nil { h.onFail(ev) } }
func (h *testStageHook) OnStageRetry(ev StageEvent, _ int, _ time.Duration) {}

type testTaskHook struct {
	onComplete func(ev TaskEvent)
}

func (h *testTaskHook) OnTaskReady(ev TaskEvent)                        {}
func (h *testTaskHook) OnTaskStart(ev TaskEvent)                        {}
func (h *testTaskHook) OnTaskComplete(ev TaskEvent)                     { if h.onComplete != nil { h.onComplete(ev) } }
func (h *testTaskHook) OnTaskFail(ev TaskEvent)                         {}
func (h *testTaskHook) OnTaskRetry(ev TaskEvent, _ int, _ time.Duration) {}
