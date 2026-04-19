// Swarm — Kimi K2.5 启发的动态蜂群编排器。
//
// 核心思想 (参考 Kimi K2.5 Agent Swarm + PARL):
//   - 不预定义角色: LLM 动态分析任务, 决定拆分多少子任务
//   - 拓扑排序: 按依赖关系分层, 同层并行执行
//   - 异步检查点: 每个子任务执行前后存档
//   - 结果汇聚: LLM 综合所有子任务结果, 生成最终答案
//
// 执行流程:
//
//  1. Decompose: LLM 分析目标, 生成 SubTask DAG (含依赖)
//
//  2. Schedule:  拓扑排序, 按层级并行调度到 AgentPool
//
//  3. Execute:   每层并行执行, 完成后写入 Blackboard
//
//  4. Merge:     LLM 综合所有结果, 生成最终输出
//
//     ┌─────────────────────────────────────────────────┐
//     │ SwarmOrchestrator                               │
//     │  decompose() → SubTask DAG                      │
//     │  topologicalLevels() → [[level0], [level1], ...]│
//     │  executeLevel() → 并行执行同层子任务            │
//     │  merge() → LLM 综合最终结果                     │
//     └─────────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SubTask 蜂群子任务 (LLM 动态生成)。
type SubTask struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Role        string   `json:"role"`
	DependsOn   []string `json:"dependsOn,omitempty"`
	Priority    int      `json:"priority,omitempty"` // 0=normal, 1=high, 2=critical
}

// DecompositionPlan LLM 生成的任务拆解计划。
type DecompositionPlan struct {
	SubTasks  []SubTask `json:"subTasks"`
	Strategy  string    `json:"strategy"` // parallel, pipeline, hybrid
	Rationale string    `json:"rationale"`
}

// ComplexityProfile 任务复杂度评估结果 (Smart Role Router)。
// 参考 DRA (arXiv:2601.17152): 按任务需求动态选择最少角色子集。
type ComplexityProfile struct {
	NeedsCoding   bool     // 是否涉及编码
	NeedsResearch bool     // 是否需要调研
	NeedsReview   bool     // 是否需要审查
	NeedsTesting  bool     // 是否需要测试
	NeedsDesign   bool     // 是否需要架构设计
	DomainBreadth int      // 领域广度 (1-3: 窄/中/广)
	Complexity    int      // 整体复杂度 (1-3: 低/中/高)
	SuggestedMax  int      // 建议最大角色数
	Roles         []string // 推荐角色列表
}

// SubTaskScore 子任务质量评估 (Quality Gate)。
type SubTaskScore struct {
	TaskID     string  `json:"taskId"`
	Confidence float64 `json:"confidence"` // 0-1: 输出质量置信度
	Degraded   bool    `json:"degraded"`   // 是否为降级结果
	RetryCount int     `json:"retryCount"`
}

// SwarmCheckpoint 蜂群执行检查点。
type SwarmCheckpoint struct {
	Plan         *DecompositionPlan `json:"plan"`
	CompletedIDs []string           `json:"completedIds"`
	ResultMap    map[string]string  `json:"resultMap"`
	CurrentLevel int                `json:"currentLevel"`
	Timestamp    time.Time          `json:"timestamp"`
}

// SwarmOrchestrator 蜂群编排器。
type SwarmOrchestrator struct {
	llm         LLMClient
	pool        *AgentPool
	taskTracker TaskTracker
	notify      NotifyFunc
	chatID      string
	maxAgents   int
	evolution   *EvolutionEngine
	roles       *RoleRegistry
	stateDir    string // 检查点持久化目录
}

// NewSwarmOrchestrator 创建蜂群编排器。
func NewSwarmOrchestrator(llm LLMClient, pool *AgentPool, taskTracker TaskTracker, notify NotifyFunc, chatID string, maxAgents int) *SwarmOrchestrator {
	if maxAgents <= 0 {
		maxAgents = 8
	}
	return &SwarmOrchestrator{
		llm:         llm,
		pool:        pool,
		taskTracker: taskTracker,
		notify:      notify,
		chatID:      chatID,
		maxAgents:   maxAgents,
	}
}

