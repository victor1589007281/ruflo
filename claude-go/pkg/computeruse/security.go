package computeruse

import (
	"fmt"
	"sync"
	"time"
)

// SecurityConfig computer-use 安全策略配置。
type SecurityConfig struct {
	MaxActionsPerMinute  int      `json:"maxActionsPerMinute"`  // 每分钟最大操作次数
	MaxActionsPerSession int      `json:"maxActionsPerSession"` // 单次会话最大操作次数
	SensitiveApps        []string `json:"sensitiveApps"`        // 敏感应用列表
}

// DefaultSecurityConfig 返回默认安全配置。
func DefaultSecurityConfig() SecurityConfig {
	return SecurityConfig{
		MaxActionsPerMinute:  60,
		MaxActionsPerSession: 200,
		SensitiveApps: []string{
			"1Password", "Keychain", "KeePass",
			"银行", "Bank", "支付宝", "微信支付",
		},
	}
}

// RateLimiter 操作频率限制器。
type RateLimiter struct {
	mu           sync.Mutex
	config       SecurityConfig
	sessionCount int
	minuteCount  int
	lastMinute   time.Time
}

// NewRateLimiter 创建限速器。
func NewRateLimiter(cfg SecurityConfig) *RateLimiter {
	return &RateLimiter{
		config:     cfg,
		lastMinute: time.Now(),
	}
}

// Allow 检查是否允许执行操作。
func (rl *RateLimiter) Allow() error {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if now.Sub(rl.lastMinute) > time.Minute {
		rl.minuteCount = 0
		rl.lastMinute = now
	}

	if rl.config.MaxActionsPerMinute > 0 && rl.minuteCount >= rl.config.MaxActionsPerMinute {
		return fmt.Errorf("操作频率超限: 每分钟最多 %d 次", rl.config.MaxActionsPerMinute)
	}

	if rl.config.MaxActionsPerSession > 0 && rl.sessionCount >= rl.config.MaxActionsPerSession {
		return fmt.Errorf("会话操作次数超限: 最多 %d 次", rl.config.MaxActionsPerSession)
	}

	rl.minuteCount++
	rl.sessionCount++
	return nil
}

// Reset 重置会话计数器。
func (rl *RateLimiter) Reset() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.sessionCount = 0
	rl.minuteCount = 0
}
