// autoupdate.go — 代码智能索引自动更新维护。
//
// 能力:
//   1. 基于 git diff --name-only 检测文件变更
//   2. 变更文件数超过阈值时触发增量重建
//   3. 支持静默时段（避免工作时间干扰）
//   4. 状态持久化到 .claude-code-intel/autoupdate.json
//   5. 与 settings.CodeIntel.AutoUpdate 配置联动
package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// AutoUpdater 自动更新器
// ============================================================================

// AutoUpdater 代码智能索引自动更新器。
type AutoUpdater struct {
	repoPath   string
	interval   time.Duration
	threshold  int
	quietStart string
	quietEnd   string

	mu        sync.Mutex
	running   bool
	stopCh    chan struct{}
	lastCheck time.Time

	metrics *MetricsCollector
}

// AutoUpdateState 持久化状态。
type AutoUpdateState struct {
	LastCheckTime  time.Time `json:"last_check_time"`
	LastUpdateTime time.Time `json:"last_update_time,omitempty"`
	ChangedFiles   int       `json:"changed_files"`
	TotalChecks    int64     `json:"total_checks"`
	TotalUpdates   int64     `json:"total_updates"`
	LastError      string    `json:"last_error,omitempty"`
}

// NewAutoUpdater 创建自动更新器。
func NewAutoUpdater(repoPath string, interval time.Duration, threshold int) *AutoUpdater {
	return &AutoUpdater{
		repoPath:  repoPath,
		interval:  interval,
		threshold: threshold,
		stopCh:    make(chan struct{}),
		metrics:   NewMetricsCollector(repoPath),
	}
}

// NewAutoUpdaterFromSettings 从配置创建。
func NewAutoUpdaterFromSettings(repoPath string, cfg interface{}) *AutoUpdater {
	// 使用默认值
	interval := 5 * time.Minute
	threshold := 5

	// 解析配置（简单类型断言）
	if m, ok := cfg.(map[string]interface{}); ok {
		if v, ok := m["intervalSec"].(float64); ok && v > 0 {
			interval = time.Duration(v) * time.Second
		}
		if v, ok := m["fileThreshold"].(float64); ok && v > 0 {
			threshold = int(v)
		}
	}

	return NewAutoUpdater(repoPath, interval, threshold)
}

// SetQuietHours 设置静默时段（"HH:MM" 格式）。
func (a *AutoUpdater) SetQuietHours(start, end string) {
	a.quietStart = start
	a.quietEnd = end
}

// SetMetricsCollector 注入外部 metrics（可选）。
func (a *AutoUpdater) SetMetricsCollector(m *MetricsCollector) {
	a.metrics = m
}

// Start 启动后台轮询。
func (a *AutoUpdater) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return
	}
	a.running = true
	go a.loop()
}

// Stop 停止后台轮询。
func (a *AutoUpdater) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return
	}
	a.running = false
	close(a.stopCh)
}

// CheckNow 立即执行一次检测（不等待轮询）。
func (a *AutoUpdater) CheckNow() (*AutoUpdateResult, error) {
	return a.checkAndRebuild()
}

// IsRunning 检查是否正在运行。
func (a *AutoUpdater) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// ============================================================================
// 内部轮询循环
// ============================================================================

// loop 后台轮询主循环: 启动时先立即检测一次, 之后按 interval 周期触发, 直到 stopCh 关闭。
//
// 算法解读：
//
//	原理：ticker 按 interval 触发 checkAndRebuild; 启动时先跑一次 (不等第一个 tick),
//	      然后进入 select 等待 ticker 或 stopCh。
//	关键不变量：stopCh 关闭必须能让 loop 返回 —— 否则 Stop() 之后 goroutine 泄漏,
//	      且 running 标志与实际状态不符 (再 Start 会被误判为"已在运行")。
//	代价与边界：单次检测耗时若超过 interval, ticker 会丢拍 (不堆积), 实际周期被拉长。
//	设计权衡：检测错误一律静默 (源码注释"静默记录") —— 自动更新是尽力而为的后台维护,
//	      把错误打到用户终端只会制造噪音。
func (a *AutoUpdater) loop() {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	// 立即执行一次
	if _, err := a.checkAndRebuild(); err != nil {
		// 静默记录
	}

	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			if _, err := a.checkAndRebuild(); err != nil {
				// 静默记录
			}
		}
	}
}

