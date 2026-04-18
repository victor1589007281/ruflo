// 全局 LLM 请求准入控制器。
//
// 解决: 多 Agent 并发场景下 LLM API 限流导致团队编排停止工作。
//
// 架构 (三层防护):
//   1. RPM 令牌桶: 控制每分钟请求速率, 避免触发 429-Throttling.RateQuota
//   2. 并发信号量: 限制同时在途请求数, 避免触发 429-Throttling.BurstRate
//   3. AIMD 自适应: 遇到 429 时倍减并发; 成功时加性增加
//
// 参考:
//   - OpenAI Rate Limits: x-ratelimit-* headers
//   - Anthropic Token Bucket: anthropic-ratelimit-* headers
//   - 阿里百炼: BurstRate / RateQuota / AllocationQuota 三类限流
//   - LangChain InMemoryRateLimiter
//   - Justitia (arXiv:2510.17015): 任务并行 LLM Agent 公平调度
package api

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimitGuard 全局 LLM 请求准入控制器。
// 所有 Agent 共享同一个实例, 确保跨 Agent 的速率协调。
type RateLimitGuard struct {
	mu sync.Mutex

	// RPM 令牌桶
	rpmCapacity float64   // 每分钟最大请求数
	rpmTokens   float64   // 当前可用令牌
	rpmLastTick time.Time // 上次补充时间

	// 并发信号量 (AIMD)
	maxInFlight int32         // 当前允许的最大并发
	inFlight    atomic.Int32  // 当前在途请求
	hardMax     int32         // 硬上限 (不超过此值)
	hardMin     int32         // 硬下限 (不低于此值)
	sem         chan struct{} // 信号量通道

	// 全局退避 (429 时暂停所有新请求)
	pauseUntil time.Time

	// 统计
	TotalAcquires atomic.Int64
	TotalWaits    atomic.Int64 // 等待 RPM 令牌的次数
	Total429s     atomic.Int64
	TotalSuccess  atomic.Int64
	AIMDCuts      atomic.Int64 // AIMD 降低并发的次数
}

// GuardConfig 准入控制器配置
type GuardConfig struct {
	RPM         int // 每分钟最大请求数 (0=不限制)
	MaxParallel int // 最大并发 LLM 请求数
	MinParallel int // 最低并发数 (AIMD 不低于此值)
}

// DefaultGuardConfig 默认配置 (适合阿里百炼 qwen 系列)
func DefaultGuardConfig() GuardConfig {
	return GuardConfig{
		RPM:         50,
		MaxParallel: 8,
		MinParallel: 2,
	}
}

// NewRateLimitGuard 创建全局准入控制器
func NewRateLimitGuard(cfg GuardConfig) *RateLimitGuard {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 8
	}
	if cfg.MinParallel <= 0 {
		cfg.MinParallel = 2
	}
	if cfg.RPM <= 0 {
		cfg.RPM = 50
	}

	g := &RateLimitGuard{
		rpmCapacity: float64(cfg.RPM),
		rpmTokens:   float64(cfg.RPM),
		rpmLastTick: time.Now(),
		maxInFlight: int32(cfg.MaxParallel),
		hardMax:     int32(cfg.MaxParallel),
		hardMin:     int32(cfg.MinParallel),
		sem:         make(chan struct{}, cfg.MaxParallel),
	}

	// 预填充信号量
	for i := 0; i < cfg.MaxParallel; i++ {
		g.sem <- struct{}{}
	}

	// RPM 令牌补充 goroutine
	go g.refillLoop()

	return g
}

// Acquire 在发起 LLM 请求前调用, 阻塞直到获得许可。
// 返回 release 函数, 必须在请求完成后调用。
func (g *RateLimitGuard) Acquire() (release func()) {
	g.TotalAcquires.Add(1)

	// 1. 等待全局退避结束
	g.waitForPause()

	// 2. RPM 令牌桶
	g.acquireRPM()

	// 3. 并发信号量
	g.acquireSem()

	g.inFlight.Add(1)
	released := atomic.Bool{}
	return func() {
		if released.CompareAndSwap(false, true) {
			g.inFlight.Add(-1)
			g.releaseSem()
		}
	}
}

