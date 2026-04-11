// Intent — 自然语言意图识别层。
//
// 架构: 关键词快速检测 + LLM 参数提取 (双层混合)
//
// 第一层: 基于中文关键词的快速匹配 (< 1ms, 零 token 开销)
//   - 不匹配 → 直接返回 nil, 走正常对话流程
//   - 匹配   → 进入第二层
//
// 第二层: LLM 结构化参数提取 (可选, ~500ms)
//   - 提取任务目标、工作流类型、团队名称
//   - 返回结构化 TeamIntent, confidence 提升到 0.9
//
// 设计原则 (AG2 Framework, 2026):
//   - 分离"何时交接"和"交接到哪" — 关键词判断 when, LLM 判断 where
//   - 确定性优先 — 不依赖脆弱的 prompt-based routing
//   - 意图不明时不拦截 — 交给正常 LLM 对话处理
//
// 用户体验:
//   "帮我组3个agent调研kubernetes最佳实践" → 自动创建 research 团队并执行
//   "团队进展如何" → 查看所有团队状态
//   "停止调研任务" → 停止指定团队
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LLMClient 为意图识别提供 LLM 访问。
// 与 api.Client 通过 duck typing 兼容。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// TeamIntent 识别出的团队协作意图。
type TeamIntent struct {
	Action     string  `json:"action"`               // create_and_run, check_status, stop, list, none
	TeamName   string  `json:"teamName,omitempty"`    // 团队名称
	Workflow   string  `json:"workflow,omitempty"`     // development, research, debate
	Objective  string  `json:"objective,omitempty"`    // 任务目标
	Confidence float64 `json:"confidence"`             // 0.0-1.0
}

// IntentRecognizer 基于关键词 + LLM 的混合意图识别器。
type IntentRecognizer struct {
	llm LLMClient
}

// NewIntentRecognizer 创建意图识别器。llm 为 nil 时仅使用关键词检测。
func NewIntentRecognizer(llm LLMClient) *IntentRecognizer {
	return &IntentRecognizer{llm: llm}
}

// --- 关键词库 ---

var createKW = []string{
	"组建团队", "创建团队", "启动团队", "组织团队",
	"组建一个", "创建一个团队", "组一个",
	"agent协作", "多agent", "多个agent",
	"帮我调研", "帮我研究", "帮我开发", "帮我实现",
	"帮我辩论", "帮我分析", "帮我讨论", "帮我构建",
	"组织调研", "安排开发", "发起辩论", "帮我对比",
	"帮我盯盘", "分析下", "帮我写文章", "帮我写博客",
	"帮我分析股票", "帮我看看", "帮我写公众号",
}

var statusKW = []string{
	"进展如何", "进度如何", "团队状态", "执行到哪",
	"做得怎样", "完成了吗", "结果如何", "怎么样了",
	"团队进度",
}

var stopKW = []string{
	"停止团队", "终止团队", "取消团队", "停止执行",
	"别做了", "不用做了", "取消任务", "停止调研",
	"停止开发", "停止辩论",
	"停止当前", "终止当前", "取消当前",
}

var listKW = []string{
	"有哪些团队", "团队列表", "所有团队", "列出团队",
}

var deleteKW = []string{
	"删除团队", "移除团队", "清除团队", "解散团队",
	"删除研究团队", "删除开发团队",
}

var wfDetect = map[string][]string{
	"development": {"开发", "实现", "编码", "构建", "写代码", "搭建", "编程", "重构"},
	"research":    {"调研", "研究", "调查", "分析", "探索", "对比分析", "了解", "评估"},
	"debate":      {"辩论", "讨论", "对比", "权衡", "利弊", "优劣", "pk", "vs", "还是"},
	"swarm":       {"蜂群", "swarm", "并行分析", "全面调研", "深度分析", "多角度", "自动拆解"},
	"finance":     {"股票", "股价", "盯盘", "交易", "买入", "卖出", "持仓", "行情", "大盘", "a股", "美股", "港股", "基金", "投资建议", "财报", "估值"},
	"techblog":    {"写文章", "写博客", "公众号", "技术文章", "源码分析", "写作", "排版", "发文", "文章创作"},
	"creative":    {"画图", "生图", "做图", "设计图", "海报", "logo", "封面", "插画", "视觉设计", "图片创作", "视频创作", "做视频", "生成视频", "做个图", "画一个", "帮我画", "创意设计", "视觉创作", "分镜", "动画", "宣传视频", "宣传图", "做个"},
}

