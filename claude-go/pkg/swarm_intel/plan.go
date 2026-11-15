package swarm_intel

// plan.go —— 一键模式的**运行参数建议** (轮次预算 + 传播方式)。
//
// 为什么需要它: 场景识别的结果是"按什么模式模拟", 但一次模拟的质量还取决于
// 两个没人选的参数 —— 轮次预算帽与影响图拓扑。此前一键模式固定 --rounds 3 +
// 完全图, 结果 100% 的运行都是 stop_reason=budget (跑到帽才停), 说明"3 轮"从来
// 不是这个场景需要的轮数, 只是默认值; 而"所有人都看见所有人"只对少数场景成立
// (市场/博弈/政策), 舆论扩散、邻里依从、组织层级都不是完全图。
//
// 两条设计纪律:
//  1. 轮次给的是**预算帽**而不是目标轮数: 内核仍有自适应终止 (收敛/熵平台期),
//     帽子的作用是"别在收敛前就断", 所以按场景复杂度给 5~10 轮, 真正的停止
//     时刻由每轮实测的立场变化决定。MinRounds 同时抬到 3, 免得两轮偶然小波动
//     就被判成收敛。
//  2. 拓扑按"信息怎么流动"选, 不按模式名字的直觉: 有空间/邻里性质的 (健康依从、
//     社区环保) 用环格; 熟人圈 + 少量长程桥的 (舆论、消费口碑、技术采纳、危机
//     扩散、组织部门) 用小世界; 所有人对所有人可见的 (市场、博弈、政策、团队
//     开会、课堂) 用完全图。
//
// 这里是**建议**: 用户在手动页可以覆盖任何一项; 一键模式的进度里会打印选了
// 什么、为什么 (Why), 免得参数变成又一个"看不懂但默认如此"。

import (
	"fmt"
	"strings"
)

// RunPlan 一键模式建议的运行参数。字段与 CLI 参数一一对应 (零值=不传)。
type RunPlan struct {
	Rounds       int     `json:"rounds,omitempty"`        // --rounds 轮次预算帽
	MinRounds    int     `json:"min_rounds,omitempty"`    // --min-rounds 判收敛前至少跑几轮
	Agents       int     `json:"agents,omitempty"`        // --agents
	Topology     string  `json:"topology,omitempty"`      // --topology full|ring|smallworld
	Rewire       bool    `json:"rewire,omitempty"`        // --rewire 每轮同质性重连
	MemoryRounds int     `json:"memory_rounds,omitempty"` // --memory-rounds
	Epsilon      float64 `json:"epsilon,omitempty"`       // --epsilon 收敛阈值
	Why          string  `json:"why"`                     // 一句话: 为什么这么配 (展示给用户)
}

// modePlan 是每个模式的传播方式档案。
//
// agents 按"这个场景里通常有多少类角色"给: 博弈 4 (参与方少), 团队 6 (小组会),
// 组织 8 (层级 + 部门), 舆论/消费/健康/环保 8 (人群异质性更重要)。
type modePlan struct {
	topology string
	rewire   bool
	memory   int
	agents   int
	rounds   int
	why      string
}

