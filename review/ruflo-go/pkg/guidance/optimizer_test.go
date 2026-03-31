package guidance

import (
	"testing"
)

func TestOptimizerLoop_RankViolations(t *testing.T) {
	t.Parallel()
	var loop OptimizerLoop
	vv := []Violation{
		{RuleID: "r1", Message: "a", Severity: RiskLow},
		{RuleID: "r1", Message: "b", Severity: RiskLow},
		{RuleID: "r2", Message: "c", Severity: RiskCritical},
	}
	out := loop.ViolationPrioritySort(vv)
	if len(out) < 2 {
		t.Fatalf("len=%d", len(out))
	}
	// r2 single critical should outrank repeated low when score higher
	if out[0].RuleID != "r2" {
		t.Fatalf("first should be critical rule, got %+v", out[0])
	}
}

func TestOptimizerLoop_ProposeRuleChange(t *testing.T) {
	t.Parallel()
	o := NewOptimizerLoop()
	s := o.ProposeRuleChange(Violation{RuleID: "R-1", Message: "do not leak secrets", Severity: RiskHigh})
	if s == "" || len(s) < 20 {
		t.Fatalf("proposal too short: %q", s)
	}
}

func TestProofChain_AppendAndVerify(t *testing.T) {
	t.Parallel()
	c := NewProofChain("secret")
	_, err := c.Append(map[string]any{"k": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Verify() {
		t.Fatal("Verify failed on valid chain")
	}
}

func TestProofChain_TamperedChain(t *testing.T) {
	t.Parallel()
	c := NewProofChain("s")
	_, _ = c.Append(map[string]any{"x": 1})
	if len(c.chain) == 0 {
		t.Fatal("empty chain")
	}
	c.chain[len(c.chain)-1].Hash = "deadbeef"
	if c.Verify() {
		t.Fatal("Verify should fail after tamper")
	}
}
