package guidance

import (
	"testing"
	"time"
)

func TestStructuredRunLedger_StartFinalize(t *testing.T) {
	t.Parallel()
	l := NewStructuredRunLedger()
	l.StartRun("run-1", map[string]any{"intent": string(IntentFeature)})
	time.Sleep(2 * time.Millisecond)
	l.FinalizeRun("run-1", true, map[string]any{"test_results": "pass ok"})
	rec := l.GetRecord("run-1")
	if rec == nil || !rec.Success || rec.RunID != "run-1" {
		t.Fatalf("record: %+v", rec)
	}
	if rec.Duration <= 0 {
		t.Fatal("expected positive duration")
	}
	ev := l.Events("run-1")
	if len(ev) < 2 {
		t.Fatalf("events: %d", len(ev))
	}
}

func TestStructuredRunLedger_ComputeMetrics(t *testing.T) {
	t.Parallel()
	l := NewStructuredRunLedger()
	l.StartRun("a", nil)
	l.FinalizeRun("a", true, nil)
	l.StartRun("b", nil)
	l.FinalizeRun("b", false, nil)
	m := l.ComputeMetrics()
	if m.TotalRuns != 2 {
		t.Fatalf("total %d", m.TotalRuns)
	}
	if m.SuccessRate != 0.5 {
		t.Fatalf("success rate %v", m.SuccessRate)
	}
}

func TestTestsPassEvaluator(t *testing.T) {
	t.Parallel()
	var e TestsPassEvaluator
	if e.Evaluate(nil) {
		t.Fatal("nil record")
	}
	if !e.Evaluate(&RunRecord{Success: true, TestResults: "all tests pass"}) {
		t.Fatal("should pass")
	}
	if e.Evaluate(&RunRecord{Success: true, TestResults: "pass with failure"}) {
		t.Fatal("should fail on contradictory string")
	}
}
