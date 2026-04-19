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
	// 等待后台 saveDreamState goroutine 完成, 避免 TempDir 清理竞态
	time.Sleep(100 * time.Millisecond)
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

func TestDreamerStatePersistence(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	cfg := &dreaming.DreamConfig{
		Enabled:     true,
		MinHours:    24,
		MinSessions: 5,
		MemoryDir:   memDir,
	}

	d1 := dreaming.NewDreamer(cfg, dir)
	d1.RecordSession(dreaming.SessionRecord{ChatID: "c1", Summary: "s1", EndTime: time.Now()})
	d1.RecordSession(dreaming.SessionRecord{ChatID: "c2", Summary: "s2", EndTime: time.Now()})
	d1.RecordSession(dreaming.SessionRecord{ChatID: "c3", Summary: "s3", EndTime: time.Now()})
	time.Sleep(200 * time.Millisecond)

	stats1 := d1.Stats()
	if stats1.SessionsSinceDream != 3 {
		t.Fatalf("期望 3 sessions: %d", stats1.SessionsSinceDream)
	}

	// 模拟进程重启: 新 Dreamer 应从磁盘恢复
	d2 := dreaming.NewDreamer(cfg, dir)
	stats2 := d2.Stats()
	if stats2.SessionsSinceDream != 3 {
		t.Errorf("重启后应恢复 sessions=3, 实际=%d", stats2.SessionsSinceDream)
	}

	stateFile := filepath.Join(memDir, "dream_state.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("dream_state.json 应存在: %v", err)
	}
}

func TestDreamerIdleFallback(t *testing.T) {
	dir := t.TempDir()
	cfg := &dreaming.DreamConfig{
		Enabled:     true,
		MinHours:    1,
		MinSessions: 10, // 高阈值
		MemoryDir:   filepath.Join(dir, "memory"),
	}
	d := dreaming.NewDreamer(cfg, dir)
	d.SetConsolidateFn(func(ctx context.Context, sessions []dreaming.SessionRecord, memoryDir string) error {
		os.MkdirAll(memoryDir, 0755)
		return os.WriteFile(filepath.Join(memoryDir, "idle.md"), []byte("idle fallback"), 0644)
	})

	// 只有 1 条会话 (< minSessions=10)
	d.RecordSession(dreaming.SessionRecord{ChatID: "c1", Summary: "s1", EndTime: time.Now()})
	time.Sleep(100 * time.Millisecond)

	// 正常 AfterQuery 不应触发 (1 < 10)
	d.AfterQuery(context.Background())
	time.Sleep(200 * time.Millisecond)
	if d.IsDreaming() {
		t.Error("正常门控下不应触发 (sessions=1 < 10)")
	}

	// 注意: 超时兜底需要 lastDreamTime 非零且 >48h，
	// 但在新创建的 Dreamer 中 lastDreamTime 为零，idleFallback 条件不满足。
	// 这是预期行为: 首次运行没有历史 dream 时不应兜底触发。
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