var modePlans = map[string]modePlan{
	// 熟人圈传播 + 少量长程桥: 舆论/传言/口碑/技术采纳/危机扩散
	"social":   {topology: "smallworld", rewire: true, memory: 3, agents: 8, rounds: 8, why: "舆论在熟人圈扩散、还会因同质性重连形成回音室"},
	"rumor":    {topology: "smallworld", rewire: true, memory: 3, agents: 8, rounds: 8, why: "传言靠熟人转发 + 少量跨圈桥传播, 信源信任决定反转"},
	"consumer": {topology: "smallworld", rewire: true, memory: 2, agents: 7, rounds: 7, why: "种草拔草沿社交圈扩散, 同温层会放大口碑"},
	"tech":     {topology: "smallworld", rewire: false, memory: 2, agents: 7, rounds: 7, why: "技术采纳沿社群网络扩散, 少数枢纽开发者带动生态"},
	"crisis":   {topology: "smallworld", rewire: false, memory: 2, agents: 6, rounds: 7, why: "突发事件先在小圈扩散再经长程桥外溢, 结构不应随立场重连"},
	"org":      {topology: "smallworld", rewire: false, memory: 2, agents: 8, rounds: 7, why: "部门内信息密集、跨部门只有少量通道, 汇报线不该被立场改写"},
	"creative": {topology: "smallworld", rewire: false, memory: 2, agents: 6, rounds: 6, why: "创意在圈内发酵 + 偶尔跨圈碰撞, 避免完全图把分歧抹平"},

	// 邻里/空间性质的行为依从: 环格
	"health":  {topology: "ring", rewire: false, memory: 2, agents: 8, rounds: 7, why: "疫苗/口罩依从主要看邻里与同伴行为, 影响半径有限"},
	"climate": {topology: "ring", rewire: false, memory: 2, agents: 8, rounds: 7, why: "垃圾分类/节水是社区层面的行为规范扩散, 影响限于邻里"},

	// 所有人对所有人可见: 完全图
	"market":     {topology: "full", rewire: false, memory: 2, agents: 6, rounds: 7, why: "价格与份额是全局信号, 每个参与者都能看到同一份市场信息"},
	"game":       {topology: "full", rewire: false, memory: 2, agents: 4, rounds: 6, why: "博弈各方直接对抗, 每一方的动作所有人都看得见"},
	"policy":     {topology: "full", rewire: false, memory: 2, agents: 7, rounds: 7, why: "政策对所有利益相关方同时生效, 反应是全局可见的"},
	"team":       {topology: "full", rewire: false, memory: 1, agents: 6, rounds: 5, why: "5~8 人小组开会, 每个人都能听见所有人, 轮次自然偏短"},
	"education":  {topology: "full", rewire: false, memory: 2, agents: 7, rounds: 6, why: "同班同伴效应是全班可见的, 不需要网络结构近似"},
	"montecarlo": {topology: "full", rewire: false, memory: 0, agents: 5, rounds: 4, why: "蒙特卡洛不跑轮次内核, 只按场景数铺世界线"},
	"predict":    {rounds: 0, why: "预测走辩论-融合链路, 轮次由引擎按分析师数决定"},
}

// 目标文本里的复杂度信号: 命中越多, 轮次帽越高 (演化越可能有多个阶段)。
// 只调**帽子**, 不调 agent 数 —— 角色数由模式决定, 与问题长度无关。
var complexityHints = []string{
	"长期", "多年", "三个阶段", "阶段", "逐步", "连锁", "反复", "持续", "演化",
	"先后", "随后", "然后", "最终", "一系列", "多次", "博弈",
}

// planFor 生成运行参数建议。kind=predict 时只给轮次 (拓扑不适用)。
func planFor(kind, mode, objective string) *RunPlan {
	if kind == "predict" {
		p := modePlans["predict"]
		return &RunPlan{Why: p.why}
	}
	mp, ok := modePlans[mode]
	if !ok {
		// 目录外的模式 (理论上不会出现, 识别结果已校验): 用社会模拟档案兜底。
		mp = modePlans["social"]
	}
	rounds := mp.rounds
	n := 0
	for _, h := range complexityHints {
		if strings.Contains(objective, h) {
			n++
		}
	}
	if n >= 2 {
		rounds += 2
	} else if n == 1 {
		rounds++
	}
	// 上限 10: 每轮 N 个 agent 各一次 LLM 调用, 10×8 已在 15 分钟任务预算内;
	// 下限 4: 再少就几乎必然跑到帽 (回到"3 轮全 budget"的老问题)。
	if rounds > 10 {
		rounds = 10
	}
	if rounds < 4 {
		rounds = 4
	}
	why := mp.why
	if n > 0 {
		why += fmt.Sprintf("; 问题含 %d 个阶段信号, 轮次帽 +%d", n, rounds-mp.rounds)
	}
	return &RunPlan{
		Rounds:       rounds,
		MinRounds:    3,
		Agents:       mp.agents,
		Topology:     mp.topology,
		Rewire:       mp.rewire,
		MemoryRounds: mp.memory,
		Epsilon:      0.03,
		Why:          why,
	}
}
