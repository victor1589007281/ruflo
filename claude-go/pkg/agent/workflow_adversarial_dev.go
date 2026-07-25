package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
)

// executeAdversarialDev 对抗式开发流水线。
// 四阶段模型 (拆分为可读的小函数):
//
//	Phase 1: 设计阶段 (research → design → plan)
//	Phase 2: [Generator ↔ Evaluator] 对抗循环
//	Phase 3: E2E 对抗测试 (tester↔coder 自适应)
//	Phase 4: 非测试的收尾阶段
func (we *WorkflowExecutor) executeAdversarialDev(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	designStages, _, _, parallelStages := classifyStages(wf.Stages)
	we.tryInitDAG()

	// 恢复检查点: 如果有已完成的阶段, 跳过并注入 prevResults
	we.restoreCheckpoints(wf.Stages, prevResults, &allResults)

	// Phase 1: 设计阶段
	designResults, err := we.runDesignPhase(ctx, designStages, objective, prevResults, team)
	allResults = append(allResults, designResults...)
	we.savePhaseCheckpoints(designResults)
	we.flushStagesLive(team, allResults)
	if err != nil {
		return allResults, err
	}

	// Phase 2: Orchestrator (DAG 驱动, pool 由 DAG 宽度精确控制)
	// 修复: 禁止 silent fallback 到对抗循环。Orchestrator 是 Plan→Execute 的唯一通道。
	orchUsed := false
	if planOutput, hasPlan := prevResults["plan"]; hasPlan && we.dagTracker != nil {
		orchResults, orchErr := we.runOrchestratedPhase(ctx, planOutput, objective, prevResults, team, allResults)
		if orchErr == nil && len(orchResults) > 0 {
			allResults = append(allResults, orchResults...)
			we.savePhaseCheckpoints(orchResults)
			we.flushStagesLive(team, allResults)
			orchUsed = true
		} else if orchErr != nil {
			we.notify(we.chatID, fmt.Sprintf("🔴 Orchestrator 启动失败, 终止工作流 (不再 fallback 到对抗循环): %v", orchErr))
			return allResults, fmt.Errorf("orchestrator 启动失败: %w", orchErr)
		}
	}

	if !orchUsed {
		we.notify(we.chatID, "🔴 未找到有效 Plan 或 DAG 未配置, 终止工作流")
		return allResults, fmt.Errorf("plan 缺失或 DAG 未配置, 无法启动 Orchestrator")
	}

	// Phase 3: E2E 对抗测试
	e2eResults := we.runE2EAdversarial(ctx, parallelStages, objective, prevResults, team)
	allResults = append(allResults, e2eResults...)
	we.savePhaseCheckpoints(e2eResults)
	we.flushStagesLive(team, allResults)

	// E2E 是质量门禁, 失败则终止工作流
	for _, er := range e2eResults {
		if er.Status == TaskFailed {
			return allResults, fmt.Errorf("E2E 测试失败: %s — %s", er.Name, er.Error)
		}
	}

	// Phase 4: 收尾阶段
	finishResults := we.runFinishPhase(ctx, parallelStages, objective, prevResults, team)
	allResults = append(allResults, finishResults...)
	we.flushStagesLive(team, allResults)
	we.savePhaseCheckpoints(finishResults)

	return allResults, nil
}

// runOrchestratedPhase 使用 Orchestrator + V2 DAG 执行开发任务。
// 当 Planner 输出了 WBS 表格时, Orchestrator 解析并通过 V2 TaskStore 调度。
// priorStages: Phase 1 等已完成的阶段, 增量刷新时会与 Orchestrator 结果合并。
func (we *WorkflowExecutor) runOrchestratedPhase(ctx context.Context, planOutput, objective string, prevResults map[string]string, team *ProductionTeam, priorStages []StageResult) ([]StageResult, error) {
	orchParallel := 3
	if we.concurrency != nil {
		suggested := we.concurrency.SuggestConcurrency()
		if suggested > 1 {
			orchParallel = suggested
		}
	}
	// 包装 factory: 注入 ModelConfigKey, 使 Orchestrator 阶段也能按 plan+role 解析模型/API 配置
	baseFactory := we.factory
	planFactory := func(pCtx context.Context, role, systemPrompt string) (AgentRunner, error) {
		if we.planCfgResolver != nil {
			pCtx = context.WithValue(pCtx, ModelConfigKey{}, we.planCfgResolver.Resolve(team.Workflow, role))
		}
		pCtx = WithRunMetadata(pCtx, RunMetadata{
			Source:   "team_stage",
			Purpose:  team.Name,
			Workflow: team.Workflow,
			Role:     role,
			Team:     team.Name,
		})
		return baseFactory(pCtx, role, systemPrompt)
	}

	orch := NewOrchestrator(
		OrchestratorConfig{MaxParallel: orchParallel, MaxRetries: 2, MicroTestAfter: true, AdversarialRound: 3},
		we.dagTracker, planFactory, we.notify, we.pool, we.chatID,
	)

	if we.activityCallback != nil {
		orch.SetActivityCallback(we.activityCallback)
	}
	if we.progressCallback != nil {
		orch.SetProgressCallback(we.progressCallback)
	}
	if we.checkpoints != nil {
		orch.SetCheckpointStore(we.checkpoints)
	}
	if designDoc, ok := prevResults["design"]; ok {
		orch.SetDesignContext(designDoc, planOutput)
	}
	if team != nil {
		orch.SetPlanningLanguage(team.Language)
	}

	// 增量刷新: Orchestrator 每批任务完成后把 Phase 1 结果 + Orchestrator 结果合并写入 team.json
	prefix := make([]StageResult, len(priorStages))
	copy(prefix, priorStages)
	orch.SetStageFlusher(func(orchResults []StageResult) {
		combined := make([]StageResult, 0, len(prefix)+len(orchResults))
		combined = append(combined, prefix...)
		combined = append(combined, orchResults...)
		we.flushStagesLive(team, combined)
	})

	nodes, err := orch.ParsePlanToDAGWithRepair(ctx, planOutput, objective, team.Name, planFactory)
	if err != nil || len(nodes) == 0 {
		return nil, fmt.Errorf("WBS 解析失败或无任务: %v", err)
	}

	we.notify(we.chatID, fmt.Sprintf("🎯 Orchestrator 接管: %d 个任务 (V2 DAG 驱动, micro-test 启用)", len(nodes)))
	results, err := orch.Execute(ctx, objective, team)

	// 将 Orchestrator 的产出写入 prevResults (供后续 E2E 使用)
	for _, sr := range results {
		if sr.Status == TaskCompleted {
			prevResults[sr.Name] = sr.Output
		}
	}

	// 写入 micro-test 汇总到 blackboard
	if team.Blackboard != nil {
		team.Blackboard.Write("micro-test-summary", orch.MicroTestSummary(), "orchestrator", "summary")
	}

	return results, err
}

// initAdaptiveTerminator 初始化自适应终止器
func (we *WorkflowExecutor) initAdaptiveTerminator(wf *WorkflowDef) (maxRounds int, terminator *AdaptiveTerminator) {
	maxRounds = wf.Rounds
	useAdaptive := maxRounds <= 0
	if useAdaptive {
		maxRounds = 5
	}
	if useAdaptive {
		terminator = NewAdaptiveTerminator(2, maxRounds)
	}
	return
}

// autoScalePool 动态扩缩 Agent Pool
func (we *WorkflowExecutor) autoScalePool(wf *WorkflowDef, design, generators []StageDef, eval *StageDef, parallel []StageDef, maxRounds int) {
	if we.pool == nil {
		return
	}
	roleNeeds := make(map[string]int)
	for _, s := range design {
		roleNeeds[s.Role]++
	}
	for _, s := range generators {
		roleNeeds[s.Role] += maxRounds
	}
	if eval != nil {
		roleNeeds[eval.Role] += maxRounds
	}
	for _, s := range parallel {
		roleNeeds[s.Role]++
	}
	complexity := 0
	if maxRounds >= 3 {
		complexity = 1
	}
	if len(wf.Stages) > 5 || maxRounds >= 5 {
		complexity = 2
	}
	we.pool.AutoScaleByRoles(roleNeeds, complexity)
}

// runDesignPhase Phase 1: 串行执行设计阶段
func (we *WorkflowExecutor) runDesignPhase(ctx context.Context, designStages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) ([]StageResult, error) {
	if len(designStages) == 0 {
		return nil, nil
	}
	var results []StageResult
	we.notify(we.chatID, fmt.Sprintf("📐 Phase 1: 设计/策划 (%d 阶段)...", len(designStages)))
	for _, ds := range designStages {
		// 检查点恢复: 如果该阶段已有 completed checkpoint, 跳过
		if _, restored := prevResults[ds.Name]; restored {
			we.notify(we.chatID, fmt.Sprintf("  ♻️ %s 已从检查点恢复, 跳过", ds.Name))
			continue
		}

		sr := we.executeStage(ctx, ds, objective, prevResults, team)
		results = append(results, sr)
		if sr.Status != TaskCompleted {
			return results, fmt.Errorf("设计阶段 %s 失败: %s", ds.Name, sr.Error)
		}
		prevResults[ds.Name] = sr.Output
	}
	return results, nil
}

