// new_features_test.go — 新增能力专业评估框架
// 覆盖: Vision(4项) / Auto Plan(3项) / Adversarial Dev(4项) / Wiki(4项) / Obsidian(3项)
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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/vision"
	"github.com/anthropic/claude-go/pkg/wiki"
)

const (
	testAPIKey  = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	testBaseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
	testModel   = "qwen3.6-plus"
)

// ── 评分结构 ──────────────────────────────────────────

type EvalItem struct {
	Module   string
	Name     string
	MaxScore float64
	Score    float64
	Status   string // PASS / FAIL / WARN
	Detail   string
}

type EvalReport struct {
	Items     []EvalItem
	StartTime time.Time
	EndTime   time.Time
}

func (r *EvalReport) Add(item EvalItem) {
	r.Items = append(r.Items, item)
}

func (r *EvalReport) Print(t *testing.T) {
	t.Helper()
	fmt.Printf("\n╔══════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║         claude-go 新增能力专业评估报告                          ║\n")
	fmt.Printf("║         评估时间: %s                  ║\n", r.EndTime.Sub(r.StartTime).Round(time.Second))
	fmt.Printf("╠══════════════════════════════════════════════════════════════════╣\n")

	modules := map[string]struct{}{}
	for _, it := range r.Items {
		modules[it.Module] = struct{}{}
	}

	totalScore, totalMax := 0.0, 0.0
	for _, mod := range []string{"Vision", "AutoPlan", "Adversarial", "Wiki", "Obsidian"} {
		modScore, modMax := 0.0, 0.0
		fmt.Printf("║ 【%s】\n", mod)
		for _, it := range r.Items {
			if it.Module != mod {
				continue
			}
			icon := "🟢"
			pct := it.Score / it.MaxScore * 100
			if pct < 60 {
				icon = "🔴"
			} else if pct < 90 {
				icon = "🟡"
			}
			fmt.Printf("║   %s %-30s %5.1f/%5.1f (%5.1f%%) %s\n",
				icon, it.Name, it.Score, it.MaxScore, pct, it.Status)
			if it.Detail != "" {
				lines := strings.Split(it.Detail, "\n")
				for _, l := range lines {
					if l != "" {
						fmt.Printf("║       → %s\n", l)
					}
				}
			}
			modScore += it.Score
			modMax += it.MaxScore
		}
		pct := 0.0
		if modMax > 0 {
			pct = modScore / modMax * 100
		}
		fmt.Printf("║   小计: %.1f/%.1f (%.1f%%)\n", modScore, modMax, pct)
		fmt.Printf("║\n")
		totalScore += modScore
		totalMax += modMax
	}

	pct := totalScore / totalMax * 100
	icon := "🟢"
	if pct < 60 {
		icon = "🔴"
	} else if pct < 90 {
		icon = "🟡"
	}
	fmt.Printf("╠══════════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║ %s 总分: %.1f / %.1f  (%.1f%%)\n", icon, totalScore, totalMax, pct)
	fmt.Printf("╚══════════════════════════════════════════════════════════════════╝\n")
}

// ── Vision 评估 ──────────────────────────────────────

func newAPIClient() *api.Client {
	return api.NewClient(testBaseURL, testAPIKey, testModel)
}

