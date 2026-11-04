// runner_test.go — Moqi R-H3 / R-H4 / R-H10 / R-H17 的门面验收。
//
// 分层判据（与 ephemeral_test.go 同一套思路）：
//
//	判据本身      → budgetGate 的单元断言（toolCalls/cost/wall 三档归因）；
//	判据有没有通电 → 起真引擎 + 假上游，走 RunTurn 全链路，看**宿主看到了什么**；
//	时序对不对    → sink 记录调用序号，断言 EnsureUser 在首次模型请求之前、
//	                AppendAssistant 在终态事件之前。
//
// 第三条是本文件的要害。只验"三个方法都被调用过"的话，一个把 AppendAssistant
// 放在 emitTerminal 之后的实现照样能通过——而那种实现在宿主侧留下的是
// "事件说完成了、库里查不到"的窗口，只在慢宿主上现形。
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ---------------------------------------------------------------------------
// 假上游 / 假 sink / 假工具
// ---------------------------------------------------------------------------

// runnerUpstream 是可编程的假上游：前 toolTurns 次请求返回 tool_use，之后收口。
type runnerUpstream struct {
	mu        sync.Mutex
	bodies    []string
	calls     int
	toolTurns int
	text      string
}

func (u *runnerUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		u.mu.Lock()
		u.calls++
		call := u.calls
		u.bodies = append(u.bodies, sb.String())
		u.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if call <= u.toolTurns {
			_, _ = w.Write([]byte(runnerSSEToolUse("tu" + strconv.Itoa(call))))
			return
		}
		text := u.text
		if text == "" {
			text = "好的"
		}
		_, _ = w.Write([]byte(runnerSSEText(text)))
	})
}

func (u *runnerUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *runnerUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return ""
	}
	return u.bodies[len(u.bodies)-1]
}

func sseFrame(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n"
}

func runnerSSEToolUse(toolID string) string {
	var b strings.Builder
	b.WriteString(sseFrame(types.StreamDelta{
		Type:    "message_start",
		Message: &types.APIResponse{ID: "msg_x", Model: "m", Usage: &types.Usage{InputTokens: 10}},
	}))
	b.WriteString(sseFrame(types.StreamDelta{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: &types.ContentBlock{
			Type: types.ContentBlockToolUse, ID: toolID, Name: "noop_tool",
		},
	}))
	b.WriteString(sseFrame(types.StreamDelta{
		Type: "content_block_delta", Index: 0,
		Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: "{}"},
	}))
	b.WriteString(sseFrame(types.StreamDelta{Type: "content_block_stop", Index: 0}))
	b.WriteString(sseFrame(types.StreamDelta{
		Type:  "message_delta",
		Usage: &types.Usage{OutputTokens: 5},
		Delta: &types.DeltaContent{StopReason: "tool_use"},
	}))
	return b.String()
}

func runnerSSEText(text string) string {
	var b strings.Builder
	b.WriteString(sseFrame(types.StreamDelta{
		Type:    "message_start",
		Message: &types.APIResponse{ID: "msg_x", Model: "m", Usage: &types.Usage{InputTokens: 10}},
	}))
	b.WriteString(sseFrame(types.StreamDelta{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: &types.ContentBlock{Type: types.ContentBlockText},
	}))
	b.WriteString(sseFrame(types.StreamDelta{
		Type: "content_block_delta", Index: 0,
		Delta: &types.DeltaContent{Type: "text_delta", Text: text},
	}))
	b.WriteString(sseFrame(types.StreamDelta{Type: "content_block_stop", Index: 0}))
	b.WriteString(sseFrame(types.StreamDelta{
		Type:  "message_delta",
		Usage: &types.Usage{OutputTokens: 5},
		Delta: &types.DeltaContent{StopReason: "end_turn"},
	}))
	return b.String()
}

// noopTool 是最小的可注册工具，用来让引擎有工具面。
type noopTool struct{}

