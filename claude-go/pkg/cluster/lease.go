package cluster

// 分布式租约 (design/02 §3.4.4): 用于 cron 选主等"多副本同一动作只做一次"。
//
// FileLease 用文件系统 O_CREATE|O_EXCL 的原子创建语义实现租约——在共享 PVC 上,
// 多副本同时 TryAcquire 同一 key 只有一个成功创建锁文件。旧锁文件按 TTL 清理
// (防副本崩溃后 key 永久占用)。单机/无共享盘场景退化为进程内去重。

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileLease 基于共享文件系统的租约 (cluster.CronLease / agent.CronLease 的实现)。
type FileLease struct {
	dir string
	ttl time.Duration
}

// NewFileLease dir 为共享锁目录 (K8s 下挂 PVC); ttl 为锁有效期 (0 → 5min)。
func NewFileLease(dir string, ttl time.Duration) *FileLease {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	_ = os.MkdirAll(dir, 0o755)
	return &FileLease{dir: dir, ttl: ttl}
}

// TryAcquire 原子抢占 key; 返回 true 表示本副本抢到。
func (l *FileLease) TryAcquire(key string) bool {
	if l == nil {
		return true // nil 租约 = 不去重 (单副本)
	}
	l.sweep()
	path := filepath.Join(l.dir, sanitizeLeaseKey(key)+".lock")
	// O_EXCL: 文件已存在则失败 → 原子"抢锁"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false // 别的副本已持有
	}
	_, _ = f.WriteString(time.Now().Format(time.RFC3339))
	_ = f.Close()
	return true
}

// sweep 清理过期锁文件 (副本崩溃遗留)。
func (l *FileLease) sweep() {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-l.ttl)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, e.Name()))
		}
	}
}

func sanitizeLeaseKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
