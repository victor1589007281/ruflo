package swarm_intel

// A4 深化 (服务端 tag filter) 的混部桶验收: 预测/反思/信素三类记录共库,
// 各自检索只回本类型 —— 不再靠 limit*4 窗口在 80 条混部记录里赌排名。
// 服务端走真实 agentDB serve (HTTP 全链路), 需要带 Memory backend 的 server。

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gitee.com/lorydb/agentDB/pkg/agentstore"
	"gitee.com/lorydb/agentDB/pkg/embedding"
	"gitee.com/lorydb/agentDB/pkg/kv"
	"gitee.com/lorydb/agentDB/pkg/memory"
	"gitee.com/lorydb/agentDB/pkg/server"
)

// newMemoryBackedTestServer 起带 Memory + Embedding (hash stub) 的 agentDB serve。
func newMemoryBackedTestServer(t *testing.T) string {
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
	mem, err := memory.NewStore(128)
	if err != nil {
		t.Fatal(err)
	}
	emb := embedding.NewService()
	emb.Register("d", embedding.NewHashStubProvider("d", 128, 512))
	emb.SetDefault("d")
	srv := server.New(server.Backends{Store: st, Leases: lm, KV: kvDB, Memory: mem, Embedding: emb})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { kvDB.Close() })
	return ts.URL
}

func TestAgentDBStoreTagFilterIsolatesTypes(t *testing.T) {
	s, err := NewAgentDBStore(newMemoryBackedTestServer(t))
	if err != nil {
		t.Fatalf("NewAgentDBStore: %v", err)
	}
	now := time.Now()
	// 三类记录同库; 内容刻意同域 (索引重建), 排名必交叉。
	if err := s.AppendPrediction(&PredictionRecord{
		ID: "tf-pred-1", Question: "数据库索引重建窗口内查询性能会不会明显退化",
		Outcomes: []OutcomePrediction{{Outcome: "会退化", Probability: 0.7}}, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendReflection(&ReflectionRecord{
		ID: "tf-refl-1", Domain: "binary/unknown", Question: "数据库索引重建期间查询性能会不会退化",
		Lesson: "预测偏差: 高估低风险。Scout 时应主动寻找 P95 回归数据。", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTrails("binary", []*PheromoneTrail{
		{HypothesisID: "tf-trail-1", Hypothesis: "索引重建窗口内查询性能明显退化", Strength: 0.8, LastUpdate: now},
	}); err != nil {
		t.Fatal(err)
	}

	q := "索引重建 查询性能 退化"
	preds, err := s.FindSimilar(q, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range preds {
		if p.ID != "tf-pred-1" {
			t.Fatalf("FindSimilar 混入了非预测记录: %s", p.ID)
		}
	}
	refls, err := s.SearchReflections(q, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(refls) == 0 {
		t.Fatal("SearchReflections 应命中反思, 实际 0")
	}
	for _, r := range refls {
		if r.ID != "tf-refl-1" {
			t.Fatalf("SearchReflections 混入了非反思记录: %s", r.ID)
		}
	}
	trails, err := s.FindTrails(q, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(trails) == 0 {
		t.Fatal("FindTrails 应命中信素, 实际 0")
	}
	for _, tr := range trails {
		if tr.HypothesisID != "tf-trail-1" {
			t.Fatalf("FindTrails 混入了非信素记录: %s", tr.HypothesisID)
		}
	}
}
