// 外部测试包 (package learners_test): 它可以 import pkg/agent 而不成环 ——
// 生产代码方向是 pkg/agent → learners, 反向 import 只发生在测试二进制里。
//
// 这个文件存在的唯一理由: data.go 的 fallbackWeights 是 pkg/agent.RewardSourceWeight
// 定值的副本 (本包不能反向依赖 pkg/agent)。副本会漂移, 所以用测试把它焊住。
package learners_test

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

// 每个已登记的奖励源, learners 的兜底权重必须与 pkg/agent 的权威表一致。
//
// 校验手法: 构造一条 Weight 缺失的奖励 (老数据), 走 ScoreRuns 得到加权分。
// 单条奖励时加权分 = value, 看不出权重; 所以配一条固定权重的对照事件, 用两条的
// 加权均值反解出被测源的权重。
func TestFallbackWeights与pkgAgent权威表一致(t *testing.T) {
	sources := []string{
		"gate.compile", "gate.test", "gate.lint", "gate.e2e",
		"user.explicit", "user.steer", "user.feedback",
		"gate.content", "review.panel", "gate.review", "llm.judge",
		"episode", "latency", "cost", "verdict.heuristic",
		"某个没登记过的源", // 未知源也要一致 (都应是 0.5)
	}
	const anchorWeight = 1.0 // gate.compile 的权重, 用作对照锚
	for _, src := range sources {
		want := agent.RewardSourceWeight(src)
		// 被测源 value=1, 锚源 value=-1 ⇒ score = (w*1 + 1*(-1))/(w+1)
		rows := []learners.RewardRow{
			{TS: 1, RunID: "r", NodeID: "a", Source: src, Value: 1},
			{TS: 2, RunID: "r", NodeID: "b", Source: "gate.compile", Value: -1},
		}
		sc := learners.ScoreRuns(rows)["r"]
		if sc.Count != 2 {
			t.Fatalf("源 %q: 应有 2 条证据, got %d", src, sc.Count)
		}
		got := solveWeight(sc.Score, anchorWeight)
		if diff := got - want; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("源 %q 的兜底权重漂移: learners 反解 %.4f, pkg/agent 权威值 %.4f "+
				"—— 改了 pkg/agent.RewardSourceWeight 就必须同步 learners.fallbackWeights",
				src, got, want)
		}
	}
}

// solveWeight 从 score=(w-anchor)/(w+anchor) 反解 w。
func solveWeight(score, anchor float64) float64 {
	// score*(w+anchor) = w - anchor  ⇒  w*(score-1) = -anchor*(score+1)
	return -anchor * (score + 1) / (score - 1)
}
