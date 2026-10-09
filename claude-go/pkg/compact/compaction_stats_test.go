// compaction_stats_test.go —— H2-1 折叠读数的单测。
//
// 验收判据 (方案 §21 H2-1 原文): **「跑一个会压缩的 run, 压缩相关 span 数 > 0,
// 且能读出压缩前后 token 数两者都非零」**。
//
// 本文件钉的是"读数从折叠内部**算对了**", 端到端那一半 (span 真的落了盘) 在
// pkg/engine/trace_compaction_test.go —— 两层分开是因为折叠逻辑本身可以直接调,
// 而 span 落盘要一条完整的 queryLoop。两边的断言刻意**不共用**任何辅助函数:
// 共用的话, 一个算错的辅助函数会让两层一起假绿。
package compact

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// statsStore 装一个只收数的 CompactionSink 替身。
type statsStore struct {
	got []CompactionStats
	ctx []context.Context
}

// capture 起一个带假上游的 compactor 并接上回调。
func withStats(t *testing.T, maxCtx int) (*Compactor, *statsStore, func()) {
	t.Helper()
	var calls int64
	c, closeSrv := newTestCompactor(t, maxCtx, 0, nil)
	_ = calls
	ss := &statsStore{}
	c.SetOnCompact(func(ctx context.Context, st CompactionStats) {
		ss.got = append(ss.got, st)
		ss.ctx = append(ss.ctx, ctx)
	})
	return c, ss, closeSrv
}

