package agent

import (
	"testing"
	"time"
)

// 自定义角色名 (工作流里的 poster-* 一类) 在 computeStageTimeout 的角色表里没有
// 条目, 一律落到默认 10 分钟。阶段声明了 timeoutSec=1800 时, 声明必须是下限 ——
// 否则审计阶段永远在 10 分钟被 ctx 掐断, 报告只写骨架。这是 2026-11-25 poster
// 批的实测故障 (logic-audit / adversarial 的发现全部丢失)。
func TestStageAttemptTimeout_声明压过角色表(t *testing.T) {
	we := &WorkflowExecutor{}
	if got := stageAttemptTimeout(we, "poster-logic-auditor", 0, 1800, false); got != 30*time.Minute {
		t.Fatalf("声明 1800s 应得 30 分钟, 实得 %s", got)
	}
	// 限流放宽 (1.5x, 45 分钟硬顶) 不允许把声明压回去。
	if got := stageAttemptTimeout(we, "poster-adversary", 0, 2400, true); got != 40*time.Minute {
		t.Fatalf("声明 2400s 在限流轮也应保持 40 分钟, 实得 %s", got)
	}
}

// 没有声明的阶段必须与改造前逐字节一致: coder 25 分钟、未知角色 10 分钟;
// 声明比角色超时更小时不缩短 (下限语义, 不是覆盖)。
func TestStageAttemptTimeout_未声明沿用角色表(t *testing.T) {
	we := &WorkflowExecutor{}
	if got := stageAttemptTimeout(we, "coder", 0, 0, false); got != 25*time.Minute {
		t.Fatalf("coder 未声明应为 25 分钟, 实得 %s", got)
	}
	if got := stageAttemptTimeout(we, "poster-intake", 0, 0, false); got != 10*time.Minute {
		t.Fatalf("未知角色未声明应为默认 10 分钟, 实得 %s", got)
	}
	if got := stageAttemptTimeout(we, "coder", 0, 60, false); got != 25*time.Minute {
		t.Fatalf("声明 60s 不应把 coder 缩短到 1 分钟, 实得 %s", got)
	}
	// 重试放大仍然生效 (20%/attempt), 声明为 0 时不干预。
	// 放大走的是浮点乘 (25m × 1.4), 会带亚微秒误差, 故按秒比较。
	if got := stageAttemptTimeout(we, "coder", 2, 0, false); got.Round(time.Second) != 35*time.Minute {
		t.Fatalf("coder 第 2 次重试应为 35 分钟, 实得 %s", got)
	}
}
