package complexity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHeuristic_ChineseComplex(t *testing.T) {
	cases := []string{
		"帮我设计并实现一个多文件功能: 先调研现有代码结构, 然后设计接口方案, 同时准备数据库迁移, 最后部署上线。",
		"请重构这个模块并补充单元测试: 1. 拆分主函数 2. 抽出接口 3. 增加依赖注入 4. 补齐覆盖率 5. 更新文档。",
		"需要同时实现 A 与 B 两套方案, 并且对比各自的性能, 此外还要写一份分析报告。",
		strings.Repeat("这是一段需要多步骤推进的复杂任务描述。", 30),
	}
	for _, c := range cases {
		v := Heuristic(c)
		if !v.Complex {
			t.Errorf("应判定复杂: %q → %+v", truncate(c), v)
		}
		if v.Reason == "" {
			t.Errorf("复杂任务必须有 reason: %q", truncate(c))
		}
	}
}

func TestHeuristic_EnglishComplex(t *testing.T) {
	cases := []string{
		"Please design and implement a multi-file feature with a proper architecture plan and a test suite.",
		"Refactor the auth module: 1. split handler 2. extract service 3. add middleware 4. update tests 5. deploy.",
	}
	for _, c := range cases {
		v := Heuristic(c)
		if !v.Complex {
			t.Errorf("应判定复杂: %q → %+v", truncate(c), v)
		}
	}
}

func TestHeuristic_Simple(t *testing.T) {
	cases := []string{
		"你好",
		"翻译这句话",
		"hi",
		"把标题改成红色",
		"2+2等于几",
		"查一下今天的天气",
	}
	for _, c := range cases {
		v := Heuristic(c)
		if v.Complex {
			t.Errorf("应判定简单: %q → %+v", c, v)
		}
	}
}

func TestHeuristic_ShortAlwaysSimple(t *testing.T) {
	// 超短文本即使含关键词也不应误伤 (对齐 MinRunesForLLM 快跳过)。
	if v := Heuristic("设计"); v.Complex {
		t.Errorf("短文本不应判复杂: %+v", v)
	}
}

func TestHeuristic_ScoreAndReason(t *testing.T) {
	long := strings.Repeat("多步骤复杂任务描述。", 40)
	v := Heuristic(long)
	if v.Score < 3 {
		t.Errorf("超长文本 score 应为 3: %d", v.Score)
	}
}

func TestJudge_Modes(t *testing.T) {
	ctx := context.Background()

	// heuristic 模式: 不调用 LLM。
	llmCalled := false
	llm := func(ctx context.Context, text string) (bool, string, error) {
		llmCalled = true
		return false, "llm says simple", nil
	}
	Judge(ctx, "帮我设计并实现一个多文件功能的完整方案", Options{Mode: ModeHeuristic, LLM: llm})
	if llmCalled {
		t.Fatal("heuristic 模式不应调用 LLM")
	}

	// hybrid 模式: 短文本快跳过, 不调 LLM。
	Judge(ctx, "你好", Options{Mode: ModeHybrid, LLM: llm})
	if llmCalled {
		t.Fatal("hybrid 模式极短文本不应调用 LLM")
	}

	// hybrid 模式: 非平凡文本调用 LLM。
	Judge(ctx, "帮我设计并实现一个多文件功能的完整方案", Options{Mode: ModeHybrid, LLM: llm})
	if !llmCalled {
		t.Fatal("hybrid 模式应调用 LLM")
	}

	// llm 模式: LLM 失败回落启发式。
	v := Judge(ctx, "帮我设计并实现一个多文件功能的完整方案", Options{Mode: ModeLLM, LLM: func(context.Context, string) (bool, string, error) {
		return false, "", errors.New("boom")
	}})
	if !v.Complex {
		t.Fatalf("LLM 失败应回落启发式并判复杂: %+v", v)
	}
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}
