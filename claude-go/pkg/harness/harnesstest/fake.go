// Package harnesstest 是 harness 契约的**可复用测试替身**（Moqi R-H15）。
//
// # 为什么单独一个包
//
// 这些替身要能被 Moqi 业务层自己的测试 import，所以不能写成 `_test.go`；
// 但把它塞进 `pkg/harness` 又会让假的模型服务器变成 harness 的公共 API ——
// R-H16 要求 `pkg/harness` 是稳定契约，多一个 `FakeModel` 就多一份要长期
// 兼容的表面。折中是单独一个只依赖 `pkg/harness` 的包：harness 不反向依赖它，
// 业务层可以自由使用，而 harness 的 API 面不变。
//
// # 它替身的是什么
//
// FakeModel 伪装的是**上游模型 HTTP 服务**（Anthropic Messages 流式协议），
// 不是 `api.Client`。这条边界是刻意的：走真的 api.Client 意味着假件测的是
// "我构造的请求长什么样"，而走真的 HTTP + SSE 解析意味着它同时验证请求序列化
// 与流解析 —— 断流、非法 JSON 这类故障只有在真实解析路径上才有意义。
//
// # 纪律
//
//   - 不出网：只监听 127.0.0.1，CI 可离线。
//   - 确定性：脚本按调用序号回放，不依赖时间或并发；Everything 都由调用方
//     显式编排。需要"慢"的地方用 Delay，不靠 sleep 猜。
//   - 说清楚：脚本耗尽时返回一个固定文本并在 Unscripted 计数，而不是静默
//     继续或 panic —— 测试可以断言"没有被脚本外的调用"。
package harnesstest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// StepKind 是脚本一步的动作类别。
type StepKind int

const (
	// StepText 回一段纯文本并正常收口（end_turn）。
	StepText StepKind = iota
	// StepToolCall 回一次工具调用（stop_reason=tool_use）。
	StepToolCall
	// StepHTTPError 直接返回一个 HTTP 错误状态码，不写 SSE。
	StepHTTPError
	// StepBadJSON 返回合法 SSE 外壳但 data 不是合法 JSON。
	StepBadJSON
	// StepBadJSONThenComplete 先发若干非法帧，再发一段**完整正常**的回复。
	//
	// 用来单独钉住"协议错误"这一判据：整个流有合法收口帧（不会被断流判据抓到），
	// 但前面混进了无法解析的帧。生产里对应"代理插入错误页后上游又正常回复"。
	StepBadJSONThenComplete
	// StepTruncate 发若干帧后**不发 message_delta** 就关闭连接（断流）。
	StepTruncate
	// StepCleanEOF 发若干帧后**正常关闭连接**（干净 EOF），不带任何收口帧。
	//
	// 与 StepTruncate 的区别是传输层含义：Truncate 走 hijack 硬断开，会触发
	// scanner.Err()；CleanEOF 是一个语义正常结束的 HTTP 响应，scanner 无错误。
	// 生产里两者都会发生（网关正常关闭 vs 连接重置），判据必须都能抓到。
	StepCleanEOF
	// StepHang 不回任何字节，一直等到请求被取消或 Release 被调用（超时/挂起）。
	StepHang
)

// Step 是脚本的一步。
type Step struct {
	Kind StepKind

	// Text 是 StepText 的正文。
	Text string
	// ToolName / ToolID / ToolInput 用于 StepToolCall。ToolInput 为空取 "{}"。
	ToolName  string
	ToolID    string
	ToolInput string

	// Status 是 StepHTTPError 的状态码；为零取 500。
	Status int

	// Delay 在任何字节写出之前先等待。它是**故障注入的主要手段之一**：
	// 把上游拖慢，观察客户端是否在预期的时间点放弃，而不是靠真实网络抖动。
	Delay time.Duration

	// Frames 是 StepTruncate 要发出的帧数（1..5）。默认 2。
	Frames int

	// BadData 是 StepBadJSON 的原始 data 内容。默认 "{\"type\":"。
	BadData string

	// OmitStopReason 让 StepText 跳过 message_delta，只留 message_stop。
	// 用于覆盖"只发 message_stop 的网关"这一合法收尾形态。
	OmitStopReason bool
}

// Text 构造一段正常文本回复。
func Text(s string) Step { return Step{Kind: StepText, Text: s} }

// TextStopOnly 构造一段**只发 message_stop、不发 message_delta** 的回复。
//
// 这是协议合法的第二种收尾形态：部分网关与翻译层不产出 message_delta(stop_reason)，
// 只发 message_stop。它必须被当作完整流 —— 一条只认 message_delta 的判据会把它
// 误判成断流，那是对真实上游的回归。
func TextStopOnly(s string) Step {
	return Step{Kind: StepText, Text: s, OmitStopReason: true}
}

