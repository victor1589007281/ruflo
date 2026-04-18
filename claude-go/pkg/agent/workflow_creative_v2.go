// creative-v2 工作流: 增强版创意团队, 支持 HTML 网页开发 + 多格式媒体输出。
//
// 架构参考:
//   - HyperFrames: HTML-native 视频合成
//   - CREA (arXiv:2504.05306): 多 Agent 协作创意
//   - EMNLP 2025: LLM 多 Agent 创意系统综述
//
// Phase 模型:
//   Phase 1: 创意策划 + 智能任务拆解
//   Phase 2: 内容生成 (HTML/SVG/CSS/JS) — 可多轮迭代
//   Phase 3: 视觉审查 + 对抗优化
//   Phase 4: 媒体输出 (PNG/PDF/MP4/PPTX)
//   Phase 5: 最终整合 + 作品交付
package agent

import (
	"context"
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
		Description: "增强版创意团队: 策划→HTML开发→审查→媒体输出(PNG/PDF/MP4/PPT)",
		Mode:        "creative_media",
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

			// Phase 3: 视觉审查
			{Name: "visual-review", Role: "art-director",
				DependsOn: []string{"html-develop"},
				Prompt:    creativeReviewPrompt},

			// Phase 4: 媒体输出 (由代码驱动, 非 LLM)
			{Name: "media-render", Role: "media-producer",
				DependsOn: []string{"html-develop"},
				Parallel:  true},

			// Phase 5: 整合交付
			{Name: "final-delivery", Role: "media-producer",
				DependsOn: []string{"media-render", "visual-review"}},
		},
	}
}

