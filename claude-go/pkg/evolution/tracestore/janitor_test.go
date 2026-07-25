package tracestore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// 回归：New 此前硬编码全采且 NewWithOptions 无生产调用方 ⇒ 采样是"有能力无开关"。
func TestNew_采样率读环境变量(t *testing.T) {
	for _, c := range []struct {
		env  string
		want float64
	}{
		{"", 1}, {"1", 1}, {"0.2", 0.2}, {"0", 0},
		{"abc", 1},       // 解析失败 → 回落全采, 不给运维制造惊喜
		{"1.5", 1},       // 越界 → 回落全采
		{"-0.1", 1},      // 越界 → 回落全采
		{"  0.5  ", 0.5}, // 两侧空白容错
	} {
		t.Run("env="+c.env, func(t *testing.T) {
			t.Setenv("CLAUDE_GO_TRACE_SAMPLE", c.env)
			s := New(statestore.NewMemStore())
			if s.opts.BodySampleRate != c.want {
				t.Errorf("BodySampleRate = %v, 期望 %v", s.opts.BodySampleRate, c.want)
			}
		})
	}
}

// 回归：SweepTraceFiles 此前全仓零调用方 ⇒ 等于没有 TTL，长跑实例只增不减。
func TestStartJanitor_删除过期trace文件(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "trace-old.jsonl")
	fresh := filepath.Join(dir, "trace-fresh.jsonl")
	other := filepath.Join(dir, "keepme.txt") // 非 trace-*.jsonl 不该被动
	for _, f := range []string{old, fresh, other} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 把 old 的 mtime 拨到 2 小时前
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartJanitor(ctx, dir, time.Hour, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(old); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("过期 trace 文件未被清理")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("未过期的 trace 文件被误删")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("非 trace 文件被误删")
	}
}

// ttl<=0 或 interval<=0 时不启动，保持"不配置就不清理"的既有语义。
func TestStartJanitor_未配置则不启动(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "trace-x.jsonl")
	_ = os.WriteFile(f, []byte("{}\n"), 0o644)
	past := time.Now().Add(-99 * time.Hour)
	_ = os.Chtimes(f, past, past)

	StartJanitor(context.Background(), dir, 0, time.Millisecond) // ttl=0
	StartJanitor(context.Background(), dir, time.Hour, 0)        // interval=0
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(f); err != nil {
		t.Error("未配置 TTL 时不应删除任何文件")
	}
}
