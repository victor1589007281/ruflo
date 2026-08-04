// filelock.go — 跨进程文件锁（flock）。
//
// 多 claude-go codeintel MCP 实例共用同一 PV 时，进程内 sync.Mutex 只能串行
// 单进程内的并发，跨实例（不同 Pod / 不同进程）对同一仓库分支的索引操作会并发
// 写 .gitnexus / graphify-out / index.json 造成竞态。这里用 flock 提供跨进程
// 排他锁：锁文件落在共享 PV 的 locks/ 目录，所有实例对同一逻辑键抢同一把锁。
//
// flock 是建议性锁，要求所有写路径自觉持锁；本包只负责原语，调用点见
// index_manager.go（index.json 元数据）与 mcp_transport.go（仓库分支索引）。
package codeintel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// FileLock 基于 flock 的跨进程互斥锁。
type FileLock struct {
	f    *os.File
	path string
}

// AcquireFileLock 获取锁文件上的排他锁；忙等直到 timeout（0 表示默认 30s）。
// lockRoot 为锁目录，key 为逻辑锁标识（如 repo/branch），内部哈希为锁文件名，
// 避免 key 直接做文件名带来的路径穿越与长度问题。
func AcquireFileLock(lockRoot, key string, timeout time.Duration) (*FileLock, error) {
	if lockRoot == "" {
		return nil, fmt.Errorf("filelock: empty lock root")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if err := os.MkdirAll(lockRoot, 0o755); err != nil {
		return nil, fmt.Errorf("filelock: mkdir %s: %w", lockRoot, err)
	}
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:8]) + ".lock"
	path := filepath.Join(lockRoot, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %s: %w", path, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return &FileLock{f: f, path: path}, nil
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, fmt.Errorf("filelock: flock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("filelock: acquire %q timed out after %s", key, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Release 释放排他锁并关闭锁文件。
func (l *FileLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
	return err
}

// locksDir 返回共享锁目录。
func (im *IndexManager) locksDir() string {
	return filepath.Join(im.BaseDir, "locks")
}

// LockRepo 为"同一仓库 + 当前分支"获取跨进程排他锁（索引/重索引前调用）。
// 与 updateMeta 的 index.json 锁不同，这把锁覆盖整段 gitnexus analyze +
// graphify update，保证同一仓库分支的写操作多实例间串行。
// 返回的 FileLock 需由调用方 defer Release()。
func (im *IndexManager) LockRepo(repoPath string) (*FileLock, error) {
	name := repoNameFromPath(repoPath)
	branch, _, _ := currentGitInfo(repoPath)
	if branch == "" || branch == "HEAD" {
		branch = "unknown"
	}
	return AcquireFileLock(im.locksDir(), name+"/"+branch, 2*time.Minute)
}
