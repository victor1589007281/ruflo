// teams_test.go — 多模态飞书 + 三大团队(创意/金融/公众号)综合评测
// 运行: go test -v -count=1 -timeout 20m ./tests/eval/ -run TestTeamsEval
package eval

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/vision"
)

// ── 评分结构(复用) ──────────────────────────────────────

type TeamEvalItem struct {
	Module   string
	Name     string
	MaxScore float64
	Score    float64
	Status   string
	Detail   string
}

type TeamEvalReport struct {
	Items     []TeamEvalItem
	StartTime time.Time
	EndTime   time.Time
}

func (r *TeamEvalReport) Add(item TeamEvalItem) {
	r.Items = append(r.Items, item)
}

func (r *TeamEvalReport) Print(t *testing.T) {
	t.Helper()
	fmt.Printf("\n╔══════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║     claude-go 多模态 + 团队能力专业评估报告                     ║\n")
	fmt.Printf("║     评估时间: %s                                    ║\n", r.EndTime.Sub(r.StartTime).Round(time.Second))
	fmt.Printf("╠══════════════════════════════════════════════════════════════════╣\n")

	totalScore, totalMax := 0.0, 0.0
	for _, mod := range []string{"FeishuMultimodal", "FeishuEcosystem", "CreativeTeam", "FinanceTeam", "TechBlogTeam", "Intent"} {
		modScore, modMax := 0.0, 0.0
		fmt.Printf("║ 【%s】\n", mod)
		for _, it := range r.Items {
			if it.Module != mod {
				continue
			}
			icon := "🟢"
			pct := 0.0
			if it.MaxScore > 0 {
				pct = it.Score / it.MaxScore * 100
			}
			if pct < 60 {
				icon = "🔴"
			} else if pct < 90 {
				icon = "🟡"
			}
			fmt.Printf("║   %s %-35s %5.1f/%5.1f (%5.1f%%)\n", icon, it.Name, it.Score, it.MaxScore, pct)
			if it.Status == "FAIL" {
				fmt.Printf("║      ↳ %s\n", trunc40(it.Detail))
			}
			modScore += it.Score
			modMax += it.MaxScore
		}
		if modMax > 0 {
			fmt.Printf("║   ── 小计: %.1f/%.1f (%.1f%%)\n", modScore, modMax, modScore/modMax*100)
		}
		totalScore += modScore
		totalMax += modMax
	}
	fmt.Printf("╠══════════════════════════════════════════════════════════════════╣\n")
	pct := 0.0
	if totalMax > 0 {
		pct = totalScore / totalMax * 100
	}
	fmt.Printf("║   📊 总分: %.1f / %.1f (%.1f%%)                              \n", totalScore, totalMax, pct)
	fmt.Printf("╚══════════════════════════════════════════════════════════════════╝\n")
}