// maxBuildRetries 编译硬门禁内部重试次数 (L2, 参考 Self-Debugging arXiv:2304.05128)。
// 编译修复循环独立于对抗循环, 不消耗 AdaptiveTerminator 的轮次配额。
const maxBuildRetries = 4

// maxTestRetries 测试硬门禁内部重试次数 (L2.5)。
const maxTestRetries = 2

// runAdversarialLoop Phase 2: Generator ↔ Evaluator 对抗循环。
// 六层质量保障: L2 编译硬门禁 + L5 上下文压缩 + L6 重采样决策。
func (we *WorkflowExecutor) runAdversarialLoop(
	ctx context.Context,
	generatorStages []StageDef, evalStage *StageDef,
	maxRounds int, terminator *AdaptiveTerminator,
	objective string, prevResults map[string]string, team *ProductionTeam,
) ([]StageResult, error) {
	if len(generatorStages) == 0 {
		we.notify(we.chatID, "⚠️ 未发现 Generator 阶段，跳过对抗循环")
		return nil, nil
	}

	var allResults []StageResult
	we.notify(we.chatID, fmt.Sprintf("⚔️ Phase 2: 对抗循环 (最多 %d 轮, 含编译硬门禁+重采样)...", maxRounds))
	var lastGenOutput, lastEvalFeedback string
	var lastScore EvalScore

	for round := 1; round <= maxRounds; round++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// L6: 重采样决策 (参考 arXiv:2604.10508)
		// 连续 2 轮 Completeness<5 且编译失败 → 清空上轮输出, 换思路重新生成
		if terminator != nil && round > 2 && terminator.ShouldResample() {
			we.notify(we.chatID, fmt.Sprintf("🔄 第 %d 轮触发重采样 (连续低完整度+编译失败, 换思路)", round))
			lastGenOutput = ""
			lastEvalFeedback = "⚠️ **重采样模式**: 前几轮的实现方式无法产出完整代码。请换一种思路:\n" +
				"1. 先实现最核心的入口文件和 1 个核心模块, 确保可编译\n" +
				"2. 每个文件写完后心理验证编译正确性\n" +
				"3. 宁可功能不全但能编译, 也不要输出不可编译的完整框架\n" +
				"4. 优先保证: 编译通过 > 功能完整 > 代码优雅"
		}

		// L5: 注入迭代记忆链 (参考 Reflexion arXiv:2303.11366)
		memoryHint := ""
		if terminator != nil && len(terminator.Memories) > 0 {
			memoryHint = FormatMemoryChain(terminator.Memories)
		}

		// Generator 执行
		genResults, genOutput := we.runGeneratorRound(ctx, generatorStages, round, maxRounds, lastGenOutput, lastEvalFeedback+"\n"+memoryHint, objective, prevResults, team)
		allResults = append(allResults, genResults...)
		if len(genResults) > 0 && genResults[len(genResults)-1].Status != TaskCompleted {
			return allResults, fmt.Errorf("generator 第 %d 轮失败", round)
		}
		lastGenOutput = genOutput

		// L2: 编译硬门禁 — 编译失败时内部重试, 不消耗对抗轮次
		buildPassed := we.runBuildHardGate(ctx, generatorStages, round, maxRounds, &lastGenOutput, objective, prevResults, team, &allResults)
		if terminator != nil {
			terminator.RecordBuildResult(buildPassed)
		}
		if !buildPassed {
			we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮编译硬门禁未通过 (含 %d 次内部重试), 跳过 Reviewer", round, maxBuildRetries))
			// 编译未通过时, 为 reviewer 构造低分 (避免让 reviewer 审查不可编译的代码)
			lastScore = EvalScore{
				Correctness: 3, Completeness: 3, Security: 5, CodeQuality: 4,
				Pass: false, Feedback: "编译未通过, 跳过 Reviewer 评审",
			}
			if terminator != nil {
				terminator.RecordRoundOutput(round, lastScore, lastGenOutput)
				terminator.RecordIterationMemory(round, lastScore, extractFileList(lastGenOutput), []string{"编译未通过"}, false)
				decision := terminator.ShouldTerminate(round, lastScore)
				if decision.ShouldStop {
					we.notify(we.chatID, fmt.Sprintf("🏁 自适应终止 (编译持续失败, 原因: %s)", decision.Reason))
					if decision.BestOutput != "" {
						lastGenOutput = decision.BestOutput
						prevResults[generatorStages[len(generatorStages)-1].Name] = decision.BestOutput
					}
					break
				}
			}
			lastEvalFeedback = "编译未通过, 必须优先修复编译错误。"
			continue
		}

		// 编译通过后, 清除之前的编译错误反馈
		lastEvalFeedback = ""

		// Evaluator 审查 (只有编译通过才进入)
		evalResult, shouldBreak, bestOutput := we.runEvaluatorRound(ctx, evalStage, generatorStages, round, maxRounds, lastGenOutput, terminator, objective, prevResults, team, &lastEvalFeedback, lastScore)
		allResults = append(allResults, evalResult...)
		if bestOutput != "" {
			lastGenOutput = bestOutput
			prevResults[generatorStages[len(generatorStages)-1].Name] = bestOutput
		}

		// 更新 lastScore
		kept := true
		if len(evalResult) > 0 {
			if parsed, err := ParseEvalScoreJSON([]byte(evalResult[len(evalResult)-1].Output)); err == nil {
				lastScore = parsed
			} else if terminator != nil && len(terminator.ScoreHistory) > 0 {
				lastScore = HoldLastOrDefault(lastScore)
			}
		}

		// L5: 记录迭代记忆
		if terminator != nil {
			issues := ExtractKeyIssues(lastScore.Feedback)
			// 即时 Keep/Revert
			if !shouldBreak && round > 1 {
				if revert, bo, br := terminator.ShouldRevert(lastScore); revert {
					we.notify(we.chatID, fmt.Sprintf("⏪ 第 %d 轮退化, revert 到第 %d 轮最佳版本", round, br))
					lastGenOutput = bo
					prevResults[generatorStages[len(generatorStages)-1].Name] = bo
					kept = false
				}
			}
			terminator.RecordIterationMemory(round, lastScore, extractFileList(lastGenOutput), issues, kept)
		}

		if shouldBreak {
			break
		}
	}
	return allResults, nil
}