// ToolCall 构造一次工具调用回复。
func ToolCall(name, id, input string) Step {
	return Step{Kind: StepToolCall, ToolName: name, ToolID: id, ToolInput: input}
}

// HTTPError 构造一个 HTTP 错误回复。
func HTTPError(status int) Step { return Step{Kind: StepHTTPError, Status: status} }

// BadJSON 构造一个 data 不是合法 JSON 的回复。
func BadJSON() Step { return Step{Kind: StepBadJSON} }

// BadJSONThenComplete 构造"混入非法帧、但整体有合法收口"的回复。
func BadJSONThenComplete() Step { return Step{Kind: StepBadJSONThenComplete} }

// Truncate 构造断流：发 frames 帧后关闭，不发 message_delta。
func Truncate(frames int) Step { return Step{Kind: StepTruncate, Frames: frames} }

// CleanEOF 构造"干净断流"：正常结束 HTTP 响应，但没有任何收口帧。
// 这是最隐蔽的一种截断 —— 传输层没有任何错误。
func CleanEOF(frames int) Step { return Step{Kind: StepCleanEOF, Frames: frames} }

// Hang 构造挂起：一个字节都不回，直到请求取消或 Release。
func Hang() Step { return Step{Kind: StepHang} }

// WithDelay 返回延迟 d 之后才开始写字节的同一个脚本步。
func (s Step) WithDelay(d time.Duration) Step {
	s.Delay = d
	return s
}

// Captured 是一次被 FakeModel 收到的请求。
type Captured struct {
	Method  string
	Path    string
	Header  http.Header
	Body    string
	At      time.Time
	Status  int  // 实际返回的状态码
	Truncat bool // 该次回复是否被判为断流
}

// BodyJSON 把请求 body 解析成 map，便于断言语义而不是字符串匹配。
func (c Captured) BodyJSON() (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal([]byte(c.Body), &m)
	return m, err
}

// FakeModel 是一个可编程的上游模型服务。
//
// 零值不可用；用 New 构造。
type FakeModel struct {
	mu       sync.Mutex
	script   []Step
	captured []Captured
	cursor   int
	// unscripted 是脚本耗尽之后又收到的调用次数。非零通常意味着测试对
	// 调用次数的假设错了（例如以为一轮就收口，实际引擎又跑了一轮）。
	unscripted int
	// release 用于放行 StepHang。nil 时 StepHang 只在请求取消时返回。
	release     chan struct{}
	releaseOnce sync.Once
}

// New 构造一个按 script 回放的假模型。script 为空时每次调用都回 "ok"。
func New(script ...Step) *FakeModel {
	f := &FakeModel{script: script, release: make(chan struct{})}
	return f
}

// SetScript 在运行中替换脚本（只影响尚未回放的调用）。
func (f *FakeModel) SetScript(steps ...Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = steps
	f.cursor = 0
}

// Release 放行所有处于 StepHang 的请求。
func (f *FakeModel) Release() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// Unscripted 返回脚本耗尽后多收到的调用数。
func (f *FakeModel) Unscripted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unscripted
}

// Calls 返回至今收到的全部请求（拷贝）。
func (f *FakeModel) Calls() []Captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Captured, len(f.captured))
	copy(out, f.captured)
	return out
}

// CallCount 返回收到的请求数。
func (f *FakeModel) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.captured)
}

// LastBody 返回最后一次请求的原始 body；无调用时返回空串。
func (f *FakeModel) LastBody() string {
	calls := f.Calls()
	if len(calls) == 0 {
		return ""
	}
	return calls[len(calls)-1].Body
}

// Server 起一个 httptest 服务器并注册清理。测试里通常用这个而不是直接 Handler()。
func (f *FakeModel) Server(t interface {
	Cleanup(func())
	Helper()
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// Handler 返回可挂到任意 mux 上的处理器。
func (f *FakeModel) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		idx := f.cursor
		var step Step
		if idx < len(f.script) {
			step = f.script[idx]
			f.cursor++
		} else {
			f.unscripted++
			step = Step{Kind: StepText, Text: "ok(脚本外)"}
		}
		rec := Captured{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   string(body),
			At:     time.Now(),
		}
		// 先登记再回放，这样即使回放阻塞（Hang）也看得到这次调用发生过。
		f.captured = append(f.captured, rec)
		f.mu.Unlock()

		f.replay(w, r, step)
	})
}