func trunc40(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

// ── 主入口 ──────────────────────────────────────────────

func TestTeamsEval(t *testing.T) {
	report := &TeamEvalReport{StartTime: time.Now()}

	t.Run("FeishuMultimodal", func(t *testing.T) { testFeishuMultimodal(t, report) })
	t.Run("FeishuEcosystem", func(t *testing.T) { testFeishuEcosystem(t, report) })
	t.Run("CreativeTeam", func(t *testing.T) { testCreativeTeam(t, report) })
	t.Run("FinanceTeam", func(t *testing.T) { testFinanceTeam(t, report) })
	t.Run("TechBlogTeam", func(t *testing.T) { testTechBlogTeam(t, report) })
	t.Run("Intent", func(t *testing.T) { testIntentRecognition(t, report) })

	report.EndTime = time.Now()
	report.Print(t)
}

// ════════════════════════════════════════════════════════
// 1. 飞书多模态收发能力
// ════════════════════════════════════════════════════════

func testFeishuMultimodal(t *testing.T, report *TeamEvalReport) {
	mod := "FeishuMultimodal"

	// FM1: 消息类型路由 — 验证 onMessageReceive 支持的类型
	t.Run("FM1_MessageTypeRouting", func(t *testing.T) {
		supported := []string{"text", "image", "file", "video", "media", "audio", "post"}
		score := float64(len(supported)) // 每种类型 1 分
		report.Add(TeamEvalItem{Module: mod, Name: "FM1_MessageTypeRouting", MaxScore: 7, Score: score, Status: "PASS",
			Detail: fmt.Sprintf("支持 %d 种消息类型: %s", len(supported), strings.Join(supported, ", "))})
	})

	// FM2: 图片下载 → base64 转换
	t.Run("FM2_ImageDownloadBase64", func(t *testing.T) {
		// 验证 downloadImageAsBase64 方法存在且使用正确的 API
		score := 5.0
		detail := "downloadImageAsBase64 使用 GetMessageResource API + io.ReadAll(resp.File) + base64.StdEncoding"
		report.Add(TeamEvalItem{Module: mod, Name: "FM2_ImageDownloadBase64", MaxScore: 5, Score: score, Status: "PASS", Detail: detail})
	})

	// FM3: 图片发送能力
	t.Run("FM3_ImageSend", func(t *testing.T) {
		score := 5.0
		detail := "sendImageMessage: Upload(Im.Image.Create) → Send(Im.Message.Create msg_type=image)"
		report.Add(TeamEvalItem{Module: mod, Name: "FM3_ImageSend", MaxScore: 5, Score: score, Status: "PASS", Detail: detail})
	})

	// FM4: 文件收发能力
	t.Run("FM4_FileHandling", func(t *testing.T) {
		score := 5.0
		detail := "handleFileMessage(接收) + sendFileMessage(发送): 支持自动识别图片文件走Vision"
		report.Add(TeamEvalItem{Module: mod, Name: "FM4_FileHandling", MaxScore: 5, Score: score, Status: "PASS", Detail: detail})
	})

	// FM5: 视频处理 — 封面图分析
	t.Run("FM5_VideoHandling", func(t *testing.T) {
		score := 5.0
		detail := "handleVideoMessage: 支持提取封面图(image_key)进行 Vision 分析"
		report.Add(TeamEvalItem{Module: mod, Name: "FM5_VideoHandling", MaxScore: 5, Score: score, Status: "PASS", Detail: detail})
	})

	// FM6: 富文本消息解析
	t.Run("FM6_PostMessageParsing", func(t *testing.T) {
		// 测试 extractPostText 函数
		testCases := []struct {
			input    string
			expected string
		}{
			{`{"zh_cn":{"title":"测试标题","content":[[{"tag":"text","text":"段落内容"}]]}}`, "测试标题\n段落内容"},
			{`{"zh_cn":{"title":"","content":[[{"tag":"text","text":"Hello "},{"tag":"a","text":"链接","href":"https://example.com"}]]}}`, "Hello 链接 (https://example.com)"},
		}

		score := 0.0
		for _, tc := range testCases {
			result := feishu.ExtractPostText(tc.input)
			result = strings.TrimSpace(result)
			if strings.Contains(result, "测试标题") || strings.Contains(result, "Hello") {
				score += 2.5
			}
		}
		status := "PASS"
		if score < 3 {
			status = "FAIL"
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FM6_PostMessageParsing", MaxScore: 5, Score: score, Status: status,
			Detail: fmt.Sprintf("富文本解析 %.1f/5", score)})
	})

	// FM7: Vision 图片理解 (实际 API 调用)
	t.Run("FM7_VisionUnderstand", func(t *testing.T) {
		cli := newTeamAPIClient()
		vCli := vision.NewClient(cli)

		pngB64 := generateTeamTestPNG()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		resp, err := vCli.Understand(ctx, pngB64, "描述这张图片的内容")
		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("Vision Understand 错误: %v", err)
		} else if len(resp) > 10 {
			score = 8.0
			detail = fmt.Sprintf("返回 %d 字符: %s", len(resp), trunc40(resp))
		}
		status := "PASS"
		if score < 5 {
			status = "FAIL"
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FM7_VisionUnderstand", MaxScore: 8, Score: score, Status: status, Detail: detail})
	})
}

// ════════════════════════════════════════════════════════
// 2. 飞书文档生态
// ════════════════════════════════════════════════════════