func (noopTool) Name() string                 { return "noop_tool" }
func (noopTool) Description() string          { return "noop" }
func (noopTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (noopTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "ok"}, nil
}
func (noopTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (noopTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}
func (noopTool) IsReadOnly(json.RawMessage) bool { return true }

// recordingSink 记录调用顺序，并可注入失败。
type recordingSink struct {
	mu        sync.Mutex
	order     []string
	users     []UserRecord
	assists   []AssistantRecord
	states    []TurnState
	ensureErr error
	appendErr error
	markErr   error
	// onBeforeModel 在 EnsureUser 时回调，用于断言"此刻还没发请求"。
	onBeforeModel func()
}

func (s *recordingSink) record(name string) {
	s.order = append(s.order, name)
}

func (s *recordingSink) EnsureUser(_ context.Context, rec UserRecord) (bool, error) {
	s.mu.Lock()
	s.record("ensure_user")
	s.users = append(s.users, rec)
	err := s.ensureErr
	cb := s.onBeforeModel
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return false, err
}

func (s *recordingSink) AppendAssistant(_ context.Context, rec AssistantRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("append_assistant")
	s.assists = append(s.assists, rec)
	return s.appendErr
}

func (s *recordingSink) MarkTurn(_ context.Context, st TurnState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("mark_turn")
	s.states = append(s.states, st)
	return s.markErr
}

func (s *recordingSink) orderSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// newTestRunner 走生产构造路径（NewQueryEngine）建引擎，再包进 Runner。
func newTestRunner(t *testing.T, up *runnerUpstream, maxTurns int, sink TurnSink, opts ...func(*RunnerOptions)) *Runner {
	t.Helper()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"

	eng := engine.NewQueryEngine(&engine.Config{Model: "m", MaxTurns: maxTurns},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	ro := RunnerOptions{Engine: eng, Sink: sink}
	for _, f := range opts {
		f(&ro)
	}
	return NewRunner(ro)
}

func textTurn(session, turnID, text string) TurnRequest {
	return TurnRequest{
		SessionID: session,
		TurnID:    turnID,
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
	}
}

// ---------------------------------------------------------------------------
// R-H3: 结构化结果
// ---------------------------------------------------------------------------

// TestRunTurn_ReturnsStructuredResult 钉住 R-H3：业务层不解析 prose 就能拿到
// 文本、usage、工具调用与终态。
func TestRunTurn_ReturnsStructuredResult(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1, text: "好的，搬家公司推荐如下"}
	sink := &recordingSink{}
	r := newTestRunner(t, up, 6, sink)

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "推荐搬家公司"), nil)
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if res == nil {
		t.Fatal("返回的 TurnResult 为 nil —— 契约要求恒非 nil")
	}
	if !strings.Contains(res.Text, "搬家公司") {
		t.Errorf("Text 未包含模型输出: %q", res.Text)
	}
	if !res.UsageKnown {
		t.Error("上游回了 usage 但 UsageKnown=false")
	}
	if res.Usage.InputTokens == 0 {
		t.Errorf("usage 未采集到: %+v", res.Usage)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("应记录 1 次工具调用, 实际 %d: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	if res.ToolCalls[0].Name != "noop_tool" {
		t.Errorf("工具名错: %q", res.ToolCalls[0].Name)
	}
	if res.Terminal.Reason != "completed" {
		t.Errorf("终态应为 completed, 实际 %q", res.Terminal.Reason)
	}
	if !res.Persisted {
		t.Error("sink 成功时 Persisted 应为 true")
	}
	if res.TurnID != "t1" || res.SessionID != "s1" {
		t.Errorf("身份字段丢失: %+v", res)
	}
	// OwnerID 空输入应归一成 DefaultOwnerID，而不是空串（R-H6）。
	if res.OwnerID != DefaultOwnerID {
		t.Errorf("空 owner 应归一为 %q, 实际 %q", DefaultOwnerID, res.OwnerID)
	}
}

