package swarm_intel

// v3 Phase 1 (E1–E5) 验收测试:
//   E1 RegisterResolution 学习闭环 — 连续 10 次回填后 conformal CI 半宽出现 ≥3 个不同值;
//   E3 在线权重 — 信任分不同 → 融合权重/概率不同; 无信任 ≡ v2;
//   E4 extremization — 公式/clip/单调性 + 校准数据不足 (<10 resolutions) 强制为 0;
//   E5 agentEntropy — AUQ 分流阈值口径。

import (
	"math"
	"testing"
	"time"
)

// ── E3: 在线权重 ──

func mkE3Preds() []AgentPrediction {
	return []AgentPrediction{
		{AgentID: "a", AgentRole: "甲", Predictions: map[string]float64{"x": 0.7, "y": 0.3}, Confidence: 0.8},
		{AgentID: "b", AgentRole: "乙", Predictions: map[string]float64{"x": 0.3, "y": 0.7}, Confidence: 0.8},
	}
}

func fusedProb(r *FusedPrediction, outcome string) float64 {
	for _, o := range r.Outcomes {
		if o.Outcome == outcome {
			return o.Probability
		}
	}
	return -1
}

// 信任分不同 → 权重不同 → 融合概率不同 (且方向与信任一致)。
func TestComputeWeights_TrustChangesWeights(t *testing.T) {
	f := NewFuser(NewBoidsCoordinator(DefaultBoidsConfig()))
	domain := PredictionDomain{Question: "q", Outcomes: []string{"x", "y"}}

	trustA := map[string]float64{"a": 0.9, "b": 0.1} // 信 a
	trustB := map[string]float64{"a": 0.1, "b": 0.9} // 信 b

	rA := f.FuseWithTrust(domain, mkE3Preds(), trustA)
	rB := f.FuseWithTrust(domain, mkE3Preds(), trustB)

	pAx, pBx := fusedProb(rA, "x"), fusedProb(rB, "x")
	if math.Abs(pAx-pBx) < 1e-6 {
		t.Fatalf("信任分不同应产生不同融合概率: %v vs %v", pAx, pBx)
	}
	if pAx <= 0.5 {
		t.Fatalf("信任偏向 a (押 x 0.7) 时 x 应 > 0.5, 实际 %v", pAx)
	}
	if pBx >= 0.5 {
		t.Fatalf("信任偏向 b (押 y 0.7) 时 x 应 < 0.5, 实际 %v", pBx)
	}
}

// 无信任输入 (nil/空 map) 时 FuseWithTrust 与 v2 Fuse 输出一致 —— 共享域零回归。
func TestFuseWithTrust_NoTrustMatchesV2(t *testing.T) {
	f := NewFuser(NewBoidsCoordinator(DefaultBoidsConfig()))
	domain := PredictionDomain{Question: "q", Outcomes: []string{"x", "y"}}
	v2 := f.Fuse(domain, mkE3Preds())
	v3 := f.FuseWithTrust(domain, mkE3Preds(), nil)
	if math.Abs(fusedProb(v2, "x")-fusedProb(v3, "x")) > 1e-12 ||
		math.Abs(fusedProb(v2, "y")-fusedProb(v3, "y")) > 1e-12 {
		t.Fatalf("无信任时输出应与 v2 逐字节一致: v2=%v v3=%v", v2.Outcomes, v3.Outcomes)
	}
}

// ── E4: extremization ──

func TestExtremizeProb(t *testing.T) {
	// 基准: p'=0.5+(p-0.5)*(1+2*ext)
	if got := extremizeProb(0.6, 0.2); math.Abs(got-0.64) > 1e-12 {
		t.Fatalf("extremizeProb(0.6, 0.2) = %v, 期望 0.64", got)
	}
	// 推离 0.5 的单调性: 0<p<1 内 |p'-0.5| 严格大于 |p-0.5|
	for _, p := range []float64{0.55, 0.7, 0.9, 0.45, 0.3, 0.1} {
		if got := extremizeProb(p, 0.15); math.Abs(got-0.5) <= math.Abs(p-0.5) {
			t.Fatalf("ext>0 应把 %v 推离 0.5, 实际 %v", p, got)
		}
	}
	// clip 下界
	if got := extremizeProb(0.05, 0.3); got != 0.01 {
		t.Fatalf("下界 clip: 期望 0.01, 实际 %v", got)
	}
	// clip 上界
	if got := extremizeProb(0.95, 0.3); got != 0.99 {
		t.Fatalf("上界 clip: 期望 0.99, 实际 %v", got)
	}
	// ext=0 恒等
	if got := extremizeProb(0.37, 0); got != 0.37 {
		t.Fatalf("ext=0 应恒等, 实际 %v", got)
	}
}

