package sessionapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
)

// streamFakeHost 实现 Host + StreamHost + InjectHost + TraceHost 的全能假件。
//
// 与 api_test.go 的 fakeHost 同一条纪律: Reserve/Run 的忙闲状态**真实绑定**,
// 不是摆拍的标志位 —— 否则"忙时 409"就成了同义反复。
type streamFakeHost struct {
	mu       sync.Mutex
	specs    map[string]Spec
	reserved map[string]bool
	runSeq   int

	// 流式脚本: RunStream 进入后逐条 emit, 然后按 blockOn 决定如何结束。
	events []Event
	// blockOn="ctx": emit 完之后阻塞直到 ctx 取消 (测 cancel); "": 立即返回。
	blockOn   string
	injectOK  bool
	traceSpans []tracestore.Span
}

func newStreamFakeHost() *streamFakeHost {
	return &streamFakeHost{specs: map[string]Spec{}, reserved: map[string]bool{}}
}

func (h *streamFakeHost) Ensure(sp Spec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs[sp.ID] = sp
	return nil
}
func (h *streamFakeHost) List() []Spec {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Spec, 0, len(h.specs))
	for _, sp := range h.specs {
		out = append(out, sp)
	}
	return out
}
func (h *streamFakeHost) Get(id string) (Spec, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sp, ok := h.specs[id]
	if !ok {
		return Spec{}, ErrNotFound
	}
	return sp, nil
}
func (h *streamFakeHost) Delete(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.specs, id)
	return nil
}
func (h *streamFakeHost) Clear(id string) error { return nil }
func (h *streamFakeHost) Busy(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reserved[id]
}
func (h *streamFakeHost) Reserve(id string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reserved[id] {
		return "", ErrBusy
	}
	h.reserved[id] = true
	h.runSeq++
	return fmt.Sprintf("run-%s-%d", id, h.runSeq), nil
}
func (h *streamFakeHost) release(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.reserved, id)
}
func (h *streamFakeHost) Run(ctx context.Context, id, text string) (string, error) {
	defer h.release(id)
	return "plain reply", nil
}
func (h *streamFakeHost) RunStream(ctx context.Context, id, text string, emit func(Event)) (string, error) {
	defer h.release(id)
	for _, ev := range h.events {
		emit(ev)
	}
	if h.blockOn == "ctx" {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "streamed reply", nil
}
func (h *streamFakeHost) Tokens(runID string) (*Usage, error) { return nil, nil }
func (h *streamFakeHost) Inject(id, text string) bool       { return h.injectOK }
func (h *streamFakeHost) Trace(runID string) ([]tracestore.Span, error) {
	// 只认 TraceID 对得上的 —— 不过滤的话, "未知 runId 返回空"这条语义
	// 在测试里就成了恒真摆设。
	var out []tracestore.Span
	for _, sp := range h.traceSpans {
		if sp.TraceID == runID {
			out = append(out, sp)
		}
	}
	return out, nil
}

// startServer 挂一个 httptest server。返回的 client 直连, 无鉴权层。
func startServer(t *testing.T, h Host) *httptest.Server {
	t.Helper()
	srv := New(func() Host { return h })
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", srv.handleSessions)
	mux.HandleFunc("/api/sessions/", srv.handleSessionItem)
	mux.HandleFunc("/api/traces/", srv.handleTrace)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestStreamEventsEndToEnd 全链路: 建会话 → 发消息 (RunStream emit 3 条事件)
// → SSE 端点回放到 run_done 关闭; 再带 ?after= 重连, 只回增量。
func TestStreamEventsEndToEnd(t *testing.T) {
	h := newStreamFakeHost()
	h.events = []Event{
		{Kind: EventDelta, Text: "你好"},
		{Kind: EventToolStart, Tool: "Bash", Input: "go test ./..."},
		{Kind: EventToolDone, Tool: "Bash", Result: "ok"},
	}
	ts := startServer(t, h)
	code, _ := postJSON(t, ts.URL+"/api/sessions", `{"id":"s1"}`)
	if code != http.StatusCreated {
		t.Fatalf("建会话 = %d", code)
	}
	code, created := postJSON(t, ts.URL+"/api/sessions/s1/messages", `{"text":"hi"}`)
	if code != http.StatusAccepted {
		t.Fatalf("发消息 = %d", code)
	}
	runID := created["runId"].(string)

	// 等 run 到终态
	waitRunDone(t, ts.URL+"/api/sessions/s1/runs/"+runID)

	// SSE 回放: 应含 3 条脚本事件 + 1 条 run_done
	events := readAllSSE(t, ts.URL+"/api/sessions/s1/runs/"+runID+"/events")
	if len(events) != 4 {
		t.Fatalf("事件数 = %d, 期望 4 (3 脚本 + run_done)", len(events))
	}
	if events[0].Kind != EventDelta || events[0].Text != "你好" {
		t.Fatalf("事件0 = %+v", events[0])
	}
	if events[1].Kind != EventToolStart || events[1].Tool != "Bash" {
		t.Fatalf("事件1 = %+v", events[1])
	}
	if events[3].Kind != EventRunDone || events[3].Status != string(RunCompleted) {
		t.Fatalf("终态事件 = %+v", events[3])
	}
	// seq 单调递增从 1 开始
	for i, ev := range events {
		if ev.Seq != i+1 {
			t.Fatalf("事件%d seq = %d", i, ev.Seq)
		}
	}

	// 断点续放: after=2 只回后两条
	events = readAllSSE(t, ts.URL+"/api/sessions/s1/runs/"+runID+"/events?after=2")
	if len(events) != 2 || events[0].Kind != EventToolDone {
		t.Fatalf("after=2 回放 = %d 条, 首条 %+v", len(events), events[0])
	}
}

func waitRunDone(t *testing.T, url string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			defer resp.Body.Close()
			var v map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&v)
			if s := v["status"]; s == string(RunCompleted) || s == string(RunFailed) || s == string(RunCancelled) {
				return v
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run 未到终态")
	return nil
}

// readAllSSE 读 SSE 流直到 run_done (或连接关闭)。
func readAllSSE(t *testing.T, url string) []Event {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var out []Event
	sc := bufio.NewScanner(resp.Body)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			data.WriteString(line[6:])
		} else if line == "" && data.Len() > 0 {
			var ev Event
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				t.Fatalf("事件解析失败: %v (%s)", err, data.String())
			}
			out = append(out, ev)
			data.Reset()
			if ev.Kind == EventRunDone {
				return out
			}
		}
	}
	return out
}

