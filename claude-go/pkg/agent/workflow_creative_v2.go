// creative-v2 工作流: 增强版创意团队, 支持 HTML 网页开发 + 多格式媒体输出。
//
// 复用研发团队核心能力:
//   - CheckpointStore: 阶段级检查点, 支持长任务恢复
//   - AdaptiveTerminator: 动态对抗调整 (基于评分的自适应轮数)
//   - AgentPool.AutoScale: 动态 Agent 池大小
//   - executeStageWithRetry: 阶段级重试 + 指数退避
//
// 架构参考:
//   - HyperFrames: HTML-native 视频合成
//   - CREA (arXiv:2504.05306): 多 Agent 协作创意
//   - EMNLP 2025: LLM 多 Agent 创意系统综述
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/media"
)

func creativeV2Workflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "creative-v2",
		Description: "增强版创意团队: 策划→HTML开发→审查→媒体输出(PNG/PDF/MP4/PPT/APP原型)",
		Mode:        "creative_media",
		Rounds:      0, // 0 = 自适应 AdaptiveTerminator
		Stages: []StageDef{
			// Phase 1: 策划
			{Name: "creative-plan", Role: "creative-planner",
				Prompt: creativePlannerPrompt},
			{Name: "task-decompose", Role: "creative-planner",
				DependsOn: []string{"creative-plan"},
				Prompt:    taskDecomposePrompt},

			// Phase 2: 内容生成
			{Name: "html-develop", Role: "html-developer",
				DependsOn: []string{"task-decompose"},
				Prompt:    htmlDeveloperPrompt},

			// Phase 3: 视觉审查 (evaluator — 包含 "pass" 关键字)
			{Name: "visual-review", Role: "art-director",
				DependsOn: []string{"html-develop"},
				Prompt:    creativeReviewPrompt},

			// Phase 4: 媒体输出 (代码驱动)
			{Name: "media-render", Role: "media-producer",
				DependsOn: []string{"html-develop"},
				Parallel:  true},

			// Phase 5: 整合交付
			{Name: "final-delivery", Role: "media-producer",
				DependsOn: []string{"media-render", "visual-review"}},
		},
	}
}