// Execute 蜂群执行: 角色路由 → 动态分解 → 拓扑排序 → 逐层并行(含质量门控) → 置信度加权汇聚。
// V2 优化 (参考 DRA + iMAD + Agent Drift 防退化):
//   - 智能角色路由: 根据任务复杂度推荐最少角色子集
//   - 逐层检查点: 每层完成后持久化，支持恢复
//   - 子任务质量门控: 关键子任务增加评审 + 1次重试
//   - 置信度加权 merge: 低质量子结果降权
func (s *SwarmOrchestrator) Execute(ctx context.Context, team *ProductionTeam, objective string) ([]StageResult, error) {
	s.notify(s.chatID, "🐝 **蜂群模式启动** (V2: 智能角色路由+质量门控)")

	// Phase 0: 尝试从检查点恢复
	cp := s.loadCheckpoint(team.Name)
	var plan *DecompositionPlan
	var resultMap map[string]string
	startLevel := 0

	if cp != nil && cp.Plan != nil && len(cp.CompletedIDs) > 0 {
		plan = cp.Plan
		resultMap = cp.ResultMap
		startLevel = cp.CurrentLevel
		s.notify(s.chatID, fmt.Sprintf("♻️ 从检查点恢复: 已完成 %d 个子任务, 从第 %d 层继续",
			len(cp.CompletedIDs), startLevel+1))
	} else {
		resultMap = make(map[string]string)
	}

	// Phase 1: 智能角色路由 (DRA: Dynamic Role Assignment)
	if plan == nil {
		profile := s.routeRoles(ctx, objective)
		s.notify(s.chatID, fmt.Sprintf("🎯 角色路由: 复杂度=%d/3, 建议角色=%d个 [%s]",
			profile.Complexity, len(profile.Roles), strings.Join(profile.Roles, ", ")))

		// 使用路由结果限制 decompose 的角色范围和子任务数
		effectiveMax := profile.SuggestedMax
		if effectiveMax > s.maxAgents {
			effectiveMax = s.maxAgents
		}
		origMax := s.maxAgents
		s.maxAgents = effectiveMax

		var err error
		plan, err = s.decomposeWithRoles(ctx, objective, profile.Roles)
		s.maxAgents = origMax
		if err != nil {
			return nil, fmt.Errorf("任务分解失败: %w", err)
		}
		if len(plan.SubTasks) == 0 {
			return nil, fmt.Errorf("LLM 未能拆解出子任务")
		}
	}

	if team.Blackboard != nil {
		planJSON, _ := json.Marshal(plan)
		team.Blackboard.Write("swarm-plan", string(planJSON), "orchestrator", "context")
		team.Blackboard.Write("swarm-strategy", plan.Strategy, "orchestrator", "context")
	}

	s.notify(s.chatID, fmt.Sprintf("📋 拆解为 **%d** 个子任务 (策略: %s)\n%s",
		len(plan.SubTasks), plan.Strategy, s.formatPlan(plan)))

	levels, err := s.topologicalLevels(plan.SubTasks)
	if err != nil {
		return nil, fmt.Errorf("拓扑排序失败: %w", err)
	}

	var allResults []StageResult
	var prevFinishRate float64 = 1.0
	scores := make(map[string]*SubTaskScore) // 子任务质量追踪

	for levelIdx, level := range levels {
		if levelIdx < startLevel {
			continue
		}
		if ctx.Err() != nil {
			s.saveCheckpoint(team.Name, plan, resultMap, levelIdx)
			return allResults, ctx.Err()
		}

		// PARL 动态并行度
		effectiveLevel := level
		if prevFinishRate < 0.5 && len(level) > 1 {
			half := (len(level) + 1) / 2
			effectiveLevel = level[:half]
			s.notify(s.chatID, fmt.Sprintf("📉 前层完成率 %.0f%%, 降低并行度: %d → %d",
				prevFinishRate*100, len(level), len(effectiveLevel)))
		}

		s.notify(s.chatID, fmt.Sprintf("🔄 执行第 %d/%d 层 (%d 个子任务并行)...",
			levelIdx+1, len(levels), len(effectiveLevel)))

		levelResults := s.executeLevel(ctx, effectiveLevel, objective, resultMap, team)

		// Phase 2: 子任务质量门控 (iMAD 思路: 按优先级选择性评审)
		for i, sr := range levelResults {
			task := effectiveLevel[i]
			sc := s.qualityGate(ctx, sr, task)
			scores[task.ID] = sc

			// 关键子任务失败时重试一次 (DoVer: 假设-干预-验证)
			if sc.Degraded && sc.RetryCount == 0 && task.Priority >= 1 {
				s.notify(s.chatID, fmt.Sprintf("🔄 子任务 **%s** 质量不达标 (置信度=%.2f), 重试中...", task.ID, sc.Confidence))
				retryResult := s.retrySubTask(ctx, task, objective, resultMap, team, sr.Error)
				retrySc := s.qualityGate(ctx, retryResult, task)
				retrySc.RetryCount = 1
				if retrySc.Confidence > sc.Confidence {
					levelResults[i] = retryResult
					scores[task.ID] = retrySc
				}
			}
		}

		completed, valid := 0, 0
		for _, sr := range levelResults {
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				completed++
				resultMap[sr.Name] = sr.Output
				if sr.Output != "" && len(sr.Output) > 50 {
					valid++
				}
			} else {
				s.notify(s.chatID, fmt.Sprintf("⚠️ 子任务 **%s** 失败: %s", sr.Name, sr.Error))
			}
		}
		if len(levelResults) > 0 {
			prevFinishRate = float64(completed) / float64(len(levelResults))
		}
		if team.Blackboard != nil {
			team.Blackboard.Write(fmt.Sprintf("swarm-level%d-metrics", levelIdx),
				fmt.Sprintf("完成=%d/%d 有效=%d 完成率=%.0f%%",
					completed, len(levelResults), valid, prevFinishRate*100),
				"orchestrator", "metric")
		}

		// 补充执行剩余任务
		if len(effectiveLevel) < len(level) {
			remaining := level[len(effectiveLevel):]
			s.notify(s.chatID, fmt.Sprintf("🔄 补充执行剩余 %d 个子任务...", len(remaining)))
			extraResults := s.executeLevel(ctx, remaining, objective, resultMap, team)
			for _, sr := range extraResults {
				allResults = append(allResults, sr)
				if sr.Status == TaskCompleted {
					resultMap[sr.Name] = sr.Output
				}
			}
		}

		// 每层完成后保存检查点
		s.saveCheckpoint(team.Name, plan, resultMap, levelIdx+1)
	}

	// Phase 3: 置信度加权汇聚 (Consensus Merge)
	successCount := 0
	for _, r := range allResults {
		if r.Status == TaskCompleted {
			successCount++
		}
	}
	if successCount > 0 && s.llm != nil {
		s.notify(s.chatID, "📊 正在汇总所有子任务结果 (置信度加权)...")
		merged, err := s.confidenceMerge(ctx, allResults, scores, objective)
		if err == nil && merged != "" {
			allResults = append(allResults, StageResult{
				Name: "swarm-synthesis", Role: "synthesizer",
				Status: TaskCompleted, Output: merged, StartedAt: time.Now(),
			})
			if team.Blackboard != nil {
				team.Blackboard.Write("swarm-final-result", merged, "orchestrator", "result")
			}
		}
	}

	// 清理检查点
	s.clearCheckpoint(team.Name)

	return allResults, nil
}