// TestRunTurn_EmitsMonotonicSeqAndOneTerminal 钉住 R-H12 的两条硬约束：
// Seq 单调递增唯一；终态事件恰好一次。
func TestRunTurn_EmitsMonotonicSeqAndOneTerminal(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 6, nil)

	var events []TurnEvent
	_, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), func(ev TurnEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("没有发出任何事件")
	}
	var terminals int
	seen := make(map[int64]bool, len(events))
	var last int64
	for i, ev := range events {
		if ev.Seq <= last {
			t.Fatalf("第 %d 个事件 Seq=%d 未递增 (前一个 %d)", i, ev.Seq, last)
		}
		last = ev.Seq
		if seen[ev.Seq] {
			t.Fatalf("Seq %d 重复", ev.Seq)
		}
		seen[ev.Seq] = true
		if ev.TurnID != "t1" {
			t.Errorf("事件缺 TurnID: %+v", ev)
		}
		if ev.Kind == EventTerminal {
			terminals++
			if ev.Terminal == nil {
				t.Error("终态事件没有 Terminal 指针")
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("终态事件应恰好 1 次, 实际 %d", terminals)
	}
	if events[len(events)-1].Kind != EventTerminal {
		t.Errorf("最后一个事件应为终态, 实际 %s", events[len(events)-1].Kind)
	}
}

// ---------------------------------------------------------------------------
// R-H4: 宿主持久化与时序
// ---------------------------------------------------------------------------

// TestRunTurn_EnsureUserBeforeFirstModelCall 是 R-H4 验收第 1 条：
// 用户消息落盘**先于**任何模型调用。
//
// 判据落在"回调时上游请求数仍为 0"，而不是"EnsureUser 被调用过"——后者在
// "先调模型再落盘"的实现上也成立。
func TestRunTurn_EnsureUserBeforeFirstModelCall(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	var callsAtEnsure int
	sink := &recordingSink{}
	sink.onBeforeModel = func() {
		callsAtEnsure = up.count()
	}
	r := newTestRunner(t, up, 4, sink)

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	if callsAtEnsure != 0 {
		t.Fatalf("EnsureUser 被调用时已有 %d 次模型请求 —— 落盘必须早于模型调用", callsAtEnsure)
	}
	if up.count() == 0 {
		t.Fatal("对照失效: 整个 run 一次模型都没调")
	}
}

// TestRunTurn_AppendAssistantBeforeTerminalEvent 是 R-H4 验收第 2 条：
// 助手消息在终态事件**之前**落盘。
func TestRunTurn_AppendAssistantBeforeTerminalEvent(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	sink := &recordingSink{}
	r := newTestRunner(t, up, 4, sink)

	// 终态事件触发的那一刻**立刻**读 sink 的调用记录, 而不是跑完再读。
	//
	// 跑完再读只能证明"后来确实写过", 证明不了"写在事件之前" —— 一个把
	// AppendAssistant 放在 emitTerminal 之后的实现, 跑完之后顺序记录一模一样。
	var orderAtTerminal []string
	sawTerminal := false
	_, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), func(ev TurnEvent) {
		if ev.Kind == EventTerminal && !sawTerminal {
			sawTerminal = true
			orderAtTerminal = sink.orderSnapshot()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawTerminal {
		t.Fatal("没有观察到终态事件")
	}
	if !containsString(orderAtTerminal, "append_assistant") {
		t.Fatalf("终态事件发出时助手消息还没落盘: 当时 sink 记录 = %v", orderAtTerminal)
	}
	order := sink.orderSnapshot()
	// 4 次: ensure_user → mark_turn(running) → append_assistant → mark_turn(终态)。
	// running 标记必须在模型调用之前写（Q-H11 的崩溃恢复扫描只看得见它），
	// 所以它出现在 ensure_user 之后、append_assistant 之前。
	if len(order) != 4 {
		t.Fatalf("sink 应被调用 4 次 (ensure/running/append/终态), 实际 %v", order)
	}
	if order[0] != "ensure_user" || order[1] != "mark_turn" || order[2] != "append_assistant" || order[3] != "mark_turn" {
		t.Fatalf("sink 调用顺序错: %v", order)
	}
	if got := sink.states[0].State; got != stateRunning {
		t.Fatalf("第一个 MarkTurn 应为 running（Q-H11）, 实际 %q", got)
	}
}

// TestRunTurn_EnsureUserFailureSkipsModelCall 是 R-H4 验收的反面：
// 落盘失败 ⇒ 不调模型，且错误分类为 sink。
func TestRunTurn_EnsureUserFailureSkipsModelCall(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	sink := &recordingSink{ensureErr: errors.New("disk full")}
	r := newTestRunner(t, up, 4, sink)

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err == nil {
		t.Fatal("落盘失败应返回错误")
	}
	if up.count() != 0 {
		t.Fatalf("落盘失败后仍调了 %d 次模型 —— 消息会丢而对话照常进行", up.count())
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindSink {
		t.Fatalf("错误分类应为 sink, 实际 %v", err)
	}
	if res == nil || res.Persisted {
		t.Fatal("Persisted 应为 false 且 result 非 nil")
	}
	if res.Terminal.Reason != "sink_failed" {
		t.Errorf("终态应为 sink_failed, 实际 %q", res.Terminal.Reason)
	}
}

// TestRunTurn_AppendAssistantFailureKeepsTextAndReportsUnpersisted 钉住
// "写失败不丢已流出的文本，且如实报 Persisted=false"。
//
// 后者是要害：报 Persisted=true 会让业务层不重试，那条助手消息就永久丢失。
func TestRunTurn_AppendAssistantFailureKeepsTextAndReportsUnpersisted(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "会议纪要如下"}
	sink := &recordingSink{appendErr: errors.New("db down")}
	r := newTestRunner(t, up, 4, sink)

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err == nil {
		t.Fatal("AppendAssistant 失败应返回错误")
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindSink {
		t.Fatalf("错误分类应为 sink, 实际 %v", err)
	}
	if res.Persisted {
		t.Fatal("AppendAssistant 失败却报了 Persisted=true —— 业务层不会重试")
	}
	if !strings.Contains(res.Text, "会议纪要") {
		t.Errorf("已流出的文本被丢掉: %q", res.Text)
	}
	if !containsString(res.Degraded, "assistant_persist_failed") {
		t.Errorf("Degraded 未标记持久化失败: %v", res.Degraded)
	}
	// MarkTurn 仍应被调用, 且**终态那条**带 Persisted=false + 失败原因。
	// states[0] 是模型调用前的 running 标记（Q-H11），它不带失败原因也正常。
	if len(sink.states) < 2 {
		t.Fatalf("应有 running 与终态两次 MarkTurn, 实际 %d 条", len(sink.states))
	}
	final := sink.states[len(sink.states)-1]
	if final.Persisted {
		t.Error("MarkTurn 报了 Persisted=true")
	}
	if final.Reason == "" {
		t.Error("MarkTurn 没带失败原因")
	}
}

// TestRunTurn_MarkTurnFailureDoesNotUnpersist 钉住边界：
// MarkTurn 失败**不**能把 Persisted 翻成 false。
//
// 消息本身已经落盘，翻 false 会让业务层重发 → 重复消息。
func TestRunTurn_MarkTurnFailureDoesNotUnpersist(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	sink := &recordingSink{markErr: errors.New("state store lag")}
	r := newTestRunner(t, up, 4, sink)

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err != nil {
		t.Fatalf("MarkTurn 失败不该让整轮失败: %v", err)
	}
	if !res.Persisted {
		t.Fatal("消息已落盘, Persisted 应为 true")
	}
	if !containsString(res.Degraded, "mark_turn_failed") {
		t.Errorf("应记录 mark_turn_failed 降级: %v", res.Degraded)
	}
}

// TestRunTurn_NilSinkStillRuns 钉住"不装 sink 也能跑"：TurnResult.Persisted=false 但无错。
func TestRunTurn_NilSinkStillRuns(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "hi there"}
	r := newTestRunner(t, up, 4, nil)

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Persisted {
		t.Error("没有 sink 却报了 Persisted=true")
	}
	if !strings.Contains(res.Text, "hi there") {
		t.Errorf("文本丢失: %q", res.Text)
	}
}

// TestRunTurn_EmptyTurnIDRejected 钉住幂等键缺失的处置：明确失败，不静默生成。
func TestRunTurn_EmptyTurnIDRejected(t *testing.T) {
	up := &runnerUpstream{}
	r := newTestRunner(t, up, 4, nil)

	_, err := r.RunTurn(context.Background(), TurnRequest{SessionID: "s1"}, nil)
	if err == nil {
		t.Fatal("空 TurnID 应报错")
	}
	if up.count() != 0 {
		t.Error("空 TurnID 时不该调模型")
	}
}

// ---------------------------------------------------------------------------
// Q-H5: 每 session 同时最多一个 run
// ---------------------------------------------------------------------------

func TestRunTurn_RejectsConcurrentRunOnSameSession(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)

	// 手工占用 session（模拟一个在途 run），再发起第二个。
	r.mu.Lock()
	r.sessions["s1"] = "inflight"
	r.mu.Unlock()

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t2", "hi"), nil)
	if err == nil {
		t.Fatal("同 session 并发 run 应被拒绝")
	}
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("应为 ErrSessionBusy, 实际 %v", err)
	}
	if res == nil || res.Terminal.Reason != "busy" {
		t.Fatalf("终态应为 busy, 实际 %+v", res.Terminal)
	}
	if up.count() != 0 {
		t.Error("被拒的 run 不该调模型")
	}
}

