// quality_jev.go 给 swarm 的 qualityGate 并行接入决策模型打分（P1，双跑观察模式）。
//
// 背景（docforge 第 19 章判断层）：qualityGate 现在的置信度是"长度先验"——
//   conf = 0.5 + 长度>200?0.1 + 长度>500?0.1 + 非平凡artifact?0.2 + 有结构?0.1
// 一份又长又错的答案拿 0.9，一份又短又对的拿 0.5。这不是判断，是长度。
// 本文件在不改变现有行为的前提下，并行用决策模型（jeb:4b）打一次真分数，
// 两个值都写进 agentDB 流水，攒数据后用 ConformalCalibrator 标定真阈值再切换。
//
// 纪律（与 recall.go 一致）：
//   - 双跑默认：jeb:4b 的分数只进流水，不影响 Confidence/Degraded；
//   - fail-open：决策模型超时/失败，qualityGate 行为完全不变；
//   - 调用方（qualityGate）同步调用本函数，但本函数内部对决策调用设了超时上限，
//     无 GPU 机器上也不会无限阻塞主路径。
package agent

import (
	"context"
	"os"
	"time"

	"github.com/anthropic/claude-go/pkg/decision"
)

// qualityJevTimeout 是 qualityGate 双跑决策打分的超时上限。
// 无 GPU 机器上 jeb:4b CPU 推理随子任务输出规模上升：实测输出 6–13KB 时单次 >8s，
// 20s 也不够（一次 6KB 输出实测 18s 拿到真分数、13KB 超过 20s 超时）。
// 双跑是观察模式、不阻塞主路径（qualityGate 同步调用本函数），这里把上限抬到 60s
// 换真数据；超出再 fail-open。生产化后应换 GPU 或更小模型，或把该路径改异步。
const qualityJevTimeout = 60 * time.Second

// jevScoreOutput 用决策模型给子任务输出打质量分，与长度先验并行（双跑）。
//
// 不返回值——结果只写流水。是否真正执行由 env CLAUDE_GO_DECISION_QUALITY 控制：
//   "0"/空 = 完全不调用决策模型（连观察都不做，纯现状）；
//   其它值/未设但 CLAUDE_GO_AGENTDB_URL 非空 = 双跑（打分+写流水，不改行为）。
// 默认采用"未设即双跑"是为了让数据尽快攒起来；要彻底关停决策调用设 CLAUDE_GO_DECISION_QUALITY=0。
func (s *SwarmOrchestrator) jevScoreOutput(sr StageResult, task SubTask, lengthConf float64) {
	if os.Getenv("CLAUDE_GO_DECISION_QUALITY") == "0" {
		return
	}
	logURL := os.Getenv("CLAUDE_GO_AGENTDB_URL")
	if logURL == "" {
		return // 没有 agentDB 流水落点就不双跑（决策值无处对照，浪费推理）
	}

	// 只对"已完成且有输出"的子任务打分——qualityGate 对空输出已直接判 0，无需再评。
	if sr.Status != TaskCompleted || sr.Output == "" {
		return
	}

	rec := decision.Record{
		Kind:  "quality_gate",
		Model: decision.ModelScore,
		Extra: map[string]any{
			"task_id":     task.ID,
			"role":        task.Role,
			"length_conf": lengthConf, // 长度先验分数（对照值）
			"output_len":  len(sr.Output),
		},
	}
	logger := decision.NewLogger(logURL)
	ctx, cancel := context.WithTimeout(context.Background(), qualityJevTimeout)
	defer cancel()
	start := time.Now()
	defer func() {
		rec.LatencyMS = time.Since(start).Milliseconds()
		// 流水（尤其是 FellBack 记录）必须写：上游 8s 决策 ctx 此刻往往已耗尽，
		// 复用它会被 logger 的取消语义静默跳过。传 Background()，由 logger 自带 Timeout 兜底。
		_ = logger.Log(context.Background(), rec) // fail-open：写流水失败不阻断（见 logger 注释）
	}()

	client := &decision.Client{
		BaseURL: getenvDefault("CLAUDE_GO_DECISION_URL", decision.DefaultBaseURL),
		Timeout: qualityJevTimeout,
	}
	// Score 原语：0..4 有序量表，判输出质量。
	state := "子任务角色: " + task.Role + "\n子任务输出（截断）:\n" + truncateResult(sr.Output, 1500)
	qs := []decision.Question{{
		ID:           "output_quality",
		Type:         decision.TypeScore,
		Instructions: "评估这份子任务输出的质量：是否切题、有实质内容、可作为最终报告的可靠论据。",
		Legend:       []string{"空洞/跑题/不可用", "单薄/部分相关", "基本可用", "良好/有实质内容", "优秀/切题且深入"},
	}}

	resp, err := client.Decide(ctx, decision.ModelScore, state, qs)
	if err != nil {
		rec.FellBack = true
		rec.Extra["error"] = err.Error()
		return
	}
	ans, ok := resp.Answers["output_quality"]
	if !ok {
		rec.FellBack = true
		rec.Extra["reason"] = "no_answer"
		return
	}
	// jeb:4b 的 score 是 0..4 的期望值（Σ i·pᵢ），归一化到 0..1 与 length_conf 同口径。
	rec.Extra["jev_score_raw"] = ans.Score
	rec.Extra["jev_conf"] = ans.Score / 4.0
	rec.Extra["jev_confidence"] = ans.Confidence
	rec.Extra["jev_probabilities"] = ans.Probabilities
}
