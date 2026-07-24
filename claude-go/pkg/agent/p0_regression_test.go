package agent

// P0 地基回归测试 (design/01 M0 / design/03 E0)。
// 守护三类修复: ① 双 mode switch 一致性 ② 嵌套重试收敛 ③ 团队学习/技能提炼辅助函数。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModeRoutingConsistency 守护 design/01 §1.2-1:
// 每个内置工作流的 Mode 必须是路由表已知的模式 —— 要么走 Coordinator 的
// pipeline 恢复 (pipeline/fanout/空), 要么在 dedicatedExecutorModes 表内。
// 新增 mode 时若忘了登记路由表, 本测试立即失败, 不再出现
// app_composite/game_composite 式"静默降级为普通 pipeline"的缺陷。
func TestModeRoutingConsistency(t *testing.T) {
	pipelineModes := map[string]bool{"": true, "pipeline": true, "fanout": true}
	for _, wf := range ListWorkflows() {
		if pipelineModes[wf.Mode] {
			continue
		}
		if !ModeHasDedicatedExecutor(wf.Mode) {
			t.Errorf("工作流 %q 的 mode %q 未在 dedicatedExecutorModes 登记, "+
				"将被静默降级为普通 pipeline 恢复路径 (design/01 §1.2-1)", wf.Name, wf.Mode)
		}
	}
}

// TestDedicatedExecutorModesExact 守护路由表内容本身:
// 意外删行会导致对应 mode 静默降级, 意外加行会让 pipeline 类 mode 丢失检查点恢复。
func TestDedicatedExecutorModesExact(t *testing.T) {
	want := []string{
		"adversarial", "adversarial_dev", "orchestrated", "trading_debate",
		"creative_media", "novel_writing", "swarm_novel", "plot_simulate",
		"plot_predict", "ensemble_extract", "review_panel",
		"app_composite", "game_composite",
	}
	if len(dedicatedExecutorModes) != len(want) {
		t.Fatalf("dedicatedExecutorModes 数量 %d != 期望 %d; 若有意增删请同步更新本测试与 design/01",
			len(dedicatedExecutorModes), len(want))
	}
	for _, m := range want {
		if !ModeHasDedicatedExecutor(m) {
			t.Errorf("mode %q 应在专用执行器表内", m)
		}
	}
	if ModeHasDedicatedExecutor("pipeline") || ModeHasDedicatedExecutor("fanout") {
		t.Error("pipeline/fanout 不应在专用执行器表内 (应走 Coordinator 检查点恢复)")
	}
}

// TestOuterRetryDrivenContext 守护 design/01 §1.2-4 嵌套重试收敛的标记机制。
func TestOuterRetryDrivenContext(t *testing.T) {
	ctx := context.Background()
	if isOuterRetryDriven(ctx) {
		t.Fatal("未标记的 ctx 不应视为外层驱动")
	}
	if !isOuterRetryDriven(WithOuterRetryDriven(ctx)) {
		t.Fatal("标记后的 ctx 应视为外层驱动")
	}
}

func TestAllStagesCompleted(t *testing.T) {
	ok := []StageResult{{Status: TaskCompleted}, {Status: TaskCompleted}}
	if !allStagesCompleted(ok) {
		t.Error("全部完成应返回 true")
	}
	mixed := []StageResult{{Status: TaskCompleted}, {Status: TaskFailed}}
	if allStagesCompleted(mixed) {
		t.Error("含失败阶段应返回 false")
	}
	if !allStagesCompleted(nil) {
		t.Error("空切片按约定视为 true (调用方已用 len>0 守卫)")
	}
}

func TestLastNonEmptyOutput(t *testing.T) {
	results := []StageResult{
		{Output: "first"},
		{Output: "  "},
		{Output: "final answer"},
		{Output: ""},
	}
	if got := lastNonEmptyOutput(results, 100); got != "final answer" {
		t.Errorf("应取最后一个非空产出, got %q", got)
	}
	if got := lastNonEmptyOutput(nil, 100); got != "" {
		t.Errorf("空输入应返回空串, got %q", got)
	}
}

// TestRecordRewardPersists 守护 design/03 §4.2 奖励落盘雏形:
// rewards.jsonl 追加写、字段齐全、value 归一化。
func TestRecordRewardPersists(t *testing.T) {
	dir := t.TempDir()
	ee := NewEvolutionEngine(dir, nil)
	ee.RecordReward(RewardEvent{RunID: "run-x", Source: "gate.content", Value: 0.6, Raw: 80, Team: "tm"})
	ee.RecordReward(RewardEvent{Source: "episode", Value: 1.0, Raw: "completed"})

	data, err := os.ReadFile(filepath.Join(dir, "rewards.jsonl"))
	if err != nil {
		t.Fatalf("rewards.jsonl 未写入: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 条奖励事件, got %d", len(lines))
	}
	var ev RewardEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if ev.RunID != "run-x" || ev.Source != "gate.content" || ev.Value != 0.6 || ev.TS == 0 {
		t.Fatalf("字段不完整: %+v", ev)
	}
	// nil 引擎防御
	var nilEE *EvolutionEngine
	nilEE.RecordReward(RewardEvent{Source: "x"})
}
