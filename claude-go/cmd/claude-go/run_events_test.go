package main

// run_events_test.go: --event-format ndjson 运行事件协议的自测
// (契约 /tmp/deck-dispatch-contract.md §NDJSON):
//   - seq 单次 run 内从 1 严格递增, 行格式字段齐全 (run_id/seq/ts/type/data);
//   - result 只发一次, 第二次 panic;
//   - heartbeat goroutine 可停, 停后绝不再有事件, result 是最后一个事件;
//   - progress 挂钩 nil 安全, text 截断 ≤200 字符且带工具名。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// parsedRunEvent 是测试侧的 NDJSON 行解析形态。
type parsedRunEvent struct {
	RunID string         `json:"run_id"`
	Seq   int            `json:"seq"`
	TS    string         `json:"ts"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data"`
}

// parseRunEvents 把缓冲里的输出按行解析成事件序列; 任一行不是合法 JSON 即 fatal
// (契约: 一行一个 JSON)。
func parseRunEvents(t *testing.T, buf *bytes.Buffer) []parsedRunEvent {
	t.Helper()
	var out []parsedRunEvent
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev parsedRunEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("行不是合法 JSON: %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描事件行: %v", err)
	}
	return out
}

func TestRunEventsEmitterSeqAndFormat(t *testing.T) {
	buf := &bytes.Buffer{}
	ev := newRunEventEmitter(buf, "run-test-x", "lbl-1")
	ev.emit("session", map[string]any{"session_id": "s1"})
	ev.emit("progress", map[string]any{"phase": "tool", "text": "Bash ls -la"})
	ev.stopHeartbeat()
	ev.emitResult(runResultEventData("final-text", "s1", ""))

	evs := parseRunEvents(t, buf)
	if len(evs) != 4 {
		t.Fatalf("事件数 = %d, 想要 4 (started/session/progress/result)", len(evs))
	}
	wantTypes := []string{"started", "session", "progress", "result"}
	for i, e := range evs {
		if e.RunID != "run-test-x" {
			t.Fatalf("事件 %d run_id = %q, 想要 run-test-x", i, e.RunID)
		}
		if e.Seq != i+1 {
			t.Fatalf("事件 %d seq = %d, 想要严格递增 %d", i, e.Seq, i+1)
		}
		if e.Type != wantTypes[i] {
			t.Fatalf("事件 %d type = %q, 想要 %q", i, e.Type, wantTypes[i])
		}
		if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
			t.Fatalf("事件 %d ts %q 不是 RFC3339: %v", i, e.TS, err)
		}
		if e.Data == nil {
			t.Fatalf("事件 %d data 缺失", i)
		}
	}
	if got := evs[0].Data["label"]; got != "lbl-1" {
		t.Fatalf("started label = %v, 想要 lbl-1", got)
	}
	if got := evs[1].Data["session_id"]; got != "s1" {
		t.Fatalf("session session_id = %v, 想要 s1", got)
	}
	if got := evs[3].Data["final"]; got != "final-text" {
		t.Fatalf("result final = %v, 想要 final-text", got)
	}
	if got := evs[3].Data["is_error"]; got != false {
		t.Fatalf("result is_error = %v, 想要 false", got)
	}
}

func TestRunEventsResultOnce(t *testing.T) {
	buf := &bytes.Buffer{}
	ev := newRunEventEmitter(buf, "run-once", "")
	ev.stopHeartbeat()
	ev.emitResult(runResultEventData("a", "", ""))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("第二次 emitResult 没有 panic (契约: 宁可崩, 不可双发)")
		}
		if !strings.Contains(fmt.Sprint(r), "twice") {
			t.Fatalf("panic 文案 %q 不含 twice", fmt.Sprint(r))
		}
	}()
	ev.emitResult(runResultEventData("b", "", ""))
}