// RouteRoles 智能角色路由 (公开, 供测试使用)。
func (s *SwarmOrchestrator) RouteRoles(objective string) ComplexityProfile {
	return s.routeRoles(nil, objective)
}

// QualityGate 子任务质量门控 (公开, 供测试使用)。
func (s *SwarmOrchestrator) QualityGate(sr StageResult, task SubTask) *SubTaskScore {
	return s.qualityGate(nil, sr, task)
}

// SetStateDir 设置检查点目录 (公开, 供测试使用)。
func (s *SwarmOrchestrator) SetStateDir(dir string) { s.stateDir = dir }

// SaveCheckpoint 公开检查点保存 (供测试使用)。
func (s *SwarmOrchestrator) SaveCheckpoint(teamName string, plan *DecompositionPlan, resultMap map[string]string, level int) {
	s.saveCheckpoint(teamName, plan, resultMap, level)
}

// ClearCheckpoint 公开检查点清理 (供测试使用)。
func (s *SwarmOrchestrator) ClearCheckpoint(teamName string) { s.clearCheckpoint(teamName) }

// EstimateDebateDivergence 公开辩论分歧度估算 (供测试使用)。
func EstimateDebateDivergence(proposer, opponent string) float64 {
	return estimateDebateDivergence(proposer, opponent)
}

// ComputeConfidence 公开置信度计算 (供测试使用)。
func ComputeConfidence(output, role string) float64 {
	return computeConfidence(output, role)
}

