// trading-v2 工作流: 复刻 TradingAgents 架构 + 量化风控增强。
//
// 参考论文:
//   - TradingAgents (arXiv:2412.20138): 多 Agent 交易桌 + Bull/Bear 辩论 + 三方风控辩论
//   - FinDebate (arXiv:2509.17395): 安全辩论协议, 校准置信度
//   - Trading-R1 (arXiv:2509.11420): 结构化推理 + 证据链接
//
// 架构 (5 层):
//   Layer 1: 4 个并行分析师 (技术/情绪/基本面/新闻)
//   Layer 2: Bull/Bear 对抗辩论 → Research Manager 裁决
//   Layer 3: Trader 制定交易方案
//   Layer 4: Aggressive/Conservative/Neutral 三方风险辩论 → Portfolio Manager 裁决
//   Layer 5: Signal Extractor (结构化信号提取 + 量化风控)
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// tradingV2Workflow 返回 trading-v2 工作流定义。
// Mode = "trading_debate" 在 WorkflowExecutor.Execute 中有专门处理。
func tradingV2Workflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "trading-v2",
		Description: "金融交易决策系统 v2: 分析→辩论→交易→风控辩论→PM裁决 (TradingAgents 架构)",
		Mode:        "trading_debate",
		Stages: []StageDef{
			// Layer 1: 并行分析
			{Name: "tech-analysis", Role: "market-analyst", Parallel: true,
				Prompt: techAnalystPrompt},
			{Name: "sentiment-analysis", Role: "sentiment-analyst", Parallel: true,
				Prompt: sentimentAnalystPrompt},
			{Name: "fundamentals-analysis", Role: "fundamentals-analyst", Parallel: true,
				Prompt: fundamentalsAnalystPrompt},
			{Name: "news-analysis", Role: "news-analyst", Parallel: true,
				Prompt: newsAnalystPrompt},
			// Layer 2: 辩论 (由 debate engine 驱动, 不在 pipeline 中)
			{Name: "bull-researcher", Role: "bull-researcher"},
			{Name: "bear-researcher", Role: "bear-researcher"},
			{Name: "research-manager", Role: "research-manager",
				DependsOn: []string{"tech-analysis", "sentiment-analysis", "fundamentals-analysis", "news-analysis"}},
			// Layer 3: 交易
			{Name: "trader", Role: "trader",
				DependsOn: []string{"research-manager"}},
			// Layer 4: 风险辩论
			{Name: "aggressive-risk", Role: "aggressive-risk"},
			{Name: "conservative-risk", Role: "conservative-risk"},
			{Name: "neutral-risk", Role: "neutral-risk"},
			{Name: "portfolio-manager", Role: "portfolio-manager",
				DependsOn: []string{"trader"}},
		},
	}
}

// ══════════════════════════════════════════════════════
//  Trading V2 Executor — 辩论驱动的金融交易决策
// ══════════════════════════════════════════════════════

const (
	defaultDebateRounds = 2
	defaultRiskRounds   = 2
)

