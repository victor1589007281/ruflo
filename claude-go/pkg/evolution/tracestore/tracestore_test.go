package tracestore

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/statestore"
)

func TestRefInlineVsBlob(t *testing.T) {
	s := New(statestore.NewMemStore())

	// 短文本内联
	short := s.MakeRef("hello")
	if short.Inline != "hello" || short.Blob != "" || short.Size != 5 {
		t.Fatalf("短文本应内联: %+v", short)
	}
	// 空文本空 Ref
	if got := s.MakeRef(""); got != (Ref{}) {
		t.Fatalf("空文本应返回空 Ref: %+v", got)
	}
	// 长文本落 Blob
	long := strings.Repeat("x", InlineLimit+100)
	lr := s.MakeRef(long)
	if lr.Blob == "" || lr.Inline != "" || lr.Size != len(long) {
		t.Fatalf("长文本应落 Blob: %+v", lr)
	}
	// Resolve 回读
	got, err := s.Resolve(lr)
	if err != nil || got != long {
		t.Fatalf("Blob 反解失败: err=%v len=%d", err, len(got))
	}
	if g, _ := s.Resolve(short); g != "hello" {
		t.Fatalf("内联反解失败: %q", g)
	}
}

func TestBlobDedup(t *testing.T) {
	s := New(statestore.NewMemStore())
	long := strings.Repeat("prompt-", 500) // > InlineLimit
	r1 := s.MakeRef(long)
	r2 := s.MakeRef(long)
	if r1.Blob != r2.Blob {
		t.Fatalf("相同内容应同 hash (去重): %q vs %q", r1.Blob, r2.Blob)
	}
}

func TestWriteReadRun(t *testing.T) {
	s := New(statestore.NewMemStore())
	s.Write(Span{TraceID: "run-a", SpanID: "s1", Kind: "turn", Name: "t0",
		InputRef: s.MakeRef("用户问题"), OutputRef: s.MakeRef("助手回答"), TS: 1})
	s.Write(Span{TraceID: "run-a", SpanID: "s2", Kind: "tool_call", Name: "Read",
		InputRef: s.MakeRef(`{"path":"x"}`), OutputRef: s.MakeRef("文件内容"), TS: 2})
	s.Write(Span{TraceID: "run-b", SpanID: "s3", Kind: "turn", TS: 3}) // 另一 run

	spans, err := s.ReadRun("run-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("run-a 应有 2 个 span, got %d", len(spans))
	}
	// 还原链路: turn 的 input/output + tool_call 的 input/output
	if got, _ := s.Resolve(spans[0].InputRef); got != "用户问题" {
		t.Errorf("turn input 还原错误: %q", got)
	}
	if got, _ := s.Resolve(spans[1].OutputRef); got != "文件内容" {
		t.Errorf("tool_call output 还原错误: %q", got)
	}
	if spans[1].Kind != "tool_call" || spans[1].Name != "Read" {
		t.Errorf("span 顺序/字段错误: %+v", spans[1])
	}
}

func TestTraceIDSanitize(t *testing.T) {
	s := New(statestore.NewMemStore())
	// 含非法 bucket 字符的 TraceID (斜杠/空格/冒号)
	dirty := "run/a b:c"
	s.Write(Span{TraceID: dirty, SpanID: "s1", Kind: "turn", TS: 1})
	spans, err := s.ReadRun(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("消毒后仍应能读回, got %d", len(spans))
	}
	if bucketName(dirty) != "trace-run_a_b_c" {
		t.Errorf("bucket 消毒错误: %q", bucketName(dirty))
	}
}

func TestEmptyTraceIDOrphan(t *testing.T) {
	if bucketName("") != "trace-orphan" || bucketName("  ") != "trace-orphan" {
		t.Fatal("空/空白 TraceID 应归 orphan 桶")
	}
	s := New(statestore.NewMemStore())
	s.Write(Span{TraceID: "", SpanID: "s1", Kind: "turn", TS: 1})
	spans, _ := s.ReadRun("")
	if len(spans) != 1 {
		t.Fatalf("orphan span 应可读回, got %d", len(spans))
	}
}

func TestNilStoreSafe(t *testing.T) {
	var s *Store
	s.Write(Span{TraceID: "x"}) // 不 panic
	if s.WriteErrors() != 0 {
		t.Fatal("nil store WriteErrors 应为 0")
	}
}