// runBuildHardGate L2 编译硬门禁: 编译失败时驱动 Coder 内部重试。
// 返回 true 表示编译通过。内部重试最多 maxBuildRetries 次, 不消耗对抗轮次。
func (we *WorkflowExecutor) runBuildHardGate(
	ctx context.Context, genStages []StageDef,
	round, maxRounds int, lastGenOutput *string,
	objective string, prevResults map[string]string, team *ProductionTeam,
	allResults *[]StageResult,
) bool {
	if team.Cwd == "" {
		return true
	}

	// L4: 文件物化 — 从 Coder 输出提取代码写入磁盘
	lang := team.Language
	if lang == "" {
		lang = "go"
	}
	if *lastGenOutput != "" {
		written := MaterializeCode(team.Cwd, *lastGenOutput, lang)
		if len(written) > 0 {
			we.notify(we.chatID, fmt.Sprintf("📁 文件物化: %d 个文件写入磁盘", len(written)))
		}
	}

	buildErrors := runBuildCheckLang(team.Cwd, lang)
	if buildErrors == "" {
		// L1.5: TODO/STUB 确定性门禁 — 编译通过不等于实现完整
		if todos := scanForTodos(team.Cwd); len(todos) > 0 {
			we.notify(we.chatID, fmt.Sprintf("🟡 第 %d 轮: 编译通过但发现 %d 处未实现项, 启动内部修复", round, len(todos)))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			// 进入内部修复循环 (和编译失败同路径)
			buildErrors = fmt.Sprintf("TODO/STUB 检测失败, 发现 %d 处未实现项", len(todos))
		} else {
			 tc := GetToolchain(lang)
			we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮编译通过且无未实现项 (%s)", round, tc.BuildCheckLabel()))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			return true
		}
	}

	we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮编译失败, 启动内部修复 (最多 %d 次)...", round, maxBuildRetries))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}

	guard := NewCompileRepairGuard(3)
	for retry := 1; retry <= maxBuildRetries; retry++ {
		if ctx.Err() != nil {
			return false
		}

		// 区分编译错误和 TODO/STUB 检测
		isTodoFix := strings.Contains(buildErrors, "TODO/STUB")
		var buildFixPrompt string
		if isTodoFix {
			todos := scanForTodos(team.Cwd)
			var buf strings.Builder
			buf.WriteString(fmt.Sprintf("### 未实现项检测 (第 %d 次修复, 必须将所有 TODO/STUB 替换为真实实现):\n", retry))
			for _, t := range todos {
				buf.WriteString(fmt.Sprintf("  - %s\n", t))
			}
			buf.WriteString("\n要求:\n1. 将上述每个 TODO/STUB/placeholder/panic 替换为真实、可运行的实现\n2. 宁可简化功能, 也不允许保留占位符\n3. 保持现有代码结构不变\n4. 输出修复后的完整文件内容")
			buildFixPrompt = buf.String()
		} else {
			buildFixPrompt = fmt.Sprintf("### 编译错误 (第 %d 次修复, 必须优先修复编译错误):\n%s\n\n"+
				"要求:\n1. 仅修复编译错误, 不要做其他改动\n2. 保持现有代码结构不变\n3. 输出修复后的完整文件内容\n%s",
				retry, truncateResult(buildErrors, 3000), ConfidencePromptSuffix)
		}

		for _, genStage := range genStages {
			fixStage := genStage
			fixStage.Prompt = strings.ReplaceAll(fixStage.Prompt, "{adversarial_feedback}", buildFixPrompt)
			fixStage.Name = fmt.Sprintf("%s-round%d-buildfix%d", genStage.Name, round, retry)

			label := map[bool]string{true: "TODO", false: "编译"}[isTodoFix]
			we.notify(we.chatID, fmt.Sprintf("  🔧 %s修复 %d/%d — %s...", label, retry, maxBuildRetries, genStage.Role))
			sr := we.executeStage(ctx, fixStage, objective, prevResults, team)
			sr.Name = fixStage.Name
			*allResults = append(*allResults, sr)
			if sr.Status == TaskCompleted {
				*lastGenOutput = sr.Output
				prevResults[genStage.Name] = sr.Output
				MaterializeCode(team.Cwd, sr.Output, lang)
			} else if isStageTransientError(sr.Error) {
				// V2 改进: build fix 中 coder 因 API 瞬态错误失败时,
				// 提前返回 false, 让对抗循环有机会重试整轮 (而非浪费 build fix 重试次数)
				we.notify(we.chatID, fmt.Sprintf("  🔴 %s修复 %d/%d 因 API 错误失败 (已自动重试), 跳过本轮", label, retry, maxBuildRetries))
				return false
			}
		}

		// L2: 编译检查
		buildErrors = runBuildCheckLang(team.Cwd, lang)
		if buildErrors != "" {
			conf := ParseConfidenceFromOutput(*lastGenOutput)
			round := repairRound{
				retryNum:        retry,
				errorSignatures: ExtractErrorSignatures(buildErrors),
				modifiedFiles:   nil,
				modifiedLines:   0,
				agentOutput:     *lastGenOutput,
				confidence:      conf,
			}
			decision := guard.RecordRound(round)
			if decision.Action == "abort" {
					we.notify(we.chatID, fmt.Sprintf("🔴 编译修复被守卫终止: %s", decision.Reason))
					return false
				}
			we.notify(we.chatID, fmt.Sprintf("  🔴 编译修复第 %d 次仍失败", retry))
			continue
		}

		// L1.5: TODO/STUB 复扫
		if todos := scanForTodos(team.Cwd); len(todos) > 0 {
			buildErrors = fmt.Sprintf("TODO/STUB 检测失败, 仍发现 %d 处未实现项", len(todos))
			we.notify(we.chatID, fmt.Sprintf("  🟡 TODO 修复第 %d 次仍残留 %d 处", retry, len(todos)))
			continue
		}

		we.notify(we.chatID, fmt.Sprintf("  🟢 编译+实现均通过 (第 %d 次重试)", retry))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name,
				map[string]string{"round": fmt.Sprint(round), "build_retry": fmt.Sprint(retry)})
		}
		return true
	}
	return false
}

// runTestHardGate L2.5 测试硬门禁: 测试失败时驱动 Coder 内部重试。
// 返回 true 表示测试通过。内部重试最多 maxTestRetries 次, 不消耗对抗轮次。
func (we *WorkflowExecutor) runTestHardGate(
	ctx context.Context, genStages []StageDef,
	round, maxRounds int, lastGenOutput *string,
	objective string, prevResults map[string]string, team *ProductionTeam,
	allResults *[]StageResult,
) bool {
	if team.Cwd == "" {
		return true
	}
	lang := team.Language
	if lang == "" {
		lang = "go"
	}

	// MySQL 项目走集成测试路径
	if lang == "cpp" && isMySQLProject(team.Cwd) {
		err := we.runMySQLIntegrationTest(team.Cwd)
		if err != "" {
			we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮 MySQL 集成测试不通过, 启动修复", round))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			return we.runTestFixCycle(ctx, genStages, round, err, lastGenOutput, objective, prevResults, team, allResults)
		}
		we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮 MySQL 集成测试通过", round))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		return true
	}

	// 标准语言测试
	testErrors := runTestCheckLang(team.Cwd, lang)
	if testErrors == "" {
		 tc := GetToolchain(lang)
		we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮测试通过 (%s)", round, tc.TestCheckLabel()))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		return true
	}

	we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮测试失败, 启动内部修复 (最多 %d 次)...", round, maxTestRetries))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}

	return we.runTestFixCycle(ctx, genStages, round, testErrors, lastGenOutput, objective, prevResults, team, allResults)
}

// runTestFixCycle 测试修复循环 (被 runTestHardGate 调用)。
func (we *WorkflowExecutor) runTestFixCycle(
	ctx context.Context, genStages []StageDef,
	round int, testErrors string, lastGenOutput *string,
	objective string, prevResults map[string]string, team *ProductionTeam,
	allResults *[]StageResult,
) bool {
	lang := team.Language
	if lang == "" {
		lang = "go"
	}

	for retry := 1; retry <= maxTestRetries; retry++ {
		if ctx.Err() != nil {
			return false
		}

		fixPrompt := fmt.Sprintf("### 测试错误 (第 %d 次修复, 必须修复所有测试):\n%s\n\n"+
			"要求:\n1. 仅修复测试错误, 不要做其他改动\n2. 保持现有代码结构不变\n3. 输出修复后的完整文件内容",
			retry, truncateResult(testErrors, 3000))
		for _, genStage := range genStages {
			fixStage := genStage
			fixStage.Prompt = strings.ReplaceAll(fixStage.Prompt, "{adversarial_feedback}", fixPrompt)
			fixStage.Name = fmt.Sprintf("%s-round%d-testfix%d", genStage.Name, round, retry)
			we.notify(we.chatID, fmt.Sprintf("  🧪 测试修复 %d/%d — %s...", retry, maxTestRetries, genStage.Role))
			sr := we.executeStage(ctx, fixStage, objective, prevResults, team)
			sr.Name = fixStage.Name
			*allResults = append(*allResults, sr)
			if sr.Status == TaskCompleted {
				*lastGenOutput = sr.Output
				prevResults[genStage.Name] = sr.Output
				MaterializeCode(team.Cwd, sr.Output, lang)
			} else if isStageTransientError(sr.Error) {
				// V2 改进: test fix 中 coder 因 API 瞬态错误失败时提前返回
				we.notify(we.chatID, fmt.Sprintf("  🔴 测试修复 %d/%d 因 API 错误失败 (已自动重试), 跳过本轮", retry, maxTestRetries))
				return false
			}
		}

		// 物化后先编译
		buildErrors := runBuildCheckLang(team.Cwd, lang)
		if buildErrors != "" {
			we.notify(we.chatID, fmt.Sprintf("  🔴 测试修复第 %d 次后编译失败", retry))
			testErrors = buildErrors
			continue
		}

		// 运行测试 (MySQL 走集成测试)
		if lang == "cpp" && isMySQLProject(team.Cwd) {
			testErrors = we.runMySQLIntegrationTest(team.Cwd)
		} else {
			testErrors = runTestCheckLang(team.Cwd, lang)
		}
		if testErrors != "" {
			we.notify(we.chatID, fmt.Sprintf("  🔴 测试修复第 %d 次仍失败", retry))
			continue
		}

		we.notify(we.chatID, fmt.Sprintf("  🟢 测试通过 (第 %d 次重试)", retry))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name,
				map[string]string{"round": fmt.Sprint(round), "test_retry": fmt.Sprint(retry)})
		}
		return true
	}
	return false
}

// extractFileList 从 coder 输出中提取文件列表 (用于迭代记忆的 Approach 字段)
func extractFileList(output string) string {
	var files []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ".go") || strings.HasSuffix(trimmed, ".ts") ||
			strings.HasSuffix(trimmed, ".py") || strings.HasSuffix(trimmed, ".js") {
			if len(trimmed) < 80 {
				files = append(files, trimmed)
			}
		}
		if strings.Contains(trimmed, "```go") || strings.Contains(trimmed, "// File:") ||
			strings.Contains(trimmed, "package ") {
			if len(trimmed) < 80 {
				files = append(files, trimmed)
			}
		}
	}
	if len(files) > 10 {
		files = files[:10]
	}
	if len(files) == 0 {
		return "未识别到文件结构"
	}
	return strings.Join(files, "; ")
}

