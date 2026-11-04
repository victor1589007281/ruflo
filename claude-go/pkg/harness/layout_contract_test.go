package harness

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件是 R-H2（稳定/动态 Prompt 布局与缓存契约，见 Moqi design/07 §4 R-H2
// 与 §5 Q-H2）的契约测试：逐条钉住验收行为，而不是复述实现细节。
//
// 与 layout_test.go 的分工：后者覆盖单次调用的边界与观测上报，这里覆盖
// "跨多轮对话"的整体契约（10 轮不变、单轮证据变化、画像升版、断点上限）。

// assertStableSegmentsIdentical 逐段比较两次布局的稳定段。
//
// 只比 PrefixHash 不够：聚合 hash 一致但段内 hash/字节数漂移，说明实现把
// "稳定"的定义偷换了（例如某轮把动态内容混进稳定段列表）。
func assertStableSegmentsIdentical(t *testing.T, want, got []PromptSegment) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("稳定段数量变化：want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		if want[i].ID != got[i].ID || want[i].Version != got[i].Version ||
			want[i].Hash != got[i].Hash || want[i].Bytes != got[i].Bytes {
			t.Fatalf("第 %d 个稳定段漂移：want=%+v got=%+v", i, want[i], got[i])
		}
	}
}

// TestLayoutContractPrefixHashStableOverTenRounds 钉 R-H2 验收第 1 条：
// 10 轮对话、画像不变时，每一轮的稳定段 hash 与整体 PrefixHash 必须完全一致，
// 且不得报失效原因。动态证据每轮都换，正是要证明它不渗进前缀。
func TestLayoutContractPrefixHashStableOverTenRounds(t *testing.T) {
	const rounds = 10
	b := NewLayoutBuilder(LayoutOptions{})

	var base PromptLayout
	for round := 0; round < rounds; round++ {
		layout, err := b.Build(SegmentSpec{
			// 画像（persona/profile）版本固定：这是"画像不变"的定义。
			Stable: stable("p1", "u1", "工具说明"),
			Dynamic: []SegmentInput{
				{ID: "evidence", Text: fmt.Sprintf("第 %d 轮检索到的证据", round)},
				{ID: "turn", Text: fmt.Sprintf("第 %d 轮用户输入", round)},
			},
			BreakpointCount: 1,
		})
		if err != nil {
			t.Fatalf("第 %d 轮 Build 失败: %v", round, err)
		}

		if round == 0 {
			base = layout
			if base.PrefixHash == "" {
				t.Fatal("首轮 PrefixHash 不应为空")
			}
			continue
		}
		if layout.PrefixHash != base.PrefixHash {
			t.Fatalf("第 %d 轮 PrefixHash 漂移：%s != %s", round, layout.PrefixHash, base.PrefixHash)
		}
		if layout.InvalidationReason != InvalidationNone {
			t.Fatalf("第 %d 轮画像未变却报了失效原因 %q", round, layout.InvalidationReason)
		}
		assertStableSegmentsIdentical(t, base.StableSegments, layout.StableSegments)
	}
}

// TestLayoutContractRound2EvidenceChangeKeepsPrefixHash 钉 R-H2 验收第 2 条：
// 只改第 2 轮的证据（动态段）时，PrefixHash 必须逐字节一致。
//
// 用"同一段对话重放两遍、仅第 2 轮证据不同"的方式，同时验证两件事：
// 该轮前缀没变，且这次变化没有污染后续轮次（历史/上一轮快照不被动态内容裹挟）。
func TestLayoutContractRound2EvidenceChangeKeepsPrefixHash(t *testing.T) {
	const rounds = 5
	const changedRound = 2

	buildSequence := func(evidenceOf func(round int) string) []PromptLayout {
		b := NewLayoutBuilder(LayoutOptions{})
		out := make([]PromptLayout, rounds)
		for round := 0; round < rounds; round++ {
			layout, err := b.Build(SegmentSpec{
				Stable: stable("p1", "u1", "工具说明"),
				Dynamic: []SegmentInput{
					{ID: "evidence", Text: evidenceOf(round)},
					{ID: "turn", Text: fmt.Sprintf("第 %d 轮用户输入", round)},
				},
				BreakpointCount: 1,
			})
			if err != nil {
				t.Fatalf("第 %d 轮 Build 失败: %v", round, err)
			}
			out[round] = layout
		}
		return out
	}

	baseline := buildSequence(func(round int) string {
		return fmt.Sprintf("第 %d 轮检索到的证据", round)
	})
	// 只改第 2 轮证据，其余轮次逐字相同。
	changed := buildSequence(func(round int) string {
		if round == changedRound {
			return "第 2 轮检索到的证据（已更新）"
		}
		return fmt.Sprintf("第 %d 轮检索到的证据", round)
	})

	for round := 0; round < rounds; round++ {
		if changed[round].PrefixHash != baseline[round].PrefixHash {
			t.Fatalf("第 %d 轮 PrefixHash 受动态证据影响：%s != %s",
				round, changed[round].PrefixHash, baseline[round].PrefixHash)
		}
	}
	if changed[changedRound].InvalidationReason != InvalidationNone {
		t.Fatalf("只改动态证据不应报失效，实得 %q", changed[changedRound].InvalidationReason)
	}
}

