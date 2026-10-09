package internal_hook

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/types"
)

// summaryMessages 造一段"已经压缩过"的消息序列: boundary + summary + tail。
// 这里直接构造而不是跑 runCompaction —— 后者要打真实 LLM。
func summaryMessages(tailTexts ...string) []types.Message {
	msgs := []types.Message{
		{
			Type:              types.MessageTypeSystem,
			UUID:              "compact-100",
			Content:           []types.ContentBlock{{Type: types.ContentBlockText, Text: "[Context compacted]"}},
			IsCompactBoundary: true,
		},
		{
			Type:    types.MessageTypeUser,
			UUID:    "summary-100",
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "<context_summary>\n做了一半\n</context_summary>"}},
		},
	}
	for i, txt := range tailTexts {
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeAssistant,
			UUID:    "tail-" + string(rune('a'+i)),
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: txt}},
		})
	}
	return msgs
}

func countRosterItems(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "- [") {
			n++
		}
	}
	return n
}

// rosterTextOf 取出压缩产物里的清单文本; 没有则返回空串。
func rosterTextOf(msgs []types.Message) string {
	for _, m := range msgs {
		if m.UUID == rosterMessageUUID {
			for _, b := range m.Content {
				if b.Type == types.ContentBlockText {
					return b.Text
				}
			}
		}
	}
	return ""
}

// TestH13_RosterSurvivesCompaction 钉住 H1-3 的验收判据:
// "强制触发压缩后读回清单，条目数与压缩前相等"。
//
// 3 条未完成 + 1 条已完成。压缩前模型能看见的未完成条目数是 3,
// 压缩后必须还是 3 —— 改造前是 0 (清单只活在摘要之前的历史里, 被折掉了)。
func TestH13_RosterSurvivesCompaction(t *testing.T) {
	const pendingBefore = 3
	src := func() []TaskRosterItem {
		return []TaskRosterItem{
			{ID: "1", Content: "改 auth.go 的鉴权路径", Status: "in_progress"},
			{ID: "2", Content: "补单元测试", Status: "pending"},
			{ID: "3", Content: "跑 go build", Status: "pending"},
			{ID: "4", Content: "写文档", Status: "completed"},
		}
	}

	c := compact.NewCompactor(nil, 1000)
	got := reinjectTaskRoster(c, summaryMessages("tail a", "tail b"), src)

	text := rosterTextOf(got)
	if text == "" {
		t.Fatal("压缩产物里找不到清单消息 —— 压缩后'还剩什么'不可信")
	}
	if n := countRosterItems(text); n != pendingBefore {
		t.Fatalf("清单条目数 = %d, 压缩前是 %d", n, pendingBefore)
	}
	// 已完成的条目不该出现在"还剩什么"里。
	if strings.Contains(text, "写文档") {
		t.Fatal("已完成条目被当成未完成项写进了清单")
	}
}

// TestH13_AllDoneDropsRoster 对照臂: 全部完成时**不插**清单消息。
// 少了这一臂, "条目数相等"会因为无条件插一条固定文案而假绿。
func TestH13_AllDoneDropsRoster(t *testing.T) {
	src := func() []TaskRosterItem {
		return []TaskRosterItem{
			{ID: "1", Content: "唯一一项", Status: "completed"},
		}
	}
	c := compact.NewCompactor(nil, 1000)
	got := reinjectTaskRoster(c, summaryMessages("t"), src)
	if rosterTextOf(got) != "" {
		t.Fatal("全部完成时仍插入了清单消息 —— 那会让模型误以为还有事没做完")
	}
}

// TestH13_NilSourceIsByteIdentical 没装配清单来源时, 产物必须与改造前逐字节相同。
func TestH13_NilSourceIsByteIdentical(t *testing.T) {
	c := compact.NewCompactor(nil, 1000)
	in := summaryMessages("t1", "t2")
	got := reinjectTaskRoster(c, in, nil)
	if len(got) != len(in) {
		t.Fatalf("nil 来源却改动了消息条数: %d → %d", len(in), len(got))
	}
	for i := range in {
		if in[i].UUID != got[i].UUID {
			t.Fatalf("nil 来源却改动了消息 %d 的 UUID", i)
		}
	}
}

