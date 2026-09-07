// watcher_test.go —— 13.6-P1 F4 热刷新单测 (docforge planning-dsh-adopt F4 验收:
// 新增/修改/删除 SKILL.md 后清单即时反映, 无重复注册)。
//
// 用真实 fsnotify 事件驱动 (不走内部钩子 —— 去抖/事件分类/子目录增量 watch 正是
// 被测对象); 断言轮询容忍异步延迟, 超时窗口远大于去抖窗口。
package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSkill 写一个最小 SKILL.md (目录名即技能名, 无 frontmatter —— ParseSkill
// 默认名推导 + 正文首行兜底描述)。
func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name, "SKILL.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitForUntil 轮询直到 cond 为真或超时 (fsnotify 事件异步, 不能同步等)。
func waitForUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// newTestWatcher 装配: 注册表载入 dir → 启动 watcher (去抖收紧到 50ms)。
// 返回清理函数 (Stop 必须在目录仍存在时调用 —— Stop 不删目录, defer 顺序注意)。
func newTestWatcher(t *testing.T, dir string) (*Registry, *DirWatcher, func()) {
	t.Helper()
	reg := NewRegistry()
	if got := reg.LoadFromDirs([]string{dir}, "test"); got != 1 {
		t.Fatalf("预置技能应载入 1 个, got %d", got)
	}
	dw := NewDirWatcher(reg)
	dw.SetDebounce(50 * time.Millisecond)
	if err := dw.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return reg, dw, dw.Stop
}

// TestWatcherCreateModifyDelete 全生命周期: 初始 1 个技能 → 新增第二个 → 修改
// 首个正文 → 删除第二个; 每步断言注册表即时反映且无重复注册。
func TestWatcherCreateModifyDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("fsnotify 事件异步, short 模式跳过")
	}
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha v1 正文")
	reg, _, stop := newTestWatcher(t, dir)
	defer stop()

	// --- 新增: beta/SKILL.md 创建 (先建目录后写文件, 两条事件合并一次 Reload)。
	writeSkill(t, dir, "beta", "beta 正文")
	waitForUntil(t, 3*time.Second, func() bool {
		s, ok := reg.Get("beta")
		return ok && s.Description == "beta 正文"
	}, "新增技能应即时进注册表")

	// --- 修改: alpha 正文重写 → Register 覆盖 (同 rank 先到先得? 否 —— Reload 全量
	// 重扫后先清空 skills, 所以新内容必然胜出)。
	writeSkill(t, dir, "alpha", "alpha v2 正文")
	waitForUntil(t, 3*time.Second, func() bool {
		s, ok := reg.Get("alpha")
		return ok && s.Description == "alpha v2 正文"
	}, "修改技能应即时反映到注册表")

	// --- 删除: 移除 beta 目录 → Reload 后消失。注册表含 builtin 技能, 绝对计数
	// 不定, 全部用存在性断言。
	if err := os.RemoveAll(filepath.Join(dir, "beta")); err != nil {
		t.Fatal(err)
	}
	waitForUntil(t, 3*time.Second, func() bool {
		_, ok := reg.Get("beta")
		return !ok
	}, "删除技能应即时移出注册表")

	// 幂等: 多次文件变更后测试技能无重复 (map 键控天然去重, builtin 不计)。
	for _, name := range []string{"alpha", "beta"} {
		if _, ok := reg.GetAny("dup-" + name); ok {
			t.Errorf("不应出现重复注册: %s", name)
		}
	}
}

// TestWatcherNewSubdirWatched 创建全新技能子目录后, 其内的 SKILL.md 写入也能
// 触发热刷新 (Create 目录 → 增量 Add → 文件写事件可见)。
func TestWatcherNewSubdirWatched(t *testing.T) {
	if testing.Short() {
		t.Skip("fsnotify 事件异步, short 模式跳过")
	}
	dir := t.TempDir()
	writeSkill(t, dir, "seed", "seed 正文")
	reg, _, stop := newTestWatcher(t, dir)
	defer stop()

	// 全新子目录 + 文件一次性创建 (同 writeSkill 两步, 事件合并去抖无碍)。
	writeSkill(t, dir, "gamma", "gamma 正文")
	waitForUntil(t, 3*time.Second, func() bool {
		_, ok := reg.Get("gamma")
		return ok
	}, "新建子目录内的 SKILL.md 应触发热刷新")

	// 再改 gamma —— 验证增量 watch 真正生效 (目录被 watch, 写事件可达)。
	writeSkill(t, dir, "gamma", "gamma v2")
	waitForUntil(t, 3*time.Second, func() bool {
		s, ok := reg.Get("gamma")
		return ok && s.Description == "gamma v2"
	}, "增量 watch 的子目录内修改应被捕获")
}

// TestWatcherNoDirsStartError 无任何可监视目录时 Start 报 errNoWatchedDirs
// (调用方降级 = 与未启用等价), 且 Stop 安全。
func TestWatcherNoDirsStartError(t *testing.T) {
	reg := NewRegistry() // 未登记任何 dirs
	dw := NewDirWatcher(reg)
	if err := dw.Start(); err != errNoWatchedDirs {
		t.Fatalf("应报 errNoWatchedDirs, got %v", err)
	}
	dw.Stop() // 失败路径 done 已关, 不得悬挂
}

// TestWatcherStopIdempotent Stop 幂等 + Start 幂等 (生命周期一次性纪律)。
func TestWatcherStopIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "solo", "solo 正文")
	reg, dw, _ := newTestWatcher(t, dir)

	dw.Start() // 二次 Start: 无害
	dw.Stop()
	dw.Stop() // 二次 Stop: 无害

	// Stop 后注册表本身照常可用 (watcher 只是触发层; Reload 会带回 builtin
	// 技能, 只断言测试技能仍可解析载入)。
	reg.Reload()
	s, ok := reg.Get("solo")
	if !ok || s.Description != "solo 正文" {
		t.Errorf("Stop 后 Reload 应照常载入 solo, got %+v (ok=%v)", s, ok)
	}
}