// TestH21_StatsBothTokenCountsNonZero 是 H2-1 的核心判据在包内的那一半:
// 一次真实折叠必须报出**前后两个都非零**的 token 数。
//
// 为什么"非零"是这条判据的全部内容: tokens_after 为 0 的成因只有两个 —— 要么
// 折叠把上下文清空了 (那是数据丢失不是压缩), 要么口径漏算 (只数 Text 不数
// tool_result 体 —— 而 tool_result 恰是长会话里体量最大的那块)。两种成因在读数
// 上同形, 所以判据必须把"非零"写死而不是"after < before"。
func TestH21_StatsBothTokenCountsNonZero(t *testing.T) {
	c, ss, closeSrv := withStats(t, 3000)
	defer closeSrv()

	// 阈值 0.8*3000 = 2400; 10 条 × 2000 字符 / 4 = 5000 tokens, 稳稳越过。
	msgs := bigHistory(10, 2000)
	if _, err := c.AutoCompact(context.Background(), msgs, "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}

	if len(ss.got) != 1 {
		t.Fatalf("只折了一次却报了 %d 条读数 —— 报告条数与实际折叠次数必须一一对应", len(ss.got))
	}
	st := ss.got[0]
	if st.TokensBefore <= 0 {
		t.Fatalf("折叠前 token = %d, 必须为正 (0 说明口径漏算或消息为空)", st.TokensBefore)
	}
	if st.TokensAfter <= 0 {
		t.Fatalf("折叠后 token = %d, 必须为正 —— 0 与'口径漏算'不可区分", st.TokensAfter)
	}
	// 对照臂: 这两个数**不能是同一个**。若 TokensAfter 直接抄了 TokensBefore,
	// 上面两条"非零"断言照样全绿, 而读数完全没信息量。
	if st.TokensBefore == st.TokensAfter {
		t.Fatalf("折叠前后 token 数相同 (%d) —— 读数没有反映折叠效果", st.TokensBefore)
	}
	if st.TokensAfter >= st.TokensBefore {
		t.Fatalf("折叠后 %d 未小于折叠前 %d —— 这次折叠没有压下去", st.TokensAfter, st.TokensBefore)
	}
}

// TestH21_StatsMessageCountsAreConsistent 条数读数必须自洽。
func TestH21_StatsMessageCountsAreConsistent(t *testing.T) {
	c, ss, closeSrv := withStats(t, 3000)
	defer closeSrv()

	const n = 10
	msgs := bigHistory(n, 2000)
	if _, err := c.AutoCompact(context.Background(), msgs, "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if len(ss.got) != 1 {
		t.Fatalf("读数条数 = %d, 期望 1", len(ss.got))
	}
	st := ss.got[0]

	if st.MessagesBefore != n {
		t.Fatalf("折叠前条数 = %d, 期望 %d", st.MessagesBefore, n)
	}
	// 尾部保护 6 条 + boundary + summary = 8。
	if st.MessagesAfter != st.TailCount+2 {
		t.Fatalf("折叠后条数 = %d, 期望 tail(%d)+boundary+summary(%d)",
			st.MessagesAfter, st.TailCount, st.TailCount+2)
	}
	if st.MessagesCompacted != n-st.TailCount {
		t.Fatalf("被摘要条数 = %d, 期望 %d (总 %d - 尾部保护 %d)",
			st.MessagesCompacted, n-st.TailCount, n, st.TailCount)
	}
	if st.MessagesCompacted <= 0 {
		t.Fatal("被摘要条数 <= 0 —— 那这次就不该被叫做折叠")
	}
	if st.SummaryTokens <= 0 {
		t.Fatalf("摘要 token = %d, 必须为正 —— 报 0 等于说'摘要不存在'", st.SummaryTokens)
	}
}

// TestH21_NoCompactionNoReport 对照臂: 没折成的时候**一条读数都不该有**。
//
// 少了这一臂, "读数条数 > 0"会被任何无条件上报满足 —— 例如把回调挪到
// AutoCompact 的开头。那种实现让"压缩发生了多少次"这个读数恒等于"AutoCompact
// 被调用过多少次", 而长程任务里这两者差着一个数量级。
func TestH21_NoCompactionNoReport(t *testing.T) {
	c, ss, closeSrv := withStats(t, 1000000)
	defer closeSrv()

	// 远低于阈值: AutoCompact 直接返回 nil, 不折叠。
	if out, err := c.AutoCompact(context.Background(), bigHistory(3, 100), "test"); err != nil || out != nil {
		t.Fatalf("阈值下不该折叠: out=%v err=%v", out, err)
	}
	if len(ss.got) != 0 {
		t.Fatalf("没折叠却报了 %d 条读数 —— 读数条数不再等价于折叠次数", len(ss.got))
	}
}

// TestH21_CallbackFiresOutsideWindow 回调必须在**折叠窗口关闭之后**触发。
//
// 这是死锁判据, 不是风格问题: 回调实现方 (引擎侧) 要走 GateEventWrite 落轨迹,
// 那是读锁; 窗口是写锁。回调若持着窗口跑, 生产上第一次折叠就自锁 —— 而单测里
// 不接 GateEventWrite 的话这条永远测不出来, 现场表现是"跑一会就卡死"。
// 判据是**回调跑完之后**折叠窗口的状态, 而不是"回调里读到的 Compacting()" ——
// 后者在回调持锁跑到一半时读到的中间态, 说不清是被谁开的。回调自己会取
// GateEventWrite 那道门 (生产实现方必须走的那条路), 所以窗口没关的话这里不是
// "返回 true"而是**直接死锁** —— 用一个有上限的通道把它变成一条能在秒级报出来的
// 失败, 而不是等 go test 的 10 分钟兜底 (那个超时信息里只有一堆 goroutine dump,
// 看不出是哪条纪律破了)。
func TestH21_CallbackFiresOutsideWindow(t *testing.T) {
	c, closeSrv := newTestCompactor(t, 3000, 0, nil)
	defer closeSrv()

	done := make(chan bool, 1)
	c.SetOnCompact(func(ctx context.Context, st CompactionStats) {
		// 真实现方要走的门: 窗口若仍开着, 这里是读锁撞写锁, 永久阻塞。
		c.GateEventWrite(func() {})
		done <- c.Compacting()
	})

	go func() {
		_, _ = c.AutoCompact(context.Background(), bigHistory(10, 2000), "test")
	}()

	select {
	case windowOpen := <-done:
		if windowOpen {
			t.Fatal("回调跑完时折叠窗口仍开着 —— 回调持写锁触发, " +
				"生产实现方走 GateEventWrite 会死锁")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("回调没能跑完 —— 折叠窗口内触发回调导致 GateEventWrite 死锁 " +
			"(回调必须排在 runCompaction 的写锁之外)")
	}
}

// TestH21_MicroStageCountsRefs tool_result 折叠的计数必须分清"折了几条"与
// "其中几条留下了可回读引用" —— 两者的差就是永久丢失的结果条数。
func TestH21_MicroStageCountsRefs(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStoreForCwd(dir)

	big := strings.Repeat("A", MicroCompactThreshold+5000)
	small := strings.Repeat("B", MicroCompactThreshold+5000)
	thin := "tiny" // 低于阈值, 不该被折

	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c1", Content: big},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c2", Content: small},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c3", Content: thin},
		}},
	}

	out, counts := StubToolResultsCounted(msgs, 0, -1, store)

	if counts.Folded != 2 {
		t.Fatalf("折叠条数 = %d, 期望 2 (只有两条超过阈值)", counts.Folded)
	}
	if counts.Refs != 2 {
		t.Fatalf("给出引用条数 = %d, 期望 2 —— 带 store 时每条都应留下可回读路径", counts.Refs)
	}
	// 对照臂: 低于阈值的那条必须原样保留, 否则"折了 2 条"这个数就没有下界意义。
	if out[2].Content[0].Content != thin {
		t.Fatal("低于阈值的 tool_result 被折了 —— 阈值没生效")
	}
}

// TestH21_MicroStageWithoutStoreLosesEverything 对照臂: 没有落盘目录时
// "折了 2 条、留下 0 条引用" —— 这正是 refs_lost 要暴露的那个数。
func TestH21_MicroStageWithoutStoreLosesEverything(t *testing.T) {
	big := strings.Repeat("A", MicroCompactThreshold+5000)
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c1", Content: big},
		}},
	}
	_, counts := StubToolResultsCounted(msgs, 0, -1, nil)
	if counts.Folded != 1 {
		t.Fatalf("折叠条数 = %d, 期望 1", counts.Folded)
	}
	if counts.Refs != 0 {
		t.Fatalf("无落盘目录却报了 %d 条引用 —— 占位里的路径是空话", counts.Refs)
	}
}

