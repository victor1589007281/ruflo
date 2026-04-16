package swarm_intel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Engine 群体智能引擎。
// 5+2 阶段流水线: Decompose → Scout → Predict → Debate → Fuse → Calibrate → Learn
type Engine struct {
	llm        LLMClient
	boids      *BoidsCoordinator
	fuser      *Fuser
	pheromones *PheromoneMemory
	simulator  *Simulator
	notify     NotifyFunc

	// M10: 工程可靠性组件
	cc *ContextCompressor // 上下文压缩
	rc *ResilientCaller   // 弹性调用 (重试/熔断)

	// M6: 持久化 + 历史学习
	pheromoneStore *PheromoneStore
	predHistory    *PredictionHistory
	reasoningBank  *ReasoningBank
	dtiController  *DTIController

	// M7: 在线校准 + 市场 + 路由
	conformal      *ConformalCalibrator
	debateGate     *DebateGate
	byzantineFuser *ByzantineFuser
	banditRouter   *BanditRouter

	// M9: 可观测性
	metrics *MetricsCollector

	maxDebateRounds int
	numAnalysts     int
	numScouts       int
}

// Config 引擎配置。
type Config struct {
	Boids           BoidsConfig
	MaxDebateRounds int
	NumAnalysts     int
	NumScouts       int
	Notify          NotifyFunc
	DataDir         string  // 持久化目录 (默认 ~/.claude-go/swarm_intel)
	DTIThreshold    float64 // DTI 触发阈值 (默认 0.7)
	TrimRatio       float64 // 拜占庭修剪比例 (默认 0.2)
	DebateEntropy   float64 // 辩论门控熵阈值 (默认 0.7)
}

// DefaultConfig 默认配置。
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Boids:           DefaultBoidsConfig(),
		MaxDebateRounds: 2,
		NumAnalysts:     3,
		NumScouts:       1,
		DataDir:         filepath.Join(home, ".claude-go", "swarm_intel"),
		DTIThreshold:    0.7,
		TrimRatio:       0.2,
		DebateEntropy:   0.7,
	}
}

// NewEngine 创建群体智能引擎。
func NewEngine(llm LLMClient, cfg Config) *Engine {
	if cfg.Notify == nil {
		cfg.Notify = func(_, _ string) {}
	}
	boids := NewBoidsCoordinator(cfg.Boids)

	// M6 组件 — 持久化 (容错: 初始化失败不阻塞引擎)
	phStore, _ := NewPheromoneStore(cfg.DataDir)
	predHist, _ := NewPredictionHistory(cfg.DataDir)
	rBank, _ := NewReasoningBank(cfg.DataDir)

	// M7 组件 — 在线校准 + 路由
	bandit := NewBanditRouter()
	for _, id := range []string{"analyst-optimist", "analyst-pessimist", "analyst-neutral", "analyst-contrarian"} {
		bandit.Register(id)
	}

	// M10: 可靠性组件
	cc := DefaultContextCompressor()
	rcCfg := DefaultResilientConfig()
	rcCfg.Notify = cfg.Notify
	rc := NewResilientCaller(llm, rcCfg)

	return &Engine{
		llm:             rc, // 用 ResilientCaller 替代裸 LLMClient
		boids:           boids,
		fuser:           NewFuser(boids),
		pheromones:      NewPheromoneMemory(),
		simulator:       NewSimulator(rc, cfg.Notify), // Simulator 也用 rc
		notify:          cfg.Notify,
		cc:              cc,
		rc:              rc,
		pheromoneStore:  phStore,
		predHistory:     predHist,
		reasoningBank:   rBank,
		dtiController:   NewDTIController(cfg.DTIThreshold),
		conformal:       NewConformalCalibrator(0.05),
		debateGate:      NewDebateGate(cfg.DebateEntropy),
		byzantineFuser:  NewByzantineFuser(cfg.TrimRatio),
		banditRouter:    bandit,
		metrics:         NewMetricsCollector(cfg.DataDir),
		maxDebateRounds: cfg.MaxDebateRounds,
		numAnalysts:     cfg.NumAnalysts,
		numScouts:       cfg.NumScouts,
	}
}

