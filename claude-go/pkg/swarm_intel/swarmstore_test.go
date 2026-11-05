package swarm_intel

// v3 Phase 0 验收: JSONLStore 与 AgentDBStore 跑同一组 SwarmStore 用例。
// 允许的差异只有时间戳 (CreatedAt/UpdatedAt/ResolvedAt 由写入路径补戳)。

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gitee.com/lorydb/agentDB/pkg/agentstore"
	"gitee.com/lorydb/agentDB/pkg/kv"
	"gitee.com/lorydb/agentDB/pkg/server"
)

// newAgentDBTestServer 起一个进程内 agentDB serve (HTTP 全链路, 与生产同协议),
// 作为 AgentDBStore 的 fake server。
func newAgentDBTestServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	kvDB, err := kv.Open(dir, kv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := agentstore.Open(kvDB, filepath.Join(dir, "agentstore"))
	if err != nil {
		t.Fatal(err)
	}
	lm, err := agentstore.NewLeaseManager(st)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Backends{Store: st, Leases: lm})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { kvDB.Close() })
	return ts.URL
}

func TestJSONLStoreSuite(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	runSwarmStoreSuite(t, s)
}

func TestAgentDBStoreSuite(t *testing.T) {
	s, err := NewAgentDBStore(newAgentDBTestServer(t))
	if err != nil {
		t.Fatalf("NewAgentDBStore: %v", err)
	}
	runSwarmStoreSuite(t, s)
}

// TestAgentDBStoreUnreachable 验证可达性探针: 连不上的地址必须报错 (调用方据此回退 JSONL)。
func TestAgentDBStoreUnreachable(t *testing.T) {
	if _, err := NewAgentDBStore("http://127.0.0.1:1"); err == nil {
		t.Fatalf("不可达地址应返回错误")
	}
}

// TestNewEngineAgentDBFallback 验证 E1/Phase0 回退语义: agentDB 不可达时 NewEngine 不 crash,
// 持久化回退 JSONL 后端。
func TestNewEngineAgentDBFallback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.StoreBackend = "agentdb"
	cfg.AgentDBURL = "http://127.0.0.1:1" // 必失败
	e := NewEngine(nil, cfg)
	if e == nil {
		t.Fatalf("引擎应能构造 (回退 jsonl)")
	}
	if _, ok := e.store.(*JSONLStore); !ok {
		t.Fatalf("agentDB 不可达时应回退 *JSONLStore, 实际 %T", e.store)
	}
}

// runSwarmStoreSuite 两个后端共用的契约用例集。
func runSwarmStoreSuite(t *testing.T, s SwarmStore) {
	t.Helper()
	t.Run("TrailsRoundTrip", func(t *testing.T) { suiteTrails(t, s) })
	t.Run("PredictionsRecent", func(t *testing.T) { suitePredictionsRecent(t, s) })
	t.Run("PredictionsFindSimilar", func(t *testing.T) { suitePredictionsFindSimilar(t, s) })
	t.Run("ReasoningsSearch", func(t *testing.T) { suiteReasonings(t, s) })
	t.Run("LearningRoundTrip", func(t *testing.T) { suiteLearning(t, s) })
	t.Run("SimEventsFilterByRun", func(t *testing.T) { suiteSimEvents(t, s) })
	t.Run("ResolvePrediction", func(t *testing.T) { suiteResolvePrediction(t, s) })
}

