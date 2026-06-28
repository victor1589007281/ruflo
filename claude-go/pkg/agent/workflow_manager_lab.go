// workflow_manager_lab.go — Manager Lab 管理训练专用工作流。
//
// 说明：
//   - manager-lab-rehearsal-npc: 行为演练 NPC，逐轮入戏回应用户话术。
//   - manager-lab-rehearsal-critic: 行为演练 critic，对整段话术按 rubric 评分。
//   - 两者都是单阶段 pipeline，输入通过 {objective} 由 manager-lab 后端序列化。
//   - 多轮对话状态由 manager-lab 后端持有并注入 objective，不依赖 claude-go blackboard。
//
// 参考设计：manager-lab/design/CLAUDE-GO-WORKFLOW.md §10
package agent

func managerLabRehearsalNPCWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "manager-lab-rehearsal-npc",
		Description: "Manager Lab 行为演练 NPC：扮演带人设/D状态的下属或同事，对用户的每一句话术实时、入戏地回应",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "npc",
				Role: "coach",
				Prompt: `你正在扮演一名带人设的下属/同事。请严格按人设与关系记忆回应用户（管理者）的这句话术。

## 角色与人设
{objective}

## 要求
- 以角色身份回应，真实、有张力，可表达情绪、防御或反问。
- 只输出角色台词，1-3 句，口语化。
- 不要解说、不要跳出角色、不要总结。
- 不要输出 JSON，只输出纯文本台词。`,
			},
		},
	}
}

func managerLabRehearsalCriticWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "manager-lab-rehearsal-critic",
		Description: "Manager Lab 行为演练 critic：对用户实际说过的话按 rubric 打分、逐句改写并对照范例",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "critic",
				Role: "critic",
				Prompt: `你是管理沟通教练。请只针对学员在演练中实际说的话进行评分，不要评价 NPC。

## 输入
{objective}

## 任务
1. 按 rubric 各维 0-100 打分。
2. 对最该改的 1-3 句话，给出 { quote, issue, rewrite }。
3. 用一句话总结与范例的主要差距。
4. 胜任力按 [-5,5] 给出有界 delta + 一句证据（数值累计由后端做，你不要累计）。

## 输出格式（只输出 JSON，不要解释）
{
  "rubricScores": { "clarity": 70, "specificity": 55, "listening": 40, "styleMatchD": 60, "issueNotPerson": 80 },
  "lineFeedback": [
    { "quote": "原话", "issue": "问题", "rewrite": "建议改法" }
  ],
  "comparedToModel": "与范例对照的一句话总结",
  "competencyEvidence": {
    "coaching": { "delta": 2, "evidence": "一句话证据" }
  }
}`,
			},
		},
	}
}