// runGeneratorRound 执行一轮 Generator
func (we *WorkflowExecutor) runGeneratorRound(
	ctx context.Context, genStages []StageDef,
	round, maxRounds int, lastOutput, lastFeedback string,
	objective string, prevResults map[string]string, team *ProductionTeam,
) (results []StageResult, finalOutput string) {
	for _, genStage := range genStages {
		feedbackSection := we.buildFeedbackSection(round, lastOutput, lastFeedback, team)
		modifiedPrompt := strings.ReplaceAll(genStage.Prompt, "{adversarial_feedback}", feedbackSection)
		tempStage := genStage
		tempStage.Prompt = modifiedPrompt
		tempStage.Name = fmt.Sprintf("%s-round%d", genStage.Name, round)

		var roleRestore func()
		if we.roles != nil {
			if role := we.roles.Get(genStage.Role); role != nil && strings.Contains(role.SystemPrompt, "{adversarial_feedback}") {
				orig := role.SystemPrompt
				role.SystemPrompt = strings.ReplaceAll(role.SystemPrompt, "{adversarial_feedback}", feedbackSection)
				roleRestore = func() { role.SystemPrompt = orig }
			}
		}

		we.notify(we.chatID, fmt.Sprintf("🔨 对抗第 %d/%d 轮 — %s (%s)...", round, maxRounds, genStage.Name, genStage.Role))
		sr := we.executeStage(ctx, tempStage, objective, prevResults, team)
		if roleRestore != nil {
			roleRestore()
		}
		sr.Name = tempStage.Name
		results = append(results, sr)
		if sr.Status == TaskCompleted {
			finalOutput = sr.Output
			prevResults[genStage.Name] = sr.Output
		}
	}
	return
}

// buildFeedbackSection 构建跨轮反馈上下文。
// L5 改进: 渐进式压缩 (参考 Kimi K2 溢出策略 + Reflexion 结构化记忆)
// - Round 1: 全量设计文档 (无压缩)
// - Round 2: 上轮输出压缩到 8K + Evaluator 反馈
// - Round 3+: 上轮输出压缩到 4K + 仅关键问题 + 工作区文件清单
func (we *WorkflowExecutor) buildFeedbackSection(round int, lastOutput, lastFeedback string, team *ProductionTeam) string {
	section := ""

	if lastFeedback != "" {
		// 反馈也做压缩: 超过 4K 时提取关键问题
		feedback := lastFeedback
		if len(feedback) > 4000 {
			feedback = SummarizeOldOutput(feedback, 4000)
		}
		section = fmt.Sprintf("### Evaluator 第 %d 轮反馈 (必须全部修复):\n%s", round-1, feedback)
	}

	if round > 1 && lastOutput != "" {
		// 渐进式压缩: 越后面的轮次压缩越狠
		maxOutputLen := 8000
		if round >= 3 {
			maxOutputLen = 4000
		}
		if round >= 4 {
			maxOutputLen = 2000
		}

		prevSummary := lastOutput
		if len(prevSummary) > maxOutputLen {
			prevSummary = SummarizeOldOutput(prevSummary, maxOutputLen)
		}

		section = fmt.Sprintf("### 你的第 %d 轮代码输出 (严禁从零重写, 仅做增量修改):\n%s\n\n%s",
			round-1, prevSummary, section)

		if team.StartedAt.Unix() > 0 {
			if manifest := workspaceFileManifest(team.Cwd, team.StartedAt); manifest != "" {
				section = manifest + "\n" + section
			}
		}
	}
	return section
}

// runBuildGate 编译验证门禁 (多语言感知)
func (we *WorkflowExecutor) runBuildGate(ctx context.Context, team *ProductionTeam, round int, prevFeedback string) string {
	if team.Cwd == "" {
		return prevFeedback
	}
	lang := team.Language
	if lang == "" {
		lang = "go"
	}
	buildErrors := runBuildCheckLang(team.Cwd, lang)
	if buildErrors != "" {
		we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮编译检查失败", round))
		logging.Event(ctx, "adversarial.build_fail", "round", round)
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		buildPrefix := "### 编译错误 (必须优先修复):\n" + buildErrors
		if prevFeedback == "" {
			return buildPrefix
		}
		return buildPrefix + "\n\n" + prevFeedback
	}
	we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮编译检查通过", round))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}
	return prevFeedback
}

// runTestGate L2.5 测试轻量门禁: 仅返回测试结果文本, 无 fix cycle。
func (we *WorkflowExecutor) runTestGate(ctx context.Context, team *ProductionTeam, round int, prevFeedback string) string {
	if team.Cwd == "" {
		return prevFeedback
	}
	lang := team.Language
	if lang == "" {
		lang = "go"
	}

	// MySQL 项目走集成测试
	if lang == "cpp" && isMySQLProject(team.Cwd) {
		if err := we.runMySQLIntegrationTest(team.Cwd); err != "" {
			we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮 MySQL 集成测试失败", round))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			testPrefix := "### MySQL 集成测试错误 (必须修复):\n" + err
			if prevFeedback == "" {
				return testPrefix
			}
			return testPrefix + "\n\n" + prevFeedback
		}
		we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮 MySQL 集成测试通过", round))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		return prevFeedback
	}

	testErrors := runTestCheckLang(team.Cwd, lang)
	if testErrors != "" {
		we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮测试检查失败", round))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		testPrefix := "### 测试错误 (必须优先修复):\n" + testErrors
		if prevFeedback == "" {
			return testPrefix
		}
		return testPrefix + "\n\n" + prevFeedback
	}
	we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮测试检查通过", round))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}
	return prevFeedback
}

// runEvaluatorRound 执行一轮 Evaluator
func (we *WorkflowExecutor) runEvaluatorRound(
	ctx context.Context, evalStage *StageDef, genStages []StageDef,
	round, maxRounds int, lastGenOutput string,
	terminator *AdaptiveTerminator,
	objective string, prevResults map[string]string, team *ProductionTeam,
	lastEvalFeedback *string, lastScore EvalScore,
) (results []StageResult, shouldBreak bool, bestOutput string) {
	if evalStage == nil {
		return nil, false, ""
	}

	genName := genStages[len(genStages)-1].Name
	evalPrevResults := map[string]string{genName: lastGenOutput}
	for k, v := range prevResults {
		evalPrevResults[k] = v
	}

	modifiedPrompt := strings.ReplaceAll(evalStage.Prompt, "{adversarial_round}", fmt.Sprintf("%d", round))
	tempStage := *evalStage
	tempStage.Prompt = modifiedPrompt
	tempStage.Name = fmt.Sprintf("%s-round%d", evalStage.Name, round)

	we.notify(we.chatID, fmt.Sprintf("🔍 对抗第 %d/%d 轮 — 审查中...", round, maxRounds))
	sr := we.runAgent(ctx, evalStage.Role, buildStagePromptWithRoles(tempStage, objective, evalPrevResults, we.roles), team)
	sr.Name = tempStage.Name
	results = append(results, sr)

	if sr.Status != TaskCompleted {
		*lastEvalFeedback = "评估器未能正常返回结果，请全面检查输出质量。"
		return results, false, ""
	}

	score, scoreErr := ParseEvalScoreJSON([]byte(sr.Output))
	if scoreErr != nil {
		log.Printf("[对抗] 第 %d 轮评分解析失败, hold-last-value: %v", round, scoreErr)
		score = HoldLastOrDefault(lastScore)
		score.Feedback = sr.Output
	}

	scoreMsg := fmt.Sprintf("正确=%.0f 完整=%.0f 安全=%.0f 质量=%.0f",
		score.Correctness, score.Completeness, score.Security, score.CodeQuality)
	if score.DesignAlignment > 0 {
		scoreMsg += fmt.Sprintf(" 对齐=%.0f", score.DesignAlignment)
	}
	if team.Blackboard != nil {
		team.Blackboard.Write(fmt.Sprintf("eval-round%d-score", round),
			scoreMsg+fmt.Sprintf(" 通过:%v", score.Pass), "evaluator", "score")
	}

	passLabel := map[bool]string{true: "✅ 通过", false: "❌ 未通过"}[score.MeetsHardPassThreshold()]
	we.notify(we.chatID, fmt.Sprintf("📊 第 %d 轮评分: %s | %s", round, scoreMsg, passLabel))

	// 自适应终止判断
	if terminator != nil {
		terminator.RecordRoundOutput(round, score, lastGenOutput)
		decision := terminator.ShouldTerminate(round, score)
		if decision.StrategyShift {
			we.notify(we.chatID, fmt.Sprintf("🔀 策略转换 (第 %d 次): 当前修补已饱和, 注入结构性变更提示",
				terminator.StrategyShiftCount))
			*lastEvalFeedback = fmt.Sprintf("⚠️ **策略转换要求** (第 %d 次):\n"+
				"当前修补方式已饱和, 请从架构层面重新思考:\n"+
				"1. 换一种完全不同的实现思路\n2. 重新分析问题本质\n3. 不要在现有方案上微调\n\n"+
				"之前的反馈:\n%s", terminator.StrategyShiftCount, *lastEvalFeedback)
			return results, false, ""
		}
		if decision.ShouldStop {
			reasonCN := map[string]string{
				"quality_pass": "质量达标", "max_rounds": "达到最大轮数",
				"degradation": "连续退化", "converged": "改进已饱和",
			}[decision.Reason]
			we.notify(we.chatID, fmt.Sprintf("🏁 自适应终止: %s (原因: %s)", passLabel, reasonCN))
			if decision.BestOutput != "" {
				we.notify(we.chatID, fmt.Sprintf("⏪ best-of-N 回滚到第 %d 轮 (最高分)", decision.BestRound))
				bestOutput = decision.BestOutput
			}
			we.recordEvalMetrics(team, round, score, decision.Reason)
			return results, true, bestOutput
		}
		we.notify(we.chatID, fmt.Sprintf("🔄 自适应继续: %s, %d/%d轮", decision.Reason, round, maxRounds))
	} else if score.MeetsHardPassThreshold() {
		we.notify(we.chatID, fmt.Sprintf("✅ 对抗通过！第 %d 轮评审达标。", round))
		we.recordEvalMetrics(team, round, score, "pass")
		return results, true, ""
	}

	*lastEvalFeedback = score.Feedback
	if *lastEvalFeedback == "" {
		*lastEvalFeedback = sr.Output
	}
	return results, false, ""
}