// TestCancelRun 取消一个阻塞中的 run → 终态 cancelled。
func TestCancelRun(t *testing.T) {
	h := newStreamFakeHost()
	h.blockOn = "ctx"
	ts := startServer(t, h)
	postJSON(t, ts.URL+"/api/sessions", `{"id":"s1"}`)
	_, created := postJSON(t, ts.URL+"/api/sessions/s1/messages", `{"text":"hi"}`)
	runID := created["runId"].(string)

	code, out := postJSON(t, ts.URL+"/api/sessions/s1/runs/"+runID+"/cancel", `{}`)
	if code != http.StatusOK || out["cancelled"] != true {
		t.Fatalf("cancel = %d %+v", code, out)
	}
	v := waitRunDone(t, ts.URL+"/api/sessions/s1/runs/"+runID)
	if v["status"] != string(RunCancelled) {
		t.Fatalf("status = %v, 期望 cancelled", v["status"])
	}

	// 幂等: 再取消一次 → 200 + cancelled:false
	code, out = postJSON(t, ts.URL+"/api/sessions/s1/runs/"+runID+"/cancel", `{}`)
	if code != http.StatusOK || out["cancelled"] != false {
		t.Fatalf("二次 cancel = %d %+v", code, out)
	}
	// 取消后占位必须已释放: 再发消息应 202 而不是 409
	code, _ = postJSON(t, ts.URL+"/api/sessions/s1/messages", `{"text":"again"}`)
	if code != http.StatusAccepted {
		t.Fatalf("取消后再发 = %d, 期望 202", code)
	}
}

