package search

// quota.go —— 单源限流冷却与剩余配额快照。
//
// provider 返回 ErrRateLimited 后, 该源在 quotaPenalty 内不再被选为主源或
// 对冲源, 到点自动恢复; 其余错误交给 provider 自身的熔断/退避。剩余配额
// 只在 provider 实现 remainingReporter 时可见, 未知时报 -1。
import (
	"sync"
	"time"
)

// quotaPenalty 是触发限流后的冷却时间。
//
// provider 返回 ErrRateLimited 说明配额或速率已到顶; 冷却期内不再把它选为
// 主源/对冲源, 让其它源顶上。冷却结束自动恢复, 不需要手工重置。
const quotaPenalty = 60 * time.Second

// remainingReporter 是 provider 可选实现的配额查询接口。
//
// Provider 接口本身不含配额方法 (见 types.go); 愿意暴露剩余量的源实现
// 本接口即可, 未实现的源在 ProviderStatus 里报 -1 (未知)。
type remainingReporter interface {
	Remaining() int
}

// quotaTracker 记录限流冷却与配额剩余量的状态。
type quotaTracker struct {
	mu   sync.Mutex
	cool map[string]time.Time
}

// newQuotaTracker 创建空跟踪器。
func newQuotaTracker() *quotaTracker {
	return &quotaTracker{cool: make(map[string]time.Time)}
}

// available 判断 provider 是否已过冷却期。
func (q *quotaTracker) available(name string, now time.Time) bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	until, ok := q.cool[name]
	if !ok {
		return true
	}
	if !now.Before(until) {
		delete(q.cool, name)
		return true
	}
	return false
}

// recordError 按错误类别更新冷却状态。仅限流会触发冷却。
func (q *quotaTracker) recordError(name string, kind ErrorKind, now time.Time) {
	if q == nil || kind != ErrRateLimited {
		return
	}
	q.mu.Lock()
	q.cool[name] = now.Add(quotaPenalty)
	q.mu.Unlock()
}

// recordSuccess 清掉 provider 的冷却状态。
func (q *quotaTracker) recordSuccess(name string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	delete(q.cool, name)
	q.mu.Unlock()
}

// remaining 返回 provider 的剩余配额; 未知时返回 -1。
func (q *quotaTracker) remaining(p Provider) int {
	if p == nil {
		return -1
	}
	if r, ok := p.(remainingReporter); ok {
		return r.Remaining()
	}
	return -1
}

// cooling 返回 provider 是否处于限流冷却中。
func (q *quotaTracker) cooling(name string) bool {
	return !q.available(name, time.Now())
}
