package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func testFinalResponseContract() *FinalResponseContract {
	return &FinalResponseContract{
		RequiredSections:  []string{"## 结论", "## 关键证据", "## 矛盾与缺口", "## 下一步"},
		RequireEvidenceID: true,
		RepairInstruction: "完整重写四节",
	}
}

func TestValidateFinalResponseStructureAndEvidenceIDs(t *testing.T) {
	contract := testFinalResponseContract()
	valid := "## 结论\n未定位根因\n## 关键证据\n- `ev_real`\n## 矛盾与缺口\n- 无\n## 下一步\n- 【需人工】补监控"
	if err := validateFinalResponse(valid, contract, map[string]bool{"ev_real": true}); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if err := validateFinalResponse(strings.Replace(valid, "## 结论\n", "", 1), contract, map[string]bool{"ev_real": true}); err == nil {
		t.Fatal("missing conclusion must not be delivered")
	}
	if err := validateFinalResponse(strings.Replace(valid, "ev_real", "ev_fake", 1), contract, map[string]bool{"ev_real": true}); err == nil {
		t.Fatal("invented evidence ID must not be delivered")
	}
	withoutID := "## 结论\n无根因\n## 关键证据\n- 没有引用\n## 矛盾与缺口\n- 无\n## 下一步\n- 补证"
	if err := validateFinalResponse(withoutID, contract, map[string]bool{"ev_real": true}); err == nil {
		t.Fatal("available evidence must be cited")
	}
	// 采集失败时没有真实 ID 可引用；允许明确写缺口，不能逼模型编一个 ID。
	if err := validateFinalResponse(withoutID, contract, map[string]bool{}); err != nil {
		t.Fatalf("empty evidence set should allow an explicit collection gap: %v", err)
	}
}

func TestFinalResponseEvidenceIDsComeOnlyFromSuccessfulToolResults(t *testing.T) {
	messages := []types.Message{
		{Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "ev_user_fake"}}},
		{Content: []types.ContentBlock{{Type: types.ContentBlockToolResult, Content: "ev_ok"}}},
		{Content: []types.ContentBlock{{Type: types.ContentBlockToolResult, Content: "ev_failed", IsError: true}}},
	}
	known := finalResponseEvidenceIDs(messages)
	if !known["ev_ok"] || known["ev_user_fake"] || known["ev_failed"] {
		t.Fatalf("unexpected evidence authority set: %+v", known)
	}
}

func TestFinalResponseContractRepairsOnceWithoutTools(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "## 关键证据\n- 无\n## 矛盾与缺口\n- 无\n## 下一步\n- 补证")
	u.onRequest = func(turn int, req seenRequest) {
		if turn != 1 {
			return
		}
		if !strings.Contains(req.Flat, "最终答案契约修订") || !strings.Contains(req.Flat, "缺少小节") {
			t.Errorf("repair request lacks the validation reason: %q", req.Flat)
		}
		if len(req.ToolNames) != 0 {
			t.Errorf("repair turn must not expose tools: %v", req.ToolNames)
		}
		u.text = "## 结论\n采集未完成，不能定因\n## 关键证据\n- 无有效证据\n## 矛盾与缺口\n- 缺少采集结果\n## 下一步\n- 【需人工】恢复只读采集"
	}
	e := newStateEngine(t, u, cfg, t.TempDir())
	got, err := e.RunIsolated(context.Background(), "排查", IsolatedRunOptions{
		MaxTurns: 3, Stage: id.Stage, FinalResponse: testFinalResponseContract(),
	})
	if err != nil {
		t.Fatalf("repaired response should pass: %v", err)
	}
	if !strings.HasPrefix(got, "## 结论") || u.count() != 2 {
		t.Fatalf("expected one repair and a complete response, calls=%d text=%q", u.count(), got)
	}
}

func TestFinalResponseContractFailsInsteadOfDeliveringSecondDraft(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "## 关键证据\n- 无")
	e := newStateEngine(t, u, cfg, t.TempDir())
	got, err := e.RunIsolated(context.Background(), "排查", IsolatedRunOptions{
		MaxTurns: 4, Stage: id.Stage, FinalResponse: testFinalResponseContract(),
	})
	if err == nil || !strings.Contains(err.Error(), "final response contract") {
		t.Fatalf("second invalid draft must fail explicitly: err=%v text=%q", err, got)
	}
	if u.count() != 2 {
		t.Fatalf("contract must repair at most once, calls=%d", u.count())
	}
}
