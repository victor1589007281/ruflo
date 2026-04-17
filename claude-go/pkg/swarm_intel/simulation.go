package swarm_intel

import (
	"context"
	"fmt"
	"time"
)

// Simulator 场景模拟器。
// 支持三种模式:
//   - social: 多 Agent 社会模拟 (参考 MiroFish 的 OASIS 模式)
//   - game: 博弈论模拟 (Agent 代表不同利益方)
//   - montecarlo: 蒙特卡洛场景树 (并行世界线)
type Simulator struct {
	llm    LLMClient
	notify NotifyFunc
	cc     *ContextCompressor // M10: 上下文压缩
}

// NewSimulator 创建场景模拟器。
func NewSimulator(llm LLMClient, notify NotifyFunc) *Simulator {
	if notify == nil {
		notify = func(_, _ string) {}
	}
	return &Simulator{llm: llm, notify: notify, cc: DefaultContextCompressor()}
}

// Simulate 执行场景模拟。
// 支持 9 种模式:
//   - social: 社会模拟
//   - game: 博弈论
//   - montecarlo: 蒙特卡洛场景树
//   - crisis: 危机推演 (新)
//   - org: 组织/团队动力学 (新)
//   - creative: 创意涌现 (新)
//   - market: 市场竞争模拟 (新)
//   - policy: 政策推演 (新)
//   - tech: 技术演进推演 (新)
func (s *Simulator) Simulate(ctx context.Context, chatID string, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	switch cfg.Mode {
	case "social":
		return s.socialSimulation(ctx, chatID, objective, cfg)
	case "game":
		return s.gameSimulation(ctx, chatID, objective, cfg)
	case "montecarlo":
		return s.monteCarloSimulation(ctx, chatID, objective, cfg)
	case "crisis":
		return s.crisisSimulation(ctx, chatID, objective, cfg)
	case "org":
		return s.orgSimulation(ctx, chatID, objective, cfg)
	case "creative":
		return s.creativeSimulation(ctx, chatID, objective, cfg)
	case "market":
		return s.marketSimulation(ctx, chatID, objective, cfg)
	case "policy":
		return s.policySimulation(ctx, chatID, objective, cfg)
	case "tech":
		return s.techSimulation(ctx, chatID, objective, cfg)
	default:
		return s.monteCarloSimulation(ctx, chatID, objective, cfg)
	}
}

// socialSimulation 多 Agent 社会模拟 (M10 v2: 单 prompt 模式, 从 5 次→1 次 LLM 调用)。
func (s *Simulator) socialSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, fmt.Sprintf("🌐 社会模拟启动: %d 个 Agent, %d 轮 (单 prompt)", cfg.Agents, cfg.Rounds))

	prompt := fmt.Sprintf(`你是社会模拟引擎。执行完整的多 Agent 社会模拟。

目标: %s
Agent 数量: %d
模拟轮数: %d

请完成以下任务:
1. 设计 %d 个具有不同立场、性格的 Agent
2. 模拟 %d 轮互动, 描述每轮关键行为和群体动态变化
3. 识别涌现行为 (群体极化、信息级联、社会惰化、非正式领导等)
4. 总结出 2-4 个可能的演化场景及概率

输出严格JSON (不要markdown代码块, 不要其他文字):
{
  "scenarios": [
    {"name": "场景名", "probability": 0.5, "description": "150字以内描述", "key_events": ["事件1", "事件2"]}
  ],
  "emergent_behaviors": ["涌现行为1", "涌现行为2", "涌现行为3"],
  "summary": "300字以内综合分析"
}`, objective, cfg.Agents, cfg.Rounds, cfg.Agents, cfg.Rounds)

	resp, err := s.llm.SimpleComplete(ctx, "你是社会模拟与群体行为专家。只输出JSON。", prompt)
	if err != nil {
		return nil, fmt.Errorf("社会模拟失败: %w", err)
	}

	return parseSimulationResult("social", cfg.Rounds, resp), nil
}

