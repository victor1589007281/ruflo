package agent

// market-radar 量化研究闭环的两个单 agent 工作流。
//
// 模型绑定在 config.json 的 global.plans（按工作流名解析）:
//   mr-worker         -> ollama:gemma4:26b-a4b-it-qat   (本地"手脚")
//   quant-strategist  -> kimi:kimi-k2                    (高阶"大脑")
// 二者都要求【只输出一个合法 JSON 对象】，market-radar 侧会剥掉 ```json 围栏后解析。

func mrWorkerWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "mr-worker",
		Description: "market-radar 手脚: 本地 gemma 做解析/仲裁/情绪打分，只输出结构化 JSON",
		Mode:        "pipeline",
		QualityGate: "none",
		Stages: []StageDef{
			{
				Name: "worker", Role: "researcher",
				Prompt: `你是一个数据处理器。你的**唯一输出是一个 JSON 对象**。禁止任何前后说明、禁止分析过程、禁止 markdown 代码围栏、禁止 "以下是"/"我将" 之类的话。第一个字符必须是 { ，最后一个字符必须是 } 。

任务:
{objective}

要求:
- 只输出 JSON；键名用英文，值可为中文；无法确定填 null。
- 若是情绪任务，严格输出: {"sentiment":"利好|中性|利空","confidence":0.0,"key_events":[]}
- 再次强调：除了这个 JSON 对象，不要输出任何其它字符。`,
			},
		},
	}
}

func quantStrategistWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "quant-strategist",
		Description: "market-radar 大脑: 高阶模型出量化策略 spec / 复盘归因，输出结构化 JSON",
		Mode:        "pipeline",
		QualityGate: "none",
		Stages: []StageDef{
			{
				Name: "strategist", Role: "trade-advisor",
				Prompt: `你是量化策略研究员。根据输入完成"提出策略"或"复盘归因"，并且【只输出一个合法的 JSON 对象】：不要解释、不要 markdown 围栏。

输入:
{objective}

若是提出策略(propose)，输出形如:
{"name":"chip-value-vX","hypothesis":"...","universe":{"chain":"chip"},"signals":[{"factor":"valuation_pct","op":"<","value":0.4},{"factor":"fundamental","op":">=","value":6.0}],"rank_by":"value_score","top_n":5,"sizing":"equal_weight","max_position_pct":0.25,"rebalance":"weekly","horizon":"swing","risk":{"stop_loss_pct":0.12,"sector_cap_pct":0.6}}
因子只能取: valuation_pct, crowding_pct, heat_pct, rs_pct, fundamental, value_score, overheat_score, price, change_pct, mktcap, turnover, vol_ratio, main_inflow, pe_ttm, pb, ps。
op 只能取: < <= > >= == !=；sizing 取 equal_weight|value_weight；rebalance 取 daily|weekly|monthly 或 Nd；horizon 取 swing|trend|long_term。
★重要约束(避免选空)：signals **最多 2-4 条**，阈值要宽松(如 valuation_pct<0.4 而非 <0.2)；
过多 signal 的合取会导致 0 只标的入选、策略空转，属于失败设计。risk 只放 stop_loss_pct/sector_cap_pct 两个键。

若是复盘(critique)，输出形如:
{"verdict":"keep|kill","attribution":"为什么亏/因子失效在哪","improved_spec":null}
★复盘判据：若报告 turnover=0 或 samples<2 或 total_return 缺失(说明没交易/无暴露/空转)，verdict 必须为 kill；
若 beats_bench=false(跑不赢等权基准)，倾向 kill 或给出更宽松的 improved_spec。`,
			},
		},
	}
}
