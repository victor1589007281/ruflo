package main

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

// TestResolveAgentRunSpecSearchResearcherCarriesBudget 钉住角色层的两条声明
// 真的被搬进了 agentRunSpec。只改 RoleDef 而不改 resolveAgentRunSpec 时,
// 这两条会安静地留在角色表里、运行时仍是 461 个工具和默认长程回合。
func TestResolveAgentRunSpecSearchResearcherCarriesBudget(t *testing.T) {
	rr := agent.NewRoleRegistry(t.TempDir())
	spec, err := resolveAgentRunSpec("search-researcher", rr, nil)
	if err != nil {
		t.Fatalf("resolveAgentRunSpec: %v", err)
	}
	if spec.Source != "role" {
		t.Fatalf("Source = %q, want role", spec.Source)
	}
	if spec.MaxTurns < 18 {
		t.Errorf("MaxTurns = %d, want >= 18", spec.MaxTurns)
	}
	if spec.FinalizeTurns < 2 {
		t.Errorf("FinalizeTurns = %d, want >= 2 (角色声明没有搬进 spec)", spec.FinalizeTurns)
	}
	if !spec.MergeBashTargets {
		t.Error("MergeBashTargets = false (角色声明没有搬进 spec)")
	}
	if !spec.RestrictWritesToCwd {
		t.Error("RestrictWritesToCwd = false (角色声明没有搬进 spec)")
	}
	if !spec.CitationGate {
		t.Error("CitationGate = false (角色声明没有搬进 spec)")
	}
	if !spec.NormalizeEvidence {
		t.Error("NormalizeEvidence = false (角色声明没有搬进 spec)")
	}
	if !spec.HasTools {
		t.Fatal("HasTools = false: 角色工具白名单没有搬进 spec")
	}
	if len(spec.Tools) == 0 {
		t.Fatal("Tools 为空: 白名单没有搬进 spec")
	}
	want := map[string]bool{"WebSearch": true, "WebFetch": true, "Write": true, "Bash": true}
	got := make(map[string]bool, len(spec.Tools))
	for _, name := range spec.Tools {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("spec.Tools 缺少 %q: %v", name, spec.Tools)
		}
	}
}
