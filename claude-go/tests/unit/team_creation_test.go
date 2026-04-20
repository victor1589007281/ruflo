package unit

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

func TestAppCompositePhases(t *testing.T) {
	wf := agent.GetWorkflow("app")
	if wf == nil {
		t.Fatal("app workflow not found")
	}

	phases := map[string][]string{
		"PhaseA-Research":  {"requirements-planning", "user-research", "tech-research", "requirements-synthesis"},
		"PhaseB-Creative":  {"creative-plan", "html-prototype", "prototype-review"},
		"PhaseC-Dev":       {"architecture", "backend-dev", "frontend-dev"},
		"PhaseD-Miniprogram": {"miniprogram-dev"},
		"PhaseE-Test":      {"integration-test", "code-review"},
	}

	stageSet := make(map[string]bool)
	for _, s := range wf.Stages {
		stageSet[s.Name] = true
	}

	for phase, stages := range phases {
		for _, name := range stages {
			if !stageSet[name] {
				t.Errorf("Phase %s 缺少阶段: %s", phase, name)
			}
		}
	}
}

func TestGameCompositePhases(t *testing.T) {
	wf := agent.GetWorkflow("game")
	if wf == nil {
		t.Fatal("game workflow not found")
	}

	phases := map[string][]string{
		"PhaseA-Design":    {"game-concept"},
		"PhaseB-Narrative": {"narrative-evolution"},
		"PhaseC-Art":       {"art-plan", "art-prototype", "art-review"},
		"PhaseD-Dev":       {"tech-architecture", "engine-dev", "logic-dev", "level-design"},
		"PhaseE-Test":      {"game-integration", "game-optimization"},
	}

	stageSet := make(map[string]bool)
	for _, s := range wf.Stages {
		stageSet[s.Name] = true
	}

	for phase, stages := range phases {
		for _, name := range stages {
			if !stageSet[name] {
				t.Errorf("Phase %s 缺少阶段: %s", phase, name)
			}
		}
	}
}

func TestAppDependencyChain(t *testing.T) {
	wf := agent.GetWorkflow("app")
	if wf == nil {
		t.Fatal("app workflow not found")
	}

	depChecks := map[string][]string{
		"requirements-synthesis": {"user-research", "tech-research"},
		"creative-plan":          {"requirements-synthesis"},
		"html-prototype":         {"creative-plan"},
		"architecture":           {"requirements-synthesis", "html-prototype"},
		"frontend-dev":           {"architecture", "html-prototype"},
		"miniprogram-dev":        {"architecture", "html-prototype"},
		"integration-test":       {"backend-dev", "frontend-dev", "miniprogram-dev"},
	}

	stageMap := make(map[string]agent.StageDef)
	for _, s := range wf.Stages {
		stageMap[s.Name] = s
	}

	for stageName, expectedDeps := range depChecks {
		s, ok := stageMap[stageName]
		if !ok {
			t.Errorf("阶段 %q 不存在", stageName)
			continue
		}
		depSet := make(map[string]bool)
		for _, d := range s.DependsOn {
			depSet[d] = true
		}
		for _, dep := range expectedDeps {
			if !depSet[dep] {
				t.Errorf("阶段 %q 应依赖 %q, 实际依赖: %v", stageName, dep, s.DependsOn)
			}
		}
	}
}

func TestGameNarrativeDependsOnConcept(t *testing.T) {
	wf := agent.GetWorkflow("game")
	if wf == nil {
		t.Fatal("game workflow not found")
	}

	for _, s := range wf.Stages {
		if s.Name == "narrative-evolution" {
			found := false
			for _, d := range s.DependsOn {
				if d == "game-concept" {
					found = true
					break
				}
			}
			if !found {
				t.Error("narrative-evolution 应依赖 game-concept")
			}
			return
		}
	}
	t.Error("未找到 narrative-evolution 阶段")
}

func TestAllNewWorkflowPromptsNonEmpty(t *testing.T) {
	workflows := []string{"ml-training", "app", "game"}
	for _, name := range workflows {
		wf := agent.GetWorkflow(name)
		if wf == nil {
			t.Errorf("工作流 %q 未注册", name)
			continue
		}
		for _, s := range wf.Stages {
			if s.Prompt == "" {
				continue // 由 RoleRegistry 提供
			}
			if len(s.Prompt) < 50 {
				t.Errorf("工作流 %q 阶段 %q 的 Prompt 太短(%d字符)", name, s.Name, len(s.Prompt))
			}
		}
	}
}
