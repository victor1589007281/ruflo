// warm.go 决策模型保活：周期性地向 Ollaya 发一次轻量 decide（keep_alive 续租），
// 防止 snap:2b / jeb:4b 因空闲超时被自动卸载（实测冷加载 10–17s，会击穿 qualityGate 的 20s 预算）。
//
// 背景（2026-10-22 优化）：默认 OLLAYA_KEEP_ALIVE 到期就卸载，CPU 上重载是 10s 级，
// 这是 quality_gate with_jev_score 率只有 20% 的根因。保活手段：
//
//   - keep_alive 是 Ollaya 的续租参数，本包在 Decide 请求里显式带上 24h，
//     每次真实调用都把该模型的过期窗口推后 24h（滑动）。
//   - 但「完全没调用」时模型照样被卸载；本文件提供一个独立的后台循环，
//     每 interval 发一次 1-token noul decide，确保至少每 interval 续租一次。
//
// 使用纪律：调用方决定要不要启用（worker 启动时），本包不自动启动 goroutine。
// 一切失败静默——保活是优化，绝不阻断主路径（fail-open）。
package decision

import (
	"context"
	"log"
	"time"
)

// defaultWarmInterval 是两次保活调用之间的间隔。
// 取 3 分钟：远低于 Ollaya 典型 idle 卸载窗口（分钟级），又不会产生可测负载
// （一次 noul decide 只有几十 token、毫秒级推理）。
const defaultWarmInterval = 3 * time.Minute

// WarmModels 以 daemon 方式周期性对 models 发起保活 decide，直到 ctx 取消。
//
// 调用方式（worker 启动处）：
//
//	if u := os.Getenv("CLAUDE_GO_DECISION_URL"); u != "" {
//	    go decision.WarmModels(ctx, &decision.Client{BaseURL: u},
//	        []string{decision.ModelRecall, decision.ModelScore})
//	}
//
// 设计上故意不走 RECALL/SCORE 的真实业务路径：保活流量不应污染 agentDB 流水
// （stats 会把 warm 计数到 latency/kind 里）。每次调用的 model+state 是独立的
// 空 noul，不进 logger。
func WarmModels(ctx context.Context, c *Client, models []string) {
	if c == nil || len(models) == 0 {
		return
	}
	// 独立的短超时不依赖全局 Client.Timeout：保活用的单次推理极小（state 一个词，
	// 1 个 noul 问题），CPU 上也应在秒级返回；20s 只是为了不挂死 goroutine。
	warmClient := *c
	if warmClient.Timeout <= 0 || warmClient.Timeout > 20*time.Second {
		warmClient.Timeout = 20 * time.Second
	}
	// 禁重试：保活失败就失败，3 分钟后再来；重试会把冷加载的 503/超时放大成多次推理。
	warmClient.MaxRetries = -1

	tick := func() {
		for _, m := range models {
			warmOnce(ctx, &warmClient, m)
			if ctx.Err() != nil {
				return
			}
		}
	}
	tick() // 启动即跑一次，省掉第一次冷加载延迟
	t := time.NewTicker(defaultWarmInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// warmOnce 向一个模型发一次 1-token noul decide。所有错误都吞掉——保活不是业务。
func warmOnce(ctx context.Context, c *Client, model string) {
	if model == "" {
		return
	}
	qs := []Question{{
		ID:           "warm",
		Type:         TypeNoul,
		Instructions: "keepalive probe, answer with anything",
	}}
	reqCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	if _, err := c.Decide(reqCtx, model, "warm", qs); err != nil {
		// 用 log.Printf 而非 logger：保活失败不进决策流水（那不是决策）。
		log.Printf("[decision] warm %s: %v (忽略)", model, err)
	}
}
