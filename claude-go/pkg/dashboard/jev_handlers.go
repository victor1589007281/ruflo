// jev_handlers.go 提供 Jev 决策层的可观测聚合 API：从 agentDB 的 decision JSONLog
// 拉回最近 N 条流水，在内存里聚合成「模型-角色-质量」三维指标供 webapp 的
// ClaudeGo 决策 tab 消费。
//
// 数据源契约（与 pkg/decision/logger.go 一致）：
//
//	GET {agentDB}/v1/log/decision  →  {"lines": ["<base64 JSONL 行>", ...]}
//
// 三种 kind：
//
//	recall_dual   P0 召回双跑：bm25_picks vs jev_picks（命中集合/顺序对照）
//	recall        P0 单次精判：jev_choice + jev_confidence + jev_probabilities
//	quality_gate  P1 swarm 打分：length_conf vs jev_conf/jev_score_raw/jev_confidence
//
// 关键设计：本端点不持久化、不做时序存储——agentDB 已经是唯一可信源，
// dashboard 只是请求期聚合器，避免双写源数据。
package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// JevStatsResponse 是 /api/jev/stats 的响应体。字段命名刻意与 decision.Record 保持
// 一致（snake_case），方便前端直接对接。
type JevStatsResponse struct {
	// Window 是本次聚合的样本数（不含非 decision 行）。
	Window int `json:"window"`
	// Since 是最早一条流水的时间（UTC RFC3339），空表示无样本。
	Since string `json:"since,omitempty"`
	// AgentDB 是被查询的 agentDB 基址（来自 CLAUDE_GO_AGENTDB_URL），供前端显示来源。
	AgentDB string `json:"agentdb,omitempty"`

	// ByKind 按 kind 统计条数。
	ByKind map[string]int `json:"by_kind"`
	// ByModel 按决策模型统计条数（snap:2b / jeb:4b / ...）。
	ByModel map[string]int `json:"by_model"`
	// FellBack 是按 kind 统计的 fail-open 次数。
	FellBack map[string]int `json:"fell_back"`

	// RecallDual 聚合 recall_dual：BM25 与 Jev 的一致性指标。
	RecallDual JevRecallDualStats `json:"recall_dual"`
	// Recall 聚合单次 recall：Jev 置信度分布。
	Recall JevRecallStats `json:"recall"`
	// QualityGate 聚合 quality_gate：length_conf vs jev_conf 对照。
	QualityGate JevQualityStats `json:"quality_gate"`

	// Latency 按 kind 的延迟分位数（毫秒）。
	Latency map[string]JevLatencyStats `json:"latency"`

	// Recent 是最近 N 条流水（按时间倒序），供前端展示明细。
	// 字段原样透出（不落额外解析），保证前端能拿到所有 extra 字段。
	Recent []json.RawMessage `json:"recent"`
}

// JevRecallDualStats recall_dual 的聚合。
type JevRecallDualStats struct {
	Count          int     `json:"count"`
	AgreeCount     int     `json:"agree_count"`      // bm25 与 jev 完全一致（含顺序）
	OverlapAvg     float64 `json:"overlap_avg"`      // 平均 Jaccard 交并比
	Top1MatchRate  float64 `json:"top1_match_rate"`  // 两边第 1 名一致的比例
	UniqueToJevAvg float64 `json:"unique_to_jev_avg"` // Jev 独有候选的平均占比（反映精判增益）
}

// JevRecallStats 单次 recall 的聚合。
type JevRecallStats struct {
	Count        int     `json:"count"`
	ConfidenceAvg float64 `json:"confidence_avg"`
	ConfidenceP50 float64 `json:"confidence_p50"`
	ConfidenceP95 float64 `json:"confidence_p95"`
}

// JevQualityStats quality_gate 的聚合：核心对照是 length_conf（长度先验）vs jev_conf（真分数）。
type JevQualityStats struct {
	Count int `json:"count"`
	// WithJevScore 是拿到真分数的条数（fell_back=false）。
	WithJevScore int `json:"with_jev_score"`
	// FellBackCount 是 fail-open 条数。
	FellBackCount int `json:"fell_back_count"`

	// LengthConfAvg / JevConfAvg 是两个口径的平均分（0..1）。
	LengthConfAvg float64 `json:"length_conf_avg"`
	JevConfAvg    float64 `json:"jev_conf_avg"`
	// DivergenceAvg 是 |length_conf - jev_conf| 的平均值——越大说明长度先验离真分数越远。
	DivergenceAvg float64 `json:"divergence_avg"`

	// ByRole 按角色切片的对照。
	ByRole map[string]JevQualityRoleStats `json:"by_role"`
}

