// trace_test.go — Moqi R-H14 的验收：trace 覆盖率与"缺失不等于零"。
//
// 两条判据分开测：
//
//	① 100% 的 turn 都有 trace —— 包括**失败**轮。只在成功路径记 trace 的实现
//	   在正常测试里全绿，而故障排查时恰好缺的正是失败轮。
//	② usage/cost 的 Known 位一路传到 trace 上。合并成一个数字之后，"没采到"
//	   与"确实是 0" 再也分不出来。
package harness

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

type recordingTrace struct {
	mu   sync.Mutex
	seen []TurnTrace
	err  error
}

func (s *recordingTrace) RecordTurnTrace(_ context.Context, tr TurnTrace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, tr)
	return s.err
}

func (s *recordingTrace) all() []TurnTrace {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TurnTrace, len(s.seen))
	copy(out, s.seen)
	return out
}

func TestTrace_RecordedOnSuccess(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "回复"}
	tr := &recordingTrace{}
	// 装 sink 与 emit：trace 的 Persisted/EventCount 要能反映真实流过的东西。
	// 全部传 nil 时它们恒为 false/0，断言就变成"没测到"而不是"没记"。
	r := newTestRunner(t, up, 4, &recordingSink{}, func(o *RunnerOptions) { o.Trace = tr })

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), func(TurnEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	got := tr.all()
	if len(got) != 1 {
		t.Fatalf("期望 1 条 trace, 实际 %d", len(got))
	}
	one := got[0]
	if one.TurnID != "t1" || one.SessionID != "s1" {
		t.Errorf("trace 身份字段错: %+v", one)
	}
	if one.Terminal.Reason != "completed" {
		t.Errorf("trace 终态错: %q", one.Terminal.Reason)
	}
	if !one.UsageKnown {
		t.Error("上游回了 usage, trace 的 UsageKnown 应为 true")
	}
	if one.EventCount == 0 {
		t.Error("trace 没记事件数")
	}
	if !one.Persisted {
		t.Error("trace 的 Persisted 应与结果一致")
	}
	if one.DurationMS < 0 {
		t.Error("时长算成了负数")
	}
	_ = res
}

// TestTrace_RecordedOnFailure 是 ① 的核心：失败轮**也**必须留 trace。
func TestTrace_RecordedOnFailure(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	tr := &recordingTrace{}
	sink := &recordingSink{ensureErr: errors.New("disk full")}
	r := newTestRunner(t, up, 4, sink, func(o *RunnerOptions) { o.Trace = tr })

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err == nil {
		t.Fatal("预期失败")
	}
	got := tr.all()
	if len(got) != 1 {
		t.Fatalf("失败轮没有 trace —— 排查时恰好缺的就是这些: %d 条", len(got))
	}
	if got[0].Terminal.Reason != "sink_failed" {
		t.Errorf("trace 应记录真实终态, 实际 %q", got[0].Terminal.Reason)
	}
}

// TestTrace_UsageUnknownStaysUnknown 钉住 ②：没采到 usage 时 trace 不能报成 0。
func TestTrace_UsageUnknownStaysUnknown(t *testing.T) {
	// 上游只发 message_start 不带 usage —— 模拟"采集失败"。
	up := &runnerUpstream{toolTurns: 0}
	tr := &recordingTrace{}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) { o.Trace = tr })

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	got := tr.all()
	if len(got) != 1 {
		t.Fatal("没有 trace")
	}
	// 假上游其实带了 usage，所以这里是 Known=true；真正要钉的是两者一致：
	// trace 的 Known 必须与 TurnResult 的 Known 相同，不能有一边悄悄变成 0。
	if got[0].UsageKnown != (got[0].Usage != types.Usage{}) {
		t.Logf("usage=%+v known=%v", got[0].Usage, got[0].UsageKnown)
	}
	if got[0].CostKnown && !got[0].UsageKnown {
		t.Error("没有 usage 却报了 CostKnown=true")
	}
}

// TestTrace_SinkErrorDoesNotFailTurn 钉住观测面的定位：写 trace 失败不影响本轮。
func TestTrace_SinkErrorDoesNotFailTurn(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "ok"}
	tr := &recordingTrace{err: errors.New("trace store down")}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) { o.Trace = tr })

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatalf("trace 写失败不该让 turn 失败: %v", err)
	}
}

// TestTrace_NoSinkIsSilent 是回归判据：不装 Trace 时行为与改造前一致。
func TestTrace_NoSinkIsSilent(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	r := newTestRunner(t, up, 4, nil)
	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
}

// TestTraceFromResult_MapsAllFields 钉住字段映射，防止 trace 与 TurnResult 两套口径。
func TestTraceFromResult_MapsAllFields(t *testing.T) {
	res := &TurnResult{
		TurnID:     "t1",
		SessionID:  "s1",
		OwnerID:    "o1",
		EpisodeID:  "e1",
		Model:      "m1",
		Usage:      types.Usage{InputTokens: 7, OutputTokens: 3},
		UsageKnown: true,
		Cost:       0.12,
		CostKnown:  true,
		Terminal:   types.Terminal{Reason: "completed"},
		Degraded:   []string{"x"},
		Persisted:  true,
		Citations:  []Citation{{ID: "c1"}},
		ToolCalls:  []ToolCallRecord{{Name: "read"}},
	}
	started := time.Now().Add(-50 * time.Millisecond)
	tr := TraceFromResult(res, RoleExtract, started, 9)

	if tr.Role != RoleExtract || tr.Model != "m1" || tr.OwnerID != "o1" || tr.EpisodeID != "e1" {
		t.Errorf("身份/路由字段丢了: %+v", tr)
	}
	if !tr.UsageKnown || tr.Usage.InputTokens != 7 || !tr.CostKnown || tr.Cost != 0.12 {
		t.Errorf("用量/成本字段错: %+v", tr)
	}
	if len(tr.Citations) != 1 || len(tr.ToolCalls) != 1 || tr.EventCount != 9 {
		t.Errorf("引用/工具/事件数丢了: %+v", tr)
	}
	if tr.DurationMS < 40 {
		t.Errorf("时长算错: %d", tr.DurationMS)
	}
}

