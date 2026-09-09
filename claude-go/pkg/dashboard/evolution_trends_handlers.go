package dashboard

// evolution_trends_handlers.go —— GET /api/evolution/trends (13.8.9 进化趋势重构)。
//
// 数据源: stateDir/metrics/evolution.jsonl (经 provider 缓存读, 单文件 1.3MB 级)。
// 输出四块, 对应前端进化 tab 的趋势区:
//   1. series      : 每个 metric 序列的日折叠曲线 (gap=断线不插值) + 每日点数;
//   2. delta7d     : 最近 7 天均值 vs 之前 7 天均值的三态 (up/down/flat/null);
//   3. regression  : 7d 窗口内 "比此前 7 天最优还差" 的天数计数 (仅 rate 型序列);
//   4. rounds      : learn_round_count 事件流 + 按 run_id/邻近窗口 join 的耗时。
//
// 语义约定 (与 ledger.go 一致):
//   - 键缺失/null ≠ 0: 无数据的序列/天数不产出;
//   - NaN 事件 (value=NaN) 跳过, 视同键缺失;
//   - 日折叠取该日【最后一个值】(gauge 语义: 最新状态), 天缺口保留为断线;
//   - 时区混合 (宿主 +08:00 → pod +00:00): 统一归 UTC 日再折叠。

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

// trendSeries 一条 metric 序列的日折叠结果。
type trendSeries struct {
	Name   string          `json:"name"`
	Points []trendPoint    `json:"points"` // 按 day 升序, 不连续日之间是断线
	Delta  *trendDelta     `json:"delta7d"`
	Regr   int             `json:"regression7d"` // 最近 7 天低于此前 7 天最优的天数 (rate 型)
	Last   *float64        `json:"last"`         // 最近一个点的值 (null=无数据)
	LastDay string         `json:"lastDay,omitempty"`
}

type trendPoint struct {
	Day   string   `json:"day"` // YYYY-MM-DD (UTC)
	Value float64  `json:"value"`
	Count int      `json:"count"` // 当日事件数 (暴露滑动窗/聚合密度)
}

type trendDelta struct {
	Prev  float64  `json:"prev"`        // 前 7 天日折叠均值 (null=不可比)
	Last  float64  `json:"last"`        // 最近 7 天日折叠均值
	State string   `json:"state"`       // up | down | flat
	Rel   *float64 `json:"relPct"`      // 相对变化百分比 (分母为 |prev|, 0 时 null)
}

// trendRound 一条学习轮次 (learn_round_count 事件)。
type trendRound struct {
	TS       string   `json:"ts"`
	Duration *float64 `json:"durationSec,omitempty"` // 邻近 join 的轮耗时 (null=未记录)
	Proposals *int    `json:"proposals,omitempty"`
}

type evolutionTrendsResp struct {
	GeneratedAt string                 `json:"generatedAt"`
	Series      []trendSeries          `json:"series"`
	Rounds      []trendRound           `json:"rounds"`
	Summary     map[string]interface{} `json:"summary"`
}

// trendsWindow 端点参数: 折叠窗口天数 (默认 30, 覆盖当前 27 天存量)。
func (s *Server) handleEvolutionTrends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	days := parseIntQuery(r, "days", 30)
	if days <= 0 || days > 365 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("days 取值 1..365"))
		return
	}
	evts, err := s.provider.readMetricEvents("evolution")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, buildEvolutionTrends(evts, time.Now().UTC(), days))
}