// JevQualityRoleStats 单角色切片。
// 平均分仅对 fell_back=false 的样本计算；全 FB 时 *_avg = 0 表示"无真分数样本"，
// 前端应以 count vs fell_back 判断"是否展示均值"。
type JevQualityRoleStats struct {
	Count         int     `json:"count"`
	LengthConfAvg float64 `json:"length_conf_avg"`
	JevConfAvg    float64 `json:"jev_conf_avg"`
	FellBack      int     `json:"fell_back"`
}

// JevLatencyStats 延迟分位数（毫秒）。
type JevLatencyStats struct {
	Count int   `json:"count"`
	P50   int64 `json:"p50"`
	P95   int64 `json:"p95"`
	Max   int64 `json:"max"`
}

// jevRecord 是 decision 流水的最小解析形态——只取聚合所需字段，其余 extra 原样保留。
type jevRecord struct {
	Kind      string                 `json:"kind"`
	Model     string                 `json:"model"`
	At        string                 `json:"at"`
	LatencyMS int64                  `json:"latency_ms"`
	FellBack  bool                   `json:"fell_back"`
	Extra     map[string]interface{} `json:"extra"`
	Raw       json.RawMessage        `json:"-"`
}

// handleJevStats GET /api/jev/stats?n=500 — 拉取 agentDB decision 流水并聚合。
//
// 查询参数：
//
//	n  最大拉取条数（默认 500，上限 5000）。agentDB 目前是全量返回，本参数是
//	   前端「只看最近 N 条」的窗口，不是服务端分页。
//
// 失败语义：agentDB 不可达返回 502 + 错误体；决策 URL 未配返回 501。
// 这与 dashboard 其它写接口的 501 纪律一致（nil 依赖不假装有数据）。
func (s *Server) handleJevStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("仅支持 GET"))
		return
	}
	agentDB := strings.TrimRight(os.Getenv("CLAUDE_GO_AGENTDB_URL"), "/")
	if agentDB == "" {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("CLAUDE_GO_AGENTDB_URL 未配置，Jev 决策流水无落点"))
		return
	}
	n := parseIntQuery(r, "n", 500)
	if n <= 0 || n > 5000 {
		n = 500
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentDB+"/v1/log/decision", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("agentDB 不可达: %w", err))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("读取 agentDB 响应失败: %w", err))
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, fmt.Errorf("agentDB 返回 %d", resp.StatusCode))
		return
	}

	var payload struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("agentDB 响应解析失败: %w", err))
		return
	}

	// 逐行 base64 → JSON → jevRecord。解析失败的行跳过（不静默丢弃：记入 ByKind["_parse_err"]）。
	recs := make([]jevRecord, 0, len(payload.Lines))
	parseErr := 0
	for _, line := range payload.Lines {
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			parseErr++
			continue
		}
		var jr jevRecord
		if err := json.Unmarshal(raw, &jr); err != nil {
			parseErr++
			continue
		}
		if jr.Kind == "" {
			// 非 decision 行（如手写探针），跳过。
			continue
		}
		jr.Raw = json.RawMessage(raw)
		recs = append(recs, jr)
	}

	stats := aggregateJevStats(recs, n)
	stats.AgentDB = agentDB
	if parseErr > 0 {
		stats.ByKind["_parse_err"] = parseErr
	}
	writeJSON(w, http.StatusOK, stats)
}