func TestRunEventsResultDataErrorFields(t *testing.T) {
	// is_error/error_kind/session_id 与信封同源: errKind 非空 ⇒ is_error 真,
	// error_kind 带上值; errKind 空 ⇒ error_kind 键整段缺席 (不冒充空串)。
	d := runResultEventData("f", "sid", "auth_error")
	if d["is_error"] != true || d["error_kind"] != "auth_error" || d["session_id"] != "sid" {
		t.Fatalf("带错结果 data 不对: %v", d)
	}
	d = runResultEventData("f", "", "")
	if d["is_error"] != false {
		t.Fatalf("is_error = %v, 想要 false", d["is_error"])
	}
	if _, ok := d["error_kind"]; ok {
		t.Fatalf("errKind 空时 error_kind 应缺席: %v", d)
	}
	if _, ok := d["session_id"]; ok {
		t.Fatalf("sessionID 空时 session_id 应缺席: %v", d)
	}
}

func TestRunEventsHeartbeatStartsAfterSessionAndStops(t *testing.T) {
	t.Setenv("CLAUDE_GO_EVENT_HEARTBEAT_SEC", "1") // 1s 间隔, 压缩测试时长
	buf := &bytes.Buffer{}
	ev := newRunEventEmitter(buf, "run-hb", "")
	ev.emit("session", map[string]any{"session_id": "s1"})
	ev.startHeartbeat()

	// 等 2 个心跳 (2s+) 再停;  CI 抖动下等到至少 1 个即可。
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(buf.Bytes(), []byte(`"heartbeat"`)) &&
			bytes.Count(buf.Bytes(), []byte(`"heartbeat"`)) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	ev.stopHeartbeat()

	evs := parseRunEvents(t, buf)
	hb := 0
	for _, e := range evs {
		if e.Type == "heartbeat" {
			hb++
		}
	}
	if hb < 1 {
		t.Fatalf("一个心跳都没收到 (等了 4s, 间隔 1s)")
	}
	// 顺序契约: started(1) < session(2) < heartbeat(3+)。
	if evs[0].Type != "started" || evs[0].Seq != 1 {
		t.Fatalf("首个事件 = %s/%d, 想要 started/1", evs[0].Type, evs[0].Seq)
	}
	if evs[1].Type != "session" || evs[1].Seq != 2 {
		t.Fatalf("第二个事件 = %s/%d, 想要 session/2", evs[1].Type, evs[1].Seq)
	}
	for i := 2; i < len(evs); i++ {
		if evs[i].Seq != i+1 {
			t.Fatalf("事件 %d seq = %d, 想要 %d", i, evs[i].Seq, i+1)
		}
	}
	// elapsed_sec 是整数秒且 ≥1。
	for _, e := range evs[2:] {
		f, ok := e.Data["elapsed_sec"].(float64)
		if !ok {
			t.Fatalf("heartbeat elapsed_sec 不是数字: %v", e.Data["elapsed_sec"])
		}
		if f != float64(int64(f)) || f < 1 {
			t.Fatalf("heartbeat elapsed_sec = %v, 想要 ≥1 的整数", f)
		}
	}

	// 停后绝不再有事件 (result 必须是最后一个事件的前提)。
	n := buf.Len()
	time.Sleep(1200 * time.Millisecond)
	if buf.Len() != n {
		t.Fatalf("stopHeartbeat 后仍有 %d 字节事件写入", buf.Len()-n)
	}

	// 停后补发 result: 必须是最后一个事件, seq 顺延。
	ev.emitResult(runResultEventData("done", "s1", ""))
	evs = parseRunEvents(t, buf)
	last := evs[len(evs)-1]
	if last.Type != "result" || last.Seq != len(evs) {
		t.Fatalf("末事件 = %s/%d, 想要 result/%d", last.Type, last.Seq, len(evs))
	}
}

