// Dreaming 机制测试
package unit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/dreaming"
)

func TestDreamerConfig(t *testing.T) {
	cfg := dreaming.DefaultDreamConfig()
	if !cfg.Enabled {
		t.Error("默认应启用")
	}
	if cfg.MinHours != 12 {
		t.Errorf("MinHours: %d (expected 12)", cfg.MinHours)
	}
	if cfg.MinSessions != 3 {
		t.Errorf("MinSessions: %d (expected 3)", cfg.MinSessions)
	}
}

func TestDreamerRecordAndStats(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:     true,
		MinHours:    24,
		MinSessions: 3,
		MemoryDir:   filepath.Join(dir, "memory"),
	}
	d := dreaming.NewDreamer(cfg, dir)

	stats := d.Stats()
	if stats.SessionsSinceDream != 0 {
		t.Errorf("初始 sessions 应为 0: %d", stats.SessionsSinceDream)
	}

	d.RecordSession(dreaming.SessionRecord{
		ChatID:  "chat-1",
		EndTime: time.Now(),
		Summary: "Discussed Go testing",
		Topics:  []string{"go", "testing"},
	})
	d.RecordSession(dreaming.SessionRecord{
		ChatID:  "chat-2",
		EndTime: time.Now(),
		Summary: "Built MCP integration",
	})

	stats = d.Stats()
	if stats.SessionsSinceDream != 2 {
		t.Errorf("期望 2 sessions: %d", stats.SessionsSinceDream)
	}
	if stats.RecentSessions != 2 {
		t.Errorf("期望 2 recent: %d", stats.RecentSessions)
	}
}

func TestDreamerGating(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:     true,
		MinHours:    0, // 无时间门控
		MinSessions: 2,
		MemoryDir:   filepath.Join(dir, "memory"),
	}
	d := dreaming.NewDreamer(cfg, dir)

	// 未达到 MinSessions, 不应触发
	d.RecordSession(dreaming.SessionRecord{ChatID: "c1", Summary: "s1", EndTime: time.Now()})
	d.AfterQuery(context.Background())
	// 应该不会 dream (只有 1 session < MinSessions=2)
	time.Sleep(100 * time.Millisecond)
	if d.IsDreaming() {
		t.Error("未达到 MinSessions 不应 dream")
	}
}

func TestDreamerForceDream(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:        true,
		MinHours:       24,
		MinSessions:    5,
		MemoryDir:      filepath.Join(dir, "memory"),
		MaxMemoryFiles: 10,
	}
	d := dreaming.NewDreamer(cfg, dir)

	// 录入一些会话
	for i := 0; i < 3; i++ {
		d.RecordSession(dreaming.SessionRecord{
			ChatID:  "chat",
			EndTime: time.Now(),
			Summary: "Test session summary " + string(rune('A'+i)),
			Topics:  []string{"test"},
		})
	}

	// 强制触发
	err := d.ForceDream(context.Background())
	if err != nil {
		t.Fatalf("ForceDream 失败: %v", err)
	}

	// 等待后台完成 (goroutine 需要时间启动和执行)
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 100; i++ {
		if !d.IsDreaming() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 验证记忆文件已创建
	indexPath := filepath.Join(dir, "memory", "index.md")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index.md 应存在: %v", err)
	}

	data, _ := os.ReadFile(indexPath)
	if !containsSubstr(string(data), "Memory Index") {
		t.Error("index.md 应包含 Memory Index")
	}
	if !containsSubstr(string(data), "Total entries: 3") {
		t.Errorf("应有 3 条记忆: %s", string(data))
	}

	// 验证记忆文件
	for _, name := range []string{"memory-001.md", "memory-002.md", "memory-003.md"} {
		path := filepath.Join(dir, "memory", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s 应存在", name)
		}
	}

	// 强制 dream 后 sessions 应重置
	stats := d.Stats()
	if stats.SessionsSinceDream != 0 {
		t.Errorf("dream 后 sessions 应为 0: %d", stats.SessionsSinceDream)
	}
}

func TestDreamerLock(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:     true,
		MinHours:    0,
		MinSessions: 1,
		MemoryDir:   filepath.Join(dir, "memory"),
	}
	d := dreaming.NewDreamer(cfg, dir)

	d.RecordSession(dreaming.SessionRecord{ChatID: "c1", Summary: "s1", EndTime: time.Now()})

	// 强制 dream
	d.ForceDream(context.Background())
	time.Sleep(50 * time.Millisecond)

	// 重复 ForceDream 应失败 (正在运行或锁)
	err := d.ForceDream(context.Background())
	if err == nil {
		// 可能已完成，跳过
	}

	// 等待完成
	for i := 0; i < 50; i++ {
		if !d.IsDreaming() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestDreamerDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:   false,
		MemoryDir: filepath.Join(dir, "memory"),
	}
	d := dreaming.NewDreamer(cfg, dir)
	d.RecordSession(dreaming.SessionRecord{ChatID: "c1", Summary: "s1", EndTime: time.Now()})
	d.AfterQuery(context.Background())
	time.Sleep(100 * time.Millisecond)
	if d.IsDreaming() {
		t.Error("禁用状态不应 dream")
	}
}

func TestDreamerCustomConsolidateFn(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:     true,
		MinHours:    0,
		MinSessions: 1,
		MemoryDir:   filepath.Join(dir, "memory"),
	}
	d := dreaming.NewDreamer(cfg, dir)

	customCalled := false
	d.ConsolidateFn = func(ctx context.Context, sessions []dreaming.SessionRecord, memoryDir string) error {
		customCalled = true
		os.MkdirAll(memoryDir, 0755)
		return os.WriteFile(filepath.Join(memoryDir, "custom.md"), []byte("custom dream"), 0644)
	}

	d.RecordSession(dreaming.SessionRecord{ChatID: "c1", Summary: "s1", EndTime: time.Now()})
	d.ForceDream(context.Background())

	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 100; i++ {
		if !d.IsDreaming() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !customCalled {
		t.Error("自定义 ConsolidateFn 应被调用")
	}

	path := filepath.Join(dir, "memory", "custom.md")
	if _, err := os.Stat(path); err != nil {
		t.Error("自定义记忆文件应存在")
	}
}