// TestRunTurn_DifferentSessionsRunIndependently 是对照：
// 不同 session 不受上面那道闸的限制。
func TestRunTurn_DifferentSessionsRunIndependently(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)

	if _, err := r.RunTurn(context.Background(), textTurn("sA", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunTurn(context.Background(), textTurn("sB", "t2", "hi"), nil); err != nil {
		t.Fatalf("不同 session 应能跑: %v", err)
	}
}

// TestRunTurn_SessionReleasedAfterRun 钉住"跑完就释放"：否则第二个 run 永远被拒。
func TestRunTurn_SessionReleasedAfterRun(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)

	for i := 0; i < 3; i++ {
		if _, err := r.RunTurn(context.Background(), textTurn("s1", "t"+strconv.Itoa(i), "hi"), nil); err != nil {
			t.Fatalf("第 %d 次 run 失败 (session 未释放?): %v", i+1, err)
		}
	}
	if r.CountRunning() != 0 {
		t.Errorf("跑完后仍有 %d 个在途 run", r.CountRunning())
	}
}

// ---------------------------------------------------------------------------
// R-H10: 预算
// ---------------------------------------------------------------------------

func TestBudgets_ToolCallLimitTrips(t *testing.T) {
	g := &budgetGate{maxToolCalls: 2, cancel: func() {}}
	if g.observeToolCall() || g.observeToolCall() {
		t.Fatal("前两次不该触发")
	}
	if !g.observeToolCall() {
		t.Fatal("第三次应触发")
	}
	if g.exhaustedReason() != "tool_calls" {
		t.Errorf("归因应为 tool_calls, 实际 %q", g.exhaustedReason())
	}
}

