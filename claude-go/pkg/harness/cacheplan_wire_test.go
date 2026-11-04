package harness

import (
	"reflect"
	"testing"
)

// TestTurnCachePlan 锁住"回合出站计划 + trace 下标同源"的契约。
//
// 断言的是**精确下标**而不是"长度大于 0"：断点位置只要偏一位就会落到动态段上，
// 每轮换缓存键，而请求依旧成功 —— 这种缺陷只能靠精确值发现。
//
// 表里同时覆盖"不干预"与"配置错误"两类 nil 返回：两者都返回 nil plan，但错误类
// 必须带 error、不干预类必须不带。把错误当成"没有计划"吞掉是本文件要防的核心回归。
func TestTurnCachePlan(t *testing.T) {
	cases := []struct {
		name     string
		stable   []SegmentInput
		dynamic  int
		budget   int
		wantPlan bool
		wantIdx  []int
		wantErr  bool
	}{
		{
			name:     "3 个稳定段 + 预算 4 落在最后一块",
			stable:   cpStable(3),
			dynamic:  1,
			budget:   4,
			wantPlan: true,
			wantIdx:  []int{2},
		},
		{
			name:     "1 个稳定段 + 预算 4 落在第 0 块",
			stable:   cpStable(1),
			dynamic:  1,
			budget:   4,
			wantPlan: true,
			wantIdx:  []int{0},
		},
		{
			name:     "0 个稳定段不干预",
			stable:   nil,
			dynamic:  1,
			budget:   4,
			wantPlan: false,
		},
		{
			name:     "预算 0 不干预",
			stable:   cpStable(3),
			dynamic:  1,
			budget:   0,
			wantPlan: false,
		},
		{
			name:     "预算 -1 不干预",
			stable:   cpStable(3),
			dynamic:  1,
			budget:   -1,
			wantPlan: false,
		},
		{
			name:     "预算 2 不足 1+2 预留",
			stable:   cpStable(3),
			dynamic:  1,
			budget:   2,
			wantPlan: false,
			wantErr:  true,
		},
		{
			name:     "预算 5 超 provider 上限",
			stable:   cpStable(3),
			dynamic:  1,
			budget:   DefaultMaxBreakpoints + 1,
			wantPlan: false,
			wantErr:  true,
		},
		{
			name:     "重复 ID 报错",
			stable:   []SegmentInput{{ID: "dup", Version: "v1", Text: "a"}, {ID: "dup", Version: "v1", Text: "b"}},
			dynamic:  1,
			budget:   4,
			wantPlan: false,
			wantErr:  true,
		},
		{
			name:     "动态段个数为负报错",
			stable:   cpStable(2),
			dynamic:  -1,
			budget:   4,
			wantPlan: false,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, idx, err := TurnCachePlan(tc.stable, tc.dynamic, tc.budget)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望错误, 实得 plan=%+v idx=%v", plan, idx)
				}
				if plan != nil {
					t.Fatalf("出错的回合不能返回可用计划, 实得 plan=%+v", plan)
				}
				if idx != nil {
					t.Fatalf("出错的回合不能返回 trace 下标, 实得 %v", idx)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望错误: %v", err)
			}

			if !tc.wantPlan {
				if plan != nil || idx != nil {
					t.Fatalf("不干预时应三者全 nil, 实得 plan=%+v idx=%v", plan, idx)
				}
				return
			}

			if plan == nil {
				t.Fatal("期望计划, 实得 nil")
			}
			if !reflect.DeepEqual(plan.SystemIndexes, tc.wantIdx) {
				t.Fatalf("plan.SystemIndexes 错误: got %v want %v", plan.SystemIndexes, tc.wantIdx)
			}
			// 核心契约: trace 下标必须逐元素等于出站计划里的 system 下标。
			if !reflect.DeepEqual(idx, plan.SystemIndexes) {
				t.Fatalf("trace 下标与出站计划不同源: trace=%v plan=%v", idx, plan.SystemIndexes)
			}
			if !reflect.DeepEqual(idx, tc.wantIdx) {
				t.Fatalf("返回下标错误: got %v want %v", idx, tc.wantIdx)
			}
			// 消息侧交回内置启发式: nil 而非空切片（两者语义不同）。
			if plan.MessageIndexes != nil {
				t.Fatalf("MessageIndexes 应为 nil, 实得 %v", plan.MessageIndexes)
			}
		})
	}
}

// TestTurnCachePlanTraceIndexesDoNotAliasPlan 防止 trace 下标与出站计划共享底层
// 数组: trace 的消费方若就地排序/修下标，会反向改掉即将出站的计划。
func TestTurnCachePlanTraceIndexesDoNotAliasPlan(t *testing.T) {
	plan, idx, err := TurnCachePlan(cpStable(3), 1, DefaultMaxBreakpoints)
	if err != nil {
		t.Fatalf("非预期错误: %v", err)
	}
	if len(plan.SystemIndexes) == 0 || len(idx) == 0 {
		t.Fatal("测试前提不成立: 需要非空下标")
	}
	restored := plan.SystemIndexes[0]
	idx[0] = -999
	if plan.SystemIndexes[0] != restored {
		t.Fatalf("修改 trace 下标污染了出站计划: %v", plan.SystemIndexes)
	}
	if restored != 2 {
		t.Fatalf("断点应落在最后一个稳定段 (2), 实得 %d", restored)
	}
}