func suiteTrails(t *testing.T, s SwarmStore) {
	trailsA := []*PheromoneTrail{
		{HypothesisID: "h1", Hypothesis: "乐观情景", Strength: 0.8, LastUpdate: time.Now()},
		{HypothesisID: "h2", Hypothesis: "悲观情景", Strength: 0.3, LastUpdate: time.Now()},
	}
	trailsB := []*PheromoneTrail{
		{HypothesisID: "h3", Hypothesis: "中性情景", Strength: 0.5, LastUpdate: time.Now()},
	}
	if err := s.SaveTrails("Scenario", trailsA); err != nil {
		t.Fatalf("SaveTrails A: %v", err)
	}
	if err := s.SaveTrails("binary", trailsB); err != nil {
		t.Fatalf("SaveTrails B: %v", err)
	}
	gotA, err := s.LoadTrails("scenario") // 大小写不敏感 (safeDomainKey)
	if err != nil {
		t.Fatalf("LoadTrails A: %v", err)
	}
	if len(gotA) != 2 {
		t.Fatalf("A 域应有 2 条信素, 实际 %d", len(gotA))
	}
	gotB, err := s.LoadTrails("binary")
	if err != nil || len(gotB) != 1 || gotB[0].HypothesisID != "h3" {
		t.Fatalf("B 域应有 1 条 h3, 实际 %v err=%v", gotB, err)
	}
	// 空域 → 空集不是错误
	if empty, err := s.LoadTrails("不存在的域"); err != nil || len(empty) != 0 {
		t.Fatalf("空域应返回空集, 实际 %v err=%v", empty, err)
	}
}

