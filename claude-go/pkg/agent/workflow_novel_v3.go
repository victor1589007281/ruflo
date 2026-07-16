// novel-v3 工作流: 群体智能驱动叙事演化引擎。
//
// 前半段 (Phase A 创世播种 + Phase B 群体演化 + Phase C 评估) 替代 v2 的 Phase 1,
// 通过 swarm_intel.Engine 的群体智能能力快速生成高质量故事方案;
// 后半段复用 v2 的 Phase 2-4 (大纲设计 → 章节对抗循环 → 全书整合) 进行实际写作。
//
// 六阶段:
//   Phase A: 创世播种 (Genesis) — 世界规则+角色灵魂+命运催化剂+演化蓝图
//   Phase B: 群体演化 (Swarm Evolution) — Engine.Simulate("social") 多时间线并行
//   Phase C: 命运审判 (Evaluation) — Engine.Predict() 多分析师辩论+融合
//   Phase D: 大纲设计 (Outline Design) — 复用 v2 outline-architect
//   Phase E: 章节写作 (Chapter Writing) — 复用 v2 novelist↔editor 对抗循环
//   Phase F: 全书整合 (Book Assembly) — 复用 v2 全书审阅
//
// 复用基础设施:
//   - swarm_intel.Engine: 群体智能引擎 (Simulate + Predict 流水线)
//   - Blackboard: 世界状态共享, 时间线存档
//   - EvolutionEngine: 跨阶段经验检索与学习
//   - AgentPool: 自动扩缩 + 并发槽位管理
//   - CheckpointStore: 时间线级 + 章节级断点保存
//   - AdaptiveTerminator: 章节写作对抗轮数控制
//   - SummarizeOldOutput: 滚动上下文摘要
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

func novelV3Workflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "novel-v3",
		Description: "群体智能叙事演化: swarm_intel.Engine 群体演化+预测 → 复用v2大纲+章节对抗+整合",
		Mode:        "swarm_novel",
		Rounds:      0,
		Stages: []StageDef{
			// Phase A: Genesis
			{Name: "world-forge", Role: "world-forger", Prompt: worldForgerPrompt},
			{Name: "soul-forge", Role: "soul-forger", DependsOn: []string{"world-forge"}, Prompt: soulForgerPrompt},
			{Name: "catalyst", Role: "fate-weaver", DependsOn: []string{"world-forge", "soul-forge"}, Prompt: fateWeaverPrompt},
			{Name: "evo-blueprint", Role: "evo-architect", DependsOn: []string{"world-forge", "soul-forge", "catalyst"}, Prompt: evoArchitectPrompt},
			// Phase D: 复用 v2 大纲设计
			// best-narrative(胜出时间线) + second-narrative(次优时间线, #3 跨线亮点) + evaluation(审判团共识/依据/融合建议)
		// 一并注入, 让大纲建立在 swarm 模拟结果之上, 而非仅凭世界观/角色空想 —— 否则 Phase B/C 沦为装饰。
		{Name: "outline-design", Role: "outline-architect", DependsOn: []string{"best-narrative", "second-narrative", "evaluation", "world-forge", "soul-forge", "catalyst"}, Prompt: v3OutlinePrompt},
			// Phase E-F: 由编排器动态驱动 (复用 v2 章节循环 + 全书整合)
		},
	}
}

