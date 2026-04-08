// 配置热加载测试
package unit

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/hotreload"
)

func TestHotReloadWatcher_DetectsChange(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{"version": 1}`), 0644)

	w := hotreload.NewWatcher(configPath, 200*time.Millisecond)

	var changeCount atomic.Int32
	w.OnChange(func(path string) {
		changeCount.Add(1)
	})

	w.Start()
	defer w.Stop()

	// 等一个轮询周期确保初始化
	time.Sleep(300 * time.Millisecond)

	// 修改文件
	os.WriteFile(configPath, []byte(`{"version": 2}`), 0644)

	// 等待检测
	time.Sleep(500 * time.Millisecond)

	if changeCount.Load() != 1 {
		t.Errorf("期望 1 次变更通知, 实际 %d", changeCount.Load())
	}
}

func TestHotReloadWatcher_NoFalsePositive(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{"stable": true}`), 0644)

	w := hotreload.NewWatcher(configPath, 200*time.Millisecond)

	var changeCount atomic.Int32
	w.OnChange(func(path string) {
		changeCount.Add(1)
	})

	w.Start()
	defer w.Stop()

	// 不修改文件，等几个周期
	time.Sleep(600 * time.Millisecond)

	if changeCount.Load() != 0 {
		t.Errorf("未修改不应触发变更: %d", changeCount.Load())
	}
}

func TestHotReloadWatcher_ForceReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{}`), 0644)

	w := hotreload.NewWatcher(configPath, 5*time.Second) // 长间隔

	var called atomic.Int32
	w.OnChange(func(path string) {
		called.Add(1)
	})

	w.Start()
	defer w.Stop()

	w.ForceReload()
	if called.Load() != 1 {
		t.Errorf("ForceReload 应触发回调: %d", called.Load())
	}
}

func TestHotReloadMultiWatcher(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "a.json")
	path2 := filepath.Join(dir, "b.json")
	os.WriteFile(path1, []byte(`{"a":1}`), 0644)
	os.WriteFile(path2, []byte(`{"b":1}`), 0644)

	mw := hotreload.NewMultiWatcher([]string{path1, path2}, 200*time.Millisecond)

	var changes atomic.Int32
	mw.OnChange(func(path string) {
		changes.Add(1)
	})

	mw.Start()
	defer mw.Stop()

	time.Sleep(300 * time.Millisecond)

	// 修改两个文件
	os.WriteFile(path1, []byte(`{"a":2}`), 0644)
	os.WriteFile(path2, []byte(`{"b":2}`), 0644)

	time.Sleep(500 * time.Millisecond)

	if changes.Load() < 2 {
		t.Errorf("期望至少 2 次变更, 实际 %d", changes.Load())
	}
}
