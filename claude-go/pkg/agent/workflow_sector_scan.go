// 放置到 claude-go: pkg/agent/workflow_sector_scan.go
// 并在 pkg/agent/workflow.go 的 workflowRegistry 注册:
//     "sector-scan": sectorScanWorkflow,
//
// market-radar 经 :18080 调用:
//     POST /api/actions/team/create/{name}  body {"workflow":"sector-scan","objective":"<环节+企业清单+快照数据>"}
//     POST /api/actions/team/run/{name}
//
// 用途: 对某产业链环节的一组企业做"横向"对比(谁低估/谁过热), 与 market-radar 的
// 规则打分互为印证。区别于 trading-v2 的单标的纵深分析。
package agent

func sectorScanWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "sector-scan",
		Description: "产业链环节横向扫描: 对一组同环节企业做估值/基本面/催化剂对比, 点名低估与过热",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "cross-valuation", Role: "fundamentals-analyst", Parallel: false,
				Prompt: `你是产业链研究专家。下面给定某产业链环节的一组企业及其截面数据。

环节与企业清单:
{objective}

请做**横向估值对比**(不是单只深挖):
1. 列出每家的 PE/PB/PS 与同环节中位数对比, 谁贵谁便宜
2. 区分"便宜得有理由"(基本面差/周期顶) 与"被错杀"(基本面稳但估值低)
3. 指出该环节当前整体估值处于历史什么水平(贵/中性/便宜)

## 估值对比表 (按估值从低到高排序)
| 企业 | PE | PB | 相对环节 | 便宜原因/错杀? |`,
			},
			{
				Name: "fundamental-rank", Role: "financial-analyst",
				DependsOn: []string{"cross-valuation"},
				Prompt: `基于上一步估值对比, 对这组企业做**基本面质量排序**。

{prev_result}

请输出:
1. 成长性排序(营收/利润增速)
2. 盈利质量排序(ROE/毛利率/现金流)
3. 竞争地位(龙头/份额/护城河)

## 基本面综合排名
| 排名 | 企业 | 成长 | 质量 | 地位 | 一句话 |`,
			},
			{
				Name: "catalyst-scan", Role: "news-tracker",
				DependsOn: []string{"fundamental-rank"},
				Prompt: `对这组企业逐一扫描近期**催化剂与风险事件**(必须 WebSearch 取真实新闻)。

{prev_result}

## 催化剂/风险表
| 企业 | 近期催化剂 | 风险点 | 净影响(利多/利空) |`,
			},
			{
				Name: "sector-judge", Role: "trade-advisor",
				DependsOn: []string{"catalyst-scan"},
				Prompt: `你是首席策略师。综合估值对比 + 基本面排名 + 催化剂扫描, 对该环节下结论。

{prev_result}

请输出:
## 环节景气位置
当前该环节处于景气周期的什么位置(底部/复苏/过热)。

## 🟢 价值低洼地点名 (1-3 家)
每家给: 为什么低估 + 关键催化剂 + 需验证的风险

## 🔴 过热风险点名 (1-3 家)
每家给: 为什么过热(估值/资金/情绪) + 风险兑现信号

## ⚠️ 风险提示
本分析仅供研究参考, 不构成投资建议。`,
			},
		},
	}
}