// Predict 执行完整的群体智能预测流水线 (M1-M7, M10 可靠性增强)。
func (e *Engine) Predict(ctx context.Context, chatID, objective string) (*FusedPrediction, error) {
	startTime := time.Now()
	llmCallCount := 0

	// M10: 分级超时预算 (默认 5 分钟总预算)
	totalBudget := 5 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		totalBudget = time.Until(dl)
	}
	tb := PredictBudget(totalBudget)

	e.notify(chatID, "🧠 群体智能引擎启动 (v2.2 — M10 工程可靠性增强)...")

	// M6: 检索历史推理路径 (M10: 截断防膨胀)
	var priorReasoning string
	if e.reasoningBank != nil {
		if entries, err := e.reasoningBank.FindSimilar(objective, 3); err == nil && len(entries) > 0 {
			var parts []string
			for _, entry := range entries {
				reasoning := e.cc.TruncateField(entry.Reasoning, 150)
				parts = append(parts, fmt.Sprintf("- [Brier %.3f] %s → %s", entry.BrierScore, entry.Question, reasoning))
			}
			priorReasoning = strings.Join(parts, "\n")
			e.notify(chatID, fmt.Sprintf("📚 找到 %d 条历史推理路径可复用", len(entries)))
		}
	}

	// Phase 1: Decompose
	phaseStart := time.Now()
	e.notify(chatID, "📋 Phase 1/7: 分解目标...")
	decompCtx, decompCancel := tb.PhaseContext(ctx, "decompose")
	domain, err := e.decompose(decompCtx, objective)
	decompCancel()
	llmCallCount++
	if err != nil {
		return nil, fmt.Errorf("decompose: %w", err)
	}
	e.notify(chatID, fmt.Sprintf("  问题: %s\n  类型: %s\n  结果空间: %v", domain.Question, domain.OutcomeType, domain.Outcomes))

	// M6: 加载历史信素先验
	if e.pheromoneStore != nil {
		if trails, err := e.pheromoneStore.Load(domain.OutcomeType); err == nil && len(trails) > 0 {
			for _, t := range trails {
				e.pheromones.Deposit(t.HypothesisID, t.Hypothesis, t.Strength*0.5, t.Evidence)
			}
			e.notify(chatID, fmt.Sprintf("  📊 加载 %d 条历史信素先验", len(trails)))
		}
	}

	// Phase 2: Scout
	e.notify(chatID, "🔍 Phase 2/7: 信息侦察...")
	scoutCtx, scoutCancel := tb.PhaseContext(ctx, "scout")
	evidence, err := e.scout(scoutCtx, domain)
	scoutCancel()
	if err != nil {
		evidence = []string{"无法获取额外证据，将基于已有知识进行预测"}
	}
	if priorReasoning != "" {
		evidence = append(evidence, "历史推理参考:\n"+priorReasoning)
	}
	e.notify(chatID, fmt.Sprintf("  搜集到 %d 条证据", len(evidence)))

	// Phase 3: Predict (独立预测 — M10: 并行化)
	e.notify(chatID, fmt.Sprintf("🎯 Phase 3/7: %d 个分析师并行预测...", e.numAnalysts))
	predCtx, predCancel := tb.PhaseContext(ctx, "predict")
	predictions, err := e.predict(predCtx, domain, evidence)
	predCancel()
	if err != nil {
		return nil, fmt.Errorf("predict: %w", err)
	}

	// 信素初始化
	for _, p := range predictions {
		for outcome, prob := range p.Predictions {
			if prob > 0.3 {
				e.pheromones.Deposit(outcome, outcome, prob*0.1, p.Evidence)
			}
		}
	}

	// M6 DTI 检查: 检测精英垄断
	if e.dtiController.ShouldTrigger(predictions) {
		e.notify(chatID, "  ⚡ DTI 触发: 检测到少数Agent主导预测, 启动强制交叉检查")
		diversified, err := e.forceDiversity(ctx, domain, evidence, predictions)
		if err == nil {
			predictions = diversified
		}
	}

	// Boids 多样性检查
	similar := e.boids.DiversityCheck(predictions)
	if len(similar) > 0 {
		e.notify(chatID, fmt.Sprintf("  ⚠️ 检测到 %d 对过于相似的预测, 强制差异化", len(similar)))
		diversified, err := e.forceDiversity(ctx, domain, evidence, predictions)
		if err == nil {
			predictions = diversified
		}
	}

	// Phase 4: Debate (M7 门控 + M10 预算控制)
	debateRound := 0
	shouldDebate := e.debateGate.ShouldDebate(predictions)
	if !shouldDebate {
		e.notify(chatID, "⚡ Phase 4/7: 辩论门控 — 预测已高度一致, 跳过辩论 (节省 ~6x token)")
	} else {
		debateCtx, debateCancel := tb.PhaseContext(ctx, "debate")
		for debateRound < e.maxDebateRounds {
			if debateCtx.Err() != nil || tb.Expired() {
				break
			}

			divergence := e.boids.MeasureDivergence(predictions)
			consensus := 1.0 - divergence
			e.notify(chatID, fmt.Sprintf("💬 Phase 4/7: 辩论第 %d 轮 (共识度: %.0f%%, 分歧: %.0f%%)",
				debateRound+1, consensus*100, divergence*100))

			if !e.boids.NeedMoreDebate(predictions) && debateRound > 0 {
				e.notify(chatID, "  ✅ 已达共识阈值, 停止辩论")
				break
			}

			updated, err := e.debateRound(debateCtx, domain, evidence, predictions, debateRound+1)
			if err != nil {
				break
			}
			predictions = updated
			debateRound++

			e.pheromones.Evaporate()
			for _, p := range predictions {
				for outcome, prob := range p.Predictions {
					if prob > 0.3 {
						e.pheromones.Deposit(outcome, outcome, prob*0.05, nil)
					}
				}
			}
		}
		debateCancel()
	}

	// Phase 5: Fuse (标准融合)
	e.notify(chatID, "🔮 Phase 5/7: 贝叶斯融合...")
	result := e.fuser.Fuse(*domain, predictions)

	// Phase 6: Byzantine Trim (M7 拜占庭容错)
	e.notify(chatID, "🛡️ Phase 6/7: 拜占庭容错校验...")
	trimmedDist := e.byzantineFuser.TrimmedFuse(predictions)
	for i, o := range result.Outcomes {
		if tp, ok := trimmedDist[o.Outcome]; ok {
			blended := 0.7*o.Probability + 0.3*tp
			result.Outcomes[i].Probability = blended
		}
	}

	// Phase 7: Conformal Calibration (M7 保形校准)
	e.notify(chatID, "📐 Phase 7/7: 保形校准...")
	confSet := e.conformal.CalibrateSet(result)
	for i, co := range confSet.Outcomes {
		if i < len(result.Outcomes) {
			result.Outcomes[i].Lower95 = co.Lower95
			result.Outcomes[i].Upper95 = co.Upper95
		}
	}

	result.PheromoneState = e.pheromones.Snapshot()

	sumCtx, sumCancel := tb.PhaseContext(ctx, "summary")
	summary, err := e.generateSummary(sumCtx, domain, result)
	sumCancel()
	if err == nil {
		result.Summary = summary
	}

	// M6: 持久化信素和预测记录
	e.persistResults(domain, result, predictions)

	// M7: 更新 Bandit 路由 (根据预测质量)
	for _, p := range predictions {
		isGood := p.Confidence > 0.5
		e.banditRouter.Update(p.AgentID, isGood)
	}

	// M9: 记录可观测性指标
	if e.metrics != nil {
		rm := RunMetrics{
			RunID:              fmt.Sprintf("pred-%d", startTime.UnixMilli()),
			Type:               "predict",
			Question:           result.Question,
			StartTime:          startTime,
			EndTime:            time.Now(),
			TotalLatencyMs:     time.Since(startTime).Milliseconds(),
			LLMCallCount:       llmCallCount + e.numAnalysts + e.numScouts + debateRound*len(predictions),
			Consensus:          result.Consensus,
			BrierScore:         result.BrierScore,
			Divergence:         e.boids.MeasureDivergence(predictions),
			DebateRounds:       debateRound,
			DebateSkipped:      !shouldDebate,
			DTITriggered:       e.dtiController.ShouldTrigger(predictions),
			AgentCount:         len(predictions),
			UniqueHypotheses:   len(result.Outcomes),
			SimilarPairs:       len(similar),
			DiversityScore:     ComputeDiversityScore(predictions),
			PheromoneTrails:    len(result.PheromoneState),
			AvgConfidence:      avgConfidence(predictions),
			ConfidenceCI:       confSet.Width,
			PhaseTiming: map[string]int64{
				"decompose": time.Since(phaseStart).Milliseconds(),
			},
		}
		e.metrics.Record(rm)
	}

	e.notify(chatID, e.formatResult(result))
	return result, nil
}

