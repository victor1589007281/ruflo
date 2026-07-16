package agent

// 面向小说平台的「群体智能」焦点工作流：把 swarm_intel 的两大引擎能力暴露成秒级可调用的
// 独立工作流，供织叙 StoryLoom 在「剧情推演 / 走向评估」等交互场景直接触发（区别于 novel-v3
// 那种一次跑整本的重工作流）。
//
//   - plot-simulate: swarm_intel.Engine.Simulate 多情景群体推演（剧情模拟 / 演化）
//   - plot-predict : swarm_intel.Engine.Predict  多分析师辩论融合（走向可信度评估）
//
// 两者都输出严格 JSON（```json 围栏），由平台侧 parseSimulation / parsePrediction 解析。

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

// ---- plot-simulate: 剧情模拟 / 演化 ----

func plotSimulateWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "plot-simulate",
		Description: "剧情模拟/演化: swarm_intel.Engine.Simulate 多情景群体推演 → JSON(情景+概率+关键事件+涌现)",
		Mode:        "plot_simulate",
	}
}

type plotScenarioJSON struct {
	Name        string   `json:"name"`
	Probability float64  `json:"probability"`
	Description string   `json:"description"`
	KeyEvents   []string `json:"keyEvents"`
}

type plotSimJSON struct {
	Mode      string             `json:"mode"`
	Scenarios []plotScenarioJSON `json:"scenarios"`
	Emergent  []string           `json:"emergent"`
	Summary   string             `json:"summary"`
}

// executePlotSimulate 用 Engine.Simulate 对「当前故事状态 + 一个转折/假设」做多情景群体推演。
func (we *WorkflowExecutor) executePlotSimulate(ctx context.Context, _ *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	notify := func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}
	if we.llm == nil {
		return nil, fmt.Errorf("plot-simulate 需要 LLMClient 以创建 swarm_intel.Engine")
	}
	notify("⚡ **剧情模拟**: swarm_intel.Engine.Simulate 多情景群体推演中…")

	cfg := swarm_intel.DefaultConfig()
	cfg.Notify = func(_, msg string) {
		notify("  [群体智能] " + msg)
		if team != nil && team.Blackboard != nil {
			team.Blackboard.Write("swarm-progress", msg, "engine", "progress")
		}
	}
	engine := swarm_intel.NewEngine(we.llm, cfg)

	// social 模式 = 人物社会动力学演化，最贴小说「人物驱动的剧情走向」。3 agent × 2 轮兼顾质量与时延。
	simCfg := swarm_intel.SimulationConfig{Mode: "social", Agents: 3, Rounds: 2}
	res, err := engine.Simulate(ctx, teamChatID(team), objective, simCfg)
	if err != nil {
		return nil, fmt.Errorf("剧情模拟失败: %w", err)
	}

	out := plotSimJSON{Mode: res.Mode, Emergent: res.Emergent, Summary: res.Summary}
	for _, sc := range res.Scenarios {
		out.Scenarios = append(out.Scenarios, plotScenarioJSON{
			Name: sc.Name, Probability: sc.Probability, Description: sc.Description, KeyEvents: sc.KeyEvents,
		})
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	output := fmt.Sprintf("剧情模拟结果（%d 个情景）:\n\n```json\n%s\n```\n", len(out.Scenarios), string(data))
	notify(fmt.Sprintf("✅ 剧情模拟完成: %d 个情景 + %d 条涌现走向", len(out.Scenarios), len(out.Emergent)))
	return []StageResult{{Name: "plot-simulate", Role: "plot-simulator", Status: TaskCompleted, Output: output}}, nil
}

// ---- plot-predict: 走向群体评估 ----

func plotPredictWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "plot-predict",
		Description: "走向评估: swarm_intel.Engine.Predict 多分析师辩论融合 → JSON(各走向概率+置信区间+共识+理据)",
		Mode:        "plot_predict",
	}
}

type plotOutcomeJSON struct {
	Outcome     string  `json:"outcome"`
	Probability float64 `json:"probability"`
	Lower95     float64 `json:"lower95"`
	Upper95     float64 `json:"upper95"`
}

type plotPredictJSON struct {
	Outcomes  []plotOutcomeJSON `json:"outcomes"`
	Consensus float64           `json:"consensus"`
	Summary   string            `json:"summary"`
	Rationale []string          `json:"rationale"`
}

// executePlotPredict 用 Engine.Predict 对「若干候选走向」做群体辩论评估，返回各走向的可信度分布。
func (we *WorkflowExecutor) executePlotPredict(ctx context.Context, _ *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	notify := func(msg string) {
		if team != nil {
			we.notify(team.ChatID, msg)
		}
	}
	if we.llm == nil {
		return nil, fmt.Errorf("plot-predict 需要 LLMClient 以创建 swarm_intel.Engine")
	}
	notify("⚡ **走向评估**: swarm_intel.Engine.Predict 多分析师辩论融合中…")

	cfg := swarm_intel.DefaultConfig()
	cfg.Notify = func(_, msg string) {
		notify("  [群体智能] " + msg)
		if team != nil && team.Blackboard != nil {
			team.Blackboard.Write("swarm-progress", msg, "engine", "progress")
		}
	}
	engine := swarm_intel.NewEngine(we.llm, cfg)

	res, err := engine.Predict(ctx, teamChatID(team), objective)
	if err != nil {
		return nil, fmt.Errorf("走向评估失败: %w", err)
	}

	out := plotPredictJSON{Consensus: res.Consensus, Summary: res.Summary}
	for _, o := range res.Outcomes {
		out.Outcomes = append(out.Outcomes, plotOutcomeJSON{
			Outcome: o.Outcome, Probability: o.Probability, Lower95: o.Lower95, Upper95: o.Upper95,
		})
	}
	// 摘出各分析师的理据（供作者看「群体为何这么判」）
	for _, a := range res.Agents {
		if a.Rationale != "" {
			out.Rationale = append(out.Rationale, fmt.Sprintf("[%s] %s", a.AgentRole, a.Rationale))
		}
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	output := fmt.Sprintf("走向评估结果（共识度 %.0f%%）:\n\n```json\n%s\n```\n", out.Consensus*100, string(data))
	notify(fmt.Sprintf("✅ 走向评估完成: %d 个走向, 共识度 %.0f%%", len(out.Outcomes), out.Consensus*100))
	return []StageResult{{Name: "plot-predict", Role: "plot-analyst", Status: TaskCompleted, Output: output}}, nil
}

// teamChatID 安全取团队 ChatID（team 可能为 nil）。
func teamChatID(team *ProductionTeam) string {
	if team == nil {
		return ""
	}
	return team.ChatID
}