// OnSuccess 请求成功时调用 (AIMD 加性增加)
func (g *RateLimitGuard) OnSuccess() {
	g.TotalSuccess.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()

	// 每 10 次成功增加 1 并发 (渐进恢复)
	if g.TotalSuccess.Load()%10 == 0 {
		cur := atomic.LoadInt32(&g.maxInFlight)
		if cur < g.hardMax {
			atomic.StoreInt32(&g.maxInFlight, cur+1)
			// 给信号量增加一个位置
			select {
			case g.sem <- struct{}{}:
			default:
			}
		}
	}
}

// On429 收到 429 时调用 (AIMD 乘性减少 + 全局退避)
func (g *RateLimitGuard) On429(retryAfterSec float64) {
	g.Total429s.Add(1)
	g.AIMDCuts.Add(1)

	g.mu.Lock()
	defer g.mu.Unlock()

	// AIMD: 并发减半
	cur := atomic.LoadInt32(&g.maxInFlight)
	next := cur / 2
	if next < g.hardMin {
		next = g.hardMin
	}
	if next < cur {
		atomic.StoreInt32(&g.maxInFlight, next)
		// 从信号量中移除多余的位置
		diff := int(cur - next)
		for i := 0; i < diff; i++ {
			select {
			case <-g.sem:
			default:
			}
		}
	}

	// 全局退避
	pause := 10 * time.Second
	if retryAfterSec > 0 {
		pause = time.Duration(retryAfterSec*1000) * time.Millisecond
		if pause < 5*time.Second {
			pause = 5 * time.Second
		}
		if pause > 120*time.Second {
			pause = 120 * time.Second
		}
	}
	until := time.Now().Add(pause)
	if until.After(g.pauseUntil) {
		g.pauseUntil = until
	}
}

// CurrentMaxParallel 返回当前 AIMD 允许的最大并发数。
func (g *RateLimitGuard) CurrentMaxParallel() int {
	return int(atomic.LoadInt32(&g.maxInFlight))
}

// SuggestConcurrency 基于当前流控状态建议工作流层并发度。
// 返回值在 [2, hardMax] 之间，考虑: 429 历史、当前 in-flight、RPM 余量。
func (g *RateLimitGuard) SuggestConcurrency() int {
	cur := int(atomic.LoadInt32(&g.maxInFlight))
	inFlight := int(g.inFlight.Load())
	avail := cur - inFlight
	if avail < 0 {
		avail = 0
	}

	g.mu.Lock()
	rpmAvail := g.rpmTokens
	isPaused := time.Until(g.pauseUntil) > 0
	g.mu.Unlock()

	// 正在全局退避中 → 最低并发
	if isPaused {
		return int(g.hardMin)
	}
	// RPM 余量不足 → 降低并发
	if rpmAvail < 5 {
		suggest := cur / 2
		if suggest < int(g.hardMin) {
			suggest = int(g.hardMin)
		}
		return suggest
	}
	// 429 比率较高 → 保守
	total := g.TotalAcquires.Load()
	rate429 := float64(0)
	if total > 0 {
		rate429 = float64(g.Total429s.Load()) / float64(total)
	}
	if rate429 > 0.1 {
		suggest := cur * 2 / 3
		if suggest < int(g.hardMin) {
			suggest = int(g.hardMin)
		}
		return suggest
	}
	// 正常: 使用当前 AIMD 值
	return cur
}

// Stats 返回当前状态 (用于日志/飞书通知)
func (g *RateLimitGuard) Stats() string {
	g.mu.Lock()
	rpmTok := g.rpmTokens
	pauseLeft := time.Until(g.pauseUntil)
	g.mu.Unlock()

	if pauseLeft < 0 {
		pauseLeft = 0
	}
	return fmt.Sprintf("inflight=%d/%d rpm_avail=%.0f 429s=%d pause=%v",
		g.inFlight.Load(), atomic.LoadInt32(&g.maxInFlight),
		rpmTok, g.Total429s.Load(), pauseLeft.Round(time.Second))
}

