package hooks

import (
	"math"
	"strings"
	"testing"
)

func TestRecordOutcome(t *testing.T) {
	t.Parallel()
	rb := NewReasoningBank()
	p := &GuidancePattern{ID: "p1", Quality: 0.5, Embedding: []float32{1, 0, 0}}
	stored, err := rb.StorePattern(p)
	if err != nil || stored == nil {
		t.Fatalf("StorePattern: %v %#v", err, stored)
	}
	id := stored.ID
	if err := rb.RecordOutcome(id, true); err != nil {
		t.Fatal(err)
	}
	if err := rb.RecordOutcome(id, false); err != nil {
		t.Fatal(err)
	}
	var found *GuidancePattern
	for _, x := range rb.ExportPatterns() {
		if x.ID == id {
			found = &x
			break
		}
	}
	if found == nil || found.UsageCount != 2 || found.SuccessCount != 1 {
		t.Fatalf("counts: %#v", found)
	}
}

func TestConsolidate(t *testing.T) {
	t.Parallel()
	rb := NewReasoningBank()
	// cosine ~0.925 (< dedup threshold 0.95) so both rows are kept by StorePattern
	v1 := []float32{1, 0, 0}
	v2 := []float32{0.92, 0.38, 0}
	_, _ = rb.StorePattern(&GuidancePattern{ID: "a", Embedding: append([]float32(nil), v1...), Quality: 0.7, UsageCount: 1})
	_, _ = rb.StorePattern(&GuidancePattern{ID: "b", Embedding: append([]float32(nil), v2...), Quality: 0.6, UsageCount: 2})
	if rb.GetStats().TotalPatterns != 2 {
		t.Fatal("expected two patterns before consolidate")
	}
	merged := rb.Consolidate(0.90)
	if merged < 1 {
		t.Fatalf("expected merge, got %d", merged)
	}
	if rb.GetStats().TotalPatterns != 1 {
		t.Fatalf("expected one pattern after merge, got %d", rb.GetStats().TotalPatterns)
	}
}

func TestGenerateGuidance(t *testing.T) {
	t.Parallel()
	rb := NewReasoningBank()
	g := rb.GenerateGuidance("fix security bug in auth")
	if g == "" {
		t.Fatal("empty guidance")
	}
	gl := strings.ToLower(g)
	if !strings.Contains(gl, "privilege") && !strings.Contains(gl, "threat") {
		t.Fatalf("expected security-flavored guidance: %q", g)
	}
	def := rb.GenerateGuidance("something vague without domain words")
	if def == "" || def != DOMAIN_GUIDANCE["default"] {
		t.Fatalf("default guidance: %q", def)
	}
}

func TestGetStatsReasoningBank(t *testing.T) {
	t.Parallel()
	rb := NewReasoningBank()
	_, _ = rb.StorePattern(&GuidancePattern{Quality: 0.8, LongTerm: true, Embedding: []float32{0, 1, 0}})
	_, _ = rb.StorePattern(&GuidancePattern{Quality: 0.4, LongTerm: false, Embedding: []float32{0, 0, 1}})
	st := rb.GetStats()
	if st.TotalPatterns != 2 || st.LongTerm+st.ShortTerm != 2 {
		t.Fatalf("GetStats: %#v", st)
	}
	wantAvg := (0.8 + 0.4) / 2
	if math.Abs(st.AvgQuality-wantAvg) > 1e-6 {
		t.Fatalf("AvgQuality: %v want %v", st.AvgQuality, wantAvg)
	}
}

func TestExportImport(t *testing.T) {
	t.Parallel()
	rb := NewReasoningBank()
	_, _ = rb.StorePattern(&GuidancePattern{Strategy: "s", Domain: "d", Quality: 0.55, Embedding: []float32{1, 1, 0}})
	exported := rb.ExportPatterns()
	if len(exported) != 1 {
		t.Fatalf("export: %d", len(exported))
	}
	rb2 := NewReasoningBank()
	n := rb2.ImportPatterns(exported)
	if n != 1 {
		t.Fatalf("ImportPatterns new count: %d", n)
	}
	if rb2.GetStats().TotalPatterns != 1 {
		t.Fatalf("dest patterns: %d", rb2.GetStats().TotalPatterns)
	}
}

func TestSessionStartEnd(t *testing.T) {
	t.Parallel()
	rb := NewReasoningBank()
	rb.OnSessionStart("session-alpha")
	rb.OnSessionEnd("wrong-id")
	rb.OnSessionStart("session-beta")
	rb.OnSessionEnd("session-beta")
}
