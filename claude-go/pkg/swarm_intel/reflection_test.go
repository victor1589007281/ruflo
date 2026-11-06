package swarm_intel

// W1 Phase 3 跨会话反思的纯函数验收: composeReflection 三类结局
// (高置信命中 / 低置信命中 / 未命中) 的 Lesson 构图与 Domain 派生。

import (
	"strings"
	"testing"
	"time"
)

func TestComposeReflection_HighConfHit(t *testing.T) {
	pred := &FusedPrediction{
		Question: "本次发布会不会回滚",
		Outcomes: []OutcomePrediction{
			{Outcome: "不会回滚", Probability: 0.85},
			{Outcome: "会回滚", Probability: 0.15},
		},
	}
	rec := composeReflection(pred, "不会回滚", 0.045, time.Now())
	if rec == nil {
		t.Fatal("不应为 nil")
	}
	if rec.Domain != "binary/unknown" {
		t.Fatalf("Domain 应 binary/unknown, 实际 %s", rec.Domain)
	}
	if rec.TopPick != "不会回滚" || rec.TopProb != 0.85 {
		t.Fatalf("TopPick/TopProb 错: %s %.2f", rec.TopPick, rec.TopProb)
	}
	if !strings.Contains(rec.Lesson, "命中") {
		t.Fatalf("高置信命中 Lesson 应含「命中」: %s", rec.Lesson)
	}
}

func TestComposeReflection_Miss(t *testing.T) {
	pred := &FusedPrediction{
		Question: "本次发布会不会回滚",
		Outcomes: []OutcomePrediction{
			{Outcome: "不会回滚", Probability: 0.85},
			{Outcome: "会回滚", Probability: 0.15},
		},
	}
	rec := composeReflection(pred, "会回滚", 0.72, time.Now())
	if !strings.Contains(rec.Lesson, "高估") || !strings.Contains(rec.Lesson, "会回滚") {
		t.Fatalf("未命中 Lesson 应指出高估的结局: %s", rec.Lesson)
	}
	// 实给概率要出现在教训里 (Scout 先验的关键信息)
	if !strings.Contains(rec.Lesson, "0.15") {
		t.Fatalf("Lesson 应含实际结局的实给概率: %s", rec.Lesson)
	}
}

func TestComposeReflection_LowConfHit(t *testing.T) {
	pred := &FusedPrediction{
		Question: "Q",
		Outcomes: []OutcomePrediction{
			{Outcome: "A", Probability: 0.55},
			{Outcome: "B", Probability: 0.45},
		},
	}
	rec := composeReflection(pred, "A", 0.405, time.Now())
	if !strings.Contains(rec.Lesson, "保守") {
		t.Fatalf("低置信命中 Lesson 应指出群体保守: %s", rec.Lesson)
	}
}

func TestComposeReflection_Nil(t *testing.T) {
	if rec := composeReflection(nil, "x", 0, time.Now()); rec != nil {
		t.Fatalf("nil pred 应返回 nil, 实际 %+v", rec)
	}
}

func TestReflectionDomain_Shape(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{2, "binary"},
		{4, "categorical"},
		{8, "scenario"},
	}
	for _, c := range cases {
		pred := &FusedPrediction{Outcomes: make([]OutcomePrediction, c.n)}
		got := reflectionDomain(pred)
		if !strings.HasPrefix(got, c.want+"/") {
			t.Fatalf("n=%d 应 %s/*, 实际 %s", c.n, c.want, got)
		}
	}
}
