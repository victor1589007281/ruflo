package swarm_intel

// 跨会话学习闭环 (design/08 §2 #5/#6/#7)。
//
// 问题: Engine 每次 NewEngine 都把 conformal(保形校准)/byzantineFuser(信任)/banditRouter(路由)
// 重新清零, 且 Predict 从不拿到"真实胜选"反馈 → Brier 恒 0、CI 恒 ±0.15、信任/路由学了就蒸发。
//
// 解法:
//   - #5 RecordOutcome: 事后拿到"实际胜选结局"→ 算 Brier + 更新推理库真实分。
//   - #6 conformal: 把 nonconformity(=1−P(actual)) 喂 AddScore → CI 反映群体真实决断力, 跨书累积。
//   - #7 信任/路由: 按各 agent 是否押中胜选结局更新 trust/bandit, 并**持久化 + 消费**
//        (TrustWeightedTrimmedFuse 在有学习信任时剔除低信任 agent; 无信任时与 TrimmedFuse 完全一致)。
//
// **隔离**: 只有传入独立 DataDir 的调用方(novel-v3 用 …/swarm_intel/novel)才会写/读 learning_state.json;
// 共享 DataDir 的 aiops 从不调 RecordOutcome → trustScores 恒空 → TrustWeightedTrimmedFuse 退化为
// TrimmedFuse, Predict 输出逐字节不变。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ── 各组件状态快照/恢复 (供跨会话持久化) ──

// Snapshot 导出保形分数历史。
func (c *ConformalCalibrator) Snapshot() []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]float64, len(c.scoreHist))
	copy(out, c.scoreHist)
	return out
}

// Restore 恢复保形分数历史 (裁到 maxHist)。
func (c *ConformalCalibrator) Restore(scores []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(scores) > c.maxHist {
		scores = scores[len(scores)-c.maxHist:]
	}
	c.scoreHist = append([]float64{}, scores...)
}

// Snapshot 导出 Beta 分布参数。
func (br *BanditRouter) Snapshot() (map[string]float64, map[string]float64) {
	br.mu.Lock()
	defer br.mu.Unlock()
	a := make(map[string]float64, len(br.alphas))
	b := make(map[string]float64, len(br.betas))
	for k, v := range br.alphas {
		a[k] = v
	}
	for k, v := range br.betas {
		b[k] = v
	}
	return a, b
}

// Restore 恢复 Beta 分布参数 (合并到已注册 agent 上)。
func (br *BanditRouter) Restore(alphas, betas map[string]float64) {
	br.mu.Lock()
	defer br.mu.Unlock()
	for k, v := range alphas {
		if v > 0 {
			br.alphas[k] = v
		}
	}
	for k, v := range betas {
		if v > 0 {
			br.betas[k] = v
		}
	}
}

// SnapshotTrust 导出信任分数。
func (bf *ByzantineFuser) SnapshotTrust() map[string]float64 {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	out := make(map[string]float64, len(bf.trustScores))
	for k, v := range bf.trustScores {
		out[k] = v
	}
	return out
}

// RestoreTrust 恢复信任分数。
func (bf *ByzantineFuser) RestoreTrust(scores map[string]float64) {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	for k, v := range scores {
		bf.trustScores[k] = v
	}
}