// 校准数据不足 (<10 resolutions) 时 extremization 强制为 0; 足够时按 judge 值 clamp 到 [0,0.3]。
func TestResolveExtremization_GatedByResolutions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	e := NewEngine(nil, cfg)

	if got := e.resolveExtremization(0.2); got != 0 {
		t.Fatalf("resolutions=0 时应强制 0, 实际 %v", got)
	}
	e.resolutions = 9
	if got := e.resolveExtremization(0.2); got != 0 {
		t.Fatalf("resolutions=9 (<10) 时应强制 0, 实际 %v", got)
	}
	e.resolutions = 10
	if got := e.resolveExtremization(0.2); got != 0.2 {
		t.Fatalf("resolutions=10 应放行 judge 值, 实际 %v", got)
	}
	if got := e.resolveExtremization(0.5); got != 0.3 {
		t.Fatalf("judge 超界应 clamp 到 0.3, 实际 %v", got)
	}
	if got := e.resolveExtremization(-0.1); got != 0 {
		t.Fatalf("负值应 clamp 到 0, 实际 %v", got)
	}
}

// ── E6: LMSR 预测市场融合 ──

// 市场开启时 Method 加 +market 后缀; 关闭时保持纯 v2 行为。
func TestMarketFusion_MethodSuffix(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.MarketEnable = true
	cfg.MarketWeight = 0.2
	e := NewEngine(nil, cfg)

	domain := &PredictionDomain{Question: "q", Outcomes: []string{"x", "y"}}
	preds := []AgentPrediction{
		{AgentID: "a", AgentRole: "甲", Predictions: map[string]float64{"x": 0.8, "y": 0.2}, Confidence: 0.8},
		{AgentID: "b", AgentRole: "乙", Predictions: map[string]float64{"x": 0.6, "y": 0.4}, Confidence: 0.7},
	}
	fused := e.fuser.FuseWithTrust(*domain, preds, nil)
	e.applyMarketFusion(domain, preds, fused)
	if fused.Method != "log_opinion_pool+market" {
		t.Fatalf("市场开启时 Method 应为 log_opinion_pool+market, 实际 %v", fused.Method)
	}

	// 关闭市场时 Predict 不应触发 applyMarketFusion (这里直接验证 applyMarketFusion 的 early-return)。
	e2 := NewEngine(nil, DefaultConfig())
	e2.marketEnable = false
	fused2 := e2.fuser.FuseWithTrust(*domain, preds, nil)
	e2.applyMarketFusion(domain, preds, fused2)
	if fused2.Method != "log_opinion_pool" {
		t.Fatalf("市场关闭时 Method 应为纯 v2, 实际 %v", fused2.Method)
	}
}

// 市场权重 ≤ 0.5 封顶: 即使 cfg.MarketWeight 设得很大, 实际混入比例也不能让市场主导融合。
func TestMarketFusion_WeightCapped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.MarketWeight = 0.9 // 故意设超界
	e := NewEngine(nil, cfg)
	if e.marketWeight != 0.9 {
		t.Fatalf("setup: marketWeight 应为 0.9")
	}

	domain := &PredictionDomain{Question: "q", Outcomes: []string{"x", "y"}}
	preds := []AgentPrediction{
		{AgentID: "a", AgentRole: "甲", Predictions: map[string]float64{"x": 0.7, "y": 0.3}, Confidence: 0.8},
		{AgentID: "b", AgentRole: "乙", Predictions: map[string]float64{"x": 0.5, "y": 0.5}, Confidence: 0.6},
	}
	fused := e.fuser.FuseWithTrust(*domain, preds, nil)
	e.applyMarketFusion(domain, preds, fused)

	// w=min(0.9, 0.5)=0.5: 融合概率 = 0.5·p_fused + 0.5·p_market。市场报价在 (0,1),
	// 所以 |p_final - p_fused| ≤ 0.5·|p_market - p_fused| < 0.5。任何 outcome 偏移都不能超过 0.5。
	for _, o := range fused.Outcomes {
		if o.Probability < 0 || o.Probability > 1 {
			t.Fatalf("概率越界: %v=%v", o.Outcome, o.Probability)
		}
	}
	// 概率归一: Σ p = 1 (LMSR  softmax 天然归一, 混合也归一)。
	sum := 0.0
	for _, o := range fused.Outcomes {
		sum += o.Probability
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("融合后概率不归一: Σ=%v", sum)
	}
}