// TestInject 会话在跑时可插话; 空闲时 409; host 不支持时 501。
func TestInject(t *testing.T) {
	h := newStreamFakeHost()
	h.injectOK = true
	ts := startServer(t, h)
	postJSON(t, ts.URL+"/api/sessions", `{"id":"s1"}`)
	code, out := postJSON(t, ts.URL+"/api/sessions/s1/inject", `{"text":"等一下"}`)
	if code != http.StatusAccepted || out["injected"] != true {
		t.Fatalf("inject = %d %+v", code, out)
	}

	h.injectOK = false // 引擎空闲, 没人接
	code, _ = postJSON(t, ts.URL+"/api/sessions/s1/inject", `{"text":"在吗"}`)
	if code != http.StatusConflict {
		t.Fatalf("空闲 inject = %d, 期望 409", code)
	}
}

// TestTraceExport 轨迹导出: span 原样返回, blob 引用已由 host 侧解析。
func TestTraceExport(t *testing.T) {
	h := newStreamFakeHost()
	h.traceSpans = []tracestore.Span{
		{TraceID: "run-s1-1", SpanID: "sp1", Kind: "llm_call", Name: "turn-1",
			InputRef:  tracestore.Ref{Inline: "prompt 全文", Size: 9},
			OutputRef: tracestore.Ref{Inline: "response", Size: 8},
			Attrs:     map[string]any{"input_tokens": 100}},
	}
	ts := startServer(t, h)
	postJSON(t, ts.URL+"/api/sessions", `{"id":"s1"}`)

	resp, err := http.Get(ts.URL + "/api/traces/run-s1-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace = %d", resp.StatusCode)
	}
	var out struct {
		RunID string             `json:"runId"`
		Spans []tracestore.Span `json:"spans"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Spans) != 1 || out.Spans[0].InputRef.Inline != "prompt 全文" {
		t.Fatalf("spans = %+v", out.Spans)
	}

	// 无轨迹的 runId → 空列表而不是 500
	resp2, _ := http.Get(ts.URL + "/api/traces/run-nobody")
	body := map[string]any{}
	_ = json.NewDecoder(resp2.Body).Decode(&body)
	resp2.Body.Close()
	if spans, ok := body["spans"].([]any); !ok || len(spans) != 0 {
		t.Fatalf("未知 runId 的 spans = %v", body["spans"])
	}
}

// TestEventLogDeltaMerge 连续同类 delta 合并: 1000 个 token 不该占 1000 条事件。
func TestEventLogDeltaMerge(t *testing.T) {
	l := newEventLog()
	for i := 0; i < 1000; i++ {
		l.append(Event{Kind: EventDelta, Text: "x", At: time.Now()})
	}
	l.append(Event{Kind: EventThinking, Text: "想", At: time.Now()})
	l.append(Event{Kind: EventThinking, Text: "想", At: time.Now()})
	l.append(Event{Kind: EventToolStart, Tool: "Bash", At: time.Now()})
	got, _ := l.snapshot(0)
	if len(got) != 3 {
		t.Fatalf("合并后 = %d 条, 期望 3", len(got))
	}
	if len(got[0].Text) != 1000 {
		t.Fatalf("delta 合并后文本长度 = %d", len(got[0].Text))
	}
	if got[2].Seq != 3 {
		t.Fatalf("第3条 seq = %d", got[2].Seq)
	}
}

// TestRunWithoutStreamHost 无 StreamHost 的 host: 事件流只剩 run_done 哨兵,
// 能力与 host 对齐, 不产生假事件。
func TestRunWithoutStreamHost(t *testing.T) {
	h := newFakeHost()
	h.release()
	ts := startServer(t, h)
	postJSON(t, ts.URL+"/api/sessions", `{"id":"s1"}`)
	_, created := postJSON(t, ts.URL+"/api/sessions/s1/messages", `{"text":"hi"}`)
	runID := created["runId"].(string)
	waitRunDone(t, ts.URL+"/api/sessions/s1/runs/"+runID)
	events := readAllSSE(t, ts.URL+"/api/sessions/s1/runs/"+runID+"/events")
	if len(events) != 1 || events[0].Kind != EventRunDone {
		t.Fatalf("无流式 host 的事件 = %+v", events)
	}
}