// aggregateJevStats 在内存里聚合流水。recs 按 at 升序假定（agentDB 是追加顺序）。
func aggregateJevStats(recs []jevRecord, recentN int) *JevStatsResponse {
	out := &JevStatsResponse{
		ByKind:   map[string]int{},
		ByModel:  map[string]int{},
		FellBack: map[string]int{},
		Latency:  map[string]JevLatencyStats{},
	}

	latByKind := map[string][]int64{}
	confidences := []float64{}

	var dualAgree, dualCount int
	var overlapSum, top1MatchSum, uniqueJevSum float64

	var qgCount, qgWithJev, qgFB int
	var qgLenSum, qgJevSum, qgDivSum float64
	qgByRole := map[string]*JevQualityRoleStats{}

	var earliest time.Time

	for _, r := range recs {
		out.Window++
		out.ByKind[r.Kind]++
		if r.Model != "" {
			out.ByModel[r.Model]++
		}
		if r.FellBack {
			out.FellBack[r.Kind]++
		}
		if r.At != "" {
			if t, err := time.Parse(time.RFC3339Nano, r.At); err == nil {
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
			}
		}
		if r.LatencyMS > 0 {
			latByKind[r.Kind] = append(latByKind[r.Kind], r.LatencyMS)
		}

		switch r.Kind {
		case "recall_dual":
			dualCount++
			bm25 := strSlice(r.Extra["bm25_picks"])
			jev := strSlice(r.Extra["jev_picks"])
			if equalStrSlice(bm25, jev) {
				dualAgree++
			}
			overlapSum += jaccard(bm25, jev)
			if len(bm25) > 0 && len(jev) > 0 && bm25[0] == jev[0] {
				top1MatchSum++
			}
			uniqueJevSum += float64(len(diff(jev, bm25))) / float64(max(1, len(jev)))

		case "recall":
			if c, ok := r.Extra["jev_confidence"].(float64); ok {
				confidences = append(confidences, c)
			}

		case "quality_gate":
			qgCount++
			role, _ := r.Extra["role"].(string)
			if role == "" {
				role = "_unknown"
			}
			rs := qgByRole[role]
			if rs == nil {
				rs = &JevQualityRoleStats{}
				qgByRole[role] = rs
			}
			rs.Count++

			lc, _ := r.Extra["length_conf"].(float64)
			if r.FellBack {
				qgFB++
				rs.FellBack++
			} else if jc, ok := r.Extra["jev_conf"].(float64); ok {
				qgWithJev++
				qgLenSum += lc
				qgJevSum += jc
				d := lc - jc
				if d < 0 {
					d = -d
				}
				qgDivSum += d

				rs.LengthConfAvg = (rs.LengthConfAvg*float64(rs.Count-rs.FellBack-1) + lc) / float64(rs.Count-rs.FellBack)
				rs.JevConfAvg = (rs.JevConfAvg*float64(rs.Count-rs.FellBack-1) + jc) / float64(rs.Count-rs.FellBack)
			}
		}
	}

	if !earliest.IsZero() {
		out.Since = earliest.UTC().Format(time.RFC3339)
	}

	// recall_dual 聚合
	if dualCount > 0 {
		out.RecallDual = JevRecallDualStats{
			Count:          dualCount,
			AgreeCount:     dualAgree,
			OverlapAvg:     overlapSum / float64(dualCount),
			Top1MatchRate:  top1MatchSum / float64(dualCount),
			UniqueToJevAvg: uniqueJevSum / float64(dualCount),
		}
	}

	// recall 置信度
	if len(confidences) > 0 {
		sort.Float64s(confidences)
		out.Recall = JevRecallStats{
			Count:         len(confidences),
			ConfidenceAvg: avg(confidences),
			ConfidenceP50: percentile(confidences, 0.5),
			ConfidenceP95: percentile(confidences, 0.95),
		}
	}

	// quality_gate 聚合
	if qgCount > 0 {
		out.QualityGate = JevQualityStats{
			Count:         qgCount,
			WithJevScore:  qgWithJev,
			FellBackCount: qgFB,
			ByRole:        map[string]JevQualityRoleStats{},
		}
		if qgWithJev > 0 {
			out.QualityGate.LengthConfAvg = qgLenSum / float64(qgWithJev)
			out.QualityGate.JevConfAvg = qgJevSum / float64(qgWithJev)
			out.QualityGate.DivergenceAvg = qgDivSum / float64(qgWithJev)
		}
		for role, rs := range qgByRole {
			out.QualityGate.ByRole[role] = *rs
		}
	}

	// 延迟分位数
	for kind, lats := range latByKind {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		out.Latency[kind] = JevLatencyStats{
			Count: len(lats),
			P50:   lats[len(lats)/2],
			P95:   lats[int(float64(len(lats))*0.95+0.5)-1],
			Max:   lats[len(lats)-1],
		}
	}

	// 最近 N 条（倒序：最新的在前）
	start := 0
	if len(recs) > recentN {
		start = len(recs) - recentN
	}
	out.Recent = make([]json.RawMessage, 0, recentN)
	for i := len(recs) - 1; i >= start; i-- {
		out.Recent = append(out.Recent, recs[i].Raw)
	}

	return out
}

// ── 小工具 ──

func strSlice(v interface{}) []string {
	xs, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// jaccard 计算两个字符串集合的交并比（|A∩B| / |A∪B|）。
func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	setA := map[string]struct{}{}
	for _, x := range a {
		setA[x] = struct{}{}
	}
	setB := map[string]struct{}{}
	for _, x := range b {
		setB[x] = struct{}{}
	}
	inter := 0
	for x := range setA {
		if _, ok := setB[x]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

// diff 返回在 a 但不在 b 的元素。
func diff(a, b []string) []string {
	setB := map[string]struct{}{}
	for _, x := range b {
		setB[x] = struct{}{}
	}
	out := []string{}
	for _, x := range a {
		if _, ok := setB[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}

func avg(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// percentile 已排序切片的分位数（线性插值）。p ∈ [0,1]。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