// GuardSnapshot RateLimitGuard 的结构化运行态快照。
// dashboard / 诊断系统拉取实时限流/并发状态, 进行可视化与分析。
type GuardSnapshot struct {
	// 并发信号量
	InFlight    int32 `json:"inFlight"`    // 当前在途请求数
	MaxParallel int32 `json:"maxParallel"` // AIMD 当前允许的最大并发
	HardMax     int32 `json:"hardMax"`     // 并发硬上限
	HardMin     int32 `json:"hardMin"`     // 并发硬下限

	// RPM 令牌桶
	RPMCapacity float64 `json:"rpmCapacity"` // 令牌桶容量 (每分钟请求)
	RPMAvailable float64 `json:"rpmAvailable"` // 当前可用令牌

	// 全局退避
	PauseSecondsLeft float64 `json:"pauseSecondsLeft"` // 距离退避结束的秒数 (0=正常)

	// 累计计数
	TotalAcquires int64 `json:"totalAcquires"` // 总尝试获取次数
	TotalWaits    int64 `json:"totalWaits"`    // 等待令牌/退避次数
	Total429s     int64 `json:"total429s"`     // 收到 429 次数
	TotalSuccess  int64 `json:"totalSuccess"`  // 成功计数
	AIMDCuts      int64 `json:"aimdCuts"`      // AIMD 降并发次数

	Timestamp time.Time `json:"timestamp"`
}

// Snapshot 读取当前限流器的结构化快照, 用于 dashboard 可视化与 metrics 采集。
func (g *RateLimitGuard) Snapshot() GuardSnapshot {
	if g == nil {
		return GuardSnapshot{Timestamp: time.Now()}
	}
	g.mu.Lock()
	rpmTok := g.rpmTokens
	rpmCap := g.rpmCapacity
	pauseLeft := time.Until(g.pauseUntil)
	g.mu.Unlock()
	if pauseLeft < 0 {
		pauseLeft = 0
	}
	return GuardSnapshot{
		InFlight:         g.inFlight.Load(),
		MaxParallel:      atomic.LoadInt32(&g.maxInFlight),
		HardMax:          g.hardMax,
		HardMin:          g.hardMin,
		RPMCapacity:      rpmCap,
		RPMAvailable:     rpmTok,
		PauseSecondsLeft: pauseLeft.Seconds(),
		TotalAcquires:    g.TotalAcquires.Load(),
		TotalWaits:       g.TotalWaits.Load(),
		Total429s:        g.Total429s.Load(),
		TotalSuccess:     g.TotalSuccess.Load(),
		AIMDCuts:         g.AIMDCuts.Load(),
		Timestamp:        time.Now(),
	}
}

// ── 内部方法 ──

func (g *RateLimitGuard) waitForPause() {
	for {
		g.mu.Lock()
		wait := time.Until(g.pauseUntil)
		g.mu.Unlock()

		if wait <= 0 {
			return
		}
		g.TotalWaits.Add(1)
		time.Sleep(wait)
	}
}

func (g *RateLimitGuard) acquireRPM() {
	for {
		g.mu.Lock()
		g.refillRPMTokens()
		if g.rpmTokens >= 1.0 {
			g.rpmTokens--
			g.mu.Unlock()
			return
		}
		// 计算等待时间
		waitSec := (1.0 - g.rpmTokens) / (g.rpmCapacity / 60.0)
		g.mu.Unlock()

		g.TotalWaits.Add(1)
		// 加小抖动防雷群
		jitter := time.Duration(rand.Float64()*200) * time.Millisecond
		time.Sleep(time.Duration(waitSec*1000)*time.Millisecond + jitter)
	}
}

func (g *RateLimitGuard) refillRPMTokens() {
	now := time.Now()
	elapsed := now.Sub(g.rpmLastTick).Seconds()
	if elapsed <= 0 {
		return
	}
	refill := elapsed * g.rpmCapacity / 60.0
	g.rpmTokens = math.Min(g.rpmCapacity, g.rpmTokens+refill)
	g.rpmLastTick = now
}