// executeSwarmNovel 群体智能叙事演化编排器。
// 核心: Phase B 使用 swarm_intel.Engine.Simulate, Phase C 使用 Engine.Predict。
func (we *WorkflowExecutor) executeSwarmNovel(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	notify := func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}

	we.restoreCheckpoints(wf.Stages, prevResults, &allResults)
	if len(allResults) > 0 {
		notify(fmt.Sprintf("♻️ 从检查点恢复: 已恢复 %d 个阶段", len(allResults)))
	}

	if we.pool != nil {
		we.pool.AutoScale(10)
		log.Printf("[novel-v3] AgentPool AutoScale(10)")
	}

	// ═══════════════════════════════════════════════════════════
	// Phase A: 创世播种 (Genesis) — 世界规则+灵魂+催化剂+蓝图
	// ═══════════════════════════════════════════════════════════
	genesisStages := []string{"world-forge", "soul-forge", "catalyst", "evo-blueprint"}
	for i, stageName := range genesisStages {
		if _, ok := prevResults[stageName]; ok {
			continue
		}
		if i == 0 {
			notify("🌍 **Phase A**: 创世播种 (世界锻造 → 灵魂铸造 → 命运种子 → 演化蓝图)")
		}
		stage := findStage(wf, stageName)
		if stage == nil {
			continue
		}
		sr := we.runNovelStage(ctx, *stage, objective, prevResults, team)
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			prevResults[stageName] = sr.Output
			we.savePhaseCheckpoints([]StageResult{sr})
		} else {
			return allResults, fmt.Errorf("创世播种阶段 %s 失败: %s", stageName, sr.Error)
		}
	}

	blueprint := parseEvoBluePrint(prevResults["evo-blueprint"])
	notify(fmt.Sprintf("📋 演化蓝图: %d 条时间线 × %d 轮演化", blueprint.TimelineCount, blueprint.Rounds))

	soulCards := parseSoulCards(prevResults["soul-forge"])
	if len(soulCards) == 0 {
		soulCards = []soulCard{{Name: "主角", Core: prevResults["soul-forge"]}}
	}
	notify(fmt.Sprintf("👥 %d 个角色灵魂: %s", len(soulCards), soulCardNames(soulCards)))

	// ═══════════════════════════════════════════════════════════
	// Phase B: 群体演化 — 使用 swarm_intel.Engine.Simulate("social")
	// 每条时间线调用一次社会模拟, 由引擎的多Agent交互引擎驱动
	// ═══════════════════════════════════════════════════════════
	if _, ok := prevResults["best-narrative"]; !ok {
		notify(fmt.Sprintf("⚡ **Phase B**: 群体演化 (swarm_intel.Engine.Simulate × %d 条时间线)", blueprint.TimelineCount))

		if team.Blackboard != nil {
			team.Blackboard.Write("world-rules", SummarizeOldOutput(prevResults["world-forge"], 4000), "system", "context")
			team.Blackboard.Write("soul-cards", SummarizeOldOutput(prevResults["soul-forge"], 4000), "system", "context")
			team.Blackboard.Write("catalysts", SummarizeOldOutput(prevResults["catalyst"], 3000), "system", "context")
		}

		siCfg := swarm_intel.DefaultConfig()
		// 隔离小说域 swarm 学习状态到独立 DataDir: 与 aiops 等共享域的预测校准/信任互不污染,
		// 且 §2 #5/#6/#7 跨会话学习(保形/信任/路由)只在小说域累积。**这也是关键安全边界**:
		// 共享域(aiops)从不调 RecordOutcome → trustScores 恒空 → TrustWeightedTrimmedFuse 退化为
		// TrimmedFuse, 其 Predict 输出零回归。
		if home, herr := os.UserHomeDir(); herr == nil {
			siCfg.DataDir = filepath.Join(home, ".claude-go", "swarm_intel", "novel")
		}
		siCfg.Notify = func(_, msg string) {
			notify("  [群体智能] " + msg)
			if team.Blackboard != nil {
				team.Blackboard.Write("swarm-progress", msg, "engine", "progress")
			}
		}

		if we.llm == nil {
			return allResults, fmt.Errorf("Phase B 需要 LLMClient 以创建 swarm_intel.Engine")
		}
		engine := swarm_intel.NewEngine(we.llm, siCfg)

		baseObjective := buildSimulateObjective(objective, prevResults, soulCards)

		// #8: 用群体智能历史经验预热模拟目标 (跨小说学习: 哪些演化模式产出过好走向)
		if we.evolution != nil {
			if exps := we.evolution.RetrieveFor("swarm-intel", objective, 3); len(exps) > 0 {
				baseObjective += "\n\n## 历史群体演化经验 (参考, 非约束)\n" + FormatExperiencesForPrompt(exps)
				notify(fmt.Sprintf("  📚 注入 %d 条历史演化经验预热模拟", len(exps)))
			}
		}

		// #9: 用 FanOutFirstN 生成时间线 —— 多生成 1 条冗余, 保留最先完成的 TimelineCount 条成功者。
		// 好处: ①容忍单条时间线失败(冗余顶上) ②凑够目标数后早停取消富余分支省算力
		//       ③复用已实现却从未被调用的 FanOutFirstN 原语 (design/08 §2 #9)。
		targetTL := blueprint.TimelineCount
		bufferTL := targetTL + 1
		para := we.effectiveParallel()

		var tlMu sync.Mutex
		tlNarr := map[int]string{}
		tlEmerg := map[int][]string{}

		branches := map[string]swarm_intel.BranchFunc{}
		for tlID := 1; tlID <= bufferTL; tlID++ {
			id := tlID
			tlObjective := fmt.Sprintf("%s\n\n## 时间线 #%d 演化指令\n分歧策略: %s\n请模拟角色在此世界中 %d 轮互动, 产生独特的故事走向。时间线 #%d 应尝试与其他时间线产生差异化的叙事路径。",
				baseObjective, id, blueprint.DivergenceStrategy, blueprint.Rounds, id)
			branches[fmt.Sprintf("timeline-%d", id)] = func(bctx context.Context) (string, error) {
				notify(fmt.Sprintf("  🔄 时间线 #%d: 群体智能社会模拟启动...", id))
				simCfg := swarm_intel.SimulationConfig{Mode: "social", Agents: len(soulCards), Rounds: blueprint.Rounds}
				simResult, err := engine.Simulate(bctx, team.ChatID, tlObjective, simCfg)
				if err != nil {
					notify(fmt.Sprintf("  ⚠️ 时间线 #%d 模拟失败: %s", id, err))
					return "", err
				}
				narrative := formatSimulationAsNarrative(simResult, id)
				tlMu.Lock()
				tlNarr[id] = narrative
				tlEmerg[id] = simResult.Emergent
				tlMu.Unlock()
				notify(fmt.Sprintf("  ✅ 时间线 #%d 完成 (%d 个场景, %d 字, %d 条涌现行为)",
					id, len(simResult.Scenarios), len(narrative), len(simResult.Emergent)))
				return narrative, nil
			}
		}

		foCfg := swarm_intel.DefaultFanOutConfig()
		foCfg.MaxConcurrency = para
		// 时间线社会模拟较慢, 且 Simulate 内部自带预算。让 FanOut 超时**非绑定**(不比 Simulate 内部预算更紧),
		// 保持原 Phase B "由 ctx / Simulate 自身预算治理" 的语义, 避免默认 3min 分支超时误杀慢时间线。
		foTimeout := 30 * time.Minute
		if dl, ok := ctx.Deadline(); ok {
			foTimeout = time.Until(dl)
		}
		foCfg.TotalTimeout = foTimeout
		foCfg.PerBranchTimeout = foTimeout
		foResults := swarm_intel.FanOutFirstN(ctx, foCfg, branches, targetTL)

		succeeded := map[int]bool{}
		for _, r := range foResults {
			if r.Error == nil {
				var idn int
				fmt.Sscanf(r.ID, "timeline-%d", &idn)
				if idn > 0 {
					succeeded[idn] = true
				}
			}
		}

		// 按 id 升序收集前 targetTL 条成功时间线, 位置即 Predict 的 "时间线 #i"
		var timelineNarratives []string
		var timelineEmergent [][]string
		for idn := 1; idn <= bufferTL && len(timelineNarratives) < targetTL; idn++ {
			if !succeeded[idn] {
				continue
			}
			tlMu.Lock()
			nar := tlNarr[idn]
			emerg := tlEmerg[idn]
			tlMu.Unlock()
			sr := StageResult{Name: fmt.Sprintf("timeline-%d", idn), Role: "swarm-intel-simulate", Status: TaskCompleted, Output: nar}
			allResults = append(allResults, sr)
			timelineNarratives = append(timelineNarratives, nar)
			timelineEmergent = append(timelineEmergent, emerg)
			if team.Blackboard != nil {
				team.Blackboard.Write(fmt.Sprintf("timeline-%d-result", idn), SummarizeOldOutput(nar, 3000), "swarm-intel", "timeline-result")
			}
			if we.evolution != nil {
				we.evolution.RecordTrajectory(Trajectory{
					StageName: fmt.Sprintf("timeline-%d-simulate", idn),
					Role:      "swarm-intel",
					Input:     fmt.Sprintf("timeline-%d", idn),
					Output:    SummarizeOldOutput(nar, 2000),
					Success:   true,
				})
			}
		}

		if len(timelineNarratives) == 0 {
			return allResults, fmt.Errorf("所有时间线均失败")
		}
		notify(fmt.Sprintf("  🎲 时间线生成完成: %d/%d 条成功 (FanOutFirstN 冗余 %d)", len(timelineNarratives), targetTL, bufferTL))

		if team.dataDir != "" {
			for i, nar := range timelineNarratives {
				os.WriteFile(filepath.Join(team.dataDir, fmt.Sprintf("timeline_%02d.md", i+1)), []byte(nar), 0644)
			}
		}

		// ═══════════════════════════════════════════════════════════
		// Phase C: 命运审判 — 使用 swarm_intel.Engine.Predict()
		// 多分析师辩论 + 贝叶斯融合 + 拜占庭修剪 + 保形校准
		// ═══════════════════════════════════════════════════════════
		notify("⚖️ **Phase C**: 命运审判 (swarm_intel.Engine.Predict — 多分析师辩论+融合)")

		predictObjective := buildPredictObjective(objective, prevResults, timelineNarratives, timelineEmergent)

		predictResult, predictErr := engine.Predict(ctx, team.ChatID, predictObjective)

		srPredict := StageResult{Name: "predict-judge", Role: "swarm-intel-predict"}
		bestTLIdx := 0

		if predictErr != nil {
			srPredict.Status = TaskFailed
			srPredict.Error = predictErr.Error()
			notify(fmt.Sprintf("  ⚠️ 群体智能预测评估失败: %s, 回退到单 Agent 评审", predictErr))

			judgePrompt := buildJudgePrompt(objective, prevResults, timelineNarratives)
			srFallback := we.runAgent(ctx, "story-judge", judgePrompt, team)
			if srFallback.Status == TaskCompleted {
				bestTLIdx = parseBestTimeline(srFallback.Output, len(timelineNarratives))
				srPredict.Status = TaskCompleted
				srPredict.Output = srFallback.Output
				srPredict.Error = ""
			}
		} else {
			evaluation := formatPredictAsEvaluation(predictResult, timelineNarratives)
			srPredict.Status = TaskCompleted
			srPredict.Output = evaluation

			var winOutcome string
			bestTLIdx, winOutcome = selectBestFromPrediction(predictResult, len(timelineNarratives))
			notify(fmt.Sprintf("  🏆 群体智能共识度: %.0f%%  辩论轮数: %d  最佳时间线: #%d",
				predictResult.Consensus*100, predictResult.Rounds, bestTLIdx+1))

			// #5/#6/#7: 事后把"胜选结局"反馈引擎, 驱动跨小说学习 (Brier + 保形校准 + 信任/路由, 持久化)
			if winOutcome != "" {
				brier := engine.RecordOutcome(predictResult, winOutcome)
				notify(fmt.Sprintf("  🧠 跨会话学习: 记录胜选结局 Brier=%.3f (保形/信任/路由已更新并持久化)", brier))
			}

			// #3: 保留次优时间线, 供大纲吸收跨线亮点 (不再丢弃 N−1 条演化产物)
			if secondIdx := secondBestTimeline(predictResult, len(timelineNarratives), bestTLIdx); secondIdx >= 0 {
				prevResults["second-narrative"] = timelineNarratives[secondIdx]
				notify(fmt.Sprintf("  🥈 次优时间线 #%d 保留, 供大纲吸收跨线亮点", secondIdx+1))
			}
		}
		allResults = append(allResults, srPredict)

		prevResults["evaluation"] = srPredict.Output
		prevResults["best-narrative"] = timelineNarratives[bestTLIdx]

		if team.dataDir != "" {
			os.WriteFile(filepath.Join(team.dataDir, "EVALUATION.md"), []byte(srPredict.Output), 0644)
		}

		we.savePhaseCheckpoints([]StageResult{
			{Name: "best-narrative", Role: "swarm-intel", Status: TaskCompleted, Output: prevResults["best-narrative"]},
			{Name: "evaluation", Role: "swarm-intel-predict", Status: TaskCompleted, Output: srPredict.Output},
		})

		if we.evolution != nil {
			traj := Trajectory{
				StageName: "swarm-evolution-complete",
				Role:      "swarm-intel",
				Input:     objective,
				Output:    SummarizeOldOutput(prevResults["best-narrative"], 3000),
				Duration:  "",
				Success:   true,
			}
			we.evolution.RecordTrajectory(traj)
			// #8: 蒸馏本次群体演化经验入进化记忆, 供后续小说的 Simulate/Predict 检索复用
			we.evolution.LearnFromStage(traj)
		}
	}

	// ═══════════════════════════════════════════════════════════════
	// Phase D: 大纲设计 — 复用 v2 的 outline-architect
	// ═══════════════════════════════════════════════════════════════
	prevResults["story-plan"] = SummarizeOldOutput(prevResults["best-narrative"], 6000)
	prevResults["world-build"] = prevResults["world-forge"]
	prevResults["char-design"] = prevResults["soul-forge"]

	if _, ok := prevResults["outline-design"]; !ok {
		notify("📋 **Phase D**: 大纲设计 (基于最佳时间线, 层次化展开章节)")

		stage := findStage(wf, "outline-design")
		if stage != nil {
			sr := we.runNovelStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["outline-design"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			} else {
				return allResults, fmt.Errorf("大纲设计失败: %s", sr.Error)
			}
		}
	}

	totalChapters := parseChapterCount(prevResults["outline-design"])
	if totalChapters < 1 {
		totalChapters = 5
	}
	if totalChapters > 30 {
		totalChapters = 30
	}
	notify(fmt.Sprintf("📝 大纲规划: %d 章", totalChapters))

	// ═══════════════════════════════════════════════════════════════
	// Phase E: 章节写作循环 — 复用 v2 的 novelist↔editor 对抗
	// ═══════════════════════════════════════════════════════════════
	notify(fmt.Sprintf("✍️ **Phase E**: 章节写作 (共 %d 章, novelist↔editor 对抗循环)", totalChapters))

	var allChapters []string
	chapterStartIdx := 0

	for i := 1; i <= totalChapters; i++ {
		key := fmt.Sprintf("chapter-%d", i)
		if content, ok := prevResults[key]; ok {
			allChapters = append(allChapters, content)
			chapterStartIdx = i
		} else {
			break
		}
	}

	for chapterNum := chapterStartIdx + 1; chapterNum <= totalChapters; chapterNum++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		notify(fmt.Sprintf("  📖 第 %d/%d 章写作中...", chapterNum, totalChapters))

		var evoHint string
		if we.evolution != nil {
			exps := we.evolution.RetrieveFor("novelist", objective, 3)
			if len(exps) > 0 {
				evoHint = FormatExperiencesForPrompt(exps)
			}
		}

		chapterContext := buildNovelContext(prevResults, allChapters, chapterNum, totalChapters)

		terminator := NewAdaptiveTerminator(1, 3)
		var bestChapter string
		var bestScore float64

		for round := 1; ; round++ {
			writePrompt := buildChapterWritePrompt(objective, prevResults, chapterContext, chapterNum, totalChapters, round)
			if evoHint != "" {
				writePrompt = evoHint + "\n\n" + writePrompt
			}
			if feedback, ok := prevResults["chapter-feedback"]; ok && round > 1 {
				writePrompt += "\n\n### 编辑反馈 (必须修改):\n" + feedback
			}

			sr := we.runAgent(ctx, "novelist", writePrompt, team)
			sr.Name = fmt.Sprintf("chapter-%d-write-round%d", chapterNum, round)
			allResults = append(allResults, sr)

			if sr.Status != TaskCompleted {
				if bestChapter != "" {
					notify("  ⚠️ 写作失败, 使用最佳历史版本")
				}
				break
			}

			chapterOutput := sr.Output
			prevResults["chapter-write"] = chapterOutput

			reviewPrompt := buildChapterReviewPrompt(objective, prevResults, chapterOutput, chapterNum)
			srReview := we.runAgent(ctx, "novel-editor", reviewPrompt, team)
			srReview.Name = fmt.Sprintf("chapter-%d-review-round%d", chapterNum, round)
			allResults = append(allResults, srReview)

			if srReview.Status != TaskCompleted {
				bestChapter = chapterOutput
				break
			}

			score := parseNovelScore(srReview.Output)
			terminator.RecordRoundOutput(round, score, chapterOutput)

			if score.AvgScore() > bestScore {
				bestScore = score.AvgScore()
				bestChapter = chapterOutput
			}

			decision := terminator.ShouldTerminate(round, score)
			if decision.BestOutput != "" {
				bestChapter = decision.BestOutput
			}

			if decision.ShouldStop {
				notify(fmt.Sprintf("  ✅ 第 %d 章完成 (第%d轮, 均分%.1f)", chapterNum, round, score.AvgScore()))
				break
			}

			prevResults["chapter-feedback"] = score.Feedback
			notify(fmt.Sprintf("  🔄 第 %d 章第 %d 轮: 均分%.1f, 继续优化", chapterNum, round, score.AvgScore()))
		}

		if bestChapter == "" && len(allResults) > 0 {
			last := allResults[len(allResults)-1]
			if last.Status == TaskCompleted {
				bestChapter = last.Output
			}
		}

		if bestChapter != "" {
			chapterKey := fmt.Sprintf("chapter-%d", chapterNum)
			prevResults[chapterKey] = bestChapter
			allChapters = append(allChapters, bestChapter)

			we.savePhaseCheckpoints([]StageResult{{
				Name: chapterKey, Role: "novelist", Status: TaskCompleted,
				Output: bestChapter, StartedAt: time.Now(),
			}})

			if team.dataDir != "" {
				os.WriteFile(filepath.Join(team.dataDir, fmt.Sprintf("chapter_%02d.md", chapterNum)), []byte(bestChapter), 0644)
			}

			if team.Blackboard != nil {
				team.Blackboard.Write(chapterKey+"-status", "completed", "novelist", "progress")
				team.Blackboard.Write(chapterKey+"-summary", SummarizeOldOutput(bestChapter, 500), "novelist", "result")
			}

			if we.evolution != nil {
				traj := Trajectory{
					StageName: chapterKey,
					Role:      "novelist",
					Input:     objective,
					Output:    SummarizeOldOutput(bestChapter, 2000),
					Duration:  "",
					Success:   true,
				}
				we.evolution.RecordTrajectory(traj)
				we.evolution.LearnFromStage(traj)
			}
		}

		delete(prevResults, "chapter-feedback")
		delete(prevResults, "chapter-write")
	}

	// ═══════════════════════════════════════════════════════════════
	// Phase F: 全书整合 — 复用 v2 的 book-assembly
	// ═══════════════════════════════════════════════════════════════
	notify("📦 **Phase F**: 全书整合 (统稿/伏笔验证/风格统一)")

	assemblyPrompt := buildBookAssemblyPrompt(objective, prevResults, allChapters)
	srAssembly := we.runAgent(ctx, "novel-editor", assemblyPrompt, team)
	srAssembly.Name = "book-assembly"

	// 先落盘完整小说 —— 这才是 novel-v3 的真正交付物 (全部章节已写好)。
	novelWritten := false
	if team.dataDir != "" && len(allChapters) > 0 {
		var novelBuilder strings.Builder
		for i, ch := range allChapters {
			novelBuilder.WriteString(fmt.Sprintf("\n\n---\n\n## 第 %d 章\n\n", i+1))
			novelBuilder.WriteString(ch)
		}
		novelFile := filepath.Join(team.dataDir, "NOVEL.md")
		if err := os.WriteFile(novelFile, []byte(novelBuilder.String()), 0644); err == nil {
			novelWritten = true
			notify(fmt.Sprintf("📎 完整小说已保存: `%s`", novelFile))
		}
	}

	// 统稿(book-assembly)只是后置的编辑点评, 不应因其失败而废掉整本已写好的书。
	// 若小说正文已落盘, 则把非瞬态失败的统稿阶段降级为"完成(带告警)", 避免 teams.go
	// 把单个统稿失败判成整队失败。瞬态 API 错误保留失败态, 以便 auto-resume 生效。
	if srAssembly.Status != TaskCompleted && novelWritten &&
		!strings.Contains(srAssembly.Error, "__API_ERROR__") && !isStageTransientError(srAssembly.Error) {
		notify("🟡 统稿点评未达门禁, 但全书正文已完成 —— 按交付成功处理 (统稿降级为告警)")
		if strings.TrimSpace(srAssembly.Output) == "" {
			srAssembly.Output = "（统稿点评阶段产出不足, 已跳过; 全书正文见 NOVEL.md）"
		} else {
			srAssembly.Output = "⚠️ 统稿点评未达门禁(已降级, 全书正文已完成):\n\n" + srAssembly.Output
		}
		srAssembly.Status = TaskCompleted
		srAssembly.Error = ""
	}
	allResults = append(allResults, srAssembly)

	if srAssembly.Status == TaskCompleted && team.dataDir != "" && strings.TrimSpace(srAssembly.Output) != "" {
		os.WriteFile(filepath.Join(team.dataDir, "ASSEMBLY_REPORT.md"), []byte(srAssembly.Output), 0644)
	}

	if we.evolution != nil {
		we.evolution.RecordTrajectory(Trajectory{
			StageName: "novel-v3-complete",
			Role:      "novel-editor",
			Input:     objective,
			Output:    "completed",
			Duration:  "",
			Success:   true,
		})
	}

	return allResults, nil
}