func suitePredictionsRecent(t *testing.T, s SwarmStore) {
	base := time.Now().Add(-time.Hour)
	for i, q := range []string{"q1", "q2", "q3"} {
		if err := s.AppendPrediction(&PredictionRecord{
			ID: q, Question: q, Consensus: 0.5, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("AppendPrediction %s: %v", q, err)
		}
	}
	recent, err := s.RecentPredictions(2)
	if err != nil {
		t.Fatalf("RecentPredictions: %v", err)
	}
	if len(recent) != 2 || recent[0].ID != "q2" || recent[1].ID != "q3" {
		t.Fatalf("最近 2 条应为 [q2 q3], 实际 %v", idsOf(recent))
	}
	all, err := s.RecentPredictions(0) // n<=0 不限条数
	if err != nil || len(all) != 3 {
		t.Fatalf("n<=0 应返回全部, 实际 %d err=%v", len(all), err)
	}
}

func suitePredictionsFindSimilar(t *testing.T, s SwarmStore) {
	if err := s.AppendPrediction(&PredictionRecord{ID: "db", Question: "数据库性能会不会退化", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPrediction(&PredictionRecord{ID: "ui", Question: "前端界面改版用户是否满意", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.FindSimilar("数据库 性能 监控", 5)
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "db" {
		t.Fatalf("应优先命中 db 记录, 实际 %v", idsOf(hits))
	}
}

func suiteReasonings(t *testing.T, s SwarmStore) {
	if err := s.StoreReasoning(&ReasoningEntry{Question: "缓存命中率下降原因", Reasoning: "r1", Confidence: 0.7, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreReasoning(&ReasoningEntry{Question: "团队士气与交付速度", Reasoning: "r2", Confidence: 0.6, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchReasonings("缓存 命中率", 5)
	if err != nil || len(hits) == 0 || hits[0].Reasoning != "r1" {
		t.Fatalf("应命中 r1, 实际 %v err=%v", hits, err)
	}
}

func suiteLearning(t *testing.T, s SwarmStore) {
	want := &LearningState{
		ConformalScores: []float64{0.1, 0.4, 0.2},
		BanditAlphas:    map[string]float64{"analyst-optimist": 3},
		BanditBetas:     map[string]float64{"analyst-optimist": 1},
		TrustScores:     map[string]float64{"analyst-optimist": 0.7},
		Resolutions:     10,
		MeanBrier:       0.33,
		UpdatedAt:       time.Now(),
	}
	if err := s.SaveLearning(want); err != nil {
		t.Fatalf("SaveLearning: %v", err)
	}
	got, err := s.LoadLearning()
	if err != nil {
		t.Fatalf("LoadLearning: %v", err)
	}
	if got == nil {
		t.Fatalf("LoadLearning 不应为 nil")
	}
	// 时间戳允许序列化差 (精度), 其余字段须逐字段一致
	if !reflect.DeepEqual(got.ConformalScores, want.ConformalScores) ||
		got.Resolutions != want.Resolutions ||
		got.MeanBrier != want.MeanBrier ||
		got.TrustScores["analyst-optimist"] != want.TrustScores["analyst-optimist"] {
		t.Fatalf("学习状态往返不一致: got %+v want %+v", got, want)
	}
}

func suiteSimEvents(t *testing.T, s SwarmStore) {
	mk := func(runID string, round int, agent string) *SimEvent {
		payload, _ := json.Marshal(map[string]float64{"stance": 0.5})
		return &SimEvent{RunID: runID, Round: round, Type: "stance", AgentID: agent, Payload: payload}
	}
	if err := s.AppendSimEvent(mk("run-1", 1, "A1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSimEvent(mk("run-1", 2, "A2")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSimEvent(mk("run-2", 1, "B1")); err != nil {
		t.Fatal(err)
	}
	evs, err := s.SimEvents("run-1")
	if err != nil {
		t.Fatalf("SimEvents: %v", err)
	}
	if len(evs) != 2 || evs[0].AgentID != "A1" || evs[1].Round != 2 {
		t.Fatalf("run-1 应有 A1/A2 两条按序返回, 实际 %+v", evs)
	}
	// 存储层补戳: CreatedAt 非零
	if evs[0].CreatedAt.IsZero() {
		t.Fatalf("AppendSimEvent 应补 CreatedAt")
	}
	other, err := s.SimEvents("run-2")
	if err != nil || len(other) != 1 || other[0].AgentID != "B1" {
		t.Fatalf("run-2 应只有 B1, 实际 %v err=%v", other, err)
	}
}

func suiteResolvePrediction(t *testing.T, s SwarmStore) {
	rec := &PredictionRecord{ID: "pending-1", Question: "谁会赢", CreatedAt: time.Now()}
	if err := s.AppendPrediction(rec); err != nil {
		t.Fatal(err)
	}
	r, ok := s.(predictionResolver)
	if !ok {
		t.Skip("后端未实现 predictionResolver, 跳过原地回填用例")
	}
	// AgentDBStore 骨架: 追加型日志桶无原地改写端点, 必须显式报"未实现",
	// 调用方 (Engine.RegisterResolution) 据此退回"追加 resolved 记录"路径。
	if _, isAdb := s.(*AgentDBStore); isAdb {
		if err := r.ResolvePrediction("pending-1", "结局A", 0.25, time.Now()); err == nil {
			t.Fatalf("agentDB 骨架的 ResolvePrediction 应返回未实现错误")
		}
		return
	}
	resolvedAt := time.Now()
	if err := r.ResolvePrediction("pending-1", "结局A", 0.25, resolvedAt); err != nil {
		t.Fatalf("ResolvePrediction: %v", err)
	}
	all, err := s.RecentPredictions(0)
	if err != nil {
		t.Fatal(err)
	}
	var found *PredictionRecord
	for _, p := range all {
		if p.ID == "pending-1" {
			found = p
		}
	}
	if found == nil || !found.Resolved || found.ActualLabel != "结局A" || found.BrierScore != 0.25 || found.ResolvedAt == nil {
		t.Fatalf("原地回填后记录应已决: %+v", found)
	}
	if err := r.ResolvePrediction("不存在的ID", "x", 0, time.Now()); err == nil {
		t.Fatalf("回填不存在的 ID 应报错")
	}
}

// TestAgentDBStoreNamespaceIsolation A7: 不同 namespace 的桶完全隔离。
// ns1 写入的数据不能被 ns2 读到; 桶名前缀不同。
func TestAgentDBStoreNamespaceIsolation(t *testing.T) {
	url := newAgentDBTestServer(t)

	// ns1 写入
	t.Setenv("AGENTDB_NAMESPACE", "ns1")
	s1, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("ns1 NewAgentDBStore: %v", err)
	}
	if err := s1.AppendPrediction(&PredictionRecord{ID: "ns1-p1", Question: "ns1 问题", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("ns1 AppendPrediction: %v", err)
	}
	if err := s1.SaveLearning(&LearningState{Resolutions: 1, MeanBrier: 0.1}); err != nil {
		t.Fatalf("ns1 SaveLearning: %v", err)
	}

	// ns2 看不到 ns1 数据
	t.Setenv("AGENTDB_NAMESPACE", "ns2")
	s2, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("ns2 NewAgentDBStore: %v", err)
	}
	preds, err := s2.RecentPredictions(10)
	if err != nil || len(preds) != 0 {
		t.Fatalf("ns2 不应看到 ns1 的预测, 实际 %d err=%v", len(preds), err)
	}
	st, err := s2.LoadLearning()
	if err != nil || st != nil {
		t.Fatalf("ns2 不应看到 ns1 的学习状态, 实际 %+v err=%v", st, err)
	}

	// ns1 仍能看到自己的数据
	t.Setenv("AGENTDB_NAMESPACE", "ns1")
	s1Again, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("ns1 re-open: %v", err)
	}
	preds1, err := s1Again.RecentPredictions(10)
	if err != nil || len(preds1) != 1 || preds1[0].ID != "ns1-p1" {
		t.Fatalf("ns1 重开应能看到自己的预测, 实际 %v err=%v", preds1, err)
	}

	// 默认 ns (空) 与 ns1 也不互通
	t.Setenv("AGENTDB_NAMESPACE", "")
	sDef, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("default NewAgentDBStore: %v", err)
	}
	predsDef, err := sDef.RecentPredictions(10)
	if err != nil || len(predsDef) != 0 {
		t.Fatalf("default ns 不应看到 ns1 数据, 实际 %d err=%v", len(predsDef), err)
	}
}

func idsOf(recs []*PredictionRecord) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	return ids
}

// TestAgentDBStoreFilteredANN 验证 Filtered ANN 语义检索的容错与回退:
//   - 写入端: AppendPrediction/StoreReasoning 即使 Memory 向量桶不可达 (本测试 server
//     未装配 Memory 后端) 也应成功 (主写路径是 log 桶, 向量写失败仅 warn)。
//   - 检索端: Memory.SearchMemory 失败时应自动回退到关键词重叠 (P0 行为),
//     保证向量桶冷启动/不可达时仍可工作。
func TestAgentDBStoreFilteredANN(t *testing.T) {
	s, err := NewAgentDBStore(newAgentDBTestServer(t))
	if err != nil {
		t.Fatalf("NewAgentDBStore: %v", err)
	}

	// 写入: 不因向量桶未装配而失败
	rec := &PredictionRecord{ID: "ann-1", Question: "向量检索是否会替代关键词匹配", CreatedAt: time.Now()}
	if err := s.AppendPrediction(rec); err != nil {
		t.Fatalf("AppendPrediction 不应因 Memory 不可达而失败: %v", err)
	}
	entry := &ReasoningEntry{Question: "语义嵌入的维度选择", Reasoning: "r-ann", Confidence: 0.8, CreatedAt: time.Now()}
	if err := s.StoreReasoning(entry); err != nil {
		t.Fatalf("StoreReasoning 不应因 Memory 不可达而失败: %v", err)
	}

	// 检索: 关键词回退仍工作 (Memory 不可达, SearchMemory 报错 → 走原路径)
	hits, err := s.FindSimilar("向量 检索 嵌入", 5)
	if err != nil {
		t.Fatalf("FindSimilar 应能容错回退: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "ann-1" {
		t.Fatalf("回退应命中 ann-1, 实际 %v", idsOf(hits))
	}

	rhits, err := s.SearchReasonings("语义 嵌入", 5)
	if err != nil {
		t.Fatalf("SearchReasonings 应能容错回退: %v", err)
	}
	if len(rhits) == 0 || rhits[0].Reasoning != "r-ann" {
		t.Fatalf("回退应命中 r-ann, 实际 %+v", rhits)
	}
}
