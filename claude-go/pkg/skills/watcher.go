// watcher.go —— 13.6-P1 F4: skill 目录热刷新 (fsnotify watch, 增删改即时生效)。
//
// 规划锚点 (docforge planning-dsh-adopt F4): dsh 的 skillChangeDetector 用 chokidar
// watch 技能目录, 增删改即时生效。本仓 Register/Reload 齐备但此前全靠调用方手动触发
// (/skill reload 命令、reloadConfig、操作台按钮), pathsCache 也只在注册时失效。
// 这里补自动触发层: 监视注册表已扫描的技能目录, 事件去抖合并后调 Reload() ——
// 注册表语义零改动 (仍是全量重扫 + rank 覆盖 + pathsCache 失效 + injected 跨
// Reload 保留), 热刷新只是把"手动按"换成"文件变更自动按"。
//
// watch 覆盖: 每个扫描目录本体 (捕捉新建技能子目录) + 其一级子目录 (SKILL.md 所在
// 层 —— inotify 不递归, 只 watch 本体看不到 <子目录>/SKILL.md 的写入); 更深层级
// LoadFromDirs 本就不扫描, 无需覆盖。启动时目录为空则 LoadFromDirs 不登记
// (loaded>0 才登记), 该目录首个技能的创建不会触发热刷新 —— 已知边界, 手动
// reload/重启后纳入。
//
// 并发纪律: 事件循环是单 goroutine; Reload 幂等且注册表自带锁, 多触发无害
// (编辑器连发事件被去抖合并)。onReload 回调在 watcher goroutine 内同步执行,
// 回调方不得触碰无同步的共享状态 —— 清单渲染等 per-run 现读注册表的路径天然安全。
package skills

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultDebounce 默认去抖窗口: 编辑器保存常连发多事件 (write+rename+create),
// 合并为一次 Reload; 500ms 对"增删改即时生效"无感, 对风暴足够。
const defaultDebounce = 500 * time.Millisecond

// DirWatcher 技能目录热刷新监视器: fsnotify 监视注册表已扫描目录, 事件去抖后
// 自动 Reload。对应 TS: skillChangeDetector.ts。
type DirWatcher struct {
	reg      *Registry
	debounce time.Duration
	// onReload 每次 Reload 完成后的可选回调 (参数 = 本次加载总数)。在 watcher
	// goroutine 内同步执行: 不得触碰无同步的共享状态。
	onReload func(total int)

	fs        *fsnotify.Watcher
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	startErr  error
}

// NewDirWatcher 构建热刷新监视器 (Start 前可用 SetDebounce/SetOnReload 定制)。
func NewDirWatcher(reg *Registry) *DirWatcher {
	return &DirWatcher{
		reg:      reg,
		debounce: defaultDebounce,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetDebounce 覆盖去抖窗口 (须在 Start 前调用; 供测试收紧)。
func (dw *DirWatcher) SetDebounce(d time.Duration) {
	if d > 0 {
		dw.debounce = d
	}
}

// SetOnReload 注入 Reload 完成回调 (须在 Start 前调用; nil = 无回调)。
func (dw *DirWatcher) SetOnReload(fn func(total int)) {
	dw.onReload = fn
}

// Start 开始监视 (幂等; 生命周期一次性, Stop 后不可复用)。
func (dw *DirWatcher) Start() error {
	dw.startOnce.Do(func() {
		dw.startErr = dw.startOnce0()
		if dw.startErr != nil {
			close(dw.done) // 失败也无 goroutine, 让 Stop 不悬挂
		}
	})
	return dw.startErr
}

// startOnce0 实际装配 (仅由 startOnce 调用一次)。
func (dw *DirWatcher) startOnce0() error {
	// 有缓冲: 编辑器连发的突发事件不因消费端去抖等待而丢。
	w, err := fsnotify.NewBufferedWatcher(64)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	n := 0
	for _, d := range dw.reg.snapshotDirs() {
		if seen[d.Path] {
			continue
		}
		seen[d.Path] = true
		if err := w.Add(d.Path); err != nil {
			continue // 目录不存在: Reload 本就静默跳过, 事件层保持一致
		}
		n++
		// 一级子目录 (技能本体) 也要 watch: SKILL.md 的写入发生在子目录内。
		subs, err := os.ReadDir(d.Path)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			p := filepath.Join(d.Path, sub.Name())
			if !seen[p] {
				seen[p] = true
				if err := w.Add(p); err == nil {
					n++
				}
			}
		}
	}
	if n == 0 {
		// 没有可监视的目录: 不起 goroutine, 直接报错让调用方降级 (与未启用等价)。
		_ = w.Close()
		return errNoWatchedDirs
	}
	dw.fs = w
	go dw.loop()
	return nil
}

// errNoWatchedDirs 没有任何可监视的技能目录 (注册表未登记/目录全不存在)。
var errNoWatchedDirs = errors.New("skills: 没有可监视的技能目录, 热刷新未启用")

// loop 事件主循环: 非 Chmod 事件一律排定去抖 Reload; Create 的目录立即纳入
// watch (后续该目录内 SKILL.md 的写事件才可见)。
func (dw *DirWatcher) loop() {
	defer close(dw.done)
	defer dw.fs.Close()

	var timer *time.Timer
	var timerC <-chan time.Time
	schedule := func() {
		if timer == nil {
			timer = time.NewTimer(dw.debounce)
			timerC = timer.C
			return
		}
		// 已有待发 tick 时 Reset 存在立即触发的竞态 —— 至多把本应合并的多次
		// Reload 提前为多次幂等 Reload, 无正确性影响。
		timer.Reset(dw.debounce)
	}

	for {
		select {
		case <-dw.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case err, ok := <-dw.fs.Errors:
			if !ok {
				return
			}
			log.Printf("[Skills] 目录监视错误: %v", err)
		case ev, ok := <-dw.fs.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Chmod) {
				continue // 权限/时间戳变更不代表内容变化
			}
			// 新建技能子目录: 立即纳入 watch (inotify 不递归, SKILL.md 的写
			// 事件只有子目录自身被 watch 才可见)。
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = dw.fs.Add(ev.Name)
				}
			}
			schedule()
		case <-timerC:
			timer, timerC = nil, nil
			total := dw.reg.Reload()
			if dw.onReload != nil {
				dw.onReload(total)
			}
		}
	}
}

// Stop 停止监视并等待事件循环退出 (幂等; Start 前调用是零操作)。
func (dw *DirWatcher) Stop() {
	dw.stopOnce.Do(func() { close(dw.stop) })
	if dw.fs == nil {
		return // Start 从未成功: 无 goroutine 可等 (done 已在失败路径关闭)
	}
	<-dw.done
}

// snapshotDirs 已扫描目录快照 (watcher 装配用; 同包直读内部状态)。
func (r *Registry) snapshotDirs() []scanDir {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]scanDir, len(r.dirs))
	copy(out, r.dirs)
	return out
}
