package swarm_intel

// v3 Phase 1 E2: 判别型角色 — redteam 与 judge。
//
// 背景: v2 的融合只消费四位分析师 (optimist/pessimist/neutral/contrarian) 的点估计,
// 没有任何环节质疑"预测逻辑本身是否成立"。E2 引入两个判别角色:
//
//	redteam: 专挑预测逻辑漏洞 (锚定、幸存者偏差、时间范围偷换等), 产出 flaws 清单;
//	judge:   汇总评审红队漏洞 + 各分析师预测, 给出 extremization 强度 (0~0.3)。
//	         该强度进 FusedPrediction.Extremization, 由 E4 在 fuse 后推离 0.5。
//
// 容错: redteam/judge 任一 LLM 调用失败即降级 (extremization=0, flaws 为空),
// 主预测流程不受影响 —— 判别层是增强回路, 不是必需阶段。

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// critiqueResult E2 判别产出: redteam 漏洞清单 + judge 极端化强度。
type critiqueResult struct {
	Flaws         []string           // redteam 挑出的逻辑漏洞
	Extremization float64            // judge 给的极端化强度 (已 clamp 到 [0, 0.3])
	Grades        map[string]float64 // judge 对各分析师的评审分 (可观测, 不直接进融合)
	JudgeNote     string             // judge 结论摘要 (可观测)
}

// runCritique 执行 redteam → judge 两级判别。
// 两次 LLM 调用串行 (judge 的 prompt 依赖 redteam 的 flaws 清单)。
func (e *Engine) runCritique(ctx context.Context, domain *PredictionDomain, predictions []AgentPrediction) critiqueResult {
	flaws := e.redteamCritique(ctx, domain, predictions)
	return e.judgeCritique(ctx, domain, predictions, flaws)
}

// redteamCritique 红队评审: 专挑各分析师预测的逻辑漏洞。
// 输出 JSON {"flaws": [...]}; 解析失败/调用失败返回空集 (降级)。
func (e *Engine) redteamCritique(ctx context.Context, domain *PredictionDomain, predictions []AgentPrediction) []string {
	compressedViews := e.cc.CompressDebateView(predictions)
	prompt := fmt.Sprintf(`你是红队评审员。以下是多名分析师对同一预测问题的判断, 请专挑**预测逻辑漏洞** (不看结论对错, 只看推理过程是否成立)。

问题: %s
可能结果: %v

各分析师预测:
%s

常见漏洞类型 (供参考): 锚定效应、幸存者偏差、基率忽视、时间范围偷换、把相关性当因果、过度外推单一样本。

输出严格JSON (不要markdown代码块):
{"flaws": ["漏洞1: 具体指出哪位分析师+哪里不成立", "漏洞2"]}
没有漏洞则输出 {"flaws": []}`, domain.Question, domain.Outcomes, compressedViews)

	resp, err := e.llm.SimpleComplete(ctx,
		"你是红队评审员。专挑预测逻辑漏洞。只输出JSON。", prompt)
	if err != nil {
		return nil
	}
	parsed := ParseJSON[struct {
		Flaws []string `json:"flaws"`
	}](resp)
	if !parsed.OK {
		return nil
	}
	return parsed.Value.Flaws
}

// judgeCritique 首席评审: 汇总 redteam 漏洞 + 各分析师预测, 输出 extremization 强度。
// 强度语义: 证据充分、分歧合理且红队未推翻核心逻辑 → 接近 0.3;
// 漏洞多/证据薄 → 0。judge 给的 grades 仅作可观测输出, 不进融合权重 (E3 权重只看信任分)。
func (e *Engine) judgeCritique(ctx context.Context, domain *PredictionDomain, predictions []AgentPrediction, flaws []string) critiqueResult {
	compressedViews := e.cc.CompressDebateView(predictions)
	var flawsBlock string
	if len(flaws) > 0 {
		var parts []string
		for _, f := range flaws {
			parts = append(parts, "- "+e.cc.TruncateField(f, 120))
		}
		flawsBlock = "\n红队挑出的逻辑漏洞:\n" + strings.Join(parts, "\n")
	} else {
		flawsBlock = "\n红队未挑出明显逻辑漏洞。"
	}

	prompt := fmt.Sprintf(`你是首席预测评审。基于以下材料汇总评审, 并给出 extremization 强度。

问题: %s
各分析师预测:
%s%s

extremization 含义: 群体判断在去掉共同信息后的"独立信息分量"强度。
- 证据充分、各分析师视角真正独立、红队漏洞不触及核心逻辑 → 取 0.2~0.3 (融合概率应更极端);
- 分析师明显互相锚定、证据薄弱或红队推翻了关键假设 → 取 0~0.05 (保持融合原样)。

输出严格JSON (不要markdown代码块):
{
  "grades": {"<分析师ID>": 0-1 的评审分},
  "extremization": 0.0,
  "note": "80字以内评审结论"
}
规则: extremization 必须在 0~0.3 之间; grades 覆盖每一位分析师。`,
		domain.Question, compressedViews, flawsBlock)

	resp, err := e.llm.SimpleComplete(ctx,
		"你是首席预测评审。给出 extremization 强度。只输出JSON。", prompt)
	if err != nil {
		return critiqueResult{Flaws: flaws} // 降级: 无强度
	}
	parsed := ParseJSON[struct {
		Grades        map[string]float64 `json:"grades"`
		Extremization float64            `json:"extremization"`
		Note          string             `json:"note"`
	}](resp)
	if !parsed.OK {
		return critiqueResult{Flaws: flaws}
	}
	ext := math.Max(0, math.Min(0.3, parsed.Value.Extremization)) // judge 输出硬 clamp 到 [0, 0.3]
	return critiqueResult{
		Flaws:         flaws,
		Extremization: ext,
		Grades:        parsed.Value.Grades,
		JudgeNote:     parsed.Value.Note,
	}
}
