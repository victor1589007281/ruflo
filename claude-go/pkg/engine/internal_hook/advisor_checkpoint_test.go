package internal_hook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestNewAdvisorCheckpointHookNilCases(t *testing.T) {
	if h := NewAdvisorCheckpointHook(nil, 5, true, nil, nil); h != nil {
		t.Fatal("consult=nil 应返回 nil")
	}
	consult := func(context.Context, []types.Message) (string, error) { return "", nil }
	if h := NewAdvisorCheckpointHook(consult, 0, false, nil, nil); h != nil {
		t.Fatal("everyN=0 且 !onLoop 应返回 nil")
	}
	if h := NewAdvisorCheckpointHook(consult, 3, false, nil, nil); h == nil {
		t.Fatal("有效配置不应返回 nil")
	}
}

func TestAdvisorCheckpointPeriodicTrigger(t *testing.T) {
	consultCalls := 0
	consult := func(_ context.Context, msgs []types.Message) (string, error) {
		consultCalls++
		return "建议: 停下来先验证假设", nil
	}
	metrics := NewEngineMetrics()
	h := NewAdvisorCheckpointHook(consult, 3, false, nil, metrics)

	for turn := 1; turn <= 2; turn++ {
		res, err := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: turn})
		if err != nil || res != nil {
			t.Fatalf("turn %d 不应触发: res=%v err=%v", turn, res, err)
		}
	}
	res, err := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 3})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || len(res.AppendMsgs) != 1 {
		t.Fatalf("turn 3 应注入建议, got %+v", res)
	}
	msg := res.AppendMsgs[0]
	if msg.Type != types.MessageTypeUser || !msg.IsMeta {
		t.Fatalf("注入消息应为 IsMeta user, got %+v", msg)
	}
	if !strings.Contains(msg.Content[0].Text, "Advisor checkpoint") || !strings.Contains(msg.Content[0].Text, "停下来先验证假设") {
		t.Fatalf("注入内容不符: %s", msg.Content[0].Text)
	}
	if consultCalls != 1 {
		t.Fatalf("consult 应调用 1 次, got %d", consultCalls)
	}
	if metrics.AdvisorCheckpoints.Load() != 1 {
		t.Fatalf("指标应+1, got %d", metrics.AdvisorCheckpoints.Load())
	}

	// 触发后计时重置: turn 4/5 不触发, turn 6 再触发
	for turn := 4; turn <= 5; turn++ {
		if res, _ := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: turn}); res != nil {
			t.Fatalf("turn %d 不应触发", turn)
		}
	}
	if res, _ := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 6}); res == nil {
		t.Fatal("turn 6 应再次触发")
	}
	if consultCalls != 2 {
		t.Fatalf("consult 应调用 2 次, got %d", consultCalls)
	}
}

func TestAdvisorCheckpointOnLoopTrigger(t *testing.T) {
	consultCalls := 0
	consult := func(context.Context, []types.Message) (string, error) {
		consultCalls++
		return "你在循环, 换一条路", nil
	}
	det := NewLoopDetector()
	h := NewAdvisorCheckpointHook(consult, 0, true, det, nil)

	// 无循环 → 不触发
	if res, _ := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 1}); res != nil {
		t.Fatal("无循环不应触发")
	}

	// 制造输入级循环: 同工具同参数连续 3 次
	input := json.RawMessage(`{"cmd":"ls"}`)
	for i := 0; i < 3; i++ {
		det.Observe("Bash", input)
	}
	if det.TriggerCount() == 0 {
		t.Fatal("LoopDetector 应已触发")
	}
	res, err := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 2})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || len(res.AppendMsgs) != 1 || !strings.Contains(res.AppendMsgs[0].Content[0].Text, "换一条路") {
		t.Fatalf("循环触发应注入建议, got %+v", res)
	}
	if consultCalls != 1 {
		t.Fatalf("consult 应调用 1 次, got %d", consultCalls)
	}

	// 计数未再增长 → 不重复触发
	if res, _ := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 3}); res != nil {
		t.Fatal("循环计数未增长不应重复触发")
	}
}

func TestAdvisorCheckpointSkipsWhenModelCalledAdvisor(t *testing.T) {
	consultCalls := 0
	consult := func(context.Context, []types.Message) (string, error) {
		consultCalls++
		return "advice", nil
	}
	h := NewAdvisorCheckpointHook(consult, 2, false, nil, nil)

	// 模型本轮已主动调用 advisor → 跳过且重置计时
	res, err := h.Execute(&HookContext{
		Ctx:       context.Background(),
		TurnCount: 5,
		ToolUseBlocks: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, Name: "advisor", Input: json.RawMessage(`{}`)},
		},
	})
	if err != nil || res != nil || consultCalls != 0 {
		t.Fatalf("模型主动调用 advisor 时应跳过: res=%v calls=%d", res, consultCalls)
	}
	// 计时已重置到 turn 5 → turn 6 不触发, turn 7 触发
	if res, _ := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 6}); res != nil {
		t.Fatal("turn 6 不应触发 (计时已重置)")
	}
	if res, _ := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 7}); res == nil {
		t.Fatal("turn 7 应触发")
	}
}

func TestAdvisorCheckpointConsultFailureSilent(t *testing.T) {
	consult := func(context.Context, []types.Message) (string, error) {
		return "", fmt.Errorf("advisor budget exhausted")
	}
	h := NewAdvisorCheckpointHook(consult, 1, false, nil, nil)
	res, err := h.Execute(&HookContext{Ctx: context.Background(), TurnCount: 1})
	if err != nil {
		t.Fatalf("咨询失败不应返回 error: %v", err)
	}
	if res != nil {
		t.Fatalf("咨询失败不应注入消息, got %+v", res)
	}
}