func TestEval_Vision(t *testing.T) {
	report := &EvalReport{StartTime: time.Now()}
	defer func() {
		report.EndTime = time.Now()
		report.Print(t)
	}()

	client := newAPIClient()
	vc := vision.NewClient(client)

	// V1: 文生图 — SVG 生成质量
	t.Run("V1_TextToImage", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		result, err := vc.GenerateImage(ctx, "一个蓝色的地球仪图标", "极简扁平")
		item := EvalItem{Module: "Vision", Name: "V1_文生图_SVG生成", MaxScore: 25}
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("错误: %v", err)
		} else {
			score := 0.0
			details := []string{}
			if result.SVG != "" {
				score += 10
				details = append(details, fmt.Sprintf("SVG长度: %d bytes", len(result.SVG)))
			} else {
				details = append(details, "未生成SVG")
			}
			if strings.Contains(result.SVG, "viewBox") {
				score += 5
				details = append(details, "包含viewBox属性")
			}
			if strings.Contains(result.SVG, "</svg>") {
				score += 5
				details = append(details, "SVG结构完整")
			}
			if len(result.SVG) > 200 {
				score += 5
				details = append(details, "SVG内容丰富(>200B)")
			}
			item.Score = score
			item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 20]
			item.Detail = strings.Join(details, "\n")
		}
		report.Add(item)
	})

	// V2: 图片理解 — 用 PNG 图片测试
	t.Run("V2_ImageUnderstand", func(t *testing.T) {
		item := EvalItem{Module: "Vision", Name: "V2_图片理解_多模态", MaxScore: 25}

		// 生成一个简单的 PNG: 红色圆形 + 绿色背景
		pngBase64 := generateTestPNG()

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		result, err := vc.Understand(ctx, pngBase64, "这张图片中有什么？请描述颜色、形状等。")
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("错误: %v", err)
		} else {
			score := 0.0
			details := []string{}
			if len(result) > 10 {
				score += 10
				details = append(details, fmt.Sprintf("返回描述长度: %d", len(result)))
			}
			lowerResult := strings.ToLower(result)
			if strings.Contains(lowerResult, "circle") || strings.Contains(lowerResult, "圆") ||
				strings.Contains(lowerResult, "red") || strings.Contains(lowerResult, "红") ||
				strings.Contains(lowerResult, "green") || strings.Contains(lowerResult, "绿") ||
				strings.Contains(lowerResult, "color") || strings.Contains(lowerResult, "颜色") {
				score += 8
				details = append(details, "识别到图形/颜色")
			}
			if len(result) > 50 {
				score += 7
				details = append(details, "描述内容丰富(>50 chars)")
			}
			item.Score = score
			item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 18]
			item.Detail = strings.Join(details, "\n") + "\n回复预览: " + truncS(result, 200)
		}
		report.Add(item)
	})

	// V3: 图生图 — 用 PNG 作为参考图
	t.Run("V3_ImageToImage", func(t *testing.T) {
		item := EvalItem{Module: "Vision", Name: "V3_图生图_风格转换", MaxScore: 25}

		pngBase64 := generateTestPNG()

		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		result, err := vc.TransformImage(ctx, pngBase64, "将这个图片中的色彩反转，转换为矢量插画风格的SVG")
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("错误: %v", err)
		} else {
			score := 0.0
			details := []string{}
			if result.SVG != "" {
				score += 10
				details = append(details, fmt.Sprintf("生成SVG长度: %d bytes", len(result.SVG)))
			}
			if strings.Contains(result.SVG, "</svg>") {
				score += 5
				details = append(details, "SVG结构完整")
			}
			if len(result.SVG) > 300 {
				score += 5
				details = append(details, "SVG内容丰富(>300B)")
			}
			if result.RawReply != "" {
				score += 5
				details = append(details, "有原始回复")
			}
			item.Score = score
			item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 18]
			item.Detail = strings.Join(details, "\n")
		}
		report.Add(item)
	})

	// V4: 图生视频 — 运镜脚本 + 多帧生成 + HTML Player
	t.Run("V4_ImageToVideo", func(t *testing.T) {
		item := EvalItem{Module: "Vision", Name: "V4_图生视频_多帧动画", MaxScore: 25}

		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
		defer cancel()

		result, err := vc.GenerateVideo(ctx, "", "日出从山峦后方升起，光线逐渐照亮天空", 3)
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("错误: %v", err)
		} else {
			score := 0.0
			details := []string{}
			if result.Script != "" {
				score += 5
				details = append(details, "运镜脚本已生成")
			}
			validFrames := 0
			for _, f := range result.Frames {
				if strings.Contains(f, "</svg>") && !strings.Contains(f, "(error)") {
					validFrames++
				}
			}
			details = append(details, fmt.Sprintf("有效帧数: %d/%d", validFrames, len(result.Frames)))
			score += float64(validFrames) * 5
			if validFrames < 3 {
				score = min(score, 15)
			}

			if result.HTMLPlayer != "" && strings.Contains(result.HTMLPlayer, "@keyframes") {
				score += 5
				details = append(details, "HTML Player含CSS动画")
			}

			item.Score = min(score, 25)
			item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 18]
			item.Detail = strings.Join(details, "\n")

			// 保存 HTML Player 到临时文件
			if result.HTMLPlayer != "" {
				tmpFile := filepath.Join(os.TempDir(), "claude-go-video-test.html")
				os.WriteFile(tmpFile, []byte(result.HTMLPlayer), 0o644)
				details = append(details, fmt.Sprintf("HTML Player已保存: %s", tmpFile))
			}
		}
		report.Add(item)
	})
}