// TrustWeightedTrimmedFuse 信任感知的截尾融合 (#7 消费点)。
// 有学习到的信任分时, 先剔除低信任 (< 0.15, 疑似拜占庭) 的 agent 再截尾融合;
// **无信任分 (trustScores 空, 如 aiops) 时行为与 TrimmedFuse 逐字节一致** —— 保证共享路径零回归。
func (bf *ByzantineFuser) TrustWeightedTrimmedFuse(predictions []AgentPrediction) map[string]float64 {
	bf.mu.Lock()
	hasTrust := len(bf.trustScores) > 0
	kept := predictions
	if hasTrust {
		var filtered []AgentPrediction
		for _, p := range predictions {
			t, ok := bf.trustScores[p.AgentID]
			if !ok {
				t = 0.5 // 未知 agent 给中性信任, 不剔除
			}
			if t >= 0.15 {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) >= 3 { // 不过度剪枝: 剩余不足 3 个则保留原集
			kept = filtered
		}
	}
	bf.mu.Unlock()
	return bf.TrimmedFuse(kept) // 复用既有算法 (它自行加锁)
}

// ── Engine 级学习状态持久化 ──

// LearningState 跨会话学习状态 (保形分数 + 路由 + 信任 + Brier 历史)。
type LearningState struct {
	ConformalScores []float64          `json:"conformal_scores"`
	BanditAlphas    map[string]float64 `json:"bandit_alphas"`
	BanditBetas     map[string]float64 `json:"bandit_betas"`
	TrustScores     map[string]float64 `json:"trust_scores"`
	Resolutions     int                `json:"resolutions"`   // 累计已反馈胜选次数
	MeanBrier       float64            `json:"mean_brier"`    // 滑动平均 Brier (可观测)
	UpdatedAt       time.Time          `json:"updated_at"`
}

func (e *Engine) learningStatePath() string {
	if e.dataDir == "" {
		return ""
	}
	return filepath.Join(e.dataDir, "learning", "learning_state.json")
}

// loadLearning 在 NewEngine 后恢复持久化的学习状态 (容错: 缺失/损坏则跳过)。
func (e *Engine) loadLearning() {
	p := e.learningStatePath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var st LearningState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	if len(st.ConformalScores) > 0 {
		e.conformal.Restore(st.ConformalScores)
	}
	if len(st.BanditAlphas) > 0 {
		e.banditRouter.Restore(st.BanditAlphas, st.BanditBetas)
	}
	if len(st.TrustScores) > 0 {
		e.byzantineFuser.RestoreTrust(st.TrustScores)
	}
	e.resolutions = st.Resolutions
	e.meanBrier = st.MeanBrier
}

// saveLearning 持久化当前学习状态 (原子写)。
func (e *Engine) saveLearning() {
	p := e.learningStatePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return
	}
	alphas, betas := e.banditRouter.Snapshot()
	st := LearningState{
		ConformalScores: e.conformal.Snapshot(),
		BanditAlphas:    alphas,
		BanditBetas:     betas,
		TrustScores:     e.byzantineFuser.SnapshotTrust(),
		Resolutions:     e.resolutions,
		MeanBrier:       e.meanBrier,
		UpdatedAt:       time.Now(),
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0644) == nil {
		os.Rename(tmp, p)
	}
}

// RecordOutcome 事后反馈"实际胜选结局", 驱动跨会话学习 (design/08 §2 #5/#6/#7)。
//
// actualOutcome: 最终被采纳/写入成书的结局标签 (即committed的叙事走向)。
// 返回本次 Brier 分数。这是一种"自一致性校准": 群体越决断 (胜选结局概率越高)、
// nonconformity 越低 → 未来 CI 越窄; 反之群体越犹疑 → CI 越宽 (更诚实的不确定性)。
// 非外部真值校准, 但对"让 CI 反映群体真实决断力 + 奖励押中委托叙事的分析师"是有效且自洽的。
func (e *Engine) RecordOutcome(pred *FusedPrediction, actualOutcome string) float64 {
	if pred == nil || actualOutcome == "" {
		return 0
	}
	brier := ComputeBrierScore(pred, actualOutcome)

	// #6: nonconformity = 1 − P(actual) 喂保形校准历史
	pActual := 0.0
	for _, o := range pred.Outcomes {
		if o.Outcome == actualOutcome {
			pActual = o.Probability
			break
		}
	}
	e.conformal.AddScore(1.0 - pActual)

	// #7: 按各分析师是否押中胜选结局更新信任 + 路由
	for _, a := range pred.Agents {
		pa := a.Predictions[actualOutcome]
		e.byzantineFuser.UpdateTrust(a.AgentID, (pa-0.5)*0.2) // 小步长, 防单本剧烈漂移
		e.banditRouter.Update(a.AgentID, pa >= 0.4)
	}

	// #5: 记录真实 Brier 到预测历史 + 推理库 (供未来 FindSimilar 拿到已校准先验)
	if e.predHistory != nil {
		now := time.Now()
		e.predHistory.Append(&PredictionRecord{
			ID:          "resolved-" + now.Format("20060102150405.000"),
			Question:    pred.Question,
			Outcomes:    pred.Outcomes,
			Consensus:   pred.Consensus,
			BrierScore:  brier,
			Rounds:      pred.Rounds,
			ActualLabel: actualOutcome,
			Resolved:    true,
			CreatedAt:   now,
			ResolvedAt:  &now,
		})
	}
	if e.reasoningBank != nil {
		for _, a := range pred.Agents {
			if a.Rationale != "" && a.Predictions[actualOutcome] >= 0.4 {
				e.reasoningBank.Store(&ReasoningEntry{
					Question:   pred.Question,
					Reasoning:  a.Rationale,
					BrierScore: brier,
					Confidence: a.Confidence,
					CreatedAt:  time.Now(),
				})
			}
		}
	}

	// 滑动平均 Brier (可观测)
	e.resolutions++
	if e.resolutions == 1 {
		e.meanBrier = brier
	} else {
		e.meanBrier = 0.9*e.meanBrier + 0.1*brier
	}

	e.saveLearning()
	return brier
}
