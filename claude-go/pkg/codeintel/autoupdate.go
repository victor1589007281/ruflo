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

func (a *AutoUpdater) statePath() string {
	return filepath.Join(a.repoPath, ".claude-code-intel", "autoupdate.json")
}

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
