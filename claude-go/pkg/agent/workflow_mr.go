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
				Name: "worker", Role: "mr-emitter",
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

func mrChainWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "mr-chain",
		Description: "market-radar 产业链建模: 单 agent(kimi) 直出完整 JSON 产业链图",
		Mode:        "pipeline",
		QualityGate: "none",
		Stages: []StageDef{
			{
				Name: "chain-builder", Role: "mr-emitter",
				Prompt: `你是产业链分析师。为给定产业输出一份【完整的产业链图】，只输出一个合法 JSON 对象：不要任何解释、不要 markdown、不要代码围栏。第一个字符是 { 。

产业: {objective}

严格要求:
- 结构: {"chain":"英文短id(小写连字符)","name":"中文名","groups":[{"id","name","layer":"上游|中游|下游"}],"segments":[{"id","name","group":"所属group的id","companies":[...]}]}
- 每个 segment 的 companies 至少 3-8 家【真实上市公司】。
- ★硬性要求: 整条产业链【必须同时包含中国大陆A股、香港、美股】三地公司。中国相关产业往往有A股/港股龙头,
  务必主动纳入(例: 商业航天有航天科技/航天电子/中国卫星/中航光电等A股; 创新药有恒瑞/百济神州/药明康德A+H等)。
  只给美股是不合格的输出。
- 公司字段: {"name","market":"A|HK|US","ticker":"纯代码不带交易所后缀","secid":"港股/美股给,如 116.00700 / 105.NVDA；A股可省略","role":"龙头|跟随等"}。
- A股 ticker 如 "600519"；港股 ticker 如 "00700" secid "116.00700"；美股 ticker 如 "NVDA" secid "105.NVDA"(纳斯达克105/纽交所106)。
- 只列你确信真实存在且已上市的公司；未上市公司不要列。
- 覆盖上中下游 6-10 个 segment。只输出 JSON。`,
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
				Name: "strategist", Role: "mr-emitter",
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
