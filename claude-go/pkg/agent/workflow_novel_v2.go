// novel-v2 工作流: 多Agent协作长篇小说写作引擎, 支持自演进。
//
// 复用研发团队核心能力:
//   - CheckpointStore: 章节级检查点, 支持中断恢复
//   - AdaptiveTerminator: 动态对抗调整 (基于评分的自适应轮数)
//   - AgentPool.AutoScale: 动态 Agent 池大小
//   - EvolutionEngine: 跨章写作经验学习
//   - SummarizeOldOutput: 远期章节渐进式摘要
//
// 架构参考:
//   - Agents' Room (ICLR 2025): 多Agent分阶段叙事生成
//   - DOC (ACL 2023): 层次化大纲控制
//   - RecurrentGPT (ICLR 2024): 长短期记忆 + 滚动上下文
//   - ainovel-cli: Novel Harness (Go实现, 500+章检查点恢复)
//   - Save the Cat / 三幕式 / 雪花法: 小说结构理论
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"os"
)

func novelV2Workflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "novel-v2",
		Description: "小说写作团队: 世界构建→大纲设计→章节写作(对抗循环)→全书整合, 支持自演进",
		Mode:        "novel_writing",
		Rounds:      0, // 自适应 AdaptiveTerminator
		Stages: []StageDef{
			// Phase 1: 世界构建
			{Name: "story-plan", Role: "story-planner",
				Prompt: storyPlannerPrompt},
			{Name: "world-build", Role: "world-builder",
				DependsOn: []string{"story-plan"},
				Prompt:    worldBuilderPrompt},
			{Name: "char-design", Role: "char-designer",
				DependsOn: []string{"story-plan", "world-build"},
				Prompt:    charDesignerPrompt},

			// Phase 2: 大纲设计
			{Name: "outline-design", Role: "outline-architect",
				DependsOn: []string{"story-plan", "world-build", "char-design"},
				Prompt:    outlineArchitectPrompt},

			// Phase 3: 章节写作 (对抗循环, 由编排器驱动)
			{Name: "chapter-write", Role: "novelist",
				DependsOn: []string{"outline-design"},
				Prompt:    novelistPrompt},
			{Name: "chapter-review", Role: "novel-editor",
				DependsOn: []string{"chapter-write"},
				Prompt:    novelEditorPrompt},

			// Phase 4: 全书整合
			{Name: "book-assembly", Role: "novel-editor",
				DependsOn: []string{"chapter-write"}},
		},
	}
}