// ── swarm_intel 集成: 构建 Simulate 目标 ──

func buildSimulateObjective(objective string, prevResults map[string]string, characters []soulCard) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 叙事群体演化模拟\n\n## 创作需求\n%s\n\n", objective))

	if world, ok := prevResults["world-forge"]; ok {
		b.WriteString("## 世界规则\n")
		b.WriteString(SummarizeOldOutput(world, 3000))
		b.WriteString("\n\n")
	}

	if souls, ok := prevResults["soul-forge"]; ok {
		b.WriteString("## 角色灵魂\n")
		b.WriteString(SummarizeOldOutput(souls, 3000))
		b.WriteString("\n\n")
	}

	if catalysts, ok := prevResults["catalyst"]; ok {
		b.WriteString("## 命运催化剂\n")
		b.WriteString(SummarizeOldOutput(catalysts, 2000))
		b.WriteString("\n\n")
	}

	b.WriteString("## 模拟要求\n")
	b.WriteString("每个 Agent 代表一个角色, 基于其灵魂设定 (核心欲望/恐惧/决策风格) 自主行动。\n")
	b.WriteString("模拟角色间的互动、冲突、合作, 让故事自然涌现。\n")
	b.WriteString("关注涌现行为: 意外的角色联盟、道德困境的抉择、情感转折、命运反转。\n")
	b.WriteString("每个场景描述必须包含: 关键事件、角色内心变化、情节走向。\n")

	return b.String()
}

