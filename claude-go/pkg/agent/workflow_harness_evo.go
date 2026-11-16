package agent

// workflow_harness_evo.go — "harness-evo" 内置工作流登记。
//
// 它登记的**只是目录元数据**: harness-evo 的真实运行时是 pkg/harnesspro
// 进化编排器 + pkg/graph 两张 DAG (出卷图 / 每轮 9 节点图), 不是 agent 团队
// 舞台能表达的执行形态。真正的接线在 pkg/dashboard:
//
//  1. POST /api/harness-evo/runs —— 进化运行触发/列表/详情/停止 (webapp 轮询);
//  2. handleAction 把 team/create + workflow=harness-evo 就地转译成进化运行,
//     不落团队队列 (否则会按这里的装饰性 stages 起一个假进化团队)。
//
// 这里的 stages 如实描述论文 DAG 的形状, 仅供目录页/详情面板展示数据流;
// Agent 字段留空 —— 节点由编排器内的 LLM 角色 (出卷/诊断/出案/动刀) 执行,
// 不经角色注册表。

func harnessEvoWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name: "harness-evo",
		Description: "HarnessPro harness 自动进化 (GLM 论文复现): 冻结合成测试卷 → 训练集多轮 rollout → 诊断 → 三向出案 → 动刀 git → 地板/帕累托/三档选点。由专用进化引擎执行 (POST /api/harness-evo/runs), 非团队运行时",
		Mode:        "pipeline", // 目录展示形态; 真实执行走 harnesspro 编排器
		Stages: []StageDef{
			{
				Name:  "testsynth",
				Prompt: "出卷 (TestSynth): 为任务池每题合成 10 条冻结测试断言 (可机械执行、零 LLM 裁判), 覆盖全池 (训练+验证)。参数: {objective}",
				Parallel: true,
			},
			{
				Name:     "selfcheck",
				Prompt:   "出卷自检: schema 校验 + FreezeTests 冻结 (评分器漂移防线, 拒绝重写出卷)",
				DependsOn: []string{"testsynth"},
			},
			{
				Name:     "floor-eval",
				Prompt:   "地板评定: h0 在验证集评测, 定 pass 率地板",
				DependsOn: []string{"selfcheck"},
			},
			{
				Name:     "rollouts",
				Prompt:   "训练集 rollout: 每候选 × 每题 × k 次执行, 机械统计 pass/cost (不经 LLM)",
				DependsOn: []string{"floor-eval"},
				Parallel: true,
			},
			{
				Name:     "analyze",
				Prompt:   "轨迹诊断 (Analyzer): 对失败 rollout 出结构化问题清单",
				DependsOn: []string{"rollouts"},
				Parallel: true,
			},
			{
				Name:     "plan",
				Prompt:   "三向出案 (Planner): 每候选三份改进计划 (短/中/长指令), 回显父 harness",
				DependsOn: []string{"analyze"},
				Parallel: true,
			},
			{
				Name:     "edit",
				Prompt:   "动刀 (Editor): 独立工作区按 plan 重写 harness, git 提交新候选",
				DependsOn: []string{"plan"},
				Parallel: true,
			},
			{
				Name:     "eval",
				Prompt:   "验证集评测: 每候选每题 1 次, infra_error 不计入",
				DependsOn: []string{"edit"},
				Parallel: true,
			},
			{
				Name:     "select",
				Prompt:   "选点: 地板过滤 → 帕累托前沿 → 三档 J=α·d_pass²+β·d_cost² (lite/medium/heavy)",
				DependsOn: []string{"eval"},
			},
		},
	}
}