// executeNovelWriting 小说写作专用编排器。
// 四阶段: 世界构建 → 大纲设计 → 章节对抗循环 → 全书整合。
func (we *WorkflowExecutor) executeNovelWriting(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	notify := func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}

	// 检查点恢复
	we.restoreCheckpoints(wf.Stages, prevResults, &allResults)
	if len(allResults) > 0 {
		notify(fmt.Sprintf("♻️ 从检查点恢复: 已恢复 %d 个阶段", len(allResults)))
	}

	// Agent Pool 动态调整
	if we.pool != nil {
		we.pool.AutoScale(6) // novel-v2 需要 6 角色
		log.Printf("[novel-v2] AgentPool 已调整")
	}

	// ── Phase 1: 世界构建 ──
	phase1Stages := []string{"story-plan", "world-build", "char-design"}
	for i, stageName := range phase1Stages {
		if _, ok := prevResults[stageName]; ok {
			continue
		}
		if i == 0 {
			notify("📖 **Phase 1/4**: 世界构建 (故事策划 → 世界观 → 角色设计)")
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
			return allResults, fmt.Errorf("世界构建阶段 %s 失败: %s", stageName, sr.Error)
		}
	}

	// ── Phase 2: 大纲设计 ──
	if _, ok := prevResults["outline-design"]; !ok {
		notify("📋 **Phase 2/4**: 大纲设计 (层次化展开: 卷→章→场景)")

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

	// 解析章节数
	totalChapters := parseChapterCount(prevResults["outline-design"])
	if totalChapters < 1 {
		totalChapters = 5
	}
	if totalChapters > 30 {
		totalChapters = 30
	}
	notify(fmt.Sprintf("📝 大纲规划: %d 章", totalChapters))

	// ── Phase 3: 章节写作循环 ──
	notify(fmt.Sprintf("✍️ **Phase 3/4**: 章节写作 (共 %d 章, 每章 novelist↔editor 对抗)", totalChapters))

	var allChapters []string
	chapterStartIdx := 0

	// 检查已恢复的章节
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

		// 构建滚动上下文
		chapterContext := buildNovelContext(prevResults, allChapters, chapterNum, totalChapters)

		// novelist↔editor 对抗循环
		terminator := NewAdaptiveTerminator(1, 3) // 每章最多3轮
		var bestChapter string
		var bestScore float64

		for round := 1; ; round++ {
			// novelist 写作
			writePrompt := buildChapterWritePrompt(objective, prevResults, chapterContext, chapterNum, totalChapters, round)
			if feedback, ok := prevResults["chapter-feedback"]; ok && round > 1 {
				writePrompt += "\n\n### 编辑反馈 (必须修改):\n" + feedback
			}

			writeStage := StageDef{
				Name: fmt.Sprintf("chapter-%d-write-round%d", chapterNum, round),
				Role: "novelist",
			}
			sr := we.runAgent(ctx, writeStage.Role, writePrompt, team)
			sr.Name = writeStage.Name
			allResults = append(allResults, sr)

			if sr.Status != TaskCompleted {
				if bestChapter != "" {
					notify("  ⚠️ 写作失败, 使用最佳历史版本")
				}
				break
			}

			chapterOutput := sr.Output
			prevResults["chapter-write"] = chapterOutput

			// editor 审查
			reviewPrompt := buildChapterReviewPrompt(objective, prevResults, chapterOutput, chapterNum)
			reviewStage := StageDef{
				Name: fmt.Sprintf("chapter-%d-review-round%d", chapterNum, round),
				Role: "novel-editor",
			}
			srReview := we.runAgent(ctx, reviewStage.Role, reviewPrompt, team)
			srReview.Name = reviewStage.Name
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

			// 保存章节到磁盘
			if team.dataDir != "" {
				chapterFile := filepath.Join(team.dataDir, fmt.Sprintf("chapter_%02d.md", chapterNum))
				os.WriteFile(chapterFile, []byte(bestChapter), 0644)
			}
		}

		// 清理临时反馈
		delete(prevResults, "chapter-feedback")
		delete(prevResults, "chapter-write")
	}

	// ── Phase 4: 全书整合 ──
	notify("📦 **Phase 4/4**: 全书整合 (统稿/伏笔验证/风格统一)")

	assemblyPrompt := buildBookAssemblyPrompt(objective, prevResults, allChapters)
	assemblyStage := StageDef{Name: "book-assembly", Role: "novel-editor"}
	srAssembly := we.runAgent(ctx, assemblyStage.Role, assemblyPrompt, team)
	srAssembly.Name = "book-assembly"
	allResults = append(allResults, srAssembly)

	// 保存完整小说到磁盘
	if team.dataDir != "" && len(allChapters) > 0 {
		var novelBuilder strings.Builder
		for i, ch := range allChapters {
			novelBuilder.WriteString(fmt.Sprintf("\n\n---\n\n## 第 %d 章\n\n", i+1))
			novelBuilder.WriteString(ch)
		}
		novelFile := filepath.Join(team.dataDir, "NOVEL.md")
		os.WriteFile(novelFile, []byte(novelBuilder.String()), 0644)
		notify(fmt.Sprintf("📎 完整小说已保存: `%s`", novelFile))
	}

	if srAssembly.Status == TaskCompleted && team.dataDir != "" {
		reportFile := filepath.Join(team.dataDir, "REPORT.md")
		os.WriteFile(reportFile, []byte(srAssembly.Output), 0644)
		notify(fmt.Sprintf("📎 审阅报告已保存: `%s`", reportFile))
	}

	return allResults, nil
}

// runNovelStage 使用 StageDef.Prompt 模板直接构建 prompt 并执行。
// 绕过 RoleRegistry.MergedPrompt (避免简短 SystemPrompt 覆盖详细 StageDef.Prompt)。
func (we *WorkflowExecutor) runNovelStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	var prevOutput strings.Builder
	for _, dep := range stage.DependsOn {
		if r, ok := prevResults[dep]; ok {
			truncated := r
			if len(truncated) > 6000 {
				truncated = truncated[:6000] + "\n...(已截断, 完整输出请参阅黑板)"
			}
			prevOutput.WriteString(fmt.Sprintf("### Output from %s:\n%s\n\n", dep, truncated))
		}
	}

	prompt := stage.Prompt
	prompt = strings.ReplaceAll(prompt, "{objective}", objective)
	prompt = strings.ReplaceAll(prompt, "{prev_result}", prevOutput.String())

	we.notify(we.chatID, fmt.Sprintf("🔄 阶段 **%s** (%s) 开始执行...", stage.Name, stage.Role))

	sr := we.runAgent(ctx, stage.Role, prompt, team)
	sr.Name = stage.Name
	sr.Role = stage.Role

	if team.Blackboard != nil {
		if sr.Status == TaskCompleted {
			team.Blackboard.Write(stage.Name+"-result", sr.Output, stage.Role, "result")
			team.Blackboard.Write(stage.Name+"-status", "completed", "system", "progress")
		} else {
			team.Blackboard.Write(stage.Name+"-status", "failed: "+sr.Error, "system", "progress")
		}
	}

	return sr
}

