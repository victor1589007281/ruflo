package unit

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

func TestManagerLabRehearsalWorkflowsRegistered(t *testing.T) {
	for _, name := range []string{"manager-lab-rehearsal-npc", "manager-lab-rehearsal-critic"} {
		wf := agent.GetWorkflow(name)
		if wf == nil {
			t.Fatalf("GetWorkflow(%q) = nil, 期望已注册", name)
		}
		if wf.Name != name {
			t.Errorf("GetWorkflow(%q).Name = %q, 期望 %q", name, wf.Name, name)
		}
		if len(wf.Stages) != 1 {
			t.Errorf("GetWorkflow(%q) 阶段数=%d, 期望 1", name, len(wf.Stages))
		}
		if wf.Mode != "pipeline" {
			t.Errorf("GetWorkflow(%q).Mode = %q, 期望 pipeline", name, wf.Mode)
		}
	}
}

func TestManagerLabRehearsalWorkflowsInList(t *testing.T) {
	workflows := agent.ListWorkflows()
	found := map[string]bool{
		"manager-lab-rehearsal-npc":    false,
		"manager-lab-rehearsal-critic": false,
	}
	for _, wf := range workflows {
		if _, ok := found[wf.Name]; ok {
			found[wf.Name] = true
		}
	}
	for name, ok := range found {
		if !ok {
			t.Errorf("ListWorkflows() 缺少 %q", name)
		}
	}
}