func avgConfidence(predictions []AgentPrediction) float64 {
	if len(predictions) == 0 {
		return 0
	}
	total := 0.0
	for _, p := range predictions {
		total += p.Confidence
	}
	return total / float64(len(predictions))
}

// GetMetrics 获取指标摘要。
func (e *Engine) GetMetrics() *MetricsSummary {
	if e.metrics == nil {
		return &MetricsSummary{}
	}
	return e.metrics.Summary()
}

// GetComparisonReport 获取与 MiroFish 的对比报告。
func (e *Engine) GetComparisonReport() *ComparisonReport {
	return GenerateComparisonReport(e.GetMetrics())
}

// persistResults M6 持久化预测结果、信素和推理路径。
func (e *Engine) persistResults(domain *PredictionDomain, result *FusedPrediction, predictions []AgentPrediction) {
	if e.pheromoneStore != nil {
		trails := e.pheromones.TopTrails(20)
		e.pheromoneStore.Save(domain.OutcomeType, trails)
	}

	if e.predHistory != nil {
		rec := &PredictionRecord{
			ID:        fmt.Sprintf("pred-%d", time.Now().UnixMilli()),
			Question:  result.Question,
			Outcomes:  result.Outcomes,
			Consensus: result.Consensus,
			BrierScore: result.BrierScore,
			Rounds:    result.Rounds,
			CreatedAt: time.Now(),
		}
		e.predHistory.Append(rec)
	}

	if e.reasoningBank != nil {
		for _, p := range predictions {
			if p.Confidence > 0.6 && p.Rationale != "" {
				e.reasoningBank.Store(&ReasoningEntry{
					Question:   result.Question,
					Reasoning:  p.Rationale,
					BrierScore: result.BrierScore,
					Confidence: p.Confidence,
					CreatedAt:  time.Now(),
				})
			}
		}
	}
}

