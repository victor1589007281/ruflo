package linux

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

var testNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)

func testCollector() *Collector {
	return &Collector{
		cfg: Config{Target: "host1", Window: 30 * time.Minute, Samples: 3, Now: func() time.Time { return testNow }},
		now: func() time.Time { return testNow },
	}
}

func baseSnapshot() *snapshot {
	s := &snapshot{
		hostname: "host1", kernel: "7.0.0-test", osRelease: "Ubuntu Test", cpuCount: 8,
		uptimeSeconds: 3600,
		fs: []fsRow{{
			Device: "/dev/sda1", Mount: "/", Size: 100 << 30, Used: 40 << 30, Avail: 60 << 30,
			UsedRatio: 0.4, Inodes: 1_000_000, InodesUsed: 100_000, InodeRatio: 0.1, InodeValid: true,
		}},
		fsValid:         true,
		fileNrAllocated: 1024, fileMax: 10_000_000, fdValid: true,
		failedValid: true,
		kernelValid: true,
		sockets:     "Total: 100\nTCP: 50", socketValid: true,
	}
	for i := 0; i < 3; i++ {
		s.samples = append(s.samples, statusSample{
			at:        testNow.Add(time.Duration(i-2) * time.Second),
			loadValid: true, memValid: true, procValid: true, psiValid: true,
			load1: 0.5, load5: 0.5, load15: 0.5,
			memTotal: 16 << 30, memAvailable: 8 << 30,
			swapTotal: 4 << 30, swapFree: 4 << 30,
		})
	}
	return s
}

func build(s *snapshot) *evidence.Bundle {
	b := testCollector().buildBundle(s)
	analyze(b, s)
	return b
}

func findHypothesis(b *evidence.Bundle, id string) *evidence.Hypothesis {
	for i := range b.Hypotheses {
		if b.Hypotheses[i].ID == id {
			return &b.Hypotheses[i]
		}
	}
	return nil
}

func TestCollectionSamplingAndManualBoundaries(t *testing.T) {
	b := build(baseSnapshot())
	s := b.Collection.Sampling
	if s == nil || s.Samples != 3 || s.IntervalSeconds != 1 || s.SpanSeconds != 2 || s.WindowSeconds != 1800 {
		t.Fatalf("sampling must report actual first/last span, not lookback: %+v", s)
	}
	var unitLogs bool
	for _, c := range b.Collection.Capabilities {
		if c.Name == "unit_logs" && c.Availability == "manual" && strings.Contains(c.Detail, "不支持 journalctl -u") {
			unitLogs = true
		}
	}
	if !unitLogs {
		t.Fatalf("unit logs must be manual, not an implied collector capability: %+v", b.Collection.Capabilities)
	}
	var trendGap, serviceGap bool
	for _, m := range b.Missing {
		if strings.Contains(m.What, "覆盖完整窗口") && strings.Contains(m.NextAction, "需人工") &&
			strings.Contains(m.NextAction, "window_minutes 不会延长采样") {
			trendGap = true
		}
		if strings.Contains(m.What, "应用/服务日志") && strings.Contains(m.NextAction, "当前不支持 unit 日志") {
			serviceGap = true
		}
	}
	if !trendGap || !serviceGap {
		t.Fatalf("missing actions must expose manual boundaries: trend=%v service=%v %+v", trendGap, serviceGap, b.Missing)
	}
}

func TestAnalyze_HealthyHostHasNoRootCause(t *testing.T) {
	b := build(baseSnapshot())
	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	if len(b.Hypotheses) != 0 {
		t.Fatalf("healthy host should not trigger hypotheses: %+v", b.Hypotheses)
	}
	if !strings.Contains(b.Summary, "未发现满足 E3 门控的根因") {
		t.Fatalf("unexpected summary: %s", b.Summary)
	}
	md := RenderMarkdown(b)
	if !strings.Contains(md, "没有假设通过根因门控") {
		t.Fatal("report must explicitly state the gate was not passed")
	}
}