// TestLayoutContractProfileVersionBumpInvalidatesPrefix 钉 R-H2 验收第 3 条：
// 画像版本 +1 时 PrefixHash 必须变化，且失效原因字符串来自 inferInvalidation
// 的推断（未被显式原因覆盖），业务层据此区分"预期失效"与"代码漂移"。
//
// 这里用字面量 "profile_version" 而不是只比常量：常量的值本身就是对外契约，
// 只比常量会让"常量被改错"的变更仍然绿。
func TestLayoutContractProfileVersionBumpInvalidatesPrefix(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	dynamic := []SegmentInput{
		{ID: "evidence", Text: "第 1 轮证据"},
		{ID: "turn", Text: "第 1 轮用户输入"},
	}

	before, err := b.Build(SegmentSpec{
		Stable:          stable("p1", "u1", "工具说明"),
		Dynamic:         dynamic,
		BreakpointCount: 1,
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if before.InvalidationReason != InvalidationNone {
		t.Fatalf("首轮不应报失效，实得 %q", before.InvalidationReason)
	}

	// 画像版本 +1（u1 → u2），动态内容与 persona 都保持不变。
	after, err := b.Build(SegmentSpec{
		Stable:          stable("p1", "u2", "工具说明"),
		Dynamic:         dynamic,
		BreakpointCount: 1,
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if after.PrefixHash == before.PrefixHash {
		t.Fatal("profile 版本 +1 后 PrefixHash 未变：版本号没有参与稳定前缀 hash")
	}
	if after.InvalidationReason != "profile_version" {
		t.Fatalf("失效原因应为字面量 %q（inferInvalidation 推断），实得 %q",
			"profile_version", after.InvalidationReason)
	}
	if after.InvalidationReason != InvalidationProfile {
		t.Fatalf("失效原因应与常量 %q 一致，实得 %q", InvalidationProfile, after.InvalidationReason)
	}
}

// TestLayoutContractFiveBreakpointsRejectedBeforeSend 钉 R-H2 验收第 4 条：
// 断点 5 个（超过 Anthropic 上限 4）必须在 Build 阶段失败，错误信息可读，
// 且**不能静默截断**成 4 个断点的"看似可用"布局。
//
// 用默认构造器（不显式调低 MaxBreakpoints），钉的是 provider 上限本身而不是
// 某个测试自定义的阈值。
func TestLayoutContractFiveBreakpointsRejectedBeforeSend(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	if got := b.MaxBreakpoints(); got != 4 {
		t.Fatalf("默认断点上限应为 provider 上限 4，实得 %d", got)
	}

	dyn := make([]SegmentInput, 5)
	for i := range dyn {
		dyn[i] = SegmentInput{ID: fmt.Sprintf("d%d", i+1), Text: "x"}
	}
	layout, err := b.Build(SegmentSpec{
		Stable:          stable("p1", "u1", "工具说明"),
		Dynamic:         dyn,
		BreakpointCount: 5,
	})
	if err == nil {
		t.Fatal("5 个断点超过 provider 上限，必须在发送前报错")
	}

	msg := err.Error()
	for _, want := range []string{"5", "4", "cache_control", "上限"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q 以便调用方定位，实得: %s", want, msg)
		}
	}

	// 不静默截断：失败时不能返回一个带着 4 个断点的布局（那会被当成可发送结果）。
	if len(layout.Breakpoints) != 0 {
		t.Fatalf("失败时不应返回任何断点，实得 %v（疑似静默截断）", layout.Breakpoints)
	}
	if layout.PrefixHash != "" {
		t.Fatalf("失败时不应返回可用的 PrefixHash，实得 %q", layout.PrefixHash)
	}
}

// TestLayoutContractBreakpointsWithinLimitSucceed 是 Q-H2 的正对照：
// 断点数在 1..4 内必须全部成功，断点精确落在动态段上。
//
// 没有这条，"永远报错"也能让上面的超限测试变绿。
func TestLayoutContractBreakpointsWithinLimitSucceed(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	dyn := make([]SegmentInput, 4)
	for i := range dyn {
		dyn[i] = SegmentInput{ID: fmt.Sprintf("d%d", i+1), Text: fmt.Sprintf("动态 %d", i+1)}
	}

	for n := 0; n <= 4; n++ {
		layout, err := b.Build(SegmentSpec{
			Stable:          stable("p1", "u1", "工具说明"),
			Dynamic:         dyn,
			BreakpointCount: n,
		})
		if err != nil {
			t.Fatalf("%d 个断点（≤ 上限 4）应被接受，得到: %v", n, err)
		}
		if len(layout.Breakpoints) != n {
			t.Fatalf("%d 个断点应原样放置，实得 %v", n, layout.Breakpoints)
		}
		for i, idx := range layout.Breakpoints {
			if idx < 0 || idx >= len(layout.DynamicSegments) {
				t.Fatalf("%d 个断点时第 %d 个断点下标 %d 越界（动态段 %d）",
					n, i, idx, len(layout.DynamicSegments))
			}
			if i > 0 && idx <= layout.Breakpoints[i-1] {
				t.Fatalf("%d 个断点时断点下标必须递增，实得 %v", n, layout.Breakpoints)
			}
		}
	}
}
