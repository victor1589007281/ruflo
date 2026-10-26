package mysql

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

var testNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)

func testCollector() *Collector {
	return &Collector{
		cfg: Config{Instance: "db1", Window: 30 * time.Minute, Samples: 3,
			Now: func() time.Time { return testNow }},
		now: func() time.Time { return testNow },
	}
}

// baseSnapshot 是一个健康实例的完整采集结果；各用例只改需要触发的字段。
func baseSnapshot() *snapshot {
	t0 := testNow.Add(-4 * time.Second)
	t1 := testNow.Add(-2 * time.Second)
	mk := func(at time.Time, conn, running, queries float64) statusSample {
		return statusSample{at: at, values: map[string]float64{
			"threads_connected": conn, "threads_running": running,
			"queries": queries, "questions": queries,
			"max_used_connections": conn + 5,
			"aborted_connects":     0, "connection_errors_max_connections": 0,
			"innodb_buffer_pool_read_requests": 1_000_000, "innodb_buffer_pool_reads": 100,
			"innodb_row_lock_current_waits": 0, "innodb_row_lock_waits": 0,
		}}
	}
	return &snapshot{
		window: evidence.TimeWindow{Start: testNow.Add(-30 * time.Minute), End: testNow},
		samples: []statusSample{
			mk(t0, 100, 3, 10_000),
			mk(t1, 100, 3, 10_100),
			mk(testNow, 100, 3, 10_200),
		},
		variables: map[string]string{
			"version": "8.4.0", "max_connections": "500",
			"innodb_buffer_pool_size": "1073741824", "slow_query_log": "ON",
			"long_query_time": "1", "read_only": "OFF", "log_output": "FILE",
			"log_error": "stderr", "performance_schema": "ON",
			"connection_errors_max_connections": "0",
		},
		process: []processRow{
			{User: "app", Host: "10.0.0.5:5000", DB: "test", Command: "Sleep", State: "", Count: 20, MaxTime: 10},
			{User: "app", Host: "10.0.0.5:5000", DB: "test", Command: "Query", State: "executing", Count: 2, MaxTime: 1},
		},
		procValid:     true,
		lockValid:     true,
		mdlValid:      true,
		trxValid:      true,
		trx:           []trxRow{{TrxID: "1", State: "RUNNING", AgeSeconds: 1}},
		replValid:     true,
		digValid:      true,
		digest:        []digestRow{{Schema: "test", Digest: "SELECT ?", Count: 10, TotalPS: 10_000_000, RowsExam: 10, RowsSent: 10}},
		capValid:      true,
		capTotals:     capacityTotals{Data: 1 << 30, Index: 1 << 28, Free: 0, Tables: 10},
		errLogOK:      true,
		deadlockValid: true,
	}
}

func evidenceByID(b *evidence.Bundle, id string) *evidence.Evidence { return b.EvidenceByID(id) }

func TestEvidenceCaliberAndSystemProcessSeparation(t *testing.T) {
	s := baseSnapshot()
	s.process = append([]processRow{{User: "event_scheduler", Command: "Daemon", Count: 1, MaxTime: 999999999}}, s.process...)
	s.capTotals = capacityTotals{}
	b := testCollector().buildBundle(s)

	digest := evidenceByID(b, "ev_slow_digest")
	if digest == nil || !strings.Contains(digest.Summary, "0.001ms") ||
		!strings.Contains(digest.Summary, "schema=test") || !strings.Contains(digest.Summary, "累计") {
		t.Fatalf("digest must retain sub-ms precision and cumulative/schema caliber: %+v", digest)
	}
	if !strings.Contains(digest.ProbeNote, "不是 window_minutes 增量") {
		t.Fatalf("digest window caliber missing: %q", digest.ProbeNote)
	}
	capacity := evidenceByID(b, "ev_capacity")
	if capacity == nil || !strings.Contains(capacity.Summary, "非系统库合计：0 张表") ||
		!strings.Contains(capacity.ProbeNote, "0 表/0B 表示该作用域为空") {
		t.Fatalf("empty non-system scope must not read as an instance anomaly: %+v", capacity)
	}
	process := evidenceByID(b, "ev_processlist_sleep")
	if process == nil || !strings.Contains(process.Summary, "非系统会话 22 个") ||
		!strings.Contains(process.Summary, "系统线程 1 个") || !strings.Contains(process.Summary, "最大非系统分组 app@") {
		t.Fatalf("event_scheduler must not dominate business process groups: %+v", process)
	}
	if b.Collection.Sampling == nil || b.Collection.Sampling.SpanSeconds != 4 ||
		b.Collection.Sampling.IntervalSeconds != 2 || b.Collection.Sampling.WindowSeconds != 1800 {
		t.Fatalf("actual sampling caliber missing: %+v", b.Collection.Sampling)
	}
	traffic := evidenceByID(b, "ev_traffic")
	if traffic == nil || !strings.Contains(traffic.Summary, "首末样本跨度 4.0s") ||
		!strings.Contains(traffic.Summary, "不是整个 window_minutes") {
		t.Fatalf("QPS must use sample-span caliber: %+v", traffic)
	}
}

