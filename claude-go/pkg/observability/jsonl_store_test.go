package observability

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJSONLStoreWriteAndRead(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer store.Close()

	e := Event{
		Type:      EvtLLMCallComplete,
		Timestamp: time.Now(),
		TraceID:   "trace-1",
		Module:    "llm",
		Name:      "success",
		Payload:   map[string]interface{}{"model": "gpt-4"},
	}
	if err := store.WriteEvent(e); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}

	fname := filepath.Join(dir, sanitizeFilename(string(EvtLLMCallComplete)+".jsonl"))
	data, err := os.ReadFile(fname)
	if err != nil {
		t.Fatalf("读取 JSONL 失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("JSONL 文件不应为空")
	}
}

func TestJSONLStoreSubscriber(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer store.Close()

	sub := NewJSONLStoreSubscriber(store)
	sub.OnEvent(Event{
		Type:      EvtStageStart,
		Timestamp: time.Now(),
		Module:    "workflow",
		Name:      "design",
	})

	fname := filepath.Join(dir, sanitizeFilename(string(EvtStageStart)+".jsonl"))
	data, err := os.ReadFile(fname)
	if err != nil {
		t.Fatalf("读取 JSONL 失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("subscriber 应写入事件")
	}
}

func TestEventFilterSubscriber(t *testing.T) {
	var received Event
	sub := NewEventFilterSubscriber(SubscriberFunc(func(e Event) {
		received = e
	}), EvtLLMCallComplete)

	sub.OnEvent(Event{Type: EvtLLMCallComplete, Module: "llm"})
	if received.Module != "llm" {
		t.Fatalf("应收到匹配类型的事件")
	}

	received = Event{}
	sub.OnEvent(Event{Type: EvtStageStart, Module: "workflow"})
	if received.Module != "" {
		t.Fatalf("不应收到未匹配类型的事件")
	}
}

func TestJSONLStoreRotateDaily(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}

	store.WriteEvent(Event{Type: EvtLLMCallComplete, Module: "llm"})
	if err := store.RotateDaily(); err != nil {
		t.Fatalf("轮转失败: %v", err)
	}
	// 轮转后写入新事件应重新创建文件
	store.WriteEvent(Event{Type: EvtLLMCallComplete, Module: "llm"})
	store.Close()
}

func TestJSONLStoreClosed(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONLStore(dir)
	store.Close()

	if err := store.WriteEvent(Event{Type: EvtLLMCallComplete}); err == nil {
		t.Fatalf("closed store 应返回错误")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello.jsonl", "hello.jsonl"},
		{"a/b/c.jsonl", "a_b_c.jsonl"},
		{"file:name.jsonl", "file:name.jsonl"},
		{"test@123", "test_123"},
	}
	for _, c := range cases {
		got := sanitizeFilename(c.in)
		if got != c.want {
			t.Fatalf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