// buildPredictObjective 构建 Engine.Predict 的评估目标。
func buildPredictObjective(objective string, prevResults map[string]string, timelines []string, emergent [][]string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 叙事时间线评估\n\n评估以下 %d 条故事时间线, 判断哪条最适合作为最终小说的方案。\n\n", len(timelines)))
	b.WriteString(fmt.Sprintf("创作需求: %s\n\n", objective))

	if souls, ok := prevResults["soul-forge"]; ok {
		b.WriteString("## 角色设定 (评审一致性依据)\n")
		b.WriteString(SummarizeOldOutput(souls, 2000))
		b.WriteString("\n\n")
	}

	for i, tl := range timelines {
		b.WriteString(fmt.Sprintf("## 时间线 #%d\n\n", i+1))
		b.WriteString(SummarizeOldOutput(tl, 4000))
		if i < len(emergent) && len(emergent[i]) > 0 {
			b.WriteString("\n\n**涌现行为**: " + strings.Join(emergent[i], "; "))
		}
		b.WriteString("\n\n---\n\n")
	}

	b.WriteString(`## 评估维度
- 感人度: 情感共鸣是否自然深刻
- 反转跌宕度: 转折是否出人意料又合情合理
- 逻辑一致性: 因果链是否严密, 角色行为是否合理
- 角色成长: 弧光是否完整, 变化是否可信
- 叙事张力: 冲突递进, 高潮是否震撼
- 对话鲜活度: 对话是否有个性且推进叙事

请预测哪条时间线最终能产出最优质的小说。`)

	return b.String()
}