func TestBudgets_CostLimitTrips(t *testing.T) {
	g := &budgetGate{
		maxCost: 0.05,
		cancel:  func() {},
		price:   func(_ string, u types.Usage) float64 { return float64(u.InputTokens) * 0.001 },
		model:   "m",
	}
	if g.observeUsage(types.Usage{InputTokens: 10}) { // 0.01
		t.Fatal("第一次不该触发")
	}
	if !g.observeUsage(types.Usage{InputTokens: 60}) { // 累计 0.07 > 0.05
		t.Fatal("第二次应触发")
	}
	if g.exhaustedReason() != "cost" {
		t.Errorf("归因应为 cost, 实际 %q", g.exhaustedReason())
	}
}

func TestBudgets_NoPriceMeansNoCostLimit(t *testing.T) {
	g := &budgetGate{maxCost: 0.01, cancel: func() {}}
	for i := 0; i < 10; i++ {
		if g.observeUsage(types.Usage{InputTokens: 1000}) {
			t.Fatal("没有价目表时不该按成本拦截 —— 算不出钱就不能假装有上限")
		}
	}
	if g.exhaustedReason() != "" {
		t.Errorf("未触发却记了原因: %q", g.exhaustedReason())
	}
}

func TestBudgets_FirstReasonWins(t *testing.T) {
	g := &budgetGate{maxToolCalls: 1, maxCost: 0.001, cancel: func() {},
		price: func(string, types.Usage) float64 { return 1 }, model: "m"}
	g.observeToolCall() // 第二次触发 tool_calls
	g.observeToolCall()
	g.observeUsage(types.Usage{InputTokens: 1})
	if g.exhaustedReason() != "tool_calls" {
		t.Errorf("应保留首个原因 tool_calls, 实际 %q", g.exhaustedReason())
	}
}

