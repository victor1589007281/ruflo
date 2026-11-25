// quality_jev.go 给 swarm 的 qualityGate 并行接入决策模型打分（P1，双跑观察模式）。
//
// 背景（docforge 第 19 章判断层）：qualityGate 现在的置信度是"长度先验"——
//
//	conf = 0.5 + 长度>200?0.1 + 长度>500?0.1 + 非平凡artifact?0.2 + 有结构?0.1
//
// 一份又长又错的答案拿 0.9，一份又短又对的拿 0.5。这不是判断，是长度。
// 本文件在不改变现有行为的前提下，并行用决策模型（jeb:4b）打一次真分数，
// 两个值都写进 agentDB 流水，攒数据后用 ConformalCalibrator 标定真阈值再切换。
//
// 纪律（与 recall.go 一致）：
//   - 双跑默认：jeb:4b 的分数只进流水，不影响 Confidence/Degraded；
//   - fail-open：决策模型超时/失败，qualityGate 行为完全不变；
//   - 本函数立即返回，决策调用在后台执行：jeb:4b 是本机 CPU 推理，实测
//     p50 5.6s / p95 33.8s，绝不能同步压在 qualityGate 主路径上。
package agent

import (
	"context"
	"os"
	"time"

	"github.com/anthropic/claude-go/pkg/decision"
)

// qualityJevTimeout 是后台决策打分的超时上限。
// 无 GPU 机器上 jeb:4b CPU 推理随子任务输出规模上升：实测输出 6–13KB 时单次 >8s，
// 20s 也不够（一次 6KB 输出实测 18s 拿到真分数、13KB 超过 20s 超时）。
// 观察已移到后台，这里保留 60s 换真数据；配合 MaxRetries=-1，最坏只占一个
// 后台槽位 60s，不再出现 3×60s 的串行重试。
const qualityJevTimeout = 60 * time.Second

// qualityObserverConcurrency 限制后台打分并发。jeb:4b 与 snap:2b 共用同一台
// CPU，观察满载时必须丢数据而不是排队。
const qualityObserverConcurrency = 2

var qualityObserverSlots = make(chan struct{}, qualityObserverConcurrency)

// jevScoreOutput 安排一次后台质量打分（双跑观察），立即返回。
//
// 是否真正执行由 env CLAUDE_GO_DECISION_QUALITY 控制：
//
//	"0"/空 = 完全不调用决策模型（连观察都不做，纯现状）；
//	其它值/未设但 CLAUDE_GO_AGENTDB_URL 非空 = 双跑（打分+写流水，不改行为）。
//
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

	select {
	case qualityObserverSlots <- struct{}{}:
	default:
		return // 打分已满载：丢弃本次观察，不阻塞 qualityGate。
	}
	go func() {
		defer func() { <-qualityObserverSlots }()
		scoreOutputQuality(logURL, task.ID, task.Role, sr.Output, lengthConf)
	}()
}

// scoreOutputQuality 是后台质量打分的实际执行体。参数按值传入，避免与调用方
// 后续对 StageResult / SubTask 的修改产生数据竞争。
func scoreOutputQuality(logURL, taskID, role, output string, lengthConf float64) {
	rec := decision.Record{
		Kind:  "quality_gate",
		Model: decision.ModelScore,
		Extra: map[string]any{
			"task_id":     taskID,
			"role":        role,
			"length_conf": lengthConf, // 长度先验分数（对照值）
			"output_len":  len(output),
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
		BaseURL:    getenvDefault("CLAUDE_GO_DECISION_URL", decision.DefaultBaseURL),
		Timeout:    qualityJevTimeout,
		MaxRetries: -1,
	}
	// Score 原语：0..4 有序量表，判输出质量。
	state := "子任务角色: " + role + "\n子任务输出（截断）:\n" + truncateResult(output, 1500)
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
