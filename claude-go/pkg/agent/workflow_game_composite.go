// workflow_game_composite.go — 游戏研发跨团队协调工作流。
//
// 核心理念: 整合现有团队资源, 构建全新游戏团队:
//   Phase A: 游戏设计 — 游戏设计师产出 GDD
//   Phase B: 剧情演化 — 复用群体智能引擎(predict&simulate)做多线叙事衍生
//   Phase C: 美术原型 — 复用 Creative-v2 的 HTML 原型 + 对抗审查
//   Phase D: 技术开发 — 扩展研发团队: 新增 2D/3D 引擎专属角色
//   Phase E: 集成测试 — 可玩性验证 + 性能优化
//
// 参考:
//   - Genie 2 (DeepMind): 文本→可交互世界
//   - Dual-Agent PCG (arXiv:2512.10501): Actor/Critic 迭代生成
//   - novel-v3: swarm_intel.Engine 做叙事演化 (直接复用模式)
//   - creative-v2: HTML 原型对抗循环 (直接复用模式)
//   - Voyager: 代码即技能
package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/media"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

func gameCompositeWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "game",
		Description: "游戏研发整合团队: 游戏设计→群体智能剧情→创意团队美术→2D/3D引擎开发→测试优化",
		Mode:        "game_composite",
		Rounds:      0,
		Stages: []StageDef{
			// Phase A: 游戏设计
			{Name: "game-concept", Role: "game-designer",
				Prompt: gameConceptPrompt},

			// Phase B: 剧情演化 (swarm_intel)
			{Name: "narrative-evolution", Role: "game-designer",
				DependsOn: []string{"game-concept"},
				Prompt:    "群体智能剧情演化阶段 (由 executor 内部驱动)"},

			// Phase C: 美术原型 (creative-v2)
			{Name: "art-plan", Role: "creative-planner",
				DependsOn: []string{"game-concept"},
				Prompt:    gameArtPlanPrompt},
			{Name: "art-prototype", Role: "html-developer",
				DependsOn: []string{"art-plan"},
				Prompt:    gameArtPrototypePrompt},
			{Name: "art-review", Role: "art-director",
				DependsOn: []string{"art-prototype"},
				Prompt:    gameArtReviewPrompt},

			// Phase D: 技术开发 (扩展研发团队)
			{Name: "tech-architecture", Role: "game-architect",
				DependsOn: []string{"game-concept", "narrative-evolution"}},
			{Name: "engine-dev", Role: "game-engine-dev",
				DependsOn: []string{"tech-architecture"}, Parallel: true},
			{Name: "logic-dev", Role: "game-logic-dev",
				DependsOn: []string{"tech-architecture"}, Parallel: true},
			{Name: "level-design", Role: "level-designer",
				DependsOn: []string{"tech-architecture", "narrative-evolution"}, Parallel: true},

			// Phase E: 集成测试 + 优化
			{Name: "game-integration", Role: "game-tester",
				DependsOn: []string{"engine-dev", "logic-dev", "level-design", "art-prototype"}},
			{Name: "game-optimization", Role: "game-optimizer",
				DependsOn: []string{"game-integration"}},
		},
	}
}