// gameSimulation 博弈论模拟。
func (s *Simulator) gameSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, fmt.Sprintf("♟️ 博弈模拟启动: %d 个博弈方, %d 轮", cfg.Agents, cfg.Rounds))

	prompt := fmt.Sprintf(`你是博弈论模拟引擎。

场景: %s
博弈方数量: %d
模拟轮数: %d

请执行完整的博弈模拟:
1. 识别各博弈方的利益和策略空间
2. 模拟多轮策略互动
3. 分析是否收敛到纳什均衡
4. 给出各方最优策略和预期结果

输出严格JSON (不要markdown代码块, description每个150字以内):
{
  "scenarios": [{"name": "均衡名", "probability": 0.5, "description": "描述", "key_events": ["策略变化"]}],
  "emergent_behaviors": ["合作涌现", "背叛模式"],
  "summary": "博弈分析总结"
}`, objective, cfg.Agents, cfg.Rounds)

	resp, err := s.llm.SimpleComplete(ctx, "你是顶级博弈论专家和模拟引擎。", prompt)
	if err != nil {
		return nil, fmt.Errorf("博弈模拟失败: %w", err)
	}

	return parseSimulationResult("game", cfg.Rounds, resp), nil
}

// monteCarloSimulation 蒙特卡洛场景树 (M10: 并行化 + 健壮解析)。
func (s *Simulator) monteCarloSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	branchNames := cfg.Scenarios
	if len(branchNames) == 0 {
		branchNames = []string{"乐观情景", "基准情景", "悲观情景", "黑天鹅情景"}
	}
	s.notify(chatID, fmt.Sprintf("🎲 蒙特卡洛模拟启动: %d 条世界线 (并行)", len(branchNames)))

	// M10: 并行化 — 各世界线独立, 可安全并行
	fanBranches := make(map[string]BranchFunc)
	for _, branch := range branchNames {
		branch := branch
		fanBranches[branch] = func(branchCtx context.Context) (string, error) {
			prompt := fmt.Sprintf(`你是场景模拟引擎。沿着特定的世界线推演未来。

目标问题: %s
世界线: %s

请根据现实情况独立估算这条世界线的发生概率 (0.0-1.0), 不要简单均分。
输出严格JSON (不要markdown代码块):
{"name": "%s", "probability": 你估算的概率, "description": "200字以内推演描述", "key_events": ["事件1","事件2","事件3"]}`,
				objective, branch, branch)

			return s.llm.SimpleComplete(branchCtx, "你是场景推演专家。只输出JSON。", prompt)
		}
	}

	fanCfg := DefaultFanOutConfig()
	fanCfg.MaxConcurrency = 3
	fanResults := FanOutCollect(ctx, fanCfg, fanBranches)

	// M10: 健壮 JSON 解析 — 逐分支解析
	var directScenarios []ScenarioOutcome
	for _, r := range fanResults {
		if r.Error != nil {
			continue
		}
		result := ParseJSON[ScenarioOutcome](r.Value)
		if result.OK && result.Value.Name != "" {
			directScenarios = append(directScenarios, result.Value)
		}
	}

	// 合并 + 生成摘要
	scenarioSummaries := ""
	for _, sc := range directScenarios {
		scenarioSummaries += fmt.Sprintf("- %s (%.0f%%): %s\n", sc.Name, sc.Probability*100, s.cc.TruncateField(sc.Description, 100))
	}
	mergePrompt := fmt.Sprintf(`基于蒙特卡洛场景树推演, 归纳涌现行为和综合分析。

目标: %s
各场景:
%s

输出严格JSON (不要markdown代码块):
{
  "emergent_behaviors": ["共同模式1", "共同模式2"],
  "summary": "200字以内综合分析"
}`, objective, scenarioSummaries)

	mergeResp, err := s.llm.SimpleComplete(ctx, "你是场景分析专家。只输出JSON。", mergePrompt)

	simResult := &SimulationResult{
		Mode:      "montecarlo",
		Rounds:    len(directScenarios),
		Scenarios: directScenarios,
		CreatedAt: time.Now(),
	}

	// 归一化概率
	var probSum float64
	for _, sc := range simResult.Scenarios {
		probSum += sc.Probability
	}
	if probSum > 0 {
		for i := range simResult.Scenarios {
			simResult.Scenarios[i].Probability /= probSum
		}
	}

	if err == nil {
		type mergeResult struct {
			Emergent []string `json:"emergent_behaviors"`
			Summary  string   `json:"summary"`
		}
		parsed := ParseJSON[mergeResult](mergeResp)
		if parsed.OK {
			simResult.Emergent = parsed.Value.Emergent
			simResult.Summary = parsed.Value.Summary
		}
	}

	return simResult, nil
}

