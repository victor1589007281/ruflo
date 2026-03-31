package neural

import (
	"math"
	"testing"
)

func TestQLearning_SelectAction(t *testing.T) {
	t.Parallel()
	q := NewQLearning(0.5, 0.9, 0)
	state := []float32{1, 0, 0}
	acts := []string{"left", "right"}
	a, _ := q.SelectAction(state, acts)
	if a != "left" {
		t.Fatalf("expected first action on tie, got %q", a)
	}
}

func TestQLearning_Update(t *testing.T) {
	t.Parallel()
	q := NewQLearning(1, 0, 0)
	s0 := []float32{1}
	s1 := []float32{2}
	_ = q.Update(s0, "a", 1, s1)
	_ = q.Update(s1, "b", 0, s0)
	if math.Abs(q.getQ(stateKey(s0), "a")-1) > 1e-6 {
		t.Fatalf("unexpected Q: %v", q.getQ(stateKey(s0), "a"))
	}
}

func TestSARSA_OnPolicy(t *testing.T) {
	t.Parallel()
	s := NewSARSA(1, 0.5, 0)
	s0 := []float32{0.5}
	s1 := []float32{1.5}
	acts := []string{"x", "y"}
	a0, _ := s.SelectAction(s0, acts)
	_ = s.Update(s0, a0, 2, s1)
	if s.q[stateKey(s0)] == nil {
		t.Fatal("expected Q row for s0")
	}
}

func TestDQN_LinearWeights(t *testing.T) {
	t.Parallel()
	d := NewDQN(2, 0.5, 0.9, 0)
	s0 := []float32{1, 0}
	s1 := []float32{0, 1}
	acts := []string{"p", "q"}
	_, _ = d.SelectAction(s0, acts)
	_ = d.Update(s0, "p", 1, s1)
	d.mu.Lock()
	w := d.weights["p"]
	d.mu.Unlock()
	if len(w) != 2 {
		t.Fatalf("weights len %d", len(w))
	}
	var norm float64
	for _, x := range w {
		norm += float64(x * x)
	}
	if norm == 0 {
		t.Fatal("expected non-zero weights after update")
	}
}

func TestCuriosityDriven_IntrinsicReward(t *testing.T) {
	t.Parallel()
	base := NewQLearning(0.2, 0.9, 0)
	c := NewCuriosityDriven(base, 2, 0.5, 1.0)
	s0 := []float32{2, 3}
	s1 := []float32{5, 7}
	acts := []string{"a"}
	a, _ := c.SelectAction(s0, acts)
	_ = c.Update(s0, a, 0, s1)
	c.mu.Lock()
	nz := false
	for _, v := range c.wModel {
		if v != 0 {
			nz = true
			break
		}
	}
	c.mu.Unlock()
	if !nz {
		t.Fatal("expected curiosity model to move weights")
	}
}