// TestH21_FoldStageReportsZeroStubs 把"被摘要掉的历史里的 tool_result 不会留下
// 引用"这件事钉成读数, 而不是留成一句注释。
//
// 折叠边界的产物是 [boundary, summary, ...tail], 其中 boundary 与 summary 都是
// 纯文本块 —— StubToolResultsCounted 在那个区间里一条 tool_result 都遇不到。于是
// fold 阶段的 stub 计数恒为 0。**这不是缺陷也不需要修**: 被摘要掉的历史本就随历史
// 消失, 要让它可回读就等于不许摘要。但如果这个数没人报出来, "折叠会保住工具结果
// 引用"就成了一句对 fold 阶段也成立的错误印象。
//
// 这条断言的价值在于它是**可证伪的**: 哪天有人把折叠改成"先把 toCompress 里的
// tool_result 折一遍再摘要", 这条会红 —— 那是一个行为变更, 应该有人显式确认。
func TestH21_FoldStageReportsZeroStubs(t *testing.T) {
	dir := t.TempDir()
	c, ss, closeSrv := withStats(t, 3000)
	defer closeSrv()
	c.SetToolResultRefStore(dir)

	// 历史里塞一条超大 tool_result: 它会被摘要掉, 不该在读数里算成"折了 1 条"。
	big := strings.Repeat("A", MicroCompactThreshold+5000)
	msgs := bigHistory(8, 2000)
	msgs = append(msgs, types.Message{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "c-big", Content: big},
		},
	})

	if _, err := c.AutoCompact(context.Background(), msgs, "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if len(ss.got) != 1 {
		t.Fatalf("读数条数 = %d, 期望 1", len(ss.got))
	}
	st := ss.got[0]
	if st.Stub.Folded != 0 {
		t.Fatalf("fold 阶段报了 %d 条 tool_result 折叠 —— 折叠边界产区里没有 tool_result, "+
			"这个数不该非零; 若确实改了折叠形状, 请显式确认后更新本断言", st.Stub.Folded)
	}
	if st.Stub.Refs != 0 {
		t.Fatalf("fold 阶段报了 %d 条引用, 与 Folded=0 自相矛盾", st.Stub.Refs)
	}
	// 上面那条被摘要掉的超大结果确实进了被压缩的区间 —— 否则本测试测的是个空场景。
	if st.MessagesCompacted < 1 {
		t.Fatal("没有任何消息被摘要, 这条测试没有构造出它要测的场景")
	}
}

// TestH21_CallbackNilSafe 没装回调时行为与改造前一致 (不 panic, 不报数)。
func TestH21_CallbackNilSafe(t *testing.T) {
	c, closeSrv := newTestCompactor(t, 3000, 0, nil)
	defer closeSrv()
	out, err := c.AutoCompact(context.Background(), bigHistory(10, 2000), "test")
	if err != nil || out == nil {
		t.Fatalf("没装回调时折叠应当照常完成: out=%v err=%v", out != nil, err)
	}
	// nil Compactor 上的两个新方法也必须是安全的 (fail-open 与既有纪律一致)。
	var nilC *Compactor
	nilC.SetOnCompact(func(context.Context, CompactionStats) {})
	if nilC.Compacting() {
		t.Fatal("nil Compactor 报告窗口打开")
	}
}

// TestH21_StatsDoNotLeakAcrossCompactions 两次折叠各报各的, 不累加也不串台。
func TestH21_StatsDoNotLeakAcrossCompactions(t *testing.T) {
	c, ss, closeSrv := withStats(t, 3000)
	defer closeSrv()

	ctx := context.Background()
	if _, err := c.AutoCompact(ctx, bigHistory(10, 2000), "test"); err != nil {
		t.Fatal(err)
	}
	// 第二次: 条数不同, 读数必须跟着变 —— 若回调里复用了同一个 stats 变量并
	// 只填部分字段, 第二次就会带上第一次的残值。
	if _, err := c.AutoCompact(ctx, bigHistory(30, 2000), "test"); err != nil {
		t.Fatal(err)
	}
	if len(ss.got) != 2 {
		t.Fatalf("读数条数 = %d, 期望 2", len(ss.got))
	}
	if ss.got[0].MessagesBefore == ss.got[1].MessagesBefore {
		t.Fatalf("两次折叠的前置条数相同 (%d) —— 第二次读数是第一次的残值, 不是实测",
			ss.got[0].MessagesBefore)
	}
	if ss.got[1].MessagesBefore != 30 {
		t.Fatalf("第二次折叠前条数 = %d, 期望 30", ss.got[1].MessagesBefore)
	}
	_ = time.Now
}