// parseSimulationResult M10 健壮解析: 多级 fallback。
func parseSimulationResult(mode string, rounds int, raw string) *SimulationResult {
	result := &SimulationResult{
		Mode:      mode,
		Rounds:    rounds,
		CreatedAt: time.Now(),
	}

	// Level 1: 使用 RobustJSONParser 解析完整结构
	type simResultJSON struct {
		Scenarios []ScenarioOutcome `json:"scenarios"`
		Emergent  []string          `json:"emergent_behaviors"`
		Summary   string            `json:"summary"`
	}

	parsed := ParseJSON[simResultJSON](raw)
	if parsed.OK {
		result.Scenarios = parsed.Value.Scenarios
		result.Emergent = parsed.Value.Emergent
		result.Summary = parsed.Value.Summary
		return result
	}

	// Level 2: 尝试提取多个独立 JSON 对象
	allJSON := extractAllJSON(raw)
	for _, js := range allJSON {
		// 单个场景
		sc := ParseJSON[ScenarioOutcome](js)
		if sc.OK && sc.Value.Name != "" {
			result.Scenarios = append(result.Scenarios, sc.Value)
			continue
		}
		// 包含 scenarios 数组的块
		bulk := ParseJSON[simResultJSON](js)
		if bulk.OK && len(bulk.Value.Scenarios) > 0 {
			result.Scenarios = append(result.Scenarios, bulk.Value.Scenarios...)
			result.Emergent = append(result.Emergent, bulk.Value.Emergent...)
			if bulk.Value.Summary != "" {
				result.Summary = bulk.Value.Summary
			}
		}
	}

	// Level 3: fallback — 截断原始文本作为 summary
	if result.Summary == "" {
		result.Summary = truncate(raw, 1000)
	}
	return result
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// crisisSimulation 危机推演模拟。
// 模拟危机事件的升级、响应和恢复全过程。
func (s *Simulator) crisisSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, fmt.Sprintf("🚨 危机推演启动: %d 轮", cfg.Rounds))

	prompt := fmt.Sprintf(`你是危机推演引擎。模拟一场完整的危机事件。

危机场景: %s
Agent角色: 决策者、媒体、公众、对手方、盟友
轮数: %d

请模拟危机的完整生命周期:
1. 触发阶段: 危机如何爆发, 初始信号
2. 升级阶段: 危机如何扩大, 各方初始反应
3. 峰值阶段: 最严重时的状态, 关键决策点
4. 应对阶段: 各方应对策略, 博弈互动
5. 恢复阶段: 危机如何收敛, 长期影响

关注涌现行为: 信息级联、恐慌传播、协调失败、意外联盟

输出严格JSON (不要markdown代码块, description每个150字以内):
{
  "scenarios": [
    {"name": "快速控制", "probability": 0.3, "description": "描述", "key_events": ["事件"]},
    {"name": "持续升级", "probability": 0.4, "description": "描述", "key_events": ["事件"]},
    {"name": "系统性崩溃", "probability": 0.2, "description": "描述", "key_events": ["事件"]},
    {"name": "意外转机", "probability": 0.1, "description": "描述", "key_events": ["事件"]}
  ],
  "emergent_behaviors": ["涌现行为"],
  "summary": "综合分析"
}`, objective, cfg.Rounds)

	resp, err := s.llm.SimpleComplete(ctx, "你是危机管理和情景推演专家。深入分析危机演化路径。", prompt)
	if err != nil {
		return nil, fmt.Errorf("危机推演失败: %w", err)
	}
	return parseSimulationResult("crisis", cfg.Rounds, resp), nil
}

