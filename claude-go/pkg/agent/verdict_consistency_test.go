// 外部测试包 (package agent_test): 它可以 import pkg/engine/internal_hook 而不给生产
// 依赖图加边 —— pkg/agent 现在完全不依赖 pkg/engine, 这条 import 只存在于测试二进制里。
//
// 这个文件存在的唯一理由: verdict_reward.go 的 inferTurnVerdict 是
// internal_hook.InferVerdict 的**副本** (见那里的注释)。副本会漂移, 所以用测试焊住 ——
// 与 learners/weights_consistency_test.go 同一套手法。
//
// 漂移的后果很具体: 同一条轨迹, 引擎侧的 turn verdict 说 partial, 学习侧的
// verdict.heuristic 奖励说 success, 而两者都自称"turn 裁决"。
package agent_test

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
)

func TestInferTurnVerdict与引擎侧InferVerdict逐条一致(t *testing.T) {
	cases := []struct {
		name       string
		stopReason string
		ok, total  int
	}{
		{"无工具_end_turn", "end_turn", 0, 0},
		{"无工具_空stop", "", 0, 0},
		{"无工具_max_tokens", "max_tokens", 0, 0},
		{"无工具_error", "error", 0, 0},
		{"无工具_认不出的stop", "pause_turn", 0, 0},
		{"无工具_tool_use但没工具", "tool_use", 0, 0},
		{"工具全成功", "tool_use", 3, 3},
		{"工具9成", "tool_use", 9, 10},
		{"工具刚好一半", "tool_use", 1, 2},
		{"工具全失败", "tool_use", 0, 2},
		{"工具少数成功", "tool_use", 1, 3},
		{"有工具但stop是end_turn", "end_turn", 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sigs := make([]internal_hook.ToolSig, 0, c.total)
			for i := 0; i < c.total; i++ {
				sigs = append(sigs, internal_hook.ToolSig{OK: i < c.ok})
			}
			want := string(internal_hook.InferVerdict(c.stopReason, sigs, false))
			got := agent.ExportedInferTurnVerdict(c.stopReason, c.ok, c.total)
			if got != want {
				t.Errorf("stop=%q ok=%d/%d: 学习侧判 %q, 引擎侧判 %q "+
					"—— 两份实现漂移了, 改一处必须同步改另一处 (verdict_reward.go / trajectory.go)",
					c.stopReason, c.ok, c.total, got, want)
			}
		})
	}
}