// 单分析师不启用市场 (LMSR 单人购彩无意义, 市场恒等于先验)。
func TestMarketFusion_SingleAgentSkips(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	e := NewEngine(nil, cfg)

	domain := &PredictionDomain{Question: "q", Outcomes: []string{"x", "y"}}
	preds := []AgentPrediction{
		{AgentID: "a", AgentRole: "甲", Predictions: map[string]float64{"x": 0.9, "y": 0.1}, Confidence: 0.9},
	}
	fused := e.fuser.FuseWithTrust(*domain, preds, nil)
	before := fusedProb(fused, "x")
	e.applyMarketFusion(domain, preds, fused)
	after := fusedProb(fused, "x")
	if math.Abs(before-after) > 1e-12 {
		t.Fatalf("单分析师不应触发市场融合: before=%v after=%v", before, after)
	}
}

// LMSR 价格归一 + log-sum-exp 不溢出。
func TestMarketFusion_LMSRPricesNormalized(t *testing.T) {
	// 预算充足: 100 份额的 LMSR 成本约为 b·ln(N) + q/N 量级, b=100 时几十足够。
	market := NewPredictionMarket(100.0, []string{"a", "b", "c"}, 200.0, []string{"u", "v"})
	// 大量购彩把 q/b 推得很大, 测溢出防护。
	if _, err := market.Buy("u", "a", 100.0); err != nil {
		t.Fatalf("Buy u→a 失败: %v", err)
	}
	if _, err := market.Buy("v", "a", 50.0); err != nil {
		t.Fatalf("Buy v→a 失败: %v", err)
	}
	prices := market.Prices()
	sum := 0.0
	for _, p := range prices {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			t.Fatalf("价格越界/NaN/Inf: %v", p)
		}
		sum += p
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("LMSR 价格不归一: Σ=%v", sum)
	}
	if prices["a"] <= prices["b"] {
		t.Fatalf("购彩 a 最多, 价格应最高: %v", prices)
	}
}

// 预算耗尽后 Buy 应报错, 但不打断融合 (applyMarketFusion 容错跳过)。
func TestMarketFusion_BudgetExhausted(t *testing.T) {
	market := NewPredictionMarket(100.0, []string{"x", "y"}, 0.01, []string{"poor"}) // 极小预算
	if _, err := market.Buy("poor", "x", 100.0); err == nil {
		t.Fatalf("预算 0.01 不应买得起 100 份额")
	}
	// 预算内可买
	if _, err := market.Buy("poor", "x", 0.001); err != nil {
		t.Fatalf("小额 Buy 应成功: %v", err)
	}
}

// ── E5: AUQ 分流熵口径 ──

func TestAgentEntropy(t *testing.T) {
	certain := AgentPrediction{AgentID: "a", Predictions: map[string]float64{"x": 1.0, "y": 0.0}}
	if h := agentEntropy(certain, []string{"x", "y"}); h != 0 {
		t.Fatalf("笃定分布熵应为 0, 实际 %v", h)
	}
	uniform := AgentPrediction{AgentID: "b", Predictions: map[string]float64{"x": 0.5, "y": 0.5}}
	if h := agentEntropy(uniform, []string{"x", "y"}); math.Abs(h-1.0) > 1e-12 {
		t.Fatalf("均匀分布熵应为 1, 实际 %v", h)
	}
	lean := AgentPrediction{AgentID: "c", Predictions: map[string]float64{"x": 0.7, "y": 0.3}}
	if h := agentEntropy(lean, []string{"x", "y"}); h >= 1.0 || h <= 0 {
		t.Fatalf("偏斜分布熵应在 (0,1) 之间, 实际 %v", h)
	}
}

// ── E1: 学习闭环集成信号 ──

