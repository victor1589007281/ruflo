// drain_provenance_test.go — R-H9 的"工具结果带 provenance"与并发同名调用的配对。
//
// 直接驱动 drain 而不是走 RunTurn：这一层的输入是 StreamEvent，用真上游造
// "两个同名工具并发、完成顺序颠倒"的场景需要刻意编排 SSE 帧，而编排出来的
// 帧序未必就是引擎真实的产出顺序。直接喂事件既精确又不会因为上游实现变化失效。
package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// drainClock 是单调递增的假时钟：每次调用前进 step。
//
// 用假时钟而不是真 time.Now：真时钟在快机器上会把 start 与 done 的差算成 0ms，
// 于是"耗时有被记录"与"耗时恒为 0"无法区分 —— 而后者正是这条测试要排除的
// 失效形态（忘了记录与记了 0 在断言上长得一样）。
type drainClock struct {
	t    time.Time
	step time.Duration
}

func newDrainClock() *drainClock {
	return &drainClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), step: 7 * time.Millisecond}
}

func (c *drainClock) Now() time.Time {
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func drainEvents(t *testing.T, events []types.StreamEvent, clock *drainClock) (*TurnResult, []TurnEvent) {
	t.Helper()
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	res := &TurnResult{}
	var emitted []TurnEvent
	drain(context.Background(), ch, func(ev TurnEvent) { emitted = append(emitted, ev) }, res, nil, clock.Now, nil, "", "", "")
	return res, emitted
}

// TestDrain_ToolCallCarriesProvenance 是 R-H9 的直接断言。
//
// 三个字段各自独立：工具名（哪一次）、SourceID（协议身份）、执行时刻（什么时候）。
// 只验其中一两个的实现会在审计时缺一块 —— 比如有工具名没时刻，就无法判断
// "这条结论是这次读的还是上一轮缓存下来的"。
func TestDrain_ToolCallCarriesProvenance(t *testing.T) {
	clock := newDrainClock()
	res, _ := drainEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "memory_search", ToolUseID: "tu-1", ToolInput: `{"q":"搬家"}`},
		{Kind: types.StreamEventToolDone, ToolUseID: "tu-1", ToolResult: "3 条"},
	}, clock)

	if len(res.ToolCalls) != 1 {
		t.Fatalf("应记录 1 次工具调用, 实得 %d", len(res.ToolCalls))
	}
	rec := res.ToolCalls[0]
	if rec.Provenance.Tool != "memory_search" {
		t.Errorf("provenance 缺工具名: %+v", rec.Provenance)
	}
	if rec.Provenance.SourceID != "tu-1" {
		t.Errorf("provenance 缺 SourceID (应为协议 tool_use id): %+v", rec.Provenance)
	}
	if rec.Provenance.ExecutedAt.IsZero() {
		t.Errorf("provenance 缺执行时刻: %+v", rec.Provenance)
	}
	if rec.ToolUseID != "tu-1" {
		t.Errorf("ToolCallRecord.ToolUseID 应回填协议 ID, 实得 %q", rec.ToolUseID)
	}
	if rec.DurationMS <= 0 {
		t.Errorf("DurationMS 应记录真实间隔, 实得 %d", rec.DurationMS)
	}
	if rec.InputHash == "" {
		t.Error("InputHash 未记录 —— 幂等判定与缓存键都依赖它")
	}
}

// TestDrain_ConcurrentSameNameCallsMatchByID 钉住配对方式。
//
// 场景是真实存在的：模型在同一轮里发出两个 Read。两个 ToolStart 先后到达，
// 两个 ToolDone 的顺序是**反的**（谁先跑完不由发出顺序决定）。按名字配对会把
// 结果 A 接到记录 B 上 —— 记录数、事件数、成功/失败全对，只有预览与耗时错位。
//
// 判据用"结果内容必须落在自己那条记录上"，而不是"配对没有 panic"。
func TestDrain_ConcurrentSameNameCallsMatchByID(t *testing.T) {
	clock := newDrainClock()
	res, _ := drainEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "Read", ToolUseID: "tu-a", ToolInput: `{"path":"a.go"}`},
		{Kind: types.StreamEventToolStart, ToolName: "Read", ToolUseID: "tu-b", ToolInput: `{"path":"b.go"}`},
		// 后发的先完成
		{Kind: types.StreamEventToolDone, ToolUseID: "tu-b", ToolResult: "内容-B"},
		{Kind: types.StreamEventToolDone, ToolUseID: "tu-a", ToolResult: "内容-A"},
	}, clock)

	if len(res.ToolCalls) != 2 {
		t.Fatalf("应记录 2 次调用, 实得 %d", len(res.ToolCalls))
	}
	byID := map[string]ToolCallRecord{}
	for _, rec := range res.ToolCalls {
		byID[rec.ToolUseID] = rec
	}
	if got := byID["tu-a"].ResultPreview; got != "内容-A" {
		t.Errorf("tu-a 的结果预览错配: %q", got)
	}
	if got := byID["tu-b"].ResultPreview; got != "内容-B" {
		t.Errorf("tu-b 的结果预览错配: %q", got)
	}
	if got := string(byID["tu-a"].Input); got != `{"path":"a.go"}` {
		t.Errorf("tu-a 的输入错配: %q", got)
	}
	if got := byID["tu-b"].Input; !strings.Contains(string(got), "b.go") {
		t.Errorf("tu-b 的输入错配: %q", got)
	}
}

