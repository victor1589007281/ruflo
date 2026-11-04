// tool_evidence_test.go — H-F1 证据闭环与 H-F5 输入可见性的 harness 验收。
//
// 判据刻意落在**sink 收到的字节**与**事件的预览字段**上，而不是"runner 里
// 有没有一行调用 sink"：后者可以写在一个永远跑不到的分支里。这里用 StreamEvent
// 直接驱动 drain，因为该层的输入就是事件，不需要编排假的 SSE 帧。
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

type recordingToolResultSink struct {
	mu   sync.Mutex
	recs []ToolResultRecord
	err  error
}

func (s *recordingToolResultSink) AppendToolResult(_ context.Context, rec ToolResultRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return s.err
}

func (s *recordingToolResultSink) snapshot() []ToolResultRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ToolResultRecord(nil), s.recs...)
}

func drainToolEvents(t *testing.T, events []types.StreamEvent, sink ToolResultSink) (*TurnResult, []TurnEvent) {
	t.Helper()
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	res := &TurnResult{}
	var emitted []TurnEvent
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	drain(context.Background(), ch, func(ev TurnEvent) { emitted = append(emitted, ev) }, res, nil, clock,
		sink, "owner-1", "sess-1", "turn-1")
	return res, emitted
}

// TestDrain_ToolResultSinkGetsFullResult 是 H-F1 的核心断言：
// >500B 的完整结果必须原样到达 sink，同时 TurnResult 里只保留摘要/字节数。
func TestDrain_ToolResultSinkGetsFullResult(t *testing.T) {
	// 尾部放一个只存在于全文、不在 500B 预览窗口里的标记，用来检出"全文
	// 被塞进 TurnResult/trace"的泄漏；若用前缀做标记，预览本身就会命中。
	full := strings.Repeat("evidence-", 512) + "TAIL-ONLY-MARKER"
	if len(full) <= toolPreviewBytes {
		t.Fatalf("夹具无效: full=%dB 未超过预览窗口", len(full))
	}
	sink := &recordingToolResultSink{}
	res, emitted := drainToolEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "web_fetch", ToolUseID: "tu-1", ToolInput: `{"url":"https://example.com"}`},
		{Kind: types.StreamEventToolDone, ToolName: "web_fetch", ToolUseID: "tu-1",
			ToolResult: full[:200] + "...", ToolResultFull: full,
			ToolTrust: "untrusted", ToolSource: "https://example.com"},
	}, sink)

	recs := sink.snapshot()
	if len(recs) != 1 {
		t.Fatalf("sink 应收到 1 条完整结果, 实得 %d", len(recs))
	}
	rec := recs[0]
	if rec.FullResult != full {
		t.Fatalf("sink 收到的全文不完整: got %dB want %dB", len(rec.FullResult), len(full))
	}
	if rec.OwnerID != "owner-1" || rec.SessionID != "sess-1" || rec.TurnID != "turn-1" {
		t.Errorf("归属键丢失: %+v", rec)
	}
	if rec.ToolUseID != "tu-1" || rec.Name != "web_fetch" {
		t.Errorf("工具身份丢失: %+v", rec)
	}
	if rec.InputHash == "" || string(rec.Input) == "" {
		t.Errorf("输入/哈希丢失: %+v", rec)
	}
	if rec.Trust != "untrusted" || rec.SourceURI != "https://example.com" {
		t.Errorf("trust/source 未透传到 sink: %+v", rec)
	}
	if rec.ExecutedAt.IsZero() {
		t.Errorf("执行时刻未透传到 sink: %+v", rec)
	}

	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls 应有 1 条, 实得 %d", len(res.ToolCalls))
	}
	call := res.ToolCalls[0]
	if call.ResultBytes != len(full) {
		t.Errorf("ResultBytes 应为完整字节数 %d, 实得 %d", len(full), call.ResultBytes)
	}
	if !call.ResultTruncated {
		t.Error("ResultTruncated 应标记超过 500B 的完整结果")
	}
	if call.Trust != "untrusted" || call.SourceURI != "https://example.com" {
		t.Errorf("TurnResult 丢了 trust/source: %+v", call)
	}
	if len(call.ResultPreview) != toolPreviewBytes {
		t.Errorf("ResultPreview 应为 %dB, 实得 %dB", toolPreviewBytes, len(call.ResultPreview))
	}

	// 全文绝不能进 TurnResult（trace 膨胀/私密外泄）。用 JSON 形状直接钉住。
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "TAIL-ONLY-MARKER") {
		t.Error("完整工具结果泄漏进了 TurnResult/trace —— 它只应走 ToolResultSink")
	}

	// 事件预览同样只到 500B。
	var done *ToolEvent
	for i := range emitted {
		if emitted[i].Kind == EventToolDone {
			done = emitted[i].Tool
		}
	}
	if done == nil || len(done.ResultPreview) != toolPreviewBytes {
		t.Errorf("ToolEvent.ResultPreview 应为 %dB: %+v", toolPreviewBytes, done)
	}
}

