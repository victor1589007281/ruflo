package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// newPolicyExecutor 造一个只装了 TraceStore 的执行器。
func newPolicyExecutor(t *testing.T) (*WorkflowExecutor, *tracestore.Store) {
	t.Helper()
	ts := tracestore.New(statestore.NewFileStore(filepath.Join(t.TempDir(), "statestore")))
	return &WorkflowExecutor{traceStore: ts}, ts
}

func TestWritePolicyDecisionSpan_记下注入了什么(t *testing.T) {
	we, ts := newPolicyExecutor(t)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-p", TurnID: "turn-1"})
	prompt := "系统提示\n<role_skills>\n### Skill: go-testing\n正文\n\n### Skill: code-review\n正文\n</role_skills>\n任务"
	we.writePolicyDecisionSpan(ctx, policyDecision{
		Stage: "impl", Role: "coder", Team: "tm",
		ExperienceIDs: []string{"exp-1", "exp-2"},
		Blackboard:    true, Reference: false, UserFeedback: true,
		Prompt: prompt, Objective: "实现登录",
	})

	spans := readSpans(t, ts, "run-p")
	if len(spans) != 1 {
		t.Fatalf("应写出 1 条 Span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Kind != tracestore.KindPolicyDecision {
		t.Errorf("Kind = %q, want %q", sp.Kind, tracestore.KindPolicyDecision)
	}
	if sp.NodeID != "impl" || sp.TurnID != "turn-1" {
		t.Errorf("归因缺失: node=%q turn=%q", sp.NodeID, sp.TurnID)
	}
	// 经验 ID 必须逐条留下 —— uplift 归因要回答"注入的是哪几条"。
	exps, _ := sp.Attrs["experiences"].([]any)
	if len(exps) != 2 {
		t.Errorf("experiences 应有 2 条, got %v", sp.Attrs["experiences"])
	}
	skills, _ := sp.Attrs["skills"].([]any)
	if len(skills) != 2 {
		t.Errorf("skills 应反解出 2 个, got %v", sp.Attrs["skills"])
	}
	if sp.Attrs["ctx_blackboard"] != true || sp.Attrs["ctx_user_feedback"] != true {
		t.Errorf("上下文块标记丢失: %+v", sp.Attrs)
	}
	if sp.Attrs["ctx_reference"] != false {
		t.Errorf("没注入的块必须记 false 而不是缺字段 (缺字段与 false 不可区分): %+v", sp.Attrs)
	}
	// 正文必须是**最终提示词**: uplift 复盘要能看到注入后那份 prompt 长什么样。
	body, err := ts.Resolve(sp.OutputRef)
	if err != nil || body != prompt {
		t.Errorf("OutputRef 应还原出最终提示词, err=%v", err)
	}
}

func TestWritePolicyDecisionSpan_无底座时noop(t *testing.T) {
	we := &WorkflowExecutor{}
	// 不 panic 即通过 (fail-open: 采集是观测不是治理)
	we.writePolicyDecisionSpan(context.Background(), policyDecision{Stage: "s"})
}

func TestInjectedSkillNames(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   int
	}{
		{"无技能段", "普通提示词\n### Skill: 假的\n", 0},
		{"段未闭合", "<role_skills>\n### Skill: a\n", 0},
		{"去重", "<role_skills>\n### Skill: a\n### Skill: a\n</role_skills>", 1},
		{"正常两个", "<role_skills>\n### Skill: a\n### Skill: b\n</role_skills>", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := injectedSkillNames(c.prompt); len(got) != c.want {
				t.Errorf("got %v, want %d 个", got, c.want)
			}
		})
	}
}

// 段外出现的 "### Skill:" 不得混进来 —— 经验正文里引用技能名是常见的。
func TestInjectedSkillNames_只认段内(t *testing.T) {
	p := "### Skill: 段外的\n<role_skills>\n### Skill: 段内的\n</role_skills>\n### Skill: 也是段外的\n"
	got := injectedSkillNames(p)
	if len(got) != 1 || got[0] != "段内的" {
		t.Errorf("只该认段内的技能名, got %v", got)
	}
}