func TestRunEventsHeartbeatEnvInvalidFallsBack(t *testing.T) {
	t.Setenv("CLAUDE_GO_EVENT_HEARTBEAT_SEC", "garbage")
	if got := runEventHeartbeatInterval(); got != runEventDefaultHeartbeatSec*time.Second {
		t.Fatalf("非法 env 间隔 = %v, 想要默认 %v", got, runEventDefaultHeartbeatSec*time.Second)
	}
	t.Setenv("CLAUDE_GO_EVENT_HEARTBEAT_SEC", "-3")
	if got := runEventHeartbeatInterval(); got != runEventDefaultHeartbeatSec*time.Second {
		t.Fatalf("负 env 间隔 = %v, 想要默认 %v", got, runEventDefaultHeartbeatSec*time.Second)
	}
}

func TestRunEventsProgressTextTruncate(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := runProgressText("Bash", long)
	if !strings.HasPrefix(got, "Bash ") {
		t.Fatalf("text 应以工具名开头: %q", got)
	}
	if !strings.Contains(got, "...") {
		t.Fatalf("截断摘要应有 ... 后缀: %q", got)
	}
	if len(got) > len("Bash ")+200+3 {
		t.Fatalf("text 长度 %d 超过 工具名+200+3", len(got))
	}
	// 空摘要只留工具名; 空工具名只留摘要。
	if got := runProgressText("Bash", "  "); got != "Bash" {
		t.Fatalf("空摘要应只留工具名: %q", got)
	}
	if got := runProgressText("", "ok"); got != "ok" {
		t.Fatalf("空工具名应只留摘要: %q", got)
	}
}

func TestRunEventsConsumeStreamHook(t *testing.T) {
	ch := make(chan types.StreamEvent, 4)
	ch <- types.StreamEvent{Kind: types.StreamEventToolStart, ToolName: "Bash", ToolInput: strings.Repeat("a", 400)}
	ch <- types.StreamEvent{Kind: types.StreamEventToolDone, ToolName: "Bash", ToolResult: "ok"}
	ch <- types.StreamEvent{Kind: types.StreamEventDelta, DeltaText: "hello "}
	ch <- types.StreamEvent{Kind: types.StreamEventDelta, DeltaText: "world"}
	close(ch)

	var kinds []string
	var datas []map[string]any
	hook := func(kind string, data map[string]any) {
		kinds = append(kinds, kind)
		datas = append(datas, data)
	}
	final, errKind := consumeStreamEvents(ch, true, hook)
	if final != "hello world" {
		t.Fatalf("final = %q, 想要 hello world", final)
	}
	if errKind != "" {
		t.Fatalf("errKind = %q, 想要空", errKind)
	}
	if len(kinds) != 2 || kinds[0] != "progress" || kinds[1] != "progress" {
		t.Fatalf("hook kind 序列 = %v, 想要 [progress progress]", kinds)
	}
	for i, d := range datas {
		if d["phase"] != "tool" {
			t.Fatalf("progress %d phase = %v, 想要 tool", i, d["phase"])
		}
		text, _ := d["text"].(string)
		if !strings.HasPrefix(text, "Bash ") {
			t.Fatalf("progress %d text 应以工具名开头: %q", i, text)
		}
		if len(text) > len("Bash ")+200+3 {
			t.Fatalf("progress %d text 超长 (截断失守): %d", i, len(text))
		}
	}
}

func TestRunEventsConsumeStreamNilHookSafe(t *testing.T) {
	// nil hook 必须不 panic 且行为不变 (非 ndjson 路径逐字节兼容的判据)。
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Kind: types.StreamEventToolStart, ToolName: "Read"}
	ch <- types.StreamEvent{Kind: types.StreamEventDelta, DeltaText: "x"}
	close(ch)
	final, errKind := consumeStreamEvents(ch, true, nil)
	if final != "x" || errKind != "" {
		t.Fatalf("final/errKind = %q/%q, 想要 x/空", final, errKind)
	}
}