// ── 辅助函数 ──

func parseChapterCount(outlineOutput string) int {
	lower := strings.ToLower(outlineOutput)

	type outlineJSON struct {
		Chapters []json.RawMessage `json:"chapters"`
		Outline  []json.RawMessage `json:"outline"`
	}

	start := strings.Index(outlineOutput, "{")
	end := strings.LastIndex(outlineOutput, "}")
	if start >= 0 && end > start {
		var o outlineJSON
		if json.Unmarshal([]byte(outlineOutput[start:end+1]), &o) == nil {
			if len(o.Chapters) > 0 {
				return len(o.Chapters)
			}
			if len(o.Outline) > 0 {
				return len(o.Outline)
			}
		}
	}

	// 回退: 统计 "第X章" 出现次数
	count := 0
	for _, marker := range []string{"第", "chapter"} {
		count += strings.Count(lower, marker)
	}
	if count > 2 {
		return count
	}

	return 5
}

func buildNovelContext(prevResults map[string]string, chapters []string, currentChapter, totalChapters int) string {
	var ctx strings.Builder

	// 全局上下文 (始终注入)
	if plan, ok := prevResults["story-plan"]; ok {
		ctx.WriteString("### 故事蓝图:\n")
		ctx.WriteString(SummarizeOldOutput(plan, 2000))
		ctx.WriteString("\n\n")
	}
	if world, ok := prevResults["world-build"]; ok {
		ctx.WriteString("### 世界观设定:\n")
		ctx.WriteString(SummarizeOldOutput(world, 2000))
		ctx.WriteString("\n\n")
	}
	if chars, ok := prevResults["char-design"]; ok {
		ctx.WriteString("### 角色设定:\n")
		ctx.WriteString(SummarizeOldOutput(chars, 3000))
		ctx.WriteString("\n\n")
	}
	if outline, ok := prevResults["outline-design"]; ok {
		ctx.WriteString("### 大纲:\n")
		ctx.WriteString(SummarizeOldOutput(outline, 3000))
		ctx.WriteString("\n\n")
	}

	// 滚动上下文: 近2章全文, 更早章节摘要
	if len(chapters) > 0 {
		ctx.WriteString("### 已完成章节:\n")
		for i, ch := range chapters {
			chapNum := i + 1
			if chapNum >= currentChapter-2 {
				// 近2章: 全文 (截断到4000)
				ctx.WriteString(fmt.Sprintf("\n#### 第 %d 章 (全文):\n", chapNum))
				if len(ch) > 4000 {
					ctx.WriteString(ch[:4000])
					ctx.WriteString("\n...(截断)")
				} else {
					ctx.WriteString(ch)
				}
			} else {
				// 更早: 摘要
				ctx.WriteString(fmt.Sprintf("\n#### 第 %d 章 (摘要):\n", chapNum))
				ctx.WriteString(SummarizeOldOutput(ch, 500))
			}
			ctx.WriteString("\n")
		}
	}

	return ctx.String()
}

func buildChapterWritePrompt(objective string, prevResults map[string]string, chapterContext string, chapterNum, totalChapters, round int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`你是一位才华横溢的小说家。现在写第 %d 章 (共 %d 章)。

用户创作需求: %s

`, chapterNum, totalChapters, objective))

	b.WriteString(chapterContext)

	b.WriteString(fmt.Sprintf(`

## 写作要求

1. **字数**: 2000-3000字
2. **视角一致**: 保持与前文一致的叙事视角
3. **角色忠实**: 严格遵循角色设定卡的性格、说话方式、动机
4. **情节推进**: 本章必须推进至少一个核心冲突或子线索
5. **开头**: 与上一章自然衔接 (如果是第一章, 用引人入胜的开场)
6. **结尾**: 留下悬念或情感钩子, 驱动读者继续
7. **五感描写**: 每章至少包含 2 处五感描写 (视/听/触/嗅/味)
8. **对话与叙事比例**: 对话占 30-40%%, 叙事和描写占 60-70%%
9. **冲突**: 每章至少一个微冲突 (可以是内心、人际或外部)
10. **伏笔**: 如果大纲标注了本章需要埋入伏笔, 务必自然植入

## 输出格式

直接输出小说正文。开头写章节标题 (如 "第%d章 标题")。
不要写"我是小说家"之类的角色声明, 直接输出小说内容。
`, chapterNum))

	if round > 1 {
		b.WriteString("\n⚠️ 这是修改稿, 请在前一版基础上改进, 不要完全重写。")
	}

	return b.String()
}