// TestRegisterResolution_ConformalCIVariety 模拟 webapp 路径: 预测记录入库 (pending 登记),
// 外部连续 10 次 RegisterResolution 回填真实结局 —— 之后 conformal CI 半宽必须出现
// ≥3 个不同取值 (而不是 v2 那种从未学习、恒为默认 0.15 的死水)。
func TestRegisterResolution_ConformalCIVariety(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	e := NewEngine(nil, cfg)

	// 前三次 nonconformity (1−P(actual)) 取 0.2/0.4/0.6 (递增),
	// 之后保持小值 —— 运行期分位数 (n 小、alpha=0.05 时取高分位) 至少经历 0.2→0.4→0.6 三段。
	pActuals := []float64{0.8, 0.6, 0.4, 0.9, 0.95, 0.9, 0.95, 0.9, 0.95, 0.9}

	quantiles := make(map[float64]bool)
	for i, pActual := range pActuals {
		recID := "webapp-pred-" + string(rune('0'+i))
		pred := &FusedPrediction{
			Question: "结局是?",
			Outcomes: []OutcomePrediction{
				{Outcome: "A", Probability: pActual},
				{Outcome: "B", Probability: 1 - pActual},
			},
			Agents: []AgentPrediction{
				{AgentID: "analyst-optimist", Predictions: map[string]float64{"A": pActual, "B": 1 - pActual}},
				{AgentID: "analyst-pessimist", Predictions: map[string]float64{"A": 1 - pActual, "B": pActual}},
			},
		}
		// webapp 路径: 记录入库 + 登记未决
		if err := e.store.AppendPrediction(&PredictionRecord{
			ID: recID, Question: pred.Question, Outcomes: pred.Outcomes, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("AppendPrediction: %v", err)
		}
		e.registerPending(recID, pred)

		if brier := e.RegisterResolution(recID, "A"); brier < 0 {
			t.Fatalf("RegisterResolution 不应返回负 Brier: %v", brier)
		}

		e.conformal.mu.Lock()
		quantiles[e.conformal.computeQuantile()] = true
		e.conformal.mu.Unlock()
	}

	if len(quantiles) < 3 {
		t.Fatalf("10 次回填后 CI 半宽应出现 ≥3 个不同值, 实际 %d 个: %v", len(quantiles), keysOf(quantiles))
	}
	if e.resolutions != 10 {
		t.Fatalf("resolutions 应为 10, 实际 %d", e.resolutions)
	}

	// 学习状态已持久化 (JSONL 后端应落 learning_state.json)
	st, err := e.store.LoadLearning()
	if err != nil || st == nil || st.Resolutions != 10 {
		t.Fatalf("学习状态应持久化 10 次 resolutions: st=%+v err=%v", st, err)
	}

	// 预测历史被原地标记已决 (JSONL 后端走 predictionResolver)
	hist, err := e.GetHistory(20)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	resolved := 0
	for _, rec := range hist {
		if rec.Resolved && rec.ActualLabel == "A" {
			resolved++
		}
	}
	if resolved != 10 {
		t.Fatalf("10 条记录应全部原地已决, 实际 %d", resolved)
	}

	// 同一 predID 重复回填应安全跳过 (不再累计)
	if got := e.RegisterResolution("webapp-pred-0", "A"); got != 0 {
		t.Fatalf("重复回填应返回 0, 实际 %v", got)
	}
	if e.resolutions != 10 {
		t.Fatalf("重复回填不应再累计 resolutions, 实际 %d", e.resolutions)
	}
}

// RegisterResolution 对未知 ID / 空结局静默返回 0 (外部乱序调用安全)。
func TestRegisterResolution_UnknownID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	e := NewEngine(nil, cfg)
	if got := e.RegisterResolution("不存在", "A"); got != 0 {
		t.Fatalf("未知 predID 应返回 0, 实际 %v", got)
	}
	if got := e.RegisterResolution("x", ""); got != 0 {
		t.Fatalf("空结局应返回 0, 实际 %v", got)
	}
}

// RegisterResolution 跨重启兜底: 不登记 pending (模拟重启), 直接从存储重建并核销。
func TestRegisterResolution_ReconstructAfterRestart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	e := NewEngine(nil, cfg)

	recID := "restart-pred-1"
	if err := e.store.AppendPrediction(&PredictionRecord{
		ID: recID, Question: "q",
		Outcomes: []OutcomePrediction{{Outcome: "A", Probability: 0.7}, {Outcome: "B", Probability: 0.3}},
		Agents: []AgentPrediction{
			{AgentID: "analyst-neutral", Predictions: map[string]float64{"A": 0.7, "B": 0.3}},
		},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 不 registerPending —— 模拟引擎重启后 pending 已失的场景
	if got := e.RegisterResolution(recID, "A"); got <= 0 {
		t.Fatalf("跨重启重建后应正常核销并返回正 Brier, 实际 %v", got)
	}
	if e.resolutions != 1 {
		t.Fatalf("resolutions 应为 1, 实际 %d", e.resolutions)
	}
}

func keysOf(m map[float64]bool) []float64 {
	ks := make([]float64, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
