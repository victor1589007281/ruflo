package dashboard

import (
	"context"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

// fakeSink 只满足接口, 不做事。
type fakeSink struct{}

func (fakeSink) ConsumeActions(context.Context) (agent.ActionConsumeStats, error) {
	return agent.ActionConsumeStats{}, nil
}

// 注册了消费方后, team/swarm 动作必须由队列独占 —— 否则同一条动作走两条路:
// handleAction 先直接调 TeamAction, drainQueuedAction 再让消费方跑一次。
// 真集群 E2E 里表现为回包自相矛盾("已提交任务" + "操作失败: 正在执行中")。
func TestActionSinkOwns_注册后队列独占(t *testing.T) {
	SetActionSink(nil)
	t.Cleanup(func() { SetActionSink(nil) })

	// 未注册: 直接执行路径行为不变 (向后兼容)
	for _, k := range []string{"team", "swarm", "cron"} {
		if actionSinkOwns(k, "run") {
			t.Errorf("未注册消费方时 %s 不该让路", k)
		}
	}

	SetActionSink(fakeSink{})
	for _, tc := range []struct {
		kind, action string
		want         bool
	}{
		{"team", "run", true},
		{"team", "create", true},
		{"team", "refine", true},
		{"team", "stop", true},
		{"swarm", "create", true},
		{"swarm", "predict", true},
		// cron 的几个 case 在 handleAction 里是**直接写盘**(toggleCron/cronCreate),
		// 消费方不做这件事, 让路会让它们彻底不生效 —— 故必须仍走直接路径。
		{"cron", "enable", false},
		{"cron", "create", false},
		{"cron", "delete", false},
	} {
		if got := actionSinkOwns(tc.kind, tc.action); got != tc.want {
			t.Errorf("actionSinkOwns(%q,%q) = %v, 期望 %v", tc.kind, tc.action, got, tc.want)
		}
	}
}
