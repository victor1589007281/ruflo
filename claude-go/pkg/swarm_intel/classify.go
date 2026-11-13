package swarm_intel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 场景识别 (一键模式的路由器): 一次 LLM 调用把用户问题路由到
//   - kind: predict (要概率/置信区间的未来判断) 还是 simulate (要演化过程/涌现行为)
//   - modes: 1~3 个模拟模式 (首个为主模式), 严格限定在 ListSimulationModes 目录内
//
// 设计约束:
//   - LLM 失败/输出非法时绝不硬失败 — 关键词兜底 + Fallback 标记, 调用方无感降级
//   - 识别结果只是"建议", webapp 一键页展示后自动按它发起, 用户仍可切手动页改

// ModeMatch 单个模式的识别结果。Score 是 LLM 自报置信度, 仅用于展示排序参考。
type ModeMatch struct {
	Mode  string  `json:"mode"`
	Name  string  `json:"name"`
	Emoji string  `json:"emoji,omitempty"`
	Score float64 `json:"score,omitempty"`
}

// Classification 场景识别结果。
type Classification struct {
	Kind     string      `json:"kind"`               // predict | simulate
	Modes    []ModeMatch `json:"modes"`              // 1~3 个, 首个为主模式
	Reason   string      `json:"reason"`             // 一句话中文理由 (展示给用户)
	Fallback bool        `json:"fallback,omitempty"` // true = LLM 不可用, 走了关键词兜底
}

// Primary 主模式 (第一个); 空目录兜底 social。
func (c *Classification) Primary() string {
	if len(c.Modes) > 0 {
		return c.Modes[0].Mode
	}
	return "social"
}

// modeHints 分类提示词: 每个模式"什么问题适合"的一句话, 从 ListSimulationModes 派生目录,
// 这里补充路由提示 (Desc 面向用户展示, hint 面向路由器判定, 口径分开)。
var modeHints = map[string]string{
	"social":     "公众态度/舆论走向/共识与极化/大家对某事的看法分化 (兜底)",
	"game":       "多方策略对抗/竞合/谈判/零和博弈",
	"montecarlo": "要多条未来世界线的概率分布/大范围不确定性推演",
	"crisis":     "突发事件/危机升级/应急响应/次生风险",
	"org":        "层级组织/公司部门/上下级/文化变革/执行走样",
	"creative":   "创意方向/审美趋势/内容风格/头脑风暴",
	"market":     "金融市场/行业景气/价格份额/竞争格局",
	"policy":     "政策出台后的各方反应/执行走样/利益分配",
	"tech":       "技术路线之争/采用率/生态位/颠覆",
	"rumor":      "谣言/辟谣/信息真伪信念/信源信任/反转",
	"health":     "疫苗/口罩/隔离等公共健康行为依从/疫情下的行为",
	"consumer":   "新品接受度/口碑种草拔草/品牌社群/消费决策",
	"team":       "5~8人小组开会拍板/从众/责任分散/方案之争 (扁平小组, 非层级组织)",
	"education":  "课堂/校园/同伴效应/学习方法流行/学生群体行为",
	"climate":    "垃圾分类/低碳/节水/社区环保行为改变",
}

// ClassifyObjective 场景识别: 输入用户问题, 输出路由建议。永不返回 error — 失败时兜底。
func (e *Engine) ClassifyObjective(ctx context.Context, objective string) *Classification {
	c := &Classification{Kind: "simulate", Reason: "默认路由"}
	resp, err := e.llm.SimpleComplete(ctx,
		"你是群体智能场景路由器。只输出严格 JSON, 不要输出任何其他文字。", classifyPrompt(objective))
	if err != nil {
		return keywordClassify(objective)
	}
	wire := struct {
		Kind  string `json:"kind"`
		Modes []struct {
			Mode  string  `json:"mode"`
			Score float64 `json:"score"`
		} `json:"modes"`
		Reason string `json:"reason"`
	}{}
	if err := json.Unmarshal([]byte(extractJSON(resp)), &wire); err != nil {
		return keywordClassify(objective)
	}
	// 校验 kind: 目录外一律 simulate (演化是安全默认; predict 的 conformal 口径更挑输入)
	if wire.Kind == "predict" {
		c.Kind = "predict"
	}
	// 校验 modes: 目录外丢弃, 去重, 截到 3 个; 全空兜底 social。
	// picked 从空集开始 — 预置默认 social 会在去重前污染结果 (实测 LLM 也爱回显 social)。
	picked := []ModeMatch{}
	seen := map[string]bool{}
	for _, m := range wire.Modes {
		if seen[m.Mode] || !isKnownMode(m.Mode) {
			continue
		}
		seen[m.Mode] = true
		picked = append(picked, modeMatch(m.Mode, m.Score))
		if len(picked) == 3 {
			break
		}
	}
	if len(picked) == 0 {
		picked = []ModeMatch{socialMatch(0.5)}
	}
	c.Modes = picked
	if strings.TrimSpace(wire.Reason) != "" {
		c.Reason = strings.TrimSpace(wire.Reason)
	}
	return c
}