func TestAnalyze_DiskExhaustionBecomesRootCause(t *testing.T) {
	s := baseSnapshot()
	s.fs[0].UsedRatio = 0.96
	b := build(s)
	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	disk := b.EvidenceByID("ev_host_disk")
	if disk == nil || disk.Grade != evidence.GradeE3 {
		t.Fatalf("disk evidence should be E3, got %+v", disk)
	}
	h := findHypothesis(b, "L3")
	if h == nil || !h.RootCause {
		t.Fatalf("L3 should be root cause, got %+v", h)
	}
	if ok, why := b.RootCauseEligible(*h); !ok {
		t.Fatalf("L3 should pass gate: %v", why)
	}
}

func TestAnalyze_InodeExhaustionBecomesRootCause(t *testing.T) {
	s := baseSnapshot()
	s.fs[0].InodeRatio = 0.97
	b := build(s)
	h := findHypothesis(b, "L4")
	if h == nil || !h.RootCause {
		t.Fatalf("L4 should be root cause, got %+v", h)
	}
}

func TestAnalyze_CPUPressureNeedsFusion(t *testing.T) {
	s := baseSnapshot()
	for i := range s.samples {
		s.samples[i].load1 = 20
		s.samples[i].psiCPU = 35
	}
	b := build(s)
	load := b.EvidenceByID("ev_host_load")
	if load == nil || load.Grade != evidence.GradeE3 {
		t.Fatalf("load should be E3 when load and PSI agree, got %+v", load)
	}
	h := findHypothesis(b, "L1")
	if h == nil || !h.RootCause {
		t.Fatalf("L1 should be root cause, got %+v", h)
	}

	// 只有 load 高、PSI 低：只能到 E2，不能定因。
	s2 := baseSnapshot()
	for i := range s2.samples {
		s2.samples[i].load1 = 20
		s2.samples[i].psiCPU = 1
	}
	b2 := build(s2)
	h2 := findHypothesis(b2, "L1")
	if h2 == nil || h2.RootCause {
		t.Fatalf("load-only signal must not be a root cause, got %+v", h2)
	}
}

func TestAnalyze_OOMKillIsE4RootCause(t *testing.T) {
	s := baseSnapshot()
	s.kernelEvents = []kernelEvent{{
		At: testNow.Add(-time.Minute), Kind: "oom", Process: "mysqld", PID: 4242,
		Line: "Oct 26 11:59:00 host kernel: Out of memory: Killed process 4242 (mysqld)",
	}}
	b := build(s)
	klog := b.EvidenceByID("ev_host_kernel_log")
	if klog == nil || klog.Grade != evidence.GradeE4 {
		t.Fatalf("OOM log should be E4, got %+v", klog)
	}
	h := findHypothesis(b, "L6")
	if h == nil || !h.RootCause {
		t.Fatalf("L6 should be root cause, got %+v", h)
	}
	if !strings.Contains(h.Statement, "mysqld") {
		t.Fatalf("statement should name the killed process: %s", h.Statement)
	}
}

func TestAnalyze_ProbeFailureIsNotZero(t *testing.T) {
	s := baseSnapshot()
	for i := range s.samples {
		s.samples[i].psiValid = false
	}
	s.kernelValid = false
	s.failedValid = false
	b := build(s)
	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	for _, id := range []string{"ev_host_pressure", "ev_host_kernel_log", "ev_host_failed_units"} {
		e := b.EvidenceByID(id)
		if e == nil || e.ProbeValid {
			t.Fatalf("%s must be marked probe_valid=false, got %+v", id, e)
		}
	}
	var missingPSI, missingKernel bool
	for _, m := range b.Missing {
		if strings.Contains(m.What, "PSI") {
			missingPSI = true
		}
		if strings.Contains(m.What, "内核日志") {
			missingKernel = true
		}
	}
	if !missingPSI || !missingKernel {
		t.Fatalf("probe failures must appear in missing: %+v", b.Missing)
	}
}

func TestAnalyze_FailedUnitIsInconclusive(t *testing.T) {
	s := baseSnapshot()
	s.failedUnits = []failedUnit{{Unit: "mysql.service", Load: "loaded", Active: "failed", Sub: "failed"}}
	b := build(s)
	h := findHypothesis(b, "L8")
	if h == nil || h.Status != evidence.HypothesisInconcl || h.RootCause {
		t.Fatalf("failed unit should stay inconclusive, got %+v", h)
	}
}