// GetHistory 获取最近 N 条预测历史。
func (e *Engine) GetHistory(n int) ([]*PredictionRecord, error) {
	if e.predHistory == nil {
		return nil, fmt.Errorf("prediction history not initialized")
	}
	return e.predHistory.Recent(n)
}

// Simulate 执行场景模拟。
func (e *Engine) Simulate(ctx context.Context, chatID, objective string, cfg SimulationConfig) (*SimulationResult, error) {
	return e.simulator.Simulate(ctx, chatID, objective, cfg)
}

// decompose 将自由文本目标分解为结构化预测域。
func (e *Engine) decompose(ctx context.Context, objective string) (*PredictionDomain, error) {
	prompt := fmt.Sprintf(`将以下目标分解为结构化预测问题。

目标: %s

输出严格JSON (不要其他文字):
{
  "question": "精确的预测问题",
  "horizon": "short/medium/long",
  "outcome_type": "binary/categorical/numeric/scenario",
  "outcomes": ["结果A", "结果B", "结果C"],
  "context": "相关背景",
  "constraints": ["约束1"]
}

规则:
- binary: 只有2个outcomes (是/否)
- categorical: 3-6个离散结果
- scenario: 3-5个场景
- 每个outcome要互斥且穷尽`, objective)

	resp, err := e.llm.SimpleComplete(ctx,
		"你是预测问题设计专家。将模糊目标转化为精确、可预测的结构化问题。", prompt)
	if err != nil {
		return nil, err
	}

	jsonStr := extractJSON(resp)
	var domain PredictionDomain
	if err := json.Unmarshal([]byte(jsonStr), &domain); err != nil {
		return &PredictionDomain{
			Question:    objective,
			Horizon:     "medium",
			OutcomeType: "scenario",
			Outcomes:    []string{"乐观情景", "基准情景", "悲观情景"},
			Context:     objective,
		}, nil
	}
	if len(domain.Outcomes) == 0 {
		domain.Outcomes = []string{"乐观情景", "基准情景", "悲观情景"}
	}
	return &domain, nil
}