func buildChapterReviewPrompt(objective string, prevResults map[string]string, chapterContent string, chapterNum int) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf(`你是资深文学编辑, 正在审查第 %d 章。

创作需求: %s

`, chapterNum, objective))

	if chars, ok := prevResults["char-design"]; ok {
		b.WriteString("### 角色设定 (审查一致性用):\n")
		b.WriteString(SummarizeOldOutput(chars, 2000))
		b.WriteString("\n\n")
	}
	if outline, ok := prevResults["outline-design"]; ok {
		b.WriteString("### 大纲 (审查情节对齐用):\n")
		b.WriteString(SummarizeOldOutput(outline, 2000))
		b.WriteString("\n\n")
	}

	b.WriteString(fmt.Sprintf(`### 第 %d 章正文:
%s

## 审查维度 (每项 1-10 分)

1. **narrative_quality**: 叙事技巧、文笔流畅度、修辞手法
2. **character_consistency**: 角色言行与设定卡一致性
3. **plot_coherence**: 情节逻辑、因果关系、大纲对齐
4. **pacing**: 节奏控制: 张弛有度, 不拖沓不仓促
5. **dialogue_quality**: 对话个性化、自然度、推进情节
6. **world_consistency**: 世界观设定一致性
7. **emotional_arc**: 情感张力曲线是否符合预期

## 输出格式 (严格 JSON, 不要多余文字)

{"narrative_quality": 8, "character_consistency": 7, "plot_coherence": 8, "pacing": 7, "dialogue_quality": 8, "world_consistency": 8, "emotional_arc": 7, "pass": true, "feedback": "具体修改建议..."}

通过标准: 所有维度 >= 6 且加权平均 >= 7。
如果不通过, 在 feedback 中给出具体修改指导 (引用原文+修改建议)。`, chapterNum, chapterContent))

	return b.String()
}

func buildBookAssemblyPrompt(objective string, prevResults map[string]string, chapters []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`你是资深文学编辑, 负责全书最终整合审阅。

创作需求: %s
总章数: %d

`, objective, len(chapters)))

	// 注入世界观和角色设定
	if world, ok := prevResults["world-build"]; ok {
		b.WriteString("### 世界观:\n")
		b.WriteString(SummarizeOldOutput(world, 1500))
		b.WriteString("\n\n")
	}
	if chars, ok := prevResults["char-design"]; ok {
		b.WriteString("### 角色设定:\n")
		b.WriteString(SummarizeOldOutput(chars, 2000))
		b.WriteString("\n\n")
	}

	// 每章摘要
	b.WriteString("### 各章内容摘要:\n")
	for i, ch := range chapters {
		b.WriteString(fmt.Sprintf("\n**第 %d 章:**\n", i+1))
		b.WriteString(SummarizeOldOutput(ch, 800))
		b.WriteString("\n")
	}

	b.WriteString(`

## 整合审阅任务

请输出全书审阅报告:

1. **整体评价**: 故事完成度、文学质量总评 (1-10)
2. **角色一致性审查**: 每个主要角色在全书中的表现是否一致
3. **伏笔回收验证**: 列出所有伏笔, 标注 ✅已回收 / ❌未回收
4. **节奏分析**: 各章张力曲线是否合理, 高潮安排是否恰当
5. **风格统一性**: 全书语言风格是否统一, 有无明显断裂
6. **章节间过渡**: 各章衔接是否流畅
7. **修改建议**: 具体需要修改的地方 (章节号+具体问题+修改方向)
8. **亮点**: 全书写得最好的 2-3 处

## 最终评分

| 维度 | 评分 |
|------|------|
| 故事性 | ?/10 |
| 文学性 | ?/10 |
| 角色塑造 | ?/10 |
| 世界构建 | ?/10 |
| 节奏控制 | ?/10 |
| 总评 | ?/10 |
`)

	return b.String()
}