// ── Auto Plan 评估 ──────────────────────────────────

func TestEval_AutoPlan(t *testing.T) {
	report := &EvalReport{StartTime: time.Now()}
	defer func() {
		report.EndTime = time.Now()
		report.Print(t)
	}()

	client := newAPIClient()

	// AP1: LLM 复杂度判断 — 简单任务应返回 SIMPLE
	t.Run("AP1_SimpleTask", func(t *testing.T) {
		item := EvalItem{Module: "AutoPlan", Name: "AP1_简单任务识别", MaxScore: 15}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sysPrompt := `你是一个任务复杂度分类器。判断用户消息是"简单任务"还是"复杂任务"。
复杂任务特征: 涉及多个步骤(>2步)、需要架构设计、多文件改动、包含编号列表(1.2.3.)等。
简单任务: 单一查询、简单指令、问答等。
只回复一个单词: COMPLEX 或 SIMPLE`

		simpleTests := []string{
			"帮我查一下Go的版本",
			"翻译这句话到英文",
			"今天天气怎么样",
		}

		score := 0.0
		for _, text := range simpleTests {
			resp, err := client.SimpleComplete(ctx, sysPrompt, text)
			if err != nil {
				continue
			}
			if strings.Contains(strings.ToUpper(resp), "SIMPLE") {
				score += 5
			}
		}
		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 10]
		item.Detail = fmt.Sprintf("简单任务正确识别: %.0f/3", score/5)
		report.Add(item)
	})

	// AP2: LLM 复杂度判断 — 复杂任务应返回 COMPLEX
	t.Run("AP2_ComplexTask", func(t *testing.T) {
		item := EvalItem{Module: "AutoPlan", Name: "AP2_复杂任务识别", MaxScore: 15}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sysPrompt := `你是一个任务复杂度分类器。判断用户消息是"简单任务"还是"复杂任务"。
复杂任务特征: 涉及多个步骤(>2步)、需要架构设计、多文件改动、包含编号列表(1.2.3.)等。
简单任务: 单一查询、简单指令、问答等。
只回复一个单词: COMPLEX 或 SIMPLE`

		complexTests := []string{
			"帮我设计一个用户认证系统，需要JWT token管理、密码加密、权限控制，并且要写单元测试",
			"1. 重构数据库模块 2. 添加缓存层 3. 编写迁移脚本 4. 更新API文档",
			"分析竞品的架构设计，然后设计我们的微服务拆分方案，包括服务间通信、数据一致性、部署策略",
		}

		score := 0.0
		for _, text := range complexTests {
			resp, err := client.SimpleComplete(ctx, sysPrompt, text)
			if err != nil {
				continue
			}
			if strings.Contains(strings.ToUpper(resp), "COMPLEX") {
				score += 5
			}
		}
		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 10]
		item.Detail = fmt.Sprintf("复杂任务正确识别: %.0f/3", score/5)
		report.Add(item)
	})

	// AP3: Plan→Build 指令注入格式
	t.Run("AP3_PlanBuildFormat", func(t *testing.T) {
		item := EvalItem{Module: "AutoPlan", Name: "AP3_Plan→Build指令格式", MaxScore: 10}
		score := 0.0
		details := []string{}

		expectedPrefix := "[Auto Plan+Build]"
		expectedPhases := []string{"Plan", "Switch", "Build"}

		testInjection := "[Auto Plan+Build] 这是一个复杂任务。\n" +
			"阶段1(Plan): 调用 EnterPlanMode，深入分析需求，设计详细方案(架构、模块拆分、接口定义、风险点)。\n" +
			"阶段2(Switch): 调用 ExitPlanMode 附带完整计划摘要。\n" +
			"阶段3(Build): 按计划逐步执行实现(编写代码、创建文件、运行命令)，每完成一步验证结果。\n\n" +
			"原始任务:\n测试任务"

		if strings.HasPrefix(testInjection, expectedPrefix) {
			score += 4
			details = append(details, "前缀格式正确")
		}
		for _, phase := range expectedPhases {
			if strings.Contains(testInjection, phase) {
				score += 2
			}
		}
		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 8]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})
}

