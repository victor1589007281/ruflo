// helpers.go — 引擎内部通用辅助函数与常量。
package internal_hook

import (
	"fmt"
	"sync/atomic"
	"time"
)

// UUIDCounter 全局原子计数器，用于 GenerateUUID 避免并发碰撞。
var UUIDCounter atomic.Int64

// GenerateUUID 生成唯一标识 (时间戳 + 全局原子计数器)。
func GenerateUUID() string {
	seq := UUIDCounter.Add(1)
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), seq)
}

// 断路器与压缩常量。
const (
	MaxConsecutiveErrors = 5     // 连续错误达到此阈值触发熔断
	MicroCompactMaxChars = 16000 // MicroCompact 截断阈值 (字符)
	MessageCompactChars  = 30000 // messages 超过该字符数时提前压缩旧 tool_result
)