// routeRoles 智能角色路由: 分析任务复杂度，推荐最小角色子集。
// 参考 DRA (arXiv:2601.17152) + Kimi K2.5 PARL 按需实例化。
func (s *SwarmOrchestrator) routeRoles(_ context.Context, objective string) ComplexityProfile {
	obj := strings.ToLower(objective)
	p := ComplexityProfile{DomainBreadth: 1, Complexity: 1}

	codeKeywords := []string{"实现", "开发", "编写", "代码", "implement", "develop", "code", "build", "create", "重构", "refactor", "修复", "fix", "bug"}
	researchKeywords := []string{"调研", "研究", "分析", "对比", "research", "analyze", "investigate", "compare", "评估", "evaluate"}
	reviewKeywords := []string{"审查", "review", "audit", "检查", "check", "安全", "security", "质量"}
	testKeywords := []string{"测试", "test", "验证", "verify", "benchmark"}
	designKeywords := []string{"设计", "架构", "design", "architect", "方案", "plan", "规划"}

	for _, kw := range codeKeywords {
		if strings.Contains(obj, kw) {
			p.NeedsCoding = true
			break
		}
	}
	for _, kw := range researchKeywords {
		if strings.Contains(obj, kw) {
			p.NeedsResearch = true
			break
		}
	}
	for _, kw := range reviewKeywords {
		if strings.Contains(obj, kw) {
			p.NeedsReview = true
			break
		}
	}
	for _, kw := range testKeywords {
		if strings.Contains(obj, kw) {
			p.NeedsTesting = true
			break
		}
	}
	for _, kw := range designKeywords {
		if strings.Contains(obj, kw) {
			p.NeedsDesign = true
			break
		}
	}

	// 领域广度: 根据涉及的能力类型数判断
	capCount := 0
	if p.NeedsCoding {
		capCount++
	}
	if p.NeedsResearch {
		capCount++
	}
	if p.NeedsReview {
		capCount++
	}
	if p.NeedsTesting {
		capCount++
	}
	if p.NeedsDesign {
		capCount++
	}
	switch {
	case capCount >= 4:
		p.DomainBreadth = 3
	case capCount >= 2:
		p.DomainBreadth = 2
	default:
		p.DomainBreadth = 1
	}

	// 整体复杂度: 基于文本长度、能力种类和关键词
	switch {
	case len(objective) > 200 && capCount >= 3:
		p.Complexity = 3
	case len(objective) > 80 || capCount >= 2:
		p.Complexity = 2
	default:
		p.Complexity = 1
	}

	// 推荐角色
	if p.NeedsResearch || (!p.NeedsCoding && !p.NeedsDesign) {
		p.Roles = append(p.Roles, "researcher")
	}
	if p.NeedsDesign {
		p.Roles = append(p.Roles, "architect")
	}
	if p.NeedsCoding {
		lang := detectLanguage(obj)
		if lang != "" {
			p.Roles = append(p.Roles, lang+"-coder")
		} else {
			p.Roles = append(p.Roles, "coder")
		}
	}
	if p.NeedsTesting {
		p.Roles = append(p.Roles, "tester")
	}
	if p.NeedsReview {
		p.Roles = append(p.Roles, "reviewer")
	}
	p.Roles = append(p.Roles, "analyst") // 总是保留综合分析角色

	// 建议最大子任务数 (防过度分解)
	switch p.Complexity {
	case 1:
		p.SuggestedMax = 3
	case 2:
		p.SuggestedMax = 5
	default:
		p.SuggestedMax = 7
	}

	return p
}

// detectLanguage 从目标文本中检测编程语言。
func detectLanguage(obj string) string {
	langMap := map[string]string{
		"go ": "go", "golang": "go", "go语言": "go",
		"typescript": "typescript", "ts ": "typescript",
		"python": "python", "django": "django",
		"dotnet": "dotnet", ".net": "dotnet", "c#": "dotnet",
		"c++": "cpp", "cpp": "cpp",
	}
	for kw, lang := range langMap {
		if strings.Contains(obj, kw) {
			return lang
		}
	}
	return ""
}

// decomposeWithRoles 使用 LLM 动态分解任务 (角色受限版)。
func (s *SwarmOrchestrator) decomposeWithRoles(ctx context.Context, objective string, roles []string) (*DecompositionPlan, error) {
	if s.llm == nil {
		return s.fallbackDecompose(objective), nil
	}

	roleList := strings.Join(roles, ", ")
	sysPrompt := fmt.Sprintf(`你是任务分解引擎。将复杂任务拆解为可并行执行的子任务。

## 拆解规则
1. 每个子任务应是独立可执行的工作单元
2. 标注依赖关系 (dependsOn): 哪些子任务必须在其之前完成
3. 无依赖的子任务会自动并行执行
4. 最多拆解 %d 个子任务 (严格不超过!)
5. role 只能从以下列表中选择: %s
6. **最少角色原则**: 只使用完成任务真正需要的角色，不要为了凑数而添加不必要的子任务

## 优化规则 (重要!)
7. **最小依赖原则**: 只依赖真正需要的前置任务，不要过度串行化
8. **负载均衡**: 平衡各角色的任务数量和复杂度
   - reviewer 任务不应超过 2 个
9. **量化要求**: 评估/审计类任务的 description 中，明确要求输出量化指标
10. **最终汇总**: 最后一个任务必须是综合汇总，包含量化指标和结论

## priority 字段 (决定质量门控级别)
- 0 = 普通 (基础校验)
- 1 = 高 (快速自评)
- 2 = 关键 (完整评审, 失败可重试)

输出严格JSON (不要解释):
{
  "subTasks": [
    {"id": "t1", "description": "具体任务描述", "role": "researcher", "dependsOn": [], "priority": 0},
    {"id": "t2", "description": "另一个任务", "role": "coder", "dependsOn": ["t1"], "priority": 1}
  ],
  "strategy": "parallel|pipeline|hybrid",
  "rationale": "简述拆解理由"
}`, s.maxAgents, roleList)

	resp, err := s.llm.SimpleComplete(ctx, sysPrompt, objective)
	if err != nil {
		return s.fallbackDecompose(objective), nil
	}

	resp = strings.TrimSpace(resp)
	if idx := strings.Index(resp, "```"); idx >= 0 {
		resp = resp[idx+3:]
		resp = strings.TrimPrefix(resp, "json")
		if end := strings.Index(resp, "```"); end >= 0 {
			resp = resp[:end]
		}
	}
	resp = strings.TrimSpace(resp)

	var plan DecompositionPlan
	if err := json.Unmarshal([]byte(resp), &plan); err != nil {
		return s.fallbackDecompose(objective), nil
	}

	if len(plan.SubTasks) > s.maxAgents {
		plan.SubTasks = plan.SubTasks[:s.maxAgents]
	}

	return &plan, nil
}

