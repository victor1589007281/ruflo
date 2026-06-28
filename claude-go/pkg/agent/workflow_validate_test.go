package agent

import "testing"

func TestValidateAgentOutputAllowsTechnicalRateLimitDiscussion(t *testing.T) {
	output := `# AgentDB 技术调研报告

本报告比较文件、向量、图谱、倒排索引与普通 KV 存储需求。Agent 系统需要记录工具调用、上下文窗口、checkpoint、限流策略、超时恢复、向量召回、图谱关系和普通元数据。这里出现“限流”和“timeout”只是设计主题，不是 API 错误。`
	if reason := validateAgentOutput(output, "researcher"); reason != "" {
		t.Fatalf("expected technical report to pass validation, got %q", reason)
	}
}

func TestEvalScorePassMissingUsesScores(t *testing.T) {
	score, err := ParseEvalScoreJSON([]byte(`{"correctness":8,"completeness":8,"security":9,"code_quality":8,"feedback":"looks good"}`))
	if err != nil {
		t.Fatal(err)
	}
	if score.PassSet {
		t.Fatal("pass presence should be false when reviewer omitted pass")
	}
	if !score.MeetsHardPassThreshold() {
		t.Fatalf("high score without explicit pass should pass, got %#v", score)
	}
	score, err = ParseEvalScoreJSON([]byte(`{"correctness":8,"completeness":8,"security":9,"code_quality":8,"feedback":"blocked","pass":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if !score.PassSet {
		t.Fatal("explicit pass=false should be recorded")
	}
	if score.MeetsHardPassThreshold() {
		t.Fatalf("explicit pass=false must fail, got %#v", score)
	}
}

func TestAdaptiveTerminatorDoesNotFastPassBlockingFeedback(t *testing.T) {
	terminator := NewAdaptiveTerminator(2, 4)
	score := EvalScore{
		Correctness:  6,
		Completeness: 6,
		Security:     9,
		CodeQuality:  7,
		Feedback:     "implementation is not implemented yet",
		Pass:         false,
		PassSet:      true,
	}
	decision := terminator.ShouldTerminate(1, score)
	if decision.ShouldStop {
		t.Fatalf("blocking feedback must continue to another round, got %#v", decision)
	}
}

func TestValidateAgentOutputStillDetectsAPIErrorWrapper(t *testing.T) {
	output := `API 错误 (限流/超时/熔断): 429 rate limit exceeded`
	if reason := validateAgentOutput(output, "researcher"); reason != "__API_ERROR__" {
		t.Fatalf("expected API wrapper to be detected, got %q", reason)
	}
}

func TestManagerLabSimulationV2WorkflowIsValid(t *testing.T) {
	wf := GetWorkflow("manager-lab-simulation-v2")
	if wf == nil {
		t.Fatal("manager-lab-simulation-v2 workflow should be registered")
	}
	if err := wf.Validate(nil); err != nil {
		t.Fatalf("manager-lab-simulation-v2 should validate: %v", err)
	}
}