// TestDrain_NilToolResultSinkKeepsLegacyBehavior nil sink 不得改变任何结果形状。
func TestDrain_NilToolResultSinkKeepsLegacyBehavior(t *testing.T) {
	full := strings.Repeat("x", 900)
	res, emitted := drainToolEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "read", ToolUseID: "tu-1"},
		{Kind: types.StreamEventToolDone, ToolName: "read", ToolUseID: "tu-1", ToolResult: full},
	}, nil)

	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ResultBytes != len(full) {
		t.Fatalf("nil sink 下摘要/字节数应照常记录: %+v", res.ToolCalls)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("nil sink 不应产生 Degraded: %v", res.Degraded)
	}
	var done bool
	for i := range emitted {
		if emitted[i].Kind == EventToolDone {
			done = true
			// 旧生产者没有 ToolResultFull 时退回摘要，预览行为必须与旧版一致。
			if emitted[i].Tool.ResultPreview != full[:toolPreviewBytes] {
				t.Errorf("回退预览与旧行为不一致: %q", emitted[i].Tool.ResultPreview)
			}
		}
	}
	if !done {
		t.Fatal("未发出 EventToolDone")
	}
}

// TestDrain_ToolEventPreviewsAreTruncated H-F5: 输入与结果预览各 500B。
func TestDrain_ToolEventPreviewsAreTruncated(t *testing.T) {
	input := strings.Repeat("i", toolPreviewBytes+300)
	result := strings.Repeat("r", toolPreviewBytes+700)
	_, emitted := drainToolEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "grep", ToolUseID: "tu-1", ToolInput: input},
		{Kind: types.StreamEventToolDone, ToolName: "grep", ToolUseID: "tu-1", ToolResultFull: result},
	}, nil)

	var start, done *ToolEvent
	for i := range emitted {
		switch emitted[i].Kind {
		case EventToolStart:
			start = emitted[i].Tool
		case EventToolDone:
			done = emitted[i].Tool
		}
	}
	if start == nil || start.InputPreview != input[:toolPreviewBytes] {
		t.Errorf("InputPreview 应为前 %dB: %+v", toolPreviewBytes, start)
	}
	if done == nil || done.ResultPreview != result[:toolPreviewBytes] {
		t.Errorf("ResultPreview 应为前 %dB: %+v", toolPreviewBytes, done)
	}
}

// TestDrain_ToolResultSinkFailureDegradesOnce sink 失败不得让 turn 失败，
// 且同一工具只记一次 Degraded（一次 turn 内可被调用几十次）。
func TestDrain_ToolResultSinkFailureDegradesOnce(t *testing.T) {
	sink := &recordingToolResultSink{err: errors.New("disk full")}
	res, _ := drainToolEvents(t, []types.StreamEvent{
		{Kind: types.StreamEventToolStart, ToolName: "web_fetch", ToolUseID: "tu-1"},
		{Kind: types.StreamEventToolDone, ToolName: "web_fetch", ToolUseID: "tu-1", ToolResultFull: "a"},
		{Kind: types.StreamEventToolStart, ToolName: "web_fetch", ToolUseID: "tu-2"},
		{Kind: types.StreamEventToolDone, ToolName: "web_fetch", ToolUseID: "tu-2", ToolResultFull: "b"},
	}, sink)

	if len(res.ToolCalls) != 2 {
		t.Fatalf("sink 失败不应影响 ToolCalls 记录, 实得 %d", len(res.ToolCalls))
	}
	want := "tool_result_sink_failed:web_fetch"
	n := 0
	for _, d := range res.Degraded {
		if d == want {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("同一工具的 sink 失败应只记一次 Degraded, 实得 %d 次: %v", n, res.Degraded)
	}
	if len(sink.snapshot()) != 2 {
		t.Fatalf("sink 应被调用 2 次 (失败也不停止重试), 实得 %d", len(sink.snapshot()))
	}
}