// decompose 使用 LLM 动态分解任务 (保留兼容: 不限角色)。
func (s *SwarmOrchestrator) decompose(ctx context.Context, objective string) (*DecompositionPlan, error) {
	allRoles := []string{"researcher", "coder", "reviewer", "tester", "architect", "analyst",
		"go-coder", "go-reviewer", "go-tester",
		"typescript-coder", "typescript-reviewer", "typescript-tester",
		"python-coder", "python-reviewer", "python-tester"}
	return s.decomposeWithRoles(ctx, objective, allRoles)
}

// fallbackDecompose 当 LLM 不可用时的降级拆解。
func (s *SwarmOrchestrator) fallbackDecompose(objective string) *DecompositionPlan {
	return &DecompositionPlan{
		Strategy:  "pipeline",
		Rationale: "LLM 不可用, 使用默认 research→synthesize 流程",
		SubTasks: []SubTask{
			{ID: "t1", Description: "调研和分析: " + objective, Role: "researcher"},
			{ID: "t2", Description: "综合整理研究结果", Role: "analyst", DependsOn: []string{"t1"}},
		},
	}
}

// topologicalLevels 将子任务按依赖拓扑排序, 返回分层结果。
// 同一层内的子任务可以并行执行。
func (s *SwarmOrchestrator) topologicalLevels(tasks []SubTask) ([][]SubTask, error) {
	taskMap := make(map[string]SubTask)
	inDegree := make(map[string]int)
	depGraph := make(map[string][]string)

	for _, t := range tasks {
		taskMap[t.ID] = t
		inDegree[t.ID] = 0
	}

	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if _, exists := taskMap[dep]; exists {
				depGraph[dep] = append(depGraph[dep], t.ID)
				inDegree[t.ID]++
			}
		}
	}

	var levels [][]SubTask
	processed := make(map[string]bool)

	for len(processed) < len(tasks) {
		var level []SubTask
		for _, t := range tasks {
			if processed[t.ID] {
				continue
			}
			if inDegree[t.ID] == 0 {
				level = append(level, t)
			}
		}

		if len(level) == 0 {
			return levels, fmt.Errorf("检测到循环依赖")
		}

		levels = append(levels, level)

		for _, t := range level {
			processed[t.ID] = true
			for _, next := range depGraph[t.ID] {
				inDegree[next]--
			}
		}
	}

	return levels, nil
}

// executeLevel 并行执行同一层的所有子任务。
func (s *SwarmOrchestrator) executeLevel(
	ctx context.Context,
	level []SubTask,
	objective string,
	prevResults map[string]string,
	team *ProductionTeam,
) []StageResult {
	// 自动扩缩池: 根据本层并发任务数调整
	if s.pool != nil {
		s.pool.AutoScale(len(level))
	}

	results := make([]StageResult, len(level))
	var wg sync.WaitGroup

	for i, task := range level {
		wg.Add(1)
		go func(idx int, t SubTask) {
			defer wg.Done()
			results[idx] = s.executeSubTask(ctx, t, objective, prevResults, team)
		}(i, task)
	}

	wg.Wait()
	return results
}

