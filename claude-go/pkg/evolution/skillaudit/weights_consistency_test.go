package skillaudit

// 这个文件存在的唯一理由: auditFallbackWeights 是 pkg/agent.RewardSourceWeight 定值的
// **第三份副本** (另两份在 pkg/agent 与 pkg/evolution/learners)。副本会漂移, 而这一份
// 漂移的后果是治理闸的判据悄悄变松/变紧 —— 必须用测试焊住。
//
// 内部测试包 (package skillaudit): auditFallbackWeights 不导出, 外部测试包看不见它。
// import pkg/agent 不成环 —— 生产方向本来就是 skillaudit → govern → agent。

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

func TestAudit兜底权重与pkgAgent权威表一致(t *testing.T) {
	for src, got := range auditFallbackWeights {
		if want := agent.RewardSourceWeight(src); got != want {
			t.Errorf("源 %q 的兜底权重漂移: skillaudit %.2f, pkg/agent 权威值 %.2f "+
				"—— 改了 pkg/agent.RewardSourceWeight 就必须同步这里", src, got, want)
		}
	}
	// 未登记的源两边都必须落到同一个 unknown 档 (不给 0: 新源不该被静默忽略)。
	if agent.RewardSourceWeight("某个没登记过的源") != auditUnknownWeight {
		t.Errorf("未知源兜底权重不一致: skillaudit %.2f, pkg/agent %.2f",
			auditUnknownWeight, agent.RewardSourceWeight("某个没登记过的源"))
	}
	// 权威表里已登记的确定性门禁必须都在副本里 —— 漏一个就意味着它按 unknown(0.5)
	// 参与裁决, 一道满权重的确定性证据被打成半档。
	for _, src := range []string{
		agent.RewardSourceGateCompile, agent.RewardSourceGateTest,
		agent.RewardSourceGateE2E, agent.RewardSourceEpisode,
		agent.RewardSourceVerdictHeuristic,
	} {
		if _, ok := auditFallbackWeights[src]; !ok {
			t.Errorf("已登记的奖励源 %q 不在 skillaudit 兜底表里, 会被当成未知源 (0.5)", src)
		}
	}
}

// 权重必须真的参与裁决 —— 这条断言防的是"表加了但读侧没用"。
func TestAudit加权均值真的按权重算(t *testing.T) {
	rows := []rewardRow{
		{Value: -1, Source: agent.RewardSourceGateTest},        // 权重 1.0
		{Value: 1, Source: agent.RewardSourceVerdictHeuristic}, // 权重 0.15
		{Value: 1, Source: agent.RewardSourceVerdictHeuristic}, // 权重 0.15
	}
	var weighted, sum float64
	for _, r := range rows {
		weighted += r.weight() * r.Value
		sum += r.weight()
	}
	got := weighted / sum
	want := (-1.0 + 0.15 + 0.15) / 1.3
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("加权均值 = %.4f, want %.4f", got, want)
	}
	if got >= 0 {
		t.Errorf("两条弱信号不该把一条确定性否决翻正, got %.4f", got)
	}
}

// 落盘的 Weight 优先于源名查表: 权重表将来会调, 已发生的奖励应保留当时的可信度。
func TestAudit优先用落盘权重(t *testing.T) {
	r := rewardRow{Value: 1, Source: agent.RewardSourceGateTest, Weight: 0.11}
	if r.weight() != 0.11 {
		t.Errorf("应优先用落盘的 Weight, got %v", r.weight())
	}
}