// orgSimulation 组织/团队动力学模拟。
// 模拟组织内部的决策、文化演化和团队动力学。
func (s *Simulator) orgSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, fmt.Sprintf("🏢 组织动力学模拟启动: %d 个成员, %d 轮", cfg.Agents, cfg.Rounds))

	prompt := fmt.Sprintf(`你是组织行为学模拟引擎。模拟组织内部的动力学。

目标场景: %s
组织成员: %d
轮数: %d

请模拟组织动力学:
1. 权力结构: 正式vs非正式权力网络
2. 信息流动: 沟通瓶颈、信息不对称
3. 文化演化: 规范如何形成和变化
4. 决策过程: 群体决策质量 vs 个体决策
5. 冲突与协作: 部门博弈、资源争夺

关注涌现行为: 群体极化、社会惰化、群体思维、创新抑制、非正式领导涌现

输出严格JSON (不要markdown代码块, 简洁):
{
  "scenarios": [{"name": "场景", "probability": 0.0, "description": "描述", "key_events": []}],
  "emergent_behaviors": ["涌现行为"],
  "summary": "组织动力学分析"
}`, objective, cfg.Agents, cfg.Rounds)

	resp, err := s.llm.SimpleComplete(ctx, "你是组织行为学专家。深入分析团队动力学和涌现行为。", prompt)
	if err != nil {
		return nil, fmt.Errorf("组织模拟失败: %w", err)
	}
	return parseSimulationResult("org", cfg.Rounds, resp), nil
}

// creativeSimulation 创意涌现模拟 (M10 v2: 单 prompt 模式, 从 3 次→1 次 LLM 调用)。
func (s *Simulator) creativeSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, "💡 创意涌现模拟启动 — 多视角头脑风暴 (单 prompt)")

	prompt := fmt.Sprintf(`你是创意涌现引擎。对以下目标同时执行"发散+收敛"创意流程。

目标: %s

流程:
1. 发散: 从6个视角 (技术专家、艺术家、经济学家、哲学家、儿童、外星人) 分别提出创意
2. 收敛: 将最有潜力的创意跨视角组合为 3 个综合方案
3. 涌现: 找出任何单一视角无法产生的"跨领域灵感"

输出严格JSON (不要markdown代码块, 不要其他文字):
{
  "scenarios": [
    {"name": "方案名", "probability": 0.5, "description": "150字以内方案描述", "key_events": ["关键步骤1", "步骤2"]}
  ],
  "emergent_behaviors": ["涌现洞察1", "涌现洞察2", "涌现洞察3"],
  "summary": "300字以内创意涌现总结"
}`, objective)

	resp, err := s.llm.SimpleComplete(ctx, "你是跨学科创新专家。善于从碎片中发现系统性创新。只输出JSON。", prompt)
	if err != nil {
		return nil, fmt.Errorf("创意涌现失败: %w", err)
	}
	return parseSimulationResult("creative", 1, resp), nil
}

// marketSimulation 市场竞争模拟。
// 模拟多个市场参与者的竞争动态和市场演化。
func (s *Simulator) marketSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, fmt.Sprintf("📈 市场竞争模拟启动: %d 个参与者, %d 轮", cfg.Agents, cfg.Rounds))

	prompt := fmt.Sprintf(`你是市场竞争模拟引擎。模拟多方市场竞争动态。

市场场景: %s
参与者数: %d
模拟轮数: %d

请模拟完整的市场竞争:
1. 市场结构: 各参与者定位、资源、策略
2. 竞争动态: 价格战、差异化、进入壁垒
3. 市场演化: 集中度变化、新进入者、退出者
4. 消费者行为: 偏好转移、锁定效应、网络效应
5. 监管影响: 政策干预如何改变竞争格局

关注涌现: 赢者通吃、平台化、颠覆式创新、市场崩溃

输出严格JSON (不要markdown代码块, 简洁):
{
  "scenarios": [{"name": "市场走向", "probability": 0.0, "description": "描述", "key_events": []}],
  "emergent_behaviors": ["市场涌现行为"],
  "summary": "市场竞争分析"
}`, objective, cfg.Agents, cfg.Rounds)

	resp, err := s.llm.SimpleComplete(ctx, "你是产业经济学和市场竞争专家。深入分析竞争动态。", prompt)
	if err != nil {
		return nil, fmt.Errorf("市场模拟失败: %w", err)
	}
	return parseSimulationResult("market", cfg.Rounds, resp), nil
}