// TestH13_IdempotentAcrossRepeatedCompaction 反复压缩不能让清单叠成 N 条。
// 长程任务会压缩很多次, 这条不成立的话消息会单调膨胀。
func TestH13_IdempotentAcrossRepeatedCompaction(t *testing.T) {
	src := func() []TaskRosterItem {
		return []TaskRosterItem{{ID: "1", Content: "唯一一项", Status: "pending"}}
	}
	c := compact.NewCompactor(nil, 1000)
	msgs := summaryMessages("t")
	for i := 0; i < 5; i++ {
		msgs = reinjectTaskRoster(c, msgs, src)
	}
	n := 0
	for _, m := range msgs {
		if m.UUID == rosterMessageUUID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("清单消息出现了 %d 次, 期望恰好 1 次", n)
	}
	if got := countRosterItems(rosterTextOf(msgs)); got != 1 {
		t.Fatalf("清单条目数 = %d, 期望 1 (反复压缩叠了条目)", got)
	}
}

// TestH13_RosterUUIDStable 清单消息的 UUID 必须**跨轮恒定**。
//
// 这是缓存正确性判据: 这条消息每轮都会被重新插进请求, UUID 一旦带上轮次计数,
// 压缩之后最贵的那段 (整个摘要) 的前缀缓存每轮全废。
func TestH13_RosterUUIDStable(t *testing.T) {
	src := func() []TaskRosterItem {
		return []TaskRosterItem{{ID: "1", Content: "a", Status: "pending"}}
	}
	c := compact.NewCompactor(nil, 1000)

	first := reinjectTaskRoster(c, summaryMessages("tail x"), src)

	// 第二次: 清单**内容没变**, 但历史更长 (模拟下一轮)。
	src2 := func() []TaskRosterItem {
		return []TaskRosterItem{{ID: "1", Content: "a", Status: "pending"}}
	}
	second := reinjectTaskRoster(c, summaryMessages("tail x", "tail y", "tail z"), src2)

	a, b := rosterTextOf(first), rosterTextOf(second)
	if a == "" || b == "" {
		t.Fatal("两次都没插进清单")
	}
	if a != b {
		t.Fatalf("清单内容随历史长度变了 —— 前缀缓存会从插入点起全废\nfirst=%q\nsecond=%q", a, b)
	}
	for _, m := range second {
		if m.UUID == rosterMessageUUID && m.UUID != rosterMessageUUID {
			t.Fatal("清单消息 UUID 不稳定")
		}
	}
}

// TestH13_RosterSitsAfterSummary 位置判据: 清单要插在摘要之后、tail 之前。
// 插到 tail 之后会改动 tail 的字节, 而 tail 是缓存最热的一段。
func TestH13_RosterSitsAfterSummary(t *testing.T) {
	src := func() []TaskRosterItem {
		return []TaskRosterItem{{ID: "1", Content: "a", Status: "pending"}}
	}
	c := compact.NewCompactor(nil, 1000)
	got := reinjectTaskRoster(c, summaryMessages("tail a", "tail b"), src)

	idxRoster, idxSummary, idxFirstTail := -1, -1, -1
	for i, m := range got {
		switch {
		case m.UUID == rosterMessageUUID:
			idxRoster = i
		case strings.HasPrefix(m.UUID, "summary-"):
			idxSummary = i
		case m.UUID == "tail-a":
			idxFirstTail = i
		}
	}
	if idxRoster < 0 || idxSummary < 0 || idxFirstTail < 0 {
		t.Fatalf("索引没找齐: roster=%d summary=%d tail=%d", idxRoster, idxSummary, idxFirstTail)
	}
	if !(idxSummary < idxRoster && idxRoster < idxFirstTail) {
		t.Fatalf("清单位置不对: summary=%d roster=%d tail=%d (期望 summary < roster < tail)",
			idxSummary, idxRoster, idxFirstTail)
	}
}

// TestH13_CancelledNotCountedAsPending 已取消的条目不该出现在"还剩什么"里。
func TestH13_CancelledNotCountedAsPending(t *testing.T) {
	text, n := renderTaskRoster([]TaskRosterItem{
		{ID: "1", Content: "还在做", Status: "in_progress"},
		{ID: "2", Content: "不做了", Status: "cancelled"},
	})
	if n != 1 {
		t.Fatalf("未完成条目数 = %d, 期望 1 (cancelled 不算)", n)
	}
	if strings.Contains(text, "不做了") {
		t.Fatal("已取消的条目出现在清单里")
	}
}