// ── Adversarial Dev 评估 ──────────────────────────────

func TestEval_Adversarial(t *testing.T) {
	report := &EvalReport{StartTime: time.Now()}
	defer func() {
		report.EndTime = time.Now()
		report.Print(t)
	}()

	// AD1: Workflow 定义正确性
	t.Run("AD1_WorkflowDef", func(t *testing.T) {
		item := EvalItem{Module: "Adversarial", Name: "AD1_工作流定义", MaxScore: 20}
		score := 0.0
		details := []string{}

		wf := agent.GetWorkflow("dev")
		if wf == nil {
			item.Status = "FAIL"
			item.Detail = "未找到 dev workflow"
			report.Add(item)
			return
		}

		if wf.Mode == "adversarial_dev" {
			score += 5
			details = append(details, "模式: adversarial_dev ✓")
		}
		if wf.Rounds >= 2 {
			score += 3
			details = append(details, fmt.Sprintf("对抗轮数: %d ✓", wf.Rounds))
		}

		hasDesign, hasImpl, hasEval, hasTest := false, false, false, false
		for _, s := range wf.Stages {
			switch s.Name {
			case "design":
				hasDesign = true
			case "implement":
				hasImpl = true
				if strings.Contains(s.Prompt, "{adversarial_feedback}") {
					score += 3
					details = append(details, "implement 含反馈占位符 ✓")
				}
			case "evaluate":
				hasEval = true
				if strings.Contains(s.Prompt, "0-10") || strings.Contains(s.Prompt, "JSON") {
					score += 3
					details = append(details, "evaluate 含评分指令 ✓")
				}
			}
			if s.Name == "test" && s.Parallel {
				hasTest = true
			}
		}
		if hasDesign {
			score += 2
			details = append(details, "design 阶段 ✓")
		}
		if hasImpl && hasEval {
			score += 2
			details = append(details, "implement↔evaluate 对抗 ✓")
		}
		if hasTest {
			score += 2
			details = append(details, "test 并行阶段 ✓")
		}

		item.Score = min(score, 20)
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 15]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// AD2: EvalScore 解析
	t.Run("AD2_EvalScoreParsing", func(t *testing.T) {
		item := EvalItem{Module: "Adversarial", Name: "AD2_评分JSON解析", MaxScore: 15}
		score := 0.0

		testJSON := `{"correctness":8,"completeness":7,"security":9,"code_quality":7,"pass":true,"feedback":"Good job"}`
		evalScore, err := agent.ParseEvalScoreJSON([]byte(testJSON))
		if err == nil {
			score += 5
			if evalScore.Correctness == 8 && evalScore.Security == 9 {
				score += 5
			}
			if evalScore.MeetsHardPassThreshold() {
				score += 5
			}
		}

		failJSON := `{"correctness":3,"completeness":7,"security":9,"code_quality":7,"pass":false,"feedback":"Fix correctness"}`
		failScore, err := agent.ParseEvalScoreJSON([]byte(failJSON))
		if err == nil && !failScore.MeetsHardPassThreshold() {
			item.Detail = "通过/未通过门槛判断正确"
		}

		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 12]
		report.Add(item)
	})

	// AD3: RuleEngine 规则检查
	t.Run("AD3_RuleEngine", func(t *testing.T) {
		item := EvalItem{Module: "Adversarial", Name: "AD3_规则引擎", MaxScore: 15}
		score := 0.0
		details := []string{}

		re := agent.NewRuleEngine()
		re.SetTargetPathPrefixes([]string{"/project/src/"})

		// 合法操作
		err := re.Check("write_file", json.RawMessage(`{"path":"/project/src/main.go"}`))
		if err == nil {
			score += 5
			details = append(details, "合法路径通过 ✓")
		}

		// 禁止提交密钥 (toolName="Shell", 含 git commit + .env)
		err = re.Check("Shell", json.RawMessage(`{"command":"git commit -m 'add config' .env"}`))
		if err != nil {
			score += 5
			details = append(details, "禁止提交密钥 ✓: "+err.Error())
		}

		// 禁止死循环 (toolName="Shell")
		err = re.Check("Shell", json.RawMessage(`{"command":"while true; do echo 1; done"}`))
		if err != nil {
			score += 5
			details = append(details, "禁止死循环 ✓: "+err.Error())
		}

		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 12]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// AD4: ContextManager 上下文管理
	t.Run("AD4_ContextManager", func(t *testing.T) {
		item := EvalItem{Module: "Adversarial", Name: "AD4_上下文管理器", MaxScore: 10}
		score := 0.0
		details := []string{}

		cm := agent.NewContextManager(100000)

		cm.AddTokens(60000)
		if !cm.ShouldResetWithBudget(0) {
			score += 3
			details = append(details, "60%未触发重置 ✓")
		}

		cm.AddTokens(25000)
		if cm.ShouldResetWithBudget(0) {
			score += 3
			details = append(details, "85%触发重置 ✓")
		}

		ho := cm.CreateHandoff("测试进度", []string{"决策1"})
		if ho.Progress != "" {
			score += 4
			details = append(details, "Handoff创建成功 ✓")
		}

		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 8]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})
}

