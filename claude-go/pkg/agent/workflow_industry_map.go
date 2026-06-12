// 放置到 claude-go: pkg/agent/workflow_industry_map.go
// 注册: workflowRegistry["industry-map"] = industryMapWorkflow
//
// 用途: 给定一个产业(如"芯片"), 输出结构化产业链图(环节/企业/股票代码/份额),
// 用于初始化和季度更新 market-radar 的 chain/seed/*.yaml。
package agent

func industryMapWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "industry-map",
		Description: "产业链建模: 拆解环节→代表企业→股票代码→市场份额, 输出可落库的结构化产业链图",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "chain-architect", Role: "architect",
				Prompt: `你是产业链分析专家。请拆解给定产业的完整产业链。

目标产业: {objective}

请按上游/中游/下游分层, 列出每个关键**环节**(segment):
1. 环节名称 + 所属层级(上游/中游/下游)
2. 该环节在产业链中的作用
3. 环节的技术壁垒/景气驱动因素

## 产业链环节清单
| 层级 | 环节 | 作用 | 壁垒 |`,
			},
			{
				Name: "company-mapper", Role: "researcher",
				DependsOn: []string{"chain-architect"},
				Prompt: `基于环节拆解, 为每个环节找出**代表企业 + 股票代码**(必须 WebSearch 取真实代码)。

{prev_result}

要求:
1. 每环节列 2-5 家代表企业, A 股优先, 关键全球龙头(美/港/台)也列
2. **必须给准确股票代码**(A股6位, 美股 ticker, 港股5位), 标注市场
3. 龙头/跟随/细分定位

## 企业-代码映射
| 环节 | 企业 | 市场 | 代码 | 定位 |`,
			},
			{
				Name: "share-researcher", Role: "fundamentals-analyst",
				DependsOn: []string{"company-mapper"},
				Prompt: `为上述企业补充**市场份额**与龙头判断(WebSearch 取研报数据, 标注来源与日期)。

{prev_result}

## 市场份额表
| 环节 | 企业 | 市场份额 | 数据来源/日期 | 是否龙头 |

注意: 份额数据须标注来源, 不确定的标"待确认", 不要编造。`,
			},
			{
				Name: "graph-builder", Role: "architect",
				DependsOn: []string{"share-researcher"},
				Prompt: `把以上汇总成**机器可读的结构化产业链图**, 直接输出 YAML(与 market-radar 种子格式一致)。

{prev_result}

严格按此 YAML 结构输出(只输出 YAML, 不要多余文字):

chain: <产业英文短id, 如 chip>
name: <产业中文名>
segments:
  - id: <环节英文id>
    name: <环节中文名>
    layer: <上游/中游/下游>
    companies:
      - { name: <名>, market: <A/US/HK>, ticker: "<代码>", role: <定位>, share: <份额%或留空> }
edges:
  - { from: <环节id>, to: <环节id>, type: <关系> }`,
			},
		},
	}
}