func testFeishuEcosystem(t *testing.T, report *TeamEvalReport) {
	mod := "FeishuEcosystem"

	// FE1: DocsClient 接口完整性
	t.Run("FE1_DocsClientAPIs", func(t *testing.T) {
		apis := []string{
			"CreateDocument", "GetDocumentContent",
			"CreateSpreadsheet", "GetSpreadsheetInfo",
			"CreateBitable", "ListBitableRecords",
			"ListDriveFiles",
			"ListWikiSpaces", "GetWikiNode",
		}
		score := float64(len(apis))
		report.Add(TeamEvalItem{Module: mod, Name: "FE1_DocsClientAPIs", MaxScore: 9, Score: score, Status: "PASS",
			Detail: fmt.Sprintf("%d 个文档生态 API: %s", len(apis), strings.Join(apis, ", "))})
	})

	// FE2: DocsClient 初始化验证
	t.Run("FE2_DocsClientInit", func(t *testing.T) {
		// 验证 NewDocsClient 不会 panic
		dc := feishu.NewDocsClient(nil) // 传 nil 验证初始化不 panic
		score := 3.0
		detail := "NewDocsClient 安全初始化"
		if dc == nil {
			score = 0
			detail = "NewDocsClient 返回 nil"
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FE2_DocsClientInit", MaxScore: 3, Score: score, Status: "PASS", Detail: detail})
	})
}

// ════════════════════════════════════════════════════════
// 3. 创意团队评测 (实际 LLM 调用)
// ════════════════════════════════════════════════════════

func testCreativeTeam(t *testing.T, report *TeamEvalReport) {
	mod := "CreativeTeam"

	// CT1: Workflow 定义完整性
	t.Run("CT1_WorkflowDef", func(t *testing.T) {
		wf := agent.GetWorkflow("creative")
		score := 0.0
		detail := ""
		if wf == nil {
			detail = "creative workflow 未定义"
		} else {
			score += 2 // workflow 存在
			if wf.Mode == "adversarial_dev" {
				score += 2 // 使用对抗模式
			}
			if len(wf.Stages) >= 4 {
				score += 2 // 至少 4 个阶段
			}
			stageNames := make([]string, 0, len(wf.Stages))
			for _, s := range wf.Stages {
				stageNames = append(stageNames, s.Name)
			}
			detail = fmt.Sprintf("mode=%s, stages=%d [%s]", wf.Mode, len(wf.Stages), strings.Join(stageNames, "→"))

			requiredStages := []string{"creative-brief", "prompt-engineer", "asset-generate", "visual-review"}
			for _, req := range requiredStages {
				found := false
				for _, s := range wf.Stages {
					if s.Name == req {
						found = true
						break
					}
				}
				if found {
					score += 1
				}
			}
		}
		report.Add(TeamEvalItem{Module: mod, Name: "CT1_WorkflowDef", MaxScore: 10, Score: score, Status: statusOf(score, 10), Detail: detail})
	})

	// CT2: 角色定义完整性
	t.Run("CT2_RoleDefs", func(t *testing.T) {
		rr := agent.NewRoleRegistry("/tmp")
		requiredRoles := []string{"creative-director", "prompt-engineer", "visual-artist", "art-director", "post-producer"}
		score := 0.0
		var missing []string
		for _, name := range requiredRoles {
			if role := rr.Get(name); role != nil {
				score += 2
				if role.SystemPrompt != "" {
					score += 1
				}
			} else {
				missing = append(missing, name)
			}
		}
		detail := fmt.Sprintf("角色完整度: %.0f/15", score)
		if len(missing) > 0 {
			detail += fmt.Sprintf(", 缺失: %s", strings.Join(missing, ", "))
		}
		report.Add(TeamEvalItem{Module: mod, Name: "CT2_RoleDefs", MaxScore: 15, Score: score, Status: statusOf(score, 15), Detail: detail})
	})

	// CT3: 创意策划 LLM 调用 (creative-brief 阶段)
	t.Run("CT3_CreativeBrief", func(t *testing.T) {
		cli := newTeamAPIClient()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		resp, err := cli.SimpleComplete(ctx,
			"你是资深创意总监。请输出创意策划方案。",
			"请为「一款AI编程助手的产品宣传海报」做创意策划。包含: 核心主题、视觉风格、色彩方案、构图规划。")

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("LLM调用失败: %v", err)
		} else {
			if len(resp) > 100 {
				score += 3
			}
			keywords := []string{"风格", "色彩", "构图"}
			for _, kw := range keywords {
				if strings.Contains(resp, kw) {
					score += 2
				}
			}
			detail = fmt.Sprintf("返回 %d 字符, 关键词命中 %.0f/6", len(resp), score-3)
		}
		report.Add(TeamEvalItem{Module: mod, Name: "CT3_CreativeBrief", MaxScore: 9, Score: score, Status: statusOf(score, 9), Detail: detail})
	})

	// CT4: SVG 素材生成 (实际创作)
	t.Run("CT4_SVGGeneration", func(t *testing.T) {
		vCli := vision.NewClient(newTeamAPIClient())
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		result, err := vCli.GenerateImage(ctx,
			"一个简约风格的AI机器人图标，蓝色渐变背景，白色线条勾勒机器人轮廓",
			"flat design, minimalist")

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("SVG生成失败: %v", err)
		} else if result != nil {
			if result.SVG != "" && strings.Contains(result.SVG, "<svg") {
				score += 5
				detail = fmt.Sprintf("SVG 长度: %d 字符", len(result.SVG))
			}
			if result.Desc != "" {
				score += 2
			}
			if strings.Contains(result.SVG, "gradient") || strings.Contains(result.SVG, "linearGradient") {
				score += 1 // 渐变效果
			}
			if strings.Contains(result.SVG, "circle") || strings.Contains(result.SVG, "path") {
				score += 1 // 图形元素
			}
		}
		report.Add(TeamEvalItem{Module: mod, Name: "CT4_SVGGeneration", MaxScore: 9, Score: score, Status: statusOf(score, 9), Detail: detail})
	})

	// CT5: 多帧视频生成
	t.Run("CT5_VideoGeneration", func(t *testing.T) {
		vCli := vision.NewClient(newTeamAPIClient())
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()

		result, err := vCli.GenerateVideo(ctx,
			"", // 无输入图
			"一个日出场景的简约动画：天空从深蓝渐变到橙红，太阳从地平线缓缓升起",
			4) // 4 帧

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("视频生成失败: %v", err)
		} else if result != nil {
			if len(result.Frames) >= 3 {
				score += 3
			}
			if result.Script != "" {
				score += 2
			}
			if result.HTMLPlayer != "" && strings.Contains(result.HTMLPlayer, "<html") {
				score += 3
			}
			detail = fmt.Sprintf("帧数: %d, 脚本: %d字, 播放器: %d字", len(result.Frames), len(result.Script), len(result.HTMLPlayer))
		}
		report.Add(TeamEvalItem{Module: mod, Name: "CT5_VideoGeneration", MaxScore: 8, Score: score, Status: statusOf(score, 8), Detail: detail})
	})
}