// parseNovelScore 从审查 JSON 中解析小说评分
func parseNovelScore(output string) EvalScore {
	type novelReview struct {
		NarrativeQuality     float64 `json:"narrative_quality"`
		CharacterConsistency float64 `json:"character_consistency"`
		PlotCoherence        float64 `json:"plot_coherence"`
		Pacing               float64 `json:"pacing"`
		DialogueQuality      float64 `json:"dialogue_quality"`
		WorldConsistency     float64 `json:"world_consistency"`
		EmotionalArc         float64 `json:"emotional_arc"`
		Pass                 bool    `json:"pass"`
		Feedback             string  `json:"feedback"`
	}

	var review novelReview
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(output[start:end+1]), &review); err == nil {
			// 映射到 EvalScore (复用现有 AdaptiveTerminator)
			// Correctness  ← 加权: narrative(0.2) + dialogue(0.1)
			// Completeness ← 加权: plot(0.15) + emotional(0.1)
			// Security     ← character_consistency(0.2) + world_consistency(0.1)
			// CodeQuality  ← pacing(0.15)
			narrative := review.NarrativeQuality*0.2 + review.DialogueQuality*0.1
			plot := review.PlotCoherence*0.15 + review.EmotionalArc*0.1
			consistency := review.CharacterConsistency*0.2 + review.WorldConsistency*0.1
			pacing := review.Pacing * 0.15

			avg := (narrative + plot + consistency + pacing) / 0.9 // 归一化到 10 分
			return EvalScore{
				Correctness:  avg,
				Completeness: avg,
				Security:     avg,
				CodeQuality:  avg,
				Pass:         review.Pass,
				Feedback:     review.Feedback,
			}
		}
	}

	// 解析失败
	lower := strings.ToLower(output)
	pass := strings.Contains(lower, `"pass": true`) || strings.Contains(lower, `"pass":true`)
	return EvalScore{
		Correctness:  6,
		Completeness: 6,
		Security:     6,
		CodeQuality:  6,
		Pass:         pass,
		Feedback:     output,
	}
}

// ── Prompt 定义 ──

const storyPlannerPrompt = `你是资深故事策划师, 精通各类文学体裁和商业小说创作。

用户需求: {objective}

请输出故事蓝图 (JSON 格式):
{
  "title": "小说暂定名",
  "genre": "题材 (科幻/言情/悬疑/奇幻/现实/武侠/都市等)",
  "subgenre": "细分题材",
  "theme": "核心主题 (一句话)",
  "premise": "故事前提 (3-5句话概括)",
  "target_audience": "目标读者画像",
  "tone": "基调 (轻松/沉重/幽默/严肃/温暖/黑暗)",
  "narrative_pov": "叙事视角 (第一人称/第三人称限制/第三人称全知)",
  "estimated_chapters": 10,
  "words_per_chapter": 2500,
  "core_conflict": "核心冲突描述",
  "three_act_structure": {
    "act1_setup": "第一幕: 建置 (占25%)",
    "act2_confrontation": "第二幕: 对抗 (占50%)",
    "act3_resolution": "第三幕: 解决 (占25%)"
  },
  "key_plot_points": [
    "开场钩子",
    "激励事件",
    "第一转折点",
    "中点逆转",
    "至暗时刻",
    "高潮",
    "结局"
  ]
}

注意:
- 如果用户没有指定题材, 根据描述智能推断
- 章节数根据故事复杂度合理规划 (5-20章)
- 每章字数建议 2000-3000字
- 三幕式结构必须包含清晰的转折点`

const worldBuilderPrompt = `你是世界观构建大师, 擅长为各类小说构建沉浸式世界设定。

用户需求: {objective}

故事蓝图:
{prev_result}

请构建完整的世界观设定文档:

## 时空背景
- 时代: 年代/纪元/时间线
- 地理: 主要场景/城市/地标
- 气候/环境特征

## 社会结构
- 政治体制/权力结构
- 阶层划分
- 经济体系
- 文化传统/禁忌

## 规则体系 (如适用)
- 魔法/科技/超能力体系及其规则
- 限制条件和代价
- 与日常生活的交集

## 日常生活
- 普通人的一天
- 交通/通讯方式
- 饮食/服饰/住居

## 关键地点 (3-5个)
| 地点 | 描述 | 氛围 | 关联角色 |
|------|------|------|---------|

## 设定红线 (不可违反的硬规则)
列出 3-5 条绝对不能打破的世界观规则。

注意: 设定要为故事服务, 不要过度展开与情节无关的细节。`

