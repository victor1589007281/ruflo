package dashboard

// evolution_trends_handlers_test.go —— /api/evolution/trends 单测 (13.8.9)。
//
// 锁定语义:
//  1. 日折叠 = 每序列每日最后一个值, gap 天保留断线 (不插值);
//  2. delta7d 三态 + 任一窗空 → null; NaN 事件视同键缺失;
//  3. 轮次表 = learn_round_count 流 + 60s 邻近 join 耗时;
//  4. 空账本 → 空 series/rounds 但仍 200。

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTrendFixture 写两天 × 两序列 + NaN 事件 + 轮次对 (count + 3s 后 duration)。
// 日期相对 now (6 天前/5 天前), 保证落在默认 30 天窗内且不随时间漂移。
func writeTrendFixture(t *testing.T, stateDir string) {
	t.Helper()
	dir := filepath.Join(stateDir, "metrics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(ts time.Time, name string, v float64) {
		line, _ := json.Marshal(map[string]any{
			"ts": ts.Format(time.RFC3339Nano), "module": "evolution",
			"name": name, "value": v,
		})
		f, err := os.OpenFile(filepath.Join(dir, "evolution.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(line, '\n'))
		f.Close()
	}
	// evo_success_rate: D1=0.5 (两点, 末值 0.6), D2=0.8 → 日折叠 [0.6, 0.8]。
	d1 := time.Now().UTC().AddDate(0, 0, -6)
	d2 := d1.AddDate(0, 0, 1)
	mk(d1, "evo_success_rate", 0.5)
	mk(d1.Add(2*time.Hour), "evo_success_rate", 0.6)
	mk(d2, "evo_success_rate", 0.8)
	// evo_fail_rate: 仅 D1=0.2, D2 缺失 → gap。
	mk(d1, "evo_fail_rate", 0.2)
	// NaN 事件: 视同键缺失, 不进曲线。
	mk(d1, "evo_nan_series", math.NaN())
	// 轮次: count 在 t, duration 在 t+3s (60s 窗内 → 应 join 上)。
	roundT := time.Date(d2.Year(), d2.Month(), d2.Day(), 10, 0, 0, 0, time.UTC)
	mk(roundT, "learn_round_duration_sec", 42.5)
	mk(roundT.Add(3*time.Second), "learn_round_count", 1)
	// 窗外 (65 天前) 的旧轮次: 默认 30 天窗不含。
	mk(roundT.AddDate(0, 0, -60), "learn_round_count", 1)
}

func TestHandleEvolutionTrends(t *testing.T) {
	stateDir := t.TempDir()
	writeTrendFixture(t, stateDir)
	s := NewServer(Config{StateDir: stateDir})

	code, body := getJSON(t, s.handleEvolutionTrends)
	if code != http.StatusOK {
		t.Fatalf("应 200, 得 %d", code)
	}

	raw, _ := json.Marshal(body)
	var resp evolutionTrendsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}

	byName := map[string]trendSeries{}
	for _, sr := range resp.Series {
		byName[sr.Name] = sr
	}

	// 日折叠: evo_success_rate 应有 D1(0.6, count=2) 与 D2(0.8)。
	sr, ok := byName["evo_success_rate"]
	if !ok {
		t.Fatalf("缺 evo_success_rate 序列: %s", raw)
	}
	if len(sr.Points) != 2 || sr.Points[0].Value != 0.6 || sr.Points[0].Count != 2 || sr.Points[1].Value != 0.8 {
		t.Fatalf("日折叠错: %+v", sr.Points)
	}
	// last 语义。
	if sr.Last == nil || *sr.Last != 0.8 {
		t.Fatalf("last 应 0.8, 得 %v", sr.Last)
	}

	// gap: evo_fail_rate 仅 D1 一点。
	fr, ok := byName["evo_fail_rate"]
	if !ok || len(fr.Points) != 1 || fr.Points[0].Value != 0.2 {
		t.Fatalf("gap 序列错: %+v", fr)
	}

	// NaN 序列不落 0、不产出。
	if _, ok := byName["evo_nan_series"]; ok {
		t.Fatalf("NaN 事件应视同键缺失, 不应产出序列")
	}

	// 轮次: 两条 count (一条 60 天前在 30 天窗外 → 只 1 条), join 耗时 42.5。
	if len(resp.Rounds) != 1 {
		t.Fatalf("轮次应 1 条 (窗外不计), 得 %d: %+v", len(resp.Rounds), resp.Rounds)
	}
	r := resp.Rounds[0]
	if r.Duration == nil || *r.Duration != 42.5 {
		t.Fatalf("轮次应 join 到耗时 42.5, 得 %+v", r)
	}
	if resp.Summary["roundCount"].(float64) != 1 {
		t.Fatalf("summary.roundCount 应 1, 得 %v", resp.Summary["roundCount"])
	}
}

func TestHandleEvolutionTrendsEmpty(t *testing.T) {
	s := NewServer(Config{StateDir: t.TempDir()})
	code, body := getJSON(t, s.handleEvolutionTrends)
	if code != http.StatusOK {
		t.Fatalf("空账本应 200, 得 %d", code)
	}
	if n, _ := body["series"].([]any); len(n) != 0 {
		t.Errorf("空账本 series 应空数组, 得 %v", n)
	}
	if n, _ := body["rounds"].([]any); len(n) != 0 {
		t.Errorf("空账本 rounds 应空数组, 得 %v", n)
	}
}

// computeDelta 三态: up/down/flat/null 边界。
func TestComputeDeltaStates(t *testing.T) {
	mkpts := func(vals ...float64) []trendPoint {
		out := make([]trendPoint, len(vals))
		for i, v := range vals {
			out[i] = trendPoint{Day: fmt.Sprintf("2026-08-%02d", i+1), Value: v}
		}
		return out
	}
	if d := computeDelta(mkpts(0.8, 0.9), mkpts(0.5, 0.6)); d == nil || d.State != "up" {
		t.Errorf("0.85 vs 0.55 应 up, 得 %+v", d)
	}
	if d := computeDelta(mkpts(0.3), mkpts(0.7)); d == nil || d.State != "down" {
		t.Errorf("0.3 vs 0.7 应 down, 得 %+v", d)
	}
	if d := computeDelta(mkpts(0.5), mkpts(0.5)); d == nil || d.State != "flat" {
		t.Errorf("等值应 flat, 得 %+v", d)
	}
	if d := computeDelta(mkpts(0.5), nil); d != nil {
		t.Errorf("前窗空应 null, 得 %+v", d)
	}
}

// computeRegression: 最近窗低于前窗最优的天数。
func TestComputeRegression(t *testing.T) {
	last := []trendPoint{{Day: "2026-08-08", Value: 0.9}, {Day: "2026-08-09", Value: 0.5}}
	prev := []trendPoint{{Day: "2026-08-01", Value: 0.8}, {Day: "2026-08-02", Value: 0.85}}
	if n := computeRegression(last, prev); n != 1 {
		t.Errorf("仅 0.5 低于最优 0.8, 应 1, 得 %d", n)
	}
	if n := computeRegression(last, nil); n != 0 {
		t.Errorf("前窗空应 0 (不可判), 得 %d", n)
	}
}