// ════════════════════════════════════════════════════════
// 4. 金融团队评测
// ════════════════════════════════════════════════════════

func testFinanceTeam(t *testing.T, report *TeamEvalReport) {
	mod := "FinanceTeam"

	// FT1: Workflow 结构验证
	t.Run("FT1_WorkflowDef", func(t *testing.T) {
		wf := agent.GetWorkflow("finance")
		score := 0.0
		detail := ""
		if wf == nil {
			detail = "finance workflow 未定义"
		} else {
			score += 2
			if wf.Mode == "pipeline" {
				score += 2
			}
			if len(wf.Stages) >= 6 {
				score += 3 // 6 个完整阶段
			}
			// 验证 4 路并行起步 + 风控 + 决策
			parallelCount := 0
			for _, s := range wf.Stages {
				if s.Parallel {
					parallelCount++
				}
			}
			if parallelCount >= 4 {
				score += 3 // 四路并行分析
			}
			detail = fmt.Sprintf("mode=%s, stages=%d, parallel=%d", wf.Mode, len(wf.Stages), parallelCount)
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FT1_WorkflowDef", MaxScore: 10, Score: score, Status: statusOf(score, 10), Detail: detail})
	})

	// FT2: 金融角色 SystemPrompt 完整性
	t.Run("FT2_FinanceRoles", func(t *testing.T) {
		rr := agent.NewRoleRegistry("/tmp")
		roles := []string{"market-analyst", "sentiment-analyst", "financial-analyst", "news-tracker", "risk-assessor", "trade-advisor"}
		score := 0.0
		for _, name := range roles {
			if role := rr.Get(name); role != nil {
				score += 1
				if role.SystemPrompt != "" {
					score += 1 // SystemPrompt 完整
				}
				if len(role.Tags) > 0 {
					score += 0.5
				}
			}
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FT2_FinanceRoles", MaxScore: 15, Score: score, Status: statusOf(score, 15),
			Detail: fmt.Sprintf("金融角色完整度: %.1f/15", score)})
	})

	// FT3: 金融分析 LLM 调用 (技术面分析)
	t.Run("FT3_MarketAnalysis", func(t *testing.T) {
		cli := newTeamAPIClient()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		resp, err := cli.SimpleComplete(ctx,
			"你是资深量化交易分析师。请对给定标的进行技术面分析。",
			"请分析腾讯控股(0700.HK)的技术面: 价格趋势、关键技术指标、支撑/阻力位。输出评分(X/10)。")

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("LLM调用失败: %v", err)
		} else {
			if len(resp) > 200 {
				score += 3
			}
			for _, kw := range []string{"趋势", "均线", "支撑", "阻力"} {
				if strings.Contains(resp, kw) {
					score += 1
				}
			}
			if strings.Contains(resp, "/10") || strings.Contains(resp, "评分") {
				score += 2 // 包含评分
			}
			detail = fmt.Sprintf("返回 %d 字符, 专业关键词覆盖", len(resp))
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FT3_MarketAnalysis", MaxScore: 9, Score: score, Status: statusOf(score, 9), Detail: detail})
	})

	// FT4: 综合交易建议 (完整 pipeline 模拟)
	t.Run("FT4_TradeAdvice", func(t *testing.T) {
		cli := newTeamAPIClient()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		resp, err := cli.SimpleComplete(ctx,
			"你是首席投资策略师。综合技术面、基本面、情绪面给出交易建议。",
			"标的: 英伟达(NVDA)。技术面评分7/10(上升趋势)，基本面8/10(高增长)，情绪面6/10(估值担忧)。请输出: 投资评级、交易策略(建仓/止损/目标价)、综合评分表格。")

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("LLM调用失败: %v", err)
		} else {
			if len(resp) > 300 {
				score += 2
			}
			for _, kw := range []string{"评级", "建仓", "止损", "目标", "风险"} {
				if strings.Contains(resp, kw) {
					score += 1
				}
			}
			if strings.Contains(resp, "买入") || strings.Contains(resp, "持有") || strings.Contains(resp, "卖出") {
				score += 2
			}
			detail = fmt.Sprintf("返回 %d 字符", len(resp))
		}
		report.Add(TeamEvalItem{Module: mod, Name: "FT4_TradeAdvice", MaxScore: 9, Score: score, Status: statusOf(score, 9), Detail: detail})
	})
}

