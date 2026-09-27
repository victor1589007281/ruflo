// events.go — 一次运行的**事件流**: 缓冲 + SSE 出口 (design 03 §G1)。
//
// # 为什么需要它
//
// postMessage 是 202+轮询: 调用方只在 run 结束时拿到**整段**回复。结对编程类的
// 前端 (面试考场页) 需要实时感 —— "AI 正在编辑 x.go / 正在跑 go test" 必须在
// 发生的当下流出去, 而不是两分钟之后一次性倒出来。引擎侧的事件模型
// (types.StreamEvent) 一直是齐的, 缺的只是把进程内 channel 桥到 HTTP 的这一米线。
//
// # 形态
//
//	POST /api/sessions/{id}/messages            → 202 {runId}   (不变)
//	GET  /api/sessions/{id}/runs/{runId}/events → SSE 事件流
//
// 事件**先进缓冲再转发** (不是直连引擎 channel): SSE 客户端会断线重连
// (EventSource 自动重连是浏览器内建行为), 重连时带 ?after=<seq> 从断点续放,
// 一条不丢。直连 channel 的话, 断连窗口里的事件就永远丢了。
//
// 缓冲随 run 档案一起淘汰 (maxRetainedRuns): 这是一个常驻进程, 事件表只增不减
// 迟早把内存吃光 —— 与 runs 表同一条纪律。
package sessionapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event 一次运行中的一条流式事件。
//
// 与 types.StreamEvent 是一对多的映射: 那里是引擎的内部表示 (Kind 是枚举、Error
// 是 error 接口), 这里是**线上协议** —— 字段全部 JSON 原生类型, 加字段向后兼容,
// 改枚举值不会悄悄破坏下游解析器。
type Event struct {
	Seq    int       `json:"seq"`    // run 内单调递增, 断线重连的锚点
	Kind   string    `json:"kind"`   // delta | thinking | tool_start | tool_done | run_done
	Text   string    `json:"text,omitempty"`   // delta/thinking 的文本片段
	Tool   string    `json:"tool,omitempty"`   // tool_start/tool_done 的工具名
	Input  string    `json:"input,omitempty"`  // tool_start 的输入摘要
	Result string    `json:"result,omitempty"` // tool_done 的结果摘要
	Status string    `json:"status,omitempty"` // run_done: completed | failed | cancelled
	Error  string    `json:"error,omitempty"`  // run_done(failed) 的错误
	At     time.Time `json:"at"`
}

// 事件类别常量。用字符串而不是 iota: 这是跨进程协议, 字符串在 SSE 线上与日志里
// 都可读, 枚举值重排不会变成线上事故。
const (
	EventDelta     = "delta"
	EventThinking  = "thinking"
	EventToolStart = "tool_start"
	EventToolDone  = "tool_done"
	EventError     = "error"    // 运行中的非致命错误 (引擎 StreamEventError)
	EventRunDone   = "run_done" // 终态哨兵: 订阅方见到它即可关连接
)

// maxBufferedDeltaEvents 一个 run 的 delta/thinking 事件缓冲上限。
//
// 连续同类 delta 在入缓冲时会**合并** (见 appendEvent), 所以实际条数远小于
// token 数; 这个上限防的是"delta/工具事件交替刷屏"的病态序列。超过上限的新
// delta 并入最后一条 (内容不丢), 只是不再增加条数。
const maxBufferedDeltaEvents = 4000

// eventLog 一个 run 的事件缓冲。seq 从 1 开始。
//
// 广播用 "关旧开新" 的 chan 换手: 每次 append 把 waiters 手里的 chan 关掉
// (唤醒所有等待者), 换上新的。比 sync.Cond 好: 等待方可以用 select 同时听
// ctx.Done (SSE 客户端断开), Cond 做不到。
type eventLog struct {
	mu     sync.Mutex
	events []Event
	notify chan struct{} // 每次 append 后关闭并换新的广播 chan
}

func newEventLog() *eventLog {
	return &eventLog{notify: make(chan struct{})}
}

// append 追加一条事件并广播。连续的同类 delta/thinking 合并进上一条:
// token 级流式会在几秒内产生几千条事件, 全留既浪费内存又让重连回放变慢。
func (l *eventLog) append(ev Event) {
	l.mu.Lock()
	if n := len(l.events); n > 0 {
		last := &l.events[n-1]
		if (ev.Kind == EventDelta || ev.Kind == EventThinking) && ev.Kind == last.Kind {
			last.Text += ev.Text
			last.At = ev.At
			l.mu.Unlock()
			l.broadcast()
			return
		}
		if (ev.Kind == EventDelta || ev.Kind == EventThinking) && len(l.events) >= maxBufferedDeltaEvents {
			last.Text += ev.Text // 条数封顶, 内容并入上一条不丢
			l.mu.Unlock()
			l.broadcast()
			return
		}
	}
	ev.Seq = len(l.events) + 1
	l.events = append(l.events, ev)
	l.mu.Unlock()
	l.broadcast()
}

func (l *eventLog) broadcast() {
	l.mu.Lock()
	close(l.notify)
	l.notify = make(chan struct{})
	l.mu.Unlock()
}

// snapshot 返回 after 序号之后的全部事件与当前等待句柄。
func (l *eventLog) snapshot(after int) ([]Event, chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if after < 0 {
		after = 0
	}
	if after > len(l.events) {
		after = len(l.events)
	}
	out := make([]Event, len(l.events)-after)
	copy(out, l.events[after:])
	return out, l.notify
}

// ── SSE 端点 ─────────────────────────────────────────────────────────────────

// handleRunEvents GET /api/sessions/{id}/runs/{runId}/events[?after=N]
//
// SSE 协议: 每帧 `id: <seq>\ndata: <json>\n\n`, 重连方带 ?after=<seq> 续放。
// run 到达终态 (run_done 事件) 后再把缓冲发完就关闭连接。
//
// 不用第三方 SSE 库: 协议本身只有两行文本, 引依赖是把"攒批/flush"的正确性
// 交给一个没人读过的实现。Flusher 不可用时回 500 —— 静默降级成不流式是
// 这类端点最难查的坑 (连接正常、状态 200、永远没有数据)。
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request, sessionID, runID string) {
	run := s.findRun(sessionID, runID)
	if run == nil {
		writeErr(w, http.StatusNotFound, "没有这次运行: "+runID)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "本响应不支持流式 (no http.Flusher)")
		return
	}
	after := 0
	if v := strings.TrimSpace(r.URL.Query().Get("after")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			after = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(ev Event) bool {
		payload, err := json.Marshal(ev)
		if err != nil {
			return true // 序列化失败单条跳过, 不断流
		}
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, payload); err != nil {
			return false // 客户端断开
		}
		flusher.Flush()
		return true
	}

	// keepalive: 长任务的工具执行期间可能几十秒没有事件, 反代/浏览器的空闲
	// 超时会把连接悄悄掐掉。注释帧 (": ka") 对事件流无语义, 只保活。
	ka := time.NewTicker(15 * time.Second)
	defer ka.Stop()

	log := run.events
	for {
		batch, wait := log.snapshot(after)
		terminal := false
		for _, ev := range batch {
			if !send(ev) {
				return
			}
			after = ev.Seq
			if ev.Kind == EventRunDone {
				terminal = true
			}
		}
		if terminal {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ka.C:
			fmt.Fprint(w, ": ka\n\n")
			flusher.Flush()
		case <-wait:
		}
	}
}
