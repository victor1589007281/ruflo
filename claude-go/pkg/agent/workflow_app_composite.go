// workflow_app_composite.go — APP & 小程序跨团队协调工作流。
//
// 核心理念: 不是独立团队, 而是整合调度现有团队能力:
//   Phase A: 需求分析 — 复用 Research 团队的 fanout 调研能力
//   Phase B: UI/UX 原型 — 复用 Creative-v2 团队的 HTML 原型 + 对抗审查
//   Phase C: 研发实现 — 复用 Development 团队的 adversarial_dev 对抗循环
//   Phase D: 小程序适配 — 扩展: 专属小程序开发角色
//   Phase E: 集成测试 — 复用 Development 团队的 tester 角色
//
// 参考:
//   - AgentMesh (arXiv:2507.19902): 多智能体软件工程
//   - Vercel AI SDK + v0: 组件生成 → 代码
//   - Figma Make/Code Connect: 设计→代码映射
//   - novel-v3 编排模式: 单 executor 内复用多引擎
package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"github.com/anthropic/claude-go/pkg/media"
)

func appCompositeWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "app",
		Description: "APP&小程序整合团队: 调研团队(需求)→创意团队(原型)→研发团队(实现)→小程序适配→集成测试",
		Mode:        "app_composite",
		Rounds:      0,
		Stages: []StageDef{
			// Phase A: 需求分析 (复用 Research 团队 fanout 模式)
			{Name: "requirements-planning", Role: "synthesizer",
				Prompt: appRequirementsPlanningPrompt},
			{Name: "user-research", Role: "researcher",
				DependsOn: []string{"requirements-planning"}, Parallel: true,
				Prompt: appUserResearchPrompt},
			{Name: "tech-research", Role: "researcher",
				DependsOn: []string{"requirements-planning"}, Parallel: true,
				Prompt: appTechResearchPrompt},
			{Name: "requirements-synthesis", Role: "synthesizer",
				DependsOn: []string{"user-research", "tech-research"},
				Prompt:    appRequirementsSynthesisPrompt},

			// Phase B: UI/UX 原型 (复用 Creative-v2 的 HTML 对抗生成)
			{Name: "creative-plan", Role: "creative-planner",
				DependsOn: []string{"requirements-synthesis"},
				Prompt:    appCreativePlanPrompt},
			{Name: "html-prototype", Role: "html-developer",
				DependsOn: []string{"creative-plan"},
				Prompt:    appHTMLPrototypePrompt},
			{Name: "prototype-review", Role: "art-director",
				DependsOn: []string{"html-prototype"},
				Prompt:    appPrototypeReviewPrompt},

			// Phase C: 研发实现 (复用 Development 团队角色)
			{Name: "architecture", Role: "architect",
				DependsOn: []string{"requirements-synthesis", "html-prototype"}},
			{Name: "backend-dev", Role: "coder",
				DependsOn: []string{"architecture"},
				Prompt:    appBackendPrompt},
			{Name: "frontend-dev", Role: "coder",
				DependsOn: []string{"architecture", "html-prototype"},
				Parallel:  true,
				Prompt:    appFrontendPrompt},

			// Phase D: 小程序适配 (扩展能力)
			{Name: "miniprogram-dev", Role: "miniprogram-dev",
				DependsOn: []string{"architecture", "html-prototype"},
				Parallel:  true,
				Prompt:    appMiniprogramPrompt},

			// Phase E: 集成测试 + 审查
			{Name: "integration-test", Role: "tester",
				DependsOn: []string{"backend-dev", "frontend-dev", "miniprogram-dev"}},
			{Name: "code-review", Role: "reviewer",
				DependsOn: []string{"integration-test"}},
		},
	}
}