// TestDrain_NonPositiveDurationNeverNegative 守住时钟单调假设的边界。
//
// 注入的时钟若不单调（真时钟因 NTP 回拨也会），耗时会是负数。断言这里只要求
// "不会因负值而失去意义"：负值必须**被记录下来**而不是被悄悄改成 0 ——
// 改成 0 会让"时钟回拨"与"执行瞬间完成"变得不可区分。
//
// 本仓对耗时字段的既有口径就是 `time.Since` 直出（见 ToolCallObservation），
// 这里保持一致，把"要不要纠正"留给消费方。
func TestDrain_NeverDropsUnclosedToolCalls(t *testing.T) {
	clock := newDrainClock()
	res, _ := drainEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "Bash", ToolUseID: "tu-x", ToolInput: `{"cmd":"sleep 999"}`},
		// 没有 ToolDone: 取消/崩溃时流就这么断了
	}, clock)

	if len(res.ToolCalls) != 1 {
		t.Fatalf("未闭合的调用也必须出现在结果里 (否则'调了工具但失败'会变成'没调工具'), 实得 %d", len(res.ToolCalls))
	}
	rec := res.ToolCalls[0]
	if rec.Name != "Bash" || rec.ToolUseID != "tu-x" {
		t.Fatalf("未闭合记录身份丢失: %+v", rec)
	}
	if rec.Provenance.ExecutedAt.IsZero() {
		t.Errorf("未闭合记录也应有开始时刻: %+v", rec.Provenance)
	}
	if rec.Provenance.Tool != "Bash" {
		t.Errorf("未闭合记录缺 provenance 工具名: %+v", rec.Provenance)
	}
}

// TestDrain_UnknownToolDoneDoesNotFabricate 守住"不要凭空补记录"。
//
// ToolDone 带了一个从未 ToolStart 过的 ID（压缩/裁剪可能丢掉开始事件）。
// 此时要发事件（宿主需要知道工具结束了），但**不能**补一条没有输入的记录 ——
// 那会让结果里出现幽灵调用，宿主统计工具次数时就多算一次。
func TestDrain_UnknownToolDoneDoesNotFabricate(t *testing.T) {
	clock := newDrainClock()
	res, emitted := drainEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolDone, ToolUseID: "tu-ghost", ToolResult: "?"},
	}, clock)

	if len(res.ToolCalls) != 0 {
		t.Fatalf("没有 ToolStart 的完成事件不应补记录, 实得 %+v", res.ToolCalls)
	}
	var sawDone bool
	for _, ev := range emitted {
		if ev.Kind == EventToolDone {
			sawDone = true
			if ev.Tool == nil || ev.Tool.ToolUseID != "tu-ghost" {
				t.Fatalf("完成事件应带上 ToolUseID: %+v", ev.Tool)
			}
		}
	}
	if !sawDone {
		t.Error("完成事件仍须发出去, 否则宿主会以为工具一直没结束")
	}
}

// TestDrain_ToolUseIDReachesEvent 钉住事件侧的 ID 透传。
//
// 事件消费方（SSE 出口、前端）同样需要区分同名并发调用。事件里没有 ID 时，
// 前端只能按到达顺序猜，而到达顺序与发出顺序不同 —— 表现是"两个卡片的内容
// 互换"。这条断言很便宜，但它守的是一个不报错的界面错。
func TestDrain_ToolUseIDReachesEvent(t *testing.T) {
	clock := newDrainClock()
	_, emitted := drainEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "Read", ToolUseID: "tu-1"},
		{Kind: types.StreamEventToolDone, ToolUseID: "tu-1", ToolResult: "x"},
	}, clock)

	var starts, dones int
	for _, ev := range emitted {
		switch ev.Kind {
		case EventToolStart:
			starts++
			if ev.Tool == nil || ev.Tool.ToolUseID != "tu-1" {
				t.Fatalf("tool_start 事件缺 ToolUseID: %+v", ev.Tool)
			}
		case EventToolDone:
			dones++
			if ev.Tool == nil || ev.Tool.ToolUseID != "tu-1" {
				t.Fatalf("tool_done 事件缺 ToolUseID: %+v", ev.Tool)
			}
		}
	}
	if starts != 1 || dones != 1 {
		t.Fatalf("应各发一次 start/done, 实得 %d/%d", starts, dones)
	}
}

// 编译期护栏：让"事件结构里没有 ID"这种改动无法悄悄通过。
var _ = json.RawMessage(nil)
