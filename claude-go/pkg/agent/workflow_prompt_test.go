package agent

import (
	"context"
	"strings"
	"testing"
)

func TestBuildStagePromptWithRoles_PreservesObjectiveAcrossTemplateShapes(t *testing.T) {
	const objective = "先 Read /home/victor/base/git/gitee/diagramforge/tools/arch2d/runs/test/objective.md，然后严格按 BRIEF 执行。MODEL_ALIAS=opencode:hungerg-free。"
	roles := NewRoleRegistryFrom(nil)
	for _, role := range []*RoleDef{
		{Name: "persona-no-objective", Category: "workflow", SystemPrompt: "你是海报审计员，只根据源码产出模型。"},
		{Name: "role-with-objective", Category: "workflow", SystemPrompt: "你是海报审计员。\n任务：{objective}\n{prev_result}"},
	} {
		if err := roles.RegisterCustom(role); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		name     string
		stage    StageDef
		useRoles bool
	}{
		{
			name:     "人格角色加无目标占位的阶段说明",
			stage:    StageDef{Name: "intake", Agent: "persona-no-objective", Prompt: "先读 BRIEF，再写 MODEL_PATH。"},
			useRoles: true,
		},
		{
			name:     "人格角色加空阶段说明",
			stage:    StageDef{Name: "intake", Agent: "persona-no-objective"},
			useRoles: true,
		},
		{
			name:     "角色模板已承载目标",
			stage:    StageDef{Name: "intake", Agent: "role-with-objective", Prompt: "这段阶段说明应被角色模板替代。"},
			useRoles: true,
		},
		{
			name:  "角色表缺失但阶段说明非空",
			stage: StageDef{Name: "intake", Agent: "unregistered", Prompt: "先读 BRIEF，再写 MODEL_PATH。"},
		},
		{
			name:  "角色表缺失且阶段说明为空",
			stage: StageDef{Name: "intake", Agent: "unregistered"},
		},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var registry *RoleRegistry
			if c.useRoles {
				registry = roles
			}
			got := buildStagePromptWithRoles(c.stage, objective, nil, registry)
			if n := strings.Count(got, objective); n != 1 {
				t.Fatalf("目标应完整出现且只出现一次，实际 %d 次:\n%s", n, got)
			}
			if c.useRoles && c.stage.Agent == "role-with-objective" && strings.Contains(got, "这段阶段说明应被角色模板替代") {
				t.Fatal("角色模板已含目标时不应再拼接 stage prompt")
			}
		})
	}
}

func TestBuildStagePromptWithRoles_ObjectiveFallbackIncludesDependency(t *testing.T) {
	const objective = "先 Read /home/victor/base/git/gitee/diagramforge/tools/arch2d/runs/test/objective.md。"
	roles := NewRoleRegistryFrom(nil)
	if err := roles.RegisterCustom(&RoleDef{
		Name: "persona-no-objective", Category: "workflow", SystemPrompt: "你是海报审计员。",
	}); err != nil {
		t.Fatal(err)
	}
	got := buildStagePromptWithRoles(
		StageDef{Name: "dry", Agent: "persona-no-objective", DependsOn: []string{"intake"}},
		objective, map[string]string{"intake": "model ready"}, roles,
	)
	if !strings.Contains(got, "### Dependency output from intake:") || !strings.Contains(got, "model ready") {
		t.Fatalf("目标补齐块丢失上游交接:\n%s", got)
	}
}

func TestExecuteSingleStage_ObjectiveReachesRunner(t *testing.T) {
	const objective = "先 Read /home/victor/base/git/gitee/diagramforge/tools/arch2d/runs/test/objective.md，再按 BRIEF 执行。"
	var gotPrompt string
	we := &WorkflowExecutor{
		roles: NewRoleRegistryFrom(nil),
		factory: func(_ context.Context, _, _ string) (AgentRunner, error) {
			return &promptCaptureRunner{capture: func(prompt string) { gotPrompt = prompt }}, nil
		},
		notify: func(_, _ string) {},
	}
	if err := we.roles.RegisterCustom(&RoleDef{
		Name: "persona-no-objective", Category: "workflow", SystemPrompt: "你是海报审计员。",
	}); err != nil {
		t.Fatal(err)
	}
	team := newStubTeam(t, "objective-delivery")
	team.Agents["persona-no-objective"] = &BGAgent{Name: "persona-no-objective", Role: "persona-no-objective", Status: AgentStatusIdle}
	stage := StageDef{Name: "intake", Agent: "persona-no-objective", Prompt: "先读 BRIEF，再写 MODEL_PATH。"}
	res := we.ExecuteSingleStage(context.Background(), stage, objective, nil, team)
	if res.Status != TaskCompleted {
		t.Fatalf("阶段应完成: %+v", res)
	}
	if n := strings.Count(gotPrompt, objective); n != 1 {
		t.Fatalf("runner 未收到完整目标或目标重复，实际 %d 次:\n%s", n, gotPrompt)
	}
}

type promptCaptureRunner struct {
	capture func(string)
}

func (r *promptCaptureRunner) Execute(_ context.Context, prompt string) (string, error) {
	r.capture(prompt)
	return "已读取 BRIEF 并完成源码审计，模型与计划均已落盘。\n\n## 分析\n- 结论：目标与固定路径完整送达。\n- 方案：按 BRIEF 验收。", nil
}