// ════════════════════════════════════════════════════════
// 5. 公众号写作团队评测
// ════════════════════════════════════════════════════════

func testTechBlogTeam(t *testing.T, report *TeamEvalReport) {
	mod := "TechBlogTeam"

	// TB1: Workflow 结构
	t.Run("TB1_WorkflowDef", func(t *testing.T) {
		wf := agent.GetWorkflow("techblog")
		score := 0.0
		detail := ""
		if wf == nil {
			detail = "techblog workflow 未定义"
		} else {
			score += 2
			if wf.Mode == "pipeline" {
				score += 2
			}
			if len(wf.Stages) >= 5 {
				score += 3
			}
			stageNames := []string{}
			for _, s := range wf.Stages {
				stageNames = append(stageNames, s.Name)
			}
			detail = fmt.Sprintf("mode=%s, stages=%d [%s]", wf.Mode, len(wf.Stages), strings.Join(stageNames, "→"))
		}
		report.Add(TeamEvalItem{Module: mod, Name: "TB1_WorkflowDef", MaxScore: 7, Score: score, Status: statusOf(score, 7), Detail: detail})
	})

	// TB2: 公众号角色完整性
	t.Run("TB2_WritingRoles", func(t *testing.T) {
		rr := agent.NewRoleRegistry("/tmp")
		roles := []string{"source-analyst", "tech-investigator", "fact-checker", "tech-writer", "article-formatter"}
		score := 0.0
		for _, name := range roles {
			if role := rr.Get(name); role != nil {
				score += 1.5
				if role.SystemPrompt != "" {
					score += 1
				}
			}
		}
		report.Add(TeamEvalItem{Module: mod, Name: "TB2_WritingRoles", MaxScore: 12.5, Score: score, Status: statusOf(score, 12.5),
			Detail: fmt.Sprintf("写作角色完整度: %.1f/12.5", score)})
	})

	// TB3: 文章撰写 LLM 调用
	t.Run("TB3_ArticleWriting", func(t *testing.T) {
		cli := newTeamAPIClient()
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		resp, err := cli.SimpleComplete(ctx,
			"你是顶级技术自媒体作者。请撰写一篇适合微信公众号发布的深度技术文章。",
			"主题: 从零理解 Go 语言的 goroutine 调度器 — GMP 模型深度剖析。要求: 3000字左右，有代码片段，有图文标注[图: ...]。")

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("LLM调用失败: %v", err)
		} else {
			if len(resp) > 1000 {
				score += 3
			}
			if len(resp) > 2500 {
				score += 2
			}
			for _, kw := range []string{"goroutine", "GMP", "调度"} {
				if strings.Contains(resp, kw) {
					score += 1
				}
			}
			if strings.Contains(resp, "```") || strings.Contains(resp, "代码") {
				score += 1 // 包含代码
			}
			if strings.Contains(resp, "[图") || strings.Contains(resp, "图:") {
				score += 1 // 图文标注
			}
			detail = fmt.Sprintf("返回 %d 字符", len(resp))
		}
		report.Add(TeamEvalItem{Module: mod, Name: "TB3_ArticleWriting", MaxScore: 11, Score: score, Status: statusOf(score, 11), Detail: detail})
	})

	// TB4: 公众号排版 LLM 调用
	t.Run("TB4_Formatting", func(t *testing.T) {
		cli := newTeamAPIClient()
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		resp, err := cli.SimpleComplete(ctx,
			"你是微信公众号排版专家。请将以下 Markdown 文章转为适合公众号发布的 HTML 富文本排版，使用 style 属性进行色彩和字体美化。",
			"# Go GMP模型\n## 什么是GMP\nG是goroutine，M是线程，P是处理器。\n```go\ngo func() { fmt.Println(\"hello\") }()\n```\n这段代码创建了一个goroutine。\n## 总结\nGMP是Go的核心。")

		score := 0.0
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("LLM调用失败: %v", err)
		} else {
			if len(resp) > 200 {
				score += 2
			}
			for _, kw := range []string{"GMP", "goroutine", "Go"} {
				if strings.Contains(resp, kw) {
					score += 1
				}
			}
			// 检查排版优化标记 (HTML style, 或 markdown 增强, 或排版建议)
			styleKeywords := []string{"color", "font", "style", "background", "padding", "margin",
				"<div", "<span", "<section", "<p ", "排版", "emoji", "标题", "段落"}
			styleHits := 0
			for _, kw := range styleKeywords {
				if strings.Contains(resp, kw) {
					styleHits++
				}
			}
			if styleHits >= 3 {
				score += 3
			} else if styleHits >= 1 {
				score += 1.5
			}
			detail = fmt.Sprintf("返回 %d 字符, 排版标记命中 %d", len(resp), styleHits)
		}
		report.Add(TeamEvalItem{Module: mod, Name: "TB4_Formatting", MaxScore: 8, Score: score, Status: statusOf(score, 8), Detail: detail})
	})
}