// executeGameComposite 游戏研发跨团队协调编排器。
// 在单一 executor 中串联: 游戏设计 → swarm_intel 群体智能 → creative-v2 美术 → 扩展 dev → 测试。
func (we *WorkflowExecutor) executeGameComposite(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
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
		log.Printf("[game-composite] AgentPool 已调整")
	}

	// ═══════════════════════════════════════════════════════════
	// Phase A: 游戏设计
	// ═══════════════════════════════════════════════════════════
	notify("🎮 **Phase A**: 游戏概念设计")

	if _, ok := prevResults["game-concept"]; !ok {
		stage := findStage(wf, "game-concept")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["game-concept"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// ═══════════════════════════════════════════════════════════
	// Phase B: 剧情演化 (复用 swarm_intel.Engine, 参照 novel-v3 模式)
	// ═══════════════════════════════════════════════════════════
	if _, ok := prevResults["narrative-evolution"]; !ok {
		notify("⚡ **Phase B**: 剧情演化 (群体智能引擎: 多线模拟→辩论→融合)")

		if we.llm != nil {
			siCfg := swarm_intel.DefaultConfig()
			siCfg.Notify = func(_, msg string) {
				notify("  [群体智能] " + msg)
			}
			engine := swarm_intel.NewEngine(we.llm, siCfg)

			gameConcept := prevResults["game-concept"]
			narrativeObjective := fmt.Sprintf(
				"基于以下游戏设计，生成多条剧情线并评估最佳方案:\n\n%s\n\n请模拟不同的故事走向、角色命运和世界观演化。",
				truncateForPrompt(gameConcept, 3000))

			// Phase B.1: 多线模拟 (social simulation)
			notify("  🌐 多线剧情模拟启动...")
			var narratives []string
			var emergents []string

			simConfigs := []struct {
				name   string
				agents int
				rounds int
			}{
				{"主线剧情", 5, 3},
				{"支线剧情", 4, 2},
				{"隐藏剧情", 3, 2},
			}

			var wg sync.WaitGroup
			var mu sync.Mutex
			sem := make(chan struct{}, 3)

			for _, sc := range simConfigs {
				sc := sc
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					simCfg := swarm_intel.SimulationConfig{
						Mode:   "social",
						Agents: sc.agents,
						Rounds: sc.rounds,
					}
					simResult, err := engine.Simulate(ctx, team.ChatID,
						fmt.Sprintf("[%s] %s", sc.name, narrativeObjective), simCfg)
					if err != nil {
						notify(fmt.Sprintf("  ⚠️ %s 模拟失败: %v", sc.name, err))
						return
					}

					narrative := formatGameNarrative(simResult, sc.name)
					mu.Lock()
					narratives = append(narratives, narrative)
					for _, s := range simResult.Scenarios {
						emergents = append(emergents, s.Description)
					}
					mu.Unlock()
					notify(fmt.Sprintf("  ✅ %s: %d 个场景", sc.name, len(simResult.Scenarios)))
				}()
			}
			wg.Wait()

			// Phase B.2: 群体智能评估与融合 (Predict)
			if len(narratives) > 0 {
				notify("  ⚖️ 群体智能剧情评估 (多分析师辩论+融合)...")
				predictObj := fmt.Sprintf(
					"评估以下游戏剧情方案, 选出最佳组合:\n\n游戏设计:\n%s\n\n剧情方案:\n%s\n\n涌现行为:\n%s",
					truncateForPrompt(gameConcept, 1500),
					strings.Join(narratives, "\n---\n"),
					strings.Join(emergents, "\n"))

				predictResult, err := engine.Predict(ctx, team.ChatID, predictObj)
				if err != nil {
					notify(fmt.Sprintf("  ⚠️ Predict 失败, 使用模拟结果: %v", err))
					prevResults["narrative-evolution"] = strings.Join(narratives, "\n\n---\n\n")
				} else {
					bestIdx, _ := selectBestFromPrediction(predictResult, len(narratives))
					var result strings.Builder
					result.WriteString("## 群体智能剧情评估报告\n\n")
					result.WriteString(fmt.Sprintf("**最佳方案**: #%d (共识度: %.0f%%)\n\n", bestIdx+1, predictResult.Consensus*100))
					if predictResult.Summary != "" {
						result.WriteString("**评估摘要**:\n" + predictResult.Summary + "\n\n")
					}
					result.WriteString("**所有剧情方案**:\n")
					for i, n := range narratives {
						if i == bestIdx {
							result.WriteString(fmt.Sprintf("\n### ⭐ 方案 #%d (推荐)\n%s\n", i+1, n))
						} else {
							result.WriteString(fmt.Sprintf("\n### 方案 #%d\n%s\n", i+1, n))
						}
					}
					prevResults["narrative-evolution"] = result.String()
				}
			} else {
				prevResults["narrative-evolution"] = "剧情模拟未产出有效结果, 请根据游戏概念自行设计剧情。"
			}

			sr := StageResult{
				Name:   "narrative-evolution",
				Role:   "swarm-intelligence",
				Status: TaskCompleted,
				Output: prevResults["narrative-evolution"],
			}
			allResults = append(allResults, sr)
			we.savePhaseCheckpoints([]StageResult{sr})

			if team.Blackboard != nil {
				team.Blackboard.Write("narrative-evolution", prevResults["narrative-evolution"], "swarm-intel", "narrative")
			}
		} else {
			notify("  ⚠️ LLM 不可用, 跳过群体智能剧情演化")
			prevResults["narrative-evolution"] = "（群体智能不可用, 请手动设计剧情）"
		}
	}

	// ═══════════════════════════════════════════════════════════
	// Phase C: 美术原型 (复用 Creative-v2 对抗循环)
	// ═══════════════════════════════════════════════════════════
	notify("🎨 **Phase C**: 美术原型设计 (创意团队模式)")

	if _, ok := prevResults["art-plan"]; !ok {
		stage := findStage(wf, "art-plan")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["art-plan"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	if _, ok := prevResults["art-prototype"]; !ok {
		notify("  💻 美术原型 + 对抗审查...")
		terminator := NewAdaptiveTerminator(1, 3)
		var bestHTML string
		var bestScore float64

		for round := 1; ; round++ {
			htmlStage := findStage(wf, "art-prototype")
			if htmlStage == nil {
				break
			}
			stageForRound := *htmlStage
			stageForRound.Name = fmt.Sprintf("art-prototype-round%d", round)

			if feedback, ok := prevResults["art-review"]; ok && round > 1 {
				prevResults["visual-feedback"] = feedback
			}

			sr := we.executeStage(ctx, stageForRound, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status != TaskCompleted {
				if bestHTML != "" {
					prevResults["art-prototype"] = bestHTML
				}
				break
			}
			prevResults["art-prototype"] = sr.Output

			reviewStage := findStage(wf, "art-review")
			if reviewStage == nil {
				break
			}
			stageReview := *reviewStage
			stageReview.Name = fmt.Sprintf("art-review-round%d", round)
			srReview := we.executeStage(ctx, stageReview, objective, prevResults, team)
			allResults = append(allResults, srReview)
			if srReview.Status != TaskCompleted {
				break
			}
			prevResults["art-review"] = srReview.Output

			score := parseCreativeScore(srReview.Output)
			terminator.RecordRoundOutput(round, score, sr.Output)
			if score.AvgScore() > bestScore {
				bestScore = score.AvgScore()
				bestHTML = sr.Output
			}

			decision := terminator.ShouldTerminate(round, score)
			if decision.BestOutput != "" {
				prevResults["art-prototype"] = decision.BestOutput
			}
			if decision.ShouldStop {
				notify(fmt.Sprintf("  ✅ 美术原型完成 (第%d轮, 均分%.1f)", round, score.AvgScore()))
				break
			}
		}
		we.savePhaseCheckpoints(allResults[len(allResults)-1:])
	}

	// 渲染美术资源
	if team.dataDir != "" {
		htmlContent := extractHTMLFromOutput(prevResults["art-prototype"])
		if htmlContent != "" {
			mediaDir := filepath.Join(team.dataDir, "media")
			eng := media.NewEngine(mediaDir)
			for _, mr := range eng.RenderAll(ctx, htmlContent, team.Name, []string{"png"}) {
				if mr.Error == "" {
					notify(fmt.Sprintf("  🖼️ 美术资源: %s (%.1fKB)", mr.FilePath, float64(mr.Size)/1024))
				}
			}
		}
	}

	// ═══════════════════════════════════════════════════════════
	// Phase D: 技术开发 (扩展2D/3D引擎能力)
	// ═══════════════════════════════════════════════════════════
	notify("🔧 **Phase D**: 技术架构与引擎开发 (扩展研发团队)")

	if _, ok := prevResults["tech-architecture"]; !ok {
		stage := findStage(wf, "tech-architecture")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["tech-architecture"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// 引擎+逻辑+关卡 并行开发
	var devParallel []StageDef
	for _, name := range []string{"engine-dev", "logic-dev", "level-design"} {
		if _, ok := prevResults[name]; ok {
			continue
		}
		if s := findStage(wf, name); s != nil {
			devParallel = append(devParallel, *s)
		}
	}
	if len(devParallel) > 0 {
		notify(fmt.Sprintf("  ⚡ %d 路并行开发 (引擎+逻辑+关卡)...", len(devParallel)))
		results := we.executeParallel(ctx, devParallel, objective, prevResults, team)
		for _, sr := range results {
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults[sr.Name] = sr.Output
			}
		}
		we.savePhaseCheckpoints(results)
	}

	// ═══════════════════════════════════════════════════════════
	// Phase E: 集成测试 + 优化
	// ═══════════════════════════════════════════════════════════
	notify("🧪 **Phase E**: 游戏集成测试与优化")

	for _, name := range []string{"game-integration", "game-optimization"} {
		if _, ok := prevResults[name]; ok {
			continue
		}
		stage := findStage(wf, name)
		if stage == nil {
			continue
		}
		sr := we.executeStage(ctx, *stage, objective, prevResults, team)
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			prevResults[sr.Name] = sr.Output
			we.savePhaseCheckpoints([]StageResult{sr})
		}
	}

	notify("✅ **游戏研发整合团队** 工作流完成")
	return allResults, nil
}

// ═══════════════════════════════════════════════════════════
// 辅助函数
// ═══════════════════════════════════════════════════════════

func truncateForPrompt(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n...(已截断)"
}

func formatGameNarrative(result *swarm_intel.SimulationResult, name string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## %s 模拟结果\n\n", name))
	for i, s := range result.Scenarios {
		b.WriteString(fmt.Sprintf("### 场景 %d: %s (概率: %.0f%%)\n", i+1, s.Name, s.Probability*100))
		b.WriteString(s.Description + "\n")
		if len(s.KeyEvents) > 0 {
			b.WriteString("关键事件: " + strings.Join(s.KeyEvents, " → ") + "\n")
		}
		b.WriteString("\n")
	}
	if result.Summary != "" {
		b.WriteString("**综合分析**: " + result.Summary + "\n")
	}
	return b.String()
}

// ═══════════════════════════════════════════════════════════
// 游戏工作流专属 Prompt
// ═══════════════════════════════════════════════════════════

const gameConceptPrompt = `你是**资深游戏设计师** (参考 Genie 2 世界构建理念)。

游戏项目: {objective}

## 游戏设计文档 (GDD)

### 1. 核心定位
根据项目描述判断游戏类型:
- **小游戏**: HTML5/Canvas, 微信小游戏, 轻量快节奏
- **2D游戏**: 像素风/卡通/横版/俯视角
- **3D游戏**: Web 3D (Three.js/Babylon.js)

### 2. 核心机制
- **核心玩法循环**: 玩家做什么→反馈→激励
- **操作设计**: 触屏/键鼠映射
- **规则系统**: 胜负判定、计分、进度

### 3. 叙事框架 (供群体智能演化)
- **世界观**: 时代背景、核心设定
- **主角设定**: 身份、动机、成长弧线
- **核心冲突**: 主要矛盾和对立面
- **关键角色**: 3-5 个核心 NPC 及其关系网
(此部分将由群体智能引擎进一步演化和评估)

### 4. 内容设计
- 角色/单位属性
- 关卡/地图概要
- 道具/装备系统

### 5. 数值设计
- 经济系统、难度曲线、平衡性

### 6. 用户体验
- 新手引导、UI/HUD、音效需求`

const gameArtPlanPrompt = `你是**游戏美术总监** (复用创意团队策划能力)。

游戏项目: {objective}

游戏设计文档:
{prev_result}

## 美术方案
1. **视觉风格定义**: 整体风格 (像素/卡通/写实/扁平) + 参考描述
2. **色彩方案**: 主色/辅色/强调色, 昼夜/场景变化
3. **角色设计规范**:
   - 主角各状态 (idle/walk/run/attack/death)
   - NPC/敌人设计风格
   - 动画帧序列定义
4. **场景设计**:
   - 各关卡/场景视觉描述
   - 背景层 (视差滚动, 若适用)
   - 地形/障碍物风格
5. **UI资源清单**:
   - 按钮/图标/面板样式
   - HUD 布局
   - 字体建议

为 HTML 美术原型提供明确指引。`

const gameArtPrototypePrompt = `你是**游戏美术原型开发专家** (复用 html-developer 能力)。

游戏项目: {objective}

美术方案:
{prev_result}

## 任务: 用 HTML/CSS/SVG/Canvas 实现游戏美术原型
1. **角色精灵**: 用 SVG 或 Canvas 绘制主要角色的各状态
2. **场景背景**: 用 CSS 渐变/SVG 绘制游戏场景
3. **UI 元素**: 按钮、血条、对话框等 HUD 元素
4. **动画效果**: CSS 动画展示角色动作、场景转换
5. **Sprite Sheet**: 定义精灵图规格和帧序列

## 要求
- 输出完整 HTML 文件, 可在浏览器中预览
- 使用 CSS 变量系统 (方便主题切换)
- 包含交互: 点击角色可切换动画状态
- 适配移动端和桌面端`

const gameArtReviewPrompt = `你是**游戏美术审查总监** (复用 art-director 审查能力)。

游戏项目: {objective}

美术原型:
{prev_result}

## 评审维度 (每项 0-10)
1. **visual_design**: 游戏美术风格一致性和美感
2. **code_quality**: SVG/Canvas/CSS 代码质量
3. **responsive**: 多端适配
4. **content_complete**: 是否覆盖所有需要的美术资源
5. **creativity**: 视觉创意和游戏氛围感

## 输出格式 (严格 JSON)
{"visual_design": N, "code_quality": N, "responsive": N, "content_complete": N, "creativity": N, "pass": bool, "feedback": "具体改进建议"}`
