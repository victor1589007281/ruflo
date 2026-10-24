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

// NewProgressReporter 构造进度上报器, 记录总步数与起始时刻。
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

// estimateETA 用已完成步的平均耗时线性外推剩余步数所需时间。
//
// 算法解读：
//
//	原理：平均每步耗时 = 已用时长 / 已完成步数, 再乘以剩余步数, 得到线性外推的剩余时长。
//	关键不变量：current == 0 时必须返回 0——否则这里就是一次除零, 调用方也据此跳过 ETA 展示。
//	代价与边界：current 在 Step 开头就自增, 于是「进行中的这一步」被算成已完成, 平均耗时偏小、ETA 偏乐观; 步长差异大的任务外推误差会被放大。
//	设计权衡：只用 O(1) 的均值外推, 不做滑动窗口或指数平滑; 注意 stepTs 被写入却从未被读取, 是留给将来做滑动窗口的残留字段。
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
