package neural

import (
	"math"
	"testing"
)

func TestBeginEndTrajectory(t *testing.T) {
	t.Parallel()
	s := NewSONACoordinator(DefaultSONAConfig(), "")
	id := s.BeginTrajectory("task-1")
	if id != "task-1" {
		t.Fatalf("trajectory id: %q", id)
	}
	s.RecordStep(id, TrajectoryStep{Type: StepAction, Content: "step-a"})
	s.RecordStep(id, TrajectoryStep{Type: StepResult, Content: "done"})
	if err := s.EndTrajectory(id, "success"); err != nil {
		t.Fatal(err)
	}
}

func TestPatternLearning(t *testing.T) {
	t.Parallel()
	cfg := DefaultSONAConfig()
	cfg.LearningRate = 0.1
	s := NewSONACoordinator(cfg, "")
	id := s.BeginTrajectory("learn")
	s.RecordStep(id, TrajectoryStep{Type: StepThought, Content: "unique-pattern-xyz"})
	if err := s.EndTrajectory(id, "success"); err != nil {
		t.Fatal(err)
	}
	pats := s.Patterns()
	if len(pats) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(pats))
	}
	if pats[0].Content != "unique-pattern-xyz" {
		t.Fatalf("content: %q", pats[0].Content)
	}
	if pats[0].Confidence <= 0.1 {
		t.Fatalf("confidence should increase above initial, got %v", pats[0].Confidence)
	}
}

func TestFindSimilarPatterns(t *testing.T) {
	t.Parallel()
	s := NewSONACoordinator(DefaultSONAConfig(), "")
	t1 := s.BeginTrajectory("t1")
	s.RecordStep(t1, TrajectoryStep{
		Type:      StepObservation,
		Content:   "near-a",
		Embedding: []float32{1, 0, 0},
	})
	_ = s.EndTrajectory(t1, "success")

	t2 := s.BeginTrajectory("t2")
	s.RecordStep(t2, TrajectoryStep{
		Type:      StepObservation,
		Content:   "far-b",
		Embedding: []float32{0, 1, 0},
	})
	_ = s.EndTrajectory(t2, "success")

	q := []float32{0.99, 0.01, 0}
	sim := s.FindSimilarPatterns(q, 2)
	if len(sim) < 1 {
		t.Fatal("expected at least one similar pattern")
	}
	if sim[0].Content != "near-a" {
		t.Fatalf("expected near-a first, got %#v", sim[0])
	}
	if sim[0].Embedding == nil || math.Abs(float64(sim[0].Embedding[0]-1)) > 0.01 {
		t.Fatalf("unexpected embedding copy: %#v", sim[0].Embedding)
	}
}

func TestSetMode(t *testing.T) {
	t.Parallel()
	bal := NewSONACoordinator(DefaultSONAConfig(), "")
	bal.SetMode("balanced")
	res := NewSONACoordinator(DefaultSONAConfig(), "")
	res.SetMode("research")
	idb := bal.BeginTrajectory("cmp-bal")
	bal.RecordStep(idb, TrajectoryStep{Type: StepThought, Content: "mode-compare-unique"})
	_ = bal.EndTrajectory(idb, "success")
	idr := res.BeginTrajectory("cmp-res")
	res.RecordStep(idr, TrajectoryStep{Type: StepThought, Content: "mode-compare-unique"})
	_ = res.EndTrajectory(idr, "success")
	pb := bal.Patterns()
	pr := res.Patterns()
	if len(pb) != 1 || len(pr) != 1 {
		t.Fatalf("patterns bal=%d res=%d", len(pb), len(pr))
	}
	if !(pr[0].Confidence < pb[0].Confidence) {
		t.Fatalf("research mode should learn slower: bal=%v res=%v", pb[0].Confidence, pr[0].Confidence)
	}
}

func TestGetTrajectory(t *testing.T) {
	t.Parallel()
	s := NewSONACoordinator(DefaultSONAConfig(), "")
	id := s.BeginTrajectory("tr-lookup")
	s.RecordStep(id, TrajectoryStep{Type: StepAction, Content: "step1"})
	tr, ok := s.GetTrajectory(id)
	if !ok || tr == nil || len(tr.Steps) != 1 || tr.Steps[0].Content != "step1" {
		t.Fatalf("GetTrajectory: %#v ok=%v", tr, ok)
	}
}

func TestGetStatsSONA(t *testing.T) {
	t.Parallel()
	s := NewSONACoordinator(DefaultSONAConfig(), "")
	s.RecordSignal(Signal{Kind: "k", Payload: map[string]any{"x": 1}})
	_ = s.BeginTrajectory("open-traj")
	s.IngestPattern(Pattern{Content: "p", Confidence: 0.5, Embedding: []float32{1, 0}})
	st := s.GetStats()
	if st.TotalPatterns < 1 || st.SignalCount < 1 || st.TotalTrajectories < 1 {
		t.Fatalf("GetStats: %#v", st)
	}
	if st.ActiveTrajectories < 1 {
		t.Fatalf("expected active trajectory: %#v", st)
	}
}

func TestCleanup(t *testing.T) {
	t.Parallel()
	s := NewSONACoordinator(DefaultSONAConfig(), "")
	s.IngestPattern(Pattern{Content: "low", Confidence: 0.01})
	s.IngestPattern(Pattern{Content: "high", Confidence: 0.5})
	tid := s.BeginTrajectory("done-traj")
	s.RecordStep(tid, TrajectoryStep{Type: StepResult, Content: "x"})
	_ = s.EndTrajectory(tid, "success")
	s.Cleanup()
	pats := s.Patterns()
	var hasLow bool
	for _, p := range pats {
		if p.Content == "low" {
			hasLow = true
		}
	}
	if hasLow {
		t.Fatalf("low-confidence pattern should be dropped: %#v", pats)
	}
	_, ok := s.GetTrajectory("done-traj")
	if ok {
		t.Fatal("completed trajectory should be removed")
	}
}