// executeTradingDebate 执行 trading-v2 的完整决策流程。
func (we *WorkflowExecutor) executeTradingDebate(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	// ── Phase 1: 并行分析 ──
	we.notify(we.chatID, "📊 **Phase 1/5**: 并行分析 — 技术/情绪/基本面/新闻")
	analysisDefs := []StageDef{
		wf.Stages[0], // tech
		wf.Stages[1], // sentiment
		wf.Stages[2], // fundamentals
		wf.Stages[3], // news
	}
	analysisResults := we.executeParallel(ctx, analysisDefs, objective, prevResults, team)
	for _, sr := range analysisResults {
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			prevResults[sr.Name] = sr.Output
		}
	}
	if ctx.Err() != nil {
		return allResults, ctx.Err()
	}

	// 汇总分析报告 (供辩论使用)
	analysisReport := we.buildAnalysisReport(prevResults)

	// ── Phase 2: Bull/Bear 对抗辩论 ──
	we.notify(we.chatID, "⚔️ **Phase 2/5**: Bull/Bear 研究辩论")
	debateResult, err := we.runResearchDebate(ctx, objective, analysisReport, team)
	if err != nil {
		return allResults, fmt.Errorf("研究辩论失败: %w", err)
	}
	allResults = append(allResults, debateResult...)
	for _, sr := range debateResult {
		if sr.Status == TaskCompleted {
			prevResults[sr.Name] = sr.Output
		}
	}

	// ── Phase 3: Trader 制定交易方案 ──
	we.notify(we.chatID, "💼 **Phase 3/5**: Trader 制定交易方案")
	traderStage := StageDef{
		Name: "trader", Role: "trader",
		Prompt: fmt.Sprintf(traderPrompt, objective, prevResults["research-manager"]),
	}
	traderSR := we.executeStage(ctx, traderStage, objective, prevResults, team)
	allResults = append(allResults, traderSR)
	if traderSR.Status == TaskCompleted {
		prevResults["trader"] = traderSR.Output
	}
	if ctx.Err() != nil {
		return allResults, ctx.Err()
	}

	// ── Phase 4: 三方风险辩论 ──
	we.notify(we.chatID, "🛡️ **Phase 4/5**: 三方风险辩论 (激进/保守/中性)")
	riskResult, err := we.runRiskDebate(ctx, objective, prevResults["trader"], team)
	if err != nil {
		return allResults, fmt.Errorf("风险辩论失败: %w", err)
	}
	allResults = append(allResults, riskResult...)
	for _, sr := range riskResult {
		if sr.Status == TaskCompleted {
			prevResults[sr.Name] = sr.Output
		}
	}

	// ── Phase 5: Signal 提取 + 风控 ──
	we.notify(we.chatID, "🎯 **Phase 5/5**: 信号提取 + 量化风控")
	signalStage := StageDef{
		Name: "signal-extractor", Role: "portfolio-manager",
		Prompt: fmt.Sprintf(signalExtractorPrompt, prevResults["portfolio-manager"]),
	}
	signalSR := we.executeStage(ctx, signalStage, objective, prevResults, team)
	allResults = append(allResults, signalSR)
	if signalSR.Status == TaskCompleted {
		prevResults["signal-extractor"] = signalSR.Output
		// 解析并应用量化风控
		signal := parseTradeSignal(signalSR.Output)
		signal = applyRiskGuardrails(signal)
		we.notify(we.chatID, formatSignalNotification(signal, objective))
	}

	return allResults, nil
}

// ── 研究辩论引擎 (Bull/Bear) ──

func (we *WorkflowExecutor) runResearchDebate(ctx context.Context, objective, analysisReport string, team *ProductionTeam) ([]StageResult, error) {
	var results []StageResult
	var debateHistory []string

	for round := 1; round <= defaultDebateRounds; round++ {
		if ctx.Err() != nil {
			break
		}

		// Bull turn
		we.notify(we.chatID, fmt.Sprintf("  🐂 辩论第 %d 轮: Bull Researcher", round))
		bullPrompt := fmt.Sprintf(bullResearcherPrompt, objective, analysisReport, strings.Join(debateHistory, "\n\n"))
		bullStage := StageDef{Name: fmt.Sprintf("bull-round%d", round), Role: "bull-researcher", Prompt: bullPrompt}
		bullSR := we.executeStage(ctx, bullStage, objective, nil, team)
		results = append(results, bullSR)
		if bullSR.Status == TaskCompleted {
			debateHistory = append(debateHistory, "**Bull Researcher**: "+truncateForDebate(bullSR.Output))
		}

		if ctx.Err() != nil {
			break
		}

		// Bear turn
		we.notify(we.chatID, fmt.Sprintf("  🐻 辩论第 %d 轮: Bear Researcher", round))
		bearPrompt := fmt.Sprintf(bearResearcherPrompt, objective, analysisReport, strings.Join(debateHistory, "\n\n"))
		bearStage := StageDef{Name: fmt.Sprintf("bear-round%d", round), Role: "bear-researcher", Prompt: bearPrompt}
		bearSR := we.executeStage(ctx, bearStage, objective, nil, team)
		results = append(results, bearSR)
		if bearSR.Status == TaskCompleted {
			debateHistory = append(debateHistory, "**Bear Researcher**: "+truncateForDebate(bearSR.Output))
		}
	}

	// Research Manager 裁决
	we.notify(we.chatID, "  👨‍💼 Research Manager 裁决中...")
	managerPrompt := fmt.Sprintf(researchManagerPrompt, objective, analysisReport, strings.Join(debateHistory, "\n\n"))
	managerStage := StageDef{Name: "research-manager", Role: "research-manager", Prompt: managerPrompt}
	managerSR := we.executeStage(ctx, managerStage, objective, nil, team)
	results = append(results, managerSR)

	return results, nil
}

