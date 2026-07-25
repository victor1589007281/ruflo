package api

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/trace"
)

// TestStampTraceIDs 守护 design/03 §4.1 E0: llm.jsonl 记录必须携带 trace 四元组,
// CallID 恒非空且每条独立。
func TestStampTraceIDs(t *testing.T) {
	rec := LLMCallRecord{}
	stampTraceIDs(&rec, trace.IDs{RunID: "run-a", NodeID: "stage-b", TurnID: "t2"})
	if rec.RunID != "run-a" || rec.NodeID != "stage-b" || rec.TurnID != "t2" {
		t.Fatalf("trace 字段未正确落到记录: %+v", rec)
	}
	if rec.CallID == "" {
		t.Fatal("CallID 应自动生成, 不得为空")
	}

	rec2 := LLMCallRecord{}
	stampTraceIDs(&rec2, trace.IDs{})
	if rec2.CallID == "" || rec2.CallID == rec.CallID {
		t.Fatalf("每条记录的 CallID 应独立生成: %q vs %q", rec.CallID, rec2.CallID)
	}
	// 空 ids: run/node/turn 允许为空 (非团队路径), 不 panic 即可
	stampTraceIDs(nil, trace.IDs{}) // nil 记录防御
}

// TestEstimateInputTokens 守护 design/02 §1.4 token 双边记账兜底。
func TestEstimateInputTokens(t *testing.T) {
	pc := PromptComponentMetrics{SystemChars: 400, MessagesChars: 800} // 1200 chars
	if got := estimateInputTokens(pc); got != 300 {                    // 1200/4
		t.Fatalf("估算应为 300 token, got %d", got)
	}
	if got := estimateInputTokens(PromptComponentMetrics{}); got != 0 {
		t.Fatalf("无 prompt 应估 0, got %d", got)
	}
}