// scout 信息侦察阶段 (M10: 健壮 JSON 解析)。
func (e *Engine) scout(ctx context.Context, domain *PredictionDomain) ([]string, error) {
	var allEvidence []string

	// M10: 上下文压缩 — 背景截断
	compressedCtx := e.cc.TruncateField(domain.Context, 300)

	for i := 0; i < e.numScouts; i++ {
		if ctx.Err() != nil {
			break
		}

		perspective := "通用视角"
		if i == 1 {
			perspective = "反面视角 (寻找反例和风险)"
		}

		prompt := fmt.Sprintf(`作为信息侦察员 (%s), 针对以下预测问题搜集关键证据。

问题: %s
背景: %s
可能结果: %v

输出严格JSON (不要markdown代码块):
{"evidence": ["证据1", "证据2", "证据3"]}`, perspective, domain.Question, compressedCtx, domain.Outcomes)

		resp, err := e.llm.SimpleComplete(ctx, "你是信息分析师。只输出JSON。", prompt)
		if err != nil {
			continue
		}

		// M10: 健壮 JSON 解析
		type evidenceResp struct {
			Evidence []string `json:"evidence"`
		}
		result := ParseJSON[evidenceResp](resp)
		if result.OK {
			allEvidence = append(allEvidence, result.Value.Evidence...)
		}
	}

	return allEvidence, nil
}

// predict 多 Agent 独立预测 (M10: 并行化 + 上下文压缩)。
func (e *Engine) predict(ctx context.Context, domain *PredictionDomain, evidence []string) ([]AgentPrediction, error) {
	perspectives := []struct {
		id   string
		role string
		bias string
	}{
		{"analyst-optimist", "乐观分析师", "倾向于看到积极信号和机遇"},
		{"analyst-pessimist", "悲观分析师", "倾向于识别风险和负面因素"},
		{"analyst-neutral", "中立分析师", "完全基于证据的客观分析"},
		{"analyst-contrarian", "黑天鹅猎手", "专注于低概率高影响事件"},
	}
	if e.numAnalysts < len(perspectives) {
		perspectives = perspectives[:e.numAnalysts]
	}

	// M10: 上下文压缩 — 证据去重+截断
	evidenceStr := e.cc.CompressEvidence(evidence, 1500)
	outcomesJSON, _ := json.Marshal(domain.Outcomes)

	// M10: 并行化 — 各分析师独立, 可安全并行
	branches := make(map[string]BranchFunc)
	for _, p := range perspectives {
		p := p
		branches[p.id] = func(branchCtx context.Context) (string, error) {
			prompt := fmt.Sprintf(`你是 %s。你的分析偏向: %s

预测问题: %s
可能结果: %s
已知证据:
%s

请独立给出你的概率预测。输出严格JSON (不要markdown代码块):
{
  "predictions": {%s},
  "confidence": 0.7,
  "rationale": "100字以内推理过程",
  "evidence": ["你重点依据的证据"]
}

规则:
- predictions中每个outcome对应一个0-1的概率
- 所有概率之和必须等于1.0
- confidence是你对自己预测的置信度(0-1)`, p.role, p.bias, domain.Question, string(outcomesJSON), evidenceStr,
				buildOutcomeTemplate(domain.Outcomes))

			return e.llm.SimpleComplete(branchCtx,
				fmt.Sprintf("你是%s。给出独立概率预测。只输出JSON。", p.role), prompt)
		}
	}

	cfg := DefaultFanOutConfig()
	cfg.MaxConcurrency = 3
	results := FanOutCollect(ctx, cfg, branches)

	var predictions []AgentPrediction
	roleMap := make(map[string]string)
	for _, p := range perspectives {
		roleMap[p.id] = p.role
	}
	for _, r := range results {
		if r.Error != nil {
			continue
		}
		role := roleMap[r.ID]
		pred := parseAgentPrediction(r.Value, r.ID, role, domain.Outcomes)
		pred.Round = 1
		predictions = append(predictions, pred)
	}

	if len(predictions) == 0 {
		return nil, fmt.Errorf("所有分析师均未返回有效预测")
	}
	return predictions, nil
}

// forceDiversity Boids Separation: 强制过于相似的 Agent 差异化。
func (e *Engine) forceDiversity(ctx context.Context, domain *PredictionDomain, evidence []string, predictions []AgentPrediction) ([]AgentPrediction, error) {
	// M10: 上下文压缩
	compressedEvidence := e.cc.CompressEvidence(evidence, 500)
	diversePrompt := fmt.Sprintf(`分析师预测过于一致, 请从完全不同的角度重新分析。

问题: %s
当前共识: %v
证据: %s

要求: 寻找被忽略的因素、黑天鹅事件。
输出严格JSON (不要markdown代码块):
{
  "predictions": {%s},
  "confidence": 0.5,
  "rationale": "80字以内差异化推理",
  "evidence": ["反常证据"]
}`, domain.Question, predictions[0].Predictions,
		compressedEvidence,
		buildOutcomeTemplate(domain.Outcomes))

	resp, err := e.llm.SimpleComplete(ctx, "你是反共识分析师。专门寻找主流预测忽视的可能性。", diversePrompt)
	if err != nil {
		return predictions, nil
	}

	newPred := parseAgentPrediction(resp, "analyst-divergent", "反共识分析师", domain.Outcomes)
	newPred.Round = 1

	result := make([]AgentPrediction, len(predictions))
	copy(result, predictions)
	if len(result) > 0 {
		result[len(result)-1] = newPred
	} else {
		result = append(result, newPred)
	}
	return result, nil
}

