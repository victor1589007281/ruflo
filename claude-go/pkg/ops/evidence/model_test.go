package evidence

import (
	"encoding/json"
	"testing"
	"time"
)

func validEvidence(id string, grade Grade) Evidence {
	return Evidence{
		ID: id, Type: TypeSignal, Source: "test", CollectedAt: time.Now(),
		Subject: Subject{Kind: "instance", Name: "db1"},
		Grade:   grade, Reliability: 0.9, Summary: "ok", ProbeValid: true,
	}
}

func validHypothesis(id string, support ...string) Hypothesis {
	return Hypothesis{
		ID: id, Statement: "s", Predicts: []string{"p"}, Disconfirms: []string{"d"},
		Support: support, Status: HypothesisSupported, Confidence: 0.8,
	}
}

func TestRootCauseEligible_RequiresE3(t *testing.T) {
	b := &Bundle{Evidences: []Evidence{
		validEvidence("ev1", GradeE2),
		validEvidence("ev2", GradeE3),
	}}
	h := validHypothesis("H1", "ev1")
	if ok, why := b.RootCauseEligible(h); ok {
		t.Fatalf("E2-only support must not be eligible: %v", why)
	}
	h.Support = []string{"ev1", "ev2"}
	if ok, why := b.RootCauseEligible(h); !ok {
		t.Fatalf("E3 support should be eligible: %v", why)
	}
}

func TestRootCauseEligible_InvalidProbeDoesNotCount(t *testing.T) {
	e := validEvidence("ev1", GradeE3)
	e.ProbeValid = false
	b := &Bundle{Evidences: []Evidence{e}}
	if ok, _ := b.RootCauseEligible(validHypothesis("H1", "ev1")); ok {
		t.Fatal("invalid probe must not count as evidence")
	}
}

func TestRootCauseEligible_HighUnresolvedContradictionBlocks(t *testing.T) {
	b := &Bundle{
		Evidences: []Evidence{validEvidence("ev1", GradeE3), validEvidence("ev2", GradeE3)},
		Contradictions: []Contradiction{{
			ID: "C1", Description: "conflict", EvidenceA: "ev1", EvidenceB: "ev2", Severity: "high",
		}},
	}
	if ok, why := b.RootCauseEligible(validHypothesis("H1", "ev1")); ok {
		t.Fatalf("unresolved high contradiction must block: %v", why)
	}
	// 同一矛盾被解释后不再阻断。
	b.Contradictions[0].Resolution = "explained by probe timing"
	if ok, why := b.RootCauseEligible(validHypothesis("H1", "ev1")); !ok {
		t.Fatalf("resolved contradiction should not block: %v", why)
	}
}

func TestBundleValidate_RejectsDanglingReference(t *testing.T) {
	b := &Bundle{
		Evidences:  []Evidence{validEvidence("ev1", GradeE3)},
		Hypotheses: []Hypothesis{validHypothesis("H1", "ev_missing")},
	}
	if err := b.Validate(); err == nil {
		t.Fatal("dangling evidence reference must fail validation")
	}
}

func TestBundleValidate_RejectsUnfalsifiableHypothesis(t *testing.T) {
	h := validHypothesis("H1")
	h.Disconfirms = nil
	b := &Bundle{Evidences: []Evidence{validEvidence("ev1", GradeE3)}, Hypotheses: []Hypothesis{h}}
	if err := b.Validate(); err == nil {
		t.Fatal("hypothesis without disconfirms must fail validation")
	}
}

func TestBundleMarshal_NilSlicesBecomeArrays(t *testing.T) {
	out, err := json.Marshal(&Bundle{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"evidences", "hypotheses", "timeline", "contradictions", "missing"} {
		if _, ok := m[k].([]any); !ok {
			t.Errorf("%s should marshal as [] not %T (%v)", k, m[k], m[k])
		}
	}
}
