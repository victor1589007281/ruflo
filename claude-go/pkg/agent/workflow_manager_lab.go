// workflow_manager_lab.go — Manager Lab 管理训练专用工作流。
//
// 说明：
//   - manager-lab-rehearsal-npc: 行为演练 NPC，逐轮入戏回应用户话术。
//   - manager-lab-rehearsal-critic: 行为演练 critic，对整段话术按 rubric 评分。
//   - manager-lab-simulation-v2: 六阶段决策情境模拟（支撑模式）。
//   - 输入通过 {objective} 由 manager-lab 后端序列化；多轮/编排状态由后端持有。
//
// 参考设计：manager-lab/design/CLAUDE-GO-WORKFLOW.md §2–§9
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
1. 先重读目标行为、好/差范例和对话。
2. 对每一维，先引用学员原话作为证据，再判断该维落在哪个分数段，最后给出 0-100 的整数分数（不要小数）。
3. 对最该改的 1-3 句话，给出 { quote, issue, rewrite }。
4. 用一句话总结与范例的主要差距。
5. 胜任力按 [-5,5] 给出有界 delta + 一句证据（数值累计由后端做，你不要累计）。

## 分数段
0-20 严重偏离；21-40 明显不足；41-60 部分符合；61-80 基本符合；81-100 优秀。

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

// managerLabSimulationV2Workflow 返回六阶段管理决策模拟工作流。
// 阶段 DAG：intake → situate → simulator → reflect → evaluator → coach。
func managerLabSimulationV2Workflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "manager-lab-simulation-v2",
		Description: "Manager Lab 六阶段管理情境模拟：intake → situate → simulator → reflect → evaluator → coach",
		Mode:        "orchestrated",
		Stages: []StageDef{
			{
				Name: "intake",
				Role: "researcher",
				Prompt: `你是一名严谨的需求解析员。请阅读下面的管理训练情景与用户决策，提炼关键信息。

## 原始输入
{objective}

## 任务
1. 用户真正想达成的目标。
2. 当前情景的关键约束（时间、预算、人力、关系）。
3. 用户决策中可能隐藏的风险点。
4. 用户决策体现出的胜任力信号（正面/负面）。

## 输出格式（只输出 JSON，不要解释）
{
  "userIntent": "一句话目标",
  "constraints": ["约束1", "约束2"],
  "riskPoints": ["风险1", "风险2"],
  "competencySignals": {
    "positive": ["信号1"],
    "negative": ["信号2"]
  }
}`,
			},
			{
				Name:      "situate",
				Role:      "researcher",
				DependsOn: []string{"intake"},
				Prompt: `你是一名情境领导诊断员。请根据 intake 结果与原始情景，判断用户对每位下属采取的管理风格是否合适。

## intake 结果
{intake}

## 原始输入
{objective}

## 情境领导规则
- D1 热情新手：能力低、意愿高 → S1 指令
- D2 幻灭学习者：能力中、意愿低 → S2 教练
- D3 能干但谨慎者：能力高、意愿波动 → S3 支持
- D4 自主成就者：能力高、意愿高 → S4 授权

## 输出格式（只输出 JSON，不要解释）
{
  "situationalLeadershipMatch": {
    "follower-1": { "expected": "S2", "actual": "S1", "score": 50, "reason": "..." }
  }
}`,
			},
			{
				Name:      "simulator",
				Role:      "architect",
				DependsOn: []string{"intake", "situate"},
				Prompt: `你是一名公司运行模拟器。请根据情景、用户决策与情境领导匹配结果，推演接下来一段时间内的运行结果。

## intake 结果
{intake}

## 情境领导匹配
{situate}

## 原始输入
{objective}

## 要求
- 事件需具体、有因果关系，不要泛泛而谈。
- 六维指标（finance, morale, customer, efficiency, risk, growth）取值 0-100 的整数。
- 识别 1-2 个延迟事件（decision consequences that manifest later）。
- 给出推演理由（reasoning）。

## 输出格式（只输出 JSON，不要解释）
{
  "timeSpan": "1 week",
  "events": [
    { "day": 1, "text": "...", "impact": { "morale": -5, "efficiency": 3 } }
  ],
  "delayedEvents": [
    { "triggerAfter": "1 month", "text": "...", "impact": { "risk": 10 } }
  ],
  "finalMetrics": {
    "finance": 70, "morale": 60, "customer": 75, "efficiency": 65, "risk": 40, "growth": 55
  },
  "reasoning": "..."
}`,
			},
			{
				Name:      "reflect",
				Role:      "reviewer",
				DependsOn: []string{"simulator"},
				Prompt: `你是一名反思引导员。请根据模拟结果生成反思问题，引导用户从经验中提取洞察。

## 模拟结果
{simulator}

## 原始输入
{objective}

## 任务
生成 2-3 个反思问题，关注：
1. 决策背后的假设。
2. 未使用的信息或未考虑的利益相关者。
3. 与理论（情境领导、授权、反馈）的连接。

## 输出格式（只输出 JSON，不要解释）
{
  "reflectionPrompts": [
    "..."
  ]
}`,
			},
			{
				Name:      "evaluator",
				Role:      "critic",
				DependsOn: []string{"simulator", "situate", "reflect"},
				Prompt: `你是一名管理决策评分员。请根据模拟结果、情境领导匹配度与阶段成功标准给出评分。

## 模拟结果
{simulator}

## 情境领导匹配
{situate}

## 反思
{reflect}

## 原始输入
{objective}

## 评级规则
- S：平均分 ≥ 90 且无维度低于 70
- A：平均分 ≥ 80 且无维度低于 60
- B：平均分 ≥ 70
- C：平均分 ≥ 60
- D：平均分 < 60

## 胜任力成长规则
- 只给用户明确展示过的胜任力加分。
- 每条成长需附带证据（一句话）。
- delta 范围为 [-5, 5] 的整数；数值累计由后端完成，你不要累计。

## 输出格式（只输出 JSON，不要解释）
{
  "score": {
    "overall": "B",
    "details": { "finance": 70, "morale": 60, "customer": 75, "efficiency": 65, "risk": 40, "growth": 55 }
  },
  "competencyGrowth": {
    "prioritization": { "delta": 3, "evidence": "你权衡了风险与可见性。" }
  },
  "passed": true
}`,
			},
			{
				Name:      "coach",
				Role:      "coach",
				DependsOn: []string{"evaluator"},
				Prompt: `你是一名管理教练。请针对以下决策、结果、情境领导匹配度与胜任力成长，给出建设性反馈。

## 评分与成长
{evaluator}

## 原始输入
{objective}

## 任务
1. 给出 2-3 条具体、引用决策或结果的建设性反馈。
2. 给出 2-3 条可执行的改进行动。
3. 推荐 1-2 张概念卡 ID（如 eisenhower-matrix, delegation-101）。
4. 给出 skillGrowth 摘要（ competency → 整数增量）。

## 风格
- 面向情景中指定的 roleLevel 阶段学员，语言亲切、具体、可操作。
- 优先使用白帽/右脑激励（成长、意义、创造力），避免羞辱性语言。

## 输出格式（只输出 JSON，不要解释）
{
  "feedback": ["...", "..."],
  "actionItems": ["...", "..."],
  "conceptCards": ["eisenhower-matrix"],
  "skillGrowth": {
    "prioritization": 2,
    "communication": 1
  }
}`,
			},
		},
	}
}