// recordEvalMetrics 记录评估指标
func (we *WorkflowExecutor) recordEvalMetrics(team *ProductionTeam, round int, score EvalScore, reason string) {
	if we.metrics == nil {
		return
	}
	val := 0.0
	if score.MeetsHardPassThreshold() {
		val = 1.0
	}
	we.metrics.RecordRun("team", metrics.MTeamEvalPassRate, val, team.Name,
		map[string]string{"round": fmt.Sprint(round), "termination": reason})
	we.metrics.RecordRun("team", metrics.MTeamRoundCount, float64(round), team.Name, nil)
}

// runE2EAdversarial Phase 3: E2E 对抗测试 (tester↔coder 自适应循环)。
// 不再是1轮 E2E + 1轮修复, 而是完整的对抗循环:
// E2E-tester 发现问题 → coder 修复 → E2E-tester 回归验证 → 直到通过或达到上限。
// 参考 TDAD (2026): E2E 作为质量门禁, 驱动增量修复。
func (we *WorkflowExecutor) runE2EAdversarial(ctx context.Context, parallelStages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	var testerStage *StageDef
	for i, ps := range parallelStages {
		if ps.Role == "tester" {
			testerStage = &parallelStages[i]
			break
		}
	}
	if testerStage == nil {
		return nil
	}

	var results []StageResult
	e2eTerminator := NewAdaptiveTerminator(1, 3) // E2E: 最少1轮, 最多3轮
	we.notify(we.chatID, "🧪 Phase 3: E2E 对抗测试 (tester↔coder 自适应)...")

	if local := we.runLocalE2EGate(team, objective); local != nil {
		prevResults["e2e-test"] = local.Output
		if local.Status == TaskCompleted || we.factory == nil {
			return []StageResult{*local}
		}
		results = append(results, *local)
		fixedLocal, fixResults := we.runLocalE2EFixCycle(ctx, local, objective, prevResults, team)
		results = append(results, fixResults...)
		prevResults["e2e-test"] = fixedLocal.Output
		results = append(results, *fixedLocal)
		return results
	}

	var lastE2EOutput string
	for round := 1; round <= 3; round++ {
		if ctx.Err() != nil {
			break
		}

		// 每轮独立超时, 防止 429 限流耗尽全部时间
		e2eRoundTimeout := 5 * time.Minute
		roundCtx, roundCancel := context.WithTimeout(ctx, e2eRoundTimeout)

		// E2E Tester
		e2ePrompt := we.buildE2EPrompt(objective, prevResults, lastE2EOutput, round)
		e2eStageDef := StageDef{Name: fmt.Sprintf("e2e-round%d", round), Role: "tester", Prompt: e2ePrompt}
		e2eResult := we.executeStage(roundCtx, e2eStageDef, objective, prevResults, team)
		roundCancel()
		e2eResult.Name = e2eStageDef.Name
		results = append(results, e2eResult)

		if e2eResult.Status != TaskCompleted {
			break
		}
		lastE2EOutput = e2eResult.Output
		prevResults["e2e-test"] = e2eResult.Output

		// 评估 E2E 结果: 无 Bug/FAIL 则通过
		hasBugs := strings.Contains(strings.ToLower(e2eResult.Output), "bug") ||
			strings.Contains(strings.ToLower(e2eResult.Output), "fail") ||
			strings.Contains(strings.ToLower(e2eResult.Output), "错误")

		e2eScore := EvalScore{Correctness: 8, Completeness: 8, Security: 7, CodeQuality: 7, Pass: !hasBugs}
		if hasBugs {
			e2eScore = EvalScore{Correctness: 4, Completeness: 5, Security: 7, CodeQuality: 6, Pass: false}
		}

		decision := e2eTerminator.ShouldTerminate(round, e2eScore)
		if decision.ShouldStop && !hasBugs {
			we.notify(we.chatID, fmt.Sprintf("✅ E2E 第 %d 轮通过, 无阻断性问题", round))
			break
		}

		if !hasBugs {
			we.notify(we.chatID, fmt.Sprintf("✅ E2E 第 %d 轮未发现严重问题", round))
			break
		}

		// Coder 修复
		we.notify(we.chatID, fmt.Sprintf("🔧 E2E 第 %d 轮发现问题, 驱动 coder 修复...", round))
		fixPrompt := fmt.Sprintf(`E2E 测试第 %d 轮发现以下问题, 请逐一修复:

%s

修复后确保编译和测试通过。
`, round, e2eResult.Output)
		fixStage := StageDef{Name: fmt.Sprintf("e2e-fix-round%d", round), Role: "coder", Prompt: fixPrompt}
		fixResult := we.executeStage(ctx, fixStage, objective, prevResults, team)
		fixResult.Name = fixStage.Name
		results = append(results, fixResult)
		if fixResult.Status == TaskCompleted {
			prevResults["implement"] = fixResult.Output
		}

		if decision.ShouldStop {
			we.notify(we.chatID, fmt.Sprintf("🏁 E2E 对抗终止 (原因: %s)", decision.Reason))
			break
		}
	}
	return results
}

// runLocalE2EFixCycle E2E 本地修复循环。
func (we *WorkflowExecutor) runLocalE2EFixCycle(ctx context.Context, initial *StageResult, objective string, prevResults map[string]string, team *ProductionTeam) (*StageResult, []StageResult) {
	if initial == nil || initial.Status == TaskCompleted {
		return initial, nil
	}
	if team == nil || strings.TrimSpace(team.Cwd) == "" || we.factory == nil {
		return initial, nil
	}
	lang := team.Language
	if strings.TrimSpace(lang) == "" {
		lang = "go"
	}
	gateCwd := localE2EGateCwd(team, objective)
	goBaseline := captureGoModBaseline(gateCwd)
	var results []StageResult
	lastFailure := initial.Output
	if strings.TrimSpace(lastFailure) == "" {
		lastFailure = initial.Error
	}

	for retry := 1; retry <= maxTestRetries; retry++ {
		if ctx.Err() != nil {
			break
		}
		prompt := buildLocalE2EFixPrompt(gateCwd, lang, objective, lastFailure, retry, team, goBaseline)
		stageCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		sr := we.runAgent(stageCtx, "coder", prompt, team)
		cancel()
		sr.Name = fmt.Sprintf("e2e-local-fix-%d", retry)
		results = append(results, sr)
		if sr.Status != TaskCompleted {
			if isStageTransientError(sr.Error) {
				break
			}
			lastFailure = sr.Error
			continue
		}

		written := MaterializeCode(gateCwd, sr.Output, lang)
		if len(written) == 0 {
			written = MaterializeCode(team.Cwd, sr.Output, lang)
		}
		if len(written) > 0 {
			we.notify(we.chatID, fmt.Sprintf("📁 E2E 本地修复物化: %d 个文件", len(written)))
		}
		if changed := restoreGoModBaseline(gateCwd, goBaseline); changed > 0 {
			we.notify(we.chatID, fmt.Sprintf("🛡️ E2E 本地修复保护 go.mod: 回滚 %d 处 module/依赖漂移", changed))
		}
		local := we.runLocalE2EGate(team, objective)
		if local == nil {
			break
		}
		prevResults["e2e-local-fix"] = sr.Output
		if local.Status == TaskCompleted {
			we.notify(we.chatID, fmt.Sprintf("🟢 E2E 本地修复第 %d 次后通过", retry))
			return local, results
		}
		lastFailure = local.Output
		if strings.TrimSpace(lastFailure) == "" {
			lastFailure = local.Error
		}
		we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地修复第 %d 次后仍失败", retry))
	}

	failed := *initial
	failed.Name = "e2e-local-gate"
	failed.Status = TaskFailed
	failed.Error = "E2E 本地门禁失败: build/test 未通过"
	failed.Output = failed.Error + "\n" + truncateResult(lastFailure, 4000)
	return &failed, results
}