// classifyPrompt 从 ListSimulationModes 生成模式目录, 保证目录与内核注册表单一来源。
func classifyPrompt(objective string) string {
	var b strings.Builder
	b.WriteString("从下面的模式目录中, 为用户问题选最合适的模拟模式 (1~3 个, 第一个为主模式), 并判断问题类型。\n\n【模式目录】\n")
	for _, m := range ListSimulationModes() {
		hint := modeHints[m.Mode]
		if hint == "" {
			hint = m.Desc
		}
		fmt.Fprintf(&b, "- %s %s (%s): %s\n", m.Emoji, m.Mode, m.Name, hint)
	}
	b.WriteString(`
【判定规则】
1. kind: 问题想要"概率多大/会不会/是否超过"这类带置信区间的判断 → predict;
   想要"会怎样演化/谁影响谁/会涌现什么行为" → simulate; 拿不准一律 simulate。
2. modes: 只能选目录里列出的 mode 值; 主模式放第一个; 最多 3 个; 都不贴切就选 social。
3. score: 你对该匹配的置信度 0~1。
4. reason: 一句话中文, 向用户解释为什么这个模式最贴切。

【输出格式】严格 JSON (不要 markdown 代码块, 不要任何其他文字):
{"kind":"simulate","modes":[{"mode":"social","score":0.9}],"reason":"..."}

【用户问题】
`)
	b.WriteString(objective)
	return b.String()
}

func isKnownMode(mode string) bool {
	for _, m := range ListSimulationModes() {
		if m.Mode == mode {
			return true
		}
	}
	return false
}

func modeMatch(mode string, score float64) ModeMatch {
	for _, m := range ListSimulationModes() {
		if m.Mode == mode {
			return ModeMatch{Mode: m.Mode, Name: m.Name, Emoji: m.Emoji, Score: score}
		}
	}
	return socialMatch(0)
}

func socialMatch(score float64) ModeMatch {
	return ModeMatch{Mode: "social", Name: "社会模拟", Emoji: "🌐", Score: score}
}

// keywordClassify LLM 失败时的兜底: 关键词打分, 最高分模式胜出。
// 规则表是有序切片 (不是 map): 同分时排前者胜出 — 新领域模式放前面,
// 它们语义更专门, 平手时应压过 market/policy/org 等泛化模式; 全零分兜底 social。
// 宁可粗不可错 — 兜底结果带 Fallback 标记, 前端可提示"识别降级"。
var kwRules = []struct {
	mode  string
	words []string
}{
	{"rumor", []string{"谣言", "辟谣", "传言", "传闻", "真伪", "信源"}},
	{"health", []string{"疫苗", "疫情", "口罩", "防疫", "隔离", "流感"}},
	{"consumer", []string{"新品", "口碑", "种草", "拔草", "品牌", "消费", "购买", "下单"}},
	{"team", []string{"团队", "开会", "小组", "拍板", "决策会"}},
	{"education", []string{"课堂", "学生", "校园", "同学", "老师", "学习"}},
	{"climate", []string{"垃圾", "分类", "低碳", "环保", "节水", "减排"}},
	{"crisis", []string{"危机", "突发", "应急", "事故", "灾难"}},
	{"org", []string{"公司", "部门", "组织", "职场", "KPI", "裁员"}},
	{"game", []string{"博弈", "谈判", "对抗", "竞价", "策略"}},
	{"creative", []string{"创意", "设计", "审美", "风格", "内容"}},
	{"market", []string{"市场", "价格", "股价", "份额", "行业", "竞争格局", "房价", "加息"}},
	{"policy", []string{"政策", "法规", "监管", "新规", "补贴", "税收"}},
	{"tech", []string{"技术", "框架", "开源", "路线", "芯片", "模型"}},
}

func keywordClassify(objective string) *Classification {
	best, bestN := "social", 0
	for _, rule := range kwRules {
		n := 0
		for _, w := range rule.words {
			if strings.Contains(objective, w) {
				n++
			}
		}
		// 严格大于才替换: 同分时规则表排前者胜出 (确定性, 与 map 迭代序无关)
		if n > bestN {
			best, bestN = rule.mode, n
		}
	}
	return &Classification{
		Kind:     "simulate",
		Modes:    []ModeMatch{modeMatch(best, 0.3)},
		Reason:   fmt.Sprintf("按关键词匹配到「%s」(识别服务降级, 建议手动确认)", modeMatch(best, 0).Name),
		Fallback: true,
	}
}
