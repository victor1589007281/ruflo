package agent

import (
	"context"
	"encoding/json"

	"fmt"
	"github.com/anthropic/claude-go/pkg/trace"
	"strings"
)

// 内容质量门禁: 给 pipeline 类写作工作流 (techblog/research 等) 补上"评审→未达标→自动修订"环。
//
// 背景: 代码类工作流有编译/测试硬门禁 + coder 自动修复 (tryGateWithRemediation); 但写作类
// 工作流是一次性串行跑完即止, tech-critic 打完分也不阻塞、不回修。本门禁把代码类已验证的
// "门禁+自动修复"模式平移到内容类: 用一次便宜的 LLM 评审打分, 未达标则把问题作为反馈注入
// 写作阶段自动重做一轮 (复用 RefineTeam 的 {user_feedback} 注入管线)。
//
// 对应 Self-Refine / evaluator-optimizer 模式。为防 token 失控: 仅对白名单工作流生效、
// 评审走单次 SimpleComplete(不开 agent/工具)、默认最多 1 轮自动修订。

const (
	contentQualityThreshold = 75 // 达标分数线 (0-100)
	contentQualityMaxRounds = 1  // 未达标时最多自动修订轮数
)

// contentQualityGated 判定工作流是否启用内容质量门禁 (保守白名单, 避免对所有工作流额外烧 token)。
func contentQualityGated(workflow string) bool {
	// 动态(自定义)工作流: 读声明式 QualityGate 字段, 不能按名字 switch(否则静默丢门禁)。
	if _, qg, isCustom := customWorkflowFlags(workflow); isCustom {
		return qg == "content"
	}
	switch strings.ToLower(strings.TrimSpace(workflow)) {
	case "techblog", "research":
		return true
	default:
		return false
	}
}

type contentVerdict struct {
	Score  int      `json:"score"`
	Pass   bool     `json:"pass"`
	Issues []string `json:"issues"`
}

// tryContentQualityGate 评审最终产出; 未达标则注入问题、自动重做主写作阶段一轮。返回(可能被改进的)结果。
func (ptm *ProductionTeamManager) tryContentQualityGate(ctx context.Context, team *ProductionTeam, executor *WorkflowExecutor, wf *WorkflowDef, results []StageResult) []StageResult {
	if ptm.llm == nil || executor == nil || wf == nil {
		return results
	}
	idx := primaryContentStageIndex(results)
	if idx < 0 {
		return results
	}

	for round := 0; round <= contentQualityMaxRounds; round++ {
		if ctx.Err() != nil {
			return results
		}
		v := ptm.runContentCritic(ctx, team.Objective, results[idx].Output)
		if v == nil {
			return results // 评审不可用 → 不阻塞交付
		}
		// 奖励持久化 (design/03 §4.2): 0-100 分归一化到 [-1,1] 落 rewards.jsonl,
		// 修复"content_gate 连续分用完即丢"(design/03 §1.4)。
		if ptm.evolution != nil {
			ptm.evolution.RecordReward(RewardEvent{
				RunID:  trace.From(ctx).RunID,
				NodeID: results[idx].Name,
				Source: "gate.content",
				Value:  float64(v.Score)/50.0 - 1.0,
				Raw:    v.Score,
				Team:   team.Name,
			})
		}
		if v.Pass || v.Score >= contentQualityThreshold {
			ptm.notify(team.ChatID, fmt.Sprintf("✅ 内容质量门禁通过 (%d/100)", v.Score))
			return results
		}
		if round >= contentQualityMaxRounds {
			ptm.notify(team.ChatID, fmt.Sprintf("🟡 内容质量 %d/100 仍未达标, 已达自动修订上限(%d轮), 按现状交付。可用 /team refine 继续人工迭代。", v.Score, contentQualityMaxRounds))
			return results
		}

		ptm.notify(team.ChatID, fmt.Sprintf("🟡 内容质量 %d/100 未达标, 自动修订第 %d 轮:\n- %s",
			v.Score, round+1, strings.Join(v.Issues, "\n- ")))

		stage := stageByName(wf, results[idx].Name)
		if stage == nil {
			return results
		}
		prev := make(map[string]string, len(results))
		for _, r := range results {
			prev[r.Name] = r.Output
		}
		// 复用 {user_feedback} 注入管线把评审问题喂回写作阶段。
		// 此处 PendingFeedback 进入时必为空 (refine 运行已在调用方排除), 用完即清。
		team.mu.Lock()
		team.PendingFeedback = "内容质量评审未达标 (" + fmt.Sprintf("%d", v.Score) + "/100), 必须针对性修正以下问题, 重写产出:\n- " + strings.Join(v.Issues, "\n- ")
		team.mu.Unlock()
		sr := executor.ExecuteSingleStage(ctx, *stage, team.Objective, prev, team)
		team.mu.Lock()
		team.PendingFeedback = ""
		team.mu.Unlock()

		if sr.Status == TaskCompleted && strings.TrimSpace(sr.Output) != "" {
			results[idx].Output = sr.Output
			if team.Blackboard != nil {
				team.Blackboard.Write(stage.Name+"-result", sr.Output, stage.Role, "result")
			}
		} else {
			return results // 重做失败 → 保留原产出
		}
	}
	return results
}