func localE2EGateCwd(team *ProductionTeam, objective string) string {
	if team == nil {
		return ""
	}
	gateCwd := team.Cwd
	if targetRoot := inferObjectiveTargetRoot(objective); targetRoot != "" {
		candidate := filepath.Join(team.Cwd, targetRoot)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			gateCwd = candidate
		}
	}
	return gateCwd
}

type goModBaseline struct {
	Exists      bool
	ModulePath  string
	RequireSet  map[string]bool
	RequireList []string
}

func captureGoModBaseline(cwd string) goModBaseline {
	path := filepath.Join(cwd, "go.mod")
	if _, err := os.Stat(path); err != nil {
		return goModBaseline{}
	}
	modulePath, requires := readGoModModuleAndRequires(path)
	set := make(map[string]bool, len(requires))
	for _, req := range requires {
		req = strings.TrimSpace(req)
		if req != "" {
			set[req] = true
		}
	}
	return goModBaseline{
		Exists:      true,
		ModulePath:  strings.TrimSpace(modulePath),
		RequireSet:  set,
		RequireList: requires,
	}
}

func restoreGoModBaseline(cwd string, baseline goModBaseline) int {
	if cwd == "" || !baseline.Exists {
		return 0
	}
	path := filepath.Join(cwd, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	changed := 0
	currentModule, _ := readGoModModuleAndRequires(path)
	updated := string(data)
	if baseline.ModulePath != "" && strings.TrimSpace(currentModule) != baseline.ModulePath {
		next := rewriteGoModModuleLine(updated, baseline.ModulePath)
		if next != updated {
			changed++
			updated = next
		}
		if strings.TrimSpace(currentModule) != "" {
			changed += rewriteGoModuleImportPrefix(cwd, strings.TrimSpace(currentModule), baseline.ModulePath)
		}
	}
	next, removed := removeGoModRequiresNotInBaseline(updated, baseline.RequireSet)
	if removed > 0 {
		changed += removed
		updated = next
	}
	if updated != string(data) {
		_ = os.WriteFile(path, []byte(updated), 0o644)
	}
	return changed
}

func rewriteGoModModuleLine(src, modulePath string) string {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" {
		return src
	}
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "module ") {
			lines[i] = "module " + modulePath
			return strings.Join(lines, "\n")
		}
	}
	return "module " + modulePath + "\n\n" + src
}

func rewriteGoModuleImportPrefix(cwd, fromModule, toModule string) int {
	fromModule = strings.TrimSpace(fromModule)
	toModule = strings.TrimSpace(toModule)
	if cwd == "" || fromModule == "" || toModule == "" || fromModule == toModule {
		return 0
	}
	changed := 0
	var changedFiles []string
	_ = filepath.Walk(cwd, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".claude-go", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		original := string(data)
		updated := strings.ReplaceAll(original, strconv.Quote(fromModule), strconv.Quote(toModule))
		updated = strings.ReplaceAll(updated, fromModule+"/", toModule+"/")
		if updated == original {
			return nil
		}
		if writeErr := os.WriteFile(p, []byte(updated), info.Mode()); writeErr == nil {
			changed++
			changedFiles = append(changedFiles, p)
		}
		return nil
	})
	if len(changedFiles) > 0 {
		_ = exec.Command("gofmt", append([]string{"-w"}, changedFiles...)...).Run()
	}
	return changed
}

func removeGoModRequiresNotInBaseline(src string, allowed map[string]bool) (string, int) {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	removed := 0
	inBlock := false
	var block []string
	var keptEntries int
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inBlock && strings.HasPrefix(trimmed, "require (") {
			inBlock = true
			block = []string{line}
			keptEntries = 0
			continue
		}
		if inBlock {
			if trimmed == ")" {
				if keptEntries > 0 {
					out = append(out, block...)
					out = append(out, line)
				} else {
					removed++
				}
				inBlock = false
				block = nil
				continue
			}
			fields := strings.Fields(trimmed)
			if len(fields) == 0 {
				block = append(block, line)
				continue
			}
			if allowed[fields[0]] {
				block = append(block, line)
				keptEntries++
			} else {
				removed++
			}
			continue
		}
		if strings.HasPrefix(trimmed, "require ") {
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(trimmed, "require ")))
			if len(fields) > 0 && !allowed[fields[0]] {
				removed++
				continue
			}
		}
		out = append(out, line)
	}
	if inBlock {
		out = append(out, block...)
	}
	return strings.Join(out, "\n"), removed
}

func buildLocalE2EFixPrompt(gateCwd, lang, objective, failure string, retry int, team *ProductionTeam, goBaseline goModBaseline) string {
	manifest := workspaceFileManifest(gateCwd, time.Time{})
	if strings.TrimSpace(manifest) == "" && team != nil {
		manifest = workspaceFileManifest(team.Cwd, time.Time{})
	}
	if len(manifest) > 3000 {
		manifest = SummarizeOldOutput(manifest, 3000)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### E2E 本地门禁失败修复任务 (第 %d 次)\n\n", retry)
	fmt.Fprintf(&b, "目标:\n%s\n\n", objective)
	fmt.Fprintf(&b, "语言/运行时: %s\n", lang)
	fmt.Fprintf(&b, "工作目录: %s\n\n", filepath.ToSlash(gateCwd))
	if strings.EqualFold(lang, "go") && goBaseline.Exists {
		if goBaseline.ModulePath != "" {
			fmt.Fprintf(&b, "Go module 基线: %s (go.mod 的 module 行不得修改)\n", goBaseline.ModulePath)
		}
		if len(goBaseline.RequireList) > 0 {
			fmt.Fprintf(&b, "Go require 基线: %s (E2E 修复不得新增 require)\n", strings.Join(goBaseline.RequireList, ", "))
		} else {
			b.WriteString("Go require 基线: 空 (E2E 修复不得新增第三方 require/import)\n")
		}
		b.WriteString("本地门禁在禁网沙盒中运行; 修复必须优先使用标准库或本项目内实现, 不要通过新增 go.mod 依赖让测试依赖网络下载。\n\n")
	}
	fmt.Fprintf(&b, "当前失败:\n%s\n\n", truncateResult(failure, 5000))
	if strings.TrimSpace(manifest) != "" {
		b.WriteString(manifest)
		b.WriteString("\n")
	}
	if strings.EqualFold(lang, "go") {
		if api := summarizeGoPackageAPI(gateCwd, 9000); api != "" {
			b.WriteString("### 当前真实 Go API 摘要 (来自磁盘, 修复必须对齐这些签名)\n")
			b.WriteString(api)
			b.WriteString("\n")
		}
	}
	b.WriteString("要求:\n")
	b.WriteString("1. 只修复导致本地 build/test 失败的问题, 不要重写项目或更换架构。\n")
	b.WriteString("2. 必须基于现有文件增量修改; 若测试显示行为不正确, 修复实现而不是删除/放宽测试。\n")
	b.WriteString("3. 保留用户要求的输出目录和语言; 不要引入未请求的外部服务、HTTP/RPC server/client、API Key 或大型持久化引擎。\n")
	fmt.Fprintf(&b, "4. 输出需要修改的完整文件内容, 每个代码块必须带文件路径, 例如 ```%s:path/to/file.ext。\n", materializeFenceLang(lang))
	b.WriteString("5. Go 项目不得修改既有 go.mod module 行, 不得新增第三方 require/import; 若失败来自 testify/grpc/protobuf 等依赖, 请改成 testing/标准库或项目内轻量接口实现。\n")
	b.WriteString("6. 修复后应能通过项目本地 build/test。")
	return b.String()
}

func materializeFenceLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "python", "py":
		return "python"
	case "typescript", "ts":
		return "typescript"
	case "javascript", "js":
		return "javascript"
	case "rust", "rs":
		return "rust"
	case "cpp", "c++", "c":
		return "cpp"
	default:
		return "go"
	}
}