func TestAnalyze_ConnectionExhaustionBecomesRootCause(t *testing.T) {
	s := baseSnapshot()
	s.samples[0].values["threads_connected"] = 200
	s.samples[1].values["threads_connected"] = 400
	s.samples[2].values["threads_connected"] = 465
	s.process = []processRow{
		{User: "batch", Host: "10.0.0.9:5000", DB: "test", Command: "Sleep", Count: 120, MaxTime: 900},
		{User: "app", Host: "10.0.0.5:5000", DB: "test", Command: "Query", State: "executing", Count: 10, MaxTime: 2},
	}
	s.errorLog = []errorRow{
		{At: testNow.Add(-time.Minute), Prio: "ERROR", Code: "1040", Subsys: "Server", Message: "Too many connections"},
		{At: testNow.Add(-2 * time.Minute), Prio: "ERROR", Code: "1040", Subsys: "Server", Message: "Too many connections"},
	}

	b := testCollector().buildBundle(s)
	analyze(b, s)

	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	conn := evidenceByID(b, "ev_connections")
	if conn == nil || conn.Grade != evidence.GradeE3 {
		t.Fatalf("connection evidence should be E3, got %+v", conn)
	}
	h1 := findHypothesis(b, "H1")
	if h1 == nil || h1.Status != evidence.HypothesisSupported || !h1.RootCause {
		t.Fatalf("H1 should be supported root cause, got %+v", h1)
	}
	if ok, why := b.RootCauseEligible(*h1); !ok {
		t.Fatalf("H1 should pass root-cause gate: %v", why)
	}
	h2 := findHypothesis(b, "H2")
	if h2 == nil || h2.Status != evidence.HypothesisRefuted {
		t.Fatalf("traffic hypothesis should be refuted, got %+v", h2)
	}
	if !strings.Contains(b.Summary, "满足门控") {
		t.Fatalf("summary should mention eligible root cause: %s", b.Summary)
	}
}

func TestAnalyze_Error1040WithoutCurrentPressureStaysUnresolved(t *testing.T) {
	s := baseSnapshot()
	s.errorLog = []errorRow{
		{At: testNow.Add(-time.Minute), Prio: "ERROR", Code: "1040", Subsys: "Server", Message: "Too many connections"},
	}

	b := testCollector().buildBundle(s)
	analyze(b, s)

	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	h1 := findHypothesis(b, "H1")
	if h1 == nil || h1.Status != evidence.HypothesisInconcl {
		t.Fatalf("H1 should be inconclusive without E3 support, got %+v", h1)
	}
	if h1.RootCause {
		t.Fatal("inconclusive hypothesis must not be marked root cause")
	}
	var high bool
	for _, c := range b.Contradictions {
		if c.Severity == "high" && c.Resolution == "" {
			high = true
		}
	}
	if !high {
		t.Fatal("1040 vs low current connections should be an unresolved high contradiction")
	}
}

func TestAnalyze_LockWaitBecomesRootCause(t *testing.T) {
	s := baseSnapshot()
	s.lockWaits = 2
	s.lockDetails = []lockRow{{
		WaitingTrx: "200", BlockingTrx: "100", Schema: "test", Table: "orders",
		LockType: "RECORD", LockMode: "X",
	}}
	s.trx = []trxRow{{TrxID: "100", State: "RUNNING", AgeSeconds: 75, ThreadID: 42, RowsLocked: 500}}

	b := testCollector().buildBundle(s)
	analyze(b, s)

	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	if findHypothesis(b, "H1") != nil {
		t.Fatal("no connection hypothesis should be generated for a lock-only incident")
	}
	h3 := findHypothesis(b, "H3")
	if h3 == nil || h3.Status != evidence.HypothesisSupported || !h3.RootCause {
		t.Fatalf("H3 should be supported root cause, got %+v", h3)
	}
	if ok, why := b.RootCauseEligible(*h3); !ok {
		t.Fatalf("H3 should pass gate: %v", why)
	}
}