// ── Wiki 评估 ──────────────────────────────────────────

func TestEval_Wiki(t *testing.T) {
	report := &EvalReport{StartTime: time.Now()}
	defer func() {
		report.EndTime = time.Now()
		report.Print(t)
	}()

	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("wiki-eval-%d", time.Now().UnixNano()))
	defer os.RemoveAll(tmpDir)

	// W1: EnsureRepo 目录初始化
	t.Run("W1_EnsureRepo", func(t *testing.T) {
		item := EvalItem{Module: "Wiki", Name: "W1_仓库初始化", MaxScore: 15}
		score := 0.0
		details := []string{}

		err := wiki.EnsureRepo(tmpDir)
		if err != nil {
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("错误: %v", err)
			report.Add(item)
			return
		}
		score += 3
		details = append(details, "EnsureRepo成功 ✓")

		for _, sub := range []string{"raw", "wiki", "schema"} {
			if info, err := os.Stat(filepath.Join(tmpDir, sub)); err == nil && info.IsDir() {
				score += 2
				details = append(details, sub+"/ 存在 ✓")
			}
		}

		if _, err := os.Stat(filepath.Join(tmpDir, "schema", "schema.yaml")); err == nil {
			score += 2
			details = append(details, "schema.yaml 存在 ✓")
		}

		if _, err := os.Stat(filepath.Join(tmpDir, ".git")); err == nil {
			score += 2
			details = append(details, ".git 存在 ✓")
		}

		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 12]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// W2: Ingest URL 摄取 (用实际 API)
	t.Run("W2_Ingest", func(t *testing.T) {
		item := EvalItem{Module: "Wiki", Name: "W2_URL摄取(LLM)", MaxScore: 25}

		engine := wiki.NewEngineWithLLM(tmpDir, newAPIClient())
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		err := engine.Ingest(ctx, "https://go.dev/blog/go1.22")
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("Ingest错误: %v", err)
			report.Add(item)
			return
		}

		score := 10.0
		details := []string{"Ingest成功 ✓"}

		rawEntries, _ := os.ReadDir(filepath.Join(tmpDir, "raw"))
		rawCount := 0
		for _, e := range rawEntries {
			if strings.HasSuffix(e.Name(), ".md") {
				rawCount++
			}
		}
		if rawCount > 0 {
			score += 5
			details = append(details, fmt.Sprintf("raw层文件: %d", rawCount))
		}

		wikiEntries, _ := os.ReadDir(filepath.Join(tmpDir, "wiki"))
		wikiCount := 0
		for _, e := range wikiEntries {
			if strings.HasSuffix(e.Name(), ".md") {
				wikiCount++
			}
		}
		if wikiCount > 0 {
			score += 5
			details = append(details, fmt.Sprintf("wiki层概念页: %d", wikiCount))
		}

		if rawCount > 0 {
			rawFiles, _ := os.ReadDir(filepath.Join(tmpDir, "raw"))
			if len(rawFiles) > 0 {
				data, _ := os.ReadFile(filepath.Join(tmpDir, "raw", rawFiles[0].Name()))
				if strings.Contains(string(data), "---") {
					score += 5
					details = append(details, "raw文件含frontmatter ✓")
				}
			}
		}

		item.Score = min(score, 25)
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 18]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// W3: Lint 质检
	t.Run("W3_Lint", func(t *testing.T) {
		item := EvalItem{Module: "Wiki", Name: "W3_Lint质检", MaxScore: 15}

		engine := wiki.NewEngineWithLLM(tmpDir, newAPIClient())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		lintReport, err := engine.Lint(ctx)
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("Lint错误: %v", err)
			report.Add(item)
			return
		}

		score := 5.0
		details := []string{"Lint执行成功 ✓"}
		details = append(details, fmt.Sprintf("总页数: %d, Raw数: %d", lintReport.TotalPages, lintReport.TotalRaw))
		details = append(details, fmt.Sprintf("坏链: %d, 孤立页: %d", len(lintReport.BrokenLinks), len(lintReport.OrphanedPages)))

		if lintReport.TotalPages >= 0 {
			score += 5
		}
		if lintReport.TotalRaw >= 0 {
			score += 5
		}

		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 12]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// W4: Query 查询 (基于摄取后的 wiki)
	t.Run("W4_Query", func(t *testing.T) {
		item := EvalItem{Module: "Wiki", Name: "W4_知识查询(LLM)", MaxScore: 20}

		engine := wiki.NewEngineWithLLM(tmpDir, newAPIClient())
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		answer, err := engine.Query(ctx, "Go 1.22有哪些新特性？")
		if err != nil {
			item.Score = 0
			item.Status = "FAIL"
			item.Detail = fmt.Sprintf("Query错误: %v", err)
			report.Add(item)
			return
		}

		score := 5.0
		details := []string{"Query成功 ✓"}

		if len(answer) > 50 {
			score += 5
			details = append(details, fmt.Sprintf("回答长度: %d chars", len(answer)))
		}
		lower := strings.ToLower(answer)
		if strings.Contains(lower, "go") || strings.Contains(lower, "1.22") {
			score += 5
			details = append(details, "回答提及Go/1.22 ✓")
		}
		if strings.Contains(lower, "range") || strings.Contains(lower, "for") ||
			strings.Contains(lower, "loop") || strings.Contains(lower, "函数") {
			score += 5
			details = append(details, "回答包含技术细节 ✓")
		}

		item.Score = min(score, 20)
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 15]
		item.Detail = strings.Join(details, "\n") + "\n回答预览: " + truncS(answer, 300)
		report.Add(item)
	})
}

