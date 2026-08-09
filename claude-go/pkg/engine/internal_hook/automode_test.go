package internal_hook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

func newAutoModeHook(mode string, setPlan func(on bool), consult AdvisorConsultFn, autoAdvisor bool) *AutoModeHook {
	return NewAutoModeHook(true, mode, nil, autoAdvisor, consult, setPlan, "", NewEngineMetrics())
}

func userMsg(text string) types.Message {
	return types.Message{
		Type:      types.MessageTypeUser,
		UUID:      GenerateUUID(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
		CreatedAt: time.Now(),
	}
}

func preReqCtx(messages []types.Message, turn int) *HookContext {
	return &HookContext{
		Ctx:       context.Background(),
		Phase:     PhasePreRequest,
		Messages:  messages,
		TurnCount: turn,
	}
}

func TestAutoMode_ComplexTaskEntersPlanAndInjectsHint(t *testing.T) {
	var planCalls []bool
	h := newAutoModeHook("heuristic", func(on bool) { planCalls = append(planCalls, on) }, nil, false)

	res, err := h.Execute(preReqCtx([]types.Message{userMsg("帮我设计并实现一个多文件功能，并且需要数据库迁移，同时要补充测试。")}, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil {
		t.Fatal("复杂任务应返回注入结果")
	}
	if len(planCalls) != 1 || !planCalls[0] {
		t.Fatalf("应调用 SetPlanMode(true): %v", planCalls)
	}
	if res.Messages == nil || len(res.Messages) != 2 {
		t.Fatalf("应全量替换 messages 并追加 1 条 hint, got %d", len(res.Messages))
	}
	hint := res.Messages[1]
	if !hint.IsMeta || hint.Type != types.MessageTypeUser {
		t.Fatalf("hint 应为 meta user 消息: %+v", hint)
	}
	text := ""
	for _, b := range hint.Content {
		if b.Type == types.ContentBlockText {
			text = b.Text
		}
	}
	if !strings.Contains(text, "<system-reminder>") || !strings.Contains(text, "已进入规划模式") {
		t.Fatalf("hint 应含 system-reminder 与规划引导: %q", text)
	}
	if h.Metrics.AutoPlanEntered.Load() != 1 {
		t.Fatalf("AutoPlanEntered 应为 1, got %d", h.Metrics.AutoPlanEntered.Load())
	}
}

func TestAutoMode_SimpleTaskNoPlan(t *testing.T) {
	var planCalls []bool
	h := newAutoModeHook("heuristic", func(on bool) { planCalls = append(planCalls, on) }, nil, false)

	res, err := h.Execute(preReqCtx([]types.Message{userMsg("你好")}, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res != nil {
		t.Fatalf("简单任务不应返回注入: %+v", res)
	}
	if len(planCalls) != 0 {
		t.Fatalf("简单任务不应动 plan flag: %v", planCalls)
	}
}

func TestAutoMode_OnlyFirstTurn(t *testing.T) {
	var planCalls []bool
	h := newAutoModeHook("heuristic", func(on bool) { planCalls = append(planCalls, on) }, nil, false)

	res, err := h.Execute(preReqCtx([]types.Message{userMsg("帮我设计并实现一个多文件功能，并且需要数据库迁移。")}, 1))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res != nil || len(planCalls) != 0 {
		t.Fatalf("非首轮不应判定/注入: res=%+v plan=%v", res, planCalls)
	}
}

func TestAutoMode_ResetOnNewSubmit(t *testing.T) {
	var planCalls []bool
	h := newAutoModeHook("heuristic", func(on bool) { planCalls = append(planCalls, on) }, nil, false)

	// 第一个复杂任务: 进入 plan。
	if _, err := h.Execute(preReqCtx([]types.Message{userMsg("帮我设计并实现一个多文件功能，并且需要数据库迁移。")}, 0)); err != nil {
		t.Fatalf("Execute#1: %v", err)
	}
	if h.autoPlanActive != true {
		t.Fatal("autoPlanActive 应为 true")
	}

	// 新 SubmitMessage: 简单任务 → 先重置 (SetPlanMode(false)), 不重新进入。
	res, err := h.Execute(preReqCtx([]types.Message{userMsg("谢谢")}, 0))
	if err != nil {
		t.Fatalf("Execute#2: %v", err)
	}
	if res != nil {
		t.Fatalf("简单任务不应注入: %+v", res)
	}
	if len(planCalls) != 2 || planCalls[1] != false {
		t.Fatalf("应调用 SetPlanMode(false) 重置: %v", planCalls)
	}
	if h.autoPlanActive != false {
		t.Fatal("autoPlanActive 应被重置")
	}
}

func TestAutoMode_AdvisorInjected(t *testing.T) {
	var planCalls []bool
	consult := func(ctx context.Context, msgs []types.Message) (string, error) {
		return "先读 README 再看 main.go。", nil
	}
	h := newAutoModeHook("heuristic", func(on bool) { planCalls = append(planCalls, on) }, consult, true)

	res, err := h.Execute(preReqCtx([]types.Message{userMsg("帮我设计并实现一个多文件功能，并且需要数据库迁移。")}, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || len(res.Messages) != 2 {
		t.Fatalf("应注入 hint: %+v", res)
	}
	text := ""
	for _, b := range res.Messages[1].Content {
		if b.Type == types.ContentBlockText {
			text = b.Text
		}
	}
	if !strings.Contains(text, "advisor 建议") || !strings.Contains(text, "先读 README") {
		t.Fatalf("hint 应含 advisor 建议: %q", text)
	}
	if h.Metrics.AutoAdvisorInjected.Load() != 1 {
		t.Fatalf("AutoAdvisorInjected 应为 1, got %d", h.Metrics.AutoAdvisorInjected.Load())
	}
}

func TestAutoMode_AdvisorFailureSilent(t *testing.T) {
	consult := func(ctx context.Context, msgs []types.Message) (string, error) {
		return "", errors.New("advisor budget exhausted")
	}
	h := newAutoModeHook("heuristic", nil, consult, true)

	res, err := h.Execute(preReqCtx([]types.Message{userMsg("帮我设计并实现一个多文件功能，并且需要数据库迁移。")}, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil {
		t.Fatal("advisor 失败不应阻断 plan 注入")
	}
	text := ""
	for _, b := range res.Messages[1].Content {
		if b.Type == types.ContentBlockText {
			text = b.Text
		}
	}
	if strings.Contains(text, "advisor 建议") {
		t.Fatalf("advisor 失败不应注入建议: %q", text)
	}
	if h.Metrics.AutoAdvisorInjected.Load() != 0 {
		t.Fatalf("AutoAdvisorInjected 应为 0")
	}
}

func TestAutoMode_PlaceholderSkipped(t *testing.T) {
	var planCalls []bool
	h := newAutoModeHook("heuristic", func(on bool) { planCalls = append(planCalls, on) }, nil, false)

	res, err := h.Execute(preReqCtx([]types.Message{userMsg(".")}, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res != nil || len(planCalls) != 0 {
		t.Fatalf("占位符消息不应触发: res=%+v plan=%v", res, planCalls)
	}
}