func (we *WorkflowExecutor) runLocalE2EGate(team *ProductionTeam, objective string) *StageResult {
	start := time.Now()
	result := &StageResult{Name: "e2e-local-gate", Role: "tester", StartedAt: start}
	if team == nil || team.Cwd == "" {
		result.Status = TaskFailed
		result.Error = "E2E 本地门禁失败: 工作目录为空"
		result.Output = result.Error
		result.Duration = time.Since(start).Round(time.Second).String()
		return result
	}

	gateCwd := team.Cwd
	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot != "" {
		gateCwd = filepath.Join(team.Cwd, targetRoot)
		if _, err := os.Stat(gateCwd); err != nil {
			result.Status = TaskFailed
			result.Error = "E2E 本地门禁失败: 用户指定输出目录未创建: " + targetRoot
			result.Output = result.Error
			result.Duration = time.Since(start).Round(time.Second).String()
			we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败: 目标目录 %s 不存在", targetRoot))
			return result
		} else if !isDir(gateCwd) {
			result.Status = TaskFailed
			result.Error = "E2E 本地门禁失败: 用户指定输出路径不是目录: " + targetRoot
			result.Output = result.Error
			result.Duration = time.Since(start).Round(time.Second).String()
			we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败: 目标路径 %s 不是目录", targetRoot))
			return result
		}
	}
	if removed := cleanupMaterializedMetadataDirs(gateCwd); removed > 0 {
		we.notify(we.chatID, fmt.Sprintf("🧹 E2E 本地门禁: 清理错误物化元数据目录 %d 个", removed))
	}

	todos := scanForTodos(gateCwd)
	if len(todos) > 0 {
		if objectiveRequiresCompleteImplementation(objective) {
			result.Status = TaskFailed
			result.Error = fmt.Sprintf("E2E 本地门禁失败: 发现 %d 处未实现 TODO/STUB/placeholder", len(todos))
			result.Output = result.Error + "\n" + strings.Join(firstNStrings(todos, 30), "\n")
			result.Duration = time.Since(start).Round(time.Second).String()
			if we.notify != nil {
				we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败: 发现 %d 处未实现项, 完整实现任务禁止以 TODO/STUB 交付", len(todos)))
			}
			return result
		}
		we.notify(we.chatID, fmt.Sprintf("🟡 E2E 本地门禁: 发现 %d 处 TODO/STUB 注释, 记录为警告但以 build/test 为准", len(todos)))
	}

	lang := "go"
	if team.Language != "" {
		lang = team.Language
	}
	failures := runLocalE2EChecks(gateCwd, lang, objective)
	if strings.EqualFold(lang, "go") {
		for repairRound := 1; repairRound <= 2 && len(failures) > 0; repairRound++ {
			errText := strings.Join(failures, "\n\n")
			changed := applyGoCompileErrorRepairs(gateCwd, errText)
			changed += applyGoCompileTextRepairs(gateCwd)
			if changed == 0 {
				break
			}
			we.notify(we.chatID, fmt.Sprintf("🛠️ E2E 本地门禁: 第 %d 轮确定性 Go 编译修复已应用, 重新 build/test", repairRound))
			failures = runLocalE2EChecks(gateCwd, lang, objective)
		}
		if len(failures) > 0 {
			settledFailures := runLocalE2EChecks(gateCwd, lang, objective)
			if len(settledFailures) == 0 {
				failures = nil
				we.notify(we.chatID, "🟢 E2E 本地门禁: 最终 settle recheck 已通过")
			} else {
				failures = settledFailures
			}
		}
	}
	result.Duration = time.Since(start).Round(time.Second).String()
	if len(failures) > 0 {
		result.Status = TaskFailed
		result.Error = "E2E 本地门禁失败: build/test 未通过"
		result.Output = result.Error + "\n" + strings.Join(failures, "\n\n")
		we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败 (%s), 不再调用 tester LLM: %s", result.Duration, summarizeE2EFailureForNotify(failures)))
		return result
	}

	result.Status = TaskCompleted
	result.Output = "E2E 本地门禁通过: build/test 通过"
	if len(todos) > 0 {
		result.Output += fmt.Sprintf("; TODO/STUB warning=%d", len(todos))
	}
	we.notify(we.chatID, fmt.Sprintf("✅ E2E 本地门禁通过 (%s), 跳过 tester LLM", result.Duration))
	return result
}

func runLocalE2EChecks(gateCwd, lang, objective string) []string {
	var failures []string
	if strings.EqualFold(lang, "go") {
		if errText := enforceGoExternalDependencyPolicy(gateCwd, objective); errText != "" {
			failures = append(failures, errText)
			return failures
		}
	}
	if errText := runBuildCheckScoped(gateCwd, lang, nil); errText != "" {
		failures = append(failures, errText)
	}
	if errText := runTestCheckLang(gateCwd, lang); errText != "" {
		failures = append(failures, errText)
	}
	return failures
}

func enforceGoExternalDependencyPolicy(cwd, objective string) string {
	if cwd == "" || !objectiveRequestsZeroExternalDeps(objective) {
		return ""
	}
	modPath := filepath.Join(cwd, "go.mod")
	if _, err := os.Stat(modPath); err != nil {
		return ""
	}
	_, requires := readGoModModuleAndRequires(modPath)
	var declared []string
	for _, req := range requires {
		req = strings.TrimSpace(req)
		if req != "" {
			declared = append(declared, req)
		}
	}
	imports := goExternalImports(cwd)
	var sumEntries []string
	if data, err := os.ReadFile(filepath.Join(cwd, "go.sum")); err == nil {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) == 0 {
				continue
			}
			if fields[0] != "" && !seen[fields[0]] {
				sumEntries = append(sumEntries, fields[0])
				seen[fields[0]] = true
			}
		}
		sort.Strings(sumEntries)
	}
	if len(declared) == 0 && len(imports) == 0 && len(sumEntries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Go 零外部依赖门禁失败: 目标/参考设计要求零外部依赖或单二进制部署, 最终项目不得保留第三方 require/import/go.sum。\n")
	if len(declared) > 0 {
		b.WriteString("go.mod require: " + strings.Join(declared, ", ") + "\n")
	}
	if len(imports) > 0 {
		b.WriteString("外部 import: " + strings.Join(imports, ", ") + "\n")
	}
	if len(sumEntries) > 0 {
		b.WriteString("go.sum entries: " + strings.Join(sumEntries, ", ") + "\n")
	}
	b.WriteString("请改为 Go 标准库或项目内轻量实现, 删除新增 require/go.sum, 并保持 go.mod module 行不变。")
	return b.String()
}

func objectiveRequestsZeroExternalDeps(objective string) bool {
	if containsZeroExternalDepsTerm(objective) {
		return true
	}
	for _, p := range extractLocalReferencePaths(objective) {
		excerpt := readLocalReferenceExcerpt(p, 60000)
		if containsZeroExternalDepsTerm(excerpt) {
			return true
		}
	}
	return false
}