// debateRound 单轮辩论 (M10: 上下文压缩 + 并行化)。
func (e *Engine) debateRound(ctx context.Context, domain *PredictionDomain, evidence []string, predictions []AgentPrediction, round int) ([]AgentPrediction, error) {
	// M10: 上下文压缩 — 辩论视图只保留概率+摘要推理
	compressedViews := e.cc.CompressDebateView(predictions)

	topTrails := e.pheromones.TopTrails(3)
	var trailInfo string
	if len(topTrails) > 0 {
		var parts []string
		for _, t := range topTrails {
			parts = append(parts, fmt.Sprintf("%s(%.2f)", t.Hypothesis, t.Strength))
		}
		trailInfo = "\n信素: " + strings.Join(parts, ", ")
	}

	// M10: 并行化 — 各 agent 更新可并行
	branches := make(map[string]BranchFunc)
	for _, p := range predictions {
		p := p
		branches[p.AgentID] = func(branchCtx context.Context) (string, error) {
			// M10: 自身推理也截断
			myRationale := e.cc.TruncateField(p.Rationale, 100)
			prompt := fmt.Sprintf(`辩论第 %d 轮。你是 %s。

问题: %s
你的上一轮预测: %v (置信度: %.0f%%)
你的推理: %s

其他分析师:
%s%s

更新你的预测。输出严格JSON (不要markdown代码块):
{
  "predictions": {%s},
  "confidence": 0.7,
  "rationale": "80字以内更新推理",
  "evidence": ["新因素"]
}`, round, p.AgentRole, domain.Question,
				p.Predictions, p.Confidence*100, myRationale,
				compressedViews, trailInfo,
				buildOutcomeTemplate(domain.Outcomes))

			return e.llm.SimpleComplete(branchCtx,
				fmt.Sprintf("你是%s。更新预测,只输出JSON。", p.AgentRole), prompt)
		}
	}

	cfg := DefaultFanOutConfig()
	cfg.MaxConcurrency = 3
	results := FanOutCollect(ctx, cfg, branches)

	// 重组结果, 失败的保留原预测
	predMap := make(map[string]AgentPrediction)
	for _, p := range predictions {
		predMap[p.AgentID] = p
	}

	var updated []AgentPrediction
	for _, r := range results {
		orig := predMap[r.ID]
		if r.Error != nil {
			updated = append(updated, orig)
			continue
		}
		newPred := parseAgentPrediction(r.Value, orig.AgentID, orig.AgentRole, domain.Outcomes)
		newPred.Round = round
		updated = append(updated, newPred)
	}

	return updated, nil
}

// generateSummary 生成可读的预测总结。
func (e *Engine) generateSummary(ctx context.Context, domain *PredictionDomain, result *FusedPrediction) (string, error) {
	var outcomeSummary strings.Builder
	for _, o := range result.Outcomes {
		outcomeSummary.WriteString(fmt.Sprintf("- %s: %.1f%% [%.1f%% ~ %.1f%%]\n",
			o.Outcome, o.Probability*100, o.Lower95*100, o.Upper95*100))
	}

	prompt := fmt.Sprintf(`基于群体智能预测结果, 生成简洁的分析总结。

问题: %s
融合结果:
%s
共识度: %.0f%%
辩论轮数: %d
参与分析师: %d

请用300字以内总结:
1. 最可能的结果及原因
2. 关键的不确定因素
3. 需要关注的风险信号`, domain.Question, outcomeSummary.String(),
		result.Consensus*100, result.Rounds, len(result.Agents))

	return e.llm.SimpleComplete(ctx, "你是预测分析师。输出简洁、有洞察的总结。", prompt)
}