// executeSubTask 执行单个子任务。
func (s *SwarmOrchestrator) executeSubTask(
	ctx context.Context,
	task SubTask,
	objective string,
	prevResults map[string]string,
	team *ProductionTeam,
) StageResult {
	start := time.Now()

	// 构建子任务 prompt (优先从 RoleRegistry 获取角色系统提示词)
	var sb strings.Builder
	rolePromptInjected := false
	if s.roles != nil {
		if rp := s.roles.MergedPrompt(task.Role, objective, ""); rp != "" {
			sb.WriteString(rp)
			sb.WriteString("\n\n")
			rolePromptInjected = true
		}
	}
	if !rolePromptInjected {
		sb.WriteString(fmt.Sprintf("你的角色: %s\n\n", task.Role))
	}
	sb.WriteString(fmt.Sprintf("整体目标: %s\n\n", objective))
	sb.WriteString(fmt.Sprintf("你的具体任务: %s\n\n", task.Description))

	// 注入依赖结果
	if len(task.DependsOn) > 0 {
		sb.WriteString("前置任务的结果:\n")
		for _, dep := range task.DependsOn {
			if r, ok := prevResults[dep]; ok {
				sb.WriteString(fmt.Sprintf("\n--- %s ---\n%s\n", dep, truncateResult(r, 3000)))
			}
		}
		sb.WriteString("\n")
	}

	// 注入黑板上下文 (使用角色感知的智能截断)
	if team.Blackboard != nil {
		snapshot := team.Blackboard.SnapshotForRole(task.Role, 4000)
		if len(snapshot) > 0 {
			sb.WriteString("\n共享知识库:\n")
			sb.WriteString(snapshot)
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n请深入执行你的任务, 给出详细、高质量的结果。使用可用的工具来完成工作。")
	// 蜂群通用质量约束: 防止空壳/编译失败/架构脱节
	sb.WriteString(`

## 质量红线 (所有蜂群子任务必须遵守)
1. 禁止 Mock/Stub: 核心模块必须有真实实现
2. 编译检查: 如果你写了代码, 完成后必须运行 go build/go vet 确认编译通过
3. 架构对齐: 如果有前置任务的设计文档, 必须严格遵循其接口定义和目录结构
4. 测试函数名不允许包含中文符号(如-), 使用下划线分隔
5. 最终产出必须是可运行/可编译的代码, 不是文档或说明
6. 如果是审查/分析任务, 必须输出结构化报告(表格+评分)
`)

	prompt := sb.String()

	// 注入进化经验
	var injectedExpIDs []string
	if s.evolution != nil {
		exps := s.evolution.RetrieveFor(task.Role, task.Description, 3)
		if len(exps) > 0 {
			prompt = FormatExperiencesForPrompt(exps) + "\n" + prompt
			for _, e := range exps {
				injectedExpIDs = append(injectedExpIDs, e.ID)
			}
		}
	}

	// 通过 AgentPool 获取 agent
	if s.pool == nil {
		return StageResult{Name: task.ID, Role: task.Role, Status: TaskFailed, Error: "agent pool 未初始化"}
	}

	agent, err := s.pool.Acquire(ctx, task.Role, "")
	if err != nil {
		return StageResult{Name: task.ID, Role: task.Role, Status: TaskFailed, Error: err.Error(), StartedAt: start}
	}
	defer s.pool.Release(agent)

	// 子任务级别超时保护 (防止单个子任务卡死拖垮整体)
	taskTimeout := 5 * time.Minute
	if task.Priority >= 2 {
		taskTimeout = 10 * time.Minute
	}
	taskCtx, taskCancel := context.WithTimeout(ctx, taskTimeout)
	defer taskCancel()

	// V2 Task 追踪
	var v2ID string
	if s.taskTracker != nil {
		if id, err := s.taskTracker.AddTask(fmt.Sprintf("[swarm] %s", task.ID), task.Description, task.Role); err == nil {
			v2ID = id
			_ = s.taskTracker.SetTaskStatus(id, "in_progress")
		}
	}

	// 更新 team agent 状态
	if team != nil {
		team.mu.Lock()
		team.Agents[task.ID] = &BGAgent{Name: task.ID, Role: task.Role, Status: AgentStatusRunning}
		team.mu.Unlock()
	}

	result, execErr := agent.Runner.Execute(taskCtx, prompt)
	duration := time.Since(start)

	// 更新状态
	if team != nil {
		team.mu.Lock()
		if ag, ok := team.Agents[task.ID]; ok {
			if execErr != nil {
				ag.Status = AgentStatusFailed
				ag.Error = execErr.Error()
			} else {
				ag.Status = AgentStatusCompleted
				ag.Result = truncateResult(result, 1000)
			}
		}
		team.mu.Unlock()
	}

	// 写入黑板
	if team.Blackboard != nil {
		if execErr == nil {
			team.Blackboard.Write(task.ID+"-result", result, task.Role, "result")
		} else {
			team.Blackboard.Write(task.ID+"-error", execErr.Error(), task.Role, "result")
		}
	}

	// 更新 V2 Task
	if s.taskTracker != nil && v2ID != "" {
		if execErr != nil {
			_ = s.taskTracker.SetTaskStatus(v2ID, "failed")
		} else {
			_ = s.taskTracker.SetTaskStatus(v2ID, "completed")
		}
	}

	// 记录轨迹 + 经验反馈 + 增量学习
	if s.evolution != nil {
		traj := Trajectory{
			TeamName:  team.Name,
			StageName: task.ID,
			Role:      task.Role,
			Objective: task.Description,
			Input:     prompt,
			Output:    result,
			Error: func() string {
				if execErr != nil {
					return execErr.Error()
				}
				return ""
			}(),
			Success:   execErr == nil,
			Duration:  duration.Round(time.Second).String(),
			Timestamp: time.Now(),
		}
		s.evolution.RecordTrajectory(traj)
		s.evolution.LearnFromStage(traj)
		if len(injectedExpIDs) > 0 {
			s.evolution.RecordBatchFeedback(injectedExpIDs, execErr == nil)
		}
	}

	if execErr != nil {
		return StageResult{
			Name: task.ID, Role: task.Role, Status: TaskFailed,
			Error: execErr.Error(), StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	// 蜂群输出验证: 与工作流路径一致, 防止 Agent 空转
	if reason := validateAgentOutput(result, task.Role); reason != "" {
		return StageResult{
			Name: task.ID, Role: task.Role, Status: TaskFailed,
			Error:     fmt.Sprintf("产出验证失败: %s", reason),
			Output:    result,
			StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	// 防伪并行检查 (参考 Kimi K2.5 PARL: 要求非平凡 artifact)
	if !IsNonTrivialArtifact(result, task.Role) {
		s.notify(s.chatID, fmt.Sprintf("⚠️ 子任务 %s 产出可能是伪并行 (无实质 artifact)", task.ID))
	}

	return StageResult{
		Name: task.ID, Role: task.Role, Status: TaskCompleted,
		Output: result, V2TaskID: v2ID, StartedAt: start,
		Duration: duration.Round(time.Second).String(),
	}
}

// qualityGate 子任务质量门控 (参考 iMAD: 按优先级选择性评审)。
func (s *SwarmOrchestrator) qualityGate(_ context.Context, sr StageResult, task SubTask) *SubTaskScore {
	sc := &SubTaskScore{TaskID: task.ID}

	if sr.Status != TaskCompleted || sr.Output == "" {
		sc.Confidence = 0
		sc.Degraded = true
		return sc
	}

	output := sr.Output
	outLen := len(output)

	// 基础置信度 (所有优先级都评估)
	conf := 0.5
	if outLen > 200 {
		conf += 0.1
	}
	if outLen > 500 {
		conf += 0.1
	}
	if IsNonTrivialArtifact(output, task.Role) {
		conf += 0.2
	}

	hasStructure := strings.Contains(output, "##") || strings.Contains(output, "```") || strings.Contains(output, "|")
	if hasStructure {
		conf += 0.1
	}

	if conf > 1 {
		conf = 1
	}
	sc.Confidence = conf
	sc.Degraded = conf < 0.4

	return sc
}

// retrySubTask 对失败/低质量子任务重试一次 (DoVer: 假设-干预-验证)。
func (s *SwarmOrchestrator) retrySubTask(ctx context.Context, task SubTask, objective string, prevResults map[string]string, team *ProductionTeam, failReason string) StageResult {
	hint := fmt.Sprintf("\n\n⚠️ 前次执行质量不足: %s\n请更加深入和详细地执行任务, 确保输出有实质内容。", failReason)
	task.Description = task.Description + hint
	return s.executeSubTask(ctx, task, objective, prevResults, team)
}

// confidenceMerge 置信度加权汇聚: 低质量子结果降权标注。
func (s *SwarmOrchestrator) confidenceMerge(ctx context.Context, results []StageResult, scores map[string]*SubTaskScore, objective string) (string, error) {
	if s.llm == nil {
		return s.fallbackMerge(results), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("目标: %s\n\n各子任务结果:\n", objective))

	for _, r := range results {
		if r.Status != TaskCompleted || r.Output == "" {
			continue
		}
		confidenceTag := ""
		if sc, ok := scores[r.Name]; ok {
			if sc.Degraded {
				confidenceTag = " ⚠️ [低置信度 — 仅供参考，不要作为主要论据]"
			} else if sc.Confidence >= 0.8 {
				confidenceTag = " ✅ [高置信度]"
			}
		}
		sb.WriteString(fmt.Sprintf("\n### [%s] %s%s\n%s\n", r.Role, r.Name, confidenceTag, truncateResult(r.Output, 2000)))
	}

	sysPrompt := `你是结果综合专家 (参考 Kimi K2.5 反思聚合 + 置信度加权)。将多个子任务的结果汇总为连贯完整的最终报告。

## 置信度处理 (重要!)
- 标注 ⚠️ [低置信度] 的子任务结果可能不完整或质量不足, 综合时降低其权重
- 标注 ✅ [高置信度] 的结果优先引用
- 如果某个关键结论仅来自低置信度源, 标注 "需人工验证"

## 报告结构 (必须包含)
1. **执行摘要** — 3-5 条关键发现
2. **各任务产出综合** — 提炼核心发现，去除冗余
3. **量化指标汇总表** (如果子任务有量化数据)
4. **跨 worker 共识裁决**: 不同 worker 给出不同结论时，基于置信度和证据强度裁决
5. **矛盾点分析** — 数据分歧和结论矛盾
6. **结论与建议** — 分优先级的可操作建议
7. **附录** — 耗时统计 + 产出有效性评估 + 置信度总结`

	return s.llm.SimpleComplete(ctx, sysPrompt, sb.String())
}

// merge 使用 LLM 综合所有子任务结果 (兼容旧接口)。
func (s *SwarmOrchestrator) merge(ctx context.Context, results []StageResult, objective string) (string, error) {
	return s.confidenceMerge(ctx, results, nil, objective)
}

// --- 检查点持久化 ---

func (s *SwarmOrchestrator) checkpointPath(teamName string) string {
	dir := s.stateDir
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude-go", "swarm_checkpoints")
	}
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, fmt.Sprintf("swarm_%s.json", teamName))
}

func (s *SwarmOrchestrator) saveCheckpoint(teamName string, plan *DecompositionPlan, resultMap map[string]string, level int) {
	cp := SwarmCheckpoint{
		Plan:        plan,
		ResultMap:   resultMap,
		CurrentLevel: level,
		Timestamp:   time.Now(),
	}
	for id := range resultMap {
		cp.CompletedIDs = append(cp.CompletedIDs, id)
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return
	}
	if err := os.WriteFile(s.checkpointPath(teamName), data, 0o644); err != nil {
		log.Printf("[Swarm] 检查点保存失败: %v", err)
	}
}

func (s *SwarmOrchestrator) loadCheckpoint(teamName string) *SwarmCheckpoint {
	data, err := os.ReadFile(s.checkpointPath(teamName))
	if err != nil {
		return nil
	}
	var cp SwarmCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil
	}
	if time.Since(cp.Timestamp) > 2*time.Hour {
		return nil
	}
	return &cp
}

func (s *SwarmOrchestrator) clearCheckpoint(teamName string) {
	_ = os.Remove(s.checkpointPath(teamName))
}

// computeConfidence 计算输出置信度 (不依赖 LLM 的快速评估)。
func computeConfidence(output, role string) float64 {
	if output == "" {
		return 0
	}
	conf := 0.3
	outLen := float64(len(output))

	// 长度分: 200字以上开始有信心, 递增到2000字封顶
	lengthScore := math.Min(outLen/2000.0, 1.0) * 0.3
	conf += lengthScore

	// 结构分: 有标题、代码块、表格加分
	if strings.Contains(output, "##") {
		conf += 0.1
	}
	if strings.Contains(output, "```") {
		conf += 0.1
	}
	if strings.Contains(output, "|") && strings.Count(output, "|") > 4 {
		conf += 0.05
	}
	if IsNonTrivialArtifact(output, role) {
		conf += 0.15
	}

	if conf > 1 {
		conf = 1
	}
	return conf
}

func (s *SwarmOrchestrator) fallbackMerge(results []StageResult) string {
	var sb strings.Builder
	sb.WriteString("# 蜂群执行结果汇总\n\n")
	for _, r := range results {
		if r.Status == TaskCompleted && r.Output != "" {
			sb.WriteString(fmt.Sprintf("## %s (%s)\n%s\n\n", r.Name, r.Role, r.Output))
		}
	}
	return sb.String()
}

func (s *SwarmOrchestrator) formatPlan(plan *DecompositionPlan) string {
	var sb strings.Builder
	for i, t := range plan.SubTasks {
		deps := ""
		if len(t.DependsOn) > 0 {
			deps = fmt.Sprintf(" (依赖: %s)", strings.Join(t.DependsOn, ", "))
		}
		sb.WriteString(fmt.Sprintf("  %d. [%s] %s%s\n", i+1, t.Role, t.Description, deps))
	}
	return sb.String()
}

// isNonTrivialArtifact 检查产出是否为非平凡 artifact (参考 Kimi K2.5 PARL 防伪并行)。
// 防止 worker 产出仅是声明性文本而无实质内容。
func IsNonTrivialArtifact(output, role string) bool {
	if len(output) < 100 {
		return false
	}
	hasCoder := strings.Contains(role, "coder") || strings.Contains(role, "implement")
	if hasCoder {
		return strings.Contains(output, "func ") || strings.Contains(output, "package ") ||
			strings.Contains(output, "import ") || strings.Contains(output, "class ") ||
			strings.Contains(output, "def ") || strings.Contains(output, "const ")
	}
	hasReviewer := strings.Contains(role, "review")
	if hasReviewer {
		return strings.Contains(output, "PASS") || strings.Contains(output, "FAIL") ||
			strings.Contains(output, "评分") || strings.Contains(output, "score") ||
			strings.Contains(output, "|") // 表格
	}
	hasTester := strings.Contains(role, "test")
	if hasTester {
		return strings.Contains(output, "Test") || strings.Contains(output, "test") ||
			strings.Contains(output, "assert") || strings.Contains(output, "PASS")
	}
	return len(output) > 200
}
