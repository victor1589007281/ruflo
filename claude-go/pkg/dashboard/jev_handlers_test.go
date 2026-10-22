// jev_handlers_test.go 钉住 /api/jev/stats 聚合逻辑的关键不变量：
//   - recall_dual 的 Jaccard / Top1Match / UniqueToJev 算法
//   - quality_gate 的 length_conf vs jev_conf 对照与 FellBack 计数
//   - 延迟分位数
// 这些数字是「双跑攒数据」阶段的判断依据，不能让聚合算错。
package dashboard

import (
	"encoding/json"
	"testing"
)

func mkRec(kind, model, role string, latMs int64, fellBack bool, extra map[string]interface{}) jevRecord {
	if extra == nil {
		extra = map[string]interface{}{}
	}
	if role != "" {
		extra["role"] = role
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"kind": kind, "model": model, "at": "2026-10-22T01:00:00Z",
		"latency_ms": latMs, "fell_back": fellBack, "extra": extra,
	})
	return jevRecord{Kind: kind, Model: model, At: "2026-10-22T01:00:00Z",
		LatencyMS: latMs, FellBack: fellBack, Extra: extra, Raw: raw}
}

func TestJevAggregateRecallDual(t *testing.T) {
	recs := []jevRecord{
		mkRec("recall_dual", "snap:2b", "researcher", 1200, false, map[string]interface{}{
			"bm25_picks": []interface{}{"e1", "e2", "e3"},
			"jev_picks":  []interface{}{"e1", "e2", "e3"},
		}),
		mkRec("recall_dual", "snap:2b", "analyst", 800, false, map[string]interface{}{
			"bm25_picks": []interface{}{"e1", "e2"},
			"jev_picks":  []interface{}{"e2", "e1"}, // 同集合不同序 → jaccard=1, top1 不同
		}),
		mkRec("recall_dual", "snap:2b", "critic", 2000, false, map[string]interface{}{
			"bm25_picks": []interface{}{"e1", "e2"},
			"jev_picks":  []interface{}{"e1", "e4"}, // 一半重合, top1 相同, e4 是 jev 独有
		}),
	}
	stats := aggregateJevStats(recs, 100)
	rd := stats.RecallDual
	if rd.Count != 3 {
		t.Fatalf("Count=%d, want 3", rd.Count)
	}
	if rd.AgreeCount != 1 {
		t.Errorf("AgreeCount=%d, want 1 (only the identical-order one)", rd.AgreeCount)
	}
	// jaccard: 1.0 + 1.0 + (|{e1}|/|{e1,e2,e4}|=1/3) = 2.333 / 3 ≈ 0.778
	want := (1.0 + 1.0 + 1.0/3.0) / 3.0
	if abs(rd.OverlapAvg-want) > 0.001 {
		t.Errorf("OverlapAvg=%.3f, want %.3f", rd.OverlapAvg, want)
	}
	// top1: 1 (e1==e1) + 0 (e1 vs e2) + 1 (e1==e1) = 2/3
	if abs(rd.Top1MatchRate-2.0/3.0) > 0.001 {
		t.Errorf("Top1MatchRate=%.3f, want 0.667", rd.Top1MatchRate)
	}
}

func TestJevAggregateQualityGate(t *testing.T) {
	recs := []jevRecord{
		mkRec("quality_gate", "jeb:4b", "analyst", 18780, false, map[string]interface{}{
			"length_conf": 1.0, "jev_conf": 0.621, "jev_score_raw": 2.484,
		}),
		mkRec("quality_gate", "jeb:4b", "analyst", 5000, true, map[string]interface{}{
			"length_conf": 0.9, "error": "context deadline exceeded",
		}),
		mkRec("quality_gate", "jeb:4b", "researcher", 12000, false, map[string]interface{}{
			"length_conf": 1.0, "jev_conf": 0.8, "jev_score_raw": 3.2,
		}),
	}
	stats := aggregateJevStats(recs, 100)
	qg := stats.QualityGate
	if qg.Count != 3 || qg.WithJevScore != 2 || qg.FellBackCount != 1 {
		t.Errorf("Count=%d WithJev=%d FB=%d, want 3/2/1", qg.Count, qg.WithJevScore, qg.FellBackCount)
	}
	// 平均只对 WithJevScore 的样本算：length=(1.0+1.0)/2=1.0, jev=(0.621+0.8)/2=0.7105
	if abs(qg.LengthConfAvg-1.0) > 0.001 {
		t.Errorf("LengthConfAvg=%.3f, want 1.0", qg.LengthConfAvg)
	}
	if abs(qg.JevConfAvg-0.7105) > 0.001 {
		t.Errorf("JevConfAvg=%.4f, want 0.7105", qg.JevConfAvg)
	}
	// divergence: |1.0-0.621| + |1.0-0.8| = 0.379+0.2 = 0.579/2 = 0.2895
	if abs(qg.DivergenceAvg-0.2895) > 0.001 {
		t.Errorf("DivergenceAvg=%.4f, want 0.2895", qg.DivergenceAvg)
	}
	// 按角色：analyst 2 条（1 OK + 1 FB），researcher 1 条 OK
	if stats.QualityGate.ByRole["analyst"].Count != 2 || stats.QualityGate.ByRole["analyst"].FellBack != 1 {
		t.Errorf("analyst by_role=%+v", stats.QualityGate.ByRole["analyst"])
	}
	if stats.QualityGate.ByRole["researcher"].Count != 1 {
		t.Errorf("researcher by_role=%+v", stats.QualityGate.ByRole["researcher"])
	}
}

func TestJevLatencyPercentiles(t *testing.T) {
	recs := []jevRecord{}
	lats := []int64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000}
	for _, l := range lats {
		recs = append(recs, mkRec("recall", "snap:2b", "r", l, false, nil))
	}
	stats := aggregateJevStats(recs, 100)
	lat := stats.Latency["recall"]
	if lat.Count != 10 || lat.P50 != 600 || lat.Max != 1000 {
		t.Errorf("latency=%+v, want Count=10 P50=600 Max=1000", lat)
	}
}

func TestJevRecentIsReversedAndCapped(t *testing.T) {
	recs := []jevRecord{}
	for i := 0; i < 30; i++ {
		recs = append(recs, mkRec("recall", "snap:2b", "r", int64(i*100), false, nil))
	}
	stats := aggregateJevStats(recs, 5)
	if len(stats.Recent) != 5 {
		t.Fatalf("Recent len=%d, want 5 (capped)", len(stats.Recent))
	}
	// 倒序：最近的最大 latency 在最前
	var first map[string]interface{}
	_ = json.Unmarshal(stats.Recent[0], &first)
	if first["latency_ms"].(float64) != 2900 {
		t.Errorf("first recent latency=%v, want 2900 (last record)", first["latency_ms"])
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