// executeCreativeMedia 专用编排器: 策划→生成→审查→媒体输出
func (we *WorkflowExecutor) executeCreativeMedia(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	notify := func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}

	// ── Phase 1: 创意策划 + 任务拆解 ──
	notify("🎨 **Phase 1/5**: 创意策划 + 任务拆解")

	planStage := findStage(wf, "creative-plan")
	if planStage != nil {
		sr := we.executeStage(ctx, *planStage, objective, prevResults, team)
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			prevResults["creative-plan"] = sr.Output
		}
	}

	decomposeStage := findStage(wf, "task-decompose")
	if decomposeStage != nil {
		sr := we.executeStage(ctx, *decomposeStage, objective, prevResults, team)
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			prevResults["task-decompose"] = sr.Output
		}
	}

	// ── Phase 2: HTML 内容生成 (最多 3 轮迭代) ──
	notify("💻 **Phase 2/5**: HTML 内容生成")

	maxRounds := 3
	var htmlOutput string
	for round := 1; round <= maxRounds; round++ {
		notify(fmt.Sprintf("  🔄 生成第 %d/%d 轮", round, maxRounds))

		htmlStage := findStage(wf, "html-develop")
		if htmlStage == nil {
			break
		}
		stageForRound := *htmlStage
		stageForRound.Name = fmt.Sprintf("html-develop-round%d", round)

		// 注入前一轮审查反馈
		if feedback, ok := prevResults["visual-review"]; ok && round > 1 {
			prevResults["visual-feedback"] = feedback
		}

		sr := we.executeStage(ctx, stageForRound, objective, prevResults, team)
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			htmlOutput = sr.Output
			prevResults["html-develop"] = sr.Output
		} else {
			break
		}

		// ── Phase 3: 视觉审查 ──
		if round < maxRounds {
			notify("  🔍 视觉审查中...")
			reviewStage := findStage(wf, "visual-review")
			if reviewStage != nil {
				stageReview := *reviewStage
				stageReview.Name = fmt.Sprintf("visual-review-round%d", round)
				srReview := we.executeStage(ctx, stageReview, objective, prevResults, team)
				allResults = append(allResults, srReview)
				if srReview.Status == TaskCompleted {
					prevResults["visual-review"] = srReview.Output
					// 检查是否通过
					if strings.Contains(strings.ToLower(srReview.Output), `"pass": true`) ||
						strings.Contains(strings.ToLower(srReview.Output), `"pass":true`) ||
						strings.Contains(srReview.Output, "通过") {
						notify(fmt.Sprintf("  ✅ 第 %d 轮审查通过", round))
						break
					}
					notify(fmt.Sprintf("  📝 第 %d 轮审查未通过, 继续优化", round))
				}
			}
		}
	}

	// ── Phase 4: 媒体输出 ──
	notify("🎬 **Phase 4/5**: 媒体输出 (PNG/PDF/MP4/PPTX)")

	htmlContent := extractHTMLFromOutput(htmlOutput)
	if htmlContent == "" {
		log.Printf("[creative-v2] 未能从 LLM 输出中提取 HTML, 使用原始输出")
		htmlContent = htmlOutput
	}

	// 确定输出格式
	formats := detectOutputFormats(objective, htmlContent)
	notify(fmt.Sprintf("  📦 输出格式: %v", formats))

	// 创建媒体引擎
	mediaDir := filepath.Join(team.dataDir, "media")
	engine := media.NewEngine(mediaDir)

	mediaResults := engine.RenderAll(ctx, htmlContent, team.Name, formats)
	var mediaSummary strings.Builder
	for _, mr := range mediaResults {
		if mr.Error != "" {
			mediaSummary.WriteString(fmt.Sprintf("❌ %s: %s\n", mr.Format, mr.Error))
		} else {
			mediaSummary.WriteString(fmt.Sprintf("✅ %s: %s (%.1f KB, %.1fs)\n",
				mr.Format, mr.FilePath, float64(mr.Size)/1024, mr.Duration.Seconds()))
		}
	}

	mediaResult := StageResult{
		Name:   "media-render",
		Status: TaskCompleted,
		Output: mediaSummary.String(),
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

	// 发送媒体文件通知
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

// extractHTMLFromOutput 从 LLM 输出中提取完整 HTML 代码。
// 按优先级尝试: 代码块 → DOCTYPE → <html> 标签 → 最大 HTML 片段
func extractHTMLFromOutput(output string) string {
	// 1. 匹配 ```html ... ``` 代码块 (最常见的 LLM 输出格式)
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

	// 2. 匹配 <!DOCTYPE html> ... </html>
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

	// 3. 匹配 <html> ... </html>
	if idx := strings.Index(lower, "<html"); idx >= 0 {
		endIdx := strings.LastIndex(lower, "</html>")
		if endIdx > idx {
			return strings.TrimSpace(output[idx : endIdx+7])
		}
	}

	// 4. 如果有 <body> 标签, 包装为完整 HTML
	if strings.Contains(lower, "<body") {
		return fmt.Sprintf("<!DOCTYPE html>\n<html>\n<head><meta charset=\"utf-8\"></head>\n%s\n</html>", output)
	}

	return ""
}

// detectOutputFormats 根据目标和内容检测需要的输出格式
func detectOutputFormats(objective, htmlContent string) []string {
	lower := strings.ToLower(objective)
	formats := []string{"png"} // 始终输出 PNG 预览

	hasPDF := strings.Contains(lower, "pdf")
	hasVideo := strings.Contains(lower, "视频") || strings.Contains(lower, "video") ||
		strings.Contains(lower, "mp4") || strings.Contains(lower, "动画")
	hasPPT := strings.Contains(lower, "ppt") || strings.Contains(lower, "幻灯片") ||
		strings.Contains(lower, "演示") || strings.Contains(lower, "slides")
	hasSVG := strings.Contains(lower, "svg") || strings.Contains(htmlContent, "<svg")

	// 默认输出 PDF
	if hasPDF || (!hasVideo && !hasPPT) {
		formats = append(formats, "pdf")
	}
	if hasVideo || strings.Contains(htmlContent, "animation") || strings.Contains(htmlContent, "@keyframes") {
		formats = append(formats, "mp4")
	}
	if hasPPT || strings.Contains(htmlContent, "<section") {
		formats = append(formats, "pptx")
	}
	if hasSVG {
		formats = append(formats, "svg")
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

const creativePlannerPrompt = `你是资深创意策划师，精通视觉设计和网页开发。你的任务是理解用户需求，制定详细的创意执行方案。

用户需求: {objective}

请输出:
1. **需求分析**: 理解用户真实意图和期望效果
2. **创意方向**: 视觉风格、配色方案、排版策略
3. **技术方案**: HTML/CSS/SVG 实现策略
4. **输出规格**: 分辨率、格式、页数等
5. **时间线**: 各阶段估时

注意:
- 如果是多页内容 (PPT/网站), 规划每页的内容结构
- 如果包含动画/视频, 规划关键帧和时间线
- 使用现代 CSS 技术 (Grid, Flexbox, 动画, 渐变)
- 确保输出方案具有可执行性`

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
      "type": "html|svg|css|animation",
      "description": "详细描述",
      "html_requirements": "HTML 实现要点",
      "estimated_lines": 100
    }
  ],
  "output_formats": ["png", "pdf", "mp4", "pptx"],
  "total_pages": 1,
  "has_animation": false
}

规则:
- 单页网页/海报/名片: 1个任务
- 多页 PPT: 每页1个任务 (用 <section> 标签分隔)
- 视频/动画: 拆分为场景, 每个场景1个任务
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
- 视口 1920×1080
- 多页 PPT 用 <section class="slide"> 分隔
- 所有文字必须是真实内容 (禁止 Lorem ipsum)
- 输出的第一行必须是 <!DOCTYPE html>`

const creativeReviewPrompt = `你是资深艺术指导和前端技术审查专家。审查提交的 HTML 作品质量。

作品代码:
{prev_result}

原始需求: {objective}

请从以下维度审查 (每项 1-10 分):
1. **视觉设计** (配色、排版、层次感): X/10
2. **代码质量** (语义化、CSS 最佳实践): X/10
3. **响应式** (适配不同分辨率): X/10
4. **内容完整** (是否满足需求、无占位符): X/10
5. **创意水平** (独特性、美感): X/10

输出 JSON:
{
  "visual_design": 8,
  "code_quality": 7,
  "responsive": 8,
  "content_complete": 9,
  "creativity": 8,
  "pass": true,
  "feedback": "具体修改建议..."
}

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
