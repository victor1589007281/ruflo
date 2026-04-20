package unit

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

func TestNewWorkflowsRegistered(t *testing.T) {
	tests := []struct {
		name      string
		aliases   []string
		wfName    string
		minStages int
	}{
		{
			name:      "ml-training",
			aliases:   []string{"ml-training", "ml", "finetune", "training"},
			wfName:    "ml-training",
			minStages: 7,
		},
		{
			name:      "app-composite",
			aliases:   []string{"app", "miniprogram", "mobile"},
			wfName:    "app",
			minStages: 10,
		},
		{
			name:      "game-composite",
			aliases:   []string{"game", "gamedev", "game-dev"},
			wfName:    "game",
			minStages: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, alias := range tt.aliases {
				wf := agent.GetWorkflow(alias)
				if wf == nil {
					t.Errorf("GetWorkflow(%q) = nil, 期望非nil", alias)
					continue
				}
				if wf.Name != tt.wfName {
					t.Errorf("GetWorkflow(%q).Name = %q, 期望 %q", alias, wf.Name, tt.wfName)
				}
				if len(wf.Stages) < tt.minStages {
					t.Errorf("GetWorkflow(%q) 阶段数=%d, 期望至少 %d", alias, len(wf.Stages), tt.minStages)
				}
			}
		})
	}
}

func TestNewWorkflowsInList(t *testing.T) {
	workflows := agent.ListWorkflows()
	wantNames := map[string]bool{
		"ml-training": false,
		"app":         false,
		"game":        false,
	}

	for _, wf := range workflows {
		if _, ok := wantNames[wf.Name]; ok {
			wantNames[wf.Name] = true
		}
	}

	for name, found := range wantNames {
		if !found {
			t.Errorf("ListWorkflows() 缺少 %q", name)
		}
	}
}

func TestMLTrainingWorkflowStages(t *testing.T) {
	wf := agent.GetWorkflow("ml-training")
	if wf == nil {
		t.Fatal("ml-training workflow not found")
	}

	expectedRoles := []string{"data-analyst", "ml-engineer", "ml-architect", "ml-trainer", "ml-evaluator", "ml-optimizer", "synthesizer"}
	stageRoles := make(map[string]bool)
	for _, s := range wf.Stages {
		stageRoles[s.Role] = true
	}

	for _, role := range expectedRoles {
		if !stageRoles[role] {
			t.Errorf("ml-training 工作流缺少角色: %s", role)
		}
	}
}

func TestAppCompositeWorkflow(t *testing.T) {
	wf := agent.GetWorkflow("app")
	if wf == nil {
		t.Fatal("app workflow not found")
	}

	if wf.Mode != "app_composite" {
		t.Errorf("app Mode=%q, 期望 app_composite", wf.Mode)
	}

	crossTeamRoles := map[string]bool{
		"synthesizer":      false, // from research team
		"researcher":       false, // from research team
		"creative-planner": false, // from creative-v2 team
		"html-developer":   false, // from creative-v2 team
		"art-director":     false, // from creative-v2 team
		"architect":        false, // from dev team
		"coder":            false, // from dev team
		"tester":           false, // from dev team
		"reviewer":         false, // from dev team
		"miniprogram-dev":  false, // extended role
	}

	for _, s := range wf.Stages {
		if _, ok := crossTeamRoles[s.Role]; ok {
			crossTeamRoles[s.Role] = true
		}
	}

	for role, found := range crossTeamRoles {
		if !found {
			t.Errorf("app 复合工作流缺少跨团队角色: %s", role)
		}
	}

	parallelResearch := 0
	for _, s := range wf.Stages {
		if s.Parallel && (s.Name == "user-research" || s.Name == "tech-research") {
			parallelResearch++
		}
	}
	if parallelResearch < 2 {
		t.Errorf("app 缺少并行调研阶段, 只有 %d 个", parallelResearch)
	}
}

func TestGameCompositeWorkflow(t *testing.T) {
	wf := agent.GetWorkflow("game")
	if wf == nil {
		t.Fatal("game workflow not found")
	}

	if wf.Mode != "game_composite" {
		t.Errorf("game Mode=%q, 期望 game_composite", wf.Mode)
	}

	crossTeamRoles := map[string]bool{
		"game-designer":    false, // game specific
		"creative-planner": false, // from creative team
		"html-developer":   false, // from creative team
		"art-director":     false, // from creative team
		"game-architect":   false, // extended role
		"game-engine-dev":  false, // extended role
		"game-logic-dev":   false, // extended role
		"level-designer":   false, // extended role
		"game-tester":      false, // extended role
		"game-optimizer":   false, // extended role
	}

	for _, s := range wf.Stages {
		if _, ok := crossTeamRoles[s.Role]; ok {
			crossTeamRoles[s.Role] = true
		}
	}

	for role, found := range crossTeamRoles {
		if !found {
			t.Errorf("game 复合工作流缺少角色: %s", role)
		}
	}

	hasNarrativeEvolution := false
	for _, s := range wf.Stages {
		if s.Name == "narrative-evolution" {
			hasNarrativeEvolution = true
			break
		}
	}
	if !hasNarrativeEvolution {
		t.Error("game 缺少 narrative-evolution 阶段 (群体智能剧情)")
	}

	parallelDev := 0
	for _, s := range wf.Stages {
		if s.Parallel && (s.Name == "engine-dev" || s.Name == "logic-dev" || s.Name == "level-design") {
			parallelDev++
		}
	}
	if parallelDev < 3 {
		t.Errorf("game 引擎/逻辑/关卡应并行, 只有 %d 个", parallelDev)
	}
}

func TestExistingWorkflowsUnchanged(t *testing.T) {
	unchanged := []struct {
		name string
		mode string
	}{
		{"development", "adversarial_dev"},
		{"research", "fanout"},
		{"debate", "adversarial"},
		{"creative-v2", "creative_media"},
		{"predict", "predict"},
		{"novel-v3", "swarm_novel"},
		{"trading-v2", "trading_debate"},
	}

	for _, tt := range unchanged {
		wf := agent.GetWorkflow(tt.name)
		if wf == nil {
			t.Errorf("原有工作流 %q 被破坏: GetWorkflow 返回 nil", tt.name)
			continue
		}
		if wf.Mode != tt.mode {
			t.Errorf("原有工作流 %q Mode 被修改: %q, 期望 %q", tt.name, wf.Mode, tt.mode)
		}
	}
}