// policySimulation 政策推演模拟。
// 模拟政策实施的多阶段影响和各方博弈。
func (s *Simulator) policySimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, "🏛️ 政策推演启动")

	prompt := fmt.Sprintf(`你是政策推演引擎。模拟政策实施的全过程和多阶段影响。

政策场景: %s

请进行多层次推演:
1. 一阶效应: 政策直接影响的群体和指标
2. 二阶效应: 间接影响、行为适应、套利行为
3. 三阶效应: 长期系统性变化、制度演化
4. 各方博弈: 支持者/反对者/中立方的策略互动
5. 意外后果: 政策制定者未预见的涌现效果

关注涌现: Cobra效应 (政策适得其反)、制度变迁、利益重组

输出严格JSON (不要markdown代码块, 简洁):
{
  "scenarios": [{"name": "政策效果路径", "probability": 0.0, "description": "描述", "key_events": []}],
  "emergent_behaviors": ["涌现效应"],
  "summary": "政策推演总结"
}`, objective)

	resp, err := s.llm.SimpleComplete(ctx, "你是公共政策和制度经济学专家。", prompt)
	if err != nil {
		return nil, fmt.Errorf("政策推演失败: %w", err)
	}
	return parseSimulationResult("policy", cfg.Rounds, resp), nil
}

// techSimulation 技术演进推演。
// 模拟技术路线的竞争、融合和颠覆。
func (s *Simulator) techSimulation(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	s.notify(chatID, "🔬 技术演进推演启动")

	prompt := fmt.Sprintf(`你是技术演进推演引擎。模拟技术发展路线的多种可能。

技术场景: %s

请推演:
1. 技术路线竞争: 现有技术路线及其各自优劣
2. S曲线分析: 各技术处于哪个发展阶段
3. 颠覆性技术: 可能从哪些方向出现颠覆者
4. 融合趋势: 哪些技术可能意外融合
5. 时间线: 关键里程碑的预期时间节点
6. 生态系统: 核心技术如何影响上下游

关注涌现: 技术范式转移、涌现的应用场景、意外的跨领域融合

输出严格JSON (不要markdown代码块, 简洁):
{
  "scenarios": [{"name": "技术路径", "probability": 0.0, "description": "描述", "key_events": []}],
  "emergent_behaviors": ["技术涌现"],
  "summary": "技术演进分析"
}`, objective)

	resp, err := s.llm.SimpleComplete(ctx, "你是技术未来学家和产业分析专家。", prompt)
	if err != nil {
		return nil, fmt.Errorf("技术推演失败: %w", err)
	}
	return parseSimulationResult("tech", cfg.Rounds, resp), nil
}

// ListSimulationModes 返回所有可用的模拟模式。
func ListSimulationModes() []SimModeInfo {
	return []SimModeInfo{
		{Mode: "social", Name: "社会模拟", Emoji: "🌐", Desc: "多Agent社会互动, 观察群体涌现行为"},
		{Mode: "game", Name: "博弈模拟", Emoji: "♟️", Desc: "多方策略博弈, 纳什均衡分析"},
		{Mode: "montecarlo", Name: "蒙特卡洛", Emoji: "🎲", Desc: "并行世界线推演, 概率加权"},
		{Mode: "crisis", Name: "危机推演", Emoji: "🚨", Desc: "危机升级/响应/恢复全生命周期"},
		{Mode: "org", Name: "组织动力学", Emoji: "🏢", Desc: "团队决策/文化/权力动力学"},
		{Mode: "creative", Name: "创意涌现", Emoji: "💡", Desc: "多视角头脑风暴 + 跨领域创新"},
		{Mode: "market", Name: "市场竞争", Emoji: "📈", Desc: "多方市场竞争动态和演化"},
		{Mode: "policy", Name: "政策推演", Emoji: "🏛️", Desc: "政策多阶效应和意外后果"},
		{Mode: "tech", Name: "技术演进", Emoji: "🔬", Desc: "技术路线竞争/融合/颠覆"},
	}
}

// SimModeInfo 模拟模式信息。
type SimModeInfo struct {
	Mode  string `json:"mode"`
	Name  string `json:"name"`
	Emoji string `json:"emoji"`
	Desc  string `json:"desc"`
}