// TestRunTurn_WallBudgetCancels 是墙钟预算的**接线**验收：
// 上游永不返回，MaxWall 到点必须让 RunTurn 返回，且终态归因为 budget 而非 cancelled。
func TestRunTurn_WallBudgetCancels(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"
	eng := engine.NewQueryEngine(&engine.Config{Model: "m", MaxTurns: 4},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)
	r := NewRunner(RunnerOptions{Engine: eng})

	req := textTurn("s1", "t1", "hi")
	req.Policy.MaxWall = 150 * time.Millisecond

	done := make(chan struct{})
	var res *TurnResult
	var err error
	go func() {
		defer close(done)
		res, err = r.RunTurn(context.Background(), req, nil)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("MaxWall 到点后 run 没有返回 —— 墙钟闸没接上")
	}
	if err == nil {
		t.Fatal("墙钟耗尽应返回错误")
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindBudget {
		t.Fatalf("错误分类应为 budget, 实际 %v", err)
	}
	if res.Terminal.Reason != "budget_exhausted" {
		t.Fatalf("终态应为 budget_exhausted (而不是 cancelled), 实际 %q", res.Terminal.Reason)
	}
}

// TestRunTurn_ToolCallLimitTerminal 是工具预算的端到端接线验收：
// 假上游一直要调工具，MaxToolCalls 到点必须停，且终态是 budget_exhausted。
func TestRunTurn_ToolCallLimitTerminal(t *testing.T) {
	up := &runnerUpstream{toolTurns: 100}
	r := newTestRunner(t, up, 20, nil)

	req := textTurn("s1", "t1", "干活")
	req.Policy.MaxToolCalls = 2

	res, err := r.RunTurn(context.Background(), req, nil)
	if err == nil {
		t.Fatal("工具预算耗尽应返回错误")
	}
	if res.Terminal.Reason != "budget_exhausted" {
		t.Fatalf("终态应为 budget_exhausted, 实际 %q", res.Terminal.Reason)
	}
	// 上限 2 ⇒ 最多两次 LLM 请求 (第一次拿回 1 个 tool_use 且未超, 第二次超限)。
	if n := up.count(); n > 3 {
		t.Errorf("预算耗尽后仍在调模型: %d 次", n)
	}
}

// ---------------------------------------------------------------------------
// R-H17: 角色路由与成本归集
// ---------------------------------------------------------------------------

func TestRunTurn_RoleRoutingPicksModelAndReportsIt(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{
			RoleDialogue: "strong-model",
			RoleExtract:  "cheap-model",
		}
	})

	req := textTurn("s1", "t1", "抽取这条事实")
	req.Role = RoleExtract

	res, err := r.RunTurn(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "cheap-model" {
		t.Fatalf("抽取角色应走便宜模型, 实际 %q", res.Model)
	}
	if !strings.Contains(up.lastBody(), "cheap-model") {
		t.Error("请求体里没有便宜模型 —— 路由没进到真实请求")
	}
}

func TestRunTurn_PolicyModelRolesOverrideDefaults(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{RoleExtract: "cheap"}
	})

	req := textTurn("s1", "t1", "hi")
	req.Role = RoleExtract
	req.Policy.ModelRoles = map[string]string{RoleExtract: "per-turn"}

	res, err := r.RunTurn(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "per-turn" {
		t.Fatalf("TurnPolicy 应覆盖装配默认, 实际 %q", res.Model)
	}
}

func TestRunTurn_UnknownRoleRejectedNotSilentlyDialogue(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{"extrcat": "cheap"} // 拼错
	})

	req := textTurn("s1", "t1", "hi")
	req.Role = "extrcat"

	_, err := r.RunTurn(context.Background(), req, nil)
	if err == nil {
		t.Fatal("未知角色应报错, 而不是静默当 dialogue")
	}
	if up.count() != 0 {
		t.Error("未知角色不该调模型")
	}
}

func TestRunTurn_CostComputedFromUsage(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.Price = func(_ string, u types.Usage) float64 {
			return float64(u.InputTokens)*0.01 + float64(u.OutputTokens)*0.02
		}
	})
	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CostKnown {
		t.Fatal("装了价目表且有 usage, CostKnown 应为 true")
	}
	want := float64(res.Usage.InputTokens)*0.01 + float64(res.Usage.OutputTokens)*0.02
	if res.Cost != want {
		t.Errorf("成本算错: got %v want %v", res.Cost, want)
	}
}