// ============================================================================
// 检测与重建
// ============================================================================

// AutoUpdateResult 单次检测结果。
type AutoUpdateResult struct {
	Checked        bool      `json:"checked"`
	Triggered      bool      `json:"triggered"`
	ChangedFiles   []string  `json:"changed_files,omitempty"`
	RebuildSuccess bool      `json:"rebuild_success,omitempty"`
	DurationMs     int64     `json:"duration_ms"`
	Error          string    `json:"error,omitempty"`
}

// checkAndRebuild 执行一次"检测 → 阈值判断 → 增量重建 → 落状态/上报指标"的完整流程。
//
// 算法解读：
//
//	原理：静默时段直接返回; 否则 git diff 列出变更文件, 变更数达到阈值才触发增量重建,
//	      最后把结果写进状态文件并上报指标。
//	关键不变量：**未达阈值绝不重建** —— 阈值 (默认 5) 是"改动够多才值得付重建代价"的成本闸,
//	      不是正确性闸; 重建失败必须写进 state.LastError, 否则事后无法归因"为什么索引没更新"。
//	代价与边界：每次检测都 fork 一次 git 子进程; 重建并行跑 GitNexus 与 Graphify 两个索引器。
//	设计权衡：用"变更文件数"而非"变更行数/时间间隔"做触发判据 —— 行数要读 diff 全文代价高,
//	      文件数一次 --name-only 就够。
func (a *AutoUpdater) checkAndRebuild() (*AutoUpdateResult, error) {
	start := time.Now()
	result := &AutoUpdateResult{Checked: true}

	// 1. 静默时段检查
	if a.inQuietHours() {
		return result, nil
	}

	// 2. git diff 检测
	changed, err := a.detectChangedFiles()
	if err != nil {
		result.Error = err.Error()
		a.saveState(0, err.Error())
		if a.metrics != nil {
			a.metrics.RecordAutoUpdateCheck(0)
		}
		return result, err
	}

	result.ChangedFiles = changed
	changedCount := len(changed)

	if a.metrics != nil {
		a.metrics.RecordAutoUpdateCheck(changedCount)
	}

	// 3. 阈值判断
	if changedCount < a.threshold {
		a.saveState(changedCount, "")
		return result, nil
	}

	// 4. 触发重建
	result.Triggered = true
	a.lastCheck = time.Now()

	success, err := a.rebuild()
	result.RebuildSuccess = success
	result.DurationMs = time.Since(start).Milliseconds()

	if err != nil {
		result.Error = err.Error()
		a.saveState(changedCount, err.Error())
	} else {
		a.saveState(changedCount, "")
	}

	if a.metrics != nil {
		a.metrics.RecordAutoUpdateTrigger(success, result.DurationMs)
	}

	return result, err
}