// formatSimulationAsNarrative 将 SimulationResult 格式化为叙事文本。
func formatSimulationAsNarrative(result *swarm_intel.SimulationResult, timelineID int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 时间线 #%d — 群体智能社会模拟结果\n\n", timelineID))

	for i, sc := range result.Scenarios {
		b.WriteString(fmt.Sprintf("## 场景 %d: %s (概率: %.0f%%)\n\n", i+1, sc.Name, sc.Probability*100))
		b.WriteString(sc.Description)
		b.WriteString("\n\n")
		if len(sc.KeyEvents) > 0 {
			b.WriteString("**关键事件:**\n")
			for _, ev := range sc.KeyEvents {
				b.WriteString("- " + ev + "\n")
			}
			b.WriteString("\n")
		}
	}

	if len(result.Emergent) > 0 {
		b.WriteString("## 涌现行为\n\n")
		for _, em := range result.Emergent {
			b.WriteString("- " + em + "\n")
		}
		b.WriteString("\n")
	}

	if result.Summary != "" {
		b.WriteString("## 综合分析\n\n")
		b.WriteString(result.Summary)
		b.WriteString("\n")
	}

	return b.String()
}

// formatPredictAsEvaluation 将 FusedPrediction 格式化为评估报告。
func formatPredictAsEvaluation(result *swarm_intel.FusedPrediction, timelines []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 群体智能叙事评估报告\n\n"))
	b.WriteString(fmt.Sprintf("**评估问题**: %s\n", result.Question))
	b.WriteString(fmt.Sprintf("**共识度**: %.0f%%\n", result.Consensus*100))
	b.WriteString(fmt.Sprintf("**辩论轮数**: %d\n", result.Rounds))
	b.WriteString(fmt.Sprintf("**融合方法**: %s\n\n", result.Method))

	b.WriteString("## 各时间线评估\n\n")
	for _, o := range result.Outcomes {
		b.WriteString(fmt.Sprintf("| %s | **%.1f%%** | 95%% CI [%.1f%% ~ %.1f%%] |\n",
			o.Outcome, o.Probability*100, o.Lower95*100, o.Upper95*100))
	}
	b.WriteString("\n")

	if len(result.Agents) > 0 {
		b.WriteString("## 分析师观点\n\n")
		for _, agent := range result.Agents {
			b.WriteString(fmt.Sprintf("### %s (%s)\n", agent.AgentRole, agent.AgentID))
			b.WriteString(fmt.Sprintf("- 置信度: %.0f%%\n", agent.Confidence*100))
			if agent.Rationale != "" {
				b.WriteString(fmt.Sprintf("- 推理: %s\n", agent.Rationale))
			}
			b.WriteString("\n")
		}
	}

	if result.Summary != "" {
		b.WriteString("## 综合总结\n\n")
		b.WriteString(result.Summary)
		b.WriteString("\n")
	}

	bestIdx := 0
	bestProb := 0.0
	for i, o := range result.Outcomes {
		if o.Probability > bestProb {
			bestProb = o.Probability
			bestIdx = i
		}
	}
	if bestIdx < len(timelines) {
		b.WriteString(fmt.Sprintf("\n## 推荐\n\n推荐时间线: **#%d** (概率: %.1f%%)\n", bestIdx+1, bestProb*100))
	}

	evalJSON, _ := json.Marshal(map[string]interface{}{
		"best_timeline": bestIdx + 1,
		"consensus":     result.Consensus,
		"rounds":        result.Rounds,
	})
	b.WriteString("\n```json\n")
	b.WriteString(string(evalJSON))
	b.WriteString("\n```\n")

	return b.String()
}

