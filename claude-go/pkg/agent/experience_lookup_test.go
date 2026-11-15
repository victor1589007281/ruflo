package agent

// zz_debug_store_test.go —— 曾用于定位"反馈测试在共享经验库开启时失败"的现场,
// 现在保留为回归用例: 种子经验的位置不是 experiences[0]。
//
// 背景: NewEvolutionEngine 会按 CLAUDE_GO_EXPERIENCE_STORE 从 agentDB 合并共享
// 经验 (本机 200 条)。测试里 append 的种子经验排在末尾, 因此断言必须按 ID 查,
// 不能按下标 0 取 —— 否则测试结果会随环境变量而变。

import "testing"

// findExperience 按 ID 取经验; 找不到返回 nil (测试应显式失败而不是 panic)。
func findExperience(ee *EvolutionEngine, id string) *Experience {
	for _, e := range ee.experiences {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func TestSeededExperienceLookupByID(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	ee.experiences = append(ee.experiences, &Experience{ID: "e-seed", Quality: 0.5})
	ee.RecordFeedbackScored("e-seed", 1.0)

	exp := findExperience(ee, "e-seed")
	if exp == nil {
		t.Fatalf("种子经验 e-seed 在反馈后丢失 (共 %d 条)", len(ee.experiences))
	}
	if exp.UsageCount != 1 || exp.SuccessCount != 1 {
		t.Fatalf("种子经验未被更新: usage=%d success=%d", exp.UsageCount, exp.SuccessCount)
	}
}
