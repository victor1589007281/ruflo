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
// **隔离**: 写侧只由 RecordOutcome/RegisterResolution 触发 —— 共享 DataDir 的 aiops 从不调用
// → trustScores 恒空 → TrustWeightedTrimmedFuse 退化为 TrimmedFuse, Predict 输出逐字节不变;
// 读侧 (loadLearning) 启动时无条件执行, 有历史就恢复, 没有就跳过。

import (
	"log"
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

// ── Engine 级学习状态持久化 (v3 Phase 0 起经 SwarmStore, 后端可插拔) ──

// LearningState 跨会话学习状态 (保形分数 + 路由 + 信任 + Brier 历史)。
type LearningState struct {
	ConformalScores []float64          `json:"conformal_scores"`
	BanditAlphas    map[string]float64 `json:"bandit_alphas"`
	BanditBetas     map[string]float64 `json:"bandit_betas"`
	TrustScores     map[string]float64 `json:"trust_scores"`
	Resolutions     int                `json:"resolutions"` // 累计已反馈胜选次数
	MeanBrier       float64            `json:"mean_brier"`  // 滑动平均 Brier (可观测)
	UpdatedAt       time.Time          `json:"updated_at"`
}

// loadLearning 在 NewEngine 后恢复持久化的学习状态 (E1: 无条件执行, 不再要求独立 DataDir;
// store 为 nil 或后端无历史时安全跳过)。
func (e *Engine) loadLearning() {
	if e.store == nil {
		return
	}
	st, err := e.store.LoadLearning()
	if err != nil || st == nil {
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

// saveLearning 持久化当前学习状态 (经 SwarmStore; 后端写失败仅告警, 不阻塞预测主流程)。
func (e *Engine) saveLearning() {
	if e.store == nil {
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
	if err := e.store.SaveLearning(&st); err != nil {
		log.Printf("[swarm_intel] WARN 学习状态持久化失败: %v", err)
	}
}

// RecordOutcome 事后反馈"实际胜选结局", 驱动跨会话学习 (design/08 §2 #5/#6/#7)。
//
// actualOutcome: 最终被采纳/写入成书的结局标签 (即committed的叙事走向)。
// 返回本次 Brier 分数。这是一种"自一致性校准": 群体越决断 (胜选结局概率越高)、
// nonconformity 越低 → 未来 CI 越窄; 反之群体越犹疑 → CI 越宽 (更诚实的不确定性)。
// 非外部真值校准, 但对"让 CI 反映群体真实决断力 + 奖励押中委托叙事的分析师"是有效且自洽的。
//
// 算法解读：
//
//	原理：拿"实际胜选结局"回填四个学习器 —— Brier 分进预测历史与推理库, nonconformity
//	      (1 − P(actual)) 进保形校准, 信任/路由按各 agent 是否押中做小步更新, 最后用
//	      0.9/0.1 的 EWMA 维护可观测的 meanBrier。
//	关键不变量：保形喂的必须是 1 − P(actual) 而不是 1 − P(consensus) —— 前者衡量群体对
//	      "真结局"的把握, 后者只会让 CI 反映群体自我一致性, 校准就失效了。
//	代价与边界：EWMA 系数 0.9 意味着有效记忆约 10 本, 单本异常结局最多把 meanBrier 拉动
//	      10%; 信任更新步长上限 (pa−0.5)×0.2 即每次 ±0.1, 刻意压制单本漂移。
//	设计权衡：不做外部真值校准, 而是"自一致性校准" —— 放弃与客观真值对齐, 换取对"群体越
//	      决断 CI 越窄"这一自洽信号的利用 (见上文)。
//
// E1 之后 RecordOutcome 与 RegisterResolution 共用 appendResolvedRecord/applyOutcomeLearning
// 两个核心, 区别只在预测来源 (调用方持对象 vs 按 predID 找回)。
func (e *Engine) RecordOutcome(pred *FusedPrediction, actualOutcome string) float64 {
	if pred == nil || actualOutcome == "" {
		return 0
	}
	brier := ComputeBrierScore(pred, actualOutcome)
	e.appendResolvedRecord(pred, actualOutcome, brier)
	e.applyOutcomeLearning(pred, actualOutcome, brier)
	e.appendReflection(pred, actualOutcome, brier)
	return brier
}

// RegisterResolution E1 导出的回填钩子: 供 webapp/aiops 等外部方在拿到真实结局后
// 按预测 ID 核销 —— 学习闭环默认接通的对外入口。
//
// 流程: pending 内存表命中 → 直接核销; 未命中 (引擎重启) → 从存储按 ID 找回未决记录重建;
// 写历史优先走后端可选接口原地标记已决 (predictionResolver), 不支持的骨架后端退回
// 追加 resolved 记录的兼容路径; 随后 conformal/信任/路由/meanBrier 全部照常更新并持久化。
// 返回本次 Brier 分数; predID 未知、已决或 actualOutcome 为空时返回 0。
func (e *Engine) RegisterResolution(predID, actualOutcome string) float64 {
	if predID == "" || actualOutcome == "" {
		return 0
	}
	pred := e.takePending(predID)
	if pred == nil {
		pred = e.reconstructPending(predID)
	}
	if pred == nil {
		return 0 // 未知或已核销的 predID: 静默跳过, 外部重试/乱序调用均安全
	}

	brier := ComputeBrierScore(pred, actualOutcome)
	resolvedInPlace := false
	if e.store != nil {
		if r, ok := e.store.(predictionResolver); ok {
			if err := r.ResolvePrediction(predID, actualOutcome, brier, time.Now()); err == nil {
				resolvedInPlace = true
				e.storeResolvedReasoning(pred, actualOutcome, brier)
			}
		}
	}
	if !resolvedInPlace {
		e.appendResolvedRecord(pred, actualOutcome, brier)
	}
	e.applyOutcomeLearning(pred, actualOutcome, brier)
	e.appendReflection(pred, actualOutcome, brier)
	return brier
}

// appendReflection W1 Phase 3 跨会话反思: 结局入库时同步写一条结构化反思
// (A6 事件流 + 向量桶), Predict 的 Scout 阶段据此检索"同域历史反思"注入先验。
// 仅 warn 不回滚: 反思是增强路径, 学习闭环的主存储 (已决记录+学习状态) 已先落。
func (e *Engine) appendReflection(pred *FusedPrediction, actualOutcome string, brier float64) {
	if e.store == nil {
		return
	}
	rec := composeReflection(pred, actualOutcome, brier, time.Now())
	if rec == nil {
		return
	}
	if err := e.store.AppendReflection(rec); err != nil {
		log.Printf("[swarm_intel] WARN 反思持久化失败: %v", err)
	}
}

// appendResolvedRecord #5: 追加一条 resolved 预测记录 (兼容路径; 已随 Agents 持久化,
// 未来 FindSimilar/RegisterResolution 都能拿到完整的各分析师预测)。
func (e *Engine) appendResolvedRecord(pred *FusedPrediction, actualOutcome string, brier float64) {
	if e.store == nil {
		return
	}
	now := time.Now()
	if err := e.store.AppendPrediction(&PredictionRecord{
		ID:          "resolved-" + now.Format("20060102150405.000"),
		Question:    pred.Question,
		Outcomes:    pred.Outcomes,
		Consensus:   pred.Consensus,
		BrierScore:  brier,
		Rounds:      pred.Rounds,
		Agents:      pred.Agents,
		ActualLabel: actualOutcome,
		Resolved:    true,
		CreatedAt:   now,
		ResolvedAt:  &now,
	}); err != nil {
		log.Printf("[swarm_intel] WARN 已决记录持久化失败: %v", err)
	}
	e.storeResolvedReasoning(pred, actualOutcome, brier)
}

// storeResolvedReasoning #5 的推理库半边: 押中结局且给出推理的 agent 的真实分入库
// (供未来 SearchReasonings 拿到已校准先验)。
func (e *Engine) storeResolvedReasoning(pred *FusedPrediction, actualOutcome string, brier float64) {
	if e.store == nil {
		return
	}
	for _, a := range pred.Agents {
		if a.Rationale != "" && a.Predictions[actualOutcome] >= 0.4 {
			if err := e.store.StoreReasoning(&ReasoningEntry{
				Question:   pred.Question,
				Reasoning:  a.Rationale,
				BrierScore: brier,
				Confidence: a.Confidence,
				CreatedAt:  time.Now(),
			}); err != nil {
				log.Printf("[swarm_intel] WARN 推理路径持久化失败: %v", err)
			}
		}
	}
}

// applyOutcomeLearning #6/#7 学习核心 (RecordOutcome 与 RegisterResolution 共用):
// nonconformity(=1−P(actual)) 喂保形, 按各 agent 是否押中更新信任/路由,
// EWMA 维护 meanBrier, 最后持久化全量学习状态。
func (e *Engine) applyOutcomeLearning(pred *FusedPrediction, actualOutcome string, brier float64) {
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

	// 滑动平均 Brier (可观测)
	e.resolutions++
	if e.resolutions == 1 {
		e.meanBrier = brier
	} else {
		e.meanBrier = 0.9*e.meanBrier + 0.1*brier
	}

	e.saveLearning()
}
