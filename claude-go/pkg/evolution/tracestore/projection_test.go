package tracestore

// projection_test.go —— F11 asOf 投影的兑现形态。
//
// 钉四个断言:
//	1. fold 正确性: 会话视图 (意图+产出+工具时间线) 从 Span 重放重建, 与手写
//	   拼装一致 (这正是 F11 要消灭的那类"每次手写拼装")。
//	2. asOf 时间旅行: 同一日志在切点 N 的视图 == 只写入前 N 条事件后的全量视图
//	   (纯函数 + 前缀重放的正确性定义)。
//	3. checkpoint 续放: 版本匹配时从水位续放, 结果与全量重放一致; 版本不匹配
//	   (或水位越界) 时静默降级全量重放, 结果仍一致。
//	4. 装配纪律: nil store / 不完整 fold 显式报错 (装配错误不该静默)。

import (
	"errors"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// sessionView 学习管线最想要的"当时上下文"视图: 逐轮的用户意图 + assistant
// 产出 + 工具调用轨迹。
type sessionView struct {
	turns []string // 每轮一行摘要
	tools []string // 每次工具调用一行
}

func newSessionProjection() ProjectionDefinition[sessionView, Span] {
	return ProjectionDefinition[sessionView, Span]{
		Key:          "session_view",
		StateVersion: 1,
		Init:         func() sessionView { return sessionView{} },
		Apply: func(v sessionView, sp Span) sessionView {
			switch sp.Kind {
			case KindTurn:
				// 测试 span 用短文本, Ref 皆内联; 真实正文走 Resolve (blob 路径)
				v.turns = append(v.turns, sp.InputRef.Inline+" → "+sp.OutputRef.Inline)
			case KindToolCall:
				v.tools = append(v.tools, sp.Name)
			}
			return v
		},
	}
}

func newProjStore(t *testing.T) (*Store, *Projection[sessionView]) {
	t.Helper()
	st := New(statestore.NewMemStore())
	p, err := NewProjection(st, newSessionProjection())
	if err != nil {
		t.Fatalf("NewProjection: %v", err)
	}
	return st, p
}

// TestProjection_会话视图重放 与手写拼装逐行一致。
func TestProjection_会话视图重放(t *testing.T) {
	st, p := newProjStore(t)

	st.Write(Span{TraceID: "s-run", SpanID: "1", Kind: KindTurn, Name: "t0",
		InputRef: st.MakeRef("用户问题"), OutputRef: st.MakeRef("助手回答"), TS: 1})
	st.Write(Span{TraceID: "s-run", SpanID: "2", Kind: KindToolCall, Name: "Read", TS: 2})
	st.Write(Span{TraceID: "s-run", SpanID: "3", Kind: KindTurn, Name: "t1",
		InputRef: st.MakeRef("继续"), OutputRef: st.MakeRef("再答"), TS: 3})

	got, err := p.Latest("s-run")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got.Count != 3 || got.AsOfSeq != 3 {
		t.Fatalf("水位应为 3, got asOf=%d count=%d", got.AsOfSeq, got.Count)
	}
	want := []string{"用户问题 → 助手回答", "继续 → 再答"}
	if len(got.State.turns) != 2 || got.State.turns[0] != want[0] || got.State.turns[1] != want[1] {
		t.Fatalf("会话视图重放失真: %+v", got.State.turns)
	}
	if len(got.State.tools) != 1 || got.State.tools[0] != "Read" {
		t.Fatalf("工具时间线失真: %+v", got.State.tools)
	}
}

// TestProjection_asOf时间旅行 切点 N 的视图 == 只写入前 N 条的全量视图。
func TestProjection_asOf时间旅行(t *testing.T) {
	_, p := newProjStore(t)

	full := [3]Span{
		{TraceID: "tt-run", SpanID: "1", Kind: KindTurn, InputRef: Ref{Inline: "q1"}, OutputRef: Ref{Inline: "a1"}, TS: 1},
		{TraceID: "tt-run", SpanID: "2", Kind: KindToolCall, Name: "Bash", TS: 2},
		{TraceID: "tt-run", SpanID: "3", Kind: KindTurn, InputRef: Ref{Inline: "q2"}, OutputRef: Ref{Inline: "a2"}, TS: 3},
	}
	// 全量写入到 p 的 store (时间旅行读的是这里)
	for _, sp := range full {
		p.store.Write(sp)
	}

	// 前缀模拟: 只写入前 k 条, 取全量视图; 与"全写入后 asOf(k)"对比。
	for k := 0; k <= 3; k++ {
		st2 := New(statestore.NewMemStore())
		p2, err := NewProjection(st2, newSessionProjection())
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < k; i++ {
			st2.Write(full[i])
		}
		prefix, err := p2.Latest("tt-run")
		if err != nil {
			t.Fatal(err)
		}
		// 对照: 全量写入后取 asOf(k)
		got, err := p.AsOf("tt-run", k)
		if err != nil {
			t.Fatal(err)
		}
		if got.AsOfSeq != k || got.Count != k {
			t.Fatalf("asOf(%d) 水位应恰为 %d, got %d", k, k, got.AsOfSeq)
		}
		if strings.Join(got.State.turns, "|") != strings.Join(prefix.State.turns, "|") ||
			strings.Join(got.State.tools, "|") != strings.Join(prefix.State.tools, "|") {
			t.Fatalf("asOf(%d) 与前缀全量视图不一致: %+v vs %+v", k, got.State, prefix.State)
		}
	}

	// 切点超界/负数 → 全量视图 (不 panic, 语义取末尾)
	tail, err := p.AsOf("tt-run", -1)
	if err != nil {
		t.Fatal(err)
	}
	if tail.Count != 3 {
		t.Fatalf("asOf(-1) 应取全量, got %d", tail.Count)
	}
}

// TestProjection_checkpoint续放 版本匹配续放 == 全量重放; 版本失配静默降级。
func TestProjection_checkpoint续放(t *testing.T) {
	st, p := newProjStore(t)

	for i, sp := range []Span{
		{TraceID: "ck-run", SpanID: "1", Kind: KindTurn, InputRef: Ref{Inline: "q1"}, OutputRef: Ref{Inline: "a1"}, TS: 1},
		{TraceID: "ck-run", SpanID: "2", Kind: KindTurn, InputRef: Ref{Inline: "q2"}, OutputRef: Ref{Inline: "a2"}, TS: 2},
	} {
		_ = i
		st.Write(sp)
	}

	// 假设此前在 seq=1 做过 checkpoint (ver=1)
	baseline, err := p.AsOf("ck-run", 1)
	if err != nil {
		t.Fatal(err)
	}
	ckpt := ProjectionCheckpoint[sessionView]{
		Key: "session_view", StateVersion: 1, Seq: 1, State: baseline.State,
	}
	restored, err := Restore(p, ckpt, "ck-run")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	full, _ := p.Latest("ck-run")
	if strings.Join(restored.State.turns, "|") != strings.Join(full.State.turns, "|") {
		t.Fatalf("checkpoint 续放应与全量重放一致: %+v vs %+v", restored.State.turns, full.State.turns)
	}

	// 版本失配 (ver=0) → 静默降级全量重放, 结果仍一致
	stale := ckpt
	stale.StateVersion = 0
	got, err := Restore(p, stale, "ck-run")
	if err != nil {
		t.Fatalf("版本失配应降级而非报错: %v", err)
	}
	if strings.Join(got.State.turns, "|") != strings.Join(full.State.turns, "|") {
		t.Fatalf("降级重放应与全量一致: %+v vs %+v", got.State.turns, full.State.turns)
	}

	// 水位越界 (seq=99) → 同样降级
	far := ckpt
	far.Seq = 99
	got, err = Restore(p, far, "ck-run")
	if err != nil {
		t.Fatalf("水位越界应降级而非报错: %v", err)
	}
	if len(got.State.turns) != 2 {
		t.Fatalf("降级重放应看到 2 轮, got %+v", got.State.turns)
	}
}

// TestProjection_装配纪律 nil store / 不完整 fold 显式报错。
func TestProjection_装配纪律(t *testing.T) {
	if _, err := NewProjection[sessionView](nil, newSessionProjection()); !errors.Is(err, ErrNoProjection) {
		t.Fatalf("nil store 应返回 ErrNoProjection, got %v", err)
	}
	st := New(statestore.NewMemStore())
	if _, err := NewProjection(st, ProjectionDefinition[sessionView, Span]{Key: "x"}); err == nil {
		t.Fatal("缺 Init/Apply 的 fold 应装配报错")
	}
	var nilP *Projection[sessionView]
	if _, err := Restore(nilP, ProjectionCheckpoint[sessionView]{}, "x"); !errors.Is(err, ErrNoProjection) {
		t.Fatalf("nil 投影 Restore 应返回 ErrNoProjection, got %v", err)
	}
}
