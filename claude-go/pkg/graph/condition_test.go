package graph

import "testing"

// TestConditionTable 条件语言全语法表驱动 (含解析错误)。
func TestConditionTable(t *testing.T) {
	completed80 := NodeResult{Status: NodeStatusCompleted, Score: 80, Output: "hello world 结果"}
	failedR := NodeResult{Status: NodeStatusFailed, Err: "boom"}

	cases := []struct {
		name    string
		cond    string
		r       NodeResult
		want    bool
		wantErr bool
	}{
		{"空条件恒真-completed", "", completed80, true, false},
		{"空条件恒真-failed", "", failedR, true, false},
		{"纯空白恒真", "   ", completed80, true, false},
		{"ok命中", "ok", completed80, true, false},
		{"ok不命中", "ok", failedR, false, false},
		{"fail命中", "fail", failedR, true, false},
		{"fail不命中", "fail", completed80, false, false},
		{"score大于等于-临界命中", "score >= 80", completed80, true, false},
		{"score大于等于-不命中", "score >= 80.5", completed80, false, false},
		{"score大于", "score > 79.9", completed80, true, false},
		{"score小于等于", "score <= 80", completed80, true, false},
		{"score小于-不命中", "score < 80", completed80, false, false},
		{"score等于", "score == 80", completed80, true, false},
		{"score等于0", "score == 0", failedR, true, false},
		{"score不等于", "score != 3", completed80, true, false},
		{"contains命中", `output contains "world"`, completed80, true, false},
		{"contains大小写敏感", `output contains "World"`, completed80, false, false},
		{"contains带空格串", `output contains "hello world"`, completed80, true, false},
		{"contains中文", `output contains "结果"`, completed80, true, false},
		{"not_contains命中", `output not_contains "缺席"`, completed80, true, false},
		{"not_contains不命中", `output not_contains "world"`, completed80, false, false},
		// —— 解析错误 ——
		{"未知词", "bogus", completed80, false, true},
		{"ok带尾巴", "ok extra", completed80, false, true},
		{"score坏比较符", "score >> 5", completed80, false, true},
		{"score阈值非数字", "score >= abc", completed80, false, true},
		{"score缺操作数", "score >=", completed80, false, true},
		{"contains缺引号", "output contains abc", completed80, false, true},
		{"output坏操作符", `output has "x"`, completed80, false, true},
		{"contains缺空白", `output containsx "a"`, completed80, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCondition(tc.cond)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCondition(%q) 应报语法错误, 实际 nil", tc.cond)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCondition(%q) 意外报错: %v", tc.cond, err)
			}
			if got := c.Eval(tc.r); got != tc.want {
				t.Fatalf("条件 %q 对 %+v 求值 = %v, 期望 %v", tc.cond, tc.r, got, tc.want)
			}
		})
	}
}

// TestConditionZeroValue 零值 Condition 应为恒真 (引擎对无条件边的依赖)。
func TestConditionZeroValue(t *testing.T) {
	var c Condition
	if !c.Eval(NodeResult{Status: NodeStatusFailed}) {
		t.Fatal("零值 Condition 应恒真")
	}
}
