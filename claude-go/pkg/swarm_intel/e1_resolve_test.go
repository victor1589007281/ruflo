package swarm_intel

// E1 学习闭环对外核销入口的验收:
// ① persistResults 把落库 ID 盖回 FusedPrediction —— 外部 (webapp/aiops)
//   拿到的结果 JSON 带 id 字段, 才能按 ID 调 RegisterResolution 回填。
// ② 跨"进程"核销: 新引擎实例 (pending 已失) 从存储 reconstructPending
//   找回未决记录, 核销成功并写学习状态 (CLI `swarm resolve` 走的就是这条路)。

import "testing"

func TestPersistResults_StampsPredictionID(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir // 隔离: 绝不读写共享的 ~/.claude-go/swarm_intel
	e := NewEngine(nil, cfg)
	result := &FusedPrediction{
		Question: "索引重建窗口内查询性能会不会退化",
		Outcomes: []OutcomePrediction{
			{Outcome: "会退化", Probability: 0.7},
			{Outcome: "不会退化", Probability: 0.3},
		},
	}
	e.persistResults(&PredictionDomain{OutcomeType: "binary", Question: result.Question}, result, nil)
	if result.ID == "" {
		t.Fatal("persistResults 应把落库 ID 盖回 result.ID, 外部才能按 ID 核销")
	}
	// 从同一目录重开存储, 验证 ID 与落库记录一致 (跨进程可见)
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := s.RecentPredictions(5)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RecentPredictions: %v n=%d", err, len(recs))
	}
	if recs[0].ID != result.ID {
		t.Fatalf("存储 ID %s != 结果 ID %s", recs[0].ID, result.ID)
	}
}

func TestRegisterResolution_CrossRestartReconstruct(t *testing.T) {
	dir := t.TempDir()
	// 进程 1: 预测并落库 (等价 CLI predict 完成退出)
	cfg1 := DefaultConfig()
	cfg1.DataDir = dir
	e1 := NewEngine(nil, cfg1)
	result := &FusedPrediction{
		Question: "网关切换新模型后 P99 延迟会不会恶化",
		Outcomes: []OutcomePrediction{
			{Outcome: "会恶化", Probability: 0.4},
			{Outcome: "不会恶化", Probability: 0.6},
		},
		Agents: []AgentPrediction{
			{AgentID: "analyst-optimist", Predictions: map[string]float64{"不会恶化": 0.9}},
		},
	}
	e1.persistResults(&PredictionDomain{OutcomeType: "binary", Question: result.Question}, result, result.Agents)
	predID := result.ID
	if predID == "" {
		t.Fatal("persistResults 未盖 ID")
	}

	// 进程 2: 全新引擎 (pending 空), 模拟 `swarm resolve <predID> <结局>`
	cfg2 := DefaultConfig()
	cfg2.DataDir = dir
	e2 := NewEngine(nil, cfg2)
	brier := e2.RegisterResolution(predID, "会恶化")
	if brier <= 0 || brier > 1 {
		t.Fatalf("Brier %.3f 越界", brier)
	}
	// 核销幂等: 同一 predID 再来一次返回 0 (已决)
	if again := e2.RegisterResolution(predID, "会恶化"); again != 0 {
		t.Fatalf("重复核销应返回 0, 实际 %.3f", again)
	}
	// 学习状态落盘: meanBrier 被更新 (SaveLearning 在 RegisterResolution 内触发)
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := s.LoadLearning()
	if err != nil {
		t.Fatal(err)
	}
	if ls == nil || ls.MeanBrier == 0 {
		t.Fatalf("核销后 MeanBrier 应非零: %+v", ls)
	}
}