// runContentCritic 用一次廉价 LLM 调用给产出打分, 返回结构化评审 (解析失败返回 nil)。
func (ptm *ProductionTeamManager) runContentCritic(ctx context.Context, objective, deliverable string) *contentVerdict {
	deliverable = strings.TrimSpace(deliverable)
	if deliverable == "" {
		return nil
	}
	sys := "你是严格、挑剔的内容质量评审官。只依据产出本身评估其相对目标的质量(准确性、深度、结构、可读性、是否答非所问、有无明显事实/逻辑错误)。" +
		"只输出一个 JSON 对象, 不要任何解释或代码块标记。"
	user := fmt.Sprintf(`目标:
%s

待评审产出:
%s

请严格评分并只输出如下 JSON:
{"score": 0到100的整数, "pass": 是否达到可交付质量的布尔, "issues": ["具体、可操作的改进点", "..."]}
要求: score<%d 时 pass 必须为 false; issues 给出最关键的 2-5 条, 必须具体到"改什么、怎么改"。`,
		truncateResult(objective, 1500), truncateResult(deliverable, 12000), contentQualityThreshold)

	out, err := ptm.llm.SimpleComplete(ctx, sys, user)
	if err != nil {
		return nil
	}
	js := extractJSONObject(out)
	if js == "" {
		return nil
	}
	var v contentVerdict
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		return nil
	}
	if v.Score < 0 {
		v.Score = 0
	}
	if v.Score > 100 {
		v.Score = 100
	}
	return &v
}

// primaryContentStageIndex 在已完成结果中选"主产出"阶段: 输出最长且非评审/审查类的阶段。
func primaryContentStageIndex(results []StageResult) int {
	best, bestLen := -1, 0
	for i, r := range results {
		if r.Status != TaskCompleted || strings.TrimSpace(r.Output) == "" {
			continue
		}
		lname := strings.ToLower(r.Name + " " + r.Role)
		if strings.Contains(lname, "critic") || strings.Contains(lname, "review") || strings.Contains(lname, "评审") {
			continue
		}
		if len(r.Output) > bestLen {
			best, bestLen = i, len(r.Output)
		}
	}
	return best
}

func stageByName(wf *WorkflowDef, name string) *StageDef {
	for i := range wf.Stages {
		if wf.Stages[i].Name == name {
			return &wf.Stages[i]
		}
	}
	return nil
}

// extractJSONObject 从可能含散文/```json 包裹的文本里抽取第一个平衡的 JSON 对象。
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
