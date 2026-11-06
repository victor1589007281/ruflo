package swarm_intel

// A4/Phase-3 向量重联路径验收: 用代理 server 拦截 /v1/memory/search 返回
// 业务键 ID 的命中, 验证 FindSimilar/SearchReflections/FindTrails 能把
// MemoryHit.ID 重联回各自的完整记录。
//
// 背景: MemoryHit.ID 返回的是 memory 记录 ID (服务端生成或调用方显式给的
// req.ID), 不是 SessionID。索引函数必须把业务键放进 Memory.ID, 重联才成立 —
// 本测试钉住这个契约, 防止回归 (该 bug 曾让 A4 起向量路径静默回退关键词)。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"
)

// newVectorProxyServer 把请求反代到真实 agentDB 测试 server, 但拦截
// /v1/memory/search 返回固定命中 (id=cannedID)。cannedID 必须是调用方
// 已写入记录的业务键, 重联成功则向量路径应返回该记录。
func newVectorProxyServer(t *testing.T, cannedID string) string {
	t.Helper()
	realURL := newAgentDBTestServer(t)
	target, err := url.Parse(realURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/memory/search" {
			out := map[string]any{"results": []map[string]any{
				{"id": cannedID, "content": "canned vector hit", "score": 0.91},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAgentDBStoreVectorRejoinPredictions(t *testing.T) {
	url := newVectorProxyServer(t, "rej-p1")
	s, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("NewAgentDBStore: %v", err)
	}
	if err := s.AppendPrediction(&PredictionRecord{ID: "rej-p1", Question: "向量重联预测", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AppendPrediction: %v", err)
	}
	hits, err := s.FindSimilar("任意查询词", 5)
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "rej-p1" {
		t.Fatalf("向量命中应重联回 rej-p1, 实际 %+v", idsOf(hits))
	}
}

func TestAgentDBStoreVectorRejoinReflections(t *testing.T) {
	url := newVectorProxyServer(t, "rej-r1")
	s, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("NewAgentDBStore: %v", err)
	}
	rec := &ReflectionRecord{ID: "rej-r1", Domain: "binary/unknown", Question: "向量重联反思",
		Lesson: "重联路径钉住", CreatedAt: time.Now()}
	if err := s.AppendReflection(rec); err != nil {
		t.Fatalf("AppendReflection: %v", err)
	}
	hits, err := s.SearchReflections("任意查询词", 5)
	if err != nil {
		t.Fatalf("SearchReflections: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "rej-r1" {
		t.Fatalf("向量命中应重联回 rej-r1, 实际 %+v", hits)
	}
}

func TestAgentDBStoreVectorRejoinTrails(t *testing.T) {
	url := newVectorProxyServer(t, "rej-h1")
	s, err := NewAgentDBStore(url)
	if err != nil {
		t.Fatalf("NewAgentDBStore: %v", err)
	}
	trails := []*PheromoneTrail{{HypothesisID: "rej-h1", Hypothesis: "向量重联信素", Strength: 0.9, LastUpdate: time.Now()}}
	if err := s.SaveTrails("binary", trails); err != nil {
		t.Fatalf("SaveTrails: %v", err)
	}
	hits, err := s.FindTrails("任意查询词", 5)
	if err != nil {
		t.Fatalf("FindTrails: %v", err)
	}
	if len(hits) == 0 || hits[0].HypothesisID != "rej-h1" {
		t.Fatalf("向量命中应重联回 rej-h1, 实际 %+v", hits)
	}
}