func TestAnalyze_RecentDeadlockIsE4RootCause(t *testing.T) {
	s := baseSnapshot()
	s.deadlock = &deadlockInfo{
		At: testNow.Add(-time.Minute), Recent: true, HasDate: true,
		Excerpt: "LATEST DETECTED DEADLOCK\n2026-10-26 11:59:00",
	}

	b := testCollector().buildBundle(s)
	analyze(b, s)

	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	dl := evidenceByID(b, "ev_deadlock")
	if dl == nil || dl.Grade != evidence.GradeE4 {
		t.Fatalf("recent deadlock should be E4, got %+v", dl)
	}
	h7 := findHypothesis(b, "H7")
	if h7 == nil || !h7.RootCause {
		t.Fatalf("H7 should be a root cause, got %+v", h7)
	}
	if ok, why := b.RootCauseEligible(*h7); !ok {
		t.Fatalf("H7 should pass gate: %v", why)
	}
}

func TestAnalyze_HealthyInstanceHasNoRootCause(t *testing.T) {
	s := baseSnapshot()
	b := testCollector().buildBundle(s)
	analyze(b, s)

	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	if len(b.Hypotheses) != 0 {
		t.Fatalf("healthy instance should not trigger hypotheses, got %+v", b.Hypotheses)
	}
	if !strings.Contains(b.Summary, "未发现满足 E3 门控的根因") {
		t.Fatalf("unexpected summary: %s", b.Summary)
	}
	md := RenderMarkdown(b)
	if !strings.Contains(md, "没有假设通过根因门控") {
		t.Fatal("report must explicitly state the gate was not passed")
	}
}

func TestReport_CitesEveryEvidenceID(t *testing.T) {
	s := baseSnapshot()
	s.lockWaits = 2
	s.trx = []trxRow{{TrxID: "100", State: "RUNNING", AgeSeconds: 75, RowsLocked: 500}}
	b := testCollector().buildBundle(s)
	analyze(b, s)
	md := RenderMarkdown(b)
	for _, e := range b.Evidences {
		if !strings.Contains(md, e.ID) {
			t.Errorf("report does not cite evidence %s", e.ID)
		}
	}
}

func findHypothesis(b *evidence.Bundle, id string) *evidence.Hypothesis {
	for i := range b.Hypotheses {
		if b.Hypotheses[i].ID == id {
			return &b.Hypotheses[i]
		}
	}
	return nil
}

func TestAnalyze_SelfInducedConnectionsCannotBecomeRootCause(t *testing.T) {
	s := baseSnapshot()
	s.probeUser, s.probeUserValid = "ops_rca_ro", true
	s.variables["max_connections"] = "151"
	s.samples[0].values["threads_connected"] = 131
	s.samples[1].values["threads_connected"] = 131
	s.samples[2].values["threads_connected"] = 131
	s.process = []processRow{
		{User: "ops_rca_ro", Host: "172.17.0.1:5000", DB: "test", Command: "Sleep", Count: 130, MaxTime: 60},
		{User: "event_scheduler", Host: "localhost", Command: "Daemon", State: "Waiting on empty queue", Count: 1},
	}

	b := testCollector().buildBundle(s)
	analyze(b, s)

	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	conn := evidenceByID(b, "ev_connections")
	if conn == nil || conn.Grade == evidence.GradeE3 {
		t.Fatalf("self-induced pressure must not be graded E3, got %+v", conn)
	}
	h1 := findHypothesis(b, "H1")
	if h1 == nil || h1.RootCause {
		t.Fatalf("self-induced pressure must not be a root cause, got %+v", h1)
	}
	var c5 bool
	for _, c := range b.Contradictions {
		if c.ID == "C5" && c.Severity == "high" && c.Resolution == "" {
			c5 = true
		}
	}
	if !c5 {
		t.Fatalf("expected unresolved high contradiction C5, got %+v", b.Contradictions)
	}
	md := RenderMarkdown(b)
	if strings.Contains(md, "### 根因（已过门控）") {
		t.Fatal("report must not claim a gated root cause for self-induced pressure")
	}
	if !strings.Contains(md, "观测侧") {
		t.Fatal("report should surface the self-induced contradiction")
	}
}