const charDesignerPrompt = `你是角色设计专家, 精通文学角色心理学和人物弧光理论。

用户需求: {objective}

故事蓝图:
{prev_result}

请设计核心角色 (JSON 数组格式):

[
  {
    "name": "角色名",
    "role_type": "protagonist/antagonist/deuteragonist/supporting",
    "age": 25,
    "appearance": "外貌特征 (3-5个关键特征)",
    "personality": {
      "traits": ["主要性格特征1", "特征2", "特征3"],
      "strengths": ["优点"],
      "flaws": ["致命弱点"],
      "mbti": "INTJ (可选)",
      "speech_style": "说话方式描述 (语气/用词习惯/口头禅)"
    },
    "backstory": "背景故事 (2-3句话)",
    "motivation": "核心动机 (驱动角色行动的根本原因)",
    "internal_conflict": "内心冲突",
    "arc": "角色弧光 (从A状态到B状态的成长轨迹)",
    "relationships": [
      {"with": "另一角色名", "type": "关系类型", "dynamic": "互动模式"}
    ],
    "secrets": ["角色秘密 (影响剧情的)"]
  }
]

要求:
- 主角 1-2 个, 反派/对手 1 个, 关键配角 2-3 个
- 每个角色必须有"致命弱点" (flaw) — 完美角色是无聊的
- 角色间关系必须包含至少一组张力关系 (对立/矛盾/误解)
- 角色弧光要与故事的三幕结构呼应
- speech_style 极其重要 — 决定了对话的个性化程度`

const outlineArchitectPrompt = `你是大纲架构师, 精通三幕式、英雄之旅、雪花法等叙事结构理论。

用户需求: {objective}

故事蓝图:
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
1. 三幕式结构: Act1(1-25%) / Act2(26-75%) / Act3(76-100%)
2. tension_level (1-10): 前几章 3-5, 中段 5-7, 高潮 9-10, 结局回落
3. 每章必须有明确的"叙事目的" (推进主线/发展角色/埋伏笔/制造冲突)
4. 至少 3 条伏笔线, 标注埋入章和回收章
5. 每章结尾必须有 cliffhanger 或情感钩子
6. 不同章节适当切换 POV 角色 (如有多主角)`

const novelistPrompt = `你是一位才华横溢的小说家, 拥有深厚的文学功底和丰富的创作经验。

创作需求: {objective}

前序产出:
{prev_result}

你的任务是写出高质量的小说章节正文。

## 写作原则
1. **Show, don't tell**: 用场景和行动展示, 而非直接陈述
2. **感官浸入**: 调动五感描写, 让读者"看到/听到/闻到/触到/尝到"
3. **对话个性化**: 每个角色的说话方式必须独特且一致
4. **节奏变化**: 动作场景用短句, 情感场景用长句, 张弛有度
5. **冰山理论**: 角色内心比表面行为更丰富, 留白让读者自行想象
6. **冲突驱动**: 每个场景至少有一层冲突 (外部或内心)

## 禁忌
- 不要写角色扮演声明 ("作为小说家, 我...")
- 不要使用网文套路化表达 ("嘴角上扬45度")
- 不要在正文中加入元评论 ("此处表现了角色的...")
- 不要使用过多形容词堆砌
- 直接输出小说正文, 不要解释你的创作意图`

const novelEditorPrompt = `你是资深文学编辑, 有二十年出版业经验, 审美独到、眼光犀利。

创作需求: {objective}

{prev_result}

请以严格的专业标准审查稿件, 输出 JSON 格式评分:

{"narrative_quality": 8, "character_consistency": 7, "plot_coherence": 8, "pacing": 7, "dialogue_quality": 8, "world_consistency": 8, "emotional_arc": 7, "pass": true, "feedback": "..."}

## 审查标准
- narrative_quality: 文笔是否优美、叙事技巧是否娴熟、修辞是否恰当
- character_consistency: 角色行为/语言/决策是否符合已建立的设定
- plot_coherence: 情节是否合逻辑、因果链是否成立、与大纲是否对齐
- pacing: 节奏是否合理、是否有拖沓或仓促
- dialogue_quality: 对话是否个性化、是否推进情节、是否自然
- world_consistency: 世界观细节是否前后一致
- emotional_arc: 情感变化是否有弧度、是否能引起共鸣

通过标准: 所有维度 >= 6, 加权平均 >= 7。
如不通过, feedback 中必须引用具体段落并给出修改方向。`