// formatResult 格式化预测结果为终端友好的输出。
func (e *Engine) formatResult(result *FusedPrediction) string {
	var sb strings.Builder
	sb.WriteString("\n╔══════════════════════════════════════════════╗\n")
	sb.WriteString("║        🧠 群体智能预测结果                    ║\n")
	sb.WriteString("╠══════════════════════════════════════════════╣\n")
	sb.WriteString(fmt.Sprintf("║ 问题: %s\n", result.Question))
	sb.WriteString("╠──────────────────────────────────────────────╣\n")

	for _, o := range result.Outcomes {
		barLen := int(o.Probability * 30)
		bar := strings.Repeat("█", barLen) + strings.Repeat("░", 30-barLen)
		sb.WriteString(fmt.Sprintf("║ %-12s %s %.1f%%\n", o.Outcome, bar, o.Probability*100))
		sb.WriteString(fmt.Sprintf("║              95%% CI: [%.1f%% ~ %.1f%%]\n", o.Lower95*100, o.Upper95*100))
	}

	sb.WriteString("╠──────────────────────────────────────────────╣\n")
	sb.WriteString(fmt.Sprintf("║ 共识度: %.0f%%  辩论轮数: %d  分析师: %d\n",
		result.Consensus*100, result.Rounds, len(result.Agents)))
	sb.WriteString(fmt.Sprintf("║ 融合方法: %s\n", result.Method))

	if result.Summary != "" {
		sb.WriteString("╠──────────────────────────────────────────────╣\n")
		lines := strings.Split(result.Summary, "\n")
		for _, line := range lines {
			sb.WriteString(fmt.Sprintf("║ %s\n", line))
		}
	}

	sb.WriteString("╚══════════════════════════════════════════════╝\n")
	return sb.String()
}

// --- Helpers ---

func extractJSON(s string) string {
	// 先去掉 markdown 代码块标记
	cleaned := s
	cleaned = strings.ReplaceAll(cleaned, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```JSON", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")
	cleaned = strings.TrimSpace(cleaned)

	start := strings.Index(cleaned, "{")
	if start < 0 {
		return "{}"
	}
	depth := 0
	for i := start; i < len(cleaned); i++ {
		switch cleaned[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return cleaned[start : i+1]
			}
		}
	}
	return cleaned[start:]
}

// extractAllJSON 从文本中提取所有顶层 JSON 对象。
func extractAllJSON(s string) []string {
	cleaned := s
	cleaned = strings.ReplaceAll(cleaned, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```JSON", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")
	cleaned = strings.TrimSpace(cleaned)

	var results []string
	pos := 0
	for pos < len(cleaned) {
		start := strings.Index(cleaned[pos:], "{")
		if start < 0 {
			break
		}
		start += pos
		depth := 0
		for i := start; i < len(cleaned); i++ {
			switch cleaned[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					results = append(results, cleaned[start:i+1])
					pos = i + 1
					goto next
				}
			}
		}
		break
	next:
	}
	return results
}

func buildOutcomeTemplate(outcomes []string) string {
	var parts []string
	for _, o := range outcomes {
		parts = append(parts, fmt.Sprintf(`"%s": 0.0`, o))
	}
	return strings.Join(parts, ", ")
}

func parseAgentPrediction(raw, agentID, agentRole string, outcomes []string) AgentPrediction {
	jsonStr := extractJSON(raw)

	var parsed struct {
		Predictions map[string]float64 `json:"predictions"`
		Confidence  float64            `json:"confidence"`
		Rationale   string             `json:"rationale"`
		Evidence    []string           `json:"evidence"`
	}

	pred := AgentPrediction{
		AgentID:     agentID,
		AgentRole:   agentRole,
		Predictions: make(map[string]float64),
		Confidence:  0.5,
	}

	if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil {
		pred.Predictions = parsed.Predictions
		pred.Confidence = parsed.Confidence
		pred.Rationale = parsed.Rationale
		pred.Evidence = parsed.Evidence
	}

	// 确保所有 outcome 都有概率, 并归一化
	var total float64
	for _, o := range outcomes {
		if _, ok := pred.Predictions[o]; !ok {
			pred.Predictions[o] = 1.0 / float64(len(outcomes))
		}
		total += pred.Predictions[o]
	}
	if total > 0 {
		for o := range pred.Predictions {
			pred.Predictions[o] /= total
		}
	}

	if pred.Confidence <= 0 || pred.Confidence > 1 {
		pred.Confidence = 0.5
	}

	return pred
}