func containsZeroExternalDepsTerm(text string) bool {
	lower := strings.ToLower(text)
	terms := []string{
		"零外部依赖",
		"零依赖",
		"无外部依赖",
		"单二进制",
		"zero external dependency",
		"zero external dependencies",
		"zero dependency",
		"zero dependencies",
		"no external dependency",
		"no external dependencies",
		"single binary",
	}
	for _, term := range terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func objectiveRequiresCompleteImplementation(objective string) bool {
	if containsCompleteImplementationTerm(objective) {
		return true
	}
	for _, p := range extractLocalReferencePaths(objective) {
		excerpt := readLocalReferenceExcerpt(p, 60000)
		if containsCompleteImplementationTerm(excerpt) {
			return true
		}
	}
	return false
}

func containsCompleteImplementationTerm(text string) bool {
	lower := strings.ToLower(text)
	terms := []string{
		"100% 覆盖",
		"100%覆盖",
		"100% 满足",
		"100%满足",
		"严格参考",
		"完整实现",
		"不得降级",
		"不允许降级",
		"不能降级",
		"未来扩展",
		"未实现能力",
		"full implementation",
		"complete implementation",
		"fully implemented",
		"no mvp",
		"not mvp",
		"no placeholder",
		"no placeholders",
	}
	for _, term := range terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func firstNStrings(values []string, n int) []string {
	if n <= 0 || len(values) <= n {
		return values
	}
	out := append([]string(nil), values[:n]...)
	out = append(out, fmt.Sprintf("...还有 %d 处", len(values)-n))
	return out
}

func summarizeE2EFailureForNotify(failures []string) string {
	joined := strings.TrimSpace(strings.Join(failures, "\n\n"))
	if joined == "" {
		return "无详细失败输出"
	}
	joined = strings.ReplaceAll(joined, "\n", " | ")
	return truncateResult(joined, 600)
}

func cleanupMaterializedMetadataDirs(root string) int {
	if root == "" {
		return 0
	}
	names := []string{"path=", "file=", "filepath=", "file_path="}
	removed := 0
	for _, name := range names {
		candidate := filepath.Join(root, name)
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := os.RemoveAll(candidate); err == nil {
			removed++
		}
	}
	return removed
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (we *WorkflowExecutor) buildE2EPrompt(objective string, prevResults map[string]string, lastE2E string, round int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`你是高级质量工程师 (E2E 全量测试, 第 %d 轮)。

目标: %s

`, round, objective))

	if round > 1 && lastE2E != "" {
		b.WriteString("### 上轮 E2E 发现的问题 (需验证是否已修复):\n")
		b.WriteString(truncateResult(lastE2E, 4000))
		b.WriteString("\n\n")
	}

	b.WriteString("### 实现产出摘要:\n")
	b.WriteString(buildPrevResultsSummary(prevResults))
	b.WriteString(`
## E2E 测试要求
1. **跨模块集成测试**: 模块间接口连通性、数据传递、错误传播
2. **端到端流程测试**: Happy Path + Error Path 完整链路
3. **回归验证**: 确认之前发现的问题是否已修复
4. **编译验证**: 编译+静态分析+测试通过

必须实际编写测试代码并运行。`)
	return b.String()
}

// runFinishPhase Phase 4: 非测试的收尾阶段
func (we *WorkflowExecutor) runFinishPhase(ctx context.Context, parallelStages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	var nonTestStages []StageDef
	for _, ps := range parallelStages {
		if ps.Role != "tester" {
			nonTestStages = append(nonTestStages, ps)
		}
	}
	if len(nonTestStages) == 0 {
		return nil
	}
	we.notify(we.chatID, fmt.Sprintf("📦 Phase 4: 收尾 (%d 阶段)...", len(nonTestStages)))
	return we.executeParallel(ctx, nonTestStages, objective, prevResults, team)
}

// ClassifyStages 导出版本，供测试和外部调用。
func ClassifyStages(stages []StageDef) (design []StageDef, generators []StageDef, eval *StageDef, parallel []StageDef) {
	return classifyStages(stages)
}

func classifyStages(stages []StageDef) (design []StageDef, generators []StageDef, eval *StageDef, parallel []StageDef) {
	isEval := make(map[string]bool)
	isParallel := make(map[string]bool)
	isGenerator := make(map[string]bool)

	// Pass 1: 找 evaluator (prompt 中包含 JSON 评分格式)
	for i := range stages {
		if strings.Contains(stages[i].Prompt, `"correctness"`) && strings.Contains(stages[i].Prompt, `"pass"`) {
			eval = &stages[i]
			isEval[stages[i].Name] = true
			break
		}
	}

	// Pass 2: 找 parallel 收尾阶段
	for i := range stages {
		if stages[i].Parallel && !isEval[stages[i].Name] {
			parallel = append(parallel, stages[i])
			isParallel[stages[i].Name] = true
		}
	}

	// Pass 3: 找 generator (被 evaluator 直接或间接依赖，且包含 {adversarial_feedback})
	if eval != nil {
		for _, dep := range eval.DependsOn {
			for i := range stages {
				if stages[i].Name == dep && !isEval[dep] && !isParallel[dep] {
					isGenerator[dep] = true
				}
			}
		}
	}
	// 也检查 prompt 中包含 {adversarial_feedback} 的阶段
	for i := range stages {
		if !isEval[stages[i].Name] && !isParallel[stages[i].Name] {
			if strings.Contains(stages[i].Prompt, "{adversarial_feedback}") {
				isGenerator[stages[i].Name] = true
			}
		}
	}

	// Pass 4: 分类 — 既不是 eval/parallel/generator 的就是 design
	for _, s := range stages {
		switch {
		case isEval[s.Name], isParallel[s.Name]:
			continue
		case isGenerator[s.Name]:
			generators = append(generators, s)
		default:
			design = append(design, s)
		}
	}

	return
}

// runMySQLIntegrationTest 启动 mysqld 并执行 mysql 客户端验证集成测试。
// 返回空串表示通过, 否则返回错误信息。
func (we *WorkflowExecutor) runMySQLIntegrationTest(cwd string) string {
	if cwd == "" {
		return ""
	}

	// 定位 mysqld 二进制
	mysqldPaths := []string{
		filepath.Join(cwd, "build", "sql", "mysqld"),
		filepath.Join(cwd, "build", "runtime_output_directory", "mysqld"),
	}
	var mysqldPath string
	for _, p := range mysqldPaths {
		if _, err := os.Stat(p); err == nil {
			mysqldPath = p
			break
		}
	}
	if mysqldPath == "" {
		return "mysqld 二进制未找到 (expected in build/sql/mysqld or build/runtime_output_directory/mysqld)"
	}

	// 创建临时数据目录
	tmpDir, err := os.MkdirTemp("", "mysql-integration-*")
	if err != nil {
		return fmt.Sprintf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 初始化数据目录 (--initialize-insecure, 无密码 root)
	initCtx, initCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer initCancel()
	if out, err := runLimitedCommand(initCtx, cwd, []string{mysqldPath, "--initialize-insecure", "--datadir=" + tmpDir}, 4096, 100); err != nil {
		return fmt.Sprintf("mysqld --initialize-insecure 失败:\n%s", truncateResult(string(out), 2000))
	}

	// 后台启动 mysqld (skip-networking + unix socket, 避免端口冲突)
	socketPath := filepath.Join(tmpDir, "mysql.sock")
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	mysqldCmd := exec.CommandContext(startCtx, mysqldPath,
		"--skip-networking",
		"--socket="+socketPath,
		"--datadir="+tmpDir,
		"--skip-grant-tables",
	)
	mysqldCmd.Dir = cwd
	if err := mysqldCmd.Start(); err != nil {
		return fmt.Sprintf("mysqld 启动失败: %v", err)
	}

	// 轮询等待 socket 文件 (最多 30 秒)
	ready := false
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		if _, err := os.Stat(socketPath); err == nil {
			ready = true
			break
		}
	}
	if !ready {
		return "mysqld 未能在 30 秒内就绪 (socket 未创建)"
	}

	// 执行 mysql 客户端验证
	mysqlClient := filepath.Join(cwd, "build", "runtime_output_directory", "mysql")
	if _, err := os.Stat(mysqlClient); err != nil {
		mysqlClient = filepath.Join(cwd, "build", "client", "mysql")
	}
	if _, err := os.Stat(mysqlClient); err != nil {
		// fallback: 尝试系统 mysql
		if sysMysql, err := exec.LookPath("mysql"); err == nil {
			mysqlClient = sysMysql
		} else {
			// 无 mysql 客户端也可返回成功 (只验证 mysqld 能启动)
			return ""
		}
	}

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer clientCancel()
	out, err := runLimitedCommand(clientCtx, cwd, []string{mysqlClient,
		"-S", socketPath,
		"-e", "SHOW DATABASES;",
	}, 1024, 100)
	if err != nil {
		return fmt.Sprintf("mysql 客户端验证失败:\n%s", truncateResult(string(out), 2000))
	}
	return ""
}

// workspaceFileManifest 扫描工作区中最近修改的文件, 生成清单注入 coder prompt。
// 让每轮 coder 知道前几轮在磁盘上创建/修改了哪些文件, 避免从零重写。
func workspaceFileManifest(cwd string, since time.Time) string {
	if cwd == "" {
		return ""
	}
	var files []string
	// 剪枝与归属判据统一用 artifacts.go 的公共实现 (artifactSkipDir /
	// artifactBelongsToTeam), 避免"prompt 里看到的工作区清单"与"产物清单端点采到的
	// 文件"因两份拷贝漂移而不一致。
	_ = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if artifactSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if artifactBelongsToTeam(info.ModTime(), since) {
			rel, _ := filepath.Rel(cwd, path)
			files = append(files, rel)
		}
		return nil
	})
	if len(files) == 0 {
		return ""
	}
	if len(files) > 50 {
		files = files[:50]
	}
	return fmt.Sprintf("### 工作区中已创建/修改的文件 (%d 个, 必须在这些文件基础上增量修改):\n```\n%s\n```\n",
		len(files), strings.Join(files, "\n"))
}

// scanForTodos 扫描已物化的源码文件, 检测未实现的 TODO/STUB/HACK/placeholder。
// 返回发现的未实现项列表; 空表示全部实现完整。
// 作为 L1.5 确定性门禁, 不依赖 LLM 判断, 避免遗漏。
func scanForTodos(cwd string) []string {
	var findings []string
	sourceExts := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".java": true, ".cs": true, ".c": true, ".cpp": true,
		".h": true, ".rs": true, ".swift": true, ".kt": true, ".dart": true,
	}

	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !sourceExts[filepath.Ext(path)] {
			return nil
		}
		rel, _ := filepath.Rel(cwd, path)
		base := strings.ToLower(filepath.Base(path))
		if strings.Contains(base, "stub") || strings.Contains(base, "placeholder") {
			findings = append(findings, fmt.Sprintf("%s:1: placeholder filename %q is not allowed in deliverable source", rel, filepath.Base(path)))
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			// 跳过纯注释中的合理用法: 文档注释、版权说明等
			// 重点捕获: TODO + 不完整的实现信号
			todoPatterns := []string{
				"TODO", "FIXME", "HACK", "XXX", "STUB",
				"not implemented", "NotImplemented",
				"placeholder", "占位", "待实现", "未实现",
				"panic(", // Go 中的 panic 实现 = 未完成
			}
			for _, pat := range todoPatterns {
				if strings.Contains(trimmed, pat) {
					// 过滤: 如果是 // File: ... 或注释中的说明性文字, 跳过
					if strings.HasPrefix(trimmed, "// File:") || strings.HasPrefix(trimmed, "# File:") {
						continue
					}
					findings = append(findings, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(trimmed)))
					break // 同一行只记录一次
				}
			}
		}
		return nil
	})
	return findings
}
