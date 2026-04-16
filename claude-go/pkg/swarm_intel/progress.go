package swarm_intel

import (
	"fmt"
	"time"
)

// ProgressReporter 管理多阶段任务的进度推送和 ETA 估算。
type ProgressReporter struct {
	notify  NotifyFunc
	chatID  string
	total   int
	current int
	started time.Time
	stepTs  []time.Time
}

func NewProgressReporter(notify NotifyFunc, chatID string, totalSteps int) *ProgressReporter {
	return &ProgressReporter{
		notify:  notify,
		chatID:  chatID,
		total:   totalSteps,
		started: time.Now(),
	}
}

// Step 推进一步并推送进度。
func (pr *ProgressReporter) Step(name string) {
	pr.current++
	pr.stepTs = append(pr.stepTs, time.Now())

	eta := pr.estimateETA()
	etaStr := ""
	if eta > 0 && pr.current < pr.total {
		etaStr = fmt.Sprintf(" (预计还需 ~%.0fs)", eta.Seconds())
	}

	pr.notify(pr.chatID, fmt.Sprintf("📋 [%d/%d] %s%s", pr.current, pr.total, name, etaStr))
}

// BranchDone 并行分支完成通知。
func (pr *ProgressReporter) BranchDone(branchName string, latency time.Duration) {
	pr.notify(pr.chatID, fmt.Sprintf("  ✓ %s 完成 (%.1fs)", branchName, latency.Seconds()))
}

// Intermediate 推送中间结果。
func (pr *ProgressReporter) Intermediate(msg string) {
	pr.notify(pr.chatID, fmt.Sprintf("  💡 %s", msg))
}

// Retry 推送重试通知。
func (pr *ProgressReporter) Retry(attempt, max int, delay time.Duration) {
	pr.notify(pr.chatID, fmt.Sprintf("  🔄 重试 %d/%d (%.0fs 后)...", attempt, max, delay.Seconds()))
}

// Done 推送完成通知。
func (pr *ProgressReporter) Done(summary string) {
	elapsed := time.Since(pr.started)
	pr.notify(pr.chatID, fmt.Sprintf("✅ 完成 (%.1fs) — %s", elapsed.Seconds(), summary))
}

// Error 推送错误通知。
func (pr *ProgressReporter) Error(err error) {
	elapsed := time.Since(pr.started)
	pr.notify(pr.chatID, fmt.Sprintf("❌ 失败 (%.1fs): %v", elapsed.Seconds(), err))
}

func (pr *ProgressReporter) estimateETA() time.Duration {
	if pr.current == 0 {
		return 0
	}
	elapsed := time.Since(pr.started)
	avgPerStep := elapsed / time.Duration(pr.current)
	remaining := pr.total - pr.current
	return avgPerStep * time.Duration(remaining)
}

// Elapsed 返回已消耗时间。
func (pr *ProgressReporter) Elapsed() time.Duration {
	return time.Since(pr.started)
}