// selectBestFromPrediction 从 Predict 结果中选出最佳时间线索引。
//
// P0 修复: Predict 的结局空间由 LLM decompose 自建 (见 engine.decompose), 顺序/标签与
// buildPredictObjective 里 "时间线 #N" 的位置**不保证**对齐。旧实现按 Outcomes 位置取 argmax,
// 一旦 decompose 打乱顺序就会选错时间线, 唯一护栏是静默的 bestIdx>=n→0 钳制 (掩盖而非修复)。
// 现优先从获胜结局标签里解析编号回连 timeline; 解析失败再退回按位置取 argmax。
// 返回 (最佳时间线索引, 获胜结局标签)。结局标签供 RecordOutcome 做跨会话学习 (#5/#6/#7)。
func selectBestFromPrediction(result *swarm_intel.FusedPrediction, timelineCount int) (int, string) {
	// 置信度感知选择 (P1 优化 #2): 不再单看点估计 argmax, 而是用风险调整分
	//   score = Probability - 0.5 * (Upper95 - Lower95)
	// 对"点估计高但 95% CI 很宽 (评审分歧大/证据薄)"的时间线施加惩罚, 优先选稳健的走向。
	// 当引擎未产出有效 CI (宽度退化为 0) 时, 该式自动退回为纯 Probability argmax, 始终安全。
	bestIdx := 0
	bestScore := -1e9
	bestOutcome := ""
	for i, o := range result.Outcomes {
		width := o.Upper95 - o.Lower95
		if width < 0 {
			width = 0
		}
		score := o.Probability - 0.5*width
		if score > bestScore {
			bestScore = score
			bestIdx = i
			bestOutcome = o.Outcome
		}
	}
	// 按标签编号回连 (如 "时间线 #2" / "timeline 3" / "方案1" → 编号 N → 索引 N-1)
	if n, ok := parseLeadingIndex(bestOutcome); ok && n >= 1 && n <= timelineCount {
		return n - 1, bestOutcome
	}
	if bestIdx >= timelineCount {
		bestIdx = 0
	}
	return bestIdx, bestOutcome
}

// secondBestTimeline 返回置信度调整分次高、且与 excludeIdx 不同的时间线索引 (#3: 供大纲吸收跨线亮点);
// 找不到独立次优时返回 -1。best-effort, 优先按标签回连, 否则退回按位置。
func secondBestTimeline(result *swarm_intel.FusedPrediction, timelineCount, excludeIdx int) int {
	bestIdx := -1
	bestScore := -1e9
	for i, o := range result.Outcomes {
		width := o.Upper95 - o.Lower95
		if width < 0 {
			width = 0
		}
		score := o.Probability - 0.5*width
		tlIdx := i
		if n, ok := parseLeadingIndex(o.Outcome); ok && n >= 1 && n <= timelineCount {
			tlIdx = n - 1
		}
		if tlIdx >= timelineCount || tlIdx == excludeIdx {
			continue
		}
		if score > bestScore {
			bestScore = score
			bestIdx = tlIdx
		}
	}
	return bestIdx
}