// executeAppComposite 跨团队协调编排器。
// 核心: 在单一 executor 中串联 Research + Creative-v2 + Development 三个团队的执行模式。
func (we *WorkflowExecutor) executeAppComposite(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
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
		we.pool.AutoScale(8)
		log.Printf("[app-composite] AgentPool 已调整")
	}

	// ═══════════════════════════════════════════════════════════
	// Phase A: 需求分析 (复用 Research 团队 fanout 模式)
	// ═══════════════════════════════════════════════════════════
	notify("📋 **Phase A**: 需求分析 (调研团队模式: 规划→并行调研→综合)")

	if _, ok := prevResults["requirements-planning"]; !ok {
		stage := findStage(wf, "requirements-planning")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["requirements-planning"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// 并行调研 (复用 research fanout)
	researchStages := []string{"user-research", "tech-research"}
	var parallelDefs []StageDef
	for _, name := range researchStages {
		if _, ok := prevResults[name]; ok {
			continue
		}
		if s := findStage(wf, name); s != nil {
			parallelDefs = append(parallelDefs, *s)
		}
	}
	if len(parallelDefs) > 0 {
		notify(fmt.Sprintf("  🔍 %d 路并行调研启动...", len(parallelDefs)))
		results := we.executeParallel(ctx, parallelDefs, objective, prevResults, team)
		for _, sr := range results {
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults[sr.Name] = sr.Output
			}
		}
		we.savePhaseCheckpoints(results)
	}

	if _, ok := prevResults["requirements-synthesis"]; !ok {
		stage := findStage(wf, "requirements-synthesis")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["requirements-synthesis"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// ═══════════════════════════════════════════════════════════
	// Phase B: UI/UX 原型 (复用 Creative-v2 对抗循环)
	// ═══════════════════════════════════════════════════════════
	notify("🎨 **Phase B**: UI/UX 原型设计 (创意团队模式: 策划→HTML原型→对抗审查)")

	if _, ok := prevResults["creative-plan"]; !ok {
		stage := findStage(wf, "creative-plan")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["creative-plan"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// HTML 原型 + 视觉审查 对抗循环 (复用 creative-v2 的 AdaptiveTerminator)
	if _, ok := prevResults["html-prototype"]; !ok {
		notify("  💻 HTML 原型生成 + 对抗优化...")
		terminator := NewAdaptiveTerminator(1, 4)
		var bestHTML string
		var bestScore float64

		for round := 1; ; round++ {
			notify(fmt.Sprintf("  🔄 原型第 %d 轮 (自适应, 最多4轮)", round))

			htmlStage := findStage(wf, "html-prototype")
			if htmlStage == nil {
				break
			}
			stageForRound := *htmlStage
			stageForRound.Name = fmt.Sprintf("html-prototype-round%d", round)

			if feedback, ok := prevResults["prototype-review"]; ok && round > 1 {
				prevResults["visual-feedback"] = feedback
			}

			sr := we.executeStage(ctx, stageForRound, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status != TaskCompleted {
				if bestHTML != "" {
					prevResults["html-prototype"] = bestHTML
					notify("  ⚠️ 生成失败, 使用最佳历史版本")
				}
				break
			}
			prevResults["html-prototype"] = sr.Output

			reviewStage := findStage(wf, "prototype-review")
			if reviewStage == nil {
				break
			}
			stageReview := *reviewStage
			stageReview.Name = fmt.Sprintf("prototype-review-round%d", round)
			srReview := we.executeStage(ctx, stageReview, objective, prevResults, team)
			allResults = append(allResults, srReview)

			if srReview.Status != TaskCompleted {
				break
			}
			prevResults["prototype-review"] = srReview.Output

			score := parseCreativeScore(srReview.Output)
			terminator.RecordRoundOutput(round, score, sr.Output)
			if score.AvgScore() > bestScore {
				bestScore = score.AvgScore()
				bestHTML = sr.Output
			}

			decision := terminator.ShouldTerminate(round, score)
			if decision.BestOutput != "" {
				prevResults["html-prototype"] = decision.BestOutput
			}
			if decision.ShouldStop {
				notify(fmt.Sprintf("  ✅ 原型完成: %s (第%d轮, 均分%.1f)", decision.Reason, round, score.AvgScore()))
				break
			}
			notify(fmt.Sprintf("  📝 第 %d 轮: 均分%.1f, 继续优化", round, score.AvgScore()))
		}

		we.savePhaseCheckpoints(allResults[len(allResults)-1:])
	}

	// 生成原型媒体文件 (复用 creative-v2 媒体引擎)
	if team.dataDir != "" {
		htmlContent := extractHTMLFromOutput(prevResults["html-prototype"])
		if htmlContent != "" {
			mediaDir := filepath.Join(team.dataDir, "media")
			engine := media.NewEngine(mediaDir)
			mediaResults := engine.RenderAll(ctx, htmlContent, team.Name, []string{"png"})
			for _, mr := range mediaResults {
				if mr.Error == "" {
					notify(fmt.Sprintf("  📸 原型截图: %s (%.1fKB)", mr.FilePath, float64(mr.Size)/1024))
				}
			}
		}
	}

	// ═══════════════════════════════════════════════════════════
	// Phase C: 研发实现 (复用 Development 团队角色)
	// ═══════════════════════════════════════════════════════════
	notify("🔧 **Phase C**: 架构设计与研发实现 (研发团队模式)")

	if _, ok := prevResults["architecture"]; !ok {
		stage := findStage(wf, "architecture")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["architecture"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	if _, ok := prevResults["backend-dev"]; !ok {
		stage := findStage(wf, "backend-dev")
		if stage != nil {
			sr := we.executeStage(ctx, *stage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["backend-dev"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// Phase C+D: 前端 + 小程序并行开发
	var devParallel []StageDef
	for _, name := range []string{"frontend-dev", "miniprogram-dev"} {
		if _, ok := prevResults[name]; ok {
			continue
		}
		if s := findStage(wf, name); s != nil {
			devParallel = append(devParallel, *s)
		}
	}
	if len(devParallel) > 0 {
		notify(fmt.Sprintf("  ⚡ %d 路并行开发 (前端+小程序)...", len(devParallel)))
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
	// Phase E: 集成测试 + 代码审查 (复用 Development 团队)
	// ═══════════════════════════════════════════════════════════
	notify("🧪 **Phase E**: 集成测试与代码审查 (研发团队测试流程)")

	for _, name := range []string{"integration-test", "code-review"} {
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

	notify("✅ **APP&小程序整合团队** 工作流完成")
	return allResults, nil
}

// ═══════════════════════════════════════════════════════════
// APP 工作流专属 Prompt
// ═══════════════════════════════════════════════════════════

const appRequirementsPlanningPrompt = `你是**产品需求规划师**, 负责将项目目标分解为结构化的调研任务。

项目目标: {objective}

## 任务
1. **目标拆解**: 将项目拆解为 3-5 个核心调研方向
2. **用户画像**: 定义目标用户群体和使用场景
3. **功能优先级**: P0(MVP)/P1(重要)/P2(锦上添花)
4. **技术栈初步方向**: 根据项目类型推荐前端/后端/小程序方案

## 输出
为下游的并行调研团队提供清晰的调研指引。`

const appUserResearchPrompt = `你是**用户研究专家**, 使用 **假设→证据→验证 (HEV)** 方法论进行调研。

项目目标: {objective}

上游规划:
{prev_result}

## 调研任务
1. **竞品分析**: 分析 3-5 个同类产品的功能、UX、商业模式
2. **用户需求**: 基于场景分析用户的核心需求和痛点
3. **交互模式**: 推荐的交互设计模式 (导航、表单、列表等)
4. **用户体验基准**: 行业最佳实践和体验标准
5. **使用 WebSearch 搜索相关领域的最新方案**

## 输出
结构化调研报告, 每条发现标注证据强度 (strong/moderate/weak)。`

const appTechResearchPrompt = `你是**技术调研专家**, 使用 **假设→证据→验证 (HEV)** 方法论进行调研。

项目目标: {objective}

上游规划:
{prev_result}

## 调研任务
1. **前端技术选型**: React/Vue/Flutter/RN 对比 + 推荐
2. **后端技术选型**: Node.js/Go/Python/Java 对比
3. **小程序框架**: 原生/Taro/uni-app 对比
4. **基础设施**: 数据库/缓存/文件存储/CDN 方案
5. **AI集成**: 是否需要 AI 能力, 如何集成

## 输出
技术选型对比矩阵, 每个方案的优劣势和推荐度。`

const appRequirementsSynthesisPrompt = `你是**高级产品架构师**, 综合调研结果产出最终需求文档。

项目目标: {objective}

调研结果:
{prev_result}

## 输出: 产品需求文档 (PRD)
1. **项目概述**: 一句话定义 + 核心价值
2. **用户故事**: 按优先级排列 (格式: 作为[角色], 我想要[功能], 以便[价值])
3. **功能清单**: 详细功能表 (P0/P1/P2)
4. **技术栈决策**: 最终选择 + 决策理由
5. **页面列表**: 所有页面及核心元素
6. **API清单**: 核心接口列表 (方法+路径+说明)
7. **非功能需求**: 性能/安全/兼容性标准

此文档将作为后续所有阶段的核心输入。`

const appCreativePlanPrompt = `你是**创意策划师** (复用创意团队能力)。

项目目标: {objective}

产品需求文档:
{prev_result}

## 任务: 为 APP/小程序设计完整的视觉方案
1. **设计系统**: 色彩方案、字体层级、间距系统、圆角规范
2. **页面结构**: 每个页面的线框描述 (布局、组件、交互)
3. **组件库清单**: 需要的 UI 组件列表
4. **交互规范**: 转场动画、手势、加载状态
5. **适配策略**: 响应式断点、小程序 TabBar、状态栏适配

## 输出
结构化的设计方案, 为 HTML 原型开发提供明确指引。`

const appHTMLPrototypePrompt = `你是**HTML原型开发专家** (复用创意团队 html-developer 能力)。

项目目标: {objective}

设计方案:
{prev_result}

## 任务: 将设计方案转化为完整的 HTML 交互原型
1. **完整 HTML**: 包含所有页面的响应式 HTML 原型
2. **CSS**: 使用设计系统的变量, 支持深色/浅色模式
3. **交互**: JS 实现页面切换、表单验证、状态管理
4. **移动适配**: 使用 viewport meta, 触摸事件, 移动端 UI 范式
5. **原型导航**: 底部 TabBar 或侧边栏导航

## 要求
- 输出完整可运行的 HTML 文件
- 使用现代 CSS (Grid/Flexbox/Container Queries)
- 包含真实的交互效果, 不是静态页面
- 适配手机/平板/桌面三端`

const appPrototypeReviewPrompt = `你是**视觉审查总监** (复用创意团队 art-director 评审能力)。

项目目标: {objective}

HTML 原型:
{prev_result}

## 评审维度 (每项 0-10)
1. **visual_design**: 视觉美感、色彩和谐、排版专业度
2. **code_quality**: HTML/CSS/JS 代码质量和规范性
3. **responsive**: 响应式适配 (手机/平板/桌面)
4. **content_complete**: 功能完整度 (是否覆盖需求)
5. **creativity**: 交互创意和用户体验

## 输出格式 (严格 JSON)
{"visual_design": N, "code_quality": N, "responsive": N, "content_complete": N, "creativity": N, "pass": bool, "feedback": "具体改进建议"}`

const appBackendPrompt = `你是**后端开发专家** (复用研发团队 coder 角色)。

项目目标: {objective}

架构设计和需求:
{prev_result}

## 任务
1. **API实现**: 按架构设计实现所有 RESTful API
2. **数据库**: 数据模型定义、迁移脚本
3. **认证鉴权**: JWT/Session 认证 + 微信登录 (code2session)
4. **中间件**: 日志、限流、CORS、错误处理
5. **小程序后端**: 微信登录、小程序支付等接口

## 代码要求
- 完整可运行的后端代码
- 包含数据库迁移脚本
- 包含 .env.example 环境变量模板
- 中文注释

{adversarial_feedback}`

const appFrontendPrompt = `你是**前端开发专家** (复用研发团队 coder 角色)。

项目目标: {objective}

架构设计和 HTML 原型:
{prev_result}

## 任务
1. **项目初始化**: 基于技术栈创建前端项目
2. **组件开发**: 将原型中的 UI 转化为可复用组件
3. **状态管理**: 全局状态方案
4. **API对接**: 与后端 API 集成
5. **路由**: 页面路由配置
6. **主题**: 实现设计系统中的主题变量

## 代码要求
- 基于 HTML 原型的设计语言, 保持视觉一致性
- TypeScript 优先
- 组件有明确的 Props 类型定义

{adversarial_feedback}`

const appMiniprogramPrompt = `你是**小程序开发专家**。

项目目标: {objective}

架构设计和 HTML 原型:
{prev_result}

## 任务
1. **项目结构**: 小程序项目初始化 (基于技术选型: 原生/Taro/uni-app)
2. **页面实现**: 将 HTML 原型转化为小程序页面
3. **组件适配**: 将 Web 组件转换为小程序组件
4. **API对接**: wx.request 封装, 与后端 API 集成
5. **小程序特性**:
   - 微信登录 (wx.login → 后端 code2session)
   - 分享 (onShareAppMessage)
   - 下拉刷新、上拉加载
   - 授权管理
6. **性能优化**: 分包加载、图片懒加载

## 代码要求
- 完整的小程序项目代码
- 包含 app.json / app.wxss 全局配置
- 页面代码可直接在开发者工具中运行

{adversarial_feedback}`
