package swarm_intel

import (
	"context"
	"sync"
	"time"
)

// FanOutConfig 并行 LLM 调用配置。
type FanOutConfig struct {
	MaxConcurrency   int
	PerBranchTimeout time.Duration
	TotalTimeout     time.Duration
}

func DefaultFanOutConfig() FanOutConfig {
	return FanOutConfig{
		MaxConcurrency:   4,
		PerBranchTimeout: 3 * time.Minute,
		TotalTimeout:     5 * time.Minute,
	}
}

// BranchFunc 单分支执行函数。
type BranchFunc func(ctx context.Context) (string, error)

// BranchResult 单分支执行结果。
type BranchResult struct {
	ID      string
	Value   string
	Error   error
	Latency time.Duration
}

// FanOutCollect 并行执行多个分支并收集所有结果。
// 使用 semaphore 控制最大并发数, 每个分支有独立超时。
func FanOutCollect(ctx context.Context, cfg FanOutConfig, branches map[string]BranchFunc) []BranchResult {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}

	totalCtx, totalCancel := context.WithTimeout(ctx, cfg.TotalTimeout)
	defer totalCancel()

	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	var results []BranchResult

	for id, fn := range branches {
		wg.Add(1)
		go func(branchID string, branchFn BranchFunc) {
			defer wg.Done()

			// 获取并发槽
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-totalCtx.Done():
				mu.Lock()
				results = append(results, BranchResult{
					ID:    branchID,
					Error: totalCtx.Err(),
				})
				mu.Unlock()
				return
			}

			branchCtx, branchCancel := context.WithTimeout(totalCtx, cfg.PerBranchTimeout)
			defer branchCancel()

			start := time.Now()
			val, err := branchFn(branchCtx)
			latency := time.Since(start)

			mu.Lock()
			results = append(results, BranchResult{
				ID:      branchID,
				Value:   val,
				Error:   err,
				Latency: latency,
			})
			mu.Unlock()
		}(id, fn)
	}

	wg.Wait()
	return results
}

// FanOutFirstN 并行执行，返回前 N 个成功结果（用于 best-of-N 场景）。
func FanOutFirstN(ctx context.Context, cfg FanOutConfig, branches map[string]BranchFunc, n int) []BranchResult {
	totalCtx, totalCancel := context.WithTimeout(ctx, cfg.TotalTimeout)
	defer totalCancel()

	sem := make(chan struct{}, cfg.MaxConcurrency)
	resultCh := make(chan BranchResult, len(branches))

	for id, fn := range branches {
		go func(branchID string, branchFn BranchFunc) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-totalCtx.Done():
				resultCh <- BranchResult{ID: branchID, Error: totalCtx.Err()}
				return
			}

			branchCtx, branchCancel := context.WithTimeout(totalCtx, cfg.PerBranchTimeout)
			defer branchCancel()

			start := time.Now()
			val, err := branchFn(branchCtx)
			resultCh <- BranchResult{
				ID: branchID, Value: val, Error: err, Latency: time.Since(start),
			}
		}(id, fn)
	}

	var results []BranchResult
	collected := 0
	successes := 0
	for collected < len(branches) && successes < n {
		select {
		case r := <-resultCh:
			collected++
			if r.Error == nil {
				results = append(results, r)
				successes++
			}
		case <-totalCtx.Done():
			return results
		}
	}
	totalCancel()
	return results
}