// parseLeadingIndex 从标签里解析出第一个出现的十进制编号 (数字为 ASCII, 中文标签安全)。
func parseLeadingIndex(label string) (int, bool) {
	start := -1
	for i := 0; i < len(label); i++ {
		if label[i] >= '0' && label[i] <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	n := 0
	for i := start; i < len(label) && label[i] >= '0' && label[i] <= '9'; i++ {
		n = n*10 + int(label[i]-'0')
	}
	return n, true
}

// ── 数据结构 ──

type soulCard struct {
	Name string `json:"name"`
	Core string `json:"-"`
}

type evoBluePrint struct {
	TimelineCount      int    `json:"timeline_count"`
	Rounds             int    `json:"rounds"`
	DivergenceStrategy string `json:"divergence_strategy"`
}

func parseSoulCards(output string) []soulCard {
	type jsonSoul struct {
		Name       string `json:"name"`
		CoreDesire string `json:"core_desire"`
		CoreFear   string `json:"core_fear"`
	}

	start := strings.Index(output, "[")
	end := strings.LastIndex(output, "]")
	if start >= 0 && end > start {
		var souls []jsonSoul
		if json.Unmarshal([]byte(output[start:end+1]), &souls) == nil && len(souls) > 0 {
			var result []soulCard
			for _, s := range souls {
				result = append(result, soulCard{Name: s.Name, Core: output})
			}
			return result
		}
	}

	var cards []soulCard
	for _, name := range extractCharacterNames(output) {
		cards = append(cards, soulCard{Name: name, Core: output})
	}
	if len(cards) == 0 {
		cards = append(cards, soulCard{Name: "主角", Core: output})
	}
	return cards
}

func extractCharacterNames(text string) []string {
	var names []string
	seen := make(map[string]bool)

	for i := 0; i < len(text)-10; i++ {
		if text[i:i+7] == `"name":` || text[i:i+7] == `"name" :` {
			rest := text[i+7:]
			q1 := strings.Index(rest, `"`)
			if q1 < 0 {
				continue
			}
			q2 := strings.Index(rest[q1+1:], `"`)
			if q2 < 0 || q2 > 50 {
				continue
			}
			name := rest[q1+1 : q1+1+q2]
			if !seen[name] && len(name) > 0 && len(name) < 20 {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

func soulCardNames(cards []soulCard) string {
	var names []string
	for _, c := range cards {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

func parseEvoBluePrint(output string) evoBluePrint {
	bp := evoBluePrint{TimelineCount: 3, Rounds: 5, DivergenceStrategy: "random"}

	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		var parsed evoBluePrint
		if json.Unmarshal([]byte(output[start:end+1]), &parsed) == nil {
			if parsed.TimelineCount >= 2 && parsed.TimelineCount <= 5 {
				bp.TimelineCount = parsed.TimelineCount
			}
			if parsed.Rounds >= 3 && parsed.Rounds <= 10 {
				bp.Rounds = parsed.Rounds
			}
			if parsed.DivergenceStrategy != "" {
				bp.DivergenceStrategy = parsed.DivergenceStrategy
			}
		}
	}
	return bp
}

func parseBestTimeline(evaluation string, count int) int {
	type evalResult struct {
		BestTimeline int `json:"best_timeline"`
	}
	start := strings.Index(evaluation, "{")
	end := strings.LastIndex(evaluation, "}")
	if start >= 0 && end > start {
		var er evalResult
		if json.Unmarshal([]byte(evaluation[start:end+1]), &er) == nil {
			if er.BestTimeline >= 1 && er.BestTimeline <= count {
				return er.BestTimeline - 1
			}
		}
	}
	return 0
}

// ── Prompt 构建 ──

func buildJudgePrompt(objective string, prevResults map[string]string, timelines []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("你是文学评委。以下是 %d 条不同时间线演化出的故事, 请进行多维度评估。\n\n创作需求: %s\n\n", len(timelines), objective))

	if chars, ok := prevResults["soul-forge"]; ok {
		b.WriteString("## 角色灵魂设定 (审查一致性用)\n")
		b.WriteString(SummarizeOldOutput(chars, 2000))
		b.WriteString("\n\n")
	}

	for i, tl := range timelines {
		b.WriteString(fmt.Sprintf("## 时间线 #%d\n\n", i+1))
		b.WriteString(SummarizeOldOutput(tl, 4000))
		b.WriteString("\n\n---\n\n")
	}

	b.WriteString(`## 评估维度 (每项 1-10 分)

对每条时间线评分:
- emotional_impact: 感人度 — 是否触动情感共鸣, 泪点/感动是否自然不刻意
- plot_twist: 反转跌宕度 — 是否有出人意料又合情合理的转折
- logic_consistency: 逻辑一致性 — 因果链是否严密, 角色行为是否合理
- character_growth: 角色成长 — 弧光是否完整, 变化是否可信
- narrative_tension: 叙事张力 — 冲突递进, 高潮是否震撼
- dialogue_vividity: 对话鲜活度 — 对话是否有个性, 是否推进叙事

## 输出格式 (严格 JSON)

{
  "rankings": [
    {
      "timeline": 1,
      "scores": {"emotional_impact": 8, "plot_twist": 7, "logic_consistency": 9, "character_growth": 8, "narrative_tension": 7, "dialogue_vividity": 8},
      "total_score": 7.9,
      "highlight": "最突出的优点",
      "weakness": "最需要改进的点"
    }
  ],
  "best_timeline": 1,
  "recommendation": "最终选择建议和理由",
  "merge_suggestions": ["可以从其他时间线借鉴的具体段落"]
}`)

	return b.String()
}

// v3OutlinePrompt 基于群体演化最佳时间线生成精细大纲
const v3OutlinePrompt = `你是大纲架构师, 精通三幕式、英雄之旅、雪花法等叙事结构理论。

用户需求: {objective}

以下依次是: **best-narrative**(群体智能演化胜出的最佳故事时间线)、**second-narrative**(次优时间线, 可选, 从中吸收独特亮点情节融入主线)、**evaluation**(命运审判团的共识度/评分依据/融合建议)、以及世界观/角色/催化事件设定。请基于它们设计精细的章节大纲:

{prev_result}

请设计层次化大纲 (JSON 格式):

{
  "total_chapters": 10,
  "chapters": [
    {
      "number": 1,
      "title": "章节标题",
      "act": 1,
      "pov_character": "本章视角角色",
      "summary": "本章概要 (3-5句话)",
      "scenes": [
        {
          "location": "场景地点",
          "characters": ["参与角色"],
          "action": "发生什么",
          "purpose": "这个场景的叙事目的"
        }
      ],
      "tension_level": 3,
      "emotional_tone": "本章情绪基调",
      "key_events": ["关键事件"],
      "foreshadowing": [
        {"id": "foreshadow-1", "content": "伏笔内容", "payoff_chapter": 7}
      ],
      "cliffhanger": "章末悬念/钩子"
    }
  ],
  "foreshadowing_network": [
    {"id": "foreshadow-1", "plant_chapter": 1, "payoff_chapter": 7, "content": "伏笔描述"}
  ],
  "tension_curve": "描述全书张力曲线走势"
}

要求:
1. **忠实于演化方案**: 大纲必须基于演化产出的故事走向, 不要推翻核心情节
2. **保留精华**: 演化中涌现的独特情节/对话/转折 必须在大纲中体现
3. 三幕式结构: Act1(1-25%) / Act2(26-75%) / Act3(76-100%)
4. tension_level (1-10): 根据演化叙事的实际张力分布设计
5. 每章必须有明确的"叙事目的" + cliffhanger
6. 至少 3 条伏笔线
7. **采纳审判团建议**: 若 evaluation 中给出了"融合建议/待加强之处/风险点", 必须在大纲里落实 (强化被点名的薄弱环节、吸收 runner-up 时间线里被推荐的亮点情节)`

// ── Phase A Prompt 模板 ──

const worldForgerPrompt = `你是世界锻造师。根据用户需求, 锻造一个适合叙事演化的世界基底。

用户需求: {objective}

请输出世界规则文档 (Markdown):

## 世界概貌
- 时代/地点/技术水平/社会形态

## 硬规则 (绝对不可违反)
列出 3-5 条世界的基本法则, 如同物理定律一样不可打破。
每条规则说明: 规则内容 + 违反后果 + 对角色的约束

## 软规则 (通常成立, 极端情况可打破)
列出 3-5 条社会规范/道德共识, 打破它们需要付出巨大代价。

## 环境约束 (影响行动的客观条件)
- 自然环境: 气候/地形/昼夜/季节
- 社会环境: 阶层/权力/经济/信仰
- 资源限制: 什么是稀缺的, 什么是充裕的

## 冲突土壤 (世界内置的矛盾)
这个世界中哪些力量/群体/理念天然对立?
这些对立如何影响普通人的日常?

## 关键地点 (3-5个)
| 地点 | 特征 | 氛围 | 潜在冲突 |
|------|------|------|---------|

注意: 世界规则的核心目的是制造困境 — 让角色被迫做出艰难选择。`

const soulForgerPrompt = `你是灵魂铸造师。基于世界规则, 为故事铸造角色灵魂。

用户需求: {objective}

世界规则:
{prev_result}

请输出角色灵魂卡 (JSON 数组):

[
  {
    "name": "角色名",
    "role_type": "protagonist/antagonist/catalyst/mirror",
    "core_desire": "驱动一切行为的深层渴求 (不是表面目标)",
    "core_fear": "比死亡更可怕的东西",
    "moral_bottom_line": "无论如何不会跨越的线 (底线被打破=弧光高潮)",
    "decision_style": "面临困境时的思维路径",
    "speech_pattern": "独一无二的语言习惯/口头禅/语气",
    "stress_response": "压力下的行为模式",
    "blind_spot": "看不到自己的什么 (自欺/执念)",
    "relationship_map": {"另一角色": "关系本质+潜在冲突点"},
    "growth_potential": "从A到B的弧光路径 (必须有不可逆的改变)"
  }
]

设计原则:
- 主角 1-2 个, 对手 1 个, 催化角色 1-2 个
- 每个角色的欲望和恐惧必须互相矛盾 (内心撕裂)
- 角色间至少有 2 组张力关系 (利益冲突/价值观冲突/情感纠葛)
- 每个角色都有盲点 — 他们以为自己要什么, 实际需要什么不同
- speech_pattern 极其重要 — 决定了演化中角色对话的辨识度`

const fateWeaverPrompt = `你是命运编织师。基于世界规则和角色灵魂, 设计催化事件。

用户需求: {objective}

{prev_result}

请输出催化事件序列 (JSON):

{
  "catalyst_events": [
    {
      "id": 1,
      "name": "事件名",
      "trigger_round": 1,
      "description": "发生什么 (2-3句)",
      "irreversibility": "为什么不可逆转",
      "affected_characters": ["角色A", "角色B"],
      "dilemma": "迫使角色面对什么选择 (两难困境)",
      "chain_reaction": "触发后会连锁引发什么"
    }
  ],
  "opening_scene": "故事开始时的场景描述 (氛围/时间/地点)",
  "core_tension": "贯穿整个故事的核心张力是什么"
}

设计原则:
- 催化事件 3-5 个, 按演化轮次分布
- 第1个催化剂在第1轮触发 (打破日常)
- 中期催化剂制造反转 (颠覆已建立的秩序)
- 后期催化剂迫使角色面对终极选择 (底线考验)
- 每个催化剂必须让至少2个角色产生不同反应`

const evoArchitectPrompt = `你是演化架构师。设计群体叙事演化的运行蓝图。

用户需求: {objective}

{prev_result}

请输出演化蓝图 (JSON):

{
  "timeline_count": 3,
  "rounds": 5,
  "divergence_strategy": "random",
  "round_plan": [
    {"round": 1, "event": "催化剂#1触发", "expected_tone": "平静→震荡", "active_characters": ["角色A", "角色B"]},
    {"round": 2, "event": "角色回应+连锁", "expected_tone": "紧张上升", "active_characters": ["全部"]},
    {"round": 3, "event": "催化剂#2/反转", "expected_tone": "颠覆", "active_characters": ["全部"]},
    {"round": 4, "event": "底线考验", "expected_tone": "至暗时刻", "active_characters": ["主角", "对手"]},
    {"round": 5, "event": "终局选择", "expected_tone": "高潮→余韵", "active_characters": ["全部"]}
  ],
  "divergence_points": [
    {"round": 2, "description": "不同时间线中角色A做出不同选择"},
    {"round": 4, "description": "底线考验的结果不同: 守住/突破"}
  ],
  "success_criteria": "什么样的故事算成功 (基于用户需求)"
}

设计原则:
- timeline_count: 2-4 条 (根据故事复杂度, 不要太多以免资源浪费)
- rounds: 3-7 轮 (短篇 3-4 轮, 中篇 5-7 轮)
- 张力曲线应有呼吸感: 不是一直高潮, 要有蓄势和释放
- divergence_points: 分叉点应在有意义的选择节点, 不是随机
- 最后一轮应有收束感, 不能戛然而止`