// buildEvolutionTrends 纯函数主体 (与 handler 解耦便于单测)。
func buildEvolutionTrends(evts []rawMetricEvent, now time.Time, days int) evolutionTrendsResp {
	// 窗口起点 (含): now 所在 UTC 日 - (days-1)。
	cutoff := now.UTC().AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)

	// ---- 1) 按序列收集日桶: name -> day -> (lastVal, count) ----
	type dayBucket struct {
		last   float64
		count  int
		day    time.Time // 该日 UTC 零点, 供窗口比较
	}
	buckets := map[string]map[string]*dayBucket{}
	names := map[string]bool{}
	for _, e := range evts {
		if math.IsNaN(e.Value) || math.IsInf(e.Value, 0) {
			continue // NaN 事件视同键缺失 (ledger 口径)
		}
		day := e.Timestamp.UTC().Truncate(24 * time.Hour)
		if day.Before(cutoff) || day.After(now.UTC()) {
			continue
		}
		key := day.Format("2006-01-02")
		m, ok := buckets[e.Name]
		if !ok {
			m = map[string]*dayBucket{}
			buckets[e.Name] = m
			names[e.Name] = true
		}
		b, ok := m[key]
		if !ok {
			b = &dayBucket{last: e.Value, day: day}
			m[key] = b
		} else if !e.Timestamp.Before(b.day) {
			b.last = e.Value // 同日内取最后写入
		}
		b.count++
	}

	// ---- 2) 序列组装: 曲线 + delta + 回归计数 ----
	week := 7 * 24 * time.Hour
	out := make([]trendSeries, 0, len(buckets))
	for name, m := range buckets {
		days_ := make([]string, 0, len(m))
		for d := range m {
			days_ = append(days_, d)
		}
		sort.Strings(days_)
		pts := make([]trendPoint, 0, len(days_))
		for _, d := range days_ {
			b := m[d]
			pts = append(pts, trendPoint{Day: d, Value: b.last, Count: b.count})
		}

		// 7d 窗口切分: 以 [now-7d, now] 为最近窗, [now-14d, now-7d) 为前窗。
		lastWin, prevWin := splitWindows(pts, now, week)

		delta := computeDelta(lastWin, prevWin)
		regr := computeRegression(lastWin, prevWin)
		var last *float64
		lastDay := ""
		if len(pts) > 0 {
			v := pts[len(pts)-1].Value
			last = &v
			lastDay = pts[len(pts)-1].Day
		}
		out = append(out, trendSeries{
			Name: name, Points: pts, Delta: delta, Regr: regr,
			Last: last, LastDay: lastDay,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// ---- 3) 轮次表: learn_round_count 流 + 邻近 join 耗时/提案数 ----
	rounds := buildRounds(evts, cutoff)

	resp := evolutionTrendsResp{
		GeneratedAt: now.Format(time.RFC3339),
		Series:      out,
		Rounds:      rounds,
		Summary: map[string]interface{}{
			"windowDays":    days,
			"seriesCount":   len(out),
			"roundCount":    len(rounds),
		},
	}
	return resp
}

// splitWindows 按日切 [now-7d, now] 与 [now-14d, now-7d)。
func splitWindows(pts []trendPoint, now time.Time, week time.Duration) (last, prev []trendPoint) {
	lastStart := now.Add(-week)
	prevStart := now.Add(-2 * week)
	for _, p := range pts {
		d, err := time.ParseInLocation("2006-01-02", p.Day, time.UTC)
		if err != nil {
			continue
		}
		// 日代表点取 12:00, 与最近窗/前窗边界比较。
		mid := d.Add(12 * time.Hour)
		switch {
		case !mid.Before(lastStart):
			last = append(last, p)
		case !mid.Before(prevStart):
			prev = append(prev, p)
		}
	}
	return last, prev
}

// computeDelta 三态 delta: 最近 7 天均值 vs 前 7 天均值。任一窗空 → null。
func computeDelta(last, prev []trendPoint) *trendDelta {
	if len(last) == 0 || len(prev) == 0 {
		return nil
	}
	mean := func(vs []trendPoint) float64 {
		s := 0.0
		for _, v := range vs {
			s += v.Value
		}
		return s / float64(len(vs))
	}
	a, b := mean(last), mean(prev)
	d := &trendDelta{Prev: b, Last: a}
	const eps = 1e-9
	switch {
	case math.Abs(a-b) <= eps:
		d.State = "flat"
	case a > b:
		d.State = "up"
	default:
		d.State = "down"
	}
	if math.Abs(b) > eps {
		r := (a - b) / math.Abs(b) * 100
		d.Rel = &r
	}
	return d
}

// computeRegression 回归计数: 最近 7 天中, 值比前 7 天最优还差的日数。
// 方向由 delta 判定 (up=涨好): down-good 序列 (如失败率) 用 (best - v) 判。
// 任一窗空 → 0 (不可判, 不计入)。
func computeRegression(last, prev []trendPoint) int {
	if len(last) == 0 || len(prev) == 0 {
		return 0
	}
	best := prev[0].Value
	for _, p := range prev[1:] {
		if p.Value < best {
			best = p.Value
		}
	}
	// down 为劣化方向 (rate 型序列占多数); up 劣化的序列 (失败率/成本)
	// 语义相反 —— 这里按序列名无法判定, 统一以 down=劣化 为约定,
	// 由前端按序列语义决定是否展示该计数 (high-is-good 序列)。
	n := 0
	for _, p := range last {
		if p.Value < best {
			n++
		}
	}
	return n
}

// buildRounds 学习轮次: learn_round_count=1 事件 + 邻近 60s 内的
// learn_round_duration_sec / workflow_proposals join (同轮产物)。
func buildRounds(evts []rawMetricEvent, cutoff time.Time) []trendRound {
	var counts []rawMetricEvent
	var durs, props []rawMetricEvent
	for _, e := range evts {
		switch e.Name {
		case "learn_round_count":
			if !e.Timestamp.Before(cutoff) {
				counts = append(counts, e)
			}
		case "learn_round_duration_sec":
			durs = append(durs, e)
		case "workflow_proposals":
			props = append(props, e)
		}
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].Timestamp.Before(counts[j].Timestamp) })
	sort.Slice(durs, func(i, j int) bool { return durs[i].Timestamp.Before(durs[j].Timestamp) })
	sort.Slice(props, func(i, j int) bool { return props[i].Timestamp.Before(props[j].Timestamp) })

	nearest := func(src []rawMetricEvent, at time.Time) *rawMetricEvent {
		// 取 at 之前最近的一条 (轮次计数在轮末, 耗时先落 → 取 [at-60s, at] 内最近)。
		var best *rawMetricEvent
		var bestDt time.Duration = 61 * time.Second
		for i := range src {
			dt := at.Sub(src[i].Timestamp)
			if dt >= 0 && dt <= bestDt {
				bestDt = dt
				best = &src[i]
			}
		}
		return best
	}

	out := make([]trendRound, 0, len(counts))
	for _, c := range counts {
		r := trendRound{TS: c.Timestamp.UTC().Format(time.RFC3339)}
		if d := nearest(durs, c.Timestamp); d != nil {
			v := d.Value
			r.Duration = &v
		}
		if p := nearest(props, c.Timestamp); p != nil {
			v := int(p.Value)
			r.Proposals = &v
		}
		out = append(out, r)
	}
	// 轮次最多回溯展示 200 条 (防刷屏, 新的在后)。
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out
}