// detectChangedFiles 用 git diff 列出变更文件 (工作区优先, 失败回退到最近一次提交)。
//
// 算法解读：
//
//	原理：先 `git diff --name-only HEAD` (工作区未提交变更), 失败再退到 `HEAD~1 HEAD`
//	      (已提交但未重新索引的变更)。
//	关键不变量：两次都失败才返回错误; 空行必须剔除, 否则会凭空多算一个"变更文件"。
//	代价与边界：只统计**文件级**变更 (同文件改 1 行与改 1000 行等价);
//	      未跟踪的新文件 (git status 的 ??) 不在 diff 输出里, 会漏检。
//	设计权衡：刻意不跑 `git status --porcelain` (要额外解析状态码), 接受漏掉未跟踪文件。
func (a *AutoUpdater) detectChangedFiles() ([]string, error) {
	// 优先使用 git diff --name-only HEAD
	cmd := exec.Command("git", "diff", "--name-only", "HEAD")
	cmd.Dir = a.repoPath
	out, err := cmd.Output()
	if err != nil {
		// 尝试 git diff --name-only HEAD~1（已提交但未索引的变更）
		cmd = exec.Command("git", "diff", "--name-only", "HEAD~1", "HEAD")
		cmd.Dir = a.repoPath
		out, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git diff failed: %w", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var files []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// rebuild 并行重建两份索引 (GitNexus 全量 analyze + Graphify 增量 update)。
//
// 算法解读：
//
//	原理：两个索引器各起一个 goroutine, 用 WaitGroup 等两边都结束。
//	关键不变量：**只有两边都失败才算失败** —— 一个索引器挂掉时另一个仍能提供可用的图,
//	      故 gnErr 与 gfErr 同时非 nil 才返回错误。
//	代价与边界：两个子进程并行, 峰值 CPU/IO 翻倍; 成功判定不看输出内容, 只信退出状态。
//	设计权衡：宁可"半个索引"也不要整轮失败 —— 增量更新下次还会再跑, 而整轮失败会让调用方
//	      以为索引不可用。
func (a *AutoUpdater) rebuild() (bool, error) {
	// 并行执行 GitNexus analyze + Graphify update
	gn := NewGitNexus(a.repoPath)
	gf := NewGraphify(a.repoPath)

	var gnErr, gfErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, gnErr = gn.Analyze()
	}()
	go func() {
		defer wg.Done()
		_, gfErr = gf.Update(true)
	}()
	wg.Wait()

	if gnErr != nil && gfErr != nil {
		return false, fmt.Errorf("gitnexus: %v; graphify: %v", gnErr, gfErr)
	}
	return true, nil
}

// ============================================================================
// 静默时段
// ============================================================================

// inQuietHours 判断当前是否处于静默时段 (start/end 为 "HH:MM")。
//
// 算法解读：
//
//	原理：把当前时刻与起止时刻都归一化到同一天的 00:00 基准再比较; start > end 视为跨午夜区间。
//	关键不变量：跨午夜必须用 `now > start || now < end` —— 用 && 会让 22:00-06:00 这类区间恒为假,
//	      静默时段形同虚设。
//	代价与边界：起止未配置或解析失败一律返回 false (视为不静默); 用 time.Local,
//	      跨时区部署时需注意服务器时区。
//	设计权衡：只比 HH:MM, 忽略秒与日期, 够用且免去跨日边界处理。
func (a *AutoUpdater) inQuietHours() bool {
	if a.quietStart == "" || a.quietEnd == "" {
		return false
	}
	now := time.Now()
	start, err1 := time.Parse("15:04", a.quietStart)
	end, err2 := time.Parse("15:04", a.quietEnd)
	if err1 != nil || err2 != nil {
		return false
	}

	nowTime := time.Date(0, 1, 1, now.Hour(), now.Minute(), 0, 0, time.Local)
	startTime := time.Date(0, 1, 1, start.Hour(), start.Minute(), 0, 0, time.Local)
	endTime := time.Date(0, 1, 1, end.Hour(), end.Minute(), 0, 0, time.Local)

	if startTime.Before(endTime) {
		return nowTime.After(startTime) && nowTime.Before(endTime)
	}
	// 跨午夜
	return nowTime.After(startTime) || nowTime.Before(endTime)
}

// ============================================================================
// 状态持久化
// ============================================================================

// statePath 返回状态文件的固定路径 (<repo>/.claude-code-intel/autoupdate.json)。
func (a *AutoUpdater) statePath() string {
	return filepath.Join(a.repoPath, ".claude-code-intel", "autoupdate.json")
}

// saveState 落盘一次检测的结果与累计计数; 读不到旧状态时按"首次检查"初始化。
func (a *AutoUpdater) saveState(changedFiles int, lastErr string) {
	state := AutoUpdateState{
		LastCheckTime:  time.Now(),
		LastUpdateTime: a.lastCheck,
		ChangedFiles:   changedFiles,
		LastError:      lastErr,
	}

	// 加载已有状态以累加计数
	if old, err := a.loadState(); err == nil {
		state.TotalChecks = old.TotalChecks + 1
		state.TotalUpdates = old.TotalUpdates
		// 只有达到阈值的那次才算一次"更新", 与 checkAndRebuild 的触发条件保持一致。
		if changedFiles >= a.threshold {
			state.TotalUpdates++
		}
	} else {
		state.TotalChecks = 1
	}

	path := a.statePath()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(path, data, 0644)
}

// LoadState 加载自动更新状态。
func (a *AutoUpdater) LoadState() (*AutoUpdateState, error) {
	return a.loadState()
}

// loadState 读取持久化状态 (文件缺失或内容损坏均返回错误)。
func (a *AutoUpdater) loadState() (*AutoUpdateState, error) {
	data, err := os.ReadFile(a.statePath())
	if err != nil {
		return nil, err
	}
	var state AutoUpdateState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}
