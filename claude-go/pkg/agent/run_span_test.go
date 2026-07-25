package agent

import (
	"context"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// run Span 必须让一次 run 能**自述**: objective / 终态 / 工作流 / 阶段数 / 耗时。
// 这正是"由 TraceID 隐含"这个旧判断不成立的地方 —— TraceID 只给身份, 给不了事实。
func TestWriteRunSpan_一次run可自述(t *testing.T) {
	ptm, team, _, ts := newRewardPTM(t, true)
	team.Workflow = "development"
	team.Objective = "实现登录接口"
	team.Status = TeamStatusCompleted
	team.Stages = []StageResult{
		{Name: "impl", Status: TaskCompleted},
		{Name: "review", Status: TaskFailed},
	}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-r"})

	ptm.writeRunSpan(ctx, team, time.Now().Add(-3*time.Second))

	spans := readSpans(t, ts, "run-r")
	if len(spans) != 1 {
		t.Fatalf("应写出 1 条 run Span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Kind != tracestore.KindRun {
		t.Fatalf("Kind = %q, want %q", sp.Kind, tracestore.KindRun)
	}
	if sp.ParentID != "" {
		t.Error("run 是这棵树的根, 不该有 ParentID")
	}
	if sp.Attrs["status"] != string(TeamStatusCompleted) || sp.Attrs["workflow"] != "development" {
		t.Errorf("终态/工作流缺失: %+v", sp.Attrs)
	}
	if sp.Attrs["stages"].(float64) != 2 || sp.Attrs["stages_failed"].(float64) != 1 {
		t.Errorf("阶段统计不对: %+v", sp.Attrs)
	}
	if obj, err := ts.Resolve(sp.InputRef); err != nil || obj != "实现登录接口" {
		t.Errorf("InputRef 应还原 objective, got %q err=%v", obj, err)
	}
	if sp.DurMS < 2000 {
		t.Errorf("耗时应从 run 起点算 (给的是 3 秒前), got %d ms", sp.DurMS)
	}
}

// 失败的 run 也必须留下 run Span —— 那恰恰是学习最想要的一批。
func TestWriteRunSpan_失败run也留痕(t *testing.T) {
	ptm, team, _, ts := newRewardPTM(t, true)
	team.Status = TeamStatusFailed
	team.Error = "全局编译门禁失败: undefined: Foo"
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-f"})

	ptm.writeRunSpan(ctx, team, time.Now())

	spans := readSpans(t, ts, "run-f")
	if len(spans) != 1 {
		t.Fatalf("失败的 run 也要写 Span, got %d 条", len(spans))
	}
	if spans[0].Attrs["error"] == nil {
		t.Errorf("失败原因必须进 Attrs, got %+v", spans[0].Attrs)
	}
}

// 无 trace 的裸调不产孤儿 Span。
func TestWriteRunSpan_无RunID时不写(t *testing.T) {
	ptm, team, _, ts := newRewardPTM(t, true)
	ptm.writeRunSpan(context.Background(), team, time.Now())
	if spans, _ := ts.ReadRun(""); len(spans) != 0 {
		t.Errorf("无 RunID 时不该写出挂不上任何桶的 Span, got %d", len(spans))
	}
}
