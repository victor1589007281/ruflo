package codex

import "testing"

func TestCollaborationTemplates_Feature(t *testing.T) {
	t.Parallel()
	var ct CollaborationTemplates
	w := ct.Feature("add auth")
	if len(w) != 4 {
		t.Fatalf("len=%d", len(w))
	}
	if w[0].Role != "architect" || len(w[0].DependsOn) != 0 {
		t.Fatalf("first worker %+v", w[0])
	}
	levels := topoLevels(w)
	if len(levels) != 4 {
		t.Fatalf("expected 4 waves got %d: %+v", len(levels), levels)
	}
}

func TestCollaborationTemplates_Security(t *testing.T) {
	t.Parallel()
	var ct CollaborationTemplates
	w := ct.Security("./src")
	if len(w) != 3 || w[1].Role != "scanner" {
		t.Fatalf("workers %+v", w)
	}
}

func TestWorkerConfig_DependencyOrder(t *testing.T) {
	t.Parallel()
	w := FeatureTemplate("task")
	levels := topoLevels(w)
	for i := 1; i < len(levels); i++ {
		for _, wc := range levels[i] {
			if len(wc.DependsOn) == 0 && wc.Role != "architect" && wc.Role != "analyst" && wc.Role != "researcher" {
				t.Fatalf("worker %s should depend on prior tier", wc.Role)
			}
		}
	}
}