// ── 三方风险辩论引擎 ──

func (we *WorkflowExecutor) runRiskDebate(ctx context.Context, objective, traderPlan string, team *ProductionTeam) ([]StageResult, error) {
	var results []StageResult
	var riskHistory []string

	for round := 1; round <= defaultRiskRounds; round++ {
		if ctx.Err() != nil {
			break
		}

		perspectives := []struct {
			name   string
			role   string
			prompt string
		}{
			{"aggressive-risk", "aggressive-risk", aggressiveRiskPrompt},
			{"conservative-risk", "conservative-risk", conservativeRiskPrompt},
			{"neutral-risk", "neutral-risk", neutralRiskPrompt},
		}

		for _, p := range perspectives {
			if ctx.Err() != nil {
				break
			}
			icon := map[string]string{"aggressive-risk": "🔥", "conservative-risk": "🛡️", "neutral-risk": "⚖️"}[p.name]
			we.notify(we.chatID, fmt.Sprintf("  %s 风险辩论第 %d 轮: %s", icon, round, p.name))

			prompt := fmt.Sprintf(p.prompt, objective, traderPlan, strings.Join(riskHistory, "\n\n"))
			stage := StageDef{Name: fmt.Sprintf("%s-round%d", p.name, round), Role: p.role, Prompt: prompt}
			sr := we.executeStage(ctx, stage, objective, nil, team)
			results = append(results, sr)
			if sr.Status == TaskCompleted {
				riskHistory = append(riskHistory, fmt.Sprintf("**%s**: %s", p.name, truncateForDebate(sr.Output)))
			}
		}
	}

	// Portfolio Manager 最终裁决
	we.notify(we.chatID, "  🏦 Portfolio Manager 最终裁决...")
	pmPrompt := fmt.Sprintf(portfolioManagerPrompt, objective, traderPlan, strings.Join(riskHistory, "\n\n"))
	pmStage := StageDef{Name: "portfolio-manager", Role: "portfolio-manager", Prompt: pmPrompt}
	pmSR := we.executeStage(ctx, pmStage, objective, nil, team)
	results = append(results, pmSR)

	return results, nil
}

// ── 辅助函数 ──

func (we *WorkflowExecutor) buildAnalysisReport(prevResults map[string]string) string {
	var sb strings.Builder
	sections := []struct {
		key   string
		title string
	}{
		{"tech-analysis", "技术分析"},
		{"sentiment-analysis", "情绪分析"},
		{"fundamentals-analysis", "基本面分析"},
		{"news-analysis", "新闻事件分析"},
	}
	for _, s := range sections {
		if r, ok := prevResults[s.key]; ok {
			content := r
			if len(content) > 3000 {
				content = content[:3000] + "\n...(已截断)"
			}
			sb.WriteString(fmt.Sprintf("## %s\n%s\n\n", s.title, content))
		}
	}
	return sb.String()
}

func truncateForDebate(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "\n...(已截断)"
	}
	return s
}

// ── 交易信号解析与量化风控 ──