// CronIntent cron 定时任务意图。
type CronIntent struct {
	Action   string `json:"action"`   // add_cron, list_cron, remove_cron
	Schedule string `json:"schedule"` // cron 表达式
	SchedDesc string `json:"schedDesc"` // 调度描述
	JobType  string `json:"jobType"`  // workflow, query
	Workflow string `json:"workflow"` // 工作流类型
	Payload  string `json:"payload"`  // 执行内容
}

var cronKW = []string{
	"定时", "每天", "每周", "每小时", "每分钟", "定期", "周期性",
	"工作日", "每日", "每月", "早上", "下午", "晚上",
	"自动执行", "定时执行", "设个闹钟", "定时提醒",
}

// Recognize 从用户文本中识别团队协作意图。
// 返回 nil 表示无团队相关意图, 应走正常对话流程。
//
// 优化: 当有 MCP 工具加载时, 提高 create_and_run 的置信度阈值,
// 避免与 MCP 工具调用冲突。通过多关键词匹配强度来区分。
func (ir *IntentRecognizer) Recognize(ctx context.Context, text string) *TeamIntent {
	lower := strings.ToLower(text)

	intent := ir.keywordDetect(lower, text)
	if intent == nil {
		return nil
	}

	// 对创建类意图, 用 LLM 精确提取参数
	if ir.llm != nil && intent.Action == "create_and_run" {
		ir.extractWithLLM(ctx, text, intent)
	}

	if intent.TeamName == "" && intent.Action == "create_and_run" {
		intent.TeamName = fmt.Sprintf("team-%d", time.Now().Unix()%10000)
	}

	return intent
}

// RecognizeWithMCPAwareness 在有 MCP 工具的场景下做意图识别。
// 当 LLM 可能将任务路由到 MCP 工具时, 提高 team 意图检测的灵敏度。
// hasMCP 表示是否有活跃的 MCP 连接。
func (ir *IntentRecognizer) RecognizeWithMCPAwareness(ctx context.Context, text string, hasMCP bool) *TeamIntent {
	intent := ir.Recognize(ctx, text)
	if intent == nil {
		return nil
	}

	// 当有 MCP 时, 对 create_and_run 要求更高的匹配强度
	if hasMCP && intent.Action == "create_and_run" {
		lower := strings.ToLower(text)
		matchStrength := ir.calcMatchStrength(lower)
		// 多关键词强匹配才走 team, 否则让 LLM 自行决定
		if matchStrength < 2 && intent.Confidence < 0.85 {
			return nil
		}
	}

	return intent
}

// calcMatchStrength 计算文本匹配关键词的总强度 (匹配的类别数)
func (ir *IntentRecognizer) calcMatchStrength(lower string) int {
	strength := 0
	// 检查创建关键词
	for _, kw := range createKW {
		if strings.Contains(lower, kw) {
			strength++
			break
		}
	}
	// 检查工作流关键词
	for _, kws := range wfDetect {
		for _, kw := range kws {
			if strings.Contains(lower, kw) {
				strength++
				break
			}
		}
	}
	// 检查明确的团队相关词
	teamExplicit := []string{"团队", "agent", "多agent", "协作", "并行", "蜂群"}
	for _, kw := range teamExplicit {
		if strings.Contains(lower, kw) {
			strength += 2
			break
		}
	}
	return strength
}

// RecognizeCron 识别 cron 定时任务意图。
func (ir *IntentRecognizer) RecognizeCron(ctx context.Context, text string) *CronIntent {
	lower := strings.ToLower(text)

	hasCron := false
	for _, kw := range cronKW {
		if strings.Contains(lower, kw) {
			hasCron = true
			break
		}
	}
	if !hasCron {
		return nil
	}

	// 还需要有明确的任务内容
	hasTask := false
	for _, kw := range createKW {
		if strings.Contains(lower, kw) {
			hasTask = true
			break
		}
	}
	if !hasTask {
		return nil
	}

	schedule, schedDesc := ParseNaturalSchedule(text)

	// 检测工作流类型
	workflow := "research"
	maxScore := 0
	for wf, kws := range wfDetect {
		score := 0
		for _, kw := range kws {
			if strings.Contains(lower, kw) {
				score++
			}
		}
		if score > maxScore {
			maxScore = score
			workflow = wf
		}
	}

	return &CronIntent{
		Action:    "add_cron",
		Schedule:  schedule,
		SchedDesc: schedDesc,
		JobType:   "workflow",
		Workflow:  workflow,
		Payload:   text,
	}
}

