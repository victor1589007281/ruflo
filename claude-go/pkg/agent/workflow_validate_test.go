package agent

import "testing"

func TestValidateAgentOutputAllowsTechnicalRateLimitDiscussion(t *testing.T) {
	output := `# AgentDB 技术调研报告

本报告比较文件、向量、图谱、倒排索引与普通 KV 存储需求。Agent 系统需要记录工具调用、上下文窗口、checkpoint、限流策略、超时恢复、向量召回、图谱关系和普通元数据。这里出现“限流”和“timeout”只是设计主题，不是 API 错误。`
	if reason := validateAgentOutput(output, "researcher"); reason != "" {
		t.Fatalf("expected technical report to pass validation, got %q", reason)
	}
}

func TestValidateAgentOutputStillDetectsAPIErrorWrapper(t *testing.T) {
	output := `API 错误 (限流/超时/熔断): 429 rate limit exceeded`
	if reason := validateAgentOutput(output, "researcher"); reason != "__API_ERROR__" {
		t.Fatalf("expected API wrapper to be detected, got %q", reason)
	}
}