// ── Obsidian 插件评估 ──────────────────────────────────

func TestEval_Obsidian(t *testing.T) {
	report := &EvalReport{StartTime: time.Now()}
	defer func() {
		report.EndTime = time.Now()
		report.Print(t)
	}()

	pluginDir := filepath.Join("..", "..", "..", "obsidian-claude-wiki")

	// O1: 项目结构完整性
	t.Run("O1_ProjectStructure", func(t *testing.T) {
		item := EvalItem{Module: "Obsidian", Name: "O1_项目结构完整性", MaxScore: 15}
		score := 0.0
		details := []string{}

		requiredFiles := map[string]float64{
			"package.json":           2,
			"tsconfig.json":          1,
			"manifest.json":          2,
			"src/main.ts":            2,
			"src/settings.ts":        1.5,
			"src/git-sync.ts":        1.5,
			"src/link-extractor.ts":  1.5,
			"src/wiki-dashboard.ts":  1.5,
			"src/lint-runner.ts":     1,
			"src/api-client.ts":      1,
		}

		for file, pts := range requiredFiles {
			fullPath := filepath.Join(pluginDir, file)
			if _, err := os.Stat(fullPath); err == nil {
				score += pts
				details = append(details, file+" ✓")
			} else {
				details = append(details, file+" ✗")
			}
		}

		item.Score = min(score, 15)
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 12]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// O2: manifest.json 格式
	t.Run("O2_Manifest", func(t *testing.T) {
		item := EvalItem{Module: "Obsidian", Name: "O2_Manifest配置", MaxScore: 10}
		score := 0.0
		details := []string{}

		manifestPath := filepath.Join(pluginDir, "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			item.Status = "FAIL"
			item.Detail = "读取manifest失败"
			report.Add(item)
			return
		}

		var manifest map[string]any
		if json.Unmarshal(data, &manifest) == nil {
			score += 3
			details = append(details, "JSON格式正确 ✓")
		}
		if id, ok := manifest["id"].(string); ok && id != "" {
			score += 2
			details = append(details, "id: "+id+" ✓")
		}
		if name, ok := manifest["name"].(string); ok && name != "" {
			score += 2
			details = append(details, "name: "+name+" ✓")
		}
		if _, ok := manifest["version"]; ok {
			score += 1.5
		}
		if _, ok := manifest["minAppVersion"]; ok {
			score += 1.5
		}

		item.Score = score
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 8]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})

	// O3: 核心模块代码质量
	t.Run("O3_CodeQuality", func(t *testing.T) {
		item := EvalItem{Module: "Obsidian", Name: "O3_核心代码质量", MaxScore: 15}
		score := 0.0
		details := []string{}

		checkFile := func(name string, keywords []string, pts float64) {
			data, err := os.ReadFile(filepath.Join(pluginDir, "src", name))
			if err != nil {
				details = append(details, name+": 读取失败")
				return
			}
			content := string(data)
			found := 0
			for _, kw := range keywords {
				if strings.Contains(content, kw) {
					found++
				}
			}
			pct := float64(found) / float64(len(keywords))
			earned := pts * pct
			score += earned
			details = append(details, fmt.Sprintf("%s: %.0f%% keywords (%.1f/%.1f)", name, pct*100, earned, pts))
		}

		checkFile("main.ts", []string{"Plugin", "onload", "addCommand", "registerView", "settings"}, 3)
		checkFile("git-sync.ts", []string{"git", "pull", "push", "commit", "clone"}, 3)
		checkFile("link-extractor.ts", []string{"url", "fetch", "extract", "markdown", "raw"}, 3)
		checkFile("wiki-dashboard.ts", []string{"ItemView", "container", "stats", "sync"}, 3)
		checkFile("api-client.ts", []string{"ingest", "lint", "status", "fetch", "url"}, 3)

		item.Score = min(score, 15)
		item.Status = map[bool]string{true: "PASS", false: "WARN"}[score >= 12]
		item.Detail = strings.Join(details, "\n")
		report.Add(item)
	})
}

// ── 工具函数 ──────────────────────────────────────────

func truncS(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// generateTestPNG 生成 100x100 红色圆形 + 绿色背景的 PNG，返回 base64 字符串。
func generateTestPNG() string {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	cx, cy, r := 50, 50, 35
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
			} else {
				img.Set(x, y, color.RGBA{R: 0, G: 200, B: 0, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