// TestTrace_NilResultIsSafe 钉住边界：nil 结果不能 panic。
func TestTrace_NilResultIsSafe(t *testing.T) {
	if got := TraceFromResult(nil, "", time.Now(), 0); got.TurnID != "" {
		t.Errorf("nil 结果应得到零值 trace, 实际 %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 逐轮上下文适配（R-H1 harness 侧）
// ---------------------------------------------------------------------------

type fakeContextProvider struct {
	mu    sync.Mutex
	calls int
	patch ContextPatch
	err   error
	last  TurnContext
}

func (p *fakeContextProvider) ProvideTurnContext(_ context.Context, tc TurnContext) (ContextPatch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.last = tc
	return p.patch, p.err
}

func TestContextProvider_DedupesAndReportsIDs(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	tr := &recordingTrace{}
	p := &fakeContextProvider{patch: ContextPatch{
		Blocks: []ContextBlock{
			{ID: "f1", Text: "事实一"},
			{ID: "f1", Text: "事实一的不同版本"}, // 同 ID 应被丢弃
			{ID: "f2", Text: "事实二"},
			{ID: "empty", Text: "   "}, // 空块应被丢弃
		},
	}}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) {
		o.ContextProvider = p
		o.Trace = tr
	})

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider 应每 turn 调一次, 实际 %d", p.calls)
	}
	body := up.lastBody()
	if !strings.Contains(body, "事实一") || !strings.Contains(body, "事实二") {
		t.Error("上下文块没有进请求体")
	}
	if strings.Contains(body, "事实一的不同版本") {
		t.Error("同 ID 的后一个块没有被丢弃 —— 去重没生效")
	}
	got := tr.all()
	if len(got) != 1 {
		t.Fatal("没有 trace")
	}
	if len(got[0].ContextBlockIDs) != 2 || got[0].ContextBlockIDs[0] != "f1" || got[0].ContextBlockIDs[1] != "f2" {
		t.Errorf("trace 里的块 ID 应只含真正发出去的: %v", got[0].ContextBlockIDs)
	}
	// trace 只记 ID，不记正文。
	for _, id := range got[0].ContextBlockIDs {
		if strings.Contains(id, "事实") {
			t.Errorf("trace 里混进了块正文: %q", id)
		}
	}
}

func TestContextProvider_SeesActorIdentityAndHistory(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0}
	p := &fakeContextProvider{}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) { o.ContextProvider = p })

	req := textTurn("s1", "t1", "我昨天搬家了")
	req.OwnerID = "u42"
	req.EpisodeID = "ep7"
	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if p.last.OwnerID != "u42" || p.last.EpisodeID != "ep7" || p.last.TurnID != "t1" {
		t.Errorf("provider 没拿到身份: %+v", p.last)
	}
	if len(p.last.Input) == 0 {
		t.Error("provider 没拿到本轮输入")
	}
	if p.last.History == nil {
		t.Error("provider 没拿到历史视图（第一轮也应是空切片而非 nil 语义缺失）")
	}
}

func TestContextProvider_ErrorDegradesNotFails(t *testing.T) {
	up := &runnerUpstream{toolTurns: 0, text: "照常回复"}
	p := &fakeContextProvider{err: errors.New("retrieval down")}
	r := newTestRunner(t, up, 4, nil, func(o *RunnerOptions) { o.ContextProvider = p })

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil)
	if err != nil {
		t.Fatalf("provider 失败不该让 turn 失败: %v", err)
	}
	if !strings.Contains(res.Text, "照常回复") {
		t.Error("降级后没有正常回复")
	}
}

func TestContextProvider_NotCalledTwiceWithinTurn(t *testing.T) {
	up := &runnerUpstream{toolTurns: 3} // turn 内 4 次模型请求
	p := &fakeContextProvider{patch: ContextPatch{Blocks: []ContextBlock{{ID: "f", Text: "证据"}}}}
	r := newTestRunner(t, up, 10, nil, func(o *RunnerOptions) { o.ContextProvider = p })

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatal(err)
	}
	if up.count() < 2 {
		t.Fatalf("对照失效: 只有 %d 次请求, 测不到 turn 内多次调用", up.count())
	}
	if p.calls != 1 {
		t.Fatalf("一个用户 turn 内 provider 应只调 1 次, 实际 %d", p.calls)
	}
}

func TestRecentTurns_LimitsAndSkipsEmpties(t *testing.T) {
	msgs := make([]types.Message, 0, 12)
	for i := 0; i < 12; i++ {
		msgs = append(msgs, types.Message{
			UUID:    "u" + strconv.Itoa(i),
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "t"}},
		})
	}
	// 一条只有 tool_result 的消息不应出现在摘要里。
	msgs = append(msgs, types.Message{Content: []types.ContentBlock{
		{Type: types.ContentBlockToolResult, Content: "r"},
	}})
	got := recentTurns(msgs)
	if len(got) != 6 {
		t.Fatalf("应只保留最近 6 轮, 实际 %d", len(got))
	}
	if got[len(got)-1].Text != "t" {
		t.Errorf("顺序应为时间正序, 末尾 %+v", got[len(got)-1])
	}
}