// executeCreativeMedia 专用编排器, 复用研发团队核心机制:
//   - 检查点恢复 (restoreCheckpoints)
//   - AdaptiveTerminator (对抗动态调整)
//   - AgentPool.AutoScale (动态池大小)
func (we *WorkflowExecutor) executeCreativeMedia(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	notify := func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}

	// ── 检查点恢复 (复用研发团队 checkpoint 机制) ──
	we.restoreCheckpoints(wf.Stages, prevResults, &allResults)
	if len(allResults) > 0 {
		notify(fmt.Sprintf("♻️ 从检查点恢复: 已恢复 %d 个阶段", len(allResults)))
	}

	// ── Agent Pool 动态调整 (复用研发团队 AutoScale) ──
	if we.pool != nil {
		we.pool.AutoScale(4) // creative-v2 需要 4 角色
		log.Printf("[creative-v2] AgentPool 已调整")
	}

	// ── Phase 1: 创意策划 + 任务拆解 ──
	if _, ok := prevResults["creative-plan"]; !ok {
		notify("🎨 **Phase 1/5**: 创意策划 + 任务拆解")

		planStage := findStage(wf, "creative-plan")
		if planStage != nil {
			sr := we.executeStage(ctx, *planStage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["creative-plan"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	if _, ok := prevResults["task-decompose"]; !ok {
		decomposeStage := findStage(wf, "task-decompose")
		if decomposeStage != nil {
			sr := we.executeStage(ctx, *decomposeStage, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				prevResults["task-decompose"] = sr.Output
				we.savePhaseCheckpoints([]StageResult{sr})
			}
		}
	}

	// ── Phase 2+3: HTML 生成 + 视觉审查 (AdaptiveTerminator 对抗循环) ──
	if _, ok := prevResults["html-develop"]; !ok {
		notify("💻 **Phase 2/5**: HTML 内容生成 + 对抗优化")

		// 创建 AdaptiveTerminator: minRounds=1, maxRounds=5
		terminator := NewAdaptiveTerminator(1, 5)
		var htmlOutput string
		var bestHTML string
		var bestScore float64

		for round := 1; ; round++ {
			notify(fmt.Sprintf("  🔄 第 %d 轮 (自适应, 最多5轮)", round))

			// 生成 HTML
			htmlStage := findStage(wf, "html-develop")
			if htmlStage == nil {
				break
			}
			stageForRound := *htmlStage
			stageForRound.Name = fmt.Sprintf("html-develop-round%d", round)

			if feedback, ok := prevResults["visual-review"]; ok && round > 1 {
				prevResults["visual-feedback"] = feedback
			}

			sr := we.executeStage(ctx, stageForRound, objective, prevResults, team)
			allResults = append(allResults, sr)
			if sr.Status != TaskCompleted {
				if bestHTML != "" {
					htmlOutput = bestHTML
					notify("  ⚠️ 生成失败, 使用最佳历史版本")
				}
				break
			}
			htmlOutput = sr.Output
			prevResults["html-develop"] = sr.Output

			// 视觉审查
			notify("  🔍 视觉审查中...")
			reviewStage := findStage(wf, "visual-review")
			if reviewStage == nil {
				break
			}
			stageReview := *reviewStage
			stageReview.Name = fmt.Sprintf("visual-review-round%d", round)
			srReview := we.executeStage(ctx, stageReview, objective, prevResults, team)
			allResults = append(allResults, srReview)

			if srReview.Status != TaskCompleted {
				break
			}
			prevResults["visual-review"] = srReview.Output

			// 解析评审分数 → EvalScore (复用研发团队对抗评分机制)
			score := parseCreativeScore(srReview.Output)

			// 记录本轮最佳 (复用 AdaptiveTerminator.RecordRoundOutput)
			terminator.RecordRoundOutput(round, score, htmlOutput)
			if score.AvgScore() > bestScore {
				bestScore = score.AvgScore()
				bestHTML = htmlOutput
			}

			// 动态终止决策 (复用研发团队 ShouldTerminate)
			decision := terminator.ShouldTerminate(round, score)
			if decision.BestOutput != "" {
				htmlOutput = decision.BestOutput
			}

			if decision.ShouldStop {
				notify(fmt.Sprintf("  ✅ 对抗终止: %s (第%d轮, 均分%.1f)", decision.Reason, round, score.AvgScore()))
				break
			}
			if decision.StrategyShift {
				notify(fmt.Sprintf("  🔄 策略转换: 第%d轮分数收敛 (%.1f), 尝试新方向", round, score.AvgScore()))
				prevResults["visual-feedback"] = srReview.Output + "\n\n[策略转换] 当前方向已收敛, 请尝试完全不同的视觉风格和布局方案。"
			} else {
				notify(fmt.Sprintf("  📝 第 %d 轮: 均分%.1f, 继续优化", round, score.AvgScore()))
			}
		}

		if htmlOutput != "" {
			prevResults["html-develop"] = htmlOutput
			we.savePhaseCheckpoints(allResults[len(allResults)-2:]) // 保存最近2个结果
		}
	}

	// ── Phase 4: 媒体输出 ──
	notify("🎬 **Phase 4/5**: 媒体输出 (PNG/PDF/MP4/PPTX)")

	htmlContent := extractHTMLFromOutput(prevResults["html-develop"])
	if htmlContent == "" {
		log.Printf("[creative-v2] 未能从 LLM 输出中提取 HTML, 使用原始输出")
		htmlContent = prevResults["html-develop"]
	}

	formats := detectOutputFormats(objective, htmlContent)
	notify(fmt.Sprintf("  📦 输出格式: %v", formats))

	mediaDir := filepath.Join(team.dataDir, "media")
	engine := media.NewEngine(mediaDir)

	mediaResults := engine.RenderAll(ctx, htmlContent, team.Name, formats)
	var mediaSummary strings.Builder
	for _, mr := range mediaResults {
		if mr.Error != "" {
			mediaSummary.WriteString(fmt.Sprintf("❌ %s: %s\n", mr.Format, mr.Error))
			notify(fmt.Sprintf("  ❌ %s: %s", mr.Format, mr.Error))
		} else {
			mediaSummary.WriteString(fmt.Sprintf("✅ %s: %s (%.1f KB, %.1fs)\n",
				mr.Format, mr.FilePath, float64(mr.Size)/1024, mr.Duration.Seconds()))
			notify(fmt.Sprintf("  ✅ %s: %.1f KB (%.1fs)", strings.ToUpper(mr.Format), float64(mr.Size)/1024, mr.Duration.Seconds()))
		}
	}

	mediaResult := StageResult{
		Name:     "media-render",
		Status:   TaskCompleted,
		Output:   mediaSummary.String(),
		Duration: fmt.Sprintf("%.1fs", sumDuration(mediaResults).Seconds()),
	}
	allResults = append(allResults, mediaResult)
	prevResults["media-render"] = mediaSummary.String()

	// ── Phase 5: 最终整合交付 ──
	notify("📦 **Phase 5/5**: 最终整合交付")

	deliveryStage := findStage(wf, "final-delivery")
	if deliveryStage != nil {
		sr := we.executeStage(ctx, *deliveryStage, objective, prevResults, team)
		allResults = append(allResults, sr)
	}

	for _, mr := range mediaResults {
		if mr.Error == "" && mr.FilePath != "" {
			notify(fmt.Sprintf("📎 %s 已生成: `%s`", strings.ToUpper(mr.Format), mr.FilePath))
		}
	}

	return allResults, nil
}

// ── 辅助函数 ──

func findStage(wf *WorkflowDef, name string) *StageDef {
	for i := range wf.Stages {
		if wf.Stages[i].Name == name {
			return &wf.Stages[i]
		}
	}
	return nil
}

// parseCreativeScore 从审查 JSON 中提取评分, 映射为研发团队的 EvalScore
func parseCreativeScore(output string) EvalScore {
	type creativeReview struct {
		VisualDesign    float64 `json:"visual_design"`
		CodeQuality     float64 `json:"code_quality"`
		Responsive      float64 `json:"responsive"`
		ContentComplete float64 `json:"content_complete"`
		Creativity      float64 `json:"creativity"`
		Pass            bool    `json:"pass"`
	}

	var review creativeReview

	// 提取 JSON
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		jsonStr := output[start : end+1]
		if err := json.Unmarshal([]byte(jsonStr), &review); err == nil {
			return EvalScore{
				Correctness:  review.VisualDesign,
				Completeness: review.ContentComplete,
				Security:     review.Responsive,
				CodeQuality:  review.CodeQuality,
				Pass:         review.Pass,
			}
		}
	}

	// 解析失败时检查文本标志
	lower := strings.ToLower(output)
	pass := strings.Contains(lower, `"pass": true`) ||
		strings.Contains(lower, `"pass":true`) ||
		strings.Contains(output, "通过")
	return EvalScore{
		Correctness:  6,
		Completeness: 6,
		Security:     6,
		CodeQuality:  6,
		Pass:         pass,
	}
}

// extractHTMLFromOutput 从 LLM 输出中提取完整 HTML 代码。
func extractHTMLFromOutput(output string) string {
	// 1. 代码块
	for _, fence := range []string{"```html", "```HTML"} {
		if idx := strings.Index(output, fence); idx >= 0 {
			start := idx + len(fence)
			if nl := strings.Index(output[start:], "\n"); nl >= 0 {
				start += nl + 1
			}
			end := strings.Index(output[start:], "```")
			if end >= 0 {
				html := strings.TrimSpace(output[start : start+end])
				if len(html) > 50 {
					return html
				}
			}
		}
	}

	// 2. DOCTYPE
	lower := strings.ToLower(output)
	for _, marker := range []string{"<!doctype html>", "<!doctype html"} {
		idx := strings.Index(lower, marker)
		if idx >= 0 {
			endIdx := strings.LastIndex(lower, "</html>")
			if endIdx > idx {
				return strings.TrimSpace(output[idx : endIdx+7])
			}
		}
	}

	// 3. <html>
	if idx := strings.Index(lower, "<html"); idx >= 0 {
		endIdx := strings.LastIndex(lower, "</html>")
		if endIdx > idx {
			return strings.TrimSpace(output[idx : endIdx+7])
		}
	}

	// 4. <body>
	if strings.Contains(lower, "<body") {
		return fmt.Sprintf("<!DOCTYPE html>\n<html>\n<head><meta charset=\"utf-8\"></head>\n%s\n</html>", output)
	}

	return ""
}

// detectOutputFormats 根据目标和内容检测需要的输出格式
func detectOutputFormats(objective, htmlContent string) []string {
	lower := strings.ToLower(objective)
	formats := []string{"png"}

	hasPDF := strings.Contains(lower, "pdf")
	hasVideo := strings.Contains(lower, "视频") || strings.Contains(lower, "video") ||
		strings.Contains(lower, "mp4") || strings.Contains(lower, "动画")
	hasPPT := strings.Contains(lower, "ppt") || strings.Contains(lower, "幻灯片") ||
		strings.Contains(lower, "演示") || strings.Contains(lower, "slides")
	hasSVG := strings.Contains(lower, "svg") || strings.Contains(htmlContent, "<svg")
	hasApp := strings.Contains(lower, "app") || strings.Contains(lower, "原型") ||
		strings.Contains(lower, "prototype") || strings.Contains(lower, "ui设计") ||
		strings.Contains(lower, "ui 设计") || strings.Contains(lower, "界面设计") ||
		strings.Contains(htmlContent, "class=\"screen\"") ||
		strings.Contains(htmlContent, "data-screen=")

	if hasPDF || (!hasVideo && !hasPPT) {
		formats = append(formats, "pdf")
	}
	if hasVideo || strings.Contains(htmlContent, "@keyframes") {
		formats = append(formats, "mp4")
	}
	if hasPPT || strings.Contains(htmlContent, "<section") {
		formats = append(formats, "pptx")
	}
	if hasSVG {
		formats = append(formats, "svg")
	}
	// APP 原型: 每个 screen 截图为独立 PNG (已覆盖在主 png 中)，额外生成多页 PDF
	if hasApp && !hasPDF {
		formats = append(formats, "pdf")
	}

	return formats
}

func sumDuration(results []media.RenderResult) time.Duration {
	var total time.Duration
	for _, r := range results {
		total += r.Duration
	}
	return total
}

// ── Prompt 定义 ──

const creativePlannerPrompt = `你是资深创意策划师，精通视觉设计、网页开发和 APP 原型设计。你的任务是理解用户需求，制定详细的创意执行方案。

用户需求: {objective}

请输出:
1. **需求分析**: 理解用户真实意图和期望效果
2. **创意方向**: 视觉风格、配色方案、排版策略
3. **技术方案**: HTML/CSS/SVG 实现策略
4. **输出规格**: 分辨率、格式、页数等
5. **时间线**: 各阶段估时

注意:
- 如果是多页内容 (PPT/网站), 规划每页的内容结构
- 如果包含动画/视频, 规划关键帧和时间线, 使用 CSS @keyframes
- 使用现代 CSS 技术 (Grid, Flexbox, 动画, 渐变)
- 确保输出方案具有可执行性
- 如果是 APP 原型/界面设计:
  · 规划页面流 (首页→详情→设置等)
  · 定义设计 token (主色、辅色、字体阶梯、间距系统)
  · 确定目标平台 (iOS/Android/跨平台)
  · 每个页面用 <section class="screen" data-screen="页面名"> 包裹
  · 容器固定 390x844 模拟 iPhone 15 (可按需切换平板 768x1024)
  · 包含状态栏、导航栏、Tab Bar 等系统 UI 模拟`

const taskDecomposePrompt = `你是项目拆解专家。根据创意方案，将任务拆分为可独立执行的子任务。

创意方案:
{prev_result}

用户需求: {objective}

请输出 JSON 格式的任务列表:
{
  "tasks": [
    {
      "id": 1,
      "name": "任务名称",
      "type": "html|svg|css|animation|app-screen",
      "description": "详细描述",
      "html_requirements": "HTML 实现要点",
      "estimated_lines": 100
    }
  ],
  "output_formats": ["png", "pdf", "mp4", "pptx"],
  "total_pages": 1,
  "has_animation": false,
  "is_app_prototype": false
}

规则:
- 单页网页/海报/名片: 1个任务
- 多页 PPT: 每页1个任务 (用 <section class="slide"> 标签分隔)
- 视频/动画: 拆分为场景, 每个场景1个任务
- APP 原型: 每个页面1个任务 (用 <section class="screen" data-screen="页面名"> 分隔)
  · 设置 is_app_prototype=true
  · type 用 "app-screen"
  · 规划页面间导航关系 (点击哪个元素跳到哪个页面)
- 每个任务的 HTML 不超过 500 行`

const htmlDeveloperPrompt = `创作需求: {objective}

任务拆解:
{prev_result}

审查反馈 (如有):
{visual-feedback}

你是顶级前端专家。直接输出一个完整 HTML 文件，不要任何解释。

规范:
- HTML5, 所有 CSS 在 <style>, 所有 JS 在 <script>
- 不引用任何外部 CDN、字体或资源
- 字体: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif
- 现代 CSS: Grid, Flexbox, 变量, 渐变, 阴影, 动画
- 如果需要动画效果, 必须使用 CSS @keyframes 定义动画
- 视口 1920×1080 (普通网页/PPT)
- 多页 PPT 用 <section class="slide"> 分隔
- 所有文字必须是真实内容 (禁止 Lorem ipsum)
- 输出的第一行必须是 <!DOCTYPE html>

APP 原型额外规范 (当需求包含 APP/原型/界面/UI 设计时):
- 外层容器: <div class="phone-frame"> 模拟手机外壳 (390x844, 圆角44px, 边框)
- 每个页面: <section class="screen" data-screen="页面名">
- 状态栏: <div class="status-bar"> 含时间/信号/电量 (SVG图标)
- 导航栏: <nav class="nav-bar"> 含返回按钮、标题、操作按钮
- Tab Bar: <div class="tab-bar"> 含 3-5 个 tab (图标+文字)
- CSS 变量定义设计 token: --color-primary, --color-bg, --spacing-sm/md/lg, --radius-sm/md/lg
- 页面切换: 点击元素触发 JS 切换 screen (添加 data-goto="目标页面名" 属性)
- 触控反馈: button:active { transform: scale(0.96); opacity: 0.8 }
- 安全区域: 底部预留 34px (iPhone X+ home indicator)
- 所有图标用内联 SVG (禁止 emoji 替代图标)
- 卡片/列表使用真实内容和合理数据`

const creativeReviewPrompt = `你是资深艺术指导和前端技术审查专家。审查提交的 HTML 作品质量。

作品代码:
{prev_result}

原始需求: {objective}

请从以下维度审查 (每项 1-10 分):
1. **visual_design** (配色、排版、层次感): X/10
2. **code_quality** (语义化、CSS 最佳实践): X/10
3. **responsive** (适配不同分辨率): X/10
4. **content_complete** (是否满足需求、无占位符): X/10
5. **creativity** (独特性、美感): X/10

你必须输出以下 JSON (不要多余文字):
{"visual_design": 8, "code_quality": 7, "responsive": 8, "content_complete": 9, "creativity": 8, "pass": true, "feedback": "具体修改建议..."}

通过标准: 所有维度 ≥ 6 且平均 ≥ 7
如果不通过, 在 feedback 中给出具体修改指导。`

const mediaProducerPrompt = `你是后期制作专家。根据 HTML 作品和媒体渲染结果，整理最终交付物清单。

HTML 作品:
{html-develop}

媒体渲染结果:
{media-render}

原始需求: {objective}

请输出交付清单:
1. 列出所有生成的文件及其路径
2. 各文件的用途说明
3. 使用建议 (分辨率、兼容性等)
4. 如有问题, 说明原因和解决方案`
