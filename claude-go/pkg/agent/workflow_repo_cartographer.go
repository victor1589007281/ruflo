package agent

// workflow_repo_cartographer.go — "repo-cartographer" 内置工作流登记。
//
// 与 harness-evo 同一模式: 这里登记的**只是目录元数据**, 真实运行时是
// webapp 后端在宿主侧执行资产 bin/run_agent.py (codeintel 预检 → 命名 agent
// 制图 → atlas.py 权威渲染/审计), 不是 agent 团队舞台能表达的执行形态 ——
// k8s pod 内的 dashboard 摸不到宿主只读 checkout, 制图运行必须在宿主发起。
//
// 真正的接线:
//  1. webapp startWorkflowRun 对 name=repo-cartographer 就地转译: 不落
//     dashboard action, 直接 spawn run_agent.py, 轮询 runs/<dir>/run.json;
//  2. handleAction 对 team/create + workflow=repo-cartographer 拒绝落团队队列
//     (否则会按这里的装饰性 stages 起一个假制图团队)。
//
// 这里的 stages 如实描述真实 DAG 的形状: 三个确定性节点是**外部命令 gate**
// (atlas.py / codeintel CLI, 零 LLM), 只经 StageDef 的 Kind/Deterministic
// 声明给目录页/详情面板展示 —— claude-go 图执行器的 gate 节点没有"执行外部
// 命令"的能力, 不要指望把这份定义直接交给团队运行时执行。

func repoCartographerWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name: "repo-cartographer",
		Description: "只读仓库制图: codeintel 预检 → repo-cartographer agent 沿真实主链产出带逐行锚点的 atlas JSON → 权威渲染离线深浅色 HTML + 锚点审计 + 稳定性比对。由 webapp 宿主侧专用运行时执行 (spawn bin/run_agent.py), 非团队运行时",
		Mode:        "pipeline", // 目录展示形态; 真实执行 = 宿主 run_agent.py
		Stages: []StageDef{
			{
				Name:          "precheck",
				Kind:          "gate",
				Deterministic: true,
				Prompt:        "代码图谱预检: code_intel_status 查索引状态, 未索引则由外部 CLI 初始化 (零 LLM); 只读仓库不索引时回退 Grep/Read 并记入 limitations。参数: {objective}",
				Parallel:      true,
			},
			{
				Name: "cartograph",
				// Agent 资产住在状态目录 agents/repo-cartographer.md (宿主 named-agent 路径),
				// 不在角色注册表里 —— 声明为有意降级, 免得目录自检对每条内置报"角色未注册"。
				Agent:                 "repo-cartographer",
				AllowUnregisteredRole: true,
				Model:                 "opencode:deepseek-v4.1-flash",
				Prompt:                "制图 (repo-cartographer agent): 沿一条真实主链逐跳读码, 语义自检后提交完整 atlas 对象 (entry/call_chain/anchors/data_flow/state_machine/failures, 每锚点 file:line + source_line + claim + evidence)。预算 12 回合, 只读, 不写文件不渲染。参数: {objective}",
				DependsOn:             []string{"precheck"},
			},
			{
				Name:          "render",
				Kind:          "gate",
				Deterministic: true,
				Prompt:        "权威渲染 (atlas.py render, 零 LLM): schema/锚点逐字符校验 (source_line 比对、symbol 必须在范围内、claim 行号落位) → 生成唯一 index.html (深浅色离线)。参数: {objective}",
				DependsOn:     []string{"cartograph"},
			},
			{
				Name:          "verify",
				Kind:          "gate",
				Deterministic: true,
				Prompt:        "运行审计 (零 LLM): 仓库只读指纹比对 (制图前后 status/diff 摘要一致)、token/耗时预算门禁 (≤300k tokens · ≤20min)、模型指纹核对; blocked 时校验 diagnostics 落盘。参数: {objective}",
				DependsOn:     []string{"render"},
			},
		},
	}
}