func (g *RateLimitGuard) refillLoop() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		g.mu.Lock()
		g.refillRPMTokens()
		g.mu.Unlock()
	}
}

func (g *RateLimitGuard) acquireSem() {
	<-g.sem
}

func (g *RateLimitGuard) releaseSem() {
	select {
	case g.sem <- struct{}{}:
	default:
	}
}

// ── Retry-After 解析 (供 client.go 使用) ──

var retryAfterRegex = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(?:s|秒|seconds?)`)

// ParseRetryAfter 从 HTTP 响应中提取 Retry-After 秒数。
// 优先级: Retry-After header → JSON body → 中文模式匹配 → -1 (未找到)
func ParseRetryAfter(resp *http.Response, body []byte) float64 {
	// 1. HTTP Retry-After header
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if sec, err := strconv.ParseFloat(ra, 64); err == nil && sec > 0 {
				return sec
			}
		}
	}

	// 2. JSON body 中的 retry_after 字段
	if len(body) > 0 {
		bodyStr := string(body)
		// 匹配 "retry_after": 10 或 "retryAfter": 10
		for _, pattern := range []string{`"retry_after"`, `"retryAfter"`, `"Retry-After"`} {
			idx := strings.Index(bodyStr, pattern)
			if idx >= 0 {
				rest := bodyStr[idx+len(pattern):]
				rest = strings.TrimLeft(rest, ": ")
				var val float64
				if _, err := fmt.Sscanf(rest, "%f", &val); err == nil && val > 0 {
					return val
				}
			}
		}

		// 3. 中文模式: "请在 10s 后重试" / "请 10 秒后重试"
		if m := retryAfterRegex.FindStringSubmatch(bodyStr); len(m) >= 2 {
			if sec, err := strconv.ParseFloat(m[1], 64); err == nil && sec > 0 {
				return sec
			}
		}
	}

	return -1
}

// ── 429 错误分类 (DashScope 特有) ──

// RateLimitKind 限流类型
type RateLimitKind int

const (
	RateLimitUnknown    RateLimitKind = iota
	RateLimitRPM                      // RateQuota: RPM/RPS 超限 → 降速
	RateLimitBurst                    // BurstRate: 突发过快 → 平滑
	RateLimitTPM                      // AllocationQuota: TPM/TPS 超限 → 降速+缩上下文
	RateLimitBilling                  // 计费问题 → 不重试
	RateLimitOverloaded               // 503/529 服务过载 → 长退避
)

// Classify429 根据错误内容分类限流类型
func Classify429(statusCode int, body string) RateLimitKind {
	if statusCode == 503 || statusCode == 529 {
		return RateLimitOverloaded
	}
	if statusCode != 429 {
		return RateLimitUnknown
	}

	lower := strings.ToLower(body)

	// DashScope 特有错误码
	if strings.Contains(lower, "burstrate") || strings.Contains(lower, "burst_rate") {
		return RateLimitBurst
	}
	if strings.Contains(lower, "allocationquota") || strings.Contains(lower, "allocation_quota") || strings.Contains(lower, "tps") || strings.Contains(lower, "tpm") {
		return RateLimitTPM
	}
	if strings.Contains(lower, "commodity") || strings.Contains(lower, "prepaid") || strings.Contains(lower, "overdue") || strings.Contains(lower, "insufficient_quota") {
		return RateLimitBilling
	}

	return RateLimitRPM
}

// ShouldRetry 判断该限流类型是否应重试
func (k RateLimitKind) ShouldRetry() bool {
	return k != RateLimitBilling
}

// String 返回中文描述
func (k RateLimitKind) String() string {
	switch k {
	case RateLimitRPM:
		return "请求频率超限(RPM)"
	case RateLimitBurst:
		return "突发速率过快(Burst)"
	case RateLimitTPM:
		return "Token吞吐超限(TPM)"
	case RateLimitBilling:
		return "计费/配额问题(不可重试)"
	case RateLimitOverloaded:
		return "服务过载(503/529)"
	default:
		return "未知限流"
	}
}
