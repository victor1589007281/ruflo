// Package hotreload 实现配置文件热加载。
// 监控 JSON 配置文件变更，自动重新加载并应用到运行中的系统。
//
// 设计思路:
//   - 使用 polling 方式检测文件变更 (跨平台兼容)
//   - 文件 hash 对比避免无效重载
//   - debounce 防抖 (避免编辑器连续保存触发多次重载)
//   - 支持回调链 (多个模块注册 onChange 回调)
//
// 对应 TS: skillChangeDetector.ts 中的 chokidar watcher
package hotreload

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"sync"
	"time"
)

// Watcher 配置文件热加载监控器
type Watcher struct {
	path      string
	interval  time.Duration
	lastHash  string
	callbacks []func(path string)
	mu        sync.Mutex
	stopCh    chan struct{}
	running   bool
}

// NewWatcher 创建热加载监控器。
// path: 被监控的配置文件路径
// interval: 轮询间隔 (默认 5 秒)
func NewWatcher(path string, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Watcher{
		path:     path,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// OnChange 注册文件变更回调。
// 当文件内容发生变化时（基于 SHA-256 对比），回调被调用。
func (w *Watcher) OnChange(fn func(path string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callbacks = append(w.callbacks, fn)
}

// Start 开始监控 (非阻塞, 启动后台 goroutine)
func (w *Watcher) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.lastHash = w.computeHash()
	w.mu.Unlock()

	go w.pollLoop()
	log.Printf("[HotReload] 开始监控: %s (间隔 %v)", w.path, w.interval)
}

// Stop 停止监控
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		close(w.stopCh)
		w.running = false
		log.Printf("[HotReload] 停止监控: %s", w.path)
	}
}

// ForceReload 手动触发重载 (不检查 hash)
func (w *Watcher) ForceReload() {
	w.mu.Lock()
	callbacks := make([]func(string), len(w.callbacks))
	copy(callbacks, w.callbacks)
	w.lastHash = w.computeHash()
	w.mu.Unlock()

	log.Printf("[HotReload] 手动重载: %s", w.path)
	for _, cb := range callbacks {
		cb(w.path)
	}
}

// pollLoop 后台轮询检测文件变更
func (w *Watcher) pollLoop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.checkAndReload()
		}
	}
}

// checkAndReload 检查文件是否变更，如果变更则触发回调
func (w *Watcher) checkAndReload() {
	newHash := w.computeHash()

	w.mu.Lock()
	if newHash == w.lastHash {
		w.mu.Unlock()
		return
	}
	w.lastHash = newHash
	callbacks := make([]func(string), len(w.callbacks))
	copy(callbacks, w.callbacks)
	w.mu.Unlock()

	log.Printf("[HotReload] 检测到变更: %s", w.path)
	for _, cb := range callbacks {
		cb(w.path)
	}
}

// computeHash 计算文件的 SHA-256 hash
func (w *Watcher) computeHash() string {
	data, err := os.ReadFile(w.path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// MultiWatcher 监控多个文件/目录
type MultiWatcher struct {
	watchers []*Watcher
}

// NewMultiWatcher 创建多文件监控器
func NewMultiWatcher(paths []string, interval time.Duration) *MultiWatcher {
	mw := &MultiWatcher{}
	for _, p := range paths {
		mw.watchers = append(mw.watchers, NewWatcher(p, interval))
	}
	return mw
}

// OnChange 注册变更回调到所有子监控器
func (mw *MultiWatcher) OnChange(fn func(path string)) {
	for _, w := range mw.watchers {
		w.OnChange(fn)
	}
}

// Start 启动所有子监控器
func (mw *MultiWatcher) Start() {
	for _, w := range mw.watchers {
		w.Start()
	}
}

// Stop 停止所有子监控器
func (mw *MultiWatcher) Stop() {
	for _, w := range mw.watchers {
		w.Stop()
	}
}