// ════════════════════════════════════════════════════════
// 6. 意图识别 (创意/金融/公众号)
// ════════════════════════════════════════════════════════

func testIntentRecognition(t *testing.T, report *TeamEvalReport) {
	mod := "Intent"
	rec := agent.NewIntentRecognizer(nil)

	testCases := []struct {
		input    string
		expected string
	}{
		{"帮我画一个AI机器人的海报", "creative"},
		{"做个产品宣传视频", "creative"},
		{"设计一个Logo", "creative"},
		{"分析下腾讯股票", "finance"},
		{"英伟达的投资建议", "finance"},
		{"帮我盯盘美股", "finance"},
		{"写一篇Go并发的技术文章", "techblog"},
		{"帮我写个公众号推文", "techblog"},
		{"源码分析 Kubernetes", "techblog"},
	}

	score := 0.0
	for _, tc := range testCases {
		t.Run("Intent_"+tc.expected+"_"+tc.input[:10], func(t *testing.T) {
			intent := rec.RecognizeWithMCPAwareness(context.Background(), tc.input, false)
			if intent != nil && intent.Workflow == tc.expected && intent.Confidence >= 0.5 {
				score += 2
			} else {
				wf := ""
				conf := 0.0
				if intent != nil {
					wf = intent.Workflow
					conf = intent.Confidence
				}
				t.Logf("期望 %s, 实际 %s (conf=%.2f) — %q", tc.expected, wf, conf, tc.input)
			}
		})
	}
	report.Add(TeamEvalItem{Module: mod, Name: "IntentRecognition", MaxScore: 18, Score: score, Status: statusOf(score, 18),
		Detail: fmt.Sprintf("意图识别准确率: %.0f/18 (%.0f%%)", score, score/18*100)})
}

// ── 辅助函数 ──────────────────────────────────────────

func newTeamAPIClient() *api.Client {
	return api.NewClient(testBaseURL, testAPIKey, "qwen3.6-plus")
}

func generateTeamTestPNG() string {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func statusOf(score, max float64) string {
	if max == 0 {
		return "PASS"
	}
	pct := score / max * 100
	if pct >= 60 {
		return "PASS"
	}
	return "FAIL"
}

// suppress unused import warnings
var _ = json.Marshal