func (ir *IntentRecognizer) keywordDetect(lower, original string) *TeamIntent {
	for _, kw := range stopKW {
		if strings.Contains(lower, kw) {
			return &TeamIntent{Action: "stop", Confidence: 0.8}
		}
	}

	for _, kw := range statusKW {
		if strings.Contains(lower, kw) {
			return &TeamIntent{Action: "check_status", Confidence: 0.8}
		}
	}

	for _, kw := range listKW {
		if strings.Contains(lower, kw) {
			return &TeamIntent{Action: "list", Confidence: 0.8}
		}
	}

	for _, kw := range deleteKW {
		if strings.Contains(lower, kw) {
			return &TeamIntent{Action: "delete", Confidence: 0.8}
		}
	}

	hasCreate := false
	for _, kw := range createKW {
		if strings.Contains(lower, kw) {
			hasCreate = true
			break
		}
	}

	workflow := "research"
	maxScore := 0
	for wf, kws := range wfDetect {
		score := 0
		for _, kw := range kws {
			if strings.Contains(lower, kw) {
				score++
			}
		}
		// 专业团队关键词优先: 当 finance/techblog/creative 有匹配时
		// 给予额外权重，避免被 research 的泛化关键词（如 "分析"）覆盖
		if score > 0 {
			switch wf {
			case "finance", "techblog", "creative":
				score += 1
			}
		}
		if score > maxScore {
			maxScore = score
			workflow = wf
		}
	}

	// 即使没有 createKW，当 wfDetect 强匹配专业团队关键词时也触发
	if !hasCreate {
		specialWorkflows := map[string]bool{"finance": true, "techblog": true, "creative": true}
		if maxScore >= 1 && specialWorkflows[workflow] {
			hasCreate = true
		}
	}
	if !hasCreate {
		return nil
	}

	conf := 0.7
	if maxScore >= 2 {
		conf = 0.8
	}
	return &TeamIntent{
		Action:     "create_and_run",
		Workflow:   workflow,
		Objective:  original,
		Confidence: conf,
	}
}

const extractSysPrompt = `你是命令解析器。给定用户的中文消息，提取多Agent协作任务的参数。

输出 JSON（仅JSON，不要解释）:
{"workflow":"development|research|debate|swarm|finance|techblog|creative","objective":"简洁的任务目标","teamName":"kebab-case英文短名"}

判断规则:
- development: 编写代码/实现功能/构建系统
- research: 调研/分析/探索/对比方案
- debate: 辩论/讨论利弊/权衡选择
- swarm: 复杂任务需要多角度并行分析/全面调研/自动拆解子任务
- finance: 股票分析/盯盘/交易建议/财报分析/投资评估
- techblog: 写技术文章/公众号文章/博客/源码分析文章
- creative: 画图/做图/海报/Logo/封面/视频创作/视觉设计/动画`

func (ir *IntentRecognizer) extractWithLLM(ctx context.Context, text string, intent *TeamIntent) {
	resp, err := ir.llm.SimpleComplete(ctx, extractSysPrompt, text)
	if err != nil {
		return
	}

	resp = strings.TrimSpace(resp)
	// 处理 LLM 可能包裹在 markdown 代码块中的情况
	if idx := strings.Index(resp, "```"); idx >= 0 {
		resp = resp[idx+3:]
		resp = strings.TrimPrefix(resp, "json")
		if end := strings.Index(resp, "```"); end >= 0 {
			resp = resp[:end]
		}
	}
	resp = strings.TrimSpace(resp)

	var parsed struct {
		Workflow  string `json:"workflow"`
		Objective string `json:"objective"`
		TeamName  string `json:"teamName"`
	}
	if json.Unmarshal([]byte(resp), &parsed) == nil {
		if parsed.Workflow != "" {
			intent.Workflow = parsed.Workflow
		}
		if parsed.Objective != "" {
			intent.Objective = parsed.Objective
		}
		if parsed.TeamName != "" {
			intent.TeamName = parsed.TeamName
		}
		intent.Confidence = 0.9
	}
}