func TestRunTurn_CostUnknownWithoutPrice(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)
	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.CostKnown {
		t.Error("没有价目表却报了 CostKnown=true")
	}
}

// ---------------------------------------------------------------------------
// R-H11: 模态协商
// ---------------------------------------------------------------------------

func TestRunTurn_ImageToTextOnlyModelTypedError(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{RoleDialogue: "deepseek-v4.1-flash"}
	})

	req := textTurn("s1", "t1", "看看这张图")
	req.Input = append(req.Input, types.ContentBlock{
		Type: types.ContentBlockImage,
		Source: &types.MediaSource{
			Type: "base64", MediaType: "image/png", Data: "AAAA",
		},
	})

	_, err := r.RunTurn(context.Background(), req, nil)
	if err == nil {
		t.Fatal("不支持图片的模型收到图片应返回 typed error")
	}
	if !errors.Is(err, ErrUnsupportedModality) {
		t.Fatalf("应为 ErrUnsupportedModality, 实际 %v", err)
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindModality {
		t.Fatalf("错误分类应为 modality, 实际 %v", err)
	}
	if up.count() != 0 {
		t.Error("模态不支持时不该发请求")
	}
}

func TestRunTurn_ImageToVisionModelAllowed(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{RoleDialogue: "claude-sonnet-5-5"}
	})

	req := textTurn("s1", "t1", "看看这张图")
	req.Input = append(req.Input, types.ContentBlock{
		Type:   types.ContentBlockImage,
		Source: &types.MediaSource{Type: "base64", MediaType: "image/png", Data: "AAAA"},
	})

	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatalf("支持视觉的模型应放行: %v", err)
	}
	if up.count() == 0 {
		t.Error("没有发出请求")
	}
}

func TestRunTurn_AttachmentRefCheckedToo(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{RoleDialogue: "deepseek-v4.1-flash"}
	})
	req := textTurn("s1", "t1", "附件")
	req.Attachments = []AttachmentRef{
		{ID: "a1", Kind: AttachmentImage, MIME: "image/png", BlobHash: "h", Name: "photo.png"},
	}
	_, err := r.RunTurn(context.Background(), req, nil)
	if !errors.Is(err, ErrUnsupportedModality) {
		t.Fatalf("AttachmentRef 里的图片也应被判定, 实际 %v", err)
	}
}

// TestModalityOverrideWins 钉住宿主覆盖优先于内置表。
func TestModalityOverrideWins(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.RoleModels = map[string]string{RoleDialogue: "deepseek-v4.1-flash"}
		o.Modality = func(_, kind string) bool { return kind == AttachmentImage }
	})
	req := textTurn("s1", "t1", "图")
	req.Input = append(req.Input, types.ContentBlock{
		Type:   types.ContentBlockImage,
		Source: &types.MediaSource{Type: "base64", MediaType: "image/png", Data: "A"},
	})
	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatalf("宿主覆盖应生效: %v", err)
	}
}

// ---------------------------------------------------------------------------
// R-H1 接线: 逐轮上下文通过 Runner 真的进入了请求
// ---------------------------------------------------------------------------

func TestRunTurn_EphemeralProviderReachesRequest(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	var calls int
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.Context = func(_ context.Context, _ engine.EphemeralView) ([]types.ContentBlock, string, error) {
			calls++
			return []types.ContentBlock{{Type: types.ContentBlockText, Text: "证据-EV"}}, "", nil
		}
	})
	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("每个用户 turn 应只调 provider 一次, 实际 %d", calls)
	}
	if !strings.Contains(up.lastBody(), "证据-EV") {
		t.Error("临时块没有进入请求体 —— Runner 到 engine 的接线断了")
	}
	for _, text := range engineTexts(r.opts.Engine.GetMessages()) {
		if strings.Contains(text, "证据-EV") {
			t.Fatalf("临时块进了 transcript: %q", text)
		}
	}
}

func engineTexts(msgs []types.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		for _, b := range m.Content {
			out = append(out, b.Text)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
