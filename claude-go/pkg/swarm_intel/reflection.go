package swarm_intel

// W1 Phase 3 跨会话反思 (tau2 式, 规划 13.10.2 Phase 3)。
//
// 动机: RecordOutcome/RegisterResolution 只更新数值学习状态 (conformal/信任/
// bandit/meanBrier), 跨会话只留下了"分", 没留下"教训"。Phase 3 要求结局入库时
// 同时写一条结构化反思记录, Predict 的 Scout 阶段可检索"同域历史反思"注入先验,
// 让群体在新问题上能回忆起"上次同类问题我们错在哪"。
//
// 设计取舍:
//   - 反思文本是**确定性模板**生成的 (命中/偏差 + 概率分布摘要), 不走 LLM:
//     学习闭环的写侧必须离线可测、零额外延迟, LLM 总结留给未来的增强层。
//   - Domain 用 outcome_type/horizon 口径 (如 "binary/short"), 而不是自由文本
//     域标签 —— decompose 的产物稳定、可枚举, 检索侧语义匹配交给向量桶。
//   - 向量检索是增强: Memory 桶不可用时 SearchReflections 回退关键词重叠
//     (与 Filtered ANN 同一回退哲学, 见 swarmstore.go)。

import (
	"fmt"
	"time"
)

// ReflectionRecord 一条结构化反思 (跨会话可检索)。
type ReflectionRecord struct {
	ID        string              `json:"id"`         // 服务端事件 seq 或本地时间戳 ID
	Domain    string              `json:"domain"`     // outcome_type/horizon, 如 "binary/short"
	Question  string              `json:"question"`   // 原预测问题 (语义检索主键)
	TopPick   string              `json:"top_pick"`   // 融合后概率最高的结局
	TopProb   float64             `json:"top_prob"`   // 最高概率
	Actual    string              `json:"actual"`     // 实际结局
	Brier     float64             `json:"brier"`      // 本次 Brier 分数
	Lesson    string              `json:"lesson"`     // 一句话教训 (模板生成)
	Outcomes  []OutcomePrediction `json:"outcomes"`   // 当时的完整融合分布 (审计)
	CreatedAt time.Time           `json:"created_at"` // 反思生成时间 (= 结局回填时间)
}

// composeReflection 从融合结果 + 真实结局构造反思记录。
// brier 由调用方 (RecordOutcome/RegisterResolution) 算好传入 —— 与入库的
// BrierScore 同源, 不做第二次计算。
func composeReflection(pred *FusedPrediction, actual string, brier float64, now time.Time) *ReflectionRecord {
	if pred == nil {
		return nil
	}
	rec := &ReflectionRecord{
		ID:        fmt.Sprintf("refl-%s", now.Format("20060102150405.000000000")),
		Domain:    reflectionDomain(pred),
		Question:  pred.Question,
		Actual:    actual,
		Brier:     brier,
		Outcomes:  pred.Outcomes,
		CreatedAt: now,
	}
	for _, o := range pred.Outcomes {
		if o.Probability > rec.TopProb {
			rec.TopProb = o.Probability
			rec.TopPick = o.Outcome
		}
	}
	rec.Lesson = composeLesson(rec)
	return rec
}

// reflectionDomain 派生反思的领域标签。
// FusedPrediction 不携带 decompose 的 OutcomeType/Horizon (Predict 流程没把它
// 传到持久化层), 这里用结果空间形状近似: 2 结局 = binary, 3-6 = categorical,
// 更多 = scenario。horizon 一律记 "unknown" —— 检索靠语义, 不靠该字段精确匹配。
func reflectionDomain(pred *FusedPrediction) string {
	shape := "scenario"
	switch n := len(pred.Outcomes); {
	case n <= 2:
		shape = "binary"
	case n <= 6:
		shape = "categorical"
	}
	return shape + "/unknown"
}

// composeLesson 一句话教训 (确定性模板):
//   - 命中且高置信: 确认直觉方向, 提示关注置信校准
//   - 命中但低置信: 群体过度保守, 下次可更决断
//   - 未命中: 明确指出被高估/低估的结局对, 这是 Scout 先验里最可执行的信息
func composeLesson(rec *ReflectionRecord) string {
	hit := rec.TopPick == rec.Actual
	switch {
	case hit && rec.TopProb >= 0.7:
		return fmt.Sprintf("预测命中: 「%s」(P=%.2f) 兑现。高置信直觉在本域有效, 保持。", rec.TopPick, rec.TopProb)
	case hit:
		return fmt.Sprintf("预测命中但置信不足: 「%s」只给了 P=%.2f, 群体偏保守, 同类问题下次可更决断。", rec.TopPick, rec.TopProb)
	default:
		actualProb := 0.0
		for _, o := range rec.Outcomes {
			if o.Outcome == rec.Actual {
				actualProb = o.Probability
			}
		}
		return fmt.Sprintf("预测偏差: 高估「%s」(P=%.2f), 低估「%s」(实给 P=%.2f)。同类问题 Scout 时应主动寻找 \"%s\" 的证据。",
			rec.TopPick, rec.TopProb, rec.Actual, actualProb, rec.Actual)
	}
}