// TradeSignal 结构化交易信号
type TradeSignal struct {
	Rating     string  `json:"rating"`     // BUY, OVERWEIGHT, HOLD, UNDERWEIGHT, SELL
	Confidence float64 `json:"confidence"` // 0.0 - 1.0
	Position   float64 `json:"position"`   // 建议仓位 %
	StopLoss   string  `json:"stop_loss"`  // 止损价位
	Target     string  `json:"target"`     // 目标价位
	Rationale  string  `json:"rationale"`  // 核心理由
	Timestamp  string  `json:"timestamp"`
}

func parseTradeSignal(raw string) TradeSignal {
	signal := TradeSignal{
		Rating:    "HOLD",
		Timestamp: time.Now().Format("2006-01-02 15:04"),
	}

	// 去除 markdown 代码块
	cleaned := raw
	cleaned = strings.ReplaceAll(cleaned, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")
	cleaned = strings.TrimSpace(cleaned)

	// 尝试 JSON 解析 (先用 map 做宽松解析)
	if idx := strings.Index(cleaned, "{"); idx >= 0 {
		end := strings.LastIndex(cleaned, "}")
		if end > idx {
			jsonStr := cleaned[idx : end+1]
			var m map[string]interface{}
			if json.Unmarshal([]byte(jsonStr), &m) == nil {
				if r, ok := m["rating"].(string); ok && r != "" {
					signal.Rating = strings.ToUpper(r)
					// 规范化: Overweight → OVERWEIGHT
					for _, valid := range []string{"BUY", "OVERWEIGHT", "HOLD", "UNDERWEIGHT", "SELL"} {
						if strings.EqualFold(signal.Rating, valid) {
							signal.Rating = valid
							break
						}
					}
				}
				// confidence: 可能是 0.82 或 82
				if c, ok := m["confidence"].(float64); ok {
					if c > 1 {
						signal.Confidence = c / 100.0
					} else {
						signal.Confidence = c
					}
				}
				// position: 可能是数字或字符串 "8-10%", "LONG", "Core Long (8-10%)"
				switch v := m["position"].(type) {
				case float64:
					signal.Position = v
				case string:
					var num float64
					if _, err := fmt.Sscanf(v, "%f", &num); err == nil {
						signal.Position = num
					} else {
						// "Core Long" 等文本 → 根据 rating 推断默认仓位
						switch signal.Rating {
						case "BUY":
							signal.Position = 15
						case "OVERWEIGHT":
							signal.Position = 10
						case "HOLD":
							signal.Position = 5
						case "UNDERWEIGHT":
							signal.Position = 3
						case "SELL":
							signal.Position = 0
						}
					}
				}
				if s, ok := m["stop_loss"].(string); ok {
					signal.StopLoss = s
				}
				if t, ok := m["target"].(string); ok {
					signal.Target = t
				}
				if r, ok := m["rationale"].(string); ok {
					signal.Rationale = r
				}
			}
		}
	}

	// 关键词回退
	if signal.Rating == "HOLD" || signal.Rating == "" {
		upper := strings.ToUpper(raw)
		for _, r := range []string{"BUY", "OVERWEIGHT", "UNDERWEIGHT", "SELL"} {
			if strings.Contains(upper, r) {
				signal.Rating = r
				break
			}
		}
	}

	return signal
}

func applyRiskGuardrails(s TradeSignal) TradeSignal {
	// 规则 1: 单一标的仓位不超过 30%
	if s.Position > 30 {
		s.Position = 30
		s.Rationale += " [风控: 仓位上限 30%]"
	}

	// 规则 2: 置信度 < 0.6 → 降级为 HOLD
	if s.Confidence > 0 && s.Confidence < 0.6 {
		s.Rating = "HOLD"
		s.Rationale += " [风控: 置信度不足, 降级 HOLD]"
	}

	return s
}

func formatSignalNotification(s TradeSignal, objective string) string {
	icon := map[string]string{
		"BUY": "🟢", "OVERWEIGHT": "🟢", "HOLD": "🟡",
		"UNDERWEIGHT": "🟠", "SELL": "🔴",
	}[s.Rating]
	if icon == "" {
		icon = "⚪"
	}

	msg := fmt.Sprintf(`
%s **最终交易信号**

📌 标的: %s
📊 评级: **%s**
🎯 置信度: %.0f%%
💰 建议仓位: %.0f%%`, icon, objective, s.Rating, s.Confidence*100, s.Position)

	if s.StopLoss != "" {
		msg += fmt.Sprintf("\n🛑 止损: %s", s.StopLoss)
	}
	if s.Target != "" {
		msg += fmt.Sprintf("\n🎯 目标: %s", s.Target)
	}
	if s.Rationale != "" {
		rationale := s.Rationale
		if len(rationale) > 200 {
			rationale = rationale[:200] + "..."
		}
		msg += fmt.Sprintf("\n💡 核心理由: %s", rationale)
	}
	return msg
}

// ══════════════════════════════════════════════════════
//  Prompts — 11 个 Agent 的提示词
// ══════════════════════════════════════════════════════

const techAnalystPrompt = `你是资深量化交易分析师 (CTA 策略专家)。

分析标的: {objective}

**必须使用 WebSearch 工具** 搜索标的最新价格和交易数据。

输出:
## 技术面分析
1. **价格趋势**: 当前价位、近期高低点、趋势方向 (标注数据来源)
2. **关键指标**: MA5/MA20/MA60、MACD、RSI、布林带
3. **成交量**: 量价关系、主力资金动向
4. **支撑/阻力**: 关键价位
5. **K线形态**: 近期形态信号

## 技术面评分: X/10 (含理由)`

const sentimentAnalystPrompt = `你是金融市场情绪分析专家。

分析标的: {objective}

**必须使用 WebSearch 工具** 搜索最新的市场新闻、分析师评级、资金流向。

输出:
## 情绪分析
1. **新闻热度**: 近7天关键新闻 (标注来源)
2. **分析师共识**: 买入/持有/卖出比例
3. **资金流向**: 主力净流入/流出
4. **市场氛围**: 贪婪/恐惧指数
5. **散户 vs 机构**: 持仓变化

## 情绪评分: X/10 (含理由)`

const fundamentalsAnalystPrompt = `你是高级财务分析师 (CFA)。

分析标的: {objective}

输出:
## 基本面分析
1. **盈利能力**: 营收增速、净利率、ROE
2. **估值水平**: P/E、P/B、P/S、PEG 及与行业对比
3. **成长性**: 营收/利润趋势、研发投入、护城河
4. **财务健康**: 负债率、现金流、商誉
5. **同业对比**: 2-3 个竞品核心指标对比表

## 基本面评分: X/10 (含理由)`

const newsAnalystPrompt = `你是金融新闻与事件分析专家。

分析标的: {objective}

**必须使用 WebSearch 工具** 搜索最新事件和行业动态。

输出:
## 事件分析
1. **近期重大事件**: 影响股价的关键事件 (标注来源)
2. **行业动态**: 行业趋势、竞争格局变化
3. **宏观因素**: 利率、汇率、地缘政治
4. **催化剂**: 未来 1-3 月可预见的重要事件
5. **监管风险**: 合规、政策变化

## 事件影响表
| 事件 | 方向 | 程度 | 概率 |
|------|------|------|------|

## 事件面评分: X/10`

var bullResearcherPrompt = `你是 **多头研究员 (Bull Researcher)**。你的任务是为标的构建最强的买入论据。

标的: %s

## 分析师报告
%s

## 辩论历史
%s

**你的任务**:
1. 构建 3-5 条核心买入论据, 每条必须引用分析报告中的具体数据
2. 如果有空头观点, 逐条反驳 (引用数据)
3. 论证为什么当前是买入时机
4. 评估上涨空间和催化剂

格式: 先陈述核心论据, 再反驳空头, 最后给出目标价。`

var bearResearcherPrompt = `你是 **空头研究员 (Bear Researcher)**。你的任务是找出所有卖出理由和风险。

标的: %s

## 分析师报告
%s

## 辩论历史
%s

**你的任务**:
1. 构建 3-5 条核心风险/卖出论据, 每条必须引用具体数据
2. 如果有多头观点, 逐条挑战 (指出盲区和过度乐观)
3. 论证当前估值/价格的风险
4. 评估下行空间和黑天鹅场景

格式: 先陈述核心风险, 再挑战多头, 最后给出风险价位。`

var researchManagerPrompt = `你是 **研究主管 (Research Manager)**。裁决 Bull/Bear 辩论, 输出投资计划。

标的: %s

## 分析师报告
%s

## Bull/Bear 辩论记录
%s

**你的任务**:
1. 评估双方论据的质量和数据支撑度
2. 判断哪些论据更有说服力
3. 形成明确立场 (倾向买入/持有/卖出)
4. 输出投资计划 (含理由、催化剂、风险)

输出格式:
## 裁决: 【倾向买入/持有/卖出】
## 核心理由 (3 条)
## 投资计划
- 进场条件
- 目标价位
- 关键风险`

var traderPrompt = `你是 **交易员 (Trader)**。基于研究报告制定具体的交易方案。

标的: %s

## Research Manager 投资计划
%s

**输出**:
## 交易方案
1. **操作**: BUY / HOLD / SELL
2. **仓位**: 占总仓位的 X%%
3. **进场价位/条件**: 具体数值
4. **止损价位**: 具体数值 (含逻辑)
5. **目标价位**: 短期(1周)/中期(1月)
6. **盈亏比**: X:1
7. **执行策略**: 一次性建仓 / 分批建仓 / 条件单`

var aggressiveRiskPrompt = `你是 **激进风险分析师 (Aggressive Risk Analyst)**。你倾向抓住高收益机会, 挑战过度保守的观点。

标的: %s

## 交易方案
%s

## 风险讨论历史
%s

**你的视角**: 
- 当前市场是否存在被低估的机会?
- 止损是否设置得太紧?
- 仓位是否可以更大? (论证)
- 如果其他人过于保守, 指出他们错失了什么

限制 200 字以内, 聚焦 1-2 个核心观点。`

var conservativeRiskPrompt = `你是 **保守风险分析师 (Conservative Risk Analyst)**。你优先保护资本, 挑战过度乐观的观点。

标的: %s

## 交易方案
%s

## 风险讨论历史
%s

**你的视角**: 
- 最大下行风险是什么? 概率多大?
- 仓位是否过大? 止损是否足够?
- 有哪些尾部风险被忽视?
- 如果其他人过于激进, 指出他们忽视的风险

限制 200 字以内, 聚焦 1-2 个核心观点。`

var neutralRiskPrompt = `你是 **中性风险分析师 (Neutral Risk Analyst)**。你平衡双方观点, 挑战偏见。

标的: %s

## 交易方案
%s

## 风险讨论历史
%s

**你的视角**: 
- 激进方和保守方的观点各有什么盲区?
- 综合考虑, 最合理的仓位和止损是什么?
- 当前最被忽视的因素是什么?
- 提出折中建议

限制 200 字以内, 聚焦最关键的 1-2 个平衡点。`

var portfolioManagerPrompt = `你是 **投资组合经理 (Portfolio Manager)**。做出最终投资决策。

标的: %s

## 交易方案
%s

## 风险辩论
%s

⚠️ 重要: 你的回复必须且只能是一个 JSON 对象 (不要用 markdown 代码块包裹, 不要有任何其他文字)。

rating 必须是五个之一: BUY, OVERWEIGHT, HOLD, UNDERWEIGHT, SELL
confidence 必须是 0 到 1 之间的小数 (如 0.75)
position 必须是数字 (建议仓位占比, 如 10 表示 10%%)

示例:
{"rating":"OVERWEIGHT","confidence":0.75,"position":10,"stop_loss":"-12%%","target":"+20%%","rationale":"AI算力需求结构性增长"}`

var signalExtractorPrompt = `从以下内容中提取交易信号。只输出 JSON, 不要任何其他文字。

%s

{"rating":"五选一:BUY/OVERWEIGHT/HOLD/UNDERWEIGHT/SELL","confidence":0到1的小数,"position":仓位百分比数字,"stop_loss":"止损","target":"目标","rationale":"一句话理由"}`