func (f *FakeModel) replay(w http.ResponseWriter, r *http.Request, step Step) {
	if step.Delay > 0 {
		select {
		case <-time.After(step.Delay):
		case <-r.Context().Done():
			return
		}
	}

	switch step.Kind {
	case StepHang:
		select {
		case <-f.release:
		case <-r.Context().Done():
		}
		return

	case StepHTTPError:
		status := step.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"type":"error","error":{"type":"test_error","message":"注入的 %d"}}`, status)))
		return

	case StepBadJSON:
		data := step.BadData
		if data == "" {
			data = `{"type":`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+data+"\n\n")
		flush(w)
		return

	case StepBadJSONThenComplete:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 两帧非法 data，随后是一段完整合法的回复。
		for i := 0; i < 2; i++ {
			_, _ = io.WriteString(w, "data: {\"type\":\n\n")
			flush(w)
		}
		for _, fr := range textFrames("好的", false) {
			_, _ = io.WriteString(w, fr)
			flush(w)
		}
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	frames := f.framesFor(step)
	if step.Kind == StepTruncate || step.Kind == StepCleanEOF {
		n := step.Frames
		if n <= 0 {
			n = 2
		}
		if n > len(frames) {
			n = len(frames)
		}
		for i := 0; i < n; i++ {
			_, _ = io.WriteString(w, frames[i])
			flush(w)
		}
		if step.Kind == StepCleanEOF {
			// 直接返回：HTTP 响应正常结束，scanner 读到 EOF 且无错误。
			return
		}
		// 不发 message_delta：客户端会看到流在收口前断开。
		// 显式 CloseNotify 语义由 hijack 触发。
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}

	for _, fr := range frames {
		_, _ = io.WriteString(w, fr)
		flush(w)
	}
}

func flush(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// framesFor 渲染一步的 SSE 帧序列。
func (f *FakeModel) framesFor(step Step) []string {
	switch step.Kind {
	case StepToolCall:
		input := step.ToolInput
		if input == "" {
			input = "{}"
		}
		id := step.ToolID
		if id == "" {
			id = "toolu_test"
		}
		return toolUseFrames(step.ToolName, id, input)
	case StepTruncate, StepCleanEOF:
		// 断流用一段**正常文本回复的前缀**，这样"截断"不是靠畸形帧，
		// 而是真的在语义完成之前断开 —— 这才是生产里网络中断的样子。
		all := textFrames("断流前的半句", false)
		return all
	default:
		return textFrames(step.Text, step.OmitStopReason)
	}
}

func sse(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

// textFrames 是一段合法的纯文本回复，顺序与生产协议一致。
//
// omitStopReason 为真时跳过 message_delta —— 只剩 message_stop 收尾，
// 模拟不产出 stop_reason 的网关。
func textFrames(text string, omitStopReason bool) []string {
	frames := []string{
		sse(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_test", "type": "message", "role": "assistant",
				"model": "fake", "content": []any{},
				"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
			},
		}),
		sse(map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		}),
		sse(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}),
		sse(map[string]any{"type": "content_block_stop", "index": 0}),
	}
	if !omitStopReason {
		frames = append(frames, sse(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]int{"output_tokens": 5},
		}))
	}
	frames = append(frames, sse(map[string]any{"type": "message_stop"}))
	return frames
}

// toolUseFrames 是一次工具调用回复。
func toolUseFrames(name, id, input string) []string {
	if name == "" {
		name = "noop_tool"
	}
	return []string{
		sse(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_test", "type": "message", "role": "assistant",
				"model": "fake", "content": []any{},
				"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
			},
		}),
		sse(map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		}),
		sse(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": input},
		}),
		sse(map[string]any{"type": "content_block_stop", "index": 0}),
		sse(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use"},
			"usage": map[string]int{"output_tokens": 5},
		}),
		sse(map[string]any{"type": "message_stop"}),
	}
}

// Clock 是可控时钟（fake clock）。
//
// harness 侧凡是需要时间的契约（租约、退避、预算窗口）都应通过注入的函数取时间；
// 这个类型让测试能把"三分钟后"变成一次 Advance，而不是 sleep 三分钟。
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock 从 start 开始。零值时间会被换成 2026-01-01T00:00:00Z，避免
// Unix 零值在某些比较里被当成"未设置"。
func NewClock(start time.Time) *Clock {
	if start.IsZero() {
		start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return &Clock{t: start}
}

// Now 返回当前假时间。
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance 把假时间向前推 d。
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set 直接设定假时间。
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// BodyContains 是断言语义的小助手：在请求 body 里找一段子串。
func BodyContains(body, sub string) bool { return strings.Contains(body, sub) }
