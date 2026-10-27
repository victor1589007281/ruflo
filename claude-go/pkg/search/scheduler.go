package search

// scheduler.go —— 单次检索的源调度 (主源 + 对冲)。
//
// 调度上限是 2 个并发源: 主源 t=0 启动, 第二源延迟 HedgeDelay 启动; 首源
// 有结果后再等最多 250ms 收第二源, 避免慢源把 p95 拖到整个搜索超时。每个源
// 都有独立超时与 panic 隔离, 一个卡死/崩溃的 provider 只产生 degraded 标记,
// 不影响其它源。
import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// sourceResult 是单个源的执行结果。
type sourceResult struct {
	name    string
	results []Result
	err     error
	elapsed time.Duration
	cost    float64
}

// scheduleSearch 执行主源 + 对冲源的调度。
//
// 约束来自设计文档 §5.2/§9.3 与任务书:
//   - 最多 2 个并发源;
//   - 主源 t=0 启动, 对冲源 t=cfg.HedgeDelay 启动;
//   - 单源超时 = min(cfg.SearchTimeout, 距总 deadline 的剩余时间);
//   - 总 deadline = req.Deadline, 未设置时 now+cfg.SearchTimeout。
//
// 返回每个源的结局。父 context 被调用方取消时, 已完成的源仍会返回,
// 方便上层决定是降级还是直接返回取消错误。
func (m *Manager) scheduleSearch(ctx context.Context, req SearchRequest, candidates []Provider) []sourceResult {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) > maxSourceConcurrency {
		candidates = candidates[:maxSourceConcurrency]
	}

	deadline := req.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(m.cfg.SearchTimeout)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ch := make(chan sourceResult, len(candidates))
	var wg sync.WaitGroup

	for i, p := range candidates {
		delay := time.Duration(0)
		if i > 0 {
			delay = m.cfg.HedgeDelay
		}
		wg.Add(1)
		go func(p Provider, delay time.Duration) {
			defer wg.Done()
			if delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					ch <- sourceResult{name: p.Name(), err: ctx.Err(), elapsed: delay}
					return
				case <-timer.C:
				}
			}
			ch <- m.callSource(ctx, p, req)
		}(p, delay)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	out := make([]sourceResult, 0, len(candidates))
	// gatherWindow 是首源有结果后, 继续等待第二源的时间窗。
	//
	// 目的是拿到 2 源融合的召回, 但不能为了等一个慢源把 p95 拖到搜索超时:
	// 首源有结果后最多再等 250ms, 到点立即返回并取消其余请求。
	//
	// 触发条件限定为"主源已落定"(primaryDone): 若快的是对冲源而主源仍在途,
	// 立即返回会丢掉偏好最高的源 —— 新闻意图下 bing_rss 400ms 返回、parallel
	// 冷启动 2s, 早退会让每次检索都退化成 bing 单源且门禁不过。此时必须等
	// 主源出结果或单源超时 (有硬上限, 不会无限等)。
	const gatherWindow = 250 * time.Millisecond
	var gatherC <-chan time.Time
	primary := candidates[0].Name()
	primaryDone := false

	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
			if r.name == primary {
				primaryDone = true
			}
			if len(out) == len(candidates) {
				return out
			}
			if r.err == nil && len(r.results) > 0 && gatherC == nil && primaryDone {
				gatherC = time.NewTimer(gatherWindow).C
			}
		case <-gatherC:
			return out
		case <-ctx.Done():
			// 超时后继续收完 ch: 每个 source 都有独立硬超时, 关闭是即时的;
			// 直接返回会把"已超时"的源漏记成调度器取消, 丢失 degraded 归因。
			for r := range ch {
				out = append(out, r)
			}
			return out
		}
	}
}

// callSource 调用单个 provider, 并把单源超时隔离在它自己的 context 里。
//
// provider 的实现可能忽略 context; 因此调用放在独立 goroutine 中,
// 由 select 在 cctx.Done() 时先返回, 保证一个卡死的源不会拖住整次检索。
func (m *Manager) callSource(ctx context.Context, p Provider, req SearchRequest) sourceResult {
	start := time.Now()
	name := p.Name()

	timeout := m.cfg.SearchTimeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			timeout = rem
		}
	}
	if timeout <= 0 {
		return sourceResult{name: name, err: Wrap(name, ErrTimeout, context.DeadlineExceeded)}
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req.Deadline = time.Now().Add(timeout)

	type callOut struct {
		results []Result
		err     error
		cost    float64
	}
	done := make(chan callOut, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- callOut{err: Wrap(name, ErrUnavailable, errPanic{r})}
			}
		}()
		results, cost, err := m.callProviderWithBudget(cctx, p, req)
		done <- callOut{results: results, cost: cost, err: err}
	}()

	var out callOut
	select {
	case out = <-done:
	case <-cctx.Done():
		out.err = cctx.Err()
	}

	elapsed := time.Since(start)
	result := sourceResult{name: name, elapsed: elapsed}

	m.metrics.Observe("search_latency_seconds", elapsed.Seconds(), map[string]string{
		"provider": name,
		"tier":     p.Tier().String(),
	})

	if out.err != nil {
		kind := classifyError(out.err)
		if isSharedBudgetError(out.err) {
			m.metrics.Inc("shared_budget_errors_total", map[string]string{
				"provider": name, "error_class": string(kind),
			})
		} else {
			m.quota.recordError(name, kind, time.Now())
			m.metrics.Inc("provider_errors_total", map[string]string{
				"provider":    name,
				"error_class": string(kind),
			})
		}
		result.err = out.err
		return result
	}

	// provider 若未填 Rank/Provider, 在这里补齐, 保证 RRF 有确定名次。
	results := make([]Result, 0, len(out.results))
	for i, r := range out.results {
		if r.Provider == "" {
			r.Provider = name
		}
		if r.Rank <= 0 {
			r.Rank = i + 1
		}
		results = append(results, r)
	}
	m.quota.recordSuccess(name)
	result.results = results
	result.cost = out.cost
	return result
}

// classifyError 把 provider 错误归一为四类。
//
// context deadline 与 net.Error.Timeout 都算 timeout; 其余走 *Error 的
// Kind; 无法识别时按 unavailable。
func classifyError(err error) ErrorKind {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	return KindOf(err)
}

// degradedFor 把单源失败转成 degraded 标记。
//
// 超时与限流有独立标签, 其它统一 provider_error; 都带 provider 名,
// 便于日志定位且不影响其它源。
func degradedFor(name string, err error) string {
	switch classifyError(err) {
	case ErrTimeout:
		return "provider_timeout:" + name
	case ErrRateLimited:
		return "quota_exhausted:" + name
	default:
		return "provider_error:" + name
	}
}

// errPanic 包装 provider 的 panic 值。
type errPanic struct{ v any }

// Error 实现 error 接口。
func (e errPanic) Error() string {
	return "provider panicked"
}
