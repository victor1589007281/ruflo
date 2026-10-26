package report

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

func TestRenderModelViewKeepsReasoningDataButNotRawReplay(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	b := &evidence.Bundle{
		IncidentID: "inc_test", Instance: "host-a", Domain: "linux", GeneratedAt: now,
		Window: evidence.TimeWindow{Start: now.Add(-30 * time.Minute), End: now},
		Collection: evidence.CollectionInfo{
			Sampling: &evidence.Sampling{Samples: 3, IntervalSeconds: 1, SpanSeconds: 2, WindowSeconds: 1800,
				Note: "lookback does not extend samples"},
			Capabilities: []evidence.Capability{{Name: "unit_logs", Availability: "manual", Detail: "operator must collect"}},
		},
		Timeline: []evidence.TimelinePoint{{At: now, Metrics: map[string]float64{"timeline_only": 123}}},
		Evidences: []evidence.Evidence{{
			ID: "ev_real", Type: evidence.TypeMetric, Source: "/proc/test", CollectedAt: now,
			Subject: evidence.Subject{Kind: "host", Name: "host-a"}, Grade: evidence.GradeE2,
			Reliability: 0.9, Summary: "current reading", RawPreview: "raw_preview_must_stay_in_artifact",
			ProbeValid: true, ProbeNote: "two-second sample",
		}},
		Hypotheses: []evidence.Hypothesis{{
			ID: "L1", Statement: "candidate", Predicts: []string{"prediction"}, Disconfirms: []string{"refutation"},
			Support: []string{"ev_real"}, Status: evidence.HypothesisInconcl, Confidence: 0.4,
			TestPlan: []string{"next probe"},
		}},
		Contradictions: []evidence.Contradiction{{
			ID: "LC1", Description: "different caliber", EvidenceA: "ev_real", EvidenceB: "ev_real", Severity: "medium",
		}},
		Missing: []evidence.Missing{{What: "window curve", Why: "short sample", NextAction: "manual monitoring"}},
		Summary: "no eligible root cause",
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	got := RenderModelView(b)
	for _, want := range []string{
		"ev_real", "current reading", "two-second sample", "root_cause_gate: not_passed",
		"samples=3", "span=2.0s", "lookback=1800.0s", "availability=manual",
		"id=L1", "prediction", "refutation", "next probe", "id=LC1", "unresolved",
		"window curve", "short sample", "manual monitoring",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("compact view lost %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"raw_preview_must_stay_in_artifact", "timeline_only", "## 证据"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("compact view should not replay %q:\n%s", unwanted, got)
		}
	}
}

func TestRenderMarkdownLabelsCollectionCapabilities(t *testing.T) {
	b := &evidence.Bundle{Collection: evidence.CollectionInfo{Capabilities: []evidence.Capability{
		{Name: "snapshot", Availability: "tool", Detail: "fixed"},
		{Name: "logs", Availability: "manual", Detail: "operator"},
		{Name: "arbitrary", Availability: "unsupported", Detail: "not available"},
	}}}
	got := RenderMarkdown("Test", b, nil)
	for _, want := range []string{"[工具支持] snapshot", "[需人工] logs", "[采集面不支持] arbitrary"} {
		if !strings.Contains(got, want) {
			t.Errorf("full report missing %q:\n%s", want, got)
		}
	}
}
